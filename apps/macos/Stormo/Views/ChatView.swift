import StormoKit
import SwiftUI

/// Chat with an agent: its conversations (Slack threads and schedules read-only, direct ones to talk
/// in), clearing them, and the engine's automatic compaction shown where it happened.
struct ChatView: View {
    @Environment(AppModel.self) private var model

    private var store: ChatStore { model.chat }
    private var agents: [FleetAgent] { model.fleet.fleet?.agents ?? [] }

    var body: some View {
        Group {
            if !model.core.state.isRunning && agents.isEmpty {
                CoreNotRunningView()
            } else if agents.isEmpty {
                ContentUnavailableView("No agents yet", systemImage: "person.3",
                                       description: Text("Agents appear here once the core has polled the fleet."))
            } else {
                HSplitView {
                    ChatAgentList(agents: agents)
                        .frame(minWidth: 170, idealWidth: 200, maxWidth: 260, maxHeight: .infinity)
                    ConversationList()
                        .frame(minWidth: 220, idealWidth: 260, maxWidth: 360, maxHeight: .infinity)
                    TranscriptPane()
                        .frame(minWidth: 360, maxWidth: .infinity, maxHeight: .infinity)
                }
                .frame(maxWidth: .infinity, maxHeight: .infinity)
            }
        }
        .navigationTitle("Chat")
        .task(id: model.instances.active?.root) {
            store.use(model.cli, scope: model.instances.active?.root)
            await pickAgent()
        }
        .onChange(of: model.pinned?.url) { store.backend = model.cli }
        .onChange(of: agents.map(\.id)) { Task { await pickAgent() } }
    }

    /// Opens the first agent on shift once the fleet is known, if none is chosen.
    private func pickAgent() async {
        guard store.agent == nil, let first = agents.first(where: { $0.state == "running" }) ?? agents.first else { return }
        await store.selectAgent(first.id)
    }
}

// MARK: Agents

private struct ChatAgentList: View {
    @Environment(AppModel.self) private var model
    let agents: [FleetAgent]

    var body: some View {
        let store = model.chat
        List(selection: Binding(get: { store.agent }, set: { id in Task { await store.selectAgent(id) } })) {
            Section("Agents") {
                ForEach(agents) { a in
                    HStack(spacing: 8) {
                        AgentBadge(agent: a, size: 26)
                        VStack(alignment: .leading, spacing: 1) {
                            Text(a.name).fontWeight(.medium).lineLimit(1)
                            HStack(spacing: 4) {
                                StateDot(agent: a)
                                Text(a.statusLine).font(.caption).foregroundStyle(.secondary).lineLimit(1)
                            }
                        }
                    }
                    .padding(.vertical, 2)
                    .tag(Optional(a.id))
                }
            }
        }
        .listStyle(.sidebar)
    }
}

// MARK: Conversations

private struct ConversationList: View {
    @Environment(AppModel.self) private var model
    @State private var clearing: Conversation?
    @State private var clearingAll = false

    var body: some View {
        let store = model.chat
        VStack(spacing: 0) {
            HStack {
                Text(agentName).font(.headline).lineLimit(1)
                Spacer()
                if store.loadingList { ProgressView().controlSize(.small) }
                Menu {
                    Button("Refresh", systemImage: "arrow.clockwise") { Task { await store.refreshList() } }
                    Divider()
                    Button("Clear All Direct Conversations…", systemImage: "trash", role: .destructive) { clearingAll = true }
                        .disabled(!store.conversations.contains(where: \.isDirect))
                } label: {
                    Image(systemName: "ellipsis.circle")
                }
                .menuStyle(.borderlessButton)
                .menuIndicator(.hidden)
                .fixedSize()
                .disabled(store.agent == nil)
                Button("New Conversation", systemImage: "square.and.pencil") { Task { await store.open(nil) } }
                    .labelStyle(.iconOnly)
                    .buttonStyle(.borderless)
                    .keyboardShortcut("n", modifiers: [.command, .shift])
                    .help("New conversation (⇧⌘N)")
                    .disabled(store.agent == nil)
            }
            .padding(.horizontal, 12)
            .frame(height: 38)
            Divider()
            list(store)
        }
        .confirmationDialog("Clear this conversation?", isPresented: Binding(get: { clearing != nil }, set: { if !$0 { clearing = nil } }), presenting: clearing) { c in
            Button("Clear Conversation", role: .destructive) { Task { await store.clear(c.id) } }
        } message: { c in
            Text(c.isDirect
                 ? "“\(c.displayTitle)” and its messages are deleted from \(agentName). What it learned stays."
                 : "This \(c.sourceLabel) conversation is deleted from \(agentName)'s memory of it; the channel itself keeps its messages. A thread that continues starts with no history.")
        }
        .confirmationDialog("Clear all direct conversations with \(agentName)?", isPresented: $clearingAll) {
            Button("Clear All", role: .destructive) { Task { await store.clearAll() } }
        } message: {
            Text("Every conversation started from Stormo is deleted. Slack threads and scheduled runs are kept.")
        }
    }

    private var agentName: String {
        guard let id = model.chat.agent else { return "Conversations" }
        return model.fleet.fleet?.agents.first { $0.id == id }?.name ?? id
    }

    @ViewBuilder
    private func list(_ store: ChatStore) -> some View {
        if store.agent == nil {
            ContentUnavailableView("Choose an agent", systemImage: "person.crop.circle")
        } else if store.conversations.isEmpty && !store.loadingList {
            ContentUnavailableView {
                Label("No conversations", systemImage: "bubble.left.and.bubble.right")
            } description: {
                Text(store.problem?.kind == .notRunning ? "Start the agent to see its conversations." : "Write a message to start one.")
            }
        } else {
            List(selection: Binding(get: { store.selection }, set: { id in Task { await store.open(id) } })) {
                ForEach(store.conversations) { c in
                    ConversationRow(conversation: c, sending: store.pending?.conversation == c.id)
                        .tag(Optional(c.id))
                        .contextMenu {
                            Button("Clear Conversation…", systemImage: "trash", role: .destructive) { clearing = c }
                        }
                }
            }
            .listStyle(.inset)
            .onDeleteCommand { if let c = store.selected { clearing = c } }
        }
    }
}

private struct ConversationRow: View {
    let conversation: Conversation
    let sending: Bool

    var body: some View {
        VStack(alignment: .leading, spacing: 3) {
            HStack(alignment: .firstTextBaseline) {
                Text(conversation.displayTitle).fontWeight(.medium).lineLimit(1)
                Spacer(minLength: 4)
                if sending {
                    ProgressView().controlSize(.mini)
                } else if let d = conversation.activeDate {
                    Text(d, format: .relative(presentation: .numeric, unitsStyle: .narrow))
                        .font(.caption).foregroundStyle(.secondary)
                }
            }
            HStack(spacing: 6) {
                Label(conversation.sourceLabel, systemImage: symbol)
                    .labelStyle(.titleAndIcon)
                Text("\(conversation.messages) message\(conversation.messages == 1 ? "" : "s")")
            }
            .font(.caption)
            .foregroundStyle(.secondary)
        }
        .padding(.vertical, 3)
    }

    private var symbol: String {
        switch conversation.source {
        case nil, "api_server": "bubble.left"
        case "cron": "clock"
        case "slack": "number"
        default: "antenna.radiowaves.left.and.right"
        }
    }
}

// MARK: Transcript

private struct TranscriptPane: View {
    @Environment(AppModel.self) private var model
    @State private var draft = ""
    @FocusState private var composing: Bool

    var body: some View {
        let store = model.chat
        VStack(spacing: 0) {
            header(store)
            Divider()
            if let problem = store.problem {
                ProblemBanner(problem: problem, agent: store.agent)
                Divider()
            }
            messages(store)
            Divider()
            composer(store)
        }
        .background(.background)
        .onChange(of: store.selection) { composing = true }
    }

    private func header(_ store: ChatStore) -> some View {
        HStack(spacing: 8) {
            VStack(alignment: .leading, spacing: 1) {
                Text(store.selected?.displayTitle ?? "New conversation").font(.headline).lineLimit(1)
                if let c = store.selected, let d = c.startedAt.flatMap(Date.init(chatTime:)) {
                    Text("\(c.sourceLabel) · started \(d.formatted(date: .abbreviated, time: .shortened))")
                        .font(.caption).foregroundStyle(.secondary)
                }
            }
            Spacer()
            if store.transcript?.compacted == true {
                Label("Compacted", systemImage: "rectangle.compress.vertical")
                    .font(.caption)
                    .padding(.horizontal, 7).padding(.vertical, 3)
                    .background(.quaternary, in: Capsule())
                    .help("The conversation grew past the model's context threshold, so the agent automatically folded its older turns into a summary and carried on. Only the turns since then are shown.")
            }
        }
        .padding(.horizontal, 14)
        .frame(height: 38)
    }

    @ViewBuilder
    private func messages(_ store: ChatStore) -> some View {
        if store.agent == nil {
            ContentUnavailableView("Choose an agent", systemImage: "person.crop.circle")
                .frame(maxHeight: .infinity)
        } else if store.loadingTranscript && store.transcript == nil {
            ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity)
        } else {
            let rows = store.transcript?.messages ?? []
            let pending = store.visiblePending
            if rows.isEmpty && pending == nil {
                ContentUnavailableView {
                    Label(store.selection == nil ? "New conversation" : "No messages", systemImage: "bubble.left.and.text.bubble.right")
                } description: {
                    Text(store.selection == nil ? "Write to \(agentName) below. The conversation is kept by the agent until you clear it, and compacted automatically when it grows long." : "Nothing to show in this conversation yet.")
                }
                .frame(maxHeight: .infinity)
            } else {
                ScrollViewReader { proxy in
                    ScrollView {
                        LazyVStack(alignment: .leading, spacing: 10) {
                            if store.transcript?.compacted == true {
                                CompactionMarker()
                            }
                            ForEach(rows) { m in
                                MessageRow(message: m, agentName: agentName)
                            }
                            if let pending, !rows.contains(where: { $0.isUser && $0.content == pending.text }) {
                                MessageRow(message: ChatMessage(id: "pending", role: "user", content: pending.text), agentName: agentName)
                            }
                            if let pending {
                                ThinkingRow(name: agentName, since: pending.started) { store.cancelSend() }
                            }
                            Color.clear.frame(height: 1).id("bottom")
                        }
                        .padding(14)
                        .textSelection(.enabled)
                    }
                    .defaultScrollAnchor(.bottom)
                    .onChange(of: rows.count) { withAnimation { proxy.scrollTo("bottom", anchor: .bottom) } }
                    .onChange(of: pending?.text) { withAnimation { proxy.scrollTo("bottom", anchor: .bottom) } }
                }
            }
        }
    }

    @ViewBuilder
    private func composer(_ store: ChatStore) -> some View {
        if store.canSend {
            HStack(alignment: .bottom, spacing: 8) {
                TextField("Message \(agentName)", text: $draft, axis: .vertical)
                    .textFieldStyle(.plain)
                    .lineLimit(1...8)
                    .focused($composing)
                    .onSubmit(send)
                    .padding(.horizontal, 10).padding(.vertical, 7)
                    .background(.quaternary.opacity(0.6), in: RoundedRectangle(cornerRadius: 9))
                    .disabled(store.agent == nil)
                Button(action: send) {
                    Image(systemName: "arrow.up.circle.fill").font(.title2)
                }
                .buttonStyle(.borderless)
                .keyboardShortcut(.return, modifiers: .command)
                .disabled(!canSubmit(store))
                .help("Send (Return; ⌥Return for a new line)")
            }
            .padding(10)
        } else {
            HStack {
                Image(systemName: "lock")
                Text("This \(store.selected?.sourceLabel ?? "") conversation belongs to its channel. Reply there, or start a new conversation here.")
                Spacer()
                Button("New Conversation") { Task { await store.open(nil) } }
            }
            .font(.callout)
            .foregroundStyle(.secondary)
            .padding(12)
        }
    }

    private func canSubmit(_ store: ChatStore) -> Bool {
        store.agent != nil && !store.isSending && !draft.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
    }

    private func send() {
        let store = model.chat
        guard canSubmit(store) else { return }
        store.backend = model.cli
        store.send(draft)
        draft = ""
    }

    private var agentName: String {
        guard let id = model.chat.agent else { return "the agent" }
        return model.fleet.fleet?.agents.first { $0.id == id }?.name ?? id
    }
}

private struct MessageRow: View {
    let message: ChatMessage
    let agentName: String
    @State private var expanded = false

    var body: some View {
        if message.isTool {
            tool
        } else if message.isUser {
            HStack {
                Spacer(minLength: 60)
                Text(markdown)
                    .padding(.horizontal, 12).padding(.vertical, 8)
                    .background(Color.accentColor.opacity(0.18), in: RoundedRectangle(cornerRadius: 12))
            }
        } else {
            VStack(alignment: .leading, spacing: 3) {
                Text(agentName).font(.caption).foregroundStyle(.secondary)
                Text(markdown)
                    .padding(.horizontal, 12).padding(.vertical, 8)
                    .background(.quaternary.opacity(0.5), in: RoundedRectangle(cornerRadius: 12))
            }
            .padding(.trailing, 60)
        }
    }

    /// A tool call (the assistant asking) or its output, folded to one line.
    private var tool: some View {
        VStack(alignment: .leading, spacing: 4) {
            Button {
                expanded.toggle()
            } label: {
                HStack(spacing: 5) {
                    Image(systemName: message.role == "tool" ? "arrow.turn.down.right" : "wrench.and.screwdriver")
                    Text(toolLine).lineLimit(1)
                    if !message.content.isEmpty {
                        Image(systemName: expanded ? "chevron.down" : "chevron.right").font(.caption2)
                    }
                }
                .font(.caption)
                .foregroundStyle(.secondary)
            }
            .buttonStyle(.plain)
            .disabled(message.content.isEmpty)
            if expanded {
                Text(message.content)
                    .font(.caption.monospaced())
                    .padding(8)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .background(.quaternary.opacity(0.4), in: RoundedRectangle(cornerRadius: 6))
            }
        }
    }

    private var toolLine: String {
        if message.role == "tool" { return "\(message.tool ?? "tool") returned" }
        let names = message.tools ?? []
        return names.isEmpty ? "Used a tool" : "Using \(names.joined(separator: ", "))"
    }

    private var markdown: AttributedString {
        (try? AttributedString(markdown: message.content, options: .init(interpretedSyntax: .inlineOnlyPreservingWhitespace)))
            ?? AttributedString(message.content)
    }
}

private struct ThinkingRow: View {
    let name: String
    let since: Date
    let cancel: () -> Void

    var body: some View {
        HStack(spacing: 8) {
            ProgressView().controlSize(.small)
            TimelineView(.periodic(from: since, by: 1)) { context in
                Text("\(name) is working · \(Duration.seconds(context.date.timeIntervalSince(since)).formatted(.time(pattern: .minuteSecond)))")
                    .monospacedDigit()
            }
            .font(.callout)
            .foregroundStyle(.secondary)
            Button("Stop waiting", action: cancel)
                .buttonStyle(.link)
                .font(.callout)
        }
    }
}

private struct CompactionMarker: View {
    var body: some View {
        HStack(spacing: 8) {
            VStack { Divider() }
            Label("Earlier turns compacted into a summary", systemImage: "rectangle.compress.vertical")
                .font(.caption)
                .foregroundStyle(.secondary)
                .fixedSize()
            VStack { Divider() }
        }
        .help("The agent compacts a conversation automatically when it nears the model's context limit. It still remembers the summary.")
    }
}

private struct ProblemBanner: View {
    @Environment(AppModel.self) private var model
    let problem: ChatProblem
    let agent: String?

    var body: some View {
        HStack(spacing: 8) {
            Image(systemName: problem.kind == .busy ? "hourglass" : "exclamationmark.triangle.fill")
                .foregroundStyle(problem.kind == .busy ? Color.secondary : Color.orange)
            Text(problem.message).font(.callout).lineLimit(3)
            Spacer()
            if problem.kind == .notRunning, let agent {
                Button("Start Agent") {
                    Task {
                        await model.run("start", agents: [agent])
                        model.chat.problem = nil
                        await model.chat.refreshList()
                    }
                }
                .disabled(model.cli == nil)
            } else {
                Button("Retry") {
                    Task {
                        model.chat.problem = nil
                        await model.chat.refreshList()
                        if let id = model.chat.selection { await model.chat.open(id) }
                    }
                }
            }
            Button("Dismiss", systemImage: "xmark") { model.chat.problem = nil }
                .labelStyle(.iconOnly)
                .buttonStyle(.borderless)
        }
        .padding(.horizontal, 14).padding(.vertical, 8)
        .background(.orange.opacity(0.08))
    }
}
