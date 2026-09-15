// artifactd client: upload, download and list. .NET 8, no packages.
//
//   artifactd-cli [--url URL] mkrepo   <repo>
//   artifactd-cli [--url URL] upload   <repo> <path> <file> [--no-checksum]
//   artifactd-cli [--url URL] download <repo> <path> [outfile]
//   artifactd-cli [--url URL] list     <repo> [prefix] [-r]
//
// URL defaults to $ARTIFACTD_URL or http://localhost:8080.

using System.Net;
using System.Net.Http.Headers;
using System.Net.Http.Json;
using System.Security.Cryptography;
using System.Text.Json;
using System.Text.Json.Serialization;

namespace Artifactd;

public sealed class ArtifactdException : Exception
{
    public int Status { get; }
    public ArtifactdException(int status, string message) : base($"HTTP {status}: {message}") => Status = status;
}

public sealed record ArtifactInfo(
    [property: JsonPropertyName("path")] string Path,
    [property: JsonPropertyName("size")] long Size,
    [property: JsonPropertyName("modified_at")] DateTimeOffset ModifiedAt,
    [property: JsonPropertyName("sha256")] string? Sha256);

public sealed record DirEntry(
    [property: JsonPropertyName("name")] string Name,
    [property: JsonPropertyName("is_dir")] bool IsDir,
    [property: JsonPropertyName("size")] long Size);

public sealed class ArtifactdClient : IDisposable
{
    private readonly HttpClient _http;
    public string BaseUrl { get; }

    public ArtifactdClient(string? baseUrl = null, HttpClient? http = null)
    {
        var b = string.IsNullOrEmpty(baseUrl) ? Environment.GetEnvironmentVariable("ARTIFACTD_URL") : baseUrl;
        if (string.IsNullOrEmpty(b)) b = "http://localhost:8080";
        BaseUrl = b.TrimEnd('/');
        _http = http ?? new HttpClient { Timeout = TimeSpan.FromMinutes(10) };
    }

    public void Dispose() => _http.Dispose();

    // -- helpers ------------------------------------------------------------

    /// <summary>Drop empty and "." segments; empty segments make the server redirect instead of serving.</summary>
    private static string CleanPath(string path) =>
        string.Join("/", path.Replace('\\', '/').Split('/').Where(s => s.Length > 0 && s != "."));

    private string ArtifactUrl(string repo, string path, string? query = null)
    {
        var dir = path.EndsWith('/');
        path = CleanPath(path);
        var segs = path.Length == 0 ? "" : string.Join("/", path.Split('/').Select(Uri.EscapeDataString)) + (dir ? "/" : "");
        var url = $"{BaseUrl}/api/repos/{Uri.EscapeDataString(repo)}/artifacts/{segs}";
        return query is null ? url : $"{url}?{query}";
    }

    private static async Task EnsureSuccess(HttpResponseMessage resp)
    {
        if (resp.IsSuccessStatusCode) return;
        var body = await resp.Content.ReadAsStringAsync();
        var msg = body.Trim();
        try
        {
            using var doc = JsonDocument.Parse(body);
            if (doc.RootElement.TryGetProperty("error", out var e)) msg = e.GetString() ?? msg;
        }
        catch (JsonException) { }
        if (msg.Length == 0) msg = resp.ReasonPhrase ?? "request failed";
        throw new ArtifactdException((int)resp.StatusCode, msg);
    }

    private static async Task<string> Sha256Hex(string file)
    {
        await using var fs = File.OpenRead(file);
        var hash = await SHA256.HashDataAsync(fs);
        return Convert.ToHexString(hash).ToLowerInvariant();
    }

    // -- API ----------------------------------------------------------------

    /// <summary>Create a repository. Returns true if created, false if it already existed.</summary>
    public async Task<bool> CreateRepoAsync(string repo, CancellationToken ct = default)
    {
        using var req = new HttpRequestMessage(HttpMethod.Put, $"{BaseUrl}/api/repos/{Uri.EscapeDataString(repo)}");
        using var resp = await _http.SendAsync(req, ct);
        await EnsureSuccess(resp);
        return resp.StatusCode == HttpStatusCode.Created;
    }

    public async Task<ArtifactInfo> UploadAsync(string repo, string path, string file, bool verifyChecksum = true, CancellationToken ct = default)
    {
        string? sum = verifyChecksum ? await Sha256Hex(file) : null;
        await using var fs = File.OpenRead(file);
        using var content = new StreamContent(fs, 1 << 20);
        content.Headers.ContentType = new MediaTypeHeaderValue("application/octet-stream");
        content.Headers.ContentLength = fs.Length;
        using var req = new HttpRequestMessage(HttpMethod.Put, ArtifactUrl(repo, path)) { Content = content };
        if (sum is not null) req.Headers.TryAddWithoutValidation("X-Checksum-SHA256", sum);
        using var resp = await _http.SendAsync(req, ct);
        await EnsureSuccess(resp);
        return (await resp.Content.ReadFromJsonAsync<ArtifactInfo>(cancellationToken: ct))!;
    }

    public async Task<string> DownloadAsync(string repo, string path, string? outFile = null, CancellationToken ct = default)
    {
        outFile ??= path[(path.LastIndexOf('/') + 1)..];
        var tmp = outFile + ".part";
        using var resp = await _http.GetAsync(ArtifactUrl(repo, path), HttpCompletionOption.ResponseHeadersRead, ct);
        await EnsureSuccess(resp);
        await using (var src = await resp.Content.ReadAsStreamAsync(ct))
        await using (var dst = File.Create(tmp))
        {
            await src.CopyToAsync(dst, ct);
        }
        File.Move(tmp, outFile, overwrite: true);
        return outFile;
    }

    /// <summary>List one directory of the repo (prefix "" = root).</summary>
    public async Task<List<DirEntry>> ListAsync(string repo, string prefix = "", CancellationToken ct = default)
    {
        var p = CleanPath(prefix);
        using var resp = await _http.GetAsync(ArtifactUrl(repo, p.Length == 0 ? "" : p + "/"), ct);
        await EnsureSuccess(resp);
        using var doc = JsonDocument.Parse(await resp.Content.ReadAsStringAsync(ct));
        return doc.RootElement.GetProperty("entries").Deserialize<List<DirEntry>>() ?? new();
    }

    /// <summary>List every file under prefix.</summary>
    public async Task<List<ArtifactInfo>> ListRecursiveAsync(string repo, string prefix = "", CancellationToken ct = default)
    {
        var p = CleanPath(prefix);
        using var resp = await _http.GetAsync(ArtifactUrl(repo, p.Length == 0 ? "" : p + "/", "recursive=1"), ct);
        await EnsureSuccess(resp);
        using var doc = JsonDocument.Parse(await resp.Content.ReadAsStringAsync(ct));
        return doc.RootElement.GetProperty("files").Deserialize<List<ArtifactInfo>>() ?? new();
    }
}

// -- CLI ------------------------------------------------------------------

public static class Program
{
    private static int Usage()
    {
        Console.Error.WriteLine(
            "usage:\n" +
            "  artifactd-cli [--url URL] mkrepo   <repo>\n" +
            "  artifactd-cli [--url URL] upload   <repo> <path> <file> [--no-checksum]\n" +
            "  artifactd-cli [--url URL] download <repo> <path> [outfile]\n" +
            "  artifactd-cli [--url URL] list     <repo> [prefix] [-r]");
        return 2;
    }

    public static async Task<int> Main(string[] argv)
    {
        var args = argv.ToList();
        string? url = null;
        bool recursive = false, noChecksum = false;
        for (int i = 0; i < args.Count;)
        {
            if (args[i] == "--url" && i + 1 < args.Count) { url = args[i + 1]; args.RemoveRange(i, 2); }
            else if (args[i] is "-r" or "--recursive") { recursive = true; args.RemoveAt(i); }
            else if (args[i] == "--no-checksum") { noChecksum = true; args.RemoveAt(i); }
            else i++;
        }
        if (args.Count == 0) return Usage();

        using var c = new ArtifactdClient(url);
        try
        {
            switch (args[0])
            {
                case "mkrepo":
                {
                    if (args.Count != 2) return Usage();
                    var created = await c.CreateRepoAsync(args[1]);
                    Console.WriteLine($"repo {args[1]} {(created ? "created" : "already exists")}");
                    return 0;
                }
                case "upload":
                {
                    if (args.Count != 4) return Usage();
                    var info = await c.UploadAsync(args[1], args[2], args[3], !noChecksum);
                    Console.WriteLine($"uploaded {info.Path} ({info.Size} bytes) sha256={info.Sha256}");
                    return 0;
                }
                case "download":
                {
                    if (args.Count is < 3 or > 4) return Usage();
                    var outFile = await c.DownloadAsync(args[1], args[2], args.Count == 4 ? args[3] : null);
                    Console.WriteLine($"downloaded to {outFile} ({new FileInfo(outFile).Length} bytes)");
                    return 0;
                }
                case "list":
                {
                    if (args.Count is < 2 or > 3) return Usage();
                    var prefix = args.Count == 3 ? args[2] : "";
                    if (recursive)
                        foreach (var f in await c.ListRecursiveAsync(args[1], prefix))
                            Console.WriteLine($"{f.Size,12}  {f.Path}");
                    else
                        foreach (var e in await c.ListAsync(args[1], prefix))
                            Console.WriteLine($"{(e.IsDir ? "<dir>" : e.Size.ToString()),12}  {e.Name}");
                    return 0;
                }
                default:
                    return Usage();
            }
        }
        catch (ArtifactdException e)
        {
            Console.Error.WriteLine("error: " + e.Message);
            return 1;
        }
        catch (Exception e) when (e is HttpRequestException or IOException)
        {
            Console.Error.WriteLine("error: " + e.Message);
            return 1;
        }
    }
}
