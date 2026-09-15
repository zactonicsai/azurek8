/*
 * artifactd client: upload, download and list. Requires libcurl.
 *
 *   artifactd-cli [--url URL] mkrepo   <repo>
 *   artifactd-cli [--url URL] upload   <repo> <path> <file> [--no-checksum]
 *   artifactd-cli [--url URL] download <repo> <path> [outfile]
 *   artifactd-cli [--url URL] list     <repo> [prefix] [-r]
 *
 * URL defaults to $ARTIFACTD_URL or http://localhost:8080.
 *
 * Build:  make            (or: cc -O2 -o artifactd-cli artifactd_client.c -lcurl)
 *
 * The listing command prints the server's JSON as-is; parse it with your JSON
 * library of choice if you need structured output.
 *
 * The public functions (artifactd_upload / artifactd_download / artifactd_list)
 * can be used as a tiny library: drop the file in, define ARTIFACTD_NO_MAIN.
 */
#include <curl/curl.h>
#include <errno.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#define ERRBUF 512

/* ---------------------------------------------------------------------------
 * Small helpers
 * ------------------------------------------------------------------------- */

struct membuf { char *data; size_t len; };

static size_t mem_write(char *ptr, size_t size, size_t nmemb, void *ud) {
    struct membuf *m = ud;
    size_t n = size * nmemb;
    char *p = realloc(m->data, m->len + n + 1);
    if (!p) return 0;
    m->data = p;
    memcpy(m->data + m->len, ptr, n);
    m->len += n;
    m->data[m->len] = '\0';
    return n;
}

static size_t file_write(char *ptr, size_t size, size_t nmemb, void *ud) {
    return fwrite(ptr, size, nmemb, (FILE *)ud);
}

static size_t file_read(char *ptr, size_t size, size_t nmemb, void *ud) {
    return fread(ptr, size, nmemb, (FILE *)ud);
}

/* Build ".../api/repos/<repo>/artifacts/<path>[?query]" with each path segment escaped. */
static char *artifact_url(CURL *c, const char *base, const char *repo, const char *path, const char *query) {
    size_t cap = strlen(base) + strlen(repo) * 3 + strlen(path) * 3 + (query ? strlen(query) : 0) + 64;
    char *url = malloc(cap);
    if (!url) return NULL;
    char *r = curl_easy_escape(c, repo, 0);
    if (!r) { free(url); return NULL; }
    snprintf(url, cap, "%s/api/repos/%s/artifacts/", base, r);
    curl_free(r);

    const char *p = path;
    while (*p) {
        const char *slash = strchr(p, '/');
        size_t seglen = slash ? (size_t)(slash - p) : strlen(p);
        /* skip empty and "." segments: they would make the server redirect */
        if (seglen == 0 || (seglen == 1 && *p == '.')) {
            if (slash) { p = slash + 1; continue; } else break;
        }
        /* strip a trailing "/" left by a previous segment when we are the first real one */
        if (url[strlen(url) - 1] != '/') strncat(url, "/", cap - strlen(url) - 1);
        char *seg = strndup(p, seglen);
        char *esc = seg ? curl_easy_escape(c, seg, (int)seglen) : NULL;
        free(seg);
        if (!esc) { free(url); return NULL; }
        strncat(url, esc, cap - strlen(url) - 1);
        curl_free(esc);
        if (slash) p = slash + 1; else break;
    }
    /* a trailing slash in `path` means "list this directory": keep it */
    if (*path && path[strlen(path) - 1] == '/' && url[strlen(url) - 1] != '/')
        strncat(url, "/", cap - strlen(url) - 1);
    if (query) { strncat(url, "?", cap - strlen(url) - 1); strncat(url, query, cap - strlen(url) - 1); }
    return url;
}

/* Pull "error":"..." out of a JSON error body; falls back to raw body. */
static void extract_error(const char *body, long status, char *err, size_t errlen) {
    const char *k = body ? strstr(body, "\"error\"") : NULL;
    const char *q = k ? strchr(k + 7, '"') : NULL;
    if (q) {
        const char *end = strchr(q + 1, '"');
        if (end) { snprintf(err, errlen, "HTTP %ld: %.*s", status, (int)(end - q - 1), q + 1); return; }
    }
    snprintf(err, errlen, "HTTP %ld: %s", status, body && *body ? body : "request failed");
}

static const char *base_url(const char *explicit) {
    if (explicit && *explicit) return explicit;
    const char *e = getenv("ARTIFACTD_URL");
    return (e && *e) ? e : "http://localhost:8080";
}

/* SHA-256 (public domain style compact implementation) for the optional
 * X-Checksum-SHA256 header, so we don't need OpenSSL. */
typedef struct { unsigned int h[8]; unsigned long long len; unsigned char buf[64]; size_t n; } sha256_t;
static const unsigned int K[64] = {
 0x428a2f98,0x71374491,0xb5c0fbcf,0xe9b5dba5,0x3956c25b,0x59f111f1,0x923f82a4,0xab1c5ed5,0xd807aa98,0x12835b01,0x243185be,0x550c7dc3,0x72be5d74,0x80deb1fe,0x9bdc06a7,0xc19bf174,
 0xe49b69c1,0xefbe4786,0x0fc19dc6,0x240ca1cc,0x2de92c6f,0x4a7484aa,0x5cb0a9dc,0x76f988da,0x983e5152,0xa831c66d,0xb00327c8,0xbf597fc7,0xc6e00bf3,0xd5a79147,0x06ca6351,0x14292967,
 0x27b70a85,0x2e1b2138,0x4d2c6dfc,0x53380d13,0x650a7354,0x766a0abb,0x81c2c92e,0x92722c85,0xa2bfe8a1,0xa81a664b,0xc24b8b70,0xc76c51a3,0xd192e819,0xd6990624,0xf40e3585,0x106aa070,
 0x19a4c116,0x1e376c08,0x2748774c,0x34b0bcb5,0x391c0cb3,0x4ed8aa4a,0x5b9cca4f,0x682e6ff3,0x748f82ee,0x78a5636f,0x84c87814,0x8cc70208,0x90befffa,0xa4506ceb,0xbef9a3f7,0xc67178f2};
#define ROR(x,n) (((x)>>(n))|((x)<<(32-(n))))
static void sha256_block(sha256_t *s, const unsigned char *p) {
    unsigned int w[64], a,b,c,d,e,f,g,h,i;
    for (i=0;i<16;i++) w[i]=(unsigned)p[4*i]<<24|(unsigned)p[4*i+1]<<16|(unsigned)p[4*i+2]<<8|p[4*i+3];
    for (;i<64;i++){unsigned s0=ROR(w[i-15],7)^ROR(w[i-15],18)^(w[i-15]>>3),s1=ROR(w[i-2],17)^ROR(w[i-2],19)^(w[i-2]>>10);w[i]=w[i-16]+s0+w[i-7]+s1;}
    a=s->h[0];b=s->h[1];c=s->h[2];d=s->h[3];e=s->h[4];f=s->h[5];g=s->h[6];h=s->h[7];
    for (i=0;i<64;i++){unsigned t1=h+(ROR(e,6)^ROR(e,11)^ROR(e,25))+((e&f)^(~e&g))+K[i]+w[i],t2=(ROR(a,2)^ROR(a,13)^ROR(a,22))+((a&b)^(a&c)^(b&c));h=g;g=f;f=e;e=d+t1;d=c;c=b;b=a;a=t1+t2;}
    s->h[0]+=a;s->h[1]+=b;s->h[2]+=c;s->h[3]+=d;s->h[4]+=e;s->h[5]+=f;s->h[6]+=g;s->h[7]+=h;
}
static void sha256_init(sha256_t *s){static const unsigned int iv[8]={0x6a09e667,0xbb67ae85,0x3c6ef372,0xa54ff53a,0x510e527f,0x9b05688c,0x1f83d9ab,0x5be0cd19};memcpy(s->h,iv,32);s->len=0;s->n=0;}
static void sha256_update(sha256_t *s,const unsigned char *d,size_t n){s->len+=n;while(n){size_t k=64-s->n;if(k>n)k=n;memcpy(s->buf+s->n,d,k);s->n+=k;d+=k;n-=k;if(s->n==64){sha256_block(s,s->buf);s->n=0;}}}
static void sha256_hex(sha256_t *s,char out[65]){unsigned char pad[64]={0x80};size_t padlen=(s->n<56)?56-s->n:120-s->n;unsigned long long bits=s->len*8;unsigned char lb[8];int i;for(i=0;i<8;i++)lb[i]=(unsigned char)(bits>>(56-8*i));sha256_update(s,pad,padlen);sha256_update(s,lb,8);for(i=0;i<8;i++)sprintf(out+8*i,"%08x",s->h[i]);}

static int sha256_file(const char *fn, char out[65]) {
    FILE *f = fopen(fn, "rb");
    if (!f) return -1;
    sha256_t s; sha256_init(&s);
    unsigned char buf[1 << 16]; size_t n;
    while ((n = fread(buf, 1, sizeof buf, f)) > 0) sha256_update(&s, buf, n);
    fclose(f);
    sha256_hex(&s, out);
    return 0;
}

/* ---------------------------------------------------------------------------
 * Public API. All return 0 on success, -1 on failure with `err` filled in.
 * ------------------------------------------------------------------------- */

/* Create a repository. On success sets *created to 1 (new) or 0 (already existed). */
int artifactd_create_repo(const char *base, const char *repo, int *created, char *err, size_t errlen) {
    int rc = -1;
    CURL *c = curl_easy_init();
    if (!c) { snprintf(err, errlen, "curl init failed"); return -1; }
    char *r = curl_easy_escape(c, repo, 0);
    const char *b = base_url(base);
    size_t cap = strlen(b) + strlen(r) + 32;
    char *url = malloc(cap);
    snprintf(url, cap, "%s/api/repos/%s", b, r);
    curl_free(r);
    struct membuf resp = {0};
    curl_easy_setopt(c, CURLOPT_URL, url);
    curl_easy_setopt(c, CURLOPT_CUSTOMREQUEST, "PUT");
    curl_easy_setopt(c, CURLOPT_WRITEFUNCTION, mem_write);
    curl_easy_setopt(c, CURLOPT_WRITEDATA, &resp);
    CURLcode cc = curl_easy_perform(c);
    long status = 0;
    curl_easy_getinfo(c, CURLINFO_RESPONSE_CODE, &status);
    if (cc != CURLE_OK) snprintf(err, errlen, "%s", curl_easy_strerror(cc));
    else if (status < 200 || status >= 300) extract_error(resp.data, status, err, errlen);
    else { if (created) *created = (status == 201); rc = 0; }
    free(resp.data); free(url); curl_easy_cleanup(c);
    return rc;
}

int artifactd_upload(const char *base, const char *repo, const char *path, const char *file,
                     int verify_checksum, char *err, size_t errlen, struct membuf *response) {
    int rc = -1;
    FILE *f = fopen(file, "rb");
    if (!f) { snprintf(err, errlen, "open %s: %s", file, strerror(errno)); return -1; }
    fseek(f, 0, SEEK_END); long size = ftell(f); fseek(f, 0, SEEK_SET);

    CURL *c = curl_easy_init();
    if (!c) { fclose(f); snprintf(err, errlen, "curl init failed"); return -1; }
    char *url = artifact_url(c, base_url(base), repo, path, NULL);
    struct curl_slist *hdrs = curl_slist_append(NULL, "Content-Type: application/octet-stream");
    hdrs = curl_slist_append(hdrs, "Expect:");
    char sumhdr[96];
    if (verify_checksum) {
        char hex[65];
        if (sha256_file(file, hex) == 0) {
            snprintf(sumhdr, sizeof sumhdr, "X-Checksum-SHA256: %s", hex);
            hdrs = curl_slist_append(hdrs, sumhdr);
        }
    }
    struct membuf resp = {0};
    curl_easy_setopt(c, CURLOPT_URL, url);
    curl_easy_setopt(c, CURLOPT_UPLOAD, 1L);
    curl_easy_setopt(c, CURLOPT_CUSTOMREQUEST, "PUT");
    curl_easy_setopt(c, CURLOPT_READFUNCTION, file_read);
    curl_easy_setopt(c, CURLOPT_READDATA, f);
    curl_easy_setopt(c, CURLOPT_INFILESIZE_LARGE, (curl_off_t)size);
    curl_easy_setopt(c, CURLOPT_HTTPHEADER, hdrs);
    curl_easy_setopt(c, CURLOPT_WRITEFUNCTION, mem_write);
    curl_easy_setopt(c, CURLOPT_WRITEDATA, &resp);
    curl_easy_setopt(c, CURLOPT_FOLLOWLOCATION, 0L);

    CURLcode cc = curl_easy_perform(c);
    long status = 0;
    curl_easy_getinfo(c, CURLINFO_RESPONSE_CODE, &status);
    if (cc != CURLE_OK) snprintf(err, errlen, "%s", curl_easy_strerror(cc));
    else if (status < 200 || status >= 300) extract_error(resp.data, status, err, errlen);
    else { rc = 0; if (response) { *response = resp; resp.data = NULL; } }

    free(resp.data); free(url); curl_slist_free_all(hdrs); curl_easy_cleanup(c); fclose(f);
    return rc;
}

int artifactd_download(const char *base, const char *repo, const char *path, const char *outfile,
                       char *err, size_t errlen) {
    int rc = -1;
    const char *slash = strrchr(path, '/');
    const char *dflt = slash ? slash + 1 : path;
    if (!outfile || !*outfile) outfile = dflt;
    size_t tl = strlen(outfile) + 6;
    char *tmp = malloc(tl);
    snprintf(tmp, tl, "%s.part", outfile);
    FILE *out = fopen(tmp, "wb");
    if (!out) { snprintf(err, errlen, "create %s: %s", tmp, strerror(errno)); free(tmp); return -1; }

    CURL *c = curl_easy_init();
    char *url = artifact_url(c, base_url(base), repo, path, NULL);
    curl_easy_setopt(c, CURLOPT_URL, url);
    curl_easy_setopt(c, CURLOPT_WRITEFUNCTION, file_write);
    curl_easy_setopt(c, CURLOPT_WRITEDATA, out);
    curl_easy_setopt(c, CURLOPT_FAILONERROR, 0L);

    CURLcode cc = curl_easy_perform(c);
    long status = 0;
    curl_easy_getinfo(c, CURLINFO_RESPONSE_CODE, &status);
    fclose(out);
    if (cc != CURLE_OK) snprintf(err, errlen, "%s", curl_easy_strerror(cc));
    else if (status < 200 || status >= 300) {
        /* the body we wrote is the JSON error; read it back for the message */
        FILE *e = fopen(tmp, "rb"); char body[ERRBUF] = {0};
        if (e) { size_t n = fread(body, 1, sizeof body - 1, e); body[n] = 0; fclose(e); }
        extract_error(body, status, err, errlen);
    } else if (rename(tmp, outfile) != 0) snprintf(err, errlen, "rename: %s", strerror(errno));
    else rc = 0;
    if (rc != 0) remove(tmp);

    free(url); free(tmp); curl_easy_cleanup(c);
    return rc;
}

/* `prefix` may be "" for the repo root. Result JSON is placed in `out` (caller frees out->data). */
int artifactd_list(const char *base, const char *repo, const char *prefix, int recursive,
                   struct membuf *out, char *err, size_t errlen) {
    int rc = -1;
    /* append a "/" so the server lists the directory (artifact_url drops empty segments) */
    int has_real = 0;
    for (const char *q = prefix; *q; q++) if (*q != '/') { has_real = 1; break; }
    size_t pl = strlen(prefix);
    char *p = malloc(pl + 2);
    memcpy(p, prefix, pl); p[pl] = 0;
    if (has_real) strcat(p, "/");

    CURL *c = curl_easy_init();
    char *url = artifact_url(c, base_url(base), repo, p, recursive ? "recursive=1" : NULL);
    struct membuf resp = {0};
    curl_easy_setopt(c, CURLOPT_URL, url);
    curl_easy_setopt(c, CURLOPT_WRITEFUNCTION, mem_write);
    curl_easy_setopt(c, CURLOPT_WRITEDATA, &resp);

    CURLcode cc = curl_easy_perform(c);
    long status = 0;
    curl_easy_getinfo(c, CURLINFO_RESPONSE_CODE, &status);
    if (cc != CURLE_OK) snprintf(err, errlen, "%s", curl_easy_strerror(cc));
    else if (status < 200 || status >= 300) extract_error(resp.data, status, err, errlen);
    else { *out = resp; resp.data = NULL; rc = 0; }

    free(resp.data); free(url); free(p); curl_easy_cleanup(c);
    return rc;
}

/* ---------------------------------------------------------------------------
 * CLI
 * ------------------------------------------------------------------------- */
#ifndef ARTIFACTD_NO_MAIN

static int usage(void) {
    fprintf(stderr,
        "usage:\n"
        "  artifactd-cli [--url URL] mkrepo   <repo>\n"
        "  artifactd-cli [--url URL] upload   <repo> <path> <file> [--no-checksum]\n"
        "  artifactd-cli [--url URL] download <repo> <path> [outfile]\n"
        "  artifactd-cli [--url URL] list     <repo> [prefix] [-r]\n");
    return 2;
}

int main(int argc, char **argv) {
    const char *url = NULL;
    int recursive = 0, checksum = 1;
    char *pos[8]; int np = 0;
    for (int i = 1; i < argc; i++) {
        if (!strcmp(argv[i], "--url") && i + 1 < argc) url = argv[++i];
        else if (!strcmp(argv[i], "-r") || !strcmp(argv[i], "--recursive")) recursive = 1;
        else if (!strcmp(argv[i], "--no-checksum")) checksum = 0;
        else if (np < 8) pos[np++] = argv[i];
    }
    if (np < 1) return usage();

    curl_global_init(CURL_GLOBAL_DEFAULT);
    char err[ERRBUF] = {0};
    int rc = 0;

    if (!strcmp(pos[0], "mkrepo")) {
        if (np != 2) return usage();
        int created = 0;
        if (artifactd_create_repo(url, pos[1], &created, err, sizeof err) == 0)
            printf("repo %s %s\n", pos[1], created ? "created" : "already exists");
        else rc = 1;
    } else if (!strcmp(pos[0], "upload")) {
        if (np != 4) return usage();
        struct membuf resp = {0};
        if (artifactd_upload(url, pos[1], pos[2], pos[3], checksum, err, sizeof err, &resp) == 0) {
            printf("uploaded: %s\n", resp.data ? resp.data : "");
            free(resp.data);
        } else rc = 1;
    } else if (!strcmp(pos[0], "download")) {
        if (np < 3 || np > 4) return usage();
        const char *out = np == 4 ? pos[3] : NULL;
        if (artifactd_download(url, pos[1], pos[2], out, err, sizeof err) == 0) {
            const char *slash = strrchr(pos[2], '/');
            printf("downloaded to %s\n", out ? out : (slash ? slash + 1 : pos[2]));
        } else rc = 1;
    } else if (!strcmp(pos[0], "list")) {
        if (np < 2 || np > 3) return usage();
        struct membuf out = {0};
        if (artifactd_list(url, pos[1], np == 3 ? pos[2] : "", recursive, &out, err, sizeof err) == 0) {
            fputs(out.data, stdout);
            free(out.data);
        } else rc = 1;
    } else rc = usage();

    if (rc == 1) fprintf(stderr, "error: %s\n", err);
    curl_global_cleanup();
    return rc;
}
#endif
