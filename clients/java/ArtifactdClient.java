import java.io.IOException;
import java.io.InputStream;
import java.net.URI;
import java.net.URLEncoder;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.Paths;
import java.nio.file.StandardCopyOption;
import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;
import java.time.Duration;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.List;
import java.util.regex.Matcher;
import java.util.regex.Pattern;

/**
 * artifactd client: upload, download and list. Java 11+, no dependencies.
 *
 * <pre>
 *   java ArtifactdClient.java [--url URL] upload   &lt;repo&gt; &lt;path&gt; &lt;file&gt; [--no-checksum]
 *   java ArtifactdClient.java [--url URL] download &lt;repo&gt; &lt;path&gt; [outfile]
 *   java ArtifactdClient.java [--url URL] list     &lt;repo&gt; [prefix] [-r]
 * </pre>
 *
 * URL defaults to $ARTIFACTD_URL or http://localhost:8080.
 *
 * The listing JSON is parsed with a small regex-based extractor to stay
 * dependency free; if you already have Jackson/Gson on the classpath, swap
 * {@link #parseListing} for a real parser.
 */
public class ArtifactdClient {

    public static class ArtifactdException extends IOException {
        public final int status;
        public ArtifactdException(int status, String message) {
            super("HTTP " + status + ": " + message);
            this.status = status;
        }
    }

    /** One entry from a listing. For recursive listings {@code name} is the full path. */
    public static class Entry {
        public final String name;
        public final boolean isDir;
        public final long size;
        Entry(String name, boolean isDir, long size) { this.name = name; this.isDir = isDir; this.size = size; }
        @Override public String toString() { return (isDir ? "<dir>" : Long.toString(size)) + "  " + name; }
    }

    private final String baseUrl;
    private final HttpClient http;

    public ArtifactdClient(String baseUrl) {
        String b = baseUrl;
        if (b == null || b.isEmpty()) b = System.getenv("ARTIFACTD_URL");
        if (b == null || b.isEmpty()) b = "http://localhost:8080";
        this.baseUrl = b.replaceAll("/+$", "");
        this.http = HttpClient.newBuilder().connectTimeout(Duration.ofSeconds(30)).build();
    }

    // -- helpers ------------------------------------------------------------

    private static String seg(String s) {
        // URLEncoder is form-encoding; fix the two differences for path segments.
        return URLEncoder.encode(s, StandardCharsets.UTF_8).replace("+", "%20").replace("*", "%2A");
    }

    private URI artifactUri(String repo, String path, String query) {
        StringBuilder sb = new StringBuilder(baseUrl).append("/api/repos/").append(seg(repo)).append("/artifacts/");
        if (!path.isEmpty()) {
            String[] parts = path.split("/", -1);
            for (int i = 0; i < parts.length; i++) {
                if (i > 0) sb.append('/');
                sb.append(seg(parts[i]));
            }
        }
        if (query != null) sb.append('?').append(query);
        return URI.create(sb.toString());
    }

    private static void check(HttpResponse<?> resp, String bodyIfError) throws ArtifactdException {
        int st = resp.statusCode();
        if (st >= 200 && st < 300) return;
        String msg = bodyIfError == null ? "" : bodyIfError.trim();
        Matcher m = Pattern.compile("\"error\"\\s*:\\s*\"((?:[^\"\\\\]|\\\\.)*)\"").matcher(msg);
        if (m.find()) msg = m.group(1);
        throw new ArtifactdException(st, msg.isEmpty() ? "request failed" : msg);
    }

    private static String sha256Hex(Path file) throws IOException {
        try (InputStream in = Files.newInputStream(file)) {
            MessageDigest md = MessageDigest.getInstance("SHA-256");
            byte[] buf = new byte[1 << 20];
            int n;
            while ((n = in.read(buf)) > 0) md.update(buf, 0, n);
            StringBuilder sb = new StringBuilder();
            for (byte b : md.digest()) sb.append(String.format("%02x", b));
            return sb.toString();
        } catch (NoSuchAlgorithmException e) {
            throw new IOException(e);
        }
    }

    // -- API ----------------------------------------------------------------

    /** Upload a local file. Returns the server's JSON response body. */
    public String upload(String repo, String path, Path file, boolean verifyChecksum) throws IOException, InterruptedException {
        HttpRequest.Builder b = HttpRequest.newBuilder(artifactUri(repo, path, null))
                .timeout(Duration.ofMinutes(10))
                .header("Content-Type", "application/octet-stream")
                .PUT(HttpRequest.BodyPublishers.ofFile(file));
        if (verifyChecksum) b.header("X-Checksum-SHA256", sha256Hex(file));
        HttpResponse<String> resp = http.send(b.build(), HttpResponse.BodyHandlers.ofString());
        check(resp, resp.body());
        return resp.body();
    }

    /** Download an artifact to {@code out} (null = base name of path). Returns the output path. */
    public Path download(String repo, String path, Path out) throws IOException, InterruptedException {
        if (out == null) out = Paths.get(path.substring(path.lastIndexOf('/') + 1));
        Path tmp = out.resolveSibling(out.getFileName() + ".part");
        HttpRequest req = HttpRequest.newBuilder(artifactUri(repo, path, null))
                .timeout(Duration.ofMinutes(10)).GET().build();
        HttpResponse<InputStream> resp = http.send(req, HttpResponse.BodyHandlers.ofInputStream());
        try (InputStream in = resp.body()) {
            if (resp.statusCode() < 200 || resp.statusCode() >= 300) {
                check(resp, new String(in.readAllBytes(), StandardCharsets.UTF_8));
            }
            Files.copy(in, tmp, StandardCopyOption.REPLACE_EXISTING);
        }
        Files.move(tmp, out, StandardCopyOption.REPLACE_EXISTING, StandardCopyOption.ATOMIC_MOVE);
        return out;
    }

    /** List one directory (recursive=false) or every file under prefix (recursive=true). */
    public List<Entry> list(String repo, String prefix, boolean recursive) throws IOException, InterruptedException {
        String p = prefix == null ? "" : prefix.replaceAll("^/+|/+$", "");
        URI uri = artifactUri(repo, p.isEmpty() ? "" : p + "/", recursive ? "recursive=1" : null);
        HttpRequest req = HttpRequest.newBuilder(uri).timeout(Duration.ofMinutes(1)).GET().build();
        HttpResponse<String> resp = http.send(req, HttpResponse.BodyHandlers.ofString());
        check(resp, resp.body());
        return parseListing(resp.body(), recursive);
    }

    // Minimal JSON extraction for the two fixed listing shapes the server emits.
    private static final Pattern OBJ = Pattern.compile("\\{[^{}]*\\}");
    private static final Pattern STR = Pattern.compile("\"(name|path)\"\\s*:\\s*\"((?:[^\"\\\\]|\\\\.)*)\"");
    private static final Pattern SIZE = Pattern.compile("\"size\"\\s*:\\s*(\\d+)");
    private static final Pattern DIR = Pattern.compile("\"is_dir\"\\s*:\\s*(true|false)");

    static List<Entry> parseListing(String json, boolean recursive) {
        String key = recursive ? "\"files\"" : "\"entries\"";
        int start = json.indexOf(key);
        if (start < 0) return new ArrayList<>();
        Matcher objs = OBJ.matcher(json.substring(start));
        List<Entry> out = new ArrayList<>();
        while (objs.find()) {
            String o = objs.group();
            Matcher s = STR.matcher(o);
            if (!s.find()) continue;
            String name = s.group(2).replace("\\/", "/").replace("\\\"", "\"").replace("\\\\", "\\");
            Matcher sz = SIZE.matcher(o);
            long size = sz.find() ? Long.parseLong(sz.group(1)) : 0;
            Matcher d = DIR.matcher(o);
            boolean isDir = d.find() && d.group(1).equals("true");
            out.add(new Entry(name, isDir, size));
        }
        return out;
    }

    // -- CLI ----------------------------------------------------------------

    private static void usage() {
        System.err.println("usage:\n"
                + "  ArtifactdClient [--url URL] upload   <repo> <path> <file> [--no-checksum]\n"
                + "  ArtifactdClient [--url URL] download <repo> <path> [outfile]\n"
                + "  ArtifactdClient [--url URL] list     <repo> [prefix] [-r]");
        System.exit(2);
    }

    public static void main(String[] argv) throws Exception {
        List<String> args = new ArrayList<>(Arrays.asList(argv));
        String url = null;
        boolean recursive = false, noChecksum = false;
        for (int i = 0; i < args.size(); ) {
            String a = args.get(i);
            if (a.equals("--url") && i + 1 < args.size()) { url = args.get(i + 1); args.remove(i); args.remove(i); }
            else if (a.equals("-r") || a.equals("--recursive")) { recursive = true; args.remove(i); }
            else if (a.equals("--no-checksum")) { noChecksum = true; args.remove(i); }
            else i++;
        }
        if (args.isEmpty()) usage();
        ArtifactdClient c = new ArtifactdClient(url);
        try {
            switch (args.get(0)) {
                case "upload": {
                    if (args.size() != 4) usage();
                    String body = c.upload(args.get(1), args.get(2), Paths.get(args.get(3)), !noChecksum);
                    System.out.println("uploaded: " + body.trim());
                    break;
                }
                case "download": {
                    if (args.size() < 3 || args.size() > 4) usage();
                    Path out = c.download(args.get(1), args.get(2), args.size() == 4 ? Paths.get(args.get(3)) : null);
                    System.out.println("downloaded to " + out + " (" + Files.size(out) + " bytes)");
                    break;
                }
                case "list": {
                    if (args.size() < 2 || args.size() > 3) usage();
                    for (Entry e : c.list(args.get(1), args.size() == 3 ? args.get(2) : "", recursive)) {
                        System.out.printf("%12s  %s%n", e.isDir ? "<dir>" : Long.toString(e.size), e.name);
                    }
                    break;
                }
                default:
                    usage();
            }
        } catch (ArtifactdException e) {
            System.err.println("error: " + e.getMessage());
            System.exit(1);
        }
    }
}
