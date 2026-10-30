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

    @Test func runtimeDecisionsEncodeAsTheCoreReadsThem() throws {
        let sch = try #require(try JSONSerialization.jsonObject(with: JSONEncoder().encode(ReviewDecision.schedules(of: "atlas", .reject))) as? [String: Any])
        #expect(sch["schedules"] as? Bool == true && sch["decision"] as? String == "reject" && sch["lessons"] == nil && sch["skill"] == nil)
        let set = try #require(try JSONSerialization.jsonObject(with: JSONEncoder().encode(ReviewDecision.settings(["home_channel.slack"], of: "atlas", .accept))) as? [String: Any])
        #expect(set["settings"] as? [String] == ["home_channel.slack"] && set["schedules"] == nil)
        #expect(ReviewDecision.schedules(of: "atlas", .accept).key != ReviewDecision.settings(["x"], of: "atlas", .accept).key)
    }

    @Test func decodesVerdictsHeldReasonsAndRuntimeProposals() throws {
        let body = #"""
        {"review":"auto","reviewConnection":"chatgpt","autoAccept":true,"autoAcceptMinSeen":1,"autoRestart":false,"agents":[
         {"id":"atlas","name":"Atlas","unit":"sales","state":"running","busy":false,"unapplied":false,"autoReview":true,
          "notices":["outcome evidence (evidence-v1.json) is unreadable, so it reads as none: bad"],
          "lessons":[{"id":"l1","kind":"memory","scope":"agent","text":"t","pii":[],"status":"proposed","seenCount":1,"firstSeen":"a","lastSeen":"b","autoAcceptable":true,
                      "ai":{"approve":false,"reason":"names a client","model":"m","at":"z"}}],
          "skills":[],
          "schedules":{"jobs":[{"id":"rt1","name":"Digest","cron":"0 8 * * *","prompt":"p","agent":true,"enabled":true,"deliver":"slack"}],"runtimeJobs":["rt1"],"pii":[],"nap":"n","held":"job rt1 runs a script"},
          "settings":[{"key":"home_channel.slack","value":"C1","opaque":true,"pii":[],"nap":"n"}]}]}
        """#
        let v = try JSONDecoder().decode(ReviewView.self, from: Data(body.utf8))
        let a = try #require(v.agents.first)
        #expect(v.review == "auto" && a.pending == 3 && a.lessons[0].ai?.reason == "names a client")
        #expect(a.schedules?.held == "job rt1 runs a script" && a.schedules?.jobs.first?.when == "0 8 * * *")
        #expect(a.runtimeSettings.first?.key == "home_channel.slack" && a.runtimeSettings.first?.current == nil)
        // A core from before the automatic review still decodes.
        let old = #"{"autoAccept":false,"autoAcceptMinSeen":1,"autoRestart":false,"agents":[{"id":"a","name":"A","unit":"u","state":"stopped","busy":false,"unapplied":false,"lessons":[],"skills":[]}]}"#
        let o = try JSONDecoder().decode(ReviewView.self, from: Data(old.utf8))
        #expect(o.review == nil && o.agents[0].pending == 0 && o.agents[0].notices == nil)
        #expect(a.notices?.first?.contains("evidence-v1.json") == true)
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

@Suite struct Diffing {
    @Test func hunksKeepContextAndCollapseTheRest() {
        let old = (1...20).map { "line \($0)" }.joined(separator: "\n") + "\n"
        let new = old.replacingOccurrences(of: "line 10\n", with: "line ten\n") + "line 21\n"
        let d = LineDiff(old: old, new: new, context: 2)
        #expect(d.added == 2 && d.removed == 1)
        #expect(d.hunks.count == 2)
        #expect(d.hunks[0].skippedBefore == 7)
        #expect(d.hunks[0].lines.map(\.kind) == [.same, .same, .removed, .added, .same, .same])
        #expect(d.hunks[0].lines[2].old == 10 && d.hunks[0].lines[3].new == 10)
        #expect(d.hunks[1].skippedBefore == 6 && d.hunks[1].lines.last == .init(kind: .added, text: "line 21", old: nil, new: 21))
        #expect(d.skippedAfter == 0)
    }

    @Test func newFileIsAllAdded() {
        let d = LineDiff(old: "", new: "a\nb\n")
        #expect(d.added == 2 && d.removed == 0 && d.hunks.count == 1)
        #expect(LineDiff(old: "same\n", new: "same\n").isEmpty)
    }

    @Test func truncatedKeepsTheFirstLinesAndCountsTheRest() {
        let old = (1...40).map { "line \($0)" }.joined(separator: "\n")
        let new = old.replacingOccurrences(of: "line 5\n", with: "five\n").replacingOccurrences(of: "line 30\n", with: "thirty\n")
        let d = LineDiff(old: old, new: new, context: 2)
        #expect(d.shownLines == 12 && d.hunks.count == 2)
        #expect(d.truncated(maxLines: 12).hidden == 0 && d.truncated(maxLines: 12).diff == d)

        let (cut, hidden) = d.truncated(maxLines: 8)
        #expect(hidden == 4 && cut.shownLines == 8)
        #expect(cut.hunks.count == 2 && cut.hunks[1].lines.count == 2 && cut.hunks[1].skippedBefore == d.hunks[1].skippedBefore)
        #expect(cut.added == d.added && cut.removed == d.removed)

        let whole = LineDiff(old: "", new: (1...1000).map(String.init).joined(separator: "\n"))
        let first = whole.truncated(maxLines: LineDiff.maxShownLines)
        #expect(first.diff.shownLines == LineDiff.maxShownLines && first.hidden == 700 && first.diff.skippedAfter == 0)
    }

    @Test func storeDiffsEveryTextualSkillFileOnce() throws {
        let body = #"""
        {"autoAccept":false,"autoAcceptMinSeen":1,"autoRestart":false,"agents":[
         {"id":"atlas","name":"Atlas","unit":"sales","state":"running","busy":false,"unapplied":false,"lessons":[],
          "skills":[{"skill":"notes","pii":[],"nap":"n","new":false,"files":[
            {"path":"SKILL.md","size":4,"text":"a\nc\n","current":"a\nb\n"},
            {"path":"logo.png","size":9000}]}]}]}
        """#
        let v = try JSONDecoder().decode(ReviewView.self, from: Data(body.utf8))
        let diffs = ReviewStore.diffs(of: v)
        #expect(diffs.count == 1)
        let d = try #require(diffs[ReviewStore.diffKey(agent: "atlas", skill: "notes", path: "SKILL.md")])
        #expect(d.added == 1 && d.removed == 1)
        #expect(diffs[ReviewStore.diffKey(agent: "atlas", skill: "notes", path: "logo.png")] == nil)
    }

    @Test func onlyShortDiffsOfAFewProposalsOpenByDefault() {
        let short = LineDiff(old: "a\n", new: "b\n")
        let long = LineDiff(old: "", new: (1...500).map(String.init).joined(separator: "\n"))
        #expect(LineDiff.opensByDefault(short, skillProposals: 1))
        #expect(!LineDiff.opensByDefault(long, skillProposals: 1))
        #expect(!LineDiff.opensByDefault(short, skillProposals: 58))
        #expect(!LineDiff.opensByDefault(nil, skillProposals: 1))
    }
}
