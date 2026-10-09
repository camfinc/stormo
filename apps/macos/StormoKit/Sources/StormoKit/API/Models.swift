import Foundation

// The core's HTTP API (docs/api.md). Decoding is lenient where the engine may add or omit fields:
// optional fields stay optional, so a newer core still decodes.

/// The API version this app speaks; a core or CLI outside this range is refused.
public enum StormoAPI {
    public static let supported: ClosedRange<Int> = 1...1
}

/// `GET /health`.
public struct Health: Codable, Sendable, Equatable {
    public var status: String
    public var service: String
    public var login: String
    public var planLimitedUntil: String?
    /// Absent on cores older than API 1.
    public var version: String?
    public var api: Int?
}

/// `GET /api/core`: which binary and which instance a running core is.
public struct CoreInfo: Codable, Sendable, Equatable {
    public struct Instance: Codable, Sendable, Equatable {
        public var root: String
        public var name: String
        public var org: String
        public var slug: String
    }
    public var service: String
    public var version: String
    public var api: Int
    public var pid: Int
    public var exe: String
    public var port: Int
    public var startedAt: String
    public var instance: Instance
}

/// `GET /api/instance`: the instance's look.
public struct InstanceLook: Codable, Sendable, Equatable {
    public struct Clock: Codable, Sendable, Equatable {
        public var city: String
        public var tz: String
    }
    public struct Unit: Codable, Sendable, Equatable {
        public var hue: String?
        public var wall: String?
        public var floor: String?
        public var desk: Desk?
    }
    public var name: String
    public var org: String
    public var slug: String
    public var clocks: [Clock]
    public var units: [String: Unit]
}

public struct Desk: Codable, Sendable, Equatable {
    public var props: [String]?
    public var app: String?
    public var apps: [String]?
    public var screens: Int?
    public var side: String?
}

public struct Sprite: Codable, Sendable, Equatable {
    public var skin: String?
    public var hair: String?
    public var shirt: String?
    public var pants: String?
    public var hairStyle: String?
    public var accessory: String?

    enum CodingKeys: String, CodingKey {
        case skin, hair, shirt, pants, accessory
        case hairStyle = "hair_style"
    }
}

/// What the agent's engine reports while it runs (null when it is down or did not answer).
public struct Activity: Codable, Sendable, Equatable {
    public struct Platform: Codable, Sendable, Equatable {
        public var state: String
        public var needsAttention: Bool
    }
    public var activeAgents: Int
    public var gatewayBusy: Bool
    public var source: String?
    public var runningJobs: [String]?
    public var lastJobAt: String?
    public var lastActive: String?
    public var platforms: [String: Platform]?
    public var polledAt: String?
}

/// The office's timeline for one agent (docs/core.md §3). Times are epoch milliseconds.
public struct OfficeState: Codable, Sendable, Equatable {
    public var activity: String
    public var phase: String
    public var start: Int64
    public var end: Int64?
    public var from: String?
    public var slot: Int
    public var label: String
    public var working: Bool
    public var seq: Int
}

public struct FleetUnit: Codable, Sendable, Equatable, Identifiable {
    public var id: String
    public var name: String
    public var description: String?
}

public struct FleetAgent: Codable, Sendable, Equatable, Identifiable {
    public var id: String
    public var name: String
    public var unit: String
    public var role: String?
    public var model: String?
    public var localModel: String?
    public var channels: [String]?
    public var optionalChannels: [String]?
    public var avatar: Bool?
    public var desk: Desk?
    public var sprite: Sprite?
    public var activity: Activity?
    /// running | starting | stopped | exited | restarting | unknown
    public var state: String
    public var health: String?
    public var detail: String?
    public var endpoint: String?
    public var lastNap: String?
    public var napIntervalSeconds: Int?
    public var pendingLearnings: Int?
    public var pendingSkills: Int?
    public var office: OfficeState?
}

public struct RobotNote: Codable, Sendable, Equatable {
    public struct Changed: Codable, Sendable, Equatable {
        public var learning: Int
        public var state: Int
        public var raw: Int
    }
    public var agent: String
    public var takenAt: String?
    public var reason: String?
    public var changed: Changed?
    public var removed: Int?
    public var naps: Int?
    public var at: Int64?
}

public struct RobotState: Codable, Sendable, Equatable {
    public var phase: String
    public var from: String?
    public var target: String?
    public var start: Int64?
    public var end: Int64?
    public var seq: Int?
    public var note: RobotNote?
    public var log: [RobotNote]?
    public var queue: [String]?
}

public struct AgentUsage: Codable, Sendable, Equatable {
    public var requests: Int
    public var ok: Int
    public var errors: Int
    public var rateLimited: Int
    public var inputTokens: Int
    public var cachedTokens: Int
    public var outputTokens: Int
    public var lastAt: String?
    public var lastStatus: Int?
}

/// The gateway as `/api/fleet` carries it (no account).
public struct GatewaySummary: Codable, Sendable, Equatable {
    public var login: String
    public var planLimitedUntil: String?
    public var manageUsageUrl: String?
    public var inflight: Int
    public var queued: Int
    public var concurrency: Int
    public var usage: [String: AgentUsage]?
    public var active: [String: Int]?
}

/// `GET /api/fleet`.
public struct Fleet: Codable, Sendable, Equatable {
    public var polledAt: String?
    public var units: [FleetUnit]
    public var agents: [FleetAgent]
    /// The core's clock, epoch milliseconds.
    public var now: Int64
    public var robot: RobotState?
    public var gateway: GatewaySummary
}

/// `GET /api/gateway`.
public struct GatewayStatus: Codable, Sendable, Equatable {
    public struct Model: Codable, Sendable, Equatable {
        public var id: String
        public var displayName: String?
        public var contextLength: Int?

        enum CodingKeys: String, CodingKey {
            case id
            case displayName = "display_name"
            case contextLength = "context_length"
        }
    }
    public var login: String
    public var account: String?
    public var manageUsageUrl: String?
    public var planLimitedUntil: String?
    public var inflight: Int
    public var queued: Int
    public var concurrency: Int
    public var models: [Model]?
    public var usage: [String: AgentUsage]?
    public var active: [String: Int]?
}
