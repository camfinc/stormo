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
    /// The Review section's proposals (in memory only).
    let review: ReviewStore
    /// The Files section's shared space (in memory only: names and contents can name clients).
    let files: FilesStore
    /// The Chat section's conversations (in memory only: they hold client data).
    let chat = ChatStore()

    /// The environment every stormo command gets (ShellEnvironment), nil until captured.
    private(set) var environment: [String: String]?
    /// The binaries found for the active instance, probed.
    private(set) var candidates: [BinaryCandidate] = []
    /// What the app is doing right now ("Starting atlas…"), for the toolbar.
    private(set) var activity: String?
    /// The last failed action, shown as an alert.
    var failure: String?
    /// A start refused for unset secret values, shown as its own alert that leads to the fix.
    var missingSecrets: MissingSecrets?
    /// An agent whose editor the Agents section should open (set from elsewhere, consumed there).
    var agentToEdit: String?
    /// Agents' portraits (/avatars/<id>.png), for name tags and the inspector.
    private(set) var avatars: [String: NSImage] = [:]

    @ObservationIgnored private var poller: Task<Void, Never>?

    /// The stormo inside the app.
    static let bundledBinary = Bundle.main.bundleURL.appending(path: "Contents/Helpers/stormo")

    init() {
        let client = CoreClient(port: CoreClient.port(environment: ProcessInfo.processInfo.environment))
        core = CoreSupervisor(client: client, instanceRoot: nil)
        fleet = FleetStore(client: client)
        review = ReviewStore(client: client, instanceRoot: nil)
        files = FilesStore(client: client, instanceRoot: nil)
    }

    // MARK: Startup

    /// Captures the environment, resolves the active instance's binary and starts polling.
    func start() async {
        let env = await ShellEnvironment.current()
        environment = env
        await activate(instances.active)
        startPolling()
    }

    func activate(_ record: InstanceRecord?) async {
        if let record { instances.activate(record.root) }
        core.instanceRoot = record?.folder
        core.remoteSlug = record?.isRemote == true ? record?.slug : nil
        review.instanceRoot = record?.folder
        files.instanceRoot = record?.url
        fleet.clear()
        retarget()
        await core.refresh()
        await resolveBinary()
        // An instance that never ran a core has no port yet: the engine picks one free of every
        // other instance's (`core status` resolves and keeps it).
        if let root = record?.folder, CoreClient.pickedPort(root: root) == nil, let cli {
            try? await cli.perform(["core", "status"])
            retarget()
            await core.refresh()
        }
    }

    /// Points the clients at the active instance's core port when it changed.
    func retarget() {
        let client: CoreClient
        if let active = instances.active, let remote = active.remote, let base = URL(string: remote) {
            // A remote core: its URL, the token in the Keychain, the certificate the user trusted.
            client = CoreClient(base: base, token: CoreTokens.get(active.id), pin: active.pin, timeout: 8)
        } else {
            let root = instances.active?.folder
            let port = CoreClient.port(instanceRoot: root, environment: environment ?? ProcessInfo.processInfo.environment)
            client = CoreClient(port: port, token: root.flatMap { ReviewStore.ownerToken(root: $0) })
        }
        guard client.base != core.client.base || client.token != core.client.token else { return }
        core.client = client
        fleet.client = client
        review.client = client
        files.client = client
        files.clear()
        fleet.clear()
        avatars = [:]
    }

    // MARK: Binaries

    var resolver: BinaryResolver {
        let bundled = FileManager.default.isExecutableFile(atPath: Self.bundledBinary.path) ? Self.bundledBinary : nil
        return BinaryResolver(bundled: bundled, environment: environment ?? [:])
    }

    /// Probes the binaries and, for an instance on Auto, pins Auto's choice (on every look, so an
    /// app update moves it to the new bundled copy). A binary picked in Settings stays.
    func resolveBinary() async {
        let active = instances.active
        candidates = await resolver.candidates(instance: active?.folder)
        guard let active, !active.isRemote else { return }
        if let choice = BinaryResolver.repin(binary: active.binary, choice: active.binaryChoice, candidates: candidates, runningCoreExe: core.state.info?.exe) {
            instances.pin(active.root, binary: choice.resolved.path, choice: BinaryResolver.autoPin)
        }
    }

    /// The active instance follows Auto (no binary picked in Settings).
    var followsAuto: Bool {
        guard let choice = instances.active?.binaryChoice else { return true }
        return choice == BinaryResolver.autoPin
    }

    /// Back to Auto after a binary was picked in Settings.
    func useAuto() {
        guard let active = instances.active,
              let choice = BinaryResolver.autoChoice(candidates, runningCoreExe: core.state.info?.exe) else { return }
        instances.pin(active.root, binary: choice.resolved.path, choice: BinaryResolver.autoPin)
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
        guard let active = instances.active, !active.isRemote, let pinned, let environment else { return nil }
        return StormoCLI(executable: pinned.url, environment: environment, instance: active.url)
    }

    // MARK: Remote cores

    /// The Connect to Remote Core sheet is open.
    var connectingRemote = false

    enum RemoteProbe: Equatable {
        case connected
        /// The certificate is not trusted by this Mac: its fingerprint, to trust on first use.
        case untrusted(String)
        case failed(String)
    }

    /// Checks a remote core with the token (`/api/core` needs read) and, when it answers, adds it
    /// and makes it active. The token goes to the Keychain.
    func connectRemote(url raw: String, token: String, pin: String?) async -> RemoteProbe {
        let text = raw.trimmingCharacters(in: .whitespaces).trimmingCharacters(in: CharacterSet(charactersIn: "/"))
        guard let base = URL(string: text), let scheme = base.scheme, ["https", "http"].contains(scheme), base.host != nil else {
            return .failed("Enter the core's URL, like https://core.example:18600.")
        }
        let client = CoreClient(base: base, token: token, pin: pin, timeout: 8)
        do {
            let info = try await client.core()
            let record = instances.addRemote(url: text, name: info.instance.name, slug: info.instance.slug, pin: pin)
            CoreTokens.set(token, for: record.id)
            await activate(record)
            return .connected
        } catch CoreClient.Failure.unreachable {
            if scheme == "https", pin == nil, let fp = client.pinning.lastFingerprint { return .untrusted(fp) }
            return .failed("Nothing answers at \(text). Check the address and that a tunnel or VPN is up if one is needed.")
        } catch CoreClient.Failure.refused(let code, let message) {
            return .failed(code == 401 ? "The core refused the token: \(message)" : message)
        } catch {
            return .failed("That is not a Stormo core, or it is too old: \(error.localizedDescription)")
        }
    }

    /// Forgets a remote core and its token.
    func forgetRemote(_ record: InstanceRecord) async {
        CoreTokens.delete(record.id)
        instances.remove(record.root)
        await activate(instances.active)
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
        if let root = instances.active?.folder, CoreClient.pickedPort(root: root) == nil {
            try? await cli.perform(["core", "status"])
        }
        retarget()
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
        if instances.active?.isRemote == true { return await runRemote(action, agents: agents) }
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
            if let m = MissingSecrets(error: error.localizedDescription) {
                missingSecrets = m
            } else {
                failure = error.localizedDescription
            }
        }
        await fleet.refresh()
    }

    /// A production change waiting for the person to confirm its exact plan.
    struct PendingConfirm: Identifiable {
        let id = UUID()
        let action: String
        let agent: String
        let placement: String?
        let plan: [String]
    }

    var pendingConfirm: PendingConfirm?

    /// Start, stop or restart through the core (a remote instance has no CLI here): where the
    /// agent's link says it runs, else the first placement available. ECS asks for confirmation.
    func runRemote(_ action: String, agents: [String], placement: String? = nil, confirm: Bool = false) async {
        let label = agents.count == 1 ? agents[0] : "\(agents.count) agents"
        activity = "\(action.capitalized) \(label)…"
        defer { activity = nil }
        var available: [PlacementInfo] = []
        if placement == nil { available = (try? await core.client.placements())?.filter(\.available) ?? [] }
        for id in agents {
            let agent = fleet.fleet?.agents.first { $0.id == id }
            let where_ = placement ?? agent?.link?.placement ?? available.first(where: { $0.kind == "runner" })?.name ?? available.first?.name
            do {
                switch try await core.client.lifecycle(agent: id, action: action, placement: where_, confirm: confirm) {
                case .done: break
                case .needsConfirm(let plan):
                    pendingConfirm = PendingConfirm(action: action, agent: id, placement: where_, plan: plan)
                    return
                }
            } catch CoreClient.Failure.refused(_, let message) {
                failure = "\(action.capitalized) \(id): \(message)"
            } catch {
                failure = "\(action.capitalized) \(id): \(error.localizedDescription)"
            }
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
                self.retarget()
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
