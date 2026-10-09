import Foundation
import Testing

@testable import StormoKit

private let optionsJSON = """
    {"options":{"units":[{"id":"group","name":"Acme Group"},{"id":"sales","name":"Acme Sales"}],
    "actions":[],"skills":[],"personas":["personas/atlas"],"engines":["hermes"],
    "channels":[{"kind":"slack","secrets":["SLACK_APP_TOKEN","SLACK_BOT_TOKEN"]},{"kind":"telegram","secrets":["TELEGRAM_BOT_TOKEN"]}],
    "allowBots":["none","mentions","all"],"secrets":[],"scripts":[],"reasoning":[],
    "connections":[{"name":"openrouter","kind":"openrouter","label":"OpenRouter","api":true,"key":"OPENROUTER_API_KEY"},
    {"name":"chatgpt","kind":"chatgpt","label":"ChatGPT plan","api":false}]},
    "defaults":{"unit":"sales","engine":{"kind":"hermes","version":"0.21.5","imageTag":"v2026.9.24"},
    "model":{"name":"openai/gpt-6-luna","provider":"openrouter"}},
    "takenIds":["atlas","nova","scout"]}
    """

private func options() throws -> NewAgentOptions {
    try JSONDecoder().decode(NewAgentOptions.self, from: Data(optionsJSON.utf8))
}

@Test func draftStartsFromTheInstanceDefaults() throws {
    let d = NewAgentDraft(options: try options())
    #expect(d.unit == "sales" && !d.createUnit)
    #expect(d.engineVersion == "0.21.5" && d.model == "openai/gpt-6-luna" && !d.useLocal)
    #expect(d.brainProblem == nil)
}

@Test func nameProblems() throws {
    var d = NewAgentDraft(options: try options())
    let taken = try options().takenIds
    #expect(d.nameProblem(taken: taken) != nil)
    d.name = "Atlas"
    #expect(d.nameProblem(taken: taken)?.contains("already exists") == true)
    d.name = "Front Desk"
    #expect(d.id == "front-desk" && d.nameProblem(taken: taken) == nil)
    d.customID = "Bad ID"
    #expect(d.nameProblem(taken: taken) != nil)
}

@Test func emptyInstanceNeedsATeamAndAnEngineVersion() {
    var d = NewAgentDraft()
    d.createUnit = true
    #expect(d.unitProblem(units: ["group"]) != nil)
    d.newUnitName = "Operations"
    #expect(d.newUnitID == "operations" && d.unitProblem(units: ["group"]) == nil)
    d.engineKind = "hermes"
    d.model = "openai/gpt-6-luna"
    #expect(d.brainProblem?.contains("engine version") == true)
}

@Test func soulFollowsTheDraftUntilEdited() {
    var d = NewAgentDraft()
    d.name = "Ada"
    d.role = "Runs operations."
    d.tone = .concise
    #expect(d.soul.hasPrefix("# Ada\n\nRuns operations.\n"))
    #expect(d.soul.contains("Keep answers short"))
    d.editedSoul = "# Ada\n\nMine."
    d.role = "Changed."
    #expect(d.soul == "# Ada\n\nMine.")
}

@Test func specEncodesTheEngineKeys() throws {
    var d = NewAgentDraft(options: try options())
    d.name = "Front Desk"
    d.role = "  Greets visitors. "
    d.channels["slack"] = .init()
    d.channels["slack"]?.allowedUsers = "U0000000001, U0000000002"
    d.createUnit = true
    d.newUnitName = "Front Office"
    let json = try JSONSerialization.jsonObject(with: JSONEncoder().encode(d.spec)) as? [String: Any]
    #expect(json?["id"] as? String == "front-desk")
    #expect(json?["role"] as? String == "Greets visitors.")
    #expect(json?["unit"] == nil)
    #expect((json?["newUnit"] as? [String: Any])?["id"] as? String == "front-office")
    let engine = json?["engine"] as? [String: Any]
    #expect(engine?["imageTag"] as? String == "v2026.9.24")
    let model = json?["model"] as? [String: Any]
    #expect(model?["localName"] == nil)
    let slack = (json?["channels"] as? [[String: Any]])?.first
    #expect(slack?["allowedUsers"] as? [String] == ["U0000000001", "U0000000002"])
    #expect(d.neededSecrets(try options().options) == ["OPENROUTER_API_KEY", "SLACK_APP_TOKEN", "SLACK_BOT_TOKEN"])
}
