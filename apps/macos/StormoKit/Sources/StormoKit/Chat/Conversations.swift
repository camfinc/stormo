import Foundation

// `stormo chat` and `stormo conversations` (docs/api.md). Titles, previews and messages are client
// data: the app keeps them in memory only.

/// One row of `conversations <agent>`.
public struct Conversation: Codable, Sendable, Equatable, Identifiable {
    public var id: String
    public var title: String?
    public var source: String?
    public var preview: String?
    public var startedAt: String?
    public var lastActive: String?
    public var messages: Int
    public var ended: Bool

    public init(id: String, title: String? = nil, source: String? = "api_server", preview: String? = nil,
                startedAt: String? = nil, lastActive: String? = nil, messages: Int = 0, ended: Bool = false) {
        self.id = id; self.title = title; self.source = source; self.preview = preview
        self.startedAt = startedAt; self.lastActive = lastActive; self.messages = messages; self.ended = ended
    }

    /// Started through the engine's API (stormo, this app): the ones the app can talk in. A Slack
    /// thread's or a schedule's conversation belongs to that channel.
    public var isDirect: Bool { source == nil || source == "api_server" }

    public var displayTitle: String {
        if let t = title?.trimmingCharacters(in: .whitespacesAndNewlines), !t.isEmpty { return t }
        if let p = preview?.trimmingCharacters(in: .whitespacesAndNewlines), !p.isEmpty { return p }
        return "New conversation"
    }

    public var sourceLabel: String {
        switch source {
        case nil, "api_server": "Direct"
        case "cron": "Scheduled"
        case let s?: s.prefix(1).uppercased() + s.dropFirst()
        }
    }

    public var activeDate: Date? { (lastActive ?? startedAt).flatMap(Date.init(chatTime:)) }
}

public struct ConversationsResult: Codable, Sendable, Equatable {
    public var agent: String
    public var conversations: [Conversation]
}

/// One visible transcript row.
public struct ChatMessage: Codable, Sendable, Equatable, Identifiable {
    public var id: String
    public var role: String
    public var kind: String
    public var content: String
    public var at: String?
    public var tools: [String]?
    public var tool: String?

    enum CodingKeys: String, CodingKey { case id, role, kind, content, at, tools, tool }

    public init(id: String, role: String, kind: String = "text", content: String, at: String? = nil, tools: [String]? = nil, tool: String? = nil) {
        self.id = id; self.role = role; self.kind = kind; self.content = content; self.at = at; self.tools = tools; self.tool = tool
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        role = try c.decode(String.self, forKey: .role)
        kind = try c.decodeIfPresent(String.self, forKey: .kind) ?? "text"
        content = try c.decodeIfPresent(String.self, forKey: .content) ?? ""
        at = try c.decodeIfPresent(String.self, forKey: .at)
        tools = try c.decodeIfPresent([String].self, forKey: .tools)
        tool = try c.decodeIfPresent(String.self, forKey: .tool)
        id = try c.decodeIfPresent(String.self, forKey: .id) ?? UUID().uuidString
    }

    public var isTool: Bool { kind == "tool" }
    public var isUser: Bool { role == "user" }
    public var date: Date? { at.flatMap(Date.init(chatTime:)) }
}

/// `conversations <agent> <session>` and `conversations new`.
public struct Transcript: Codable, Sendable, Equatable {
    public var agent: String
    public var id: String
    /// The session holding the conversation now: an automatic compaction moves it.
    public var tip: String
    /// Older turns were folded into a summary by the engine's automatic compaction.
    public var compacted: Bool
    public var messages: [ChatMessage]
}

/// `chat`.
public struct ChatReply: Codable, Sendable, Equatable {
    public var agent: String
    public var session: String
    public var reply: String
}

/// `conversations clear`.
public struct ClearResult: Codable, Sendable, Equatable {
    public var agent: String
    public var cleared: [String]
    public var removed: Int
}

extension StormoCLI {
    public func conversations(_ agent: String) async throws -> [Conversation] {
        try await run(["conversations", agent], as: ConversationsResult.self).conversations
    }

    public func transcript(_ agent: String, _ id: String) async throws -> Transcript {
        try await run(["conversations", agent, id], as: Transcript.self)
    }

    public func newConversation(_ agent: String, title: String? = nil) async throws -> Transcript {
        var command = ["conversations", "new", agent]
        if let title, !title.isEmpty { command += ["--name", title] }
        return try await run(command, as: Transcript.self)
    }

    /// One turn: the message goes on stdin (it could read as a flag). Cancelling ends the turn.
    public func send(_ agent: String, session: String, message: String) async throws -> ChatReply {
        try await run(["chat", agent, "-", "--session", session], as: ChatReply.self, stdin: Data(message.utf8))
    }

    public func clearConversation(_ agent: String, _ id: String) async throws -> ClearResult {
        try await run(["conversations", "clear", agent, id], as: ClearResult.self)
    }

    /// Clears the agent's direct conversations (a Slack thread's or a schedule's stay).
    public func clearConversations(_ agent: String) async throws -> ClearResult {
        try await run(["conversations", "clear", agent, "--all"], as: ClearResult.self)
    }
}

extension Date {
    /// RFC 3339 with or without fractional seconds.
    public init?(chatTime s: String) {
        if let d = try? Date(s, strategy: .iso8601) {
            self = d
        } else if let d = try? Date(s, strategy: .iso8601.year().month().day().time(includingFractionalSeconds: true)) {
            self = d
        } else {
            return nil
        }
    }
}
