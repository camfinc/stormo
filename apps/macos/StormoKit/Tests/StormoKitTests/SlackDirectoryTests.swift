import Foundation
import Testing
@testable import StormoKit

@Suite struct SlackDirectoryLabels {
    // The core's answer (docs/api.md), omitted fields included.
    static let json = #"""
    {"team":{"id":"T1","name":"Acme"},"bot":{"id":"UBOT","name":"atlas","bot":true},
     "people":[{"id":"U0ANA","name":"Ana","realName":"Ana Alves","owner":true,"tz":"Europe/Lisbon"},
               {"id":"U0GUS","name":"Gus","guest":true},
               {"id":"UBOT","name":"atlas","bot":true},{"id":"U0NOVA","name":"Nova","bot":true}],
     "channels":[{"id":"G1","name":"deals","private":true,"member":true},{"id":"C1","name":"general","member":false}],
     "dms":[{"id":"D1","user":"U0ANA"}],"fetchedAt":"2026-10-29T00:00:00Z"}
    """#

    @Test func decodesAndNames() throws {
        let d = try JSONDecoder().decode(SlackDirectory.self, from: Data(Self.json.utf8))
        #expect(d.team.name == "Acme" && d.partial == nil)
        #expect(d.humans.map(\.id) == ["U0ANA", "U0GUS"])
        #expect(d.bots.map(\.id) == ["U0NOVA"], "the agent's own bot is not offered")
        #expect(d.label(for: "U0ANA") == "Ana (Ana Alves)")
        #expect(d.label(for: "U0GUS") == "Gus · guest")
        #expect(d.label(for: "U0NOVA") == "Nova · bot")
        #expect(d.label(for: "G1") == "#deals" && d.channels[0].isPrivate && !d.channels[1].isPrivate)
        #expect(d.label(for: "D1") == "DM with Ana")
        #expect(d.label(for: "U0NOBODY") == nil)
        #expect(d.dm(with: "U0ANA")?.id == "D1" && d.dm(with: "U0GUS") == nil)
    }

    @Test func describesDeliveryTargets() throws {
        let d = try JSONDecoder().decode(SlackDirectory.self, from: Data(Self.json.utf8))
        #expect(d.describe(target: "slack") == "the home channel")
        #expect(d.describe(target: "slack:G1") == "#deals")
        #expect(d.describe(target: "slack:D1:1712.5") == "DM with Ana (a thread)")
        #expect(d.describe(target: "local, slack:C9") == "nowhere (recorded only), C9")
        #expect(d.describe(target: " ") == nil)
    }

    @Test func problemsSayWhatToFix() {
        #expect(SlackDirectoryStore.describe(CoreClient.Failure.notFound).contains("no Slack bot token"))
        #expect(SlackDirectoryStore.describe(CoreClient.Failure.refused(502, "auth.test: invalid_auth")) == "Slack refused the bot token: auth.test: invalid_auth")
    }
}
