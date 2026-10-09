import AppKit
import SpriteKit
import StormoKit
import SwiftUI

/// The office: the fleet on a floor plan, live (SpriteKit), with the inspector beside it.
struct OfficeView: View {
    @Environment(AppModel.self) private var model
    @Environment(\.accessibilityReduceMotion) private var reduceMotion
    @State private var scene = OfficeScene(size: CGSize(width: 1200, height: 800))
    @State private var selection: OfficeSelection?

    var body: some View {
        Group {
            if model.core.state.isRunning, let fleet = model.fleet.fleet {
                OfficeSpriteView(scene: scene)
                    .overlay(alignment: .bottom) { OfficeHUD(fleet: fleet).padding(.bottom, 14) }
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
            ToolbarItemGroup {
                Button("Zoom Out", systemImage: "minus.magnifyingglass") { scene.zoom(by: 1 / 1.25) }
                    .keyboardShortcut("-")
                Button("Fit", systemImage: "arrow.up.left.and.arrow.down.right") { scene.resetZoom() }
                    .keyboardShortcut("0")
                Button("Zoom In", systemImage: "plus.magnifyingglass") { scene.zoom(by: 1.25) }
                    .keyboardShortcut("=")
            }
        }
    }

    private func configure() {
        scene.reduceMotion = reduceMotion
        scene.onSelect = { selection = $0 }
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

    /// Scrolling zooms around the pointer; dragging open floor pans (OfficeScene).
    override func scrollWheel(with e: NSEvent) {
        guard let office else { return }
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
        addTrackingArea(NSTrackingArea(rect: bounds, options: [.mouseMoved, .activeInKeyWindow, .inVisibleRect], owner: self))
    }

    override func mouseMoved(with e: NSEvent) {
        guard let office else { return }
        let p = e.location(in: office)
        (office.hit(at: p) == nil ? NSCursor.openHand : NSCursor.pointingHand).set()
    }

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
        let idle = fleet.agents.filter { OfficeLook.mode(of: $0) == .idle }.count
        let g = fleet.gateway
        HStack(spacing: 14) {
            Meter(label: "On shift", value: "\(s.onShift)/\(s.total)", color: s.onShift > 0 ? OfficePalette.ok : OfficePalette.off)
            Meter(label: "Working", value: "\(s.working)", color: s.working > 0 ? OfficePalette.busy : nil)
            Meter(label: "Idle", value: "\(idle)", color: idle > 0 ? OfficePalette.ok : nil)
            Meter(label: "To review", value: "\(fleet.agents.reduce(0) { $0 + ($1.pendingLearnings ?? 0) + ($1.pendingSkills ?? 0) })",
                  color: s.toReview > 0 ? OfficePalette.gold : nil)
            if s.attention > 0 { Meter(label: "Attention", value: "\(s.attention)", color: OfficePalette.bad) }
            if g.planLimitedUntil != nil {
                Meter(label: "ChatGPT plan", value: "limited", color: OfficePalette.bad)
            } else if g.login == "ok" {
                Meter(label: "Using ChatGPT plan", value: "", color: OfficePalette.ok)
            } else {
                Meter(label: "ChatGPT plan", value: g.login == "missing" ? "signed out" : g.login, color: OfficePalette.warn)
            }
        }
        .padding(.horizontal, 16)
        .padding(.vertical, 9)
        .glassEffect(.regular, in: .capsule)
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
                    if s.attention > 0 { LabeledContent("Needs attention") { Text("\(s.attention)").foregroundStyle(.red) } }
                    if s.toReview > 0 { LabeledContent("With learnings to review", value: "\(s.toReview)") }
                } header: {
                    Label(model.instances.active?.name ?? "Office", systemImage: "building.2")
                } footer: {
                    Text("Click a person, a desk, the core rack or the robot for details. Right-click a person for actions.")
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
