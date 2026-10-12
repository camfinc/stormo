import Foundation
import Observation

/// The Review section: proposed lessons and skills, decided with the owner token. Held in memory
/// only (lessons are scrubbed, but skill files are the agent's own writing).
@MainActor @Observable
public final class ReviewStore {
    public private(set) var view: ReviewView?
    public private(set) var loading = false
    /// The last failure, in the core's words when it gave some.
    public var problem: String?
    /// Decisions on their way to the core, by `ReviewDecision.key`.
    public private(set) var deciding: Set<String> = []
    /// Agents restarting now.
    public private(set) var restarting: Set<String> = []

    public var client: CoreClient
    public var instanceRoot: URL? {
        didSet { if instanceRoot != oldValue { view = nil; problem = nil } }
    }

    public init(client: CoreClient, instanceRoot: URL?) {
        self.client = client
        self.instanceRoot = instanceRoot
    }

    public var pending: Int { view?.agents.reduce(0) { $0 + $1.pending } ?? 0 }

    /// `<instance>/.swarm/core/owner.token`, minted by the core on its first start.
    nonisolated public static func ownerToken(root: URL) -> String? {
        let url = root.appending(path: ".swarm/core/owner.token")
        guard let s = try? String(contentsOf: url, encoding: .utf8) else { return nil }
        let t = s.trimmingCharacters(in: .whitespacesAndNewlines)
        return t.isEmpty ? nil : t
    }

    public func load() async {
        guard let owner = token() else { return }
        loading = true
        defer { loading = false }
        do {
            view = try await client.review(owner: owner)
            problem = nil
        } catch {
            problem = Self.describe(error)
        }
    }

    public func decide(_ decision: ReviewDecision) async {
        guard let owner = token(), !deciding.contains(decision.key) else { return }
        deciding.insert(decision.key)
        defer { deciding.remove(decision.key) }
        do {
            try await client.decide(decision, owner: owner)
            problem = nil
        } catch {
            problem = Self.describe(error)
        }
        await load()
    }

    public func restart(agent: String) async {
        guard let owner = token() else { return }
        restarting.insert(agent)
        defer { restarting.remove(agent) }
        do {
            try await CoreClient(base: client.base, timeout: 180).restart(agent: agent, owner: owner)
            problem = nil
        } catch {
            problem = Self.describe(error)
        }
        await load()
    }

    public func clear() {
        view = nil
        problem = nil
    }

    private func token() -> String? {
        guard let root = instanceRoot else { return nil }
        guard let t = Self.ownerToken(root: root) else {
            problem = "No owner token yet: start the core once to mint it."
            return nil
        }
        return t
    }

    nonisolated static func describe(_ error: Error) -> String {
        switch error {
        case CoreClient.Failure.refused(_, let message): return message
        case CoreClient.Failure.unreachable: return "The core is not answering."
        case CoreClient.Failure.notFound: return "This core has no review API: restart it with the app's stormo."
        case CoreClient.Failure.status(let code): return "The core answered \(code)."
        default: return error.localizedDescription
        }
    }
}
