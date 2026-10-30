import Foundation
import Observation

/// The Review section: proposed lessons and skills, decided with the owner token. Held in memory
/// only (lessons are scrubbed, but skill files are the agent's own writing).
@MainActor @Observable
public final class ReviewStore {
    public private(set) var view: ReviewView? {
        didSet { if view == nil { diffs = [:] } }
    }
    /// Each proposed skill file's diff, by `diffKey`: made once per load off the main thread, not in
    /// every row's body (a backlog is a thousand files, re-diffed on each Accept).
    public private(set) var diffs: [String: LineDiff] = [:]
    public private(set) var loading = false
    private var loadContext: UUID?
    /// The last failure, in the core's words when it gave some.
    public var problem: String?
    /// Decisions on their way to the core, by `ReviewDecision.key`.
    public private(set) var deciding: Set<String> = []
    /// Agents restarting now.
    public private(set) var restarting: Set<String> = []

    public private(set) var contextID = UUID()
    public private(set) var learningViews: [String: LearningReviewView] = [:]
    public private(set) var learningLoading: Set<String> = []
    public private(set) var learningPending: Set<String> = []
    public private(set) var learningProblems: [String: String] = [:]
    public private(set) var learningSessions: [String: String] = [:]

    public var client: CoreClient {
        didSet { if client.base != oldValue.base || client.token != oldValue.token { clearLearning(); view = nil; problem = nil } }
    }
    public var instanceRoot: URL? {
        didSet { if instanceRoot != oldValue { view = nil; problem = nil; clearLearning() } }
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
        let context = contextID
        guard let owner = token(), loadContext != context else { return }
        loadContext = context
        loading = true
        defer { if loadContext == context { loading = false; loadContext = nil } }
        do {
            // Proposed skills carry their files' text: a backlog is megabytes, more than 2 s. Its own
            // task: the view's reload task is cancelled when the pending count changes mid-load, and
            // the reload it starts returns while this one is in flight, leaving "cancelled" shown.
            let client = client.with(timeout: 40)
            let result = try await Task { try await client.review(owner: owner) }.value
            let diffs = await Task.detached { Self.diffs(of: result) }.value
            guard contextID == context else { return }
            view = result
            self.diffs = diffs
            problem = nil
        } catch {
            if contextID == context { problem = Self.describe(error) }
        }
    }

    nonisolated public static func diffKey(agent: String, skill: String, path: String) -> String {
        [agent, skill, path].joined(separator: "\u{0}")
    }

    nonisolated static func diffs(of view: ReviewView) -> [String: LineDiff] {
        var out: [String: LineDiff] = [:]
        for agent in view.agents {
            for skill in agent.skills {
                for file in skill.files {
                    guard let text = file.text else { continue }
                    out[diffKey(agent: agent.id, skill: skill.skill, path: file.path)] = LineDiff(old: file.current ?? "", new: text)
                }
            }
        }
        return out
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
            try await client.with(timeout: 180).restart(agent: agent, owner: owner)
            problem = nil
        } catch {
            problem = Self.describe(error)
        }
        await load()
    }

    public func loadLearning(agent: String, session: String? = nil) async {
        guard let owner = token(), !learningLoading.contains(agent), !learningPending.contains(agent) else { return }
        let root = instanceRoot, base = client.base, generation = contextID
        learningLoading.insert(agent)
        defer { if generation == contextID { learningLoading.remove(agent) } }
        do {
            let result = try await client.with(timeout: 40).learning(agent: agent, session: session, owner: owner)
            guard root == instanceRoot, base == client.base, generation == contextID else { return }
            learningViews[agent] = result
            learningSessions[agent] = session
            learningProblems[agent] = nil
        } catch { if root == instanceRoot, base == client.base, generation == contextID { learningProblems[agent] = Self.describe(error) } }
    }

    public func sendLearning(_ feedback: LearningFeedback, agent: String, session: String) async -> Bool {
        guard let owner = token(), !learningPending.contains(agent), !learningLoading.contains(agent) else { return false }
        let root = instanceRoot, base = client.base, generation = contextID
        learningPending.insert(agent)
        defer { if generation == contextID { learningPending.remove(agent) } }
        do {
            let result = try await client.with(timeout: 40).learning(feedback, agent: agent, session: session, owner: owner)
            guard root == instanceRoot, base == client.base, generation == contextID else { return false }
            learningViews[agent] = result
            learningProblems[agent] = nil
            return result.state == "ready" && (result.data?.operation?.ok != false || result.data?.operation?.feedbackSaved == true)
        } catch {
            if root == instanceRoot, base == client.base, generation == contextID { learningProblems[agent] = Self.describe(error) }
            return false
        }
    }

    private func clearLearning() {
        contextID = UUID(); loading = false; loadContext = nil; learningViews = [:]; learningProblems = [:]; learningSessions = [:]; learningLoading = []; learningPending = []
    }

    public func clear() {
        clearLearning()
        view = nil
        problem = nil
    }

    private func token() -> String? {
        if let t = client.token { return t }
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
