import Foundation

/// A capability the core brokers (`GET /api/capabilities`).
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
    public var id: String { name }
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

/// A job in the owner's list: a scrubbed preview, never its input or output.
public struct CapabilityJob: Codable, Sendable, Equatable, Identifiable {
    public var job: Int64
    public var capability: String
    public var agent: String
    public var status: String
    public var error: String?
    public var preview: String?
    public var costUSD: Double
    public var created: Int64
    public var finished: Int64?
    public var id: Int64 { job }
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
}
