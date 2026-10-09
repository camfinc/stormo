import StormoKit
import SwiftUI

struct AgentsView: View {
    @Environment(AppModel.self) private var model
    @State private var selection = Set<FleetAgent.ID>()
    @State private var inspecting = true

    private var agents: [FleetAgent] { model.fleet.fleet?.agents ?? [] }

    var body: some View {
        Group {
            if !model.core.state.isRunning {
                CoreNotRunningView()
            } else if agents.isEmpty {
                ContentUnavailableView("No agents yet", systemImage: "person.3",
                                       description: Text("Agents appear here once the core has polled the fleet."))
            } else {
                Table(agents, selection: $selection) {
                    TableColumn("Agent") { a in
                        HStack(spacing: 8) {
                            AgentBadge(agent: a)
                            VStack(alignment: .leading, spacing: 1) {
                                Text(a.name).fontWeight(.medium)
                                if let role = a.role { Text(role).font(.caption).foregroundStyle(.secondary) }
                            }
                        }
                    }
                    .width(min: 160, ideal: 220)
                    TableColumn("Unit") { a in Text(unitName(a.unit)) }
                        .width(min: 80, ideal: 120)
                    TableColumn("Status") { a in
                        HStack(spacing: 6) {
                            StateDot(agent: a)
                            Text(a.statusLine).lineLimit(1)
                        }
                    }
                    .width(min: 120, ideal: 180)
                    TableColumn("Last nap") { a in
                        if let d = a.lastNap.flatMap(Date.init(isoString:)) {
                            Text(d, format: .relative(presentation: .named))
                        } else {
                            Text("—").foregroundStyle(.tertiary)
                        }
                    }
                    .width(min: 80, ideal: 110)
                    TableColumn("To review") { a in
                        let n = (a.pendingLearnings ?? 0) + (a.pendingSkills ?? 0)
                        Text(n > 0 ? "\(n)" : "—").foregroundStyle(n > 0 ? .primary : .tertiary)
                    }
                    .width(min: 60, ideal: 80)
                    TableColumn("Model") { a in Text(a.localModel ?? a.model ?? "—").foregroundStyle(.secondary) }
                }
                .contextMenu(forSelectionType: FleetAgent.ID.self) { ids in
                    AgentActions(ids: Array(ids))
                }
                .inspector(isPresented: $inspecting) {
                    if selection.count == 1, let id = selection.first, let agent = model.fleet.agent(id) {
                        AgentInspector(agent: agent)
                    } else {
                        ContentUnavailableView("Select an agent", systemImage: "person.crop.circle")
                    }
                }
            }
        }
        .navigationTitle("Agents")
        .toolbar {
            ToolbarItem {
                Button("Inspector", systemImage: "sidebar.trailing") { inspecting.toggle() }
            }
        }
    }

    private func unitName(_ id: String) -> String {
        model.fleet.fleet?.units.first { $0.id == id }?.name ?? id
    }
}

/// Start, stop, restart and nap now for the given agents.
struct AgentActions: View {
    @Environment(AppModel.self) private var model
    let ids: [String]

    var body: some View {
        if !ids.isEmpty {
            Button("Start") { Task { await model.run("start", agents: ids) } }
            Button("Stop") { Task { await model.run("stop", agents: ids) } }
            Button("Restart") { Task { await model.run("restart", agents: ids) } }
            if ids.count == 1 {
                Button("Nap Now") { Task { await model.run("nap-now", agents: ids) } }
            }
            Divider()
            Button("Copy Command") {
                NSPasteboard.general.clearContents()
                NSPasteboard.general.setString("stormo restart \(ids.joined(separator: " "))", forType: .string)
            }
        }
    }
}

struct AgentInspector: View {
    let agent: FleetAgent

    var body: some View {
        Form {
            Section {
                HStack(spacing: 12) {
                    AgentBadge(agent: agent, size: 64)
                    VStack(alignment: .leading) {
                        Text(agent.name).font(.title3).fontWeight(.semibold)
                        if let role = agent.role { Text(role).foregroundStyle(.secondary) }
                    }
                }
            }
            Section("Now") {
                LabeledContent("Status") {
                    HStack(spacing: 6) { StateDot(agent: agent); Text(agent.statusLine) }
                }
                LabeledContent("Container", value: agent.state)
                if let h = agent.health { LabeledContent("Health", value: h) }
                if let d = agent.detail, !d.isEmpty { LabeledContent("Detail", value: d) }
                if let jobs = agent.activity?.runningJobs, !jobs.isEmpty {
                    LabeledContent("Jobs", value: jobs.joined(separator: ", "))
                }
                ForEach(agent.activity?.platforms?.sorted(by: { $0.key < $1.key }) ?? [], id: \.key) { name, p in
                    LabeledContent(name.capitalized) {
                        Text(p.state).foregroundStyle(p.needsAttention ? .red : .secondary)
                    }
                }
            }
            Section("Agent") {
                LabeledContent("Unit", value: agent.unit)
                LabeledContent("Model", value: agent.model ?? "—")
                if let m = agent.localModel { LabeledContent("Local model", value: m) }
                if let e = agent.endpoint { LabeledContent("API", value: e) }
                if let n = agent.napIntervalSeconds { LabeledContent("Nap every", value: Duration.seconds(n).formatted(.units(allowed: [.hours, .minutes]))) }
                LabeledContent("To review", value: "\((agent.pendingLearnings ?? 0)) learnings, \((agent.pendingSkills ?? 0)) skills")
            }
            Section {
                HStack {
                    AgentActionsBar(id: agent.id, running: agent.state == "running")
                }
            }
        }
        .formStyle(.grouped)
        .inspectorColumnWidth(min: 260, ideal: 300)
    }
}

struct AgentActionsBar: View {
    @Environment(AppModel.self) private var model
    let id: String
    let running: Bool

    var body: some View {
        if running {
            Button("Restart") { Task { await model.run("restart", agents: [id]) } }
            Button("Nap Now") { Task { await model.run("nap-now", agents: [id]) } }
            Button("Stop", role: .destructive) { Task { await model.run("stop", agents: [id]) } }
        } else {
            Button("Start") { Task { await model.run("start", agents: [id]) } }
                .buttonStyle(.borderedProminent)
        }
    }
}

/// The agent's portrait (/avatars/<id>.png); initials on its shirt colour until one loads, or when
/// the persona has no baked avatar.
struct AgentBadge: View {
    @Environment(AppModel.self) private var model
    let agent: FleetAgent
    var size: CGFloat = 28

    var body: some View {
        Group {
            if let portrait = model.avatars[agent.id] {
                Image(nsImage: portrait)
                    .resizable()
                    .interpolation(.high)
                    .scaledToFill()
            } else {
                Text(String(agent.name.prefix(1)))
                    .font(.system(size: size * 0.45, weight: .semibold, design: .rounded))
                    .foregroundStyle(.white)
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
                    .background(Color(hex: agent.sprite?.shirt) ?? .accentColor)
            }
        }
        .frame(width: size, height: size)
        .clipShape(Circle())
        .overlay(Circle().strokeBorder(.separator, lineWidth: 0.5))
        .accessibilityHidden(true)
    }
}

struct StateDot: View {
    let agent: FleetAgent

    var body: some View {
        Circle()
            .fill(color)
            .frame(width: 8, height: 8)
            .accessibilityLabel(agent.statusLine)
    }

    private var color: Color {
        if agent.needsAttention { return .red }
        switch agent.state {
        case "running": return agent.office?.working == true ? .green : .teal
        case "starting", "restarting": return .yellow
        default: return .secondary
        }
    }
}

struct CoreNotRunningView: View {
    @Environment(AppModel.self) private var model

    var body: some View {
        ContentUnavailableView {
            switch model.core.state {
            case .incompatible: Label("The core needs a restart", systemImage: "arrow.clockwise.circle")
            case .otherInstance: Label("Another instance's core is running", systemImage: "server.rack")
            default: Label("The core is not running", systemImage: "server.rack")
            }
        } description: {
            switch model.core.state {
            case .otherInstance(let c): Text("The core on this Mac belongs to \(c.instance.name). One core runs at a time.")
            case .incompatible: Text("The running core is older than this app. Restarting it with the pinned stormo takes a few seconds; agents keep running.")
            default: Text("Start the core to see the fleet.")
            }
        } actions: {
            CoreToggleButton()
                .buttonStyle(.borderedProminent)
        }
    }
}

extension Color {
    /// #rrggbb, as personas give sprite colours.
    init?(hex: String?) {
        guard let hex, hex.count == 7, hex.hasPrefix("#"), let v = Int(hex.dropFirst(), radix: 16) else { return nil }
        self.init(red: Double((v >> 16) & 0xff) / 255, green: Double((v >> 8) & 0xff) / 255, blue: Double(v & 0xff) / 255)
    }
}

extension Date {
    /// The core's timestamps: RFC 3339 with or without fractional seconds.
    init?(isoString s: String) {
        if let d = try? Date(s, strategy: .iso8601.year().month().day().time(includingFractionalSeconds: true)) {
            self = d
        } else if let d = try? Date(s, strategy: .iso8601) {
            self = d
        } else {
            return nil
        }
    }
}
