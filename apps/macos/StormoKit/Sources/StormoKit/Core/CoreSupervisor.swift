import Foundation
import Observation

/// What answers on the core's port, from this instance's point of view.
public enum CoreState: Sendable, Equatable {
    case unknown
    /// Nothing answers.
    case down
    /// `core up` or `core down` is running.
    case starting
    case stopping
    /// This instance's core.
    case running(CoreInfo)
    /// A core for another instance holds the port (one core per machine).
    case otherInstance(CoreInfo)
    /// A core this app cannot read: older than API 1 or outside the supported range.
    case incompatible(version: String?, api: Int?)

    /// Classifies what the port answered. `core` is nil when /api/core does not exist.
    public static func classify(health: Health?, core: CoreInfo?, instanceRoot: URL?) -> CoreState {
        guard let health else { return .down }
        guard let api = health.api, StormoAPI.supported.contains(api) else {
            return .incompatible(version: health.version, api: health.api)
        }
        // Every core that speaks a supported API serves /api/core: a miss is a slow or restarting
        // core, not an old one.
        guard let core else { return .unknown }
        if let instanceRoot, samePath(core.instance.root, instanceRoot.path) {
            return .running(core)
        }
        return .otherInstance(core)
    }

    static func samePath(_ a: String, _ b: String) -> Bool {
        URL(filePath: a).standardizedFileURL.resolvingSymlinksInPath().path
            == URL(filePath: b).standardizedFileURL.resolvingSymlinksInPath().path
    }

    public var info: CoreInfo? {
        switch self {
        case .running(let c), .otherInstance(let c): c
        default: nil
        }
    }

    public var isRunning: Bool {
        if case .running = self { true } else { false }
    }
}

/// Watches and drives the instance's core. It never runs `core serve` itself: starting and stopping
/// go through `core up` / `core down` of the pinned binary, so the CLI and the app see one core.
@MainActor @Observable
public final class CoreSupervisor {
    public private(set) var state: CoreState = .unknown
    public private(set) var lastError: String?
    public var client: CoreClient
    public var instanceRoot: URL?

    public init(client: CoreClient, instanceRoot: URL?) {
        self.client = client
        self.instanceRoot = instanceRoot
    }

    /// Re-reads /health and /api/core.
    public func refresh() async {
        if state == .starting || state == .stopping { return }
        state = await probe()
    }

    private func probe() async -> CoreState {
        let health = try? await client.health()
        var core: CoreInfo?
        if health != nil { core = try? await client.core() }
        return CoreState.classify(health: health, core: core, instanceRoot: instanceRoot)
    }

    /// `core up`; the state follows what then answers.
    public func start(with cli: StormoCLI) async {
        lastError = nil
        state = .starting
        do {
            _ = try await cli.run(["core", "up"], as: CoreUpResult.self)
        } catch {
            lastError = error.localizedDescription
        }
        state = await probe()
    }

    /// `core down`. A core `core up` did not start is left alone, as the CLI does.
    public func stop(with cli: StormoCLI) async {
        lastError = nil
        state = .stopping
        do {
            let r = try await cli.run(["core", "down"], as: CoreDownResult.self)
            if r.reason == "not_ours" {
                lastError = "This core was not started with `stormo core up`, so it is left running."
            }
        } catch {
            lastError = error.localizedDescription
        }
        try? await Task.sleep(for: .milliseconds(300))
        state = await probe()
    }

    /// A running core built from another version than the pinned binary (typically after an app update).
    public func isOutdated(comparedTo pinned: VersionInfo?) -> Bool {
        guard let pinned, case .running(let core) = state else { return false }
        return core.version != pinned.version
            && CoreState.samePath(core.exe, pinned.exe)
    }
}
