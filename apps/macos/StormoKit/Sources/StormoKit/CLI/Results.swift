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
    public var account: String?
    public var changed: Bool
}
