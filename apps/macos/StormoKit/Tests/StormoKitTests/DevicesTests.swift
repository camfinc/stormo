import Foundation
import Testing
@testable import StormoKit

@Suite struct Devices {
    // `GET /api/devices` as docs/devices.md gives it: one device linked, one revoked, one pairing pending.
    static let list = #"""
    {"devices":[
      {"id":"dev_0123456789abcdef","name":"Watch","model":"Pixel Watch 3","os":"Wear OS 6","appVersion":"0.1.0",
       "fingerprint":"3F2A-91C0-7B4E","grants":{"status":true,"review":true,"chat":["support"]},
       "created":1762790000000,"lastSeen":1762793600000,"lastIp":"203.0.113.7","country":"NL","attestation":2,
       "revoked":null,"revokedReason":""},
      {"id":"dev_fedcba9876543210","name":"Old phone","model":"Pixel 7","os":"Android 15","appVersion":"0.0.9",
       "fingerprint":"0A1B-2C3D-4E5F","grants":{"status":true,"review":false,"chat":["*"]},
       "created":1760000000000,"lastSeen":null,"lastIp":"","country":"","attestation":0,
       "revoked":1762000000000,"revokedReason":"lost"}],
     "pairings":[{"id":"pr_1","name":"Watch","model":"Pixel Watch 3","os":"Wear OS 6","ip":"198.51.100.4","country":"DE",
       "fingerprint":"9C8B-7A6F-5E4D","created":1762790000000,"expires":1762790600000}]}
    """#

    @Test func decodesTheList() throws {
        let l = try JSONDecoder().decode(DevicesList.self, from: Data(Self.list.utf8))
        #expect(l.devices.count == 2 && l.pairings.count == 1)
        let d = l.devices[0]
        #expect(d.id == "dev_0123456789abcdef" && d.name == "Watch" && d.model == "Pixel Watch 3")
        #expect(d.grants == DeviceGrants(status: true, review: true, chat: ["support"]))
        #expect(d.lastSeen == 1762793600000 && d.lastSeenDate == Date(timeIntervalSince1970: 1762793600))
        #expect(d.attestation == 2 && !d.isRevoked)
        let r = l.devices[1]
        #expect(r.isRevoked && r.revokedReason == "lost" && r.lastSeen == nil && r.grants.chatAccess == .all)
        let p = l.pairings[0]
        #expect(p.id == "pr_1" && p.ip == "198.51.100.4" && p.fingerprint == "9C8B-7A6F-5E4D" && p.appVersion == nil)
        #expect(p.isExpired(at: Date(timeIntervalSince1970: 1762790599)) == false)
        #expect(p.isExpired(at: Date(timeIntervalSince1970: 1762790600)))
    }

    @Test func missingOptionalsStillDecode() throws {
        // A core that omits what is empty or unknown: no appVersion, lastSeen, lastIp, country,
        // attestation, revoked or revokedReason; grants without chat (omitempty); no pairings key.
        let sparse = #"{"devices":[{"id":"dev_1","name":"Watch","model":"Pixel Watch 3","os":"Wear OS 6","fingerprint":"3F2A-91C0-7B4E","grants":{"status":true},"created":1}]}"#
        let l = try JSONDecoder().decode(DevicesList.self, from: Data(sparse.utf8))
        let d = try #require(l.devices.first)
        #expect(d.appVersion == nil && d.lastSeen == nil && d.lastIp == nil && d.country == nil)
        #expect(d.attestation == nil && d.revoked == nil && d.revokedReason == nil && !d.isRevoked)
        #expect(d.grants == DeviceGrants(status: true, review: false, chat: []))
        #expect(l.pairings.isEmpty)

        let bare = #"{"devices":[{"id":"dev_2"}],"pairings":[{"id":"pr_2"}]}"#
        let b = try JSONDecoder().decode(DevicesList.self, from: Data(bare.utf8))
        #expect(b.devices.first?.name == "dev_2" && b.devices.first?.grants == DeviceGrants())
        #expect(b.pairings.first?.name == "" && b.pairings.first?.expires == nil && b.pairings.first?.isExpired() == false)

        #expect(try JSONDecoder().decode(DevicesList.self, from: Data("{}".utf8)) == DevicesList())
    }

    @Test func decodesThePreview() throws {
        // `POST /api/devices/link` with preview: the pending pairing's claims.
        let json = #"{"pairing":{"id":"pr_1","name":"Watch","model":"Pixel Watch 3","os":"Wear OS 6","appVersion":"0.1.0","ip":"198.51.100.4","country":"DE","fingerprint":"3F2A-91C0-7B4E","created":1762790000000,"expires":1762790600000}}"#
        struct Body: Decodable { let pairing: DevicePairing }
        let p = try JSONDecoder().decode(Body.self, from: Data(json.utf8)).pairing
        #expect(p.appVersion == "0.1.0" && p.country == "DE" && p.expiresDate == Date(timeIntervalSince1970: 1762790600))
    }

    @Test func codesAreNormalizedAndShownLikeTheDevice() {
        #expect(PairingCode.normalize("wdjb-mjht") == "WDJBMJHT")
        #expect(PairingCode.normalize(" wd jb – mj ht ") == "WDJBMJHT")
        #expect(PairingCode.normalize("WDJBMJHTXX") == "WDJBMJHT", "at most 8 letters")
        #expect(PairingCode.format("wdjbmjht") == "WDJB-MJHT")
        #expect(PairingCode.format("wdj") == "WDJ")
        #expect(PairingCode.format("wdjb") == "WDJB")
        #expect(PairingCode.format("wdjbm") == "WDJB-M")
        #expect(PairingCode.format("WDJB-") == "WDJB", "a dash typed after four letters is the one shown")
        #expect(PairingCode.isComplete("wdjb mjht"))
        #expect(!PairingCode.isComplete("WDJB-MJH"))
        #expect(!PairingCode.isComplete("WDJB-MJH1"), "digits are never in a code")
        #expect(!PairingCode.isComplete("WAJB-MJHT"), "nor vowels")
        #expect(PairingCode.problem("WDJB-MJ") == nil)
        #expect(PairingCode.problem("WDJ0") != nil && PairingCode.problem("weee") != nil)
    }

    @Test func grantsSummaries() {
        #expect(DeviceGrants().summary() == "Nothing" && DeviceGrants().isEmpty)
        #expect(DeviceGrants(status: true, review: true, chat: ["*"]).summary() == "Status · Review · Chat with every agent")
        let names = ["support": "Ada"]
        #expect(DeviceGrants(chat: ["support", "sales"]).summary { names[$0] ?? $0 } == "Chat with Ada, sales")
        #expect(DeviceGrants(chat: []).chatAccess == .none && DeviceGrants(chat: ["*"]).chatAccess == .all)
        #expect(DeviceGrants(chat: ["support"]).chatAccess == .some)
    }

    // MARK: What the client sends

    static let client = CoreClient(port: 18600, token: "owner")

    static func body(_ r: URLRequest) throws -> JSONValue {
        try JSONDecoder().decode(JSONValue.self, from: try #require(r.httpBody))
    }

    @Test func previewSendsOnlyTheCode() throws {
        let r = try Self.client.linkRequest(DeviceLinkBody(userCode: PairingCode.format("wdjb mjht"), preview: true))
        #expect(r.httpMethod == "POST" && r.url?.path == "/api/devices/link")
        #expect(r.value(forHTTPHeaderField: "Content-Type") == "application/json")
        #expect(try Self.body(r) == .object(["userCode": .string("WDJB-MJHT"), "preview": .bool(true)]))
    }

    @Test func denySendsTheCodeAndDeny() throws {
        let r = try Self.client.linkRequest(DeviceLinkBody(userCode: "WDJB-MJHT", deny: true))
        #expect(try Self.body(r) == .object(["userCode": .string("WDJB-MJHT"), "deny": .bool(true)]))
    }

    @Test func linkSendsGrantsWhole() throws {
        // No chat is said as an empty list, never left out.
        let none = try Self.client.linkRequest(DeviceLinkBody(userCode: "WDJB-MJHT", grants: DeviceGrants(status: true)))
        #expect(try Self.body(none) == .object([
            "userCode": .string("WDJB-MJHT"),
            "grants": .object(["status": .bool(true), "review": .bool(false), "chat": .array([])]),
        ]), "no name: the device keeps the one it gave")

        let all = try Self.client.linkRequest(DeviceLinkBody(userCode: "WDJB-MJHT", name: "Ada's watch", grants: DeviceGrants(status: true, review: true, chat: ["*"])))
        #expect(try Self.body(all) == .object([
            "userCode": .string("WDJB-MJHT"),
            "name": .string("Ada's watch"),
            "grants": .object(["status": .bool(true), "review": .bool(true), "chat": .array([.string("*")])]),
        ]))

        let some = try Self.client.linkRequest(DeviceLinkBody(userCode: "WDJB-MJHT", grants: DeviceGrants(chat: ["support", "sales"])))
        #expect(try Self.body(some).object?["grants"]?.object?["chat"] == .array([.string("support"), .string("sales")]))
    }

    @Test func updateSendsOnlyWhatChanges() throws {
        let rename = try Self.client.updateRequest("dev_0123456789abcdef", DeviceUpdateBody(name: "Wrist"))
        #expect(rename.httpMethod == "PATCH" && rename.url?.path == "/api/devices/dev_0123456789abcdef")
        #expect(try Self.body(rename) == .object(["name": .string("Wrist")]))

        let regrant = try Self.client.updateRequest("dev_0123456789abcdef", DeviceUpdateBody(grants: DeviceGrants(review: true)))
        #expect(try Self.body(regrant) == .object([
            "grants": .object(["status": .bool(false), "review": .bool(true), "chat": .array([])]),
        ]), "a name left alone is not sent")
    }

    @Test func revokeIsADelete() {
        let r = Self.client.revokeRequest("dev_0123456789abcdef")
        #expect(r.httpMethod == "DELETE" && r.url?.path == "/api/devices/dev_0123456789abcdef" && r.httpBody == nil)
    }

    // MARK: The device gateway among the capabilities

    @Test func theGatewayIsTheIngressCapability() throws {
        let json = #"""
        {"capabilities":[
          {"name":"media.tools","description":"ffmpeg","kind":"service","template":"media","concurrency":1,"costUSD":0,"queued":0,"running":0,"agents":["support"],"worker":"asleep"},
          {"name":"devices.gateway","description":"The way in for linked devices.","kind":"service","template":"device-gateway","concurrency":1,"costUSD":0,
           "queued":0,"running":0,"agents":[],"worker":"awake","alwaysOn":true,"ingress":true,"publish":"127.0.0.1:18650","readOnly":true}],
         "spend":[]}
        """#
        let v = try JSONDecoder().decode(CapabilitiesView.self, from: Data(json.utf8))
        let g = try #require(DevicesStore.gateway(in: v.capabilities))
        #expect(g.name == "devices.gateway" && g.isIngress && g.isAlwaysOn && g.publish == "127.0.0.1:18650" && g.readOnly == true)
        #expect(v.capabilities[0].isIngress == false && v.capabilities[0].isAlwaysOn == false && v.capabilities[0].publish == nil)
        #expect(DevicesStore.gateway(in: [v.capabilities[0]]) == nil)
    }

    @Test func messagesComeFromTheCore() {
        #expect(DevicesStore.describe(CoreClient.Failure.refused(404, "no pending pairing has that code")) == "no pending pairing has that code")
        #expect(DevicesStore.describe(CoreClient.Failure.refused(429, "too many wrong codes: wait a few minutes")) == "too many wrong codes: wait a few minutes")
        #expect(DevicesStore.describe(CoreClient.Failure.notFound) == DevicesStore.noDevicesAPI)
    }
}
