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
        #expect(BinaryResolver.autoChoice(found, runningCoreExe: nil)?.info?.version == "v1.3.0")
        #expect(BinaryResolver.autoChoice(found, runningCoreExe: bundled.path)?.source == .bundled)
        #expect(BinaryResolver.autoChoice(found.filter { $0.source == .bundled }, runningCoreExe: nil)?.source == .bundled)
    }

    /// PATH, the core's binary and the bundled copy, all usable.
    func fleet(_ s: Scratch, bundledAPI: Int = 1) async throws -> (found: [BinaryCandidate], core: URL, bundled: URL) {
        let onPath = try fakeStormo(s, "path/stormo", version: "v1.2.0")
        let core = try fakeStormo(s, "checkout/bin/stormo", version: "v1.1.0")
        let bundled = try fakeStormo(s, "App.app/Contents/Helpers/stormo", version: "v1.3.0", api: bundledAPI)
        let resolver = BinaryResolver(bundled: bundled, environment: ["PATH": onPath.deletingLastPathComponent().path], home: s.url)
        var found = await resolver.candidates(instance: nil)
        found.append(await resolver.probe(BinaryCandidate(url: core, resolved: core, source: .usual)))
        return (found, core, bundled)
    }

    @Test func autoPrefersTheBundledCopy() async throws {
        let s = try Scratch()
        let (found, core, _) = try await fleet(s)
        #expect(BinaryResolver.autoChoice(found, runningCoreExe: nil)?.source == .bundled)  // over PATH
        #expect(BinaryResolver.autoChoice(found, runningCoreExe: core.path)?.source == .bundled)  // over the core's
    }

    @Test func anUnusableBundledCopyFallsBackToTheCoreThenPath() async throws {
        let s = try Scratch()
        let (found, core, _) = try await fleet(s, bundledAPI: 0)
        #expect(found.first { $0.source == .bundled }?.usable == false)
        #expect(BinaryResolver.autoChoice(found, runningCoreExe: core.path)?.resolved.path == core.path)
        #expect(BinaryResolver.autoChoice(found, runningCoreExe: nil)?.source == .path)
    }

    @Test func customWins() async throws {
        let s = try Scratch()
        let mine = try fakeStormo(s, "mine/stormo", version: "v0.9.0")
        let bundled = try fakeStormo(s, "App.app/Contents/Helpers/stormo", version: "v1.3.0")
        let resolver = BinaryResolver(bundled: bundled, environment: ["PATH": ""], home: s.url)
        let found = await resolver.candidates(instance: nil, custom: mine)
        #expect(BinaryResolver.autoChoice(found, runningCoreExe: bundled.path)?.resolved.path == mine.path)
    }

    @Test func autoPinsFollowAutoAndChosenOnesStay() async throws {
        let s = try Scratch()
        let (found, core, bundled) = try await fleet(s)
        // An instance Auto pinned to the core's binary before this app (or an update of it) moves.
        #expect(BinaryResolver.repin(binary: core.path, choice: "auto", candidates: found, runningCoreExe: core.path)?.resolved.path == bundled.path)
        // A new instance gets Auto's choice; one already on it is left alone.
        #expect(BinaryResolver.repin(binary: nil, choice: nil, candidates: found, runningCoreExe: nil)?.source == .bundled)
        #expect(BinaryResolver.repin(binary: bundled.path, choice: "auto", candidates: found, runningCoreExe: nil) == nil)
        // Picked in Settings: stays, whatever Auto would say.
        #expect(BinaryResolver.repin(binary: core.path, choice: "custom", candidates: found, runningCoreExe: nil) == nil)
        #expect(BinaryResolver.repin(binary: core.path, choice: "path", candidates: found, runningCoreExe: nil) == nil)
    }
}

/// ~/.local/bin/stormo, in a scratch home: never the real one.
@Suite struct TerminalLink {
    struct Home {
        let s: Scratch
        let bin: URL
        let link: CommandLineLink
        init() throws {
            s = try Scratch()
            bin = s.url.appending(path: "home/.local/bin")
            let bundled = try fakeStormo(s, "Applications/Stormo.app/Contents/Helpers/stormo", version: "v1.3.0")
            link = CommandLineLink(link: bin.appending(path: "stormo"), bundled: bundled)
        }
        func symlink(to target: String) throws {
            try FileManager.default.createDirectory(at: bin, withIntermediateDirectories: true)
            try FileManager.default.createSymbolicLink(atPath: link.link.path, withDestinationPath: target)
        }
        var target: String? { try? FileManager.default.destinationOfSymbolicLink(atPath: link.link.path) }
    }

    @Test func absentIsCreatedWithItsDirectory() throws {
        let h = try Home()
        #expect(h.link.state() == .absent)
        #expect(h.link.reconcile() == .created)
        #expect(h.target == h.link.bundled.path)
        #expect(h.link.state() == .current)
        #expect(h.link.reconcile() == .unchanged)
        #expect(try FileManager.default.contentsOfDirectory(atPath: h.bin.path) == ["stormo"])  // no temp link left
    }

    @Test func anOlderAppsLinkFollowsThisApp() throws {
        let h = try Home()
        let old = try fakeStormo(h.s, "Downloads/Stormo 2.app/Contents/Helpers/stormo", version: "v1.0.0")
        try h.symlink(to: old.path)
        #expect(h.link.state() == .appCopy(target: old.path))
        #expect(h.link.reconcile() == .retargeted(from: old.path))
        #expect(h.target == h.link.bundled.path)
    }

    @Test func aBrokenLinkToAGoneAppIsRetargeted() throws {
        let h = try Home()
        let gone = h.s.url.appending(path: "Trash/Stormo.app/Contents/Helpers/stormo").path
        try h.symlink(to: gone)
        #expect(h.link.state() == .appCopy(target: gone))
        #expect(h.link.reconcile() == .retargeted(from: gone))
        #expect(h.target == h.link.bundled.path)
    }

    @Test func aUsersLinkIsLeftUntilAsked() throws {
        let h = try Home()
        let dev = try fakeStormo(h.s, "src/stormo/bin/stormo", version: "abc123")
        try h.symlink(to: dev.path)
        let state = CommandLineLink.State.userLink(target: dev.path, broken: false)
        #expect(h.link.state() == state)
        #expect(h.link.reconcile() == .left(state))
        #expect(h.target == dev.path)
        try h.link.install()  // what "Use Stormo's copy in the terminal" does after confirming
        #expect(h.target == h.link.bundled.path)
    }

    @Test func aBrokenUserLinkIsLeft() throws {
        let h = try Home()
        try h.symlink(to: "../../src/gone/stormo")
        let target = h.bin.appending(path: "../../src/gone/stormo").standardizedFileURL.path
        #expect(h.link.state() == .userLink(target: target, broken: true))
        #expect(h.link.reconcile() == .left(.userLink(target: target, broken: true)))
        #expect(h.target == "../../src/gone/stormo")
    }

    @Test func aRegularFileIsLeftUntilAsked() throws {
        let h = try Home()
        try h.s.script("home/.local/bin/stormo", "echo homebrew")
        #expect(h.link.state() == .file)
        #expect(h.link.reconcile() == .left(.file))
        #expect(h.target == nil)
        try h.link.install()
        #expect(h.target == h.link.bundled.path)
    }

    @Test func anUnwritableDirectoryFailsWithoutChanges() throws {
        let h = try Home()
        try FileManager.default.createDirectory(at: h.bin, withIntermediateDirectories: true)
        try FileManager.default.setAttributes([.posixPermissions: 0o555], ofItemAtPath: h.bin.path)
        defer { try? FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: h.bin.path) }
        guard case .failed = h.link.reconcile() else { Issue.record("expected a failure"); return }
        #expect(h.link.state() == .absent)
    }

    @Test func translocatedAppsLinkNothing() throws {
        let h = try Home()
        let moved = CommandLineLink(link: h.link.link, bundled: URL(filePath: "/private/var/folders/xy/AppTranslocation/ABC/d/Stormo.app/Contents/Helpers/stormo"))
        #expect(moved.reconcile() == .skipped)
        #expect(moved.state() == .absent)
    }

    @Test func appHelpersAndPath() {
        #expect(CommandLineLink.isAppHelper("/Applications/Stormo.app/Contents/Helpers/stormo"))
        #expect(CommandLineLink.isAppHelper("/Users/x/dd/Build/Products/Debug/Stormo Beta.app/Contents/Helpers/stormo"))
        #expect(!CommandLineLink.isAppHelper("/Users/x/src/stormo/bin/stormo"))
        #expect(!CommandLineLink.isAppHelper("/Applications/Stormo.app/Contents/Helpers/swarm"))
        #expect(!CommandLineLink.isAppHelper("/opt/homebrew/bin/stormo"))
        let link = CommandLineLink(link: URL(filePath: "/Users/x/.local/bin/stormo"), bundled: URL(filePath: "/Applications/Stormo.app/Contents/Helpers/stormo"))
        #expect(link.directoryIsOnPath(["PATH": "/usr/bin:/Users/x/.local/bin/"]))
        #expect(link.directoryIsOnPath(["PATH": "~/.local/bin:/usr/bin", "HOME": "/Users/x"]))
        #expect(!link.directoryIsOnPath(["PATH": "/usr/bin:/opt/homebrew/bin", "HOME": "/Users/x"]))
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

@Suite struct NewInstances {
    @Test func slugsMatchTheEngine() {
        // The same cases as pkg/scaffold's TestSlugFrom.
        for (name, want) in [("Acme Swarm", "acme-swarm"), ("  Zeta & Co.  ", "zeta-co"), ("42 Labs", "labs"), ("日本", ""), ("", "")] {
            #expect(InstanceLocation.slug(from: name) == want, "\(name)")
        }
        #expect(InstanceLocation.isValidSlug("acme-2") && !InstanceLocation.isValidSlug("Acme") && !InstanceLocation.isValidSlug("2acme"))
        #expect(InstanceLocation.defaultParent.path.hasSuffix("Library/Application Support/Stormo/Instances"))
    }

    @Test(.enabled(if: FileManager.default.isExecutableFile(atPath: RealBinary.binary.path)))
    func createsWithTheRealBinary() async throws {
        let s = try Scratch()
        let dir = s.url.appending(path: "Application Support/Stormo/Instances/zeta")
        let env = ShellEnvironment.compose(shell: nil, process: ProcessInfo.processInfo.environment)
        let cli = StormoCLI(executable: RealBinary.binary, environment: env, instance: nil)
        let r = try await cli.run(["new", "instance", dir.path, "--name", "Zeta", "--slug", "zeta", "--no-git"], as: NewInstanceResult.self)
        #expect(r.slug == "zeta" && r.template == "empty" && !r.git)
        let made = StormoCLI(executable: RealBinary.binary, environment: env, instance: dir)
        try await made.perform(["secrets", "init"])
        #expect(try await made.run(["instance"], as: InstanceInfo.self).slug == "zeta")
        #expect(FileManager.default.fileExists(atPath: dir.appending(path: "secrets.local.yaml").path))
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

/// Editing an agent's files through this checkout's bin/stormo, on a scratch copy of the example.
@Suite struct ConfigEditing {
    @Test(.enabled(if: FileManager.default.isExecutableFile(atPath: RealBinary.binary.path)))
    @MainActor func editSaveConflictAndRefusal() async throws {
        let s = try Scratch()
        let instance = s.url.appending(path: "acme")
        try FileManager.default.copyItem(at: RealBinary.engine.appending(path: "examples/minimal"), to: instance)
        let env = ShellEnvironment.compose(shell: nil, process: ProcessInfo.processInfo.environment)
        let cli = StormoCLI(executable: RealBinary.binary, environment: env, instance: instance)
        let manifest = instance.appending(path: "agents/atlas/agent.yaml")

        let editor = ConfigEditor(path: "agents/atlas/agent.yaml")
        await editor.load(with: cli)
        #expect(editor.file?.kind == "agent" && !editor.isDirty && editor.problem == nil)

        // A valid edit is written as typed, comments and all (stdin carries it).
        editor.text = editor.text.replacingOccurrences(of: "\nrole: ", with: "\n# edited in the app\nrole: ")
        #expect(editor.isDirty)
        #expect(await editor.save(with: cli))
        let written = try String(contentsOf: manifest, encoding: .utf8)
        #expect(!editor.isDirty && written == editor.text)

        // An edit that would not load is refused and leaves the file alone.
        let saved = editor.text
        editor.text = saved.replacingOccurrences(of: "unit: sales", with: "unit: nowhere")
        #expect(!(await editor.save(with: cli)))
        #expect(editor.problem?.code == "invalid" && editor.problem?.message.contains("nowhere") == true)
        let untouched = try String(contentsOf: manifest, encoding: .utf8)
        #expect(untouched == saved)

        // A change on disk since the load is a conflict; reloading picks it up.
        try (saved + "\n# changed by another editor\n").write(to: manifest, atomically: true, encoding: .utf8)
        editor.text = saved + "\n# mine\n"
        #expect(!(await editor.save(with: cli)))
        #expect(editor.isStale)
        await editor.load(with: cli)
        #expect(!editor.isStale && editor.text.hasSuffix("# changed by another editor\n"))

        // check runs after a save, with a result row.
        let rows = try await cli.run(["check", "atlas"], as: [CheckedAgent].self)
        #expect(rows.map(\.agent) == ["atlas"])
    }
}

@Suite struct Patches {
    func json(_ s: String) throws -> JSONValue { try JSONDecoder().decode(JSONValue.self, from: Data(s.utf8)) }
    func text(_ v: JSONValue) throws -> String { String(decoding: try MergePatch.data(v), as: UTF8.self) }

    @Test func integersStayIntegers() throws {
        let v = try json(#"{"cpu":2048,"seed_fill":0.6,"on":true,"name":"2048"}"#)
        #expect(v[["cpu"]] == .int(2048) && v[["seed_fill"]] == .double(0.6) && v[["on"]] == .bool(true) && v[["name"]] == .string("2048"))
        #expect(try text(v) == #"{"cpu":2048,"name":"2048","on":true,"seed_fill":0.6}"#)
    }

    @Test func onlyWhatChanged() throws {
        let old = try json(#"{"name":"Atlas","engine":{"model":"a","local":{"via":"core","model":"b"}},"learning":{"seed_fill":0.6,"memory_char_limit":2200},"secrets":["A","B"],"env":{"X":"1"}}"#)
        #expect(MergePatch.diff(from: old, to: old) == nil)
        var new = old
        new.set(["engine", "local", "model"], .string("c"))
        new.set(["learning", "seed_fill"], .int(1))
        new.set(["learning", "memory_char_limit"], .double(2200))  // the same number
        new.set(["env", "X"], nil)
        new.set(["secrets"], .array([.string("A")]))
        new.set(["role"], .string("New"))
        let patch = try #require(MergePatch.diff(from: old, to: new))
        #expect(try text(patch) == #"{"engine":{"local":{"model":"c"}},"env":{"X":null},"learning":{"seed_fill":1},"role":"New","secrets":["A"]}"#)
    }

    @Test func paths() throws {
        var v = try json(#"{"channels":[{"kind":"slack"},{"kind":"telegram"}]}"#)
        #expect(v[["channels", 1, "kind"]] == .string("telegram") && v[["channels", 5]] == nil)
        v.set(["channels", 0, "allowed_users"], .array([.string("U1")]))
        v.set(["channels", 1], nil)
        #expect(v == (try json(#"{"channels":[{"kind":"slack","allowed_users":["U1"]}]}"#)))
    }
}

@Suite struct FormEditing {
    @Test(.enabled(if: FileManager.default.isExecutableFile(atPath: RealBinary.binary.path)))
    @MainActor func aFormSaveChangesOnlyItsFields() async throws {
        let s = try Scratch()
        let instance = s.url.appending(path: "acme")
        try FileManager.default.copyItem(at: RealBinary.engine.appending(path: "examples/minimal"), to: instance)
        let env = ShellEnvironment.compose(shell: nil, process: ProcessInfo.processInfo.environment)
        let cli = StormoCLI(executable: RealBinary.binary, environment: env, instance: instance)
        let manifest = instance.appending(path: "agents/atlas/agent.yaml")
        let before = try String(contentsOf: manifest, encoding: .utf8)

        let editor = ConfigEditor(path: "agents/atlas/agent.yaml")
        await editor.load(with: cli)
        let options = try #require(editor.file?.options)
        #expect(options.units.map(\.id).contains("sales") && options.engines.contains("hermes"))
        #expect(editor.doc?[["memory", "agent"]] == .int(2200) && editor.doc?[["format"]] == .int(1) && !editor.isDirty)

        editor.doc?.set(["role"], .string("Edited in the form."))
        editor.doc?.set(["memory", "agent"], .int(3000))
        #expect(editor.isFormDirty && !editor.isTextDirty)
        #expect(await editor.save(with: cli))
        let after = try String(contentsOf: manifest, encoding: .utf8)
        let changed = zip(before.split(separator: "\n", omittingEmptySubsequences: false), after.split(separator: "\n", omittingEmptySubsequences: false)).filter { $0 != $1 }
        #expect(changed.map { String($0.1) } == ["role: Edited in the form.", "  agent: 3000"])
        #expect(!editor.isDirty && editor.text == after)

        // The format changes only through stormo migrate agent.
        editor.doc?.set(["format"], .int(0))
        #expect(!(await editor.save(with: cli)))
        #expect(editor.problem?.code == "readonly")
        editor.revert()

        // Form and text edited at once: refused until one is reverted.
        editor.doc?.set(["name"], .string("Atlas II"))
        editor.text += "\n# note\n"
        #expect(!(await editor.save(with: cli)))
        #expect(editor.problem?.code == "usage")
    }
}

/// The Units section's files through this checkout's bin/stormo: a unit's form, a new unit, its
/// knowledge created, edited and removed.
@Suite struct UnitEditing {
    @Test func names() {
        #expect(UnitCatalog.unitID(from: "Customer Care") == "customer-care")
        #expect(UnitCatalog.knowledgeName(from: "Pricing Rules.md") == "pricing-rules.md")
        #expect(UnitCatalog.knowledgeName(from: "  ") == "")
        #expect(UnitCatalog.unitYAML(id: "care", name: "Care: \"VIP\"", description: "") == "id: care\nname: \"Care: \\\"VIP\\\"\"\ndescription: \"\"\n")
    }

    @Test(.enabled(if: FileManager.default.isExecutableFile(atPath: RealBinary.binary.path)))
    @MainActor func unitsAndKnowledge() async throws {
        let s = try Scratch()
        let instance = s.url.appending(path: "acme")
        try FileManager.default.copyItem(at: RealBinary.engine.appending(path: "examples/minimal"), to: instance)
        let env = ShellEnvironment.compose(shell: nil, process: ProcessInfo.processInfo.environment)
        let cli = StormoCLI(executable: RealBinary.binary, environment: env, instance: instance)

        let catalog = UnitCatalog()
        await catalog.load(with: cli)
        #expect(catalog.units.map(\.id) == ["group", "sales", "support"] && catalog.problem == nil)
        #expect(catalog.unit("sales")?.agents == ["atlas", "scout"] && catalog.unit("sales")?.knowledge.first?.compiled == false)

        // The form saves a unit.yaml as a patch; its id is the folder's and stays.
        let editor = ConfigEditor(path: "units/sales/unit.yaml")
        await editor.load(with: cli)
        #expect(editor.file?.kind == "unit" && editor.file?.unit == "sales" && editor.doc?[["name"]] == .string("Acme Sales"))
        editor.doc?.set(["description"], .string("Deals and quotes."))
        #expect(await editor.save(with: cli))
        #expect(try String(contentsOf: instance.appending(path: "units/sales/unit.yaml"), encoding: .utf8).contains("description: Deals and quotes."))
        editor.doc?.set(["id"], .string("deals"))
        #expect(!(await editor.save(with: cli)) && editor.problem?.code == "readonly")
        editor.revert()

        // A new unit, then a knowledge file in it.
        let made = try #require(await catalog.createUnit(id: "billing", name: "Acme Billing", description: "Invoices: \"net 30\".", with: cli))
        #expect(made.name == "Acme Billing" && made.description == "Invoices: \"net 30\"." && made.agents.isEmpty)
        #expect(await catalog.createUnit(id: "billing", name: "Again", description: "", with: cli) == nil && catalog.problem?.code == "exists")
        let path = try #require(await catalog.createKnowledge(unit: "billing", name: "terms.md", with: cli))
        #expect(catalog.unit("billing")?.knowledge.map(\.name) == ["terms.md"])

        let doc = ConfigEditor(path: path)
        await doc.load(with: cli)
        #expect(doc.file?.kind == "knowledge" && doc.text == "# Terms\n\n")
        doc.text += "Invoices are due in 30 days.\n"
        #expect(await doc.save(with: cli))
        #expect(await doc.delete(with: cli))
        await catalog.load(with: cli)
        #expect(catalog.unit("billing")?.knowledge.isEmpty == true)
    }
}

/// Export and import through this checkout's bin/stormo, as the app's sheets run them.
@Suite struct Transfer {
    @Test(.enabled(if: FileManager.default.isExecutableFile(atPath: RealBinary.binary.path)))
    func exportThenImportUnderAnotherID() async throws {
        let s = try Scratch()
        let env = ShellEnvironment.compose(shell: nil, process: ProcessInfo.processInfo.environment)
        let a = s.url.appending(path: "a"), b = s.url.appending(path: "b")
        try FileManager.default.copyItem(at: RealBinary.engine.appending(path: "examples/minimal"), to: a)
        try FileManager.default.copyItem(at: RealBinary.engine.appending(path: "examples/minimal"), to: b)
        let zip = s.url.appending(path: "atlas.zip")
        let exported = try await StormoCLI(executable: RealBinary.binary, environment: env, instance: a)
            .run(["export", "atlas", "-o", zip.path], as: ExportResult.self)
        #expect(exported.mode == "config" && !exported.containsSecrets && exported.files > 5)
        // b already has an atlas with Slack slash commands: a second one would not check.
        let cli = StormoCLI(executable: RealBinary.binary, environment: env, instance: b)
        await #expect(throws: CLIError.self) { _ = try await cli.run(["import", zip.path], as: ImportResult.self) }
        try FileManager.default.removeItem(at: b.appending(path: "agents/atlas"))
        let imported = try await cli.run(["import", zip.path, "--as", "orion"], as: ImportResult.self)
        #expect(imported.agent == "orion" && imported.from == "acme" && imported.changes.first == "added agents/orion")
        let yaml = try String(contentsOf: b.appending(path: "agents/orion/agent.yaml"), encoding: .utf8)
        #expect(yaml.contains("id: orion"))
    }
}

/// Connections and a shared key through this checkout's bin/stormo, as the Providers page runs them.
@Suite struct Providers {
    @Test(.enabled(if: FileManager.default.isExecutableFile(atPath: RealBinary.binary.path)))
    func addAConnectionAndItsKey() async throws {
        let s = try Scratch()
        let root = s.url.appending(path: "acme")
        try FileManager.default.copyItem(at: RealBinary.engine.appending(path: "examples/minimal"), to: root)
        let env = ShellEnvironment.compose(shell: nil, process: ProcessInfo.processInfo.environment)
        let cli = StormoCLI(executable: RealBinary.binary, environment: env, instance: root)
        let kinds = try await cli.run(["connections", "kinds"], as: [ConnectionKindInfo].self)
        #expect(kinds.contains { $0.kind == "openai" && $0.key == "OPENAI_API_KEY" && $0.api })
        let before = try await cli.run(["connections"], as: [ConnectionRow].self)
        #expect(before.map(\.name) == ["openrouter", "chatgpt"] && before[0].usedBy.contains("atlas"))
        let rows = try await cli.run(["connections", "add", "openai", "--kind", "openai"], as: [ConnectionRow].self)
        #expect(rows.first?.name == "openai" && rows.first?.keySet == false)
        _ = try await cli.run(["secrets", "set", "shared", "OPENAI_API_KEY"], as: [String: String].self, stdin: Data("sk-test".utf8))
        let after = try await cli.run(["connections"], as: [ConnectionRow].self)
        #expect(after.first { $0.name == "openai" }?.keySet == true)
        await #expect(throws: CLIError.self) { _ = try await cli.run(["connections", "remove", "openrouter"], as: [ConnectionRow].self) }
    }
}

@Test func missingSecretsReadsBothCLIWordings() {
    let run = MissingSecrets(error: "atlas: missing secrets OPENROUTER_API_KEY, SLACK_BOT_TOKEN in /Users/x/Library/Application Support/Stormo/Instances/acme/secrets.local.yaml (stormo secrets init / check)")
    #expect(run == MissingSecrets(agent: "atlas", names: ["OPENROUTER_API_KEY", "SLACK_BOT_TOKEN"]))
    let ops = MissingSecrets(error: "nova: missing secrets for a local run: APIFY_API_TOKEN (stormo secrets check nova)")
    #expect(ops == MissingSecrets(agent: "nova", names: ["APIFY_API_TOKEN"]))
    #expect(MissingSecrets(error: "atlas: container exited") == nil)
}

@MainActor @Test func aForgottenAgentStaysHiddenUntilTheCoreDropsIt() {
    let store = FleetStore(client: CoreClient(base: URL(string: "http://127.0.0.1:1")!))
    store.forget("nova")
    #expect(store.agent("nova") == nil)
}
