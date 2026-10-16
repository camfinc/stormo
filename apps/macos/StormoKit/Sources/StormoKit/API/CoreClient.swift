import Foundation

/// Reads the core's HTTP API on loopback. Nothing is cached to disk: responses can carry client data.
public struct CoreClient: Sendable {
    public enum Failure: Error, Equatable {
        /// Nothing answers on the port.
        case unreachable
        /// The route does not exist (an older core).
        case notFound
        case status(Int)
        /// The core answered with an error it explains (401, 400, 409…).
        case refused(Int, String)
    }

    public let base: URL
    /// Sent as `Authorization: Bearer` on every request: the owner token locally, a personal token remotely.
    public let token: String?
    public let pinning: PinningDelegate
    private let session: URLSession

    /// The core's default port, or `SWARM_CORE_PORT` from the given environment.
    public static func port(environment: [String: String]) -> Int {
        port(instanceRoot: nil, environment: environment)
    }

    /// The instance's core port: `SWARM_CORE_PORT`, else the one the engine picked and keeps in
    /// `<instance>/.swarm/core/port` (several instances, several cores), else 18600.
    public static func port(instanceRoot: URL?, environment: [String: String]) -> Int {
        if let s = environment["SWARM_CORE_PORT"], let n = Int(s), n > 0 { return n }
        if let root = instanceRoot, let n = pickedPort(root: root) { return n }
        return 18600
    }

    /// `<instance>/.swarm/core/port`, nil until the engine has picked one.
    public static func pickedPort(root: URL) -> Int? {
        guard let s = try? String(contentsOf: root.appending(path: ".swarm/core/port"), encoding: .utf8),
              let n = Int(s.trimmingCharacters(in: .whitespacesAndNewlines)), n > 0 else { return nil }
        return n
    }

    public var port: Int { base.port ?? 18600 }

    public init(port: Int, token: String? = nil, timeout: TimeInterval = 2) {
        self.init(base: URL(string: "http://127.0.0.1:\(port)")!, token: token, timeout: timeout)
    }

    public init(base: URL, token: String? = nil, pin: String? = nil, timeout: TimeInterval = 2) {
        self.base = base
        self.token = token
        let config = URLSessionConfiguration.ephemeral
        config.timeoutIntervalForRequest = timeout
        config.requestCachePolicy = .reloadIgnoringLocalCacheData
        config.urlCache = nil
        pinning = PinningDelegate(pin: pin)
        self.session = URLSession(configuration: config, delegate: pinning, delegateQueue: nil)
    }

    /// The same core with a longer timeout (restarts, slow calls).
    public func with(timeout: TimeInterval) -> CoreClient {
        CoreClient(base: base, token: token, pin: pinning.pin, timeout: timeout)
    }

    public func health() async throws -> Health { try await get("health") }
    public func core() async throws -> CoreInfo { try await get("api/core") }
    public func instance() async throws -> InstanceLook { try await get("api/instance") }
    public func fleet() async throws -> Fleet { try await get("api/fleet") }
    public func gateway() async throws -> GatewayStatus { try await get("api/gateway") }

    // MARK: Owner (the token in <instance>/.swarm/core/owner.token)

    public func review(owner: String) async throws -> ReviewView {
        try JSONDecoder().decode(ReviewView.self, from: try await send(request("api/review", owner: owner)))
    }

    public func decide(_ decision: ReviewDecision, owner: String) async throws {
        var r = request("api/review", owner: owner)
        r.httpMethod = "POST"
        r.setValue("application/json", forHTTPHeaderField: "Content-Type")
        r.httpBody = try JSONEncoder().encode(decision)
        _ = try await send(r)
    }

    /// Restarts a running local agent so accepted lessons and skills apply (slow: give the client a long timeout).
    public func restart(agent: String, owner: String) async throws {
        var r = request("api/agents/\(agent)/restart", owner: owner)
        r.httpMethod = "POST"
        _ = try await send(r)
    }

    private func request(_ path: String, owner: String) -> URLRequest {
        var r = URLRequest(url: base.appending(path: path))
        r.setValue("Bearer \(owner)", forHTTPHeaderField: "Authorization")
        return r
    }

    /// The agent's portrait, nil when it has none.
    public func avatar(agent: String) async throws -> Data? {
        do {
            return try await data("avatars/\(agent).png")
        } catch Failure.notFound {
            return nil
        }
    }

    private func get<T: Decodable>(_ path: String) async throws -> T {
        try JSONDecoder().decode(T.self, from: try await data(path))
    }

    private func data(_ path: String) async throws -> Data {
        try await send(URLRequest(url: base.appending(path: path)))
    }

    private func send(_ request: URLRequest) async throws -> Data {
        var request = request
        if let token, request.value(forHTTPHeaderField: "Authorization") == nil {
            request.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        }
        let response: (Data, URLResponse)
        do {
            response = try await session.data(for: request)
        } catch let error as URLError where error.code != .cancelled {
            throw Failure.unreachable
        }
        guard let http = response.1 as? HTTPURLResponse else { throw Failure.unreachable }
        switch http.statusCode {
        case 200, 201: return response.0
        case 404: throw Failure.notFound
        default:
            struct Body: Decodable { struct E: Decodable { let message: String }; let error: E }
            if let b = try? JSONDecoder().decode(Body.self, from: response.0) { throw Failure.refused(http.statusCode, b.error.message) }
            throw Failure.status(http.statusCode)
        }
    }
}
