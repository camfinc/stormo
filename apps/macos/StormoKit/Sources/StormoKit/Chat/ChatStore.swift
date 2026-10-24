import Foundation
import Observation

/// What the chat needs from `stormo` (StormoCLI; a fake in tests).
public protocol ChatBackend: Sendable {
    func conversations(_ agent: String) async throws -> [Conversation]
    func transcript(_ agent: String, _ id: String) async throws -> Transcript
    func newConversation(_ agent: String, title: String?) async throws -> Transcript
    func send(_ agent: String, session: String, message: String) async throws -> ChatReply
    func clearConversation(_ agent: String, _ id: String) async throws -> ClearResult
    func clearConversations(_ agent: String) async throws -> ClearResult
}

extension StormoCLI: ChatBackend {}

/// Why the chat could not do something, by the CLI's error code (docs/api.md).
public struct ChatProblem: Equatable, Sendable {
    /// The agent's API on this Mac did not answer: its container is up but the server inside is not
    /// listening yet (or is restarting), so the request was refused or cut off.
    static func unreachable(_ message: String) -> Bool {
        let m = message.lowercased()
        guard m.contains("127.0.0.1") || m.contains("localhost") else { return false }
        return m.hasSuffix(": eof") || m.contains("connection refused") || m.contains("connection reset")
            || m.contains("server closed") || m.contains("empty reply")
    }

    public enum Kind: Sendable { case notRunning, starting, busy, notFound, other }
    public var kind: Kind
    public var message: String

    public init(kind: Kind, message: String) {
        self.kind = kind
        self.message = message
    }

    public init(_ error: any Error) {
        let code = (error as? CLIError)?.code
        kind = switch code {
        case "not_running": .notRunning
        case "busy": .busy
        case "not_found": .notFound
        default: Self.unreachable(error.localizedDescription) ? .starting : .other
        }
        message = switch kind {
        case .notRunning: "The agent is not running here. Start it to chat."
        case .starting: "The agent isn't ready yet. It is still starting up; the chat connects on its own when it is."
        case .busy: "The agent is busy with other turns. Try again in a moment."
        default: error.localizedDescription
        }
    }
}

/// When the open conversation is read again with no turn of the app's in flight (a follow-up the
/// agent set itself, a turn from elsewhere): soon after it last changed, less often the longer it
/// stays the same.
public struct RefreshSchedule: Equatable, Sendable {
    public var fastest: Duration
    public var slowest: Duration
    /// The wait before the next read.
    public private(set) var delay: Duration

    public init(fastest: Duration, slowest: Duration) {
        self.fastest = fastest
        self.slowest = max(slowest, fastest)
        delay = fastest
    }

    /// A read found the conversation changed (back to the fastest) or the same (half as long again).
    public mutating func record(changed: Bool) {
        delay = changed ? fastest : min(delay * 1.5, slowest)
    }

    /// Something says the conversation may move (opened, a turn just ended): read it soon.
    public mutating func reset() { delay = fastest }

    /// Whether a read is due `since` the last one; a nudge (the agent did something) waits only the
    /// fastest interval, so a busy agent is read at most that often.
    public func isDue(since: Duration, nudged: Bool) -> Bool {
        since >= (nudged ? fastest : delay)
    }
}

extension Transcript {
    /// The same rows as `other`, as far as a re-read can tell cheaply: the session, the
    /// compaction, the number of rows and the last one. Rows are only appended, so this is what
    /// moves when anything does.
    public func sameRows(as other: Transcript) -> Bool {
        tip == other.tip && compacted == other.compacted && messages.count == other.messages.count
            && messages.last == other.messages.last
    }
}

/// A turn on its way: the message shows at once, the reply when the agent has finished.
public struct PendingTurn: Equatable, Sendable {
    public var agent: String
    public var conversation: String?
    public var text: String
    public var started: Date
}

/// The Chat section: one agent's conversations, the open one's transcript, a turn in flight.
@MainActor @Observable
public final class ChatStore {
    public var backend: (any ChatBackend)?
    public private(set) var agent: String?
    public private(set) var conversations: [Conversation] = []
    public private(set) var loadingList = false
    /// The open conversation; nil is a new one, created by its first message.
    public private(set) var selection: String?
    public private(set) var transcript: Transcript?
    public private(set) var loadingTranscript = false
    public private(set) var pending: PendingTurn?
    public var problem: ChatProblem? {
        didSet { if problem?.kind == .starting { retryWhileStarting() } }
    }
    @ObservationIgnored private var retryTask: Task<Void, Never>?

    /// How often, and for how long, a starting agent is tried again before the problem stays.
    public var startingRetry: (every: Duration, for: Duration) = (.seconds(2), .seconds(120))

    /// How often a running turn's transcript is re-read, to show its tool calls as they happen.
    public var progressInterval: Duration = .seconds(2.5)

    @ObservationIgnored private var sendTask: Task<Void, Never>?

    /// How often the open conversation is read again while nothing of the app's is in flight
    /// (RefreshSchedule): the fastest right after a change, backing off to the slowest.
    public var refreshInterval: (fastest: Duration, slowest: Duration) = (.seconds(5), .seconds(30))
    /// The chat is on screen in a window someone can see (set by the view).
    @ObservationIgnored public var isVisible = false
    /// The open agent runs (from the fleet, observe(_:)).
    @ObservationIgnored public private(set) var agentRunning = false
    /// The agent did something since the last read (from the fleet, observe(_:)).
    @ObservationIgnored private(set) var nudged = false
    @ObservationIgnored private var activityMark: String?
    @ObservationIgnored private var refreshTask: Task<Void, Never>?

    public init(backend: (any ChatBackend)? = nil) {
        self.backend = backend
    }

    /// The instance the conversations belong to: another one starts the store over.
    public private(set) var scope: String?

    public func use(_ backend: (any ChatBackend)?, scope: String?) {
        self.backend = backend
        guard scope != self.scope else { return }
        self.scope = scope
        cancelSend()
        agent = nil
        conversations = []
        selection = nil
        transcript = nil
        problem = nil
    }

    public var isSending: Bool { pending != nil }

    public var selected: Conversation? { conversations.first { $0.id == selection } }

    /// The open conversation takes messages: a new one, or one started through the API.
    public var canSend: Bool { selection == nil || (selected?.isDirect ?? true) }

    /// The pending turn, when it belongs to what is on screen.
    public var visiblePending: PendingTurn? {
        guard let p = pending, p.agent == agent, p.conversation == selection else { return nil }
        return p
    }

    public func selectAgent(_ id: String?) async {
        guard id != agent else { return }
        agent = id
        conversations = []
        selection = nil
        transcript = nil
        problem = nil
        await refreshList()
        if let first = conversations.first(where: \.isDirect) ?? conversations.first {
            await open(first.id)
        }
    }

    /// Reads the agent's conversations; a quiet read (the background refresh) shows no progress
    /// and keeps a failure to itself.
    public func refreshList(quiet: Bool = false) async {
        guard let backend, let agent else { return }
        if !quiet { loadingList = true }
        defer { if !quiet { loadingList = false } }
        do {
            let list = try await backend.conversations(agent)
            guard agent == self.agent else { return }
            if list != conversations { conversations = list }
            if !quiet { problem = nil }
        } catch {
            guard agent == self.agent, !quiet else { return }
            problem = ChatProblem(error)
        }
    }

    /// Opens a conversation (nil: a new one).
    public func open(_ id: String?) async {
        selection = id
        transcript = nil
        guard let id else { return }
        loadingTranscript = true
        defer { if selection == id { loadingTranscript = false } }
        await reloadTranscript(id)
    }

    /// Reads the open conversation again; true when what is shown changed. Unchanged rows are
    /// left as they are, so nothing on screen is drawn again. A quiet read (the background
    /// refresh) keeps a failure to itself, but for a conversation cleared elsewhere.
    @discardableResult
    private func reloadTranscript(_ id: String, quiet: Bool = false) async -> Bool {
        guard let backend, let agent else { return false }
        do {
            let t = try await backend.transcript(agent, id)
            guard agent == self.agent, selection == id else { return false }
            if problem?.kind == .starting { problem = nil }
            return apply(t)
        } catch {
            guard agent == self.agent, selection == id else { return false }
            let p = ChatProblem(error)
            if p.kind == .notFound {
                // Cleared elsewhere.
                conversations.removeAll { $0.id == id }
                selection = nil
                transcript = nil
                return true
            } else if !quiet {
                problem = p
            }
            return false
        }
    }

    /// Shows a transcript read again, unless it has the rows already shown.
    func apply(_ t: Transcript) -> Bool {
        if let shown = transcript, shown.id == t.id, shown.sameRows(as: t) { return false }
        transcript = t
        return true
    }

    // MARK: Refresh

    /// Keeps the open conversation current while the chat is on screen: a follow-up the agent set
    /// itself, or a turn from elsewhere, is appended later. Reads only while the window is visible,
    /// the agent runs and no turn of the app's is in flight (that one reads its own progress), less
    /// often while nothing changes; a change also refreshes the list. Runs until stopRefreshing().
    public func startRefreshing() {
        guard refreshTask == nil else { return }
        let (fastest, slowest) = refreshInterval
        let slice = min(fastest, .seconds(1))
        refreshTask = Task { [weak self] in
            var schedule = RefreshSchedule(fastest: fastest, slowest: slowest)
            var last = ContinuousClock.now
            var watched: String?
            var wasSending = false
            while !Task.isCancelled {
                try? await Task.sleep(for: slice)
                guard !Task.isCancelled, let self else { return }
                let key = self.agent.map { "\($0)/\(self.selection ?? "")" }
                if key != watched {
                    // Another conversation: it was just read when opened.
                    watched = key
                    schedule.reset()
                    last = .now
                    continue
                }
                if self.isSending {
                    wasSending = true
                    continue
                }
                if wasSending {
                    // The turn just ended and read the conversation; it may have set a follow-up.
                    wasSending = false
                    schedule.reset()
                    last = .now
                    continue
                }
                guard schedule.isDue(since: .now - last, nudged: self.nudged), let id = self.selection else { continue }
                self.nudged = false
                guard self.canRefresh else { continue }
                last = .now
                let changed = await self.reloadTranscript(id, quiet: true)
                last = .now
                schedule.record(changed: changed)
                if changed { await self.refreshList(quiet: true) }
            }
        }
    }

    public func stopRefreshing() {
        refreshTask?.cancel()
        refreshTask = nil
    }

    /// Whether a background read may run now.
    var canRefresh: Bool {
        guard isVisible, agentRunning, backend != nil, agent != nil, selection != nil, !loadingTranscript, !isSending else { return false }
        return problem?.kind != .notRunning && problem?.kind != .starting
    }

    /// What the fleet (polled anyway) says about the open agent: whether it runs, and whether it did
    /// something since it was last seen (its office timeline moved, or its last activity), which
    /// makes a read due sooner. A hint only: the schedule reads the conversation without it.
    public func observe(_ fleetAgent: FleetAgent?) {
        guard let a = fleetAgent, a.id == agent else {
            agentRunning = false
            activityMark = nil
            return
        }
        agentRunning = a.state == "running"
        let mark = "\(a.id)|\(a.office?.seq ?? -1)|\(a.office?.working ?? false)|\(a.activity?.lastActive ?? "")"
        if let old = activityMark, old != mark { nudged = true }
        activityMark = mark
    }

    /// Sends a message in the open conversation, starting one when none is open.
    public func send(_ text: String) {
        let text = text.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !text.isEmpty, !isSending, canSend, let backend, let agent else { return }
        problem = nil
        pending = PendingTurn(agent: agent, conversation: selection, text: text, started: .now)
        let interval = progressInterval
        sendTask = Task { [weak self] in
            var conversation = self?.selection
            var progress: Task<Void, Never>?
            defer { progress?.cancel() }
            do {
                if conversation == nil {
                    let made = try await backend.newConversation(agent, title: Self.title(for: text))
                    conversation = made.id
                    guard let self else { return }
                    let row = Conversation(id: made.id, title: Self.title(for: text), startedAt: Date.now.ISO8601Format())
                    if self.agent == agent {
                        self.conversations.insert(row, at: 0)
                        if self.selection == nil {
                            self.selection = made.id
                            self.transcript = made
                        }
                    }
                    self.pending?.conversation = made.id
                }
                guard let id = conversation else { return }
                progress = Task { [weak self] in
                    while !Task.isCancelled {
                        try? await Task.sleep(for: interval)
                        guard !Task.isCancelled, let self, self.agent == agent, self.selection == id else { continue }
                        await self.reloadTranscript(id)
                    }
                }
                _ = try await backend.send(agent, session: id, message: text)
                progress?.cancel()
                guard let self else { return }
                self.pending = nil
                if self.agent == agent {
                    if self.selection == id { await self.reloadTranscript(id) }
                    await self.refreshList()
                }
            } catch {
                guard let self else { return }
                self.pending = nil
                if !(error is CancellationError) { self.problem = ChatProblem(error) }
                if self.agent == agent, let id = conversation, self.selection == id { await self.reloadTranscript(id) }
            }
        }
    }

    /// Tries the agent again until it answers (which clears the problem) or the time runs out.
    private func retryWhileStarting() {
        guard retryTask == nil else { return }
        let (every, limit) = startingRetry
        retryTask = Task { [weak self] in
            let end = ContinuousClock.now + limit
            while ContinuousClock.now < end {
                try? await Task.sleep(for: every)
                guard !Task.isCancelled, let self, self.problem?.kind == .starting else { break }
                await self.refreshList()
                if let id = self.selection, self.problem == nil { await self.reloadTranscript(id) }
            }
            guard let self else { return }
            self.retryTask = nil
            if !Task.isCancelled, self.problem?.kind == .starting {
                self.problem = ChatProblem(kind: .other, message: "The agent did not come up. Its logs say why (stormo logs).")
            }
        }
    }

    /// Stops waiting for the turn in flight (ends the `stormo chat` process).
    public func cancelSend() {
        sendTask?.cancel()
        sendTask = nil
        pending = nil
    }

    public func clear(_ id: String) async {
        guard let backend, let agent else { return }
        do {
            _ = try await backend.clearConversation(agent, id)
        } catch {
            let p = ChatProblem(error)
            if p.kind != .notFound {
                problem = p
                return
            }
        }
        guard agent == self.agent else { return }
        conversations.removeAll { $0.id == id }
        if selection == id {
            selection = nil
            transcript = nil
        }
    }

    /// Clears every direct conversation of the agent.
    public func clearAll() async {
        guard let backend, let agent else { return }
        do {
            _ = try await backend.clearConversations(agent)
        } catch {
            problem = ChatProblem(error)
        }
        guard agent == self.agent else { return }
        if let s = selected, s.isDirect {
            selection = nil
            transcript = nil
        }
        await refreshList()
    }

    /// A new conversation's title: its first line, shortened.
    nonisolated static func title(for text: String) -> String {
        let line = text.split(whereSeparator: \.isNewline).first.map(String.init) ?? text
        return line.count > 60 ? String(line.prefix(57)) + "…" : line
    }
}
