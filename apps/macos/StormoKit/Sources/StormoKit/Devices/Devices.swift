import Foundation

// Linked devices (docs/devices.md): a watch or a phone that reaches the fleet through the device
// gateway. The owner links one by typing the code it shows, with grants that say what it may reach.

/// What a linked device may reach. Encoded whole: `chat: []` (no agent) is said, never left out.
public struct DeviceGrants: Codable, Sendable, Equatable, Hashable {
    /// The compact fleet view.
    public var status: Bool
    /// Accept or reject lessons and settings (never skills, schedules, scripts or anything flagged).
    public var review: Bool
    /// The agents it may chat with: `["*"]` every agent, `[]` none.
    public var chat: [String]

    public init(status: Bool = false, review: Bool = false, chat: [String] = []) {
        self.status = status
        self.review = review
        self.chat = chat
    }

    /// A core that leaves out a false flag or an empty list (`omitempty`) still decodes.
    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        status = try c.decodeIfPresent(Bool.self, forKey: .status) ?? false
        review = try c.decodeIfPresent(Bool.self, forKey: .review) ?? false
        chat = try c.decodeIfPresent([String].self, forKey: .chat) ?? []
    }

    /// Who it may chat with, as the owner picks it.
    public enum Chat: String, Sendable, CaseIterable, Identifiable {
        case none, all, some
        public var id: String { rawValue }
    }

    public var chatAccess: Chat { chat.contains("*") ? .all : chat.isEmpty ? .none : .some }

    /// Nothing granted: the device can sign in and nothing more.
    public var isEmpty: Bool { !status && !review && chat.isEmpty }

    /// "Status · Review · Chat with every agent", "Chat with Ada, support", "Nothing".
    public func summary(name: (String) -> String = { $0 }) -> String {
        var parts: [String] = []
        if status { parts.append("Status") }
        if review { parts.append("Review") }
        switch chatAccess {
        case .all: parts.append("Chat with every agent")
        case .some: parts.append("Chat with " + chat.map(name).joined(separator: ", "))
        case .none: break
        }
        return parts.isEmpty ? "Nothing" : parts.joined(separator: " · ")
    }
}

/// A device linked to this instance, or revoked (`GET /api/devices`). Times are Unix milliseconds.
public struct LinkedDevice: Codable, Sendable, Equatable, Identifiable {
    public var id: String
    public var name: String
    public var model: String?
    public var os: String?
    public var appVersion: String?
    /// The device key's fingerprint, `XXXX-XXXX-XXXX`.
    public var fingerprint: String?
    public var grants: DeviceGrants
    public var created: Int64?
    /// nil until it first calls in.
    public var lastSeen: Int64?
    public var lastIp: String?
    public var country: String?
    /// How many attestation certificates it sent when it paired (recorded, not verified).
    public var attestation: Int?
    /// When it was revoked; nil while linked.
    public var revoked: Int64?
    public var revokedReason: String?

    public init(id: String, name: String, model: String? = nil, os: String? = nil, appVersion: String? = nil,
                fingerprint: String? = nil, grants: DeviceGrants = DeviceGrants(), created: Int64? = nil,
                lastSeen: Int64? = nil, lastIp: String? = nil, country: String? = nil, attestation: Int? = nil,
                revoked: Int64? = nil, revokedReason: String? = nil) {
        self.id = id
        self.name = name
        self.model = model
        self.os = os
        self.appVersion = appVersion
        self.fingerprint = fingerprint
        self.grants = grants
        self.created = created
        self.lastSeen = lastSeen
        self.lastIp = lastIp
        self.country = country
        self.attestation = attestation
        self.revoked = revoked
        self.revokedReason = revokedReason
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        name = try c.decodeIfPresent(String.self, forKey: .name) ?? id
        model = try c.decodeIfPresent(String.self, forKey: .model)
        os = try c.decodeIfPresent(String.self, forKey: .os)
        appVersion = try c.decodeIfPresent(String.self, forKey: .appVersion)
        fingerprint = try c.decodeIfPresent(String.self, forKey: .fingerprint)
        grants = try c.decodeIfPresent(DeviceGrants.self, forKey: .grants) ?? DeviceGrants()
        created = try c.decodeIfPresent(Int64.self, forKey: .created)
        lastSeen = try c.decodeIfPresent(Int64.self, forKey: .lastSeen)
        lastIp = try c.decodeIfPresent(String.self, forKey: .lastIp)
        country = try c.decodeIfPresent(String.self, forKey: .country)
        attestation = try c.decodeIfPresent(Int.self, forKey: .attestation)
        revoked = try c.decodeIfPresent(Int64.self, forKey: .revoked)
        revokedReason = try c.decodeIfPresent(String.self, forKey: .revokedReason)
    }

    public var isRevoked: Bool { revoked != nil }
    public var lastSeenDate: Date? { lastSeen.map(Date.init(unixMilliseconds:)) }
    public var revokedDate: Date? { revoked.map(Date.init(unixMilliseconds:)) }
}

/// A device that asked to pair and waits for the owner to type its code. The code itself is never
/// listed: only someone who sees the device's screen can link it.
public struct DevicePairing: Codable, Sendable, Equatable, Identifiable {
    public var id: String
    public var name: String
    public var model: String?
    public var os: String?
    public var appVersion: String?
    /// Where the request came from, as the gateway saw it.
    public var ip: String?
    public var country: String?
    public var fingerprint: String?
    public var created: Int64?
    public var expires: Int64?

    public init(id: String, name: String, model: String? = nil, os: String? = nil, appVersion: String? = nil,
                ip: String? = nil, country: String? = nil, fingerprint: String? = nil, created: Int64? = nil, expires: Int64? = nil) {
        self.id = id
        self.name = name
        self.model = model
        self.os = os
        self.appVersion = appVersion
        self.ip = ip
        self.country = country
        self.fingerprint = fingerprint
        self.created = created
        self.expires = expires
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        name = try c.decodeIfPresent(String.self, forKey: .name) ?? ""
        model = try c.decodeIfPresent(String.self, forKey: .model)
        os = try c.decodeIfPresent(String.self, forKey: .os)
        appVersion = try c.decodeIfPresent(String.self, forKey: .appVersion)
        ip = try c.decodeIfPresent(String.self, forKey: .ip)
        country = try c.decodeIfPresent(String.self, forKey: .country)
        fingerprint = try c.decodeIfPresent(String.self, forKey: .fingerprint)
        created = try c.decodeIfPresent(Int64.self, forKey: .created)
        expires = try c.decodeIfPresent(Int64.self, forKey: .expires)
    }

    public var expiresDate: Date? { expires.map(Date.init(unixMilliseconds:)) }

    public func isExpired(at now: Date = Date()) -> Bool {
        guard let e = expiresDate else { return false }
        return e <= now
    }
}

/// `GET /api/devices`: linked and revoked devices, and the pairings still pending.
public struct DevicesList: Codable, Sendable, Equatable {
    public var devices: [LinkedDevice]
    public var pairings: [DevicePairing]

    public init(devices: [LinkedDevice] = [], pairings: [DevicePairing] = []) {
        self.devices = devices
        self.pairings = pairings
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        devices = try c.decodeIfPresent([LinkedDevice].self, forKey: .devices) ?? []
        pairings = try c.decodeIfPresent([DevicePairing].self, forKey: .pairings) ?? []
    }
}

/// The code a device shows while it pairs: 8 letters from `alphabet`, shown `XXXX-XXXX`. The owner
/// types it; case, spaces and dashes do not matter.
public enum PairingCode {
    /// No vowels (no words) and no letters that read like digits.
    public static let alphabet = "BCDFGHJKLMNPQRSTVWXZ"
    public static let length = 8

    /// Upper case, spaces and dashes taken out, at most `length` characters. Other characters are
    /// kept so `problem` can point at them.
    public static func normalize(_ text: String) -> String {
        let dashes: Set<Character> = ["-", "‐", "‑", "‒", "–", "—", "_"]
        let kept = text.uppercased().filter { !$0.isWhitespace && !dashes.contains($0) }
        return String(kept.prefix(length))
    }

    /// As the device shows it, also while typing: "WDJB-MJHT", "WDJB-M", "WDJ".
    public static func format(_ text: String) -> String {
        let n = normalize(text)
        guard n.count > 4 else { return n }
        return n.prefix(4) + "-" + n.dropFirst(4)
    }

    /// All 8 letters, each one a code can have.
    public static func isComplete(_ text: String) -> Bool {
        let n = normalize(text)
        return n.count == length && n.allSatisfy(alphabet.contains)
    }

    /// Why what was typed cannot be a code, nil while it still can.
    public static func problem(_ text: String) -> String? {
        let n = normalize(text)
        guard n.contains(where: { !alphabet.contains($0) }) else { return nil }
        return "A code has only the letters \(alphabet): no vowels, no digits."
    }
}

extension Date {
    init(unixMilliseconds ms: Int64) { self.init(timeIntervalSince1970: Double(ms) / 1000) }
}

// MARK: The owner's routes (`/api/devices/*`)

/// `POST /api/devices/link`: a preview, a denial or a link, by the code the device shows.
struct DeviceLinkBody: Encodable, Equatable {
    var userCode: String
    var preview: Bool?
    var deny: Bool?
    var name: String?
    var grants: DeviceGrants?
}

/// `PATCH /api/devices/<id>`: only what changes.
struct DeviceUpdateBody: Encodable, Equatable {
    var name: String?
    var grants: DeviceGrants?
}

extension CoreClient {
    /// Linked and revoked devices and pending pairings (read scope).
    public func devices() async throws -> DevicesList {
        try JSONDecoder().decode(DevicesList.self, from: try await deviceCall(URLRequest(url: base.appending(path: "api/devices"))))
    }

    /// What the pending pairing with this code claims, changing nothing (admin scope).
    public func previewPairing(code: String) async throws -> DevicePairing {
        struct Body: Decodable { let pairing: DevicePairing }
        let r = try linkRequest(DeviceLinkBody(userCode: PairingCode.format(code), preview: true))
        return try JSONDecoder().decode(Body.self, from: try await deviceCall(r)).pairing
    }

    /// Refuses the pending pairing with this code: the device is told and must pair again. Returns
    /// the pairing's id.
    public func denyPairing(code: String) async throws -> String {
        struct Body: Decodable { let denied: String }
        let r = try linkRequest(DeviceLinkBody(userCode: PairingCode.format(code), deny: true))
        return try JSONDecoder().decode(Body.self, from: try await deviceCall(r)).denied
    }

    /// Links the device whose pending pairing has this code, under `name` (nil: the name it gave).
    public func linkDevice(code: String, name: String?, grants: DeviceGrants) async throws -> LinkedDevice {
        struct Body: Decodable { let device: LinkedDevice }
        let r = try linkRequest(DeviceLinkBody(userCode: PairingCode.format(code), name: name, grants: grants))
        return try JSONDecoder().decode(Body.self, from: try await deviceCall(r)).device
    }

    /// Renames a device or changes its grants; nil leaves that as it is.
    public func updateDevice(_ id: String, name: String?, grants: DeviceGrants?) async throws -> LinkedDevice {
        struct Body: Decodable { let device: LinkedDevice }
        let r = try updateRequest(id, DeviceUpdateBody(name: name, grants: grants))
        return try JSONDecoder().decode(Body.self, from: try await deviceCall(r)).device
    }

    /// Revokes a device: its sessions stop at their next call. Returns its id.
    public func revokeDevice(_ id: String) async throws -> String {
        struct Body: Decodable { let revoked: String }
        return try JSONDecoder().decode(Body.self, from: try await deviceCall(revokeRequest(id))).revoked
    }

    func linkRequest(_ body: DeviceLinkBody) throws -> URLRequest {
        try jsonRequest("api/devices/link", method: "POST", body: body)
    }

    func updateRequest(_ id: String, _ body: DeviceUpdateBody) throws -> URLRequest {
        try jsonRequest("api/devices/\(id)", method: "PATCH", body: body)
    }

    func revokeRequest(_ id: String) -> URLRequest {
        var r = URLRequest(url: base.appending(path: "api/devices/\(id)"))
        r.httpMethod = "DELETE"
        return r
    }

    private func jsonRequest(_ path: String, method: String, body: some Encodable) throws -> URLRequest {
        var r = URLRequest(url: base.appending(path: path))
        r.httpMethod = method
        r.setValue("application/json", forHTTPHeaderField: "Content-Type")
        r.httpBody = try JSONEncoder().encode(body)
        return r
    }

    /// 200 and 201 give the body. Any other answer the core explained comes back as `refused` with its
    /// message, a 404 included: on these routes it means no pending pairing has that code, or no such
    /// device. A bare 404 (no explanation) is `notFound`.
    private func deviceCall(_ request: URLRequest) async throws -> Data {
        let (data, status) = try await raw(request)
        switch status {
        case 200, 201: return data
        default:
            struct Body: Decodable { struct E: Decodable { let message: String }; let error: E }
            if let b = try? JSONDecoder().decode(Body.self, from: data) { throw Failure.refused(status, b.error.message) }
            throw status == 404 ? Failure.notFound : Failure.status(status)
        }
    }
}
