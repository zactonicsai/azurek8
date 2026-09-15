# artifactd

A minimal, single-binary, standard-library-only Go HTTP server that acts as a local artifact/file repository. Files are stored one-per-path on the filesystem under named repositories. No auth, no database, no dedup.

```
go build -o artifactd .
ARTIFACTD_DATA_ROOT=/var/lib/artifactd ./artifactd
```

Requires Go 1.22+ (uses method+wildcard patterns in `net/http`).

## Configuration (environment)

| Variable | Default | Meaning |
|---|---|---|
| `ARTIFACTD_ADDR` | `:8080` | Listen address |
| `ARTIFACTD_DATA_ROOT` | `./data` | Directory that holds all repos (created if missing) |
| `ARTIFACTD_MAX_UPLOAD_BYTES` | `268435456` (256 MiB) | Hard per-upload size limit (413 if exceeded) |
| `ARTIFACTD_ALLOW_OVERWRITE` | `false` | Allow `PUT` over an existing artifact (otherwise 409) |
| `ARTIFACTD_READ_TIMEOUT` | `10m` | Max time to read a request (covers slow uploads) |
| `ARTIFACTD_WRITE_TIMEOUT` | `10m` | Max time to write a response (covers slow downloads) |
| `ARTIFACTD_IDLE_TIMEOUT` | `2m` | Keep-alive idle timeout |
| `ARTIFACTD_MAX_PATH_DEPTH` | `32` | Max number of path segments in an artifact path |
| `ARTIFACTD_MAX_PATH_LEN` | `1024` | Max artifact path length in characters |

Header size is capped at 1 MiB and header read at 10 s. `SIGINT`/`SIGTERM` trigger a graceful shutdown (15 s drain).

## On-disk layout

```
$ARTIFACTD_DATA_ROOT/
  <repo>/
    <any/nested/path>
```

A repo is a directory; an artifact is a regular file. Uploads are written to a temp file (`.upload-*`) in the target directory, fsynced, then atomically renamed into place, so readers never see a partial file. Deleting an artifact prunes any now-empty parent directories up to the repo root.

## API

All error responses are JSON: `{"error": "..."}`.

### Health

| Method | Path | Result |
|---|---|---|
| `GET` | `/healthz` | `200 {"status":"ok"}` |

### Repositories

| Method | Path | Result |
|---|---|---|
| `PUT` | `/api/repos/{repo}` | `201` created, `200` already existed, `400` invalid name |
| `GET` | `/api/repos` | `200 {"repos":["a","b"]}` |
| `GET` | `/api/repos/{repo}` | `200 {"name","files","bytes","created_at"}` (best-effort walk), `404` |
| `DELETE` | `/api/repos/{repo}` | `204` deleted, `409` not empty, `404` |

Repo names must match `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`.

### Artifacts

| Method | Path | Result |
|---|---|---|
| `PUT` | `/api/repos/{repo}/artifacts/{path}` | Upload raw request body. `201` with `{"path","size","modified_at","sha256"}` and `X-Checksum-SHA256` header. `409` if exists (unless overwrite enabled) or path is a directory. `413` too large. `404` unknown repo. |
| `GET` | `/api/repos/{repo}/artifacts/{path}` | Download (`application/octet-stream`, `Content-Disposition: attachment`). Supports `Range`, `If-None-Match`, `If-Modified-Since`. `404` if missing. |
| `HEAD` | `/api/repos/{repo}/artifacts/{path}` | Same headers as `GET`, no body. |
| `DELETE` | `/api/repos/{repo}/artifacts/{path}` | `204`, `404` if missing, `409` if not a regular file. |
| `GET` | `/api/repos/{repo}/artifacts/` | List repo root: `{"repo","prefix","entries":[{"name","is_dir","size"}]}` |
| `GET` | `/api/repos/{repo}/artifacts/{dir}/` | List a subdirectory (trailing slash). |
| `GET` | `/api/repos/{repo}/artifacts/...?recursive=1` | Flat recursive listing: `{"files":[{"path","size","modified_at"}]}` |

Optional upload verification: send `X-Checksum-SHA256: <hex>` with a `PUT`; the upload is rejected with `400` if the computed digest differs (the file is not stored).

### Path safety

Artifact paths are validated before touching the filesystem: no empty, `.` or `..` segments; no backslashes, NUL, or control characters; no leading/trailing whitespace in a segment; length and depth limits. The resolved path is additionally checked to be under the repo directory, and directories along the way must be real directories (not symlinks), so a symlink planted in the data root cannot redirect writes elsewhere.

## Examples

```sh
B=http://localhost:8080

curl -X PUT $B/api/repos/tools
curl -X PUT --data-binary @mytool-1.2.0-linux-amd64 \
     $B/api/repos/tools/artifacts/mytool/1.2.0/mytool-linux-amd64

curl -O $B/api/repos/tools/artifacts/mytool/1.2.0/mytool-linux-amd64
curl "$B/api/repos/tools/artifacts/?recursive=1"
curl $B/api/repos/tools

curl -X DELETE $B/api/repos/tools/artifacts/mytool/1.2.0/mytool-linux-amd64
curl -X DELETE $B/api/repos/tools
```

## Non-goals

No authentication, no content-addressed storage/dedup, no metadata index, no multipart uploads. Put it behind a reverse proxy if it needs to be exposed beyond localhost.
