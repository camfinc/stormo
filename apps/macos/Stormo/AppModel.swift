import AppKit
import Observation
import StormoKit

/// The app's state: which instance, which binary, what the core and the fleet are doing.
@Observable
final class AppModel {
    let instances = InstanceStore(defaults: AppModel.defaults)

    /// Debug builds can keep their instance list apart from the installed app's (STORMO_DEFAULTS_SUITE).
    static var defaults: UserDefaults {
        #if DEBUG
        if let suite = ProcessInfo.processInfo.environment["STORMO_DEFAULTS_SUITE"], let d = UserDefaults(suiteName: suite) { return d }
        #endif
        return .standard
    }
    let core: CoreSupervisor
    let fleet: FleetStore

    /// The environment every stormo command gets (ShellEnvironment), nil until captured.
    private(set) var environment: [String: String]?
    /// The binaries found for the active instance, probed.
    private(set) var candidates: [BinaryCandidate] = []
    /// What the app is doing right now ("Starting atlas…"), for the toolbar.
    private(set) var activity: String?
    /// The last failed action, shown as an alert.
    var failure: String?
    /// Agents' portraits (/avatars/<id>.png), for name tags and the inspector.
    private(set) var avatars: [String: NSImage] = [:]

    @ObservationIgnored private var poller: Task<Void, Never>?

    /// The stormo inside the app.
    static let bundledBinary = Bundle.main.bundleURL.appending(path: "Contents/Helpers/stormo")

    init() {
        let client = CoreClient(port: CoreClient.port(environment: ProcessInfo.processInfo.environment))
        core = CoreSupervisor(client: client, instanceRoot: nil)
        fleet = FleetStore(client: client)
    }

    // MARK: Startup

    /// Captures the environment, resolves the active instance's binary and starts polling.
    func start() async {
        let env = await ShellEnvironment.current()
        environment = env
        let client = CoreClient(port: CoreClient.port(environment: env))
        core.client = client
        fleet.client = client
        await activate(instances.active)
        startPolling()
    }

    func activate(_ record: InstanceRecord?) async {
        if let record { instances.activate(record.root) }
        core.instanceRoot = record?.url
        fleet.clear()
        await core.refresh()
        await resolveBinary()
    }

    // MARK: Binaries

    var resolver: BinaryResolver {
        let bundled = FileManager.default.isExecutableFile(atPath: Self.bundledBinary.path) ? Self.bundledBinary : nil
        return BinaryResolver(bundled: bundled, environment: environment ?? [:])
    }

    /// Probes the binaries and pins Auto's choice the first time an instance is seen.
    func resolveBinary() async {
        let active = instances.active
        candidates = await resolver.candidates(instance: active?.url)
        guard let active, active.binary == nil else { return }
        if let choice = BinaryResolver.autoChoice(candidates, runningCoreExe: core.state.info?.exe) {
            instances.pin(active.root, binary: choice.resolved.path, choice: "auto")
        }
    }

    /// The pinned binary for the active instance, if it is still there and usable.
    var pinned: BinaryCandidate? {
        guard let path = instances.active?.binary else { return nil }
        return candidates.first { $0.resolved.path == path && $0.usable }
    }

    func pin(_ candidate: BinaryCandidate) {
        guard let active = instances.active else { return }
        instances.pin(active.root, binary: candidate.resolved.path, choice: candidate.source.rawValue)
    }

    /// The CLI for the active instance through its pinned binary.
    var cli: StormoCLI? {
        guard let active = instances.active, let pinned, let environment else { return nil }
        return StormoCLI(executable: pinned.url, environment: environment, instance: active.url)
    }

    // MARK: Instances

    /// Validates a folder with `stormo instance --json` and makes it the active instance.
    func open(folder: URL) async {
        let probe = await resolver.candidates(instance: folder)
        guard let bin = BinaryResolver.autoChoice(probe, runningCoreExe: nil), let environment else {
            failure = "No usable stormo binary was found to open \(folder.path)."
            return
        }
        do {
            let info = try await StormoCLI(executable: bin.url, environment: environment, instance: folder)
                .run(["instance"], as: InstanceInfo.self)
            instances.add(info)
            await activate(instances.active)
        } catch {
            failure = "\(folder.lastPathComponent) is not a Stormo instance: \(error.localizedDescription)"
        }
    }

    /// `stormo new instance` with the app's own binary (its release always has the command), then
    /// `secrets init`, then opens it. Returns the problem, nil on success.
    func createInstance(name: String, org: String, slug: String, template: String, at folder: URL) async -> String? {
        guard let environment else { return "Still reading your shell's environment; try again in a moment." }
        let bundled = FileManager.default.isExecutableFile(atPath: Self.bundledBinary.path) ? Self.bundledBinary : nil
        let fallback = BinaryResolver.autoChoice(await resolver.candidates(instance: nil), runningCoreExe: nil)?.url
        guard let binary = bundled ?? fallback else { return "No usable stormo binary was found." }
        var command = ["new", "instance", folder.path, "--name", name, "--slug", slug, "--template", template]
        let org = org.trimmingCharacters(in: .whitespaces)
        if !org.isEmpty { command += ["--org", org] }
        do {
            try FileManager.default.createDirectory(at: folder.deletingLastPathComponent(), withIntermediateDirectories: true)
            let made = try await StormoCLI(executable: binary, environment: environment, instance: nil)
                .run(command, as: NewInstanceResult.self)
            let root = URL(filePath: made.root)
            try await StormoCLI(executable: binary, environment: environment, instance: root).perform(["secrets", "init"])
            await open(folder: root)
            return nil
        } catch {
            return error.localizedDescription
        }
    }

    // MARK: Core

    func startCore() async {
        guard let cli else { return failNoBinary() }
        await core.start(with: cli)
        if let e = core.lastError { failure = e }
        await fleet.refresh()
    }

    func restartCore() async {
        guard let cli else { return failNoBinary() }
        fleet.clear()
        await core.restart(with: cli)
        if let e = core.lastError { failure = e }
        await fleet.refresh()
    }

    func stopCore() async {
        guard let cli else { return failNoBinary() }
        await core.stop(with: cli)
        if let e = core.lastError { failure = e }
        fleet.clear()
    }

    // MARK: Agents

    /// Runs a lifecycle command (start, stop, restart, nap-now) for agents through the pinned binary.
    func run(_ action: String, agents: [String]) async {
        guard let cli else { return failNoBinary() }
        let label = agents.count == 1 ? agents[0] : "\(agents.count) agents"
        activity = "\(action.capitalized) \(label)…"
        defer { activity = nil }
        do {
            _ = try await cli.run([action] + agents, as: LifecycleResult.self) { [weak self] event in
                if case .step(let msg) = event {
                    Task { @MainActor in self?.activity = msg }
                }
            }
        } catch {
            failure = error.localizedDescription
        }
        await fleet.refresh()
    }

    private func failNoBinary() {
        failure = "No usable stormo binary for this instance. Choose one in Settings › Command line."
    }

    // MARK: Polling

    /// /health and /api/core every 2.5 s, /api/fleet while this instance's core runs.
    func startPolling() {
        poller?.cancel()
        poller = Task { [weak self] in
            while !Task.isCancelled {
                guard let self else { return }
                await self.core.refresh()
                if self.core.state.isRunning {
                    await self.fleet.refresh()
                    await self.loadAvatars()
                } else if self.fleet.fleet != nil {
                    self.fleet.clear()
                }
                try? await Task.sleep(for: .milliseconds(2500))
            }
        }
    }

    /// Fetches portraits the fleet says exist and the app does not have yet.
    private func loadAvatars() async {
        for a in fleet.fleet?.agents ?? [] where a.avatar == true && avatars[a.id] == nil {
            if let data = try? await fleet.client.avatar(agent: a.id), let image = NSImage(data: data) {
                avatars[a.id] = image
            }
        }
    }

    // MARK: Command line tool

    /// ~/.local/bin/stormo → the bundled binary, when no stormo is on PATH.
    static let commandLineLink = FileManager.default.homeDirectoryForCurrentUser.appending(path: ".local/bin/stormo")

    var canInstallCommandLineTool: Bool {
        !candidates.contains { $0.source == .path } && !FileManager.default.fileExists(atPath: Self.commandLineLink.path)
    }

    func installCommandLineTool() async {
        do {
            try FileManager.default.createDirectory(at: Self.commandLineLink.deletingLastPathComponent(), withIntermediateDirectories: true)
            try FileManager.default.createSymbolicLink(at: Self.commandLineLink, withDestinationURL: Self.bundledBinary)
        } catch {
            failure = "Could not install the stormo command: \(error.localizedDescription)"
        }
        await resolveBinary()
    }
}
