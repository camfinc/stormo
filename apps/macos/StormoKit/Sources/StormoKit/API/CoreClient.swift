import Foundation

/// Reads the core's HTTP API on loopback. Nothing is cached to disk: responses can carry client data.
public struct CoreClient: Sendable {
    public enum Failure: Error, Equatable {
        /// Nothing answers on the port.
        case unreachable
        /// The route does not exist (an older core).
        case notFound
        case status(Int)
    }

    public let base: URL
    private let session: URLSession

    /// The core's default port, or `SWARM_CORE_PORT` from the given environment.
    public static func port(environment: [String: String]) -> Int {
        if let s = environment["SWARM_CORE_PORT"], let n = Int(s), n > 0 { return n }
        return 18600
    }

    public init(port: Int, timeout: TimeInterval = 2) {
        self.init(base: URL(string: "http://127.0.0.1:\(port)")!, timeout: timeout)
    }

    public init(base: URL, timeout: TimeInterval = 2) {
        self.base = base
        let config = URLSessionConfiguration.ephemeral
        config.timeoutIntervalForRequest = timeout
        config.requestCachePolicy = .reloadIgnoringLocalCacheData
        config.urlCache = nil
        self.session = URLSession(configuration: config)
    }

    public func health() async throws -> Health { try await get("health") }
    public func core() async throws -> CoreInfo { try await get("api/core") }
    public func instance() async throws -> InstanceLook { try await get("api/instance") }
    public func fleet() async throws -> Fleet { try await get("api/fleet") }
    public func gateway() async throws -> GatewayStatus { try await get("api/gateway") }

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
        let response: (Data, URLResponse)
        do {
            response = try await session.data(from: base.appending(path: path))
        } catch let error as URLError where error.code != .cancelled {
            throw Failure.unreachable
        }
        guard let http = response.1 as? HTTPURLResponse else { throw Failure.unreachable }
        switch http.statusCode {
        case 200: return response.0
        case 404: throw Failure.notFound
        default: throw Failure.status(http.statusCode)
        }
    }
}
