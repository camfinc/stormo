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

    @Test func conversationsFromCLI() throws {
        let line = #"{"event":"result","data":{"agent":"sofia","conversations":[{"id":"a","title":"Plan","source":"api_server","lastActive":"2026-10-09T19:12:55Z","messages":4,"ended":false},{"id":"b","source":"cron","preview":"Daily report","messages":6,"ended":true}]}}"#
        let list = try #require(try CLIEvent(line: line)?.decode(ConversationsResult.self)).conversations
        #expect(list[0].isDirect && list[0].displayTitle == "Plan" && list[0].activeDate != nil)
        #expect(!list[1].isDirect && list[1].displayTitle == "Daily report" && list[1].sourceLabel == "Scheduled")
    }

    @Test func errorCodes() {
        #expect(ChatProblem(CLIError(code: "not_running", message: "x", status: 1, stderr: "")).kind == .notRunning)
        #expect(ChatProblem(CLIError(code: "busy", message: "x", status: 1, stderr: "")).kind == .busy)
        #expect(ChatProblem(CLIError(code: "failed", message: "boom", status: 1, stderr: "")).message == "boom")
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

    func stop() { running = false }

    private func check() throws {
        if !running { throw CLIError(code: "not_running", message: "ada: not running locally", status: 1, stderr: "") }
    }

    func conversations(_ agent: String) async throws -> [Conversation] { try check(); return list }

    func transcript(_ agent: String, _ id: String) async throws -> Transcript {
        try check()
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
