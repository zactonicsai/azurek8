#!/usr/bin/env python3
"""artifactd client: upload, download and list artifacts. Standard library only.

Library use:
    from artifactd_client import ArtifactdClient
    c = ArtifactdClient("http://localhost:8080")
    c.upload("tools", "mytool/1.0/mytool", "./mytool")
    c.download("tools", "mytool/1.0/mytool", "./mytool.dl")
    for f in c.list("tools", recursive=True): print(f["path"], f["size"])

CLI use:
    artifactd_client.py [--url URL] upload   <repo> <path> <file> [--overwrite-check]
    artifactd_client.py [--url URL] download <repo> <path> [outfile]
    artifactd_client.py [--url URL] list     <repo> [prefix] [--recursive]

URL defaults to $ARTIFACTD_URL or http://localhost:8080.
"""
import argparse
import hashlib
import json
import os
import shutil
import sys
import urllib.error
import urllib.parse
import urllib.request

CHUNK = 1 << 20


class ArtifactdError(Exception):
    def __init__(self, status, message):
        super().__init__(f"HTTP {status}: {message}")
        self.status = status
        self.message = message


class ArtifactdClient:
    def __init__(self, base_url=None, timeout=600):
        self.base_url = (base_url or os.environ.get("ARTIFACTD_URL") or "http://localhost:8080").rstrip("/")
        self.timeout = timeout

    # -- helpers ------------------------------------------------------------

    def _url(self, repo, path=""):
        # quote each segment but keep "/" separators
        quoted = "/".join(urllib.parse.quote(seg, safe="") for seg in path.split("/")) if path else ""
        return f"{self.base_url}/api/repos/{urllib.parse.quote(repo, safe='')}/artifacts/{quoted}"

    def _request(self, req):
        try:
            return urllib.request.urlopen(req, timeout=self.timeout)
        except urllib.error.HTTPError as e:
            body = e.read().decode("utf-8", "replace")
            try:
                msg = json.loads(body).get("error", body)
            except ValueError:
                msg = body.strip() or e.reason
            raise ArtifactdError(e.code, msg) from None

    # -- API ----------------------------------------------------------------

    def upload(self, repo, path, filename, verify_checksum=True):
        """Upload a local file. Returns the server's JSON (path, size, sha256)."""
        size = os.path.getsize(filename)
        headers = {"Content-Type": "application/octet-stream", "Content-Length": str(size)}
        if verify_checksum:
            h = hashlib.sha256()
            with open(filename, "rb") as f:
                for chunk in iter(lambda: f.read(CHUNK), b""):
                    h.update(chunk)
            headers["X-Checksum-SHA256"] = h.hexdigest()
        with open(filename, "rb") as f:
            req = urllib.request.Request(self._url(repo, path), data=f, method="PUT", headers=headers)
            with self._request(req) as resp:
                return json.loads(resp.read().decode("utf-8"))

    def download(self, repo, path, outfile=None):
        """Download an artifact to outfile (default: basename of path). Returns outfile."""
        outfile = outfile or os.path.basename(path)
        req = urllib.request.Request(self._url(repo, path), method="GET")
        tmp = outfile + ".part"
        with self._request(req) as resp, open(tmp, "wb") as f:
            shutil.copyfileobj(resp, f, CHUNK)
        os.replace(tmp, outfile)
        return outfile

    def list(self, repo, prefix="", recursive=False):
        """List a directory (entries) or, with recursive=True, all files under prefix."""
        p = prefix.strip("/")
        url = self._url(repo, p + "/" if p else "")
        if recursive:
            url += "?recursive=1"
        req = urllib.request.Request(url, method="GET")
        with self._request(req) as resp:
            data = json.loads(resp.read().decode("utf-8"))
        return data["files"] if recursive else data["entries"]


# -- CLI --------------------------------------------------------------------

def main(argv=None):
    ap = argparse.ArgumentParser(description="artifactd client")
    ap.add_argument("--url", help="server base URL (default $ARTIFACTD_URL or http://localhost:8080)")
    sub = ap.add_subparsers(dest="cmd", required=True)

    up = sub.add_parser("upload")
    up.add_argument("repo"); up.add_argument("path"); up.add_argument("file")
    up.add_argument("--no-checksum", action="store_true", help="skip client-side SHA-256 verification header")

    dl = sub.add_parser("download")
    dl.add_argument("repo"); dl.add_argument("path"); dl.add_argument("outfile", nargs="?")

    ls = sub.add_parser("list")
    ls.add_argument("repo"); ls.add_argument("prefix", nargs="?", default="")
    ls.add_argument("-r", "--recursive", action="store_true")

    args = ap.parse_args(argv)
    c = ArtifactdClient(args.url)
    try:
        if args.cmd == "upload":
            info = c.upload(args.repo, args.path, args.file, verify_checksum=not args.no_checksum)
            print(f"uploaded {info['path']} ({info['size']} bytes) sha256={info['sha256']}")
        elif args.cmd == "download":
            out = c.download(args.repo, args.path, args.outfile)
            print(f"downloaded to {out} ({os.path.getsize(out)} bytes)")
        elif args.cmd == "list":
            for e in c.list(args.repo, args.prefix, args.recursive):
                if args.recursive:
                    print(f"{e['size']:>12}  {e['path']}")
                else:
                    print(f"{'<dir>' if e['is_dir'] else e.get('size', 0):>12}  {e['name']}")
    except ArtifactdError as e:
        print(f"error: {e}", file=sys.stderr)
        return 1
    except (OSError, urllib.error.URLError) as e:
        print(f"error: {e}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
