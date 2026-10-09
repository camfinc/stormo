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
    public enum Kind: Sendable { case notRunning, busy, notFound, other }
    public var kind: Kind
    public var message: String

    public init(_ error: any Error) {
        let code = (error as? CLIError)?.code
        kind = switch code {
        case "not_running": .notRunning
        case "busy": .busy
        case "not_found": .notFound
        default: .other
        }
        message = switch kind {
        case .notRunning: "The agent is not running here. Start it to chat."
        case .busy: "The agent is busy with other turns. Try again in a moment."
        default: error.localizedDescription
        }
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
    public var problem: ChatProblem?

    /// How often a running turn's transcript is re-read, to show its tool calls as they happen.
    public var progressInterval: Duration = .seconds(2.5)

    @ObservationIgnored private var sendTask: Task<Void, Never>?

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

    public func refreshList() async {
        guard let backend, let agent else { return }
        loadingList = true
        defer { loadingList = false }
        do {
            let list = try await backend.conversations(agent)
            guard agent == self.agent else { return }
            conversations = list
            problem = nil
        } catch {
            guard agent == self.agent else { return }
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

    private func reloadTranscript(_ id: String) async {
        guard let backend, let agent else { return }
        do {
            let t = try await backend.transcript(agent, id)
            guard agent == self.agent, selection == id else { return }
            transcript = t
        } catch {
            guard agent == self.agent, selection == id else { return }
            let p = ChatProblem(error)
            if p.kind == .notFound {
                // Cleared elsewhere.
                conversations.removeAll { $0.id == id }
                selection = nil
            } else {
                problem = p
            }
        }
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
