import Foundation
import Testing

@testable import StormoKit

// Fixtures are the office demo's responses (go run ./examples/office-demo), host paths replaced.
func fixture(_ name: String) throws -> Data {
    let url = try #require(Bundle.module.url(forResource: name, withExtension: "json", subdirectory: "Fixtures"))
    return try Data(contentsOf: url)
}

/// A temporary directory removed at the end of the test.
final class Scratch {
    let url: URL
    init() throws {
        url = FileManager.default.temporaryDirectory.appending(path: "stormokit-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: url, withIntermediateDirectories: true)
    }
    deinit { try? FileManager.default.removeItem(at: url) }

    /// An executable shell script standing in for a stormo binary.
    @discardableResult
    func script(_ path: String, _ body: String) throws -> URL {
        let file = url.appending(path: path)
        try FileManager.default.createDirectory(at: file.deletingLastPathComponent(), withIntermediateDirectories: true)
        try ("#!/bin/sh\n" + body).write(to: file, atomically: true, encoding: .utf8)
        try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: file.path)
        return file
    }
}

/// A fake stormo whose `version --json` reports the given version and API.
func fakeStormo(_ s: Scratch, _ path: String, version: String, api: Int = 1) throws -> URL {
    try s.script(path, """
        for a in "$@"; do last="$a"; done
        if [ "$last" = version ]; then
          echo '{"event":"result","data":{"version":"\(version)","api":\(api),"released":true,"exe":"'"$0"'"}}'
        fi
        """)
}

@Suite struct APIDecoding {
    @Test func health() throws {
        let h = try JSONDecoder().decode(Health.self, from: fixture("health"))
        #expect(h.service == "swarm-core" && h.api == 1 && h.login == "missing")
    }

    @Test func core() throws {
        let c = try JSONDecoder().decode(CoreInfo.self, from: fixture("api_core"))
        #expect(c.api == 1 && c.port == 18701 && c.instance.slug == "acme" && c.instance.root == "/Users/me/acme-swarm")
    }

    @Test func instance() throws {
        let i = try JSONDecoder().decode(InstanceLook.self, from: fixture("api_instance"))
        #expect(i.name == "Acme Swarm" && !i.clocks.isEmpty && !i.units.isEmpty)
    }

    @Test func fleet() throws {
        let f = try JSONDecoder().decode(Fleet.self, from: fixture("api_fleet"))
        #expect(f.agents.count == 7 && f.units.count == 5 && f.now > 0)
        let atlas = try #require(f.agents.first { $0.id == "atlas" })
        #expect(atlas.sprite?.hairStyle == "fade" && atlas.desk?.app == "records" && atlas.office?.activity == "desk")
        #expect(atlas.activity?.platforms?["slack"]?.state == "connected")
        #expect(f.robot?.note?.changed?.learning == 2)
        let s = FleetSummary(f)
        #expect(s.total == 7 && s.onShift == 7 && s.working <= 7)
    }

    @Test func gateway() throws {
        let g = try JSONDecoder().decode(GatewayStatus.self, from: fixture("api_gateway"))
        #expect(g.login == "missing" && g.models?.first?.id == "gpt-6-luna" && g.models?.first?.contextLength == 272000)
    }
}

@Suite struct Events {
    @Test func parsesEveryKind() throws {
        #expect(CLIEvent(line: #"{"event":"step","msg":"building atlas"}"#) == .step("building atlas"))
        #expect(CLIEvent(line: #"{"event":"auth_url","url":"https://a.example/x?a=1&b=2"}"#) == .authURL("https://a.example/x?a=1&b=2"))
        #expect(CLIEvent(line: #"{"event":"error","msg":"no stormo.yaml","code":"instance"}"#) == .error(code: "instance", message: "no stormo.yaml"))
        #expect(CLIEvent(line: "core is not running") == nil)
        let r = try #require(CLIEvent(line: #"{"event":"result","data":{"stopped":false,"reason":"not_running"}}"#))
        #expect(try r.decode(CoreDownResult.self) == CoreDownResult(stopped: false, pid: nil, reason: "not_running"))
    }
}

@Suite struct Runner {
    @Test func argumentsAreLocalAndExplicit() {
        let cli = StormoCLI(executable: URL(filePath: "/x/stormo"), environment: ["SWARM_TARGET": "remote", "PATH": "/bin"], instance: URL(filePath: "/i"))
        #expect(cli.arguments(["start", "atlas"]) == ["--json", "--target", "local", "--instance", "/i", "start", "atlas"])
        let env = cli.processEnvironment()
        #expect(env["SWARM_TARGET"] == nil && env["STORMO_INSTANCE"] == "/i")
    }

    @Test func streamsStepsThenResult() async throws {
        let s = try Scratch()
        let bin = try s.script("stormo", """
            echo '{"event":"step","msg":"one"}'
            echo 'compose noise' >&2
            echo '{"event":"step","msg":"two"}'
            echo '{"event":"result","data":{"action":"start","where":"local","agents":["atlas"]}}'
            """)
        let cli = StormoCLI(executable: bin, environment: [:], instance: nil)
        let steps = Steps()
        let r = try await cli.run(["start", "atlas"], as: LifecycleResult.self) { e in
            if case .step(let m) = e { steps.add(m) }
        }
        #expect(r.agents == ["atlas"])
        #expect(steps.all == ["one", "two"])
    }

    @Test func errorEventBecomesCLIError() async throws {
        let s = try Scratch()
        let bin = try s.script("stormo", """
            echo '{"event":"error","msg":"no manifest","code":"failed"}'
            exit 1
            """)
        let cli = StormoCLI(executable: bin, environment: [:], instance: nil)
        await #expect(throws: CLIError(code: "failed", message: "no manifest", status: 1, stderr: "")) {
            _ = try await cli.run(["start", "nope"], as: LifecycleResult.self)
        }
    }

    @Test func nonZeroExitWithoutEventKeepsStderr() async throws {
        let s = try Scratch()
        let bin = try s.script("stormo", "echo 'unknown flag: --json' >&2; exit 2")
        let cli = StormoCLI(executable: bin, environment: [:], instance: nil)
        do {
            _ = try await cli.run(["version"], as: VersionInfo.self)
            Issue.record("expected an error")
        } catch let e as CLIError {
            #expect(e.status == 2 && e.message == "unknown flag: --json")
        }
    }
}

final class Steps: @unchecked Sendable {
    private let lock = NSLock()
    private var items: [String] = []
    func add(_ s: String) { lock.withLock { items.append(s) } }
    var all: [String] { lock.withLock { items } }
}

@Suite struct Resolution {
    @Test func dedupesSymlinksAndProbes() async throws {
        let s = try Scratch()
        let real = try fakeStormo(s, "instance/stormo/bin/stormo", version: "v1.2.0")
        let pathDir = s.url.appending(path: "local-bin")
        try FileManager.default.createDirectory(at: pathDir, withIntermediateDirectories: true)
        try FileManager.default.createSymbolicLink(at: pathDir.appending(path: "stormo"), withDestinationURL: real)
        let old = try fakeStormo(s, "old/stormo", version: "v0.0.1", api: 0)
        let bundled = try fakeStormo(s, "App.app/Contents/Helpers/stormo", version: "v1.3.0")
        let resolver = BinaryResolver(bundled: bundled, environment: ["PATH": "\(pathDir.path):\(old.deletingLastPathComponent().path)"], home: s.url)
        let found = await resolver.candidates(instance: s.url.appending(path: "instance"))
        #expect(found.map(\.source) == [.path, .path, .bundled])  // the vendored file is the PATH symlink's target
        #expect(found[0].info?.version == "v1.2.0" && found[0].usable)
        #expect(found[1].problem != nil)  // API 0
        #expect(BinaryResolver.autoChoice(found, runningCoreExe: nil)?.info?.version == "v1.2.0")
        #expect(BinaryResolver.autoChoice(found, runningCoreExe: bundled.path)?.source == .bundled)
        #expect(BinaryResolver.autoChoice(found.filter { $0.source == .bundled }, runningCoreExe: nil)?.source == .bundled)
    }
}

@Suite struct Environment {
    @Test func parsesBetweenMarkersIgnoringNoise() throws {
        var out = Data("Welcome back!\n".utf8)
        out.append(Data(ShellEnvironment.marker.utf8))
        out.append(Data("PATH=/opt/homebrew/bin:/usr/bin\u{0}SWARM_TARGET=remote\u{0}EQ=a=b\u{0}".utf8))
        out.append(Data(ShellEnvironment.marker.utf8))
        out.append(Data("bye\n".utf8))
        let env = try #require(ShellEnvironment.parse(out))
        #expect(env["PATH"] == "/opt/homebrew/bin:/usr/bin" && env["EQ"] == "a=b")
        #expect(ShellEnvironment.parse(Data("no markers".utf8)) == nil)
    }

    @Test func composeKeepsWhatStormoNeedsAndNeverTheTarget() {
        let env = ShellEnvironment.compose(
            shell: ["PATH": "/custom/bin", "SWARM_TARGET": "remote", "AWS_PROFILE": "dev", "SECRET_TOKEN": "x", "HOME": "/Users/me"],
            process: ["PATH": "/usr/bin:/bin", "HOME": "/Users/me"])
        #expect(env["SWARM_TARGET"] == nil && env["SECRET_TOKEN"] == nil && env["AWS_PROFILE"] == "dev")
        let path = env["PATH"]!.split(separator: ":").map(String.init)
        #expect(path.first == "/custom/bin" && path.contains("/opt/homebrew/bin") && path.contains("/Users/me/.orbstack/bin"))
    }

    @Test func capturesTheLoginShell() async throws {
        guard FileManager.default.isExecutableFile(atPath: "/bin/zsh") else { return }
        let env = try #require(await ShellEnvironment.capture(shell: "/bin/zsh"))
        #expect(env["PATH"]?.isEmpty == false)
    }
}

@Suite struct CoreClassification {
    let core = try! JSONDecoder().decode(CoreInfo.self, from: fixture("api_core"))
    let health = try! JSONDecoder().decode(Health.self, from: fixture("health"))

    @Test func ours() {
        #expect(CoreState.classify(health: health, core: core, instanceRoot: URL(filePath: "/Users/me/acme-swarm/")) == .running(core))
    }

    @Test func anotherInstance() {
        #expect(CoreState.classify(health: health, core: core, instanceRoot: URL(filePath: "/Users/me/other")) == .otherInstance(core))
    }

    @Test func downAndOld() {
        #expect(CoreState.classify(health: nil, core: nil, instanceRoot: nil) == .down)
        var old = health
        old.api = nil
        old.version = nil
        #expect(CoreState.classify(health: old, core: nil, instanceRoot: nil) == .incompatible(version: nil, api: nil))
    }

    @Test func slowCoreIsUnknownNotOld() {
        #expect(CoreState.classify(health: health, core: nil, instanceRoot: nil) == .unknown)
    }
}

/// Against the engine built in this checkout (go build -o bin/stormo ./cmd/stormo), when present.
@Suite struct RealBinary {
    static let engine = URL(filePath: #filePath).deletingLastPathComponent().appending(path: "../../../../..").standardizedFileURL
    static let binary = engine.appending(path: "bin/stormo")

    @Test(.enabled(if: FileManager.default.isExecutableFile(atPath: binary.path)))
    func versionAndInstance() async throws {
        let env = ShellEnvironment.compose(shell: nil, process: ProcessInfo.processInfo.environment)
        let cli = StormoCLI(executable: Self.binary, environment: env, instance: Self.engine.appending(path: "examples/minimal"))
        let v = try await cli.run(["version"], as: VersionInfo.self)
        #expect(StormoAPI.supported.contains(v.api))
        let i = try await cli.run(["instance"], as: InstanceInfo.self)
        #expect(i.slug == "acme")
        await #expect(throws: CLIError.self) { _ = try await cli.run(["nap-now"], as: LifecycleResult.self) }
    }
}

/// The supervisor restarting a real core (this checkout's bin/stormo) on a scratch instance and port.
@Suite(.serialized) struct RealCore {
    @Test(.enabled(if: FileManager.default.isExecutableFile(atPath: RealBinary.binary.path)))
    @MainActor func restartReplacesTheProcess() async throws {
        let s = try Scratch()
        let instance = s.url.appending(path: "acme")
        try FileManager.default.copyItem(at: RealBinary.engine.appending(path: "examples/minimal"), to: instance)
        let port = 18690 + Int.random(in: 0..<9)
        var env = ShellEnvironment.compose(shell: nil, process: ProcessInfo.processInfo.environment)
        env["SWARM_CORE_PORT"] = String(port)
        let cli = StormoCLI(executable: RealBinary.binary, environment: env, instance: instance)
        let supervisor = CoreSupervisor(client: CoreClient(port: port), instanceRoot: instance)
        defer { Task { _ = try? await cli.run(["core", "down"], as: CoreDownResult.self) } }

        await supervisor.start(with: cli)
        let first = try #require(supervisor.state.info, "\(supervisor.state) \(supervisor.lastError ?? "")")
        await supervisor.restart(with: cli)
        let second = try #require(supervisor.state.info, "\(supervisor.state) \(supervisor.lastError ?? "")")
        #expect(supervisor.state.isRunning && second.pid != first.pid && supervisor.lastError == nil)

        await supervisor.stop(with: cli)
        #expect(supervisor.state == .down)
    }
}
