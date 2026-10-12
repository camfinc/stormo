import Foundation

/// `GET /api/review` (owner token): what the dream proposed for each agent.
public struct ReviewView: Codable, Sendable, Equatable {
    public var autoAccept: Bool
    public var autoAcceptMinSeen: Int
    public var autoRestart: Bool
    public var agents: [ReviewAgent]
}

/// One agent's proposals and accepted lessons.
public struct ReviewAgent: Codable, Sendable, Equatable, Identifiable {
    public var id: String
    public var name: String
    public var unit: String
    public var state: String
    public var busy: Bool
    /// Proposed first, then accepted (which can be promoted).
    public var lessons: [ReviewLesson]
    public var skills: [ReviewSkill]
    /// Decisions since it last restarted through the core: restart to apply them.
    public var unapplied: Bool

    public var proposedLessons: [ReviewLesson] { lessons.filter { $0.status == "proposed" } }
    public var acceptedLessons: [ReviewLesson] { lessons.filter { $0.status == "accepted" } }
    /// Proposed lessons plus skill proposals.
    public var pending: Int { proposedLessons.count + skills.count }
}

/// One ledger entry: scrubbed text, never raw session content.
public struct ReviewLesson: Codable, Sendable, Equatable, Identifiable {
    public var id: String
    public var kind: String
    public var scope: String
    public var text: String
    public var pii: [String]
    public var status: String
    public var seenCount: Int
    public var firstSeen: String
    public var lastSeen: String
    public var decidedAt: String?
    public var note: String?
    /// What auto_accept would take (agent-scoped memory, no PII flag).
    public var autoAcceptable: Bool
}

/// A skill the dream proposed: new, or a change to one the agent has.
public struct ReviewSkill: Codable, Sendable, Equatable, Identifiable {
    public var skill: String
    public var pii: [String]
    public var nap: String
    public var new: Bool
    public var files: [ReviewFile]
    public var id: String { skill }
}

/// A proposal's file, with its text when small and textual.
public struct ReviewFile: Codable, Sendable, Equatable, Identifiable {
    public var path: String
    public var size: Int64
    public var text: String?
    /// The current version when the skill exists and this file differs ("" when it is new to the skill).
    public var current: String?
    public var id: String { path }
}

/// `POST /api/review`'s body.
public struct ReviewDecision: Codable, Sendable, Equatable {
    public enum Verdict: String, Codable, Sendable { case accept, reject, promote }
    public var agent: String
    public var lessons: [String]?
    public var skill: String?
    public var decision: Verdict
    /// promote: "unit" or "group".
    public var scope: String?

    public static func lessons(_ ids: [String], of agent: String, _ decision: Verdict, scope: String? = nil) -> Self {
        Self(agent: agent, lessons: ids, skill: nil, decision: decision, scope: scope)
    }

    public static func skill(_ name: String, of agent: String, _ decision: Verdict) -> Self {
        Self(agent: agent, lessons: nil, skill: name, decision: decision, scope: nil)
    }
}
