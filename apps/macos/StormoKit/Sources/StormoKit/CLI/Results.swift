import Foundation

// `result.data` of the commands the app runs (docs/api.md).

/// `stormo version --json`.
public struct VersionInfo: Codable, Sendable, Equatable {
    public var version: String
    public var api: Int
    public var released: Bool
    public var exe: String

    public init(version: String, api: Int, released: Bool, exe: String) {
        self.version = version
        self.api = api
        self.released = released
        self.exe = exe
    }
}

/// `stormo instance --json`.
public struct InstanceInfo: Codable, Sendable, Equatable {
    public var root: String
    public var name: String
    public var org: String
    public var slug: String
}

/// One row of `stormo status --json`.
public struct AgentStatus: Codable, Sendable, Equatable, Identifiable {
    public var agent: String
    public var unit: String
    public var `where`: String
    public var state: String
    public var health: String?
    public var endpoint: String?
    public var lastNap: String?
    public var pendingLearnings: Int
    public var pendingSkills: Int
    public var detail: String?
    public var id: String { agent }
}

/// `start`, `stop`, `restart`, `nap-now`.
public struct LifecycleResult: Codable, Sendable, Equatable {
    public var action: String
    public var `where`: String
    public var agents: [String]
}

/// `core up`.
public struct CoreUpResult: Codable, Sendable, Equatable {
    public var started: Bool
    public var pid: Int?
    public var port: Int
    public var log: String?
    public var login: String
}

/// `core down`.
public struct CoreDownResult: Codable, Sendable, Equatable {
    public var stopped: Bool
    public var pid: Int?
    /// not_running | not_ours
    public var reason: String?
}

/// `core login` / `core logout`.
public struct LoginResult: Codable, Sendable, Equatable {
    public var login: String
    /// The ChatGPT connection (absent from an older stormo).
    public var connection: String?
    public var account: String?
    public var changed: Bool
}

/// `new instance`.
public struct NewInstanceResult: Codable, Sendable, Equatable {
    public var root: String
    public var name: String
    public var org: String
    public var slug: String
    /// empty | example
    public var template: String
    /// `git init` ran in it.
    public var git: Bool
}

/// One agent that passed `check`.
public struct CheckedAgent: Codable, Sendable, Equatable {
    public var agent: String
    public var unit: String
    public var engine: String
    public var files: Int
    public var skills: Int
    /// The agent's agent.yaml format and the format-0 keys it still uses (absent from an older stormo).
    public var format: Int?
    public var legacy: [String]?
}

/// `config show` / `config write`: an editable file and the hash of what was read or written.
public struct ConfigFile: Codable, Sendable, Equatable {
    public var path: String
    /// agent (agent.yaml) | soul (SOUL.md)
    public var kind: String
    public var agent: String
    public var hash: String
    public var text: String
    /// agent.yaml: the manifest as JSON (nil when it does not parse) and the form's choices.
    public var doc: JSONValue?
    public var options: AgentOptions?
}

/// What an agent form may offer, from the instance and the engine (`config show`).
public struct AgentOptions: Codable, Sendable, Equatable {
    public struct Unit: Codable, Sendable, Equatable, Identifiable {
        public var id: String
        public var name: String
        public var description: String?
    }
    public struct Action: Codable, Sendable, Equatable, Identifiable {
        public var name: String
        public var unit: String
        public var description: String
        public var mutates: Bool
        public var id: String { name }
    }
    public struct Channel: Codable, Sendable, Equatable, Identifiable {
        public var kind: String
        public var secrets: [String]
        public var id: String { kind }
    }
    public var units: [Unit]
    public var actions: [Action]
    public var skills: [String]
    public var personas: [String]
    public var engines: [String]
    public var channels: [Channel]
    public var allowBots: [String]
    public var secrets: [String]
    /// The agent's scripts/ (for schedules) and the reasoning levels its engine accepts; absent from
    /// an older stormo.
    public var scripts: [String]?
    public var reasoning: [String]?
    /// Connections the agent may use (absent from an older stormo).
    public var connections: [Connection]?

    public struct Connection: Codable, Sendable, Equatable, Identifiable {
        public var name: String
        public var kind: String
        public var label: String
        public var api: Bool
        /// The secret an agent using it declares.
        public var key: String?
        public var id: String { name }
    }
}

/// `connections kinds`: what a connection can be.
public struct ConnectionKindInfo: Codable, Sendable, Equatable, Identifiable {
    public var kind: String
    public var label: String
    public var baseUrl: String?
    public var key: String?
    public var api: Bool
    public var id: String { kind }
}

/// One row of `connections`.
public struct ConnectionRow: Codable, Sendable, Equatable, Identifiable {
    public var name: String
    public var kind: String
    public var label: String
    public var api: Bool
    public var baseUrl: String?
    public var key: String?
    /// Not written in stormo.yaml: one of the two every instance has.
    public var implicit: Bool?
    /// A shared value is set for the key; agentKeys have their own.
    public var keySet: Bool
    public var agentKeys: [String]
    public var usedBy: [String]
    public var id: String { name }
}

/// One agent `stormo migrate agent` went through.
public struct MigrateResult: Codable, Sendable, Equatable {
    public var agent: String
    public var from: Int
    public var to: Int
    public var changes: [String]
    /// moved | in-place | running (its data stays until it is stopped)
    public var data: String
}

/// `export <agent>`.
public struct ExportResult: Codable, Sendable, Equatable {
    public var path: String
    public var agent: String
    /// config | data
    public var mode: String
    public var files: Int
    public var naps: Int
    public var containsSecrets: Bool
}

/// `import <file.zip>`.
public struct ImportResult: Codable, Sendable, Equatable {
    public var agent: String
    /// The source instance's slug.
    public var from: String
    public var mode: String
    public var changes: [String]
}

/// One model of `connections models <name>`.
public struct ConnectionModel: Codable, Sendable, Equatable, Identifiable {
    public var id: String
    public var name: String?
    public var contextLength: Int?
}
