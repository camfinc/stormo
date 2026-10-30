import Foundation

/// A capability service's worker as the core sees it now.
public struct CapabilityWorker: Codable, Sendable, Equatable {
    /// asleep | booting | awake
    public var state: String
    public var since: Int64?
    public var lastUsed: Int64?
    public var running: Int?
    /// Why its last boot failed.
    public var error: String?
}

/// A capability the core brokers (`GET /api/capabilities`). Fields after `agents` are absent from
/// an older core.
public struct CapabilityInfo: Codable, Sendable, Equatable, Identifiable {
    public var name: String
    public var description: String
    public var kind: String
    public var placement: String?
    public var concurrency: Int
    public var costUSD: Double
    public var queued: Int
    public var running: Int
    public var agents: [String]
    public var template: String?
    public var image: String?
    /// none | in | out | both
    public var files: String?
    public var timeoutMinutes: Int?
    public var idleMinutes: Int?
    public var bootMinutes: Int?
    public var memoryMB: Int?
    public var shmMB: Int?
    public var env: [String: String]?
    public var docs: String?
    /// Why it cannot run on this engine (a template it does not ship).
    public var unavailable: String?
    /// asleep | booting | awake (a service on the core's host), runner, or nil (a worker per job).
    public var worker: String?
    public var service: CapabilityWorker?
    public var id: String { name }

    /// The worker can be woken or put to sleep from here: a service on the core's host.
    public var isLocalService: Bool { kind == "service" && (placement ?? "local") == "local" && unavailable == nil }
}

/// What an agent's jobs of a capability cost this month.
public struct CapabilitySpend: Codable, Sendable, Equatable, Identifiable {
    public var agent: String
    public var capability: String
    public var jobs: Int
    public var failed: Int
    public var usd: Double
    public var id: String { agent + "/" + capability }
}

public struct CapabilitiesView: Codable, Sendable, Equatable {
    public var capabilities: [CapabilityInfo]
    public var spend: [CapabilitySpend]
}

/// A job as the owner sees it: in the list a scrubbed preview; one job (`capabilityJob`) also its
/// output, scrubbed and cut at 64 KB.
public struct CapabilityJob: Codable, Sendable, Equatable, Identifiable {
    public var job: Int64
    public var capability: String
    public var agent: String
    /// waiting | queued | running | done | failed
    public var status: String
    public var error: String?
    public var preview: String?
    public var costUSD: Double
    public var created: Int64
    public var started: Int64?
    public var finished: Int64?
    public var inputs: [String]?
    public var uploads: [String]?
    public var out: String?
    public var files: [String]?
    public var notify: Bool?
    public var note: String?
    public var output: JSONValue?
    public var id: Int64 { job }

    /// Not over yet: the owner may cancel it.
    public var isCancellable: Bool { status == "waiting" || status == "queued" || status == "running" }
}

extension CoreClient {
    public func capabilities() async throws -> CapabilitiesView {
        let (data, status) = try await raw(URLRequest(url: base.appending(path: "api/capabilities")))
        guard status == 200 else { throw status == 404 ? Failure.notFound : Failure.status(status) }
        return try JSONDecoder().decode(CapabilitiesView.self, from: data)
    }

    public func capabilityJobs(limit: Int = 50) async throws -> [CapabilityJob] {
        let url = base.appending(path: "api/capabilities/jobs").appending(queryItems: [URLQueryItem(name: "limit", value: String(limit))])
        let (data, status) = try await raw(URLRequest(url: url))
        guard status == 200 else { throw status == 404 ? Failure.notFound : Failure.status(status) }
        struct Body: Decodable { let jobs: [CapabilityJob] }
        return try JSONDecoder().decode(Body.self, from: data).jobs
    }

    /// One job with its output (scrubbed). The owner's token.
    public func capabilityJob(_ id: Int64) async throws -> CapabilityJob {
        try JSONDecoder().decode(CapabilityJob.self, from: try await sendAuthorized(URLRequest(url: base.appending(path: "api/capabilities/jobs/\(id)"))))
    }

    /// Cancels any job: a waiting or queued one never runs, a running one stops. The owner's token.
    public func cancelCapabilityJob(_ id: Int64) async throws -> CapabilityJob {
        var r = URLRequest(url: base.appending(path: "api/capabilities/jobs/\(id)/cancel"))
        r.httpMethod = "POST"
        return try JSONDecoder().decode(CapabilityJob.self, from: try await sendAuthorized(r))
    }

    /// Boots a service capability's worker now (in the background) or puts it to sleep (refused while
    /// jobs run in it). The owner's token.
    public func setCapabilityAwake(_ name: String, _ awake: Bool) async throws -> CapabilityWorker {
        var r = URLRequest(url: base.appending(path: "api/capabilities/\(name)/\(awake ? "wake" : "sleep")"))
        r.httpMethod = "POST"
        struct Body: Decodable { let service: CapabilityWorker }
        return try JSONDecoder().decode(Body.self, from: try await sendAuthorized(r)).service
    }
}

// MARK: Declaring and granting (stormo.yaml, agent.yaml), through the CLI

/// A template the engine bundles (`capability templates`).
public struct CapabilityTemplate: Codable, Sendable, Equatable, Identifiable {
    public var name: String
    public var description: String
    public var docs: String?
    public var files: String?
    public var notes: String?
    public var concurrency: Int?
    public var timeoutMinutes: Int?
    public var idleMinutes: Int?
    public var memoryMB: Int?
    public var env: [String: String]?
    public var id: String { name }

    /// A name to offer for a capability made from this template.
    public var suggestedName: String {
        switch name {
        case "transcribe": "audio.transcribe"
        case "browser": "web.browse"
        case "video": "video.render"
        case "media": "media.tools"
        case "hyperframes": "motion.frames"
        default: name
        }
    }
}

/// One capability as stormo.yaml says it (`spec`) and as it runs (`effective`), with its grants.
public struct DeclaredCapability: Codable, Sendable, Equatable, Identifiable {
    public var name: String
    public var spec: [String: JSONValue]
    public var effective: CapabilityInfo.Settings
    public var agents: [String]
    public var id: String { name }
}

extension CapabilityInfo {
    /// The settings of a capability that a core loads (what `capability show` calls effective).
    public struct Settings: Codable, Sendable, Equatable {
        public var name: String
        public var description: String
        public var kind: String
        public var template: String?
        public var image: String?
        public var placement: String?
        public var files: String?
        public var concurrency: Int
        public var timeoutMinutes: Int
        public var idleMinutes: Int?
        public var bootMinutes: Int?
        public var memoryMB: Int?
        public var shmMB: Int?
        public var cpus: Double?
        public var costUSD: Double
        public var env: [String: String]?
        public var docs: String?
        public var unavailable: String?
    }

    /// What to patch env: with to make it `new`. A merge patch merges maps, so a name taken out is
    /// sent as null and an empty env removes the setting; nil when nothing changed.
    public static func envPatch(from old: [String: String]?, to new: [String: String]) -> JSONValue? {
        let old = old ?? [:]
        guard new != old else { return nil }
        if new.isEmpty { return .null }
        var p: [String: JSONValue] = [:]
        for k in old.keys where new[k] == nil { p[k] = .null }
        for (k, v) in new where old[k] != v { p[k] = .string(v) }
        return .object(p)
    }

    /// What of this capability a restart would change, compared with stormo.yaml's version.
    func differs(from s: Settings) -> Bool {
        description != s.description || kind != s.kind || (template ?? "") != (s.template ?? "") || (image ?? "") != (s.image ?? "")
            || concurrency != s.concurrency || (timeoutMinutes ?? s.timeoutMinutes) != s.timeoutMinutes
            || (idleMinutes ?? 0) != (s.idleMinutes ?? 0) || (memoryMB ?? 0) != (s.memoryMB ?? 0) || (shmMB ?? 0) != (s.shmMB ?? 0)
            || costUSD != s.costUSD || (env ?? [:]) != (s.env ?? [:]) || (files ?? "none") != (s.files ?? "none")
    }
}

/// `capability show`: stormo.yaml's capabilities and the file's hash (for edits).
public struct CapabilitiesFile: Codable, Sendable, Equatable {
    public var path: String
    public var hash: String
    public var capabilities: [DeclaredCapability]

    /// Capabilities whose stormo.yaml version the running core has not loaded (added, changed or
    /// removed since it started): a core restart applies them.
    public func pendingRestart(core: CapabilitiesView) -> [String] {
        var out: [String] = []
        for d in capabilities {
            guard let c = core.capabilities.first(where: { $0.name == d.name }) else { out.append(d.name); continue }
            if c.differs(from: d.effective) { out.append(d.name) }
        }
        for c in core.capabilities where !capabilities.contains(where: { $0.name == c.name }) { out.append(c.name) }
        return out.sorted()
    }
}

extension StormoCLI {
    public func capabilityTemplates() async throws -> [CapabilityTemplate] {
        struct Body: Decodable { let templates: [CapabilityTemplate] }
        return try await run(["capability", "templates"], as: Body.self).templates
    }

    public func capabilitiesFile() async throws -> CapabilitiesFile {
        try await run(["capability", "show"], as: CapabilitiesFile.self)
    }

    /// Declares or changes a capability by a merge patch on its stanza (`null` removes a setting).
    public func setCapability(_ name: String, patch: [String: JSONValue], ifHash: String) async throws -> CapabilitiesFile {
        try await run(["capability", "set", name, "--if-hash", ifHash], as: CapabilitiesFile.self, stdin: try JSONEncoder().encode(patch))
    }

    public func removeCapability(_ name: String, ifHash: String) async throws -> CapabilitiesFile {
        try await run(["capability", "remove", name, "--if-hash", ifHash], as: CapabilitiesFile.self)
    }

    /// Grants a capability to an agent or takes it away: its agent.yaml capabilities: edited by a
    /// merge patch, comments kept, refused by the engine when the file changed meanwhile.
    public func setCapabilityGrant(_ capability: String, agent: String, granted: Bool) async throws {
        let path = "agents/\(agent)/agent.yaml"
        let f = try await run(["config", "show", path], as: ConfigFile.self)
        var list: [String] = []
        if case .object(let doc)? = f.doc, case .array(let items)? = doc["capabilities"] {
            list = items.compactMap(\.string)
        }
        let has = list.contains(capability)
        guard has != granted else { return }
        list = granted ? list + [capability] : list.filter { $0 != capability }
        let patch: [String: JSONValue] = ["capabilities": list.isEmpty ? .null : .array(list.map { .string($0) })]
        _ = try await run(["config", "apply", path, "--if-hash", f.hash], as: ConfigFile.self, stdin: try JSONEncoder().encode(patch))
    }
}
