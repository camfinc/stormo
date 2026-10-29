import StormoKit
import SwiftUI

/// Choices by name from an agent's Slack directory (the core reads it with the agent's bot token).
enum SlackChoices {
    struct Choice: Identifiable, Hashable {
        let id: String
        let label: String
        let group: String
    }

    /// Menu sections, in order.
    static let groups = ["People", "Guests", "Other agents and bots"]

    static func people(_ d: SlackDirectory) -> [Choice] {
        d.humans.filter { !$0.isGuest }.map { Choice(id: $0.id, label: $0.label, group: groups[0]) }
            + d.humans.filter(\.isGuest).map { Choice(id: $0.id, label: $0.label, group: groups[1]) }
            + d.bots.map { Choice(id: $0.id, label: $0.name, group: groups[2]) }
    }
}

/// One line at the top of a Slack channel: whose workspace the pickers read, or why there is none.
struct SlackDirectoryStatus: View {
    @Environment(AppModel.self) private var model
    let agentID: String

    var body: some View {
        let store = model.slack
        HStack {
            if store.loading.contains(agentID) {
                ProgressView().controlSize(.small)
                Text("Reading the agent's Slack workspace…").foregroundStyle(.secondary)
            } else if let d = store.directory(agentID) {
                Label("\(d.team.name): \(d.humans.count) people, \(d.channels.count) channels", systemImage: "person.2")
                    .foregroundStyle(.secondary)
                if let partial = d.partial, !partial.isEmpty {
                    Image(systemName: "exclamationmark.triangle").foregroundStyle(.orange).help(partial.joined(separator: "\n"))
                }
            } else if let problem = store.problems[agentID] {
                Label(problem, systemImage: "person.crop.circle.badge.questionmark").foregroundStyle(.secondary)
            } else {
                Text("Start the core to pick people and channels by name.").foregroundStyle(.secondary)
            }
            Spacer()
            Button("Refresh", systemImage: "arrow.clockwise") { Task { await store.load(agentID, refresh: true) } }
                .labelStyle(.iconOnly).buttonStyle(.borderless)
                .disabled(store.loading.contains(agentID) || !model.core.state.isRunning)
                .help("Read the workspace again")
        }
        .font(.callout)
        .task(id: TaskKey(agent: agentID, running: model.core.state.isRunning)) {
            if model.core.state.isRunning { await store.load(agentID) }
        }
    }

    private struct TaskKey: Hashable {
        let agent: String
        let running: Bool
    }
}

/// A Slack id or delivery target, typed or picked by name: a channel the bot is in, another public
/// channel, or the bot's DM with a person (opened through the core, which posts nothing). A delivery
/// field writes `slack:<id>` and also offers the home channel and `local`. Without a directory it is
/// the plain text field it always was.
struct SlackTargetField: View {
    @Environment(AppModel.self) private var model
    let title: String
    @Binding var text: String
    let prompt: String
    let agentID: String
    /// A schedule's deliver / on_failure (a target) rather than a channel id.
    let delivery: Bool
    @State private var opening = false
    @State private var problem: String?

    init(title: String, text: Binding<String>, prompt: String, agentID: String, delivery: Bool) {
        self.title = title
        _text = text
        self.prompt = prompt
        self.agentID = agentID
        self.delivery = delivery
    }

    var body: some View {
        let directory = model.slack.directory(agentID)
        LabeledContent(title) {
            VStack(alignment: .trailing, spacing: 2) {
                HStack {
                    TextField(title, text: $text, prompt: Text(prompt))
                        .labelsHidden()
                        .multilineTextAlignment(.trailing)
                    if opening { ProgressView().controlSize(.small) }
                    if let directory { menu(directory) }
                }
                if let words = described(directory) {
                    Text(words).font(.caption).foregroundStyle(.secondary)
                }
                if let problem {
                    Text(problem).font(.caption).foregroundStyle(.orange)
                }
            }
        }
    }

    private func described(_ d: SlackDirectory?) -> String? {
        let value = text.trimmingCharacters(in: .whitespaces)
        guard let d, !value.isEmpty else { return nil }
        let words = delivery ? d.describe(target: value) : d.label(for: value)
        return words == value ? nil : words
    }

    private func menu(_ d: SlackDirectory) -> some View {
        Menu {
            if delivery {
                Button("The home channel") { pick("slack") }
                Button("Nowhere (recorded only)") { pick("local") }
                Divider()
            }
            let joined = d.channels.filter(\.member)
            if !joined.isEmpty {
                Section("Channels the bot is in") {
                    ForEach(joined) { c in Button(channelLabel(c)) { pick(c.id) } }
                }
            }
            let others = d.channels.filter { !$0.member }
            if !others.isEmpty {
                Menu("Other channels (invite the bot first)") {
                    ForEach(others) { c in Button(channelLabel(c)) { pick(c.id) } }
                }
            }
            Menu("A direct message with…") {
                ForEach(d.humans) { p in Button(p.label) { dm(p.id) } }
            }
        } label: {
            Image(systemName: "number.circle")
        }
        .menuStyle(.borderlessButton)
        .fixedSize()
        .disabled(opening)
        .help("Pick a channel or a person from the agent's Slack workspace")
    }

    private func channelLabel(_ c: SlackDirectory.Channel) -> String {
        c.isPrivate ? "🔒 \(c.name)" : "#\(c.name)"
    }

    /// Writes a channel id (a delivery: `slack:<id>`; `slack` and `local` as they are).
    private func pick(_ id: String) {
        problem = nil
        text = delivery && id != "slack" && id != "local" ? "slack:\(id)" : id
    }

    private func dm(_ user: String) {
        problem = nil
        opening = true
        Task {
            defer { opening = false }
            do {
                pick(try await model.slack.dm(agent: agentID, user: user))
            } catch {
                problem = "Could not open the DM: \(error.localizedDescription)"
            }
        }
    }
}
