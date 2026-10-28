import Foundation
import Testing

@testable import StormoKit

@Suite struct ChatDecodingTests {
    @Test func transcriptFromCLI() throws {
        let line = #"{"event":"result","data":{"agent":"sofia","id":"api_1","tip":"api_1b","compacted":true,"messages":[{"id":"82","role":"user","kind":"text","content":"Hi","at":"2026-10-09T19:12:52Z"},{"role":"assistant","kind":"tool","content":"","tools":["web_search"]},{"id":"83","role":"assistant","kind":"text","content":"OK","at":"2026-10-09T19:12:55Z"}]}}"#
        let t = try #require(try CLIEvent(line: line)?.decode(Transcript.self))
        #expect(t.compacted && t.tip == "api_1b" && t.messages.count == 3)
        #expect(t.messages[1].isTool && t.messages[1].tools == ["web_search"])
        #expect(t.messages[0].date != nil && t.messages[0].isUser)
    }

    @Test func followUpRows() throws {
        let line = #"{"event":"result","data":{"agent":"sofia","id":"api_1","tip":"api_1","compacted":false,"messages":[{"id":"1","role":"user","kind":"text","content":"Say hola in 10 s"},{"id":"2","role":"assistant","kind":"text","content":"Will do"},{"id":"3","role":"user","kind":"text","content":"Say hola","source":"follow_up"},{"id":"4","role":"user","kind":"text","content":"x","source":"something_new"},{"id":"5","role":"assistant","kind":"text","content":"hola","source":"follow_up"}]}}"#
        let rows = try #require(try CLIEvent(line: line)?.decode(Transcript.self)).messages
        #expect(!rows[0].isFollowUp && rows[0].isUser && rows[0].source == nil)
        #expect(rows[2].isFollowUp && rows[2].isUser && rows[2].source == "follow_up")
        // An unknown source is the owner's row, as before; only a user row can be a follow-up.
        #expect(rows[3].isUser && !rows[3].isFollowUp)
        #expect(!rows[4].isFollowUp)
    }

    @Test func rowsWithoutIDsStayTheSameAcrossReads() throws {
        let line = #"{"event":"result","data":{"agent":"sofia","id":"api_1","tip":"api_1","compacted":false,"messages":[{"role":"user","kind":"text","content":"a"},{"id":"82","role":"user","kind":"text","content":"Hi"},{"role":"assistant","kind":"tool","content":"","tools":["web_search"]},{"role":"tool","kind":"tool","content":"r","tool":"web_search"},{"id":"83","role":"assistant","kind":"text","content":"OK"},{"id":"83","role":"assistant","kind":"text","content":"again"}]}}"#
        let a = try #require(try CLIEvent(line: line)?.decode(Transcript.self))
        let b = try #require(try CLIEvent(line: line)?.decode(Transcript.self))
        #expect(a == b)
        #expect(a.messages.map(\.id) == ["start+1", "82", "82+1", "82+2", "83", "83+1"])
        #expect(Set(a.messages.map(\.id)).count == a.messages.count)
    }

    @Test func conversationsFromCLI() throws {
        let line = #"{"event":"result","data":{"agent":"sofia","conversations":[{"id":"a","title":"Plan","source":"api_server","lastActive":"2026-10-09T19:12:55Z","messages":4,"ended":false,"compacted":true},{"id":"b","source":"cron","preview":"Daily report","messages":6,"ended":true}]}}"#
        let list = try #require(try CLIEvent(line: line)?.decode(ConversationsResult.self)).conversations
        #expect(list[0].isDirect && list[0].displayTitle == "Plan" && list[0].activeDate != nil && list[0].compacted == true)
        #expect(!list[1].isDirect && list[1].displayTitle == "Daily report" && list[1].sourceLabel == "Scheduled")
    }

    @Test func errorCodes() {
        #expect(ChatProblem(CLIError(code: "not_running", message: "x", status: 1, stderr: "")).kind == .notRunning)
        #expect(ChatProblem(CLIError(code: "busy", message: "x", status: 1, stderr: "")).kind == .busy)
        #expect(ChatProblem(CLIError(code: "failed", message: "boom", status: 1, stderr: "")).message == "boom")
        let eof = #"nova: Get "http://127.0.0.1:18649/api/sessions/api_1/messages?limit=500&order=latest": EOF"#
        #expect(ChatProblem(CLIError(code: "failed", message: eof, status: 1, stderr: "")).kind == .starting)
        let refused = #"nova: Post "http://127.0.0.1:18649/api/sessions": dial tcp 127.0.0.1:18649: connect: connection refused"#
        #expect(ChatProblem(CLIError(code: "failed", message: refused, status: 1, stderr: "")).kind == .starting)
        #expect(ChatProblem(CLIError(code: "failed", message: "openrouter: EOF", status: 1, stderr: "")).kind == .other)
    }

    @Test func titles() {
        #expect(ChatStore.title(for: "First line\nsecond") == "First line")
        #expect(ChatStore.title(for: String(repeating: "a", count: 80)).count == 58)
    }
}

/// An agent's sessions in memory.
actor FakeChat: ChatBackend {
    var sessions: [String: Transcript] = [:]
    var list: [Conversation] = []
    var running = true
    var next = 0

    init(_ list: [Conversation] = []) {
        self.list = list
        for c in list { sessions[c.id] = Transcript(agent: "ada", id: c.id, tip: c.id, compacted: false, messages: []) }
    }

    /// Transcript reads so far.
    var reads = 0

    func stop() { running = false }

    /// A row the agent's runtime adds on its own (a follow-up coming due).
    func append(_ id: String, _ m: ChatMessage) { sessions[id]?.messages.append(m) }

    private func check() throws {
        if !running { throw CLIError(code: "not_running", message: "ada: not running locally", status: 1, stderr: "") }
    }

    func conversations(_ agent: String) async throws -> [Conversation] { try check(); return list }

    func transcript(_ agent: String, _ id: String) async throws -> Transcript {
        try check()
        reads += 1
        guard let t = sessions[id] else { throw CLIError(code: "not_found", message: "", status: 1, stderr: "") }
        return t
    }

    func newConversation(_ agent: String, title: String?) async throws -> Transcript {
        try check()
        next += 1
        let id = "api_\(next)"
        let t = Transcript(agent: agent, id: id, tip: id, compacted: false, messages: [])
        sessions[id] = t
        list.insert(Conversation(id: id, title: title), at: 0)
        return t
    }

    func send(_ agent: String, session: String, message: String) async throws -> ChatReply {
        try check()
        sessions[session]?.messages += [ChatMessage(id: "u\(message)", role: "user", content: message),
                                        ChatMessage(id: "a\(message)", role: "assistant", content: "echo \(message)")]
        return ChatReply(agent: agent, session: session, reply: "echo \(message)")
    }

    func clearConversation(_ agent: String, _ id: String) async throws -> ClearResult {
        try check()
        sessions[id] = nil
        list.removeAll { $0.id == id }
        return ClearResult(agent: agent, cleared: [id], removed: 1)
    }

    func clearConversations(_ agent: String) async throws -> ClearResult {
        try check()
        let ids = list.filter(\.isDirect).map(\.id)
        for id in ids { sessions[id] = nil }
        list.removeAll(where: \.isDirect)
        return ClearResult(agent: agent, cleared: ids, removed: ids.count)
    }
}

@MainActor @Suite struct ChatStoreTests {
    func waitForTurn(_ store: ChatStore) async {
        for _ in 0..<200 where store.isSending { try? await Task.sleep(for: .milliseconds(10)) }
    }

    @Test func firstMessageStartsAConversation() async {
        let fake = FakeChat([Conversation(id: "slack1", source: "slack", messages: 3)])
        let store = ChatStore(backend: fake)
        await store.selectAgent("ada")
        #expect(store.selection == "slack1" && !store.canSend)

        await store.open(nil)
        #expect(store.canSend)
        store.send("Hello there")
        #expect(store.visiblePending?.text == "Hello there")
        await waitForTurn(store)
        #expect(store.selection == "api_1")
        #expect(store.transcript?.messages.map(\.content) == ["Hello there", "echo Hello there"])
        #expect(store.conversations.first?.id == "api_1" && store.conversations.first?.title == "Hello there")

        store.send("Again")
        await waitForTurn(store)
        #expect(store.transcript?.messages.count == 4 && store.selection == "api_1")
    }

    @Test func notRunningIsReported() async {
        let fake = FakeChat()
        let store = ChatStore(backend: fake)
        await store.selectAgent("ada")
        await fake.stop()
        store.send("Hi")
        await waitForTurn(store)
        #expect(store.problem?.kind == .notRunning)
        #expect(store.pending == nil)
    }

    @Test func clearing() async {
        let fake = FakeChat([Conversation(id: "a"), Conversation(id: "b"), Conversation(id: "s", source: "slack")])
        let store = ChatStore(backend: fake)
        await store.selectAgent("ada")
        #expect(store.selection == "a")
        await store.clear("a")
        #expect(store.selection == nil && store.conversations.map(\.id) == ["b", "s"])
        await store.clearAll()
        #expect(store.conversations.map(\.id) == ["s"])
    }
}

@Suite struct RefreshScheduleTests {
    @Test func backsOffWhileNothingChanges() {
        var s = RefreshSchedule(fastest: .seconds(5), slowest: .seconds(30))
        #expect(s.delay == .seconds(5))
        var delays: [Duration] = []
        for _ in 0..<6 {
            s.record(changed: false)
            delays.append(s.delay)
        }
        #expect(delays == [.seconds(7.5), .seconds(11.25), .seconds(16.875), .seconds(25.3125), .seconds(30), .seconds(30)])
        s.record(changed: true)
        #expect(s.delay == .seconds(5))
        s.record(changed: false)
        s.reset()
        #expect(s.delay == .seconds(5))
    }

    @Test func aNudgeWaitsOnlyTheFastest() {
        var s = RefreshSchedule(fastest: .seconds(5), slowest: .seconds(30))
        for _ in 0..<5 { s.record(changed: false) }
        #expect(!s.isDue(since: .seconds(6), nudged: false))
        #expect(s.isDue(since: .seconds(6), nudged: true))
        #expect(!s.isDue(since: .seconds(2), nudged: true))
        #expect(s.isDue(since: .seconds(30), nudged: false))
    }

    @Test func sameRows() {
        let rows = [ChatMessage(id: "1", role: "user", content: "a"), ChatMessage(id: "2", role: "assistant", content: "b")]
        let t = Transcript(agent: "ada", id: "c", tip: "c", compacted: false, messages: rows)
        #expect(t.sameRows(as: t))
        var more = t
        more.messages.append(ChatMessage(id: "3", role: "user", content: "hola", source: "follow_up"))
        #expect(!t.sameRows(as: more))
        var moved = t
        moved.tip = "c2"
        #expect(!t.sameRows(as: moved))
        var compacted = t
        compacted.compacted = true
        #expect(!t.sameRows(as: compacted))
    }
}

@MainActor @Suite struct ChatRefreshTests {
    func store(_ fake: FakeChat) -> ChatStore {
        let store = ChatStore(backend: fake)
        store.refreshInterval = (.milliseconds(20), .milliseconds(40))
        store.isVisible = true
        return store
    }

    func running(_ id: String = "ada", seq: Int = 1) -> FleetAgent {
        FleetAgent(id: id, name: id, unit: "u", state: "running",
                   office: OfficeState(activity: "idle", phase: "", start: 0, end: nil, from: nil, slot: 0, label: "", working: false, seq: seq))
    }

    func wait(_ until: () async -> Bool) async {
        for _ in 0..<200 where !(await until()) { try? await Task.sleep(for: .milliseconds(10)) }
    }

    @Test func aFollowUpAppearsWithoutReopening() async {
        let fake = FakeChat([Conversation(id: "api_1")])
        let store = store(fake)
        await store.selectAgent("ada")
        store.observe(running())
        store.startRefreshing()
        defer { store.stopRefreshing() }
        let before = store.transcript
        try? await Task.sleep(for: .milliseconds(120))
        #expect(store.transcript == before, "nothing changed, nothing replaced")

        await fake.append("api_1", ChatMessage(id: "9", role: "user", content: "Say hola", source: "follow_up"))
        await fake.append("api_1", ChatMessage(id: "10", role: "assistant", content: "hola"))
        await wait { store.transcript?.messages.count == 2 }
        #expect(store.transcript?.messages.map(\.content) == ["Say hola", "hola"])
        #expect(store.transcript?.messages.first?.isFollowUp == true)
    }

    @Test func aReadWithTheSameRowsIsNotShown() async throws {
        let fake = FakeChat([Conversation(id: "api_1")])
        let store = ChatStore(backend: fake)
        await fake.append("api_1", ChatMessage(id: "1", role: "user", content: "hi"))
        await store.selectAgent("ada")
        let shown = try #require(store.transcript)
        #expect(!store.apply(shown), "the same rows")
        var more = shown
        more.messages.append(ChatMessage(id: "2", role: "user", content: "hola", source: "follow_up"))
        #expect(store.apply(more) && store.transcript == more)
        #expect(!store.apply(more))
    }

    @Test func noReadsWhileHiddenOrStopped() async {
        let fake = FakeChat([Conversation(id: "api_1")])
        let store = store(fake)
        await store.selectAgent("ada")
        store.isVisible = false
        store.observe(running())
        store.startRefreshing()
        defer { store.stopRefreshing() }
        let opened = await fake.reads
        try? await Task.sleep(for: .milliseconds(150))
        #expect(await fake.reads == opened, "hidden")

        store.isVisible = true
        store.observe(FleetAgent(id: "ada", name: "ada", unit: "u", state: "stopped"))
        try? await Task.sleep(for: .milliseconds(150))
        #expect(await fake.reads == opened, "the agent is not running")

        store.observe(running())
        await wait { await fake.reads > opened }
        #expect(await fake.reads > opened)

        store.stopRefreshing()
        try? await Task.sleep(for: .milliseconds(30))
        let stopped = await fake.reads
        try? await Task.sleep(for: .milliseconds(150))
        #expect(await fake.reads == stopped, "stopped")
    }

    @Test func theFleetNudgesOnlyWhenTheAgentMoves() async {
        let store = ChatStore(backend: FakeChat())
        store.observe(running())
        #expect(!store.agentRunning && !store.nudged, "not the open agent")
        await store.selectAgent("ada")
        store.observe(running(seq: 1))
        #expect(store.agentRunning && !store.nudged, "the first look is not a change")
        store.observe(running(seq: 1))
        #expect(!store.nudged)
        store.observe(running(seq: 2))
        #expect(store.nudged)
        store.observe(running("bob"))
        #expect(!store.agentRunning)
    }
}
