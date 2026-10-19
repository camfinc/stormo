import Foundation

/// A lifecycle call through the core (`POST /api/agents/<id>/start|stop|restart`), for instances
/// the app reaches only through their core.
public struct CoreLifecycleResult: Codable, Sendable, Equatable {
    public var action: String
    public var agent: String
    public var placement: String
    public var log: [String]
    public var state: String?
}

public enum LifecycleOutcome: Sendable, Equatable {
    case done(CoreLifecycleResult)
    /// A production change (ECS): nothing ran; the exact plan, to confirm.
    case needsConfirm(plan: [String])
}

/// A placement as `GET /api/placements` shows it.
public struct PlacementInfo: Codable, Sendable, Equatable, Identifiable {
    public var name: String
    public var kind: String
    public var available: Bool
    public var id: String { name }
}

extension CoreClient {
    public func lifecycle(agent: String, action: String, placement: String?, confirm: Bool) async throws -> LifecycleOutcome {
        var r = URLRequest(url: base.appending(path: "api/agents/\(agent)/\(action)"))
        r.httpMethod = "POST"
        r.timeoutInterval = 900
        r.setValue("application/json", forHTTPHeaderField: "Content-Type")
        var body: [String: Any] = ["confirm": confirm]
        if let placement { body["placement"] = placement }
        r.httpBody = try JSONSerialization.data(withJSONObject: body)
        let (data, status) = try await raw(r)
        switch status {
        case 200: return .done(try JSONDecoder().decode(CoreLifecycleResult.self, from: data))
        case 428:
            struct Plan: Decodable { let plan: [String] }
            return .needsConfirm(plan: (try? JSONDecoder().decode(Plan.self, from: data))?.plan ?? [])
        default:
            struct Body: Decodable { struct E: Decodable { let message: String }; let error: E }
            let message = (try? JSONDecoder().decode(Body.self, from: data))?.error.message ?? "HTTP \(status)"
            throw Failure.refused(status, message)
        }
    }

    public func placements() async throws -> [PlacementInfo] {
        let (data, status) = try await raw(URLRequest(url: base.appending(path: "api/placements")))
        guard status == 200 else { throw status == 404 ? Failure.notFound : Failure.status(status) }
        struct Body: Decodable { let placements: [PlacementInfo] }
        return try JSONDecoder().decode(Body.self, from: data).placements
    }

    /// One request with the client's token; any status comes back (lifecycle reads 428's body).
    func raw(_ request: URLRequest) async throws -> (Data, Int) {
        var request = request
        if let token { request.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization") }
        // Lifecycle actions outlast the client's short timeout: a session of their own.
        let config = URLSessionConfiguration.ephemeral
        config.timeoutIntervalForRequest = max(request.timeoutInterval, 30)
        let s = URLSession(configuration: config, delegate: pinning, delegateQueue: nil)
        defer { s.finishTasksAndInvalidate() }
        do {
            let (data, response) = try await s.data(for: request)
            return (data, (response as? HTTPURLResponse)?.statusCode ?? 0)
        } catch let error as URLError where error.code != .cancelled {
            throw Failure.unreachable
        }
    }
}
