# artifactd clients

Small upload / download / list utilities for the `artifactd` server, one per language.
Every client has the same CLI shape and reads the server URL from `--url` or `$ARTIFACTD_URL`
(default `http://localhost:8080`):

```
<cli> [--url URL] upload   <repo> <path> <file> [--no-checksum]
<cli> [--url URL] download <repo> <path> [outfile]
<cli> [--url URL] list     <repo> [prefix] [-r]
```

`upload` computes the file's SHA-256 client-side and sends it as `X-Checksum-SHA256` so the server
rejects corrupted transfers (`--no-checksum` skips this). `download` writes to `<outfile>.part` and
renames on success. `list` shows one directory level; `-r` lists every file under the prefix.

| Directory | Language / runtime | Build & run | Dependencies |
|---|---|---|---|
| `python/` | Python 3.8+ | `python3 artifactd_client.py …` | none |
| `go/` | Go 1.22+ | `go build -o artifactd-cli .` | none |
| `java/` | Java 11+ | `java ArtifactdClient.java …` (or `javac` + `java ArtifactdClient`) | none |
| `dotnet/` | .NET 8 | `dotnet run -- …` or `dotnet build` | none |
| `c/` | C11 | `make` | libcurl (`libcurl4-openssl-dev` / `curl-devel`) |

Each file also exposes a small library API (`ArtifactdClient` class / `Client` struct /
`artifactd_*` functions) if you want to embed it rather than shell out.

Note: the C `list` command prints the server's raw JSON rather than a formatted table, to avoid
adding a JSON parser dependency.

## Quick check

```sh
export ARTIFACTD_URL=http://localhost:8080
curl -X PUT $ARTIFACTD_URL/api/repos/tools      # create repo (any client)

python3 python/artifactd_client.py upload tools demo/hello.bin ./hello.bin
python3 python/artifactd_client.py list tools -r
python3 python/artifactd_client.py download tools demo/hello.bin ./hello.copy
```
