// artifactd is a minimal, single-binary local artifact/file repository.
//
// Layout on disk:
//
//	$ARTIFACTD_DATA_ROOT/
//	  <repo>/
//	    <arbitrary/nested/path/to/file>
//
// Every repo is a directory directly under the data root; every artifact is a
// plain file under its repo. No database, no dedup, no index.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ---------------------------------------------------------------------------
// Configuration (environment variables)
// ---------------------------------------------------------------------------

type config struct {
	Addr           string        // ARTIFACTD_ADDR            (default ":8080")
	DataRoot       string        // ARTIFACTD_DATA_ROOT       (default "./data")
	MaxUploadBytes int64         // ARTIFACTD_MAX_UPLOAD_BYTES (default 256 MiB)
	AllowOverwrite bool          // ARTIFACTD_ALLOW_OVERWRITE (default false)
	ReadTimeout    time.Duration // ARTIFACTD_READ_TIMEOUT    (default 10m; covers large uploads)
	WriteTimeout   time.Duration // ARTIFACTD_WRITE_TIMEOUT   (default 10m; covers large downloads)
	IdleTimeout    time.Duration // ARTIFACTD_IDLE_TIMEOUT    (default 2m)
	MaxRepoDepth   int           // ARTIFACTD_MAX_PATH_DEPTH  (default 32)
	MaxPathLen     int           // ARTIFACTD_MAX_PATH_LEN    (default 1024)
}

func loadConfig() (config, error) {
	c := config{
		Addr:           envStr("ARTIFACTD_ADDR", ":8080"),
		DataRoot:       envStr("ARTIFACTD_DATA_ROOT", "./data"),
		MaxUploadBytes: 256 << 20,
		ReadTimeout:    10 * time.Minute,
		WriteTimeout:   10 * time.Minute,
		IdleTimeout:    2 * time.Minute,
		MaxRepoDepth:   32,
		MaxPathLen:     1024,
	}
	var err error
	if c.MaxUploadBytes, err = envInt64("ARTIFACTD_MAX_UPLOAD_BYTES", c.MaxUploadBytes); err != nil {
		return c, err
	}
	if c.AllowOverwrite, err = envBool("ARTIFACTD_ALLOW_OVERWRITE", c.AllowOverwrite); err != nil {
		return c, err
	}
	if c.ReadTimeout, err = envDuration("ARTIFACTD_READ_TIMEOUT", c.ReadTimeout); err != nil {
		return c, err
	}
	if c.WriteTimeout, err = envDuration("ARTIFACTD_WRITE_TIMEOUT", c.WriteTimeout); err != nil {
		return c, err
	}
	if c.IdleTimeout, err = envDuration("ARTIFACTD_IDLE_TIMEOUT", c.IdleTimeout); err != nil {
		return c, err
	}
	if v, err := envInt64("ARTIFACTD_MAX_PATH_DEPTH", int64(c.MaxRepoDepth)); err != nil {
		return c, err
	} else {
		c.MaxRepoDepth = int(v)
	}
	if v, err := envInt64("ARTIFACTD_MAX_PATH_LEN", int64(c.MaxPathLen)); err != nil {
		return c, err
	} else {
		c.MaxPathLen = int(v)
	}
	if c.MaxUploadBytes <= 0 {
		return c, errors.New("ARTIFACTD_MAX_UPLOAD_BYTES must be > 0")
	}
	abs, err := filepath.Abs(c.DataRoot)
	if err != nil {
		return c, fmt.Errorf("resolve data root: %w", err)
	}
	c.DataRoot = abs
	return c, nil
}

func envStr(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return def
}

func envInt64(k string, def int64) (int64, error) {
	v, ok := os.LookupEnv(k)
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", k, err)
	}
	return n, nil
}

func envBool(k string, def bool) (bool, error) {
	v, ok := os.LookupEnv(k)
	if !ok || v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s: %w", k, err)
	}
	return b, nil
}

func envDuration(k string, def time.Duration) (time.Duration, error) {
	v, ok := os.LookupEnv(k)
	if !ok || v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", k, err)
	}
	return d, nil
}

// ---------------------------------------------------------------------------
// Path safety
// ---------------------------------------------------------------------------

// Repo names: conservative charset, must start alphanumeric, max 64 chars.
// This alone rules out ".", "..", "/", "\" and anything exotic.
var repoNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

var (
	errBadRepoName = errors.New("invalid repository name")
	errBadPath     = errors.New("invalid artifact path")
)

func (s *server) repoDir(repo string) (string, error) {
	if !repoNameRe.MatchString(repo) {
		return "", errBadRepoName
	}
	return filepath.Join(s.cfg.DataRoot, repo), nil
}

// cleanArtifactPath validates a user-supplied artifact path and returns it in
// canonical slash-separated form ("a/b/c"). It rejects anything that could
// escape the repo or is otherwise unsafe on common filesystems.
func (s *server) cleanArtifactPath(p string) (string, error) {
	if p == "" || len(p) > s.cfg.MaxPathLen {
		return "", errBadPath
	}
	if strings.ContainsAny(p, "\\\x00") {
		return "", errBadPath
	}
	segs := strings.Split(p, "/")
	if len(segs) > s.cfg.MaxRepoDepth {
		return "", errBadPath
	}
	for _, seg := range segs {
		switch {
		case seg == "", seg == ".", seg == "..":
			return "", errBadPath
		case strings.TrimSpace(seg) != seg:
			return "", errBadPath
		}
		for _, r := range seg {
			if r < 0x20 || r == 0x7f {
				return "", errBadPath
			}
		}
	}
	clean := path.Clean(strings.Join(segs, "/"))
	if clean != strings.Join(segs, "/") {
		return "", errBadPath
	}
	return clean, nil
}

// artifactFile resolves repo + artifact path to an absolute file path and
// double-checks it stays under the repo directory (defense in depth).
func (s *server) artifactFile(repo, artifact string) (repoDir, file, clean string, err error) {
	repoDir, err = s.repoDir(repo)
	if err != nil {
		return "", "", "", err
	}
	clean, err = s.cleanArtifactPath(artifact)
	if err != nil {
		return "", "", "", err
	}
	file = filepath.Join(repoDir, filepath.FromSlash(clean))
	rel, err := filepath.Rel(repoDir, file)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", "", errBadPath
	}
	return repoDir, file, clean, nil
}

// ---------------------------------------------------------------------------
// Server
// ---------------------------------------------------------------------------

type server struct {
	cfg config
	log *log.Logger
}

type apiError struct {
	Error string `json:"error"`
}

func (s *server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func (s *server) fail(w http.ResponseWriter, status int, msg string) {
	s.writeJSON(w, status, apiError{Error: msg})
}

// failErr maps common errors to HTTP statuses.
func (s *server) failErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errBadRepoName), errors.Is(err, errBadPath):
		s.fail(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, fs.ErrNotExist):
		s.fail(w, http.StatusNotFound, "not found")
	case errors.Is(err, fs.ErrExist):
		s.fail(w, http.StatusConflict, "already exists")
	default:
		s.log.Printf("internal error: %v", err)
		s.fail(w, http.StatusInternalServerError, "internal error")
	}
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		s.writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// Repositories
	mux.HandleFunc("GET /api/repos", s.listRepos)
	mux.HandleFunc("GET /api/repos/{repo}", s.getRepo)
	mux.HandleFunc("PUT /api/repos/{repo}", s.createRepo)
	mux.HandleFunc("DELETE /api/repos/{repo}", s.deleteRepo)

	// Artifacts
	mux.HandleFunc("GET /api/repos/{repo}/artifacts/{path...}", s.getArtifact)   // download, or listing if path is "" or ends in "/"
	mux.HandleFunc("HEAD /api/repos/{repo}/artifacts/{path...}", s.getArtifact)  // metadata only
	mux.HandleFunc("PUT /api/repos/{repo}/artifacts/{path...}", s.putArtifact)   // upload (raw body)
	mux.HandleFunc("DELETE /api/repos/{repo}/artifacts/{path...}", s.deleteArtifact)

	return s.logging(mux)
}

func (s *server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)
		s.log.Printf("%s %s %d %s %s", r.Method, r.URL.Path, rw.status, time.Since(start).Round(time.Millisecond), r.RemoteAddr)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// ---------------------------------------------------------------------------
// Repo handlers
// ---------------------------------------------------------------------------

type repoInfo struct {
	Name      string    `json:"name"`
	Files     int64     `json:"files"`
	Bytes     int64     `json:"bytes"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *server) listRepos(w http.ResponseWriter, r *http.Request) {
	entries, err := os.ReadDir(s.cfg.DataRoot)
	if err != nil {
		s.failErr(w, err)
		return
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() && repoNameRe.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	s.writeJSON(w, http.StatusOK, map[string]any{"repos": names})
}

func (s *server) createRepo(w http.ResponseWriter, r *http.Request) {
	dir, err := s.repoDir(r.PathValue("repo"))
	if err != nil {
		s.failErr(w, err)
		return
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		if errors.Is(err, fs.ErrExist) {
			s.writeJSON(w, http.StatusOK, map[string]any{"name": r.PathValue("repo"), "created": false})
			return
		}
		s.failErr(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, map[string]any{"name": r.PathValue("repo"), "created": true})
}

func (s *server) getRepo(w http.ResponseWriter, r *http.Request) {
	dir, err := s.repoDir(r.PathValue("repo"))
	if err != nil {
		s.failErr(w, err)
		return
	}
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		s.fail(w, http.StatusNotFound, "repository not found")
		return
	}
	info := repoInfo{Name: r.PathValue("repo"), CreatedAt: st.ModTime().UTC()}
	// Best-effort walk: errors on individual entries are skipped.
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				info.Files++
				info.Bytes += fi.Size()
			}
		}
		return nil
	})
	s.writeJSON(w, http.StatusOK, info)
}

func (s *server) deleteRepo(w http.ResponseWriter, r *http.Request) {
	dir, err := s.repoDir(r.PathValue("repo"))
	if err != nil {
		s.failErr(w, err)
		return
	}
	if _, err := os.Stat(dir); err != nil {
		s.fail(w, http.StatusNotFound, "repository not found")
		return
	}
	// os.Remove refuses to remove a non-empty directory. That is exactly the
	// semantic we want; no need to list first (which would race anyway).
	if err := os.Remove(dir); err != nil {
		var pe *fs.PathError
		if errors.As(err, &pe) && (errors.Is(pe.Err, syscall.ENOTEMPTY) || errors.Is(pe.Err, syscall.EEXIST)) {
			s.fail(w, http.StatusConflict, "repository is not empty")
			return
		}
		s.failErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Artifact handlers
// ---------------------------------------------------------------------------

type artifactInfo struct {
	Path       string    `json:"path"`
	Size       int64     `json:"size"`
	ModifiedAt time.Time `json:"modified_at"`
	SHA256     string    `json:"sha256,omitempty"`
}

type dirEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size,omitempty"`
}

func (s *server) putArtifact(w http.ResponseWriter, r *http.Request) {
	repoDir, file, clean, err := s.artifactFile(r.PathValue("repo"), r.PathValue("path"))
	if err != nil {
		s.failErr(w, err)
		return
	}
	if st, err := os.Stat(repoDir); err != nil || !st.IsDir() {
		s.fail(w, http.StatusNotFound, "repository not found")
		return
	}
	if r.ContentLength > s.cfg.MaxUploadBytes {
		s.fail(w, http.StatusRequestEntityTooLarge, "upload exceeds limit")
		return
	}

	// Refuse to write where a directory already lives, and refuse to
	// overwrite unless explicitly allowed.
	if st, err := os.Lstat(file); err == nil {
		if st.IsDir() {
			s.fail(w, http.StatusConflict, "path is a directory")
			return
		}
		if !s.cfg.AllowOverwrite {
			s.fail(w, http.StatusConflict, "artifact already exists (overwrite disabled)")
			return
		}
	}

	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		s.failErr(w, err)
		return
	}
	// Any component on the way down must be a real directory, not a symlink
	// pointing out of the repo.
	if err := s.ensureNoSymlinks(repoDir, filepath.Dir(file)); err != nil {
		s.failErr(w, err)
		return
	}

	// Write to a temp file in the same directory, then rename atomically.
	tmp, err := os.CreateTemp(filepath.Dir(file), ".upload-*")
	if err != nil {
		s.failErr(w, err)
		return
	}
	tmpName := tmp.Name()
	cleanup := func() { tmp.Close(); os.Remove(tmpName) }

	body := http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadBytes)
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), body)
	if err != nil {
		cleanup()
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			s.fail(w, http.StatusRequestEntityTooLarge, "upload exceeds limit")
			return
		}
		s.failErr(w, err)
		return
	}
	sum := hex.EncodeToString(h.Sum(nil))

	// Optional client-supplied checksum verification.
	if want := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Checksum-SHA256"))); want != "" && want != sum {
		cleanup()
		s.fail(w, http.StatusBadRequest, "sha256 mismatch: got "+sum)
		return
	}

	if err := tmp.Sync(); err != nil {
		cleanup()
		s.failErr(w, err)
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		s.failErr(w, err)
		return
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		os.Remove(tmpName)
		s.failErr(w, err)
		return
	}
	if err := os.Rename(tmpName, file); err != nil {
		os.Remove(tmpName)
		s.failErr(w, err)
		return
	}

	w.Header().Set("X-Checksum-SHA256", sum)
	w.Header().Set("Location", r.URL.Path)
	s.writeJSON(w, http.StatusCreated, artifactInfo{
		Path:       clean,
		Size:       n,
		ModifiedAt: time.Now().UTC(),
		SHA256:     sum,
	})
}

// ensureNoSymlinks checks each directory between repoDir and dir is a real
// directory (not a symlink), so uploads can't be redirected outside the root.
func (s *server) ensureNoSymlinks(repoDir, dir string) error {
	rel, err := filepath.Rel(repoDir, dir)
	if err != nil {
		return errBadPath
	}
	cur := repoDir
	if rel == "." {
		return nil
	}
	for _, seg := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, seg)
		st, err := os.Lstat(cur)
		if err != nil {
			return err
		}
		if st.Mode()&fs.ModeSymlink != 0 || !st.IsDir() {
			return errBadPath
		}
	}
	return nil
}

func (s *server) getArtifact(w http.ResponseWriter, r *http.Request) {
	raw := r.PathValue("path")
	// Empty path or a trailing slash means "list this directory".
	if raw == "" || strings.HasSuffix(raw, "/") {
		s.listArtifacts(w, r)
		return
	}
	_, file, clean, err := s.artifactFile(r.PathValue("repo"), raw)
	if err != nil {
		s.failErr(w, err)
		return
	}
	f, err := os.Open(file)
	if err != nil {
		s.fail(w, http.StatusNotFound, "artifact not found")
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		s.fail(w, http.StatusNotFound, "artifact not found")
		return
	}

	// Serve everything as an opaque binary unless a well-known extension says
	// otherwise, and never let the browser render it inline.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+path.Base(clean)+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("ETag", strconv.Quote(fmt.Sprintf("%x-%x", st.ModTime().UnixNano(), st.Size())))
	// http.ServeContent handles Range, If-Modified-Since, If-None-Match, HEAD.
	http.ServeContent(w, r, path.Base(clean), st.ModTime(), f)
}

func (s *server) listArtifacts(w http.ResponseWriter, r *http.Request) {
	repoDir, err := s.repoDir(r.PathValue("repo"))
	if err != nil {
		s.failErr(w, err)
		return
	}
	dir := repoDir
	prefix := ""
	if raw := strings.TrimSuffix(r.PathValue("path"), "/"); raw != "" {
		var clean string
		_, dir, clean, err = s.artifactFile(r.PathValue("repo"), raw)
		if err != nil {
			s.failErr(w, err)
			return
		}
		prefix = clean + "/"
	}
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		s.fail(w, http.StatusNotFound, "not found")
		return
	}

	// ?recursive=1 walks the whole subtree and returns flat file paths.
	if r.URL.Query().Get("recursive") != "" {
		files := []artifactInfo{}
		err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			if !d.Type().IsRegular() {
				return nil
			}
			fi, err := d.Info()
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(repoDir, p)
			if strings.HasPrefix(d.Name(), ".upload-") {
				return nil
			}
			files = append(files, artifactInfo{
				Path:       filepath.ToSlash(rel),
				Size:       fi.Size(),
				ModifiedAt: fi.ModTime().UTC(),
			})
			return nil
		})
		if err != nil {
			s.failErr(w, err)
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]any{"repo": r.PathValue("repo"), "prefix": prefix, "files": files})
		return
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		s.failErr(w, err)
		return
	}
	out := make([]dirEntry, 0, len(entries))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".upload-") {
			continue
		}
		de := dirEntry{Name: e.Name(), IsDir: e.IsDir()}
		if !e.IsDir() {
			if fi, err := e.Info(); err == nil {
				de.Size = fi.Size()
			}
		}
		out = append(out, de)
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"repo": r.PathValue("repo"), "prefix": prefix, "entries": out})
}

func (s *server) deleteArtifact(w http.ResponseWriter, r *http.Request) {
	repoDir, file, _, err := s.artifactFile(r.PathValue("repo"), r.PathValue("path"))
	if err != nil {
		s.failErr(w, err)
		return
	}
	st, err := os.Lstat(file)
	if err != nil {
		s.fail(w, http.StatusNotFound, "artifact not found")
		return
	}
	if !st.Mode().IsRegular() {
		s.fail(w, http.StatusConflict, "not a regular file")
		return
	}
	if err := os.Remove(file); err != nil {
		s.failErr(w, err)
		return
	}
	// Prune now-empty parent directories up to (but excluding) the repo dir.
	for d := filepath.Dir(file); d != repoDir && strings.HasPrefix(d, repoDir); d = filepath.Dir(d) {
		if err := os.Remove(d); err != nil {
			break // non-empty or other error: stop pruning
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func main() {
	logger := log.New(os.Stdout, "artifactd ", log.LstdFlags|log.Lmicroseconds)

	cfg, err := loadConfig()
	if err != nil {
		logger.Fatalf("config: %v", err)
	}
	if err := os.MkdirAll(cfg.DataRoot, 0o755); err != nil {
		logger.Fatalf("create data root: %v", err)
	}

	s := &server{cfg: cfg, log: logger}
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		MaxHeaderBytes:    1 << 20,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		logger.Printf("listening on %s, data root %s, max upload %d bytes, overwrite=%v",
			cfg.Addr, cfg.DataRoot, cfg.MaxUploadBytes, cfg.AllowOverwrite)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatalf("listen: %v", err)
		}
	}()

	<-ctx.Done()
	logger.Printf("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Printf("shutdown: %v", err)
	}
}
