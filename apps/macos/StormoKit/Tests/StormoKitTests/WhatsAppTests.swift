import Foundation
import Testing
@testable import StormoKit

@Suite struct WhatsAppGroups {
    // The core's answer (docs/api.md): a group may have no name.
    static let json = #"""
    {"groups":[{"id":"120363012345678901@g.us","name":"Trip planning"},{"id":"393331234567-1595512345@g.us","name":""}]}
    """#

    @Test func decodesAndLabels() throws {
        let groups = try JSONDecoder().decode(WhatsAppGroupList.self, from: Data(Self.json.utf8)).groups
        #expect(groups.map(\.id) == ["120363012345678901@g.us", "393331234567-1595512345@g.us"])
        #expect(groups.map(\.label) == ["Trip planning", "393331234567-1595512345@g.us"])
    }

    @Test func decodesStatus() throws {
        let s = try JSONDecoder().decode(WhatsAppStatus.self, from: Data(#"{"state":"pairing","qr":"2@abc"}"#.utf8))
        #expect(s.state == "pairing" && s.qr == "2@abc" && s.error == nil)
    }
}
