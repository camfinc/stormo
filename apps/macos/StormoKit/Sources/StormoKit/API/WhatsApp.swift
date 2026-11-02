import Foundation

public struct WhatsAppStatus: Decodable, Sendable {
    public let state: String
    public let qr: String?
    public let error: String?
}

extension CoreClient {
    public func whatsApp(agent: String, method: String = "GET") async throws -> WhatsAppStatus {
        var request = URLRequest(url: base.appending(path: "api/agents/\(agent)/whatsapp"))
        request.httpMethod = method
        return try JSONDecoder().decode(WhatsAppStatus.self, from: try await sendAuthorized(request))
    }
}
