import Foundation
import Testing

@testable import StormoKit

@Suite struct ReviewDecoding {
    @Test func decisionsEncodeAsTheCoreReadsThem() throws {
        let lessons = try JSONEncoder().encode(ReviewDecision.lessons(["l1"], of: "atlas", .promote, scope: "unit"))
        let o = try #require(try JSONSerialization.jsonObject(with: lessons) as? [String: Any])
        #expect(o["agent"] as? String == "atlas" && o["lessons"] as? [String] == ["l1"] && o["decision"] as? String == "promote" && o["scope"] as? String == "unit")
        #expect(o["skill"] == nil)
    }

    @Test func ownerTokenIsTrimmedAndOptional() throws {
        let s = try Scratch()
        #expect(ReviewStore.ownerToken(root: s.url) == nil)
        let dir = s.url.appending(path: ".swarm/core")
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        try "abc123\n".write(to: dir.appending(path: "owner.token"), atomically: true, encoding: .utf8)
        #expect(ReviewStore.ownerToken(root: s.url) == "abc123")
    }
}

/// The Review section against a real core (this checkout's bin/stormo) on a scratch instance.
@Suite(.serialized) struct RealReview {
    @Test(.enabled(if: FileManager.default.isExecutableFile(atPath: RealBinary.binary.path)))
    @MainActor func listsAndRejectsASkillProposal() async throws {
        let s = try Scratch()
        let instance = s.url.appending(path: "acme")
        try FileManager.default.copyItem(at: RealBinary.engine.appending(path: "examples/minimal"), to: instance)
        let prop = instance.appending(path: "agents/atlas/learnings/proposals/skills/quote-check")
        try FileManager.default.createDirectory(at: prop, withIntermediateDirectories: true)
        try #"{"skill":"quote-check","napHash":"abc","nap":"n1","pii":[]}"#.write(to: prop.appending(path: ".proposal.json"), atomically: true, encoding: .utf8)
        try "---\nname: quote-check\n---\nCheck quotes twice.\n".write(to: prop.appending(path: "SKILL.md"), atomically: true, encoding: .utf8)

        let port = 18680 + Int.random(in: 0..<9)
        var env = ShellEnvironment.compose(shell: nil, process: ProcessInfo.processInfo.environment)
        env["SWARM_CORE_PORT"] = String(port)
        let cli = StormoCLI(executable: RealBinary.binary, environment: env, instance: instance)
        let supervisor = CoreSupervisor(client: CoreClient(port: port), instanceRoot: instance)
        await supervisor.start(with: cli)
        #expect(supervisor.state.isRunning, "\(supervisor.state) \(supervisor.lastError ?? "")")

        let wrong = CoreClient(port: port)
        await #expect(throws: CoreClient.Failure.self) { try await wrong.review(owner: "not-the-owner") }

        let store = ReviewStore(client: CoreClient(port: port), instanceRoot: instance)
        await store.load()
        #expect(store.problem == nil, "\(store.problem ?? "")")
        let atlas = try #require(store.view?.agents.first { $0.id == "atlas" })
        #expect(atlas.skills.map(\.skill) == ["quote-check"] && atlas.skills[0].new && atlas.skills[0].files.first?.text?.contains("twice") == true)
        #expect(store.pending == 1)

        await store.decide(.skill("quote-check", of: "atlas", .reject))
        #expect(store.problem == nil, "\(store.problem ?? "")")
        #expect(store.pending == 0)

        await supervisor.stop(with: cli)
    }
}
