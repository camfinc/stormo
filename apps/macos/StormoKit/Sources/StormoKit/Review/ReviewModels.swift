import Foundation

/// `GET /api/review` (owner token): what the dream proposed for each agent.
public struct ReviewView: Codable, Sendable, Equatable {
    /// `core.learning.review`: "auto" (a model approves what is obviously safe, passes the rest on,
    /// never rejects) or "manual"; nil from a core before the automatic review.
    public var review: String?
    public var reviewConnection: String?
    public var reviewModel: String?
    /// The instance's own review rules file, appended to the built-in ones.
    public var reviewPrompt: String?
    /// The review is "auto" (what this field meant before: auto-accept was on).
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
    /// The schedules it changed at runtime, as agent.yaml would hold them (nil when none).
    public var schedules: ReviewSchedules?
    /// Settings it changed at runtime (home channel, approved commands, MCP servers).
    public var settings: [ReviewSetting]?
    /// The automatic review covers it (agent.yaml learning.auto_review).
    public var autoReview: Bool?
    /// Decisions since it last restarted through the core: restart to apply them.
    public var unapplied: Bool

    public var proposedLessons: [ReviewLesson] { lessons.filter { $0.status == "proposed" } }
    public var acceptedLessons: [ReviewLesson] { lessons.filter { $0.status == "accepted" } }
    public var runtimeSettings: [ReviewSetting] { settings ?? [] }
    /// Proposed lessons, skill, schedules and settings proposals.
    public var pending: Int { proposedLessons.count + skills.count + (schedules == nil ? 0 : 1) + runtimeSettings.count }
}

/// What the automatic review said about an open proposal's current content.
public struct ReviewAI: Codable, Sendable, Equatable {
    /// Always false on a proposal still open: an approval is applied at once.
    public var approve: Bool
    public var reason: String
    public var model: String
    public var at: String
}

/// The agent's runtime schedules: accept writes them to agent.yaml, reject drops `runtimeJobs`.
public struct ReviewSchedules: Codable, Sendable, Equatable {
    public var jobs: [ReviewJob]
    public var runtimeJobs: [String]
    /// Scripts the agent wrote or edited for these jobs; an accept puts them in the repo with them
    /// (nil from a core that predates them).
    public var scripts: [ReviewScript]?
    public var pii: [String]
    public var nap: String
    public var ai: ReviewAI?
    public var held: String?
}

/// One scheduled job as agent.yaml's schedules: holds it.
public struct ReviewJob: Codable, Sendable, Equatable, Identifiable {
    public var id: String
    public var name: String?
    public var every: String?
    public var cron: String?
    public var prompt: String?
    public var script: String?
    public var monitor: String?
    public var deliver: String?
    public var enabled: Bool
    public var when: String { every ?? cron ?? "" }
}

/// A script in a schedules proposal.
public struct ReviewScript: Codable, Sendable, Equatable, Identifiable {
    public var name: String
    /// "new", or "changed" (the repo has another version, in `current`).
    public var status: String
    public var size: Int64
    public var text: String?
    public var current: String?
    public var id: String { name }
}

/// A setting the agent changed at runtime.
public struct ReviewSetting: Codable, Sendable, Equatable, Identifiable {
    public var key: String
    public var value: String
    /// The repo's value (nil when the repo sets none).
    public var current: String?
    /// Widens what the agent may do: the rules always leave it to a person.
    public var sensitive: Bool?
    public var pii: [String]
    public var nap: String
    public var ai: ReviewAI?
    public var held: String?
    public var id: String { key }
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
    /// What the review's rules let its model judge (agent-scoped memory, no PII flag).
    public var autoAcceptable: Bool
    public var ai: ReviewAI?
    /// Why the rules hold it for a person.
    public var held: String?
}

/// A skill the dream proposed: new, or a change to one the agent has.
public struct ReviewSkill: Codable, Sendable, Equatable, Identifiable {
    public var skill: String
    public var pii: [String]
    public var nap: String
    public var new: Bool
    /// The agent changed an older version than the repo's (built from commit `base`): the diff shows
    /// the repo's newer edits as removals, and the core refuses to accept it.
    public var stale: Bool?
    public var base: String?
    public var files: [ReviewFile]
    public var ai: ReviewAI?
    public var held: String?
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
    public var schedules: Bool?
    public var settings: [String]?
    public var decision: Verdict
    /// promote: "unit" or "group".
    public var scope: String?

    public static func lessons(_ ids: [String], of agent: String, _ decision: Verdict, scope: String? = nil) -> Self {
        Self(agent: agent, lessons: ids, decision: decision, scope: scope)
    }

    /// What the decision is about (a skill, the schedules, settings, or lessons), whatever the verdict.
    public var key: String {
        if let skill { return agent + "/skill:" + skill }
        if schedules == true { return agent + "/schedules" }
        if let settings { return agent + "/settings:" + settings.joined(separator: ",") }
        return agent + "/lessons:" + (lessons ?? []).joined(separator: ",")
    }

    public static func skill(_ name: String, of agent: String, _ decision: Verdict) -> Self {
        Self(agent: agent, skill: name, decision: decision)
    }

    /// Accept writes the runtime schedules to agent.yaml; reject drops the jobs created at runtime.
    public static func schedules(of agent: String, _ decision: Verdict) -> Self {
        Self(agent: agent, schedules: true, decision: decision)
    }

    public static func settings(_ keys: [String], of agent: String, _ decision: Verdict) -> Self {
        Self(agent: agent, settings: keys, decision: decision)
    }
}
