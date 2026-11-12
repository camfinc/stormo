import Foundation

public struct WhatsAppStatus: Decodable, Sendable {
    public let state: String
    public let qr: String?
    public let error: String?
}

/// A group the agent's linked WhatsApp account is in (`GET /api/agents/<id>/whatsapp/groups`).
public struct WhatsAppGroup: Decodable, Sendable, Equatable, Identifiable {
    /// `…@g.us`: what agent.yaml's `allowed_groups` takes.
    public let id: String
    public let name: String
    /// The group's name, or its id when it has none.
    public var label: String { name.isEmpty ? id : name }
}

/// The groups route's answer: `{groups: [{id, name}]}`.
struct WhatsAppGroupList: Decodable { let groups: [WhatsAppGroup] }

extension CoreClient {
    public func whatsApp(agent: String, method: String = "GET") async throws -> WhatsAppStatus {
        var request = URLRequest(url: base.appending(path: "api/agents/\(agent)/whatsapp"))
        request.httpMethod = method
        return try JSONDecoder().decode(WhatsAppStatus.self, from: try await sendAuthorized(request))
    }

    /// The groups the linked account is in, by name; refused (409) while it is not linked and connected.
    public func whatsAppGroups(agent: String) async throws -> [WhatsAppGroup] {
        let request = URLRequest(url: base.appending(path: "api/agents/\(agent)/whatsapp/groups"))
        return try JSONDecoder().decode(WhatsAppGroupList.self, from: try await sendAuthorized(request)).groups
    }
}
