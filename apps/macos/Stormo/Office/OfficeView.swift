import AppKit
import SpriteKit
import StormoKit
import SwiftUI

/// The office: the fleet on a floor plan, live (SpriteKit), or over time (the timeline), with the
/// inspector beside it.
struct OfficeView: View {
    @Environment(AppModel.self) private var model
    @Environment(\.accessibilityReduceMotion) private var reduceMotion
    @State private var scene = OfficeScene(size: CGSize(width: 1200, height: 800))
    @State private var selection: OfficeSelection?
    /// The agent whose recent activity is open (a double-click, or Return).
    @State private var opened: OpenedAgent?
    @State private var mode = OfficeViewMode.saved

    /// The timeline's chosen agent is the floor's: the inspector follows either.
    private var chosenAgent: Binding<String?> {
        Binding {
            if case .agent(let id) = selection { id } else { nil }
        } set: { id in
            select(id.map { .agent($0) })
        }
    }

    var body: some View {
        Group {
            if model.core.state.isRunning, let fleet = model.fleet.fleet {
                // The counts sit under the floor, not over it: nothing on the floor is covered.
                VStack(spacing: 0) {
                    switch mode {
                    case .floor:
                        OfficeSpriteView(scene: scene)
                            .accessibilityElement(children: .contain)
                            .accessibilityLabel("Office floor")
                            .accessibilityChildren { OfficeAccessibleFloor(fleet: fleet, look: OfficeLook(instance: model.fleet.look), select: { select(.agent($0)) }) }
                    case .timeline:
                        OfficeTimelineView(fleet: fleet, selection: chosenAgent, open: { opened = OpenedAgent(id: $0) })
                    }
                    OfficeHUD(fleet: fleet)
                }
                    .onAppear {
                        configure()
                        push()
                    }
                    .onChange(of: model.fleet.fleet) { push() }
                    .onChange(of: model.fleet.look) { push() }
                    .onChange(of: model.avatars.count) { push() }
                    .onChange(of: reduceMotion) { scene.reduceMotion = reduceMotion }
            } else {
                CoreNotRunningView()
            }
        }
        .navigationTitle("Office")
        .sheet(item: $opened) { o in AgentActivitySheet(agentID: o.id) }
        // Always open: an inspector that came and went would resize the floor (and refit its zoom)
        // on every click. With nothing selected it gives the overview.
        .inspector(isPresented: .constant(true)) {
            inspector.inspectorColumnWidth(min: 260, ideal: 300)
        }
        .onKeyPress(.escape) {
            guard selection != nil else { return .ignored }
            select(nil)
            return .handled
        }
        .toolbar {
            ToolbarItem {
                Picker("View", selection: $mode) {
                    ForEach(OfficeViewMode.allCases) { Label($0.title, systemImage: $0.symbol).tag($0) }
                }
                .pickerStyle(.segmented)
                .help("The floor now, or everyone's work over time")
                .onChange(of: mode) { mode.save() }
            }
            ToolbarItemGroup {
                // The floor's camera: nothing to zoom on the timeline.
                Group {
                    Button("Zoom Out", systemImage: "minus.magnifyingglass") { scene.zoom(by: 1 / 1.25) }
                        .keyboardShortcut("-")
                    Button("Fit", systemImage: "arrow.up.left.and.arrow.down.right") { scene.resetZoom() }
                        .keyboardShortcut("0")
                    Button("Zoom In", systemImage: "plus.magnifyingglass") { scene.zoom(by: 1.25) }
                        .keyboardShortcut("=")
                }
                .disabled(mode != .floor)
            }
        }
    }

    private func configure() {
        scene.reduceMotion = reduceMotion
        scene.onSelect = { selection = $0 }
        scene.onOpen = { opened = OpenedAgent(id: $0) }
        scene.menuForAgent = { id in agentMenu(id) }
    }

    private func push() {
        guard let fleet = model.fleet.fleet else { return }
        scene.apply(fleet: fleet, look: OfficeLook(instance: model.fleet.look), clockOffset: model.fleet.clockOffset, avatars: model.avatars)
        if case .agent(let id) = selection, !fleet.agents.contains(where: { $0.id == id }) { select(nil) }
    }

    private func select(_ s: OfficeSelection?) {
        selection = s
        scene.selection = s
    }

    @ViewBuilder private var inspector: some View {
        switch selection {
        case .agent(let id):
            if let agent = model.fleet.agent(id) { AgentInspector(agent: agent) }
        case .core:
            CorePanel()
        case .robot:
            RobotPanel()
        case .vacant(let unit):
            ContentUnavailableView {
                Label("Open desk", systemImage: "chair")
            } description: {
                Text("Nobody works here yet. An agent whose manifest says `unit: \(unit)` gets this desk.")
            }
        case nil:
            OfficeOverview(select: { select(.agent($0)) })
        }
    }

    /// Right-click on a person: the lifecycle actions.
    private func agentMenu(_ id: String) -> NSMenu {
        let menu = NSMenu()
        let running = model.fleet.agent(id)?.state == "running"
        let name = model.fleet.agent(id)?.name ?? id
        menu.addItem(withTitle: name, action: nil, keyEquivalent: "").isEnabled = false
        menu.addItem(.separator())
        let actions: [(String, String)] = running ? [("Restart", "restart"), ("Nap Now", "nap-now"), ("Stop", "stop")] : [("Start", "start")]
        for (title, action) in actions {
            menu.addItem(MenuActions.item(title) { Task { await model.run(action, agents: [id]) } })
        }
        menu.addItem(.separator())
        menu.addItem(MenuActions.item("Show Details") { select(.agent(id)) })
        menu.addItem(MenuActions.item("Recent Activity…") { opened = OpenedAgent(id: id) })
        return menu
    }
}

/// Menu items that run a closure (the closure rides in representedObject).
final class MenuActions: NSObject {
    static let shared = MenuActions()

    static func item(_ title: String, _ handler: @escaping () -> Void) -> NSMenuItem {
        let item = NSMenuItem(title: title, action: #selector(run(_:)), keyEquivalent: "")
        item.target = shared
        item.representedObject = handler
        return item
    }

    @objc func run(_ sender: NSMenuItem) { (sender.representedObject as? () -> Void)?() }
}

/// The SpriteKit view, with scroll/pinch zoom, drag to pan and a pointing hand over things that click.
struct OfficeSpriteView: NSViewRepresentable {
    let scene: OfficeScene

    func makeNSView(context: Context) -> OfficeSKView {
        let v = OfficeSKView()
        v.ignoresSiblingOrder = true
        v.preferredFramesPerSecond = 60
        v.presentScene(scene)
        return v
    }

    func updateNSView(_ v: OfficeSKView, context: Context) {
        if v.scene !== scene { v.presentScene(scene) }
    }
}

final class OfficeSKView: SKView {
    private var office: OfficeScene? { scene as? OfficeScene }

    override var acceptsFirstResponder: Bool { true }

    /// A wheel zooms around the pointer. A trackpad (or a touch mouse) moves the floor once it is
    /// zoomed in, as two fingers do everywhere else; with the whole floor in view there is nowhere
    /// to move, so it zooms, and with ⌘ held it always does. Dragging open floor pans too.
    override func scrollWheel(with e: NSEvent) {
        guard let office else { return }
        if e.hasPreciseScrollingDeltas, office.isZoomed, !e.modifierFlags.contains(.command) {
            office.pan(dx: e.scrollingDeltaX, dy: e.scrollingDeltaY)
            return
        }
        let step: CGFloat = e.hasPreciseScrollingDeltas ? 0.005 : 0.08
        office.zoom(by: exp(e.scrollingDeltaY * step), at: e.location(in: office))
    }

    override func magnify(with e: NSEvent) {
        guard let office else { return }
        office.zoom(by: 1 + e.magnification, at: e.location(in: office))
    }
    override func smartMagnify(with e: NSEvent) { office?.resetZoom() }

    override func updateTrackingAreas() {
        super.updateTrackingAreas()
        trackingAreas.forEach(removeTrackingArea)
        addTrackingArea(NSTrackingArea(rect: bounds, options: [.mouseMoved, .mouseEnteredAndExited, .activeInKeyWindow, .inVisibleRect], owner: self))
    }

    override func mouseMoved(with e: NSEvent) {
        guard let office else { return }
        (office.hover(at: e.location(in: office)) ? NSCursor.pointingHand : NSCursor.openHand).set()
    }

    override func mouseExited(with e: NSEvent) { _ = office?.hover(at: nil) }

    /// Debug snapshots: the whole floor as the scene draws it.
    func floorImage() -> CGImage? {
        guard let office, let plan = office.plan, let world = office.children.first else { return nil }
        return texture(from: world, crop: CGRect(origin: .zero, size: plan.size))?.cgImage()
    }
}

/// The fleet's counts over the floor.
struct OfficeHUD: View {
    let fleet: Fleet

    var body: some View {
        let s = FleetSummary(fleet)
        let plan = OfficeLook.plan(fleet)
        HStack(spacing: 14) {
            Meter(label: "On shift", value: "\(s.onShift)/\(s.total)", color: s.onShift > 0 ? OfficePalette.ok : OfficePalette.off)
            Meter(label: "Working", value: "\(s.working)", color: s.working > 0 ? OfficePalette.busy : nil)
            Meter(label: "Idle", value: "\(s.idle)", color: s.idle > 0 ? OfficePalette.ok : nil)
            Meter(label: "To review", value: "\(s.reviewItems)", color: s.reviewItems > 0 ? OfficePalette.gold : nil)
            if s.attention > 0 { Meter(label: "Attention", value: "\(s.attention)", color: OfficePalette.bad) }
            switch plan {
            case .unused: EmptyView()
            case .online: Meter(label: "Using ChatGPT plan", value: "", color: plan.color)
            case .signedOut(let why): Meter(label: "ChatGPT plan", value: why, color: plan.color)
            case .limited: Meter(label: "ChatGPT plan", value: "limited", color: plan.color)
            }
        }
        .lineLimit(1)
        .minimumScaleFactor(0.8)
        .padding(.horizontal, 16)
        .padding(.vertical, 7)
        .frame(maxWidth: .infinity)
        .background(.bar)
        .overlay(alignment: .top) { Divider() }
        .accessibilityElement(children: .combine)
    }

    private struct Meter: View {
        let label: String
        let value: String
        let color: String?

        var body: some View {
            HStack(spacing: 6) {
                Circle().fill(color.flatMap { Color(hex: $0) } ?? .secondary).frame(width: 7, height: 7)
                Text(label).foregroundStyle(.secondary)
                if !value.isEmpty { Text(value).monospacedDigit().fontWeight(.semibold) }
            }
            .font(.callout)
        }
    }
}

struct OpenedAgent: Identifiable, Equatable {
    let id: String
}

/// The floor for VoiceOver: each room with its counts, then its people as buttons.
struct OfficeAccessibleFloor: View {
    let fleet: Fleet
    let look: OfficeLook
    let select: (String) -> Void

    var body: some View {
        let now = fleet.now
        ForEach(fleet.units) { unit in
            let people = fleet.agents.filter { $0.unit == unit.id }
            Text("\(look.shortName(unit.name)): \(RoomSummary(people).words)")
            ForEach(people) { a in
                Button("\(a.name), \(OfficeLook.statusText(a, serverNow: now))\(a.toReview > 0 ? ", \(a.toReview) to review" : "")") { select(a.id) }
            }
        }
    }
}

/// What an agent did lately: its hook events and tools, newest first (the core's timeline; no
/// message text).
struct AgentActivitySheet: View {
    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss
    let agentID: String
    @State private var entries: [AgentTimeline.Entry]?
    @State private var problem: String?

    var body: some View {
        let agent = model.fleet.agent(agentID)
        VStack(spacing: 0) {
            HStack(spacing: 10) {
                if let agent { AgentBadge(agent: agent, size: 32) }
                VStack(alignment: .leading, spacing: 1) {
                    Text(agent?.name ?? agentID).font(.headline)
                    if let agent {
                        Text(OfficeLook.statusText(agent, serverNow: Int64(Date().timeIntervalSince1970 * 1000) + model.fleet.clockOffset))
                            .font(.callout).foregroundStyle(.secondary)
                    }
                }
                Spacer()
                Button("Refresh", systemImage: "arrow.clockwise") { Task { await load() } }.labelStyle(.iconOnly)
            }
            .padding(14)
            Divider()
            Group {
                if let problem {
                    ContentUnavailableView("Could not read the activity", systemImage: "exclamationmark.triangle", description: Text(problem))
                } else if let entries, entries.isEmpty {
                    ContentUnavailableView("Nothing yet", systemImage: "clock", description: Text("The core has no events from this agent in the last 7 days."))
                } else if let entries {
                    List(Array(entries.enumerated()), id: \.offset) { _, e in
                        HStack {
                            Text(e.words)
                            Spacer()
                            if let d = Date(isoString: e.at) {
                                Text(d, format: .dateTime.month(.abbreviated).day().hour().minute().second()).monospacedDigit().foregroundStyle(.secondary)
                            }
                        }
                    }
                } else {
                    ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity)
                }
            }
            Divider()
            HStack {
                Text("The last \(entries?.count ?? 0) events").font(.callout).foregroundStyle(.secondary)
                Spacer()
                Button("Done") { dismiss() }.keyboardShortcut(.defaultAction)
            }
            .padding(12)
        }
        .frame(width: 460, height: 520)
        .task { await load() }
    }

    private func load() async {
        do {
            entries = try await model.fleet.client.timeline(agent: agentID, limit: 200).entries
            problem = nil
        } catch {
            problem = String(describing: error)
        }
    }
}

/// The inspector with nothing selected: the floor's people at a glance, each a click away.
struct OfficeOverview: View {
    @Environment(AppModel.self) private var model
    let select: (String) -> Void

    var body: some View {
        Form {
            if let fleet = model.fleet.fleet {
                let s = FleetSummary(fleet)
                Section {
                    LabeledContent("On shift", value: "\(s.onShift) of \(s.total)")
                    LabeledContent("Working", value: "\(s.working)")
                    LabeledContent("Idle", value: "\(s.idle)")
                    if s.attention > 0 { LabeledContent("Needs attention") { Text("\(s.attention)").foregroundStyle(.red) } }
                    if s.reviewItems > 0 {
                        LabeledContent("To review", value: "\(s.reviewItems) from \(s.toReview) \(s.toReview == 1 ? "agent" : "agents")")
                    }
                } header: {
                    Label(model.instances.active?.name ?? "Office", systemImage: "building.2")
                } footer: {
                    Text("Click a person, a desk, the core rack or the robot for details; double-click a person for its recent activity, right-click for actions. Arrow keys move between people. The timeline shows when everyone worked.")
                        .foregroundStyle(.secondary)
                }
                ForEach(fleet.units.filter { u in fleet.agents.contains { $0.unit == u.id } }) { unit in
                    Section(OfficeLook(instance: model.fleet.look).shortName(unit.name)) {
                        ForEach(fleet.agents.filter { $0.unit == unit.id }) { a in
                            Button { select(a.id) } label: {
                                HStack(spacing: 8) {
                                    StateDot(agent: a)
                                    Text(a.name)
                                    Spacer()
                                    Text(OfficeLook.statusText(a, serverNow: Int64(Date().timeIntervalSince1970 * 1000) + model.fleet.clockOffset))
                                        .foregroundStyle(.secondary)
                                        .lineLimit(1)
                                }
                                .contentShape(Rectangle())
                            }
                            .buttonStyle(.plain)
                        }
                    }
                }
            }
        }
        .formStyle(.grouped)
    }
}

/// The core rack's panel: the ChatGPT plan and calls through the gateway.
struct CorePanel: View {
    @Environment(AppModel.self) private var model

    var body: some View {
        Form {
            if let g = model.fleet.fleet?.gateway {
                Section {
                    LabeledContent("Sign-in", value: g.login == "ok" ? "Using ChatGPT plan" : g.login.replacingOccurrences(of: "_", with: " "))
                    LabeledContent("In flight", value: "\(g.inflight) of \(g.concurrency)")
                    LabeledContent("Queued", value: "\(g.queued)")
                    if let until = g.planLimitedUntil.flatMap(Date.init(isoString:)) {
                        LabeledContent("Plan limit") { Text("until \(until, format: .dateTime.hour().minute())").foregroundStyle(.red) }
                    }
                    if let url = g.manageUsageUrl.flatMap(URL.init(string:)), g.login == "ok" { Link("Manage usage", destination: url) }
                    Link("Sign in with ChatGPT terms", destination: g.termsUrl.flatMap(URL.init(string:)) ?? ChatGPTPlanTerms.terms)
                } header: {
                    Label("Swarm core", systemImage: "server.rack")
                }
                Section("Usage since start") {
                    let rows = (g.usage ?? [:]).sorted { $0.key < $1.key }
                    if rows.isEmpty { Text("No model calls yet.").foregroundStyle(.secondary) }
                    ForEach(rows, id: \.key) { id, u in
                        LabeledContent(model.fleet.agent(id)?.name ?? id) {
                            Text("\(u.requests) calls · \(u.inputTokens + u.outputTokens) tokens").monospacedDigit()
                        }
                    }
                }
            }
        }
        .formStyle(.grouped)
    }
}

/// The sync robot's notebook.
struct RobotPanel: View {
    @Environment(AppModel.self) private var model

    var body: some View {
        Form {
            if let r = model.fleet.fleet?.robot {
                Section {
                    Text("Each agent's nap sidecar saves its memory, skills and state on its own schedule. When a nap lands, the robot visits that desk and notes what changed. It shows the sync; it does not cause it.")
                        .foregroundStyle(.secondary)
                    LabeledContent("Now", value: where_(r))
                    if let q = r.queue, !q.isEmpty { LabeledContent("Next", value: q.map(name).joined(separator: ", ")) }
                } header: {
                    Label("Sync robot", systemImage: "figure.walk.motion")
                }
                Section("Notebook") {
                    let log = r.log ?? []
                    if log.isEmpty { Text("No naps since the core started.").foregroundStyle(.secondary) }
                    ForEach(Array(log.enumerated()), id: \.offset) { _, n in
                        VStack(alignment: .leading, spacing: 2) {
                            Text("\(name(n.agent))'s \(n.reason == "shutdown" ? "shutdown nap" : "nap")")
                            if let c = n.changed {
                                Text("\(c.learning) learning · \(c.state) state · \(c.raw) logs").font(.caption).foregroundStyle(.secondary)
                            }
                        }
                    }
                }
            }
        }
        .formStyle(.grouped)
    }

    private func name(_ id: String) -> String { model.fleet.agent(id)?.name ?? id }

    private func where_(_ r: RobotState) -> String {
        switch r.phase {
        case "docked": "At its dock"
        case "there": "Writing at \(name(r.target ?? ""))'s desk"
        case "returning": "Heading back to the dock"
        default: "On the way to \(name(r.target ?? ""))"
        }
    }
}
