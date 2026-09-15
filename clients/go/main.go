// artifactd-cli: upload, download and list artifacts on an artifactd server.
// Standard library only.
//
//	artifactd-cli [-url URL] mkrepo   <repo>
//	artifactd-cli [-url URL] upload   <repo> <path> <file> [-no-checksum]
//	artifactd-cli [-url URL] download <repo> <path> [outfile]
//	artifactd-cli [-url URL] list     <repo> [prefix] [-r]
//
// URL defaults to $ARTIFACTD_URL or http://localhost:8080.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"
)

// Client talks to an artifactd server.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// NewClient returns a client; empty base falls back to $ARTIFACTD_URL, then localhost:8080.
func NewClient(base string) *Client {
	if base == "" {
		base = os.Getenv("ARTIFACTD_URL")
	}
	if base == "" {
		base = "http://localhost:8080"
	}
	return &Client{BaseURL: strings.TrimRight(base, "/"), HTTP: &http.Client{Timeout: 10 * time.Minute}}
}

// Error is a non-2xx response from the server.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message) }

// ArtifactInfo is returned by Upload and recursive List.
type ArtifactInfo struct {
	Path       string    `json:"path"`
	Size       int64     `json:"size"`
	ModifiedAt time.Time `json:"modified_at"`
	SHA256     string    `json:"sha256,omitempty"`
}

// DirEntry is returned by non-recursive List.
type DirEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size,omitempty"`
}

// cleanPath normalizes a user-supplied artifact path: backslashes become
// slashes, leading/trailing/duplicate slashes are dropped. Empty segments
// would otherwise make the server answer with a 301/307 redirect.
func cleanPath(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	var segs []string
	for _, s := range strings.Split(p, "/") {
		if s != "" && s != "." {
			segs = append(segs, s)
		}
	}
	return strings.Join(segs, "/")
}

func (c *Client) artifactURL(repo, p string) string {
	segs := strings.Split(cleanPath(p), "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return c.BaseURL + "/api/repos/" + url.PathEscape(repo) + "/artifacts/" + strings.Join(segs, "/")
}

func (c *Client) do(req *http.Request) (*http.Response, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var e struct {
		Error string `json:"error"`
	}
	msg := strings.TrimSpace(string(body))
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		msg = e.Error
	}
	if msg == "" {
		msg = resp.Status
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		msg = fmt.Sprintf("unexpected redirect to %q (check that the base URL points directly at artifactd and the artifact path has no empty segments)", resp.Header.Get("Location"))
	}
	return nil, &Error{Status: resp.StatusCode, Message: msg}
}

// CreateRepo creates a repository. Returns true if it was created, false if it already existed.
func (c *Client) CreateRepo(repo string) (bool, error) {
	req, err := http.NewRequest(http.MethodPut, c.BaseURL+"/api/repos/"+url.PathEscape(repo), nil)
	if err != nil {
		return false, err
	}
	resp, err := c.do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	var out struct {
		Created bool `json:"created"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, err
	}
	return out.Created, nil
}

// Upload sends a local file. If verifyChecksum is set, the file is hashed
// first and the server verifies the digest.
func (c *Client) Upload(repo, artifactPath, filename string, verifyChecksum bool) (*ArtifactInfo, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	var sum string
	if verifyChecksum {
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return nil, err
		}
		sum = hex.EncodeToString(h.Sum(nil))
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequest(http.MethodPut, c.artifactURL(repo, artifactPath), f)
	if err != nil {
		return nil, err
	}
	req.ContentLength = st.Size()
	req.Header.Set("Content-Type", "application/octet-stream")
	if sum != "" {
		req.Header.Set("X-Checksum-SHA256", sum)
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var info ArtifactInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, err
	}
	return &info, nil
}

// Download writes an artifact to outfile (default: base name of the artifact path).
func (c *Client) Download(repo, artifactPath, outfile string) (string, error) {
	if outfile == "" {
		outfile = path.Base(artifactPath)
	}
	req, err := http.NewRequest(http.MethodGet, c.artifactURL(repo, artifactPath), nil)
	if err != nil {
		return "", err
	}
	resp, err := c.do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	tmp := outfile + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		out.Close()
		os.Remove(tmp)
		return "", err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, outfile); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return outfile, nil
}

// List returns the entries of one directory in the repo (prefix "" = root).
func (c *Client) List(repo, prefix string) ([]DirEntry, error) {
	var out struct {
		Entries []DirEntry `json:"entries"`
	}
	if err := c.getJSON(c.listURL(repo, prefix, false), &out); err != nil {
		return nil, err
	}
	return out.Entries, nil
}

// ListRecursive returns every file under prefix.
func (c *Client) ListRecursive(repo, prefix string) ([]ArtifactInfo, error) {
	var out struct {
		Files []ArtifactInfo `json:"files"`
	}
	if err := c.getJSON(c.listURL(repo, prefix, true), &out); err != nil {
		return nil, err
	}
	return out.Files, nil
}

func (c *Client) listURL(repo, prefix string, recursive bool) string {
	p := cleanPath(prefix)
	u := c.artifactURL(repo, p)
	if p != "" {
		u += "/"
	}
	if recursive {
		u += "?recursive=1"
	}
	return u
}

func (c *Client) getJSON(u string, v any) error {
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(v)
}

// ---------------------------------------------------------------------------

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  artifactd-cli [-url URL] mkrepo   <repo>
  artifactd-cli [-url URL] upload   <repo> <path> <file> [-no-checksum]
  artifactd-cli [-url URL] download <repo> <path> [outfile]
  artifactd-cli [-url URL] list     <repo> [prefix] [-r]`)
	os.Exit(2)
}

func main() {
	base := flag.String("url", "", "server base URL (default $ARTIFACTD_URL or http://localhost:8080)")
	flag.Usage = usage
	flag.Parse()
	args := flag.Args()
	if len(args) < 1 {
		usage()
	}
	c := NewClient(*base)

	var err error
	switch args[0] {
	case "mkrepo":
		if len(args) != 2 {
			usage()
		}
		var created bool
		created, err = c.CreateRepo(args[1])
		if err == nil {
			if created {
				fmt.Printf("repo %s created\n", args[1])
			} else {
				fmt.Printf("repo %s already exists\n", args[1])
			}
		}
	case "upload":
		fs := flag.NewFlagSet("upload", flag.ExitOnError)
		noSum := fs.Bool("no-checksum", false, "skip client-side SHA-256 verification header")
		_ = fs.Parse(args[1:])
		rest := fs.Args()
		if len(rest) != 3 {
			usage()
		}
		var info *ArtifactInfo
		info, err = c.Upload(rest[0], rest[1], rest[2], !*noSum)
		if err == nil {
			fmt.Printf("uploaded %s (%d bytes) sha256=%s\n", info.Path, info.Size, info.SHA256)
		}
	case "download":
		if len(args) < 3 || len(args) > 4 {
			usage()
		}
		out := ""
		if len(args) == 4 {
			out = args[3]
		}
		out, err = c.Download(args[1], args[2], out)
		if err == nil {
			st, _ := os.Stat(out)
			fmt.Printf("downloaded to %s (%d bytes)\n", out, st.Size())
		}
	case "list":
		fs := flag.NewFlagSet("list", flag.ExitOnError)
		rec := fs.Bool("r", false, "recursive")
		_ = fs.Parse(args[1:])
		rest := fs.Args()
		if len(rest) < 1 || len(rest) > 2 {
			usage()
		}
		prefix := ""
		if len(rest) == 2 {
			prefix = rest[1]
		}
		if *rec {
			var files []ArtifactInfo
			files, err = c.ListRecursive(rest[0], prefix)
			for _, f := range files {
				fmt.Printf("%12d  %s\n", f.Size, f.Path)
			}
		} else {
			var entries []DirEntry
			entries, err = c.List(rest[0], prefix)
			for _, e := range entries {
				if e.IsDir {
					fmt.Printf("%12s  %s\n", "<dir>", e.Name)
				} else {
					fmt.Printf("%12d  %s\n", e.Size, e.Name)
				}
			}
		}
	default:
		usage()
	}
	if err != nil {
		var ae *Error
		if errors.As(err, &ae) {
			fmt.Fprintln(os.Stderr, "error:", ae)
		} else {
			fmt.Fprintln(os.Stderr, "error:", err)
		}
		os.Exit(1)
	}
}
