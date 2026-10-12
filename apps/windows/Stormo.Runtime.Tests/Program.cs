using System.Net.Sockets;
using System.Text.Json;
using Stormo.Runtime;

static JsonElement Json(string data) => JsonDocument.Parse(data).RootElement.Clone();
static void Check(bool condition, string label) { if (!condition) throw new Exception(label); Console.WriteLine("PASS " + label); }
static async Task Refuses(Func<Task> action, string label) { try { await action(); } catch { Console.WriteLine("PASS " + label); return; } throw new Exception("Did not refuse: " + label); }

var assets = Path.Combine(Path.GetTempPath(), "Stormo tests with spaces", "assets");
var instance = Path.Combine(Path.GetTempPath(), "Stormo tests with spaces", "instance");
var calls = new List<string[]>();
Task<string> Runner(string exe, IReadOnlyList<string> args, int timeout) { calls.Add(args.ToArray()); return Task.FromResult("{\"event\":\"result\",\"data\":{\"ok\":true}}\n"); }
var baseline = new LocalRuntime(assets, instance);
JsonElement Core(string? root = null, int api = 1) => JsonSerializer.SerializeToElement(new { service = "swarm-core", api, instance = new { root = root ?? baseline.Instance }, exe = baseline.Engine });
var current = Core();
var fleet = Json("{\"agents\":[{\"id\":\"atlas\",\"name\":\"Atlas\",\"state\":\"running\"}]}");
Task<JsonElement> Reader(string route) => Task.FromResult(route == "/api/core" ? current : fleet);
var runtime = new LocalRuntime(assets, instance, Runner, Reader);
await runtime.Command("version");
Check(calls[^1].SequenceEqual(new[] { "--json", "--target", "local", "--instance", instance, "version" }), "explicit local CLI arguments retain spaces");
Environment.SetEnvironmentVariable("DOCKER_HOST", "tcp://remote:2375");
Environment.SetEnvironmentVariable("SWARM_TARGET", "cloud");
var start = LocalRuntime.StartInfo("stormo.exe", new[] { "a & b", "--instance", instance });
Check(!start.UseShellExecute && start.ArgumentList[0] == "a & b" && start.CreateNoWindow, "no shell interpolation or console flash");
Check(!start.Environment.ContainsKey("DOCKER_HOST") && !start.Environment.ContainsKey("SWARM_TARGET") && start.Environment["DOCKER_CONTEXT"] == "desktop-linux" && start.Environment["SWARM_CORE_PORT"] == "18600", "remote overrides scrubbed");
Check(LocalRuntime.ParseResult("{\"event\":\"progress\"}\n{\"event\":\"result\",\"data\":42}").GetInt32() == 42, "JSON event parsing");
await Refuses(() => Task.FromResult(LocalRuntime.ParseResult("{\"event\":\"error\",\"msg\":\"refused\"}")), "engine errors propagate");
await Refuses(() => Task.FromResult(LocalRuntime.ParseResult("{\"event\":\"progress\"}")), "missing result rejected");
await Refuses(() => Task.FromResult(LocalRuntime.ParseResult("[]")), "non-object events rejected");
await runtime.Action("core up");
Check(calls[^1][^1] == "up", "owned core may start");
current = Core(instance + "-other");
var count = calls.Count;
await Refuses(() => runtime.Action("core up"), "foreign core cannot start");
await Refuses(() => runtime.Action("core down"), "foreign core cannot stop");
Check(calls.Count == count, "ownership refusal performs no command");
current = Core(api: 2);
await Refuses(() => runtime.Fleet(), "incompatible API rejected");
current = Core();
await runtime.Action("restart", "atlas");
Check(calls[^1][^2] == "restart" && calls[^1][^1] == "atlas", "current agent may restart");
fleet = Json("{\"agents\":[]}");
count = calls.Count;
await Refuses(() => runtime.Action("stop", "atlas"), "removed agent rejected");
await Refuses(() => runtime.Action("stop", "--all"), "flag injection rejected");
Check(calls.Count == count, "agent refusal performs no command");
var refused = new LocalRuntime(assets, instance, Runner, _ => throw new HttpRequestException("Refused", new SocketException((int)SocketError.ConnectionRefused)));
await refused.Action("core up");
Check(calls.Count == count + 1, "refused port permits own core startup");
var timedOut = new LocalRuntime(assets, instance, Runner, _ => throw new TaskCanceledException());
await Refuses(() => timedOut.Action("core up"), "unresponsive port left unchanged");
var malformed = new LocalRuntime(assets, instance, Runner, _ => Task.FromResult(Json("{}")));
await Refuses(() => malformed.Action("core up"), "unknown service left unchanged");
await Refuses(() => LocalRuntime.ReadApi("https://example.com"), "API route allowlist");
if (OperatingSystem.IsWindows()) Check(LocalRuntime.SamePath(instance.ToUpperInvariant().Replace('\\', '/'), instance), "Windows identity ignores case and slash style");
Console.WriteLine("All runtime contract checks passed.");
