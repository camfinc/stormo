import StormoKit
import SwiftUI

enum SidebarItem: String, CaseIterable, Identifiable {
    case office, agents, core
    var id: String { rawValue }

    var title: String {
        switch self {
        case .office: "Office"
        case .agents: "Agents"
        case .core: "Core"
        }
    }

    var symbol: String {
        switch self {
        case .office: "building.2"
        case .agents: "person.3"
        case .core: "server.rack"
        }
    }
}

struct MainView: View {
    @Environment(AppModel.self) private var model
    @State private var section: SidebarItem? = .agents
    @State private var importing = false
    @State private var creating = false

    var body: some View {
        @Bindable var model = model
        Group {
            if model.instances.active == nil {
                WelcomeView(newInstance: { creating = true }, openInstance: { importing = true })
            } else {
                NavigationSplitView {
                    List(SidebarItem.allCases, selection: $section) { s in
                        Label(s.title, systemImage: s.symbol).tag(s)
                    }
                    .navigationSplitViewColumnWidth(min: 160, ideal: 190)
                } detail: {
                    switch section ?? .agents {
                    case .office: OfficePlaceholder()
                    case .agents: AgentsView()
                    case .core: CoreView()
                    }
                }
                .toolbar { MainToolbar(newInstance: { creating = true }, openInstance: { importing = true }) }
            }
        }
        .frame(minWidth: 820, minHeight: 520)
        .focusedSceneValue(\.openInstance, { importing = true })
        .focusedSceneValue(\.newInstance, { creating = true })
        .sheet(isPresented: $creating) { NewInstanceSheet().environment(model) }
        .fileImporter(isPresented: $importing, allowedContentTypes: [.folder]) { result in
            if case .success(let url) = result { Task { await model.open(folder: url) } }
        }
        .alert("Stormo", isPresented: Binding(get: { model.failure != nil }, set: { if !$0 { model.failure = nil } })) {
            Button("OK", role: .cancel) {}
        } message: {
            Text(model.failure ?? "")
        }
    }
}

struct MainToolbar: ToolbarContent {
    @Environment(AppModel.self) private var model
    let newInstance: () -> Void
    let openInstance: () -> Void

    var body: some ToolbarContent {
        ToolbarItem(placement: .navigation) {
            Menu {
                ForEach(model.instances.instances) { record in
                    Button(record.name) { Task { await model.activate(record) } }
                }
                Divider()
                Button("New Instance…", action: newInstance)
                Button("Open Instance…", action: openInstance)
            } label: {
                Label(model.instances.active?.name ?? "Instance", systemImage: "square.stack.3d.up")
                    .labelStyle(.titleAndIcon)
            }
        }
        ToolbarItem(placement: .status) {
            if let activity = model.activity {
                HStack(spacing: 6) {
                    ProgressView().controlSize(.small)
                    Text(activity).font(.callout).foregroundStyle(.secondary).lineLimit(1)
                }
            } else {
                CoreStatusBadge(state: model.core.state)
            }
        }
        ToolbarItem(placement: .primaryAction) {
            CoreToggleButton()
        }
    }
}

struct CoreToggleButton: View {
    @Environment(AppModel.self) private var model

    var body: some View {
        switch model.core.state {
        case .running:
            if model.core.isOutdated(comparedTo: model.pinned?.info) {
                RestartCoreButton(title: "Restart Core to Update")
            } else {
                Button("Stop Core", systemImage: "stop.circle") { Task { await model.stopCore() } }
            }
        case .down:
            Button("Start Core", systemImage: "play.circle") { Task { await model.startCore() } }
                .disabled(model.cli == nil)
        case .incompatible:
            RestartCoreButton(title: "Restart Core")
        case .starting, .stopping:
            ProgressView().controlSize(.small)
        default:
            EmptyView()
        }
    }
}

/// core down + core up with the pinned binary: how a too-old or outdated core gets replaced.
struct RestartCoreButton: View {
    @Environment(AppModel.self) private var model
    var title = "Restart Core"

    var body: some View {
        Button(title, systemImage: "arrow.clockwise.circle") { Task { await model.restartCore() } }
            .disabled(model.cli == nil)
            .help("Stops the core and starts it again with \(model.pinned?.resolved.lastPathComponent ?? "the pinned stormo"). Agents keep running; model calls pause for a few seconds.")
    }
}

struct CoreStatusBadge: View {
    let state: CoreState

    var body: some View {
        Label {
            Text(text)
        } icon: {
            Circle().fill(color).frame(width: 8, height: 8)
        }
        .labelStyle(.titleAndIcon)
        .font(.callout)
        .foregroundStyle(.secondary)
    }

    private var text: String {
        switch state {
        case .unknown: "Checking core…"
        case .down: "Core stopped"
        case .starting: "Starting core…"
        case .stopping: "Stopping core…"
        case .running: "Core running"
        case .otherInstance(let c): "Core belongs to \(c.instance.name)"
        case .incompatible(let v, _): v.map { "Core \($0) is too old" } ?? "Core is too old"
        }
    }

    private var color: Color {
        switch state {
        case .running: .green
        case .starting, .stopping, .unknown: .yellow
        case .down: .secondary
        case .otherInstance, .incompatible: .orange
        }
    }
}

struct WelcomeView: View {
    let newInstance: () -> Void
    let openInstance: () -> Void

    var body: some View {
        ContentUnavailableView {
            Label("Welcome to Stormo", systemImage: "tornado")
        } description: {
            Text("Create an instance for your organisation's agents, or open one you already have (a folder with a stormo.yaml).")
        } actions: {
            Button("New Instance…", action: newInstance)
                .buttonStyle(.borderedProminent)
            Button("Open Instance…", action: openInstance)
        }
    }
}

struct OfficePlaceholder: View {
    var body: some View {
        ContentUnavailableView("The office is coming", systemImage: "building.2",
                               description: Text("Until the native office lands, Agents shows the same fleet."))
    }
}
