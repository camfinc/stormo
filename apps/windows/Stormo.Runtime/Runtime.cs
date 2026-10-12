using System.Diagnostics;
using System.Net.Sockets;
using System.Text.Json;

namespace Stormo.Runtime;

public record SetupCheck(string Key, string Title, bool Ready, string Detail);
public record Agent(string Id, string Name, string Unit, string State, string Health, string Model);
public delegate Task<string> CommandRunner(string executable, IReadOnlyList<string> args, int timeout);

public sealed class LocalRuntime
{
    public string Engine { get; }
    public string Instance { get; }
    private readonly CommandRunner runner;
    private readonly Func<string, Task<JsonElement>> reader;
    private bool verified;
    private static readonly HttpClient http = new(new HttpClientHandler { UseProxy = false, AllowAutoRedirect = false }) { Timeout = TimeSpan.FromSeconds(3) };

    public LocalRuntime(string assets, string? instance = null, CommandRunner? runner = null, Func<string, Task<JsonElement>>? reader = null)
    {
        Engine = Path.GetFullPath(Path.Combine(assets, "engine", "bin", "stormo.exe"));
        Instance = Path.GetFullPath(instance ?? Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.LocalApplicationData), "Stormo", "Instances", "default"));
        this.runner = runner ?? Run;
        this.reader = reader ?? ReadApi;
    }

    public static string? DockerExecutable()
    {
        var directories = new[] {
            Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.ProgramFiles), "Docker", "Docker", "resources", "bin"),
            Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.LocalApplicationData), "Programs", "DockerDesktop", "resources", "bin")
        }.Concat((Environment.GetEnvironmentVariable("PATH") ?? "").Split(Path.PathSeparator));
        return directories.Select(d => Path.Combine(d, "docker.exe")).FirstOrDefault(File.Exists);
    }

    public static ProcessStartInfo StartInfo(string executable, IReadOnlyList<string> args)
    {
        var info = new ProcessStartInfo(executable) { UseShellExecute = false, CreateNoWindow = true, RedirectStandardInput = true, RedirectStandardOutput = true, RedirectStandardError = true };
        foreach (var arg in args) info.ArgumentList.Add(arg);
        foreach (var key in new[] { "SWARM_TARGET", "STORMO_INSTANCE", "DOCKER_HOST", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH", "STORMO_SRC", "SWARM_CORE_BIND" }) info.Environment.Remove(key);
        info.Environment["SWARM_CORE_PORT"] = "18600";
        info.Environment["DOCKER_CONTEXT"] = "desktop-linux";
        if (DockerExecutable() is string docker) info.Environment["PATH"] = Path.GetDirectoryName(docker) + Path.PathSeparator + (info.Environment.TryGetValue("PATH", out var path) ? path : "");
        return info;
    }

    public static async Task<string> Run(string executable, IReadOnlyList<string> args, int timeout)
    {
        using var process = new Process { StartInfo = StartInfo(executable, args) };
        process.Start();
        process.StandardInput.Close();
        var stdout = process.StandardOutput.ReadToEndAsync();
        var stderr = process.StandardError.ReadToEndAsync();
        using var cancellation = new CancellationTokenSource(TimeSpan.FromSeconds(timeout));
        try { await process.WaitForExitAsync(cancellation.Token); }
        catch (OperationCanceledException)
        {
            // Stop only the command we launched; never kill a detached core or its children.
            try { process.Kill(); } catch (InvalidOperationException) { }
            throw new InvalidOperationException("This step took too long. Check setup and retry.");
        }
        var output = (await stdout).Trim();
        var error = (await stderr).Trim();
        if (process.ExitCode != 0) throw new InvalidOperationException((error.Length > 0 ? error : output) is { Length: > 0 } detail ? detail[^Math.Min(detail.Length, 2000)..] : $"{Path.GetFileName(executable)} exited with code {process.ExitCode}.");
        return output;
    }

    public static JsonElement ParseResult(string output)
    {
        JsonElement? result = null;
        foreach (var line in output.Split('\n', StringSplitOptions.RemoveEmptyEntries))
        {
            using var document = JsonDocument.Parse(line);
            var item = document.RootElement;
            if (item.ValueKind != JsonValueKind.Object) throw new InvalidOperationException("The engine returned an invalid event.");
            var kind = Text(item, "event");
            if (kind == "error") throw new InvalidOperationException(Text(item, "msg", "The engine could not complete this action."));
            if (kind == "result") result = item.GetProperty("data").Clone();
        }
        return result ?? throw new InvalidOperationException("The engine returned no result. Reinstall the preview and retry.");
    }

    public Task<JsonElement> Command(params string[] args) => CommandWithTimeout(300, args);
    private async Task<JsonElement> CommandWithTimeout(int timeout, params string[] args) => ParseResult(await runner(Engine, new[] { "--json", "--target", "local", "--instance", Instance }.Concat(args).ToArray(), timeout));
    private Task<string> Docker(int timeout, params string[] args) => runner(DockerExecutable() ?? throw new InvalidOperationException("Install and open Docker Desktop, then check again."), new[] { "--context", "desktop-linux" }.Concat(args).ToArray(), timeout);

    public static async Task<JsonElement> ReadApi(string route)
    {
        if (route is not ("/health" or "/api/core" or "/api/fleet")) throw new ArgumentException("Unsupported desktop API route");
        using var response = await http.GetAsync("http://127.0.0.1:18600" + route);
        response.EnsureSuccessStatusCode();
        using var document = JsonDocument.Parse(await response.Content.ReadAsStringAsync());
        return document.RootElement.Clone();
    }

    public static string Text(JsonElement item, string key, string fallback = "—") => item.TryGetProperty(key, out var value) && value.ValueKind == JsonValueKind.String ? value.GetString() ?? fallback : fallback;
    public static bool SamePath(string a, string b) => !string.IsNullOrWhiteSpace(a) && !string.IsNullOrWhiteSpace(b) && string.Equals(Path.GetFullPath(a).TrimEnd('\\', '/'), Path.GetFullPath(b).TrimEnd('\\', '/'), StringComparison.OrdinalIgnoreCase);
    public void CheckCore(JsonElement core)
    {
        if (Text(core, "service") != "swarm-core" || !core.TryGetProperty("api", out var api) || !api.TryGetInt32(out int version) || version != 1) throw new InvalidOperationException("Another service or an incompatible Stormo version is using the core port.");
        if (!core.TryGetProperty("instance", out var instance) || !SamePath(Text(instance, "root", ""), Instance) || !SamePath(Text(core, "exe", ""), Engine)) throw new InvalidOperationException("Another workspace owns the core. Stop it from its own app before continuing.");
    }

    public async Task<IReadOnlyList<SetupCheck>> Detect()
    {
        var checks = new List<SetupCheck>();
        if (!OperatingSystem.IsWindowsVersionAtLeast(10, 0, 22000) || System.Runtime.InteropServices.RuntimeInformation.OSArchitecture != System.Runtime.InteropServices.Architecture.X64)
            return [new("windows", "Windows 11", false, "This preview needs Windows 11 on an Intel or AMD 64-bit PC.")];
        checks.Add(new("windows", "Windows 11", true, "Your PC is supported."));
        try
        {
            using var info = JsonDocument.Parse(await Docker(30, "info", "--format", "{{json .}}"));
            if (Text(info.RootElement, "OSType") != "linux") throw new InvalidOperationException("Switch Docker Desktop to Linux containers.");
            await Docker(30, "compose", "version");
        }
        catch (Exception error)
        {
            verified = false;
            checks.Add(new("docker", "Docker Desktop", false, "Docker Desktop runs your agents in the background. Install it, open it and finish its setup, then check again. A Windows restart may be needed.\n\n" + error.Message));
            return checks;
        }
        checks.Add(new("docker", "Docker Desktop", true, "The container engine is ready."));
        try
        {
            if (!File.Exists(Engine)) throw new InvalidOperationException("The bundled engine is missing. Reinstall the complete preview build.");
            var version = await Command("version");
            if (version.GetProperty("api").GetInt32() != 1) throw new InvalidOperationException("The bundled engine is incompatible. Reinstall the preview.");
        }
        catch (Exception error) { checks.Add(new("repair", "Stormo engine", false, error.Message)); return checks; }
        checks.Add(new("engine", "Stormo engine", true, "The native engine is ready."));
        if (!verified) { checks.Add(new("verify", "Test the runtime", false, "Run a small, disposable test container before starting agents. The first check downloads Docker’s hello-world image.")); return checks; }
        checks.Add(new("verify", "Runtime test", true, "A test container ran successfully."));
        try { await Command("instance"); }
        catch (Exception error)
        {
            // Never suggest creating over an existing but unreadable/broken workspace.
            if (Directory.Exists(Instance)) checks.Add(new("repair", "Your workspace", false, "The existing workspace could not be opened. Its files have been left unchanged.\n\n" + error.Message));
            else checks.Add(new("instance", "Your workspace", false, "Create a private workspace on this PC. It starts empty; setup won’t run agents or use a paid model."));
            return checks;
        }
        checks.Add(new("instance", "Your workspace", true, "You’re ready to open your workspace."));
        return checks;
    }

    public async Task VerifyContainer() { await Docker(300, "run", "--rm", "--network", "none", "hello-world:latest"); verified = true; }
    public Task<JsonElement> CreateInstance() => Command("new", "instance", Instance, "--name", "My Stormo", "--slug", "my-stormo", "--template", "empty", "--no-git");
    public async Task<IReadOnlyList<Agent>> Fleet()
    {
        CheckCore(await reader("/api/core"));
        var fleet = await reader("/api/fleet");
        return fleet.GetProperty("agents").EnumerateArray().Select(a => new Agent(a.GetProperty("id").GetString()!, Text(a, "name"), Text(a, "unit"), Text(a, "state"), Text(a, "health"), Text(a, "model"))).ToArray();
    }
    public async Task<JsonElement> Action(string action, string? agent = null)
    {
        if (action is not ("core up" or "core down" or "start" or "stop" or "restart" or "nap-now")) throw new ArgumentException("Unsupported desktop action");
        if (action == "core up")
        {
            try { CheckCore(await reader("/api/core")); }
            catch (HttpRequestException error) when (error.InnerException is SocketException { SocketErrorCode: SocketError.ConnectionRefused }) { }
            return await Command("core", "up");
        }
        CheckCore(await reader("/api/core"));
        if (action == "core down") return await Command("core", "down");
        if (string.IsNullOrEmpty(agent) || agent.StartsWith('-') || !(await Fleet()).Any(a => a.Id == agent)) throw new InvalidOperationException("That agent is no longer in this workspace. Refresh and select it again.");
        return await CommandWithTimeout(900, action, agent);
    }
}
