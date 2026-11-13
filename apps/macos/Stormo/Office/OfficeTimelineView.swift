import StormoKit
import SwiftUI

/// How the Office section shows the fleet: where everyone is now, or when everyone worked.
enum OfficeViewMode: String, CaseIterable, Identifiable {
    case floor, timeline

    var id: String { rawValue }
    var title: String { self == .floor ? "Floor" : "Timeline" }
    var symbol: String { self == .floor ? "building.2" : "chart.bar.xaxis" }

    private static let key = "office.view"

    /// The one last used (debug builds: STORMO_OFFICE_VIEW chooses, for snapshots).
    static var saved: OfficeViewMode {
        #if DEBUG
        if let m = ProcessInfo.processInfo.environment["STORMO_OFFICE_VIEW"].flatMap(OfficeViewMode.init(rawValue:)) { return m }
        #endif
        return AppModel.defaults.string(forKey: key).flatMap(OfficeViewMode.init(rawValue:)) ?? .floor
    }

    func save() { AppModel.defaults.set(rawValue, forKey: Self.key) }
}

/// The fleet over time: a lane per agent with its stretches of work over the last hours, from the
/// core's activity (`GET /api/timeline`: times only, nothing of what was said or done).
struct OfficeTimelineView: View {
    @Environment(AppModel.self) private var model
    let fleet: Fleet
    /// The chosen agent (the inspector beside shows it).
    @Binding var selection: String?
    /// A double-click: the agent's recent activity.
    let open: (String) -> Void

    @State private var hours = OfficeTimelineView.savedHours
    @State private var timeline: FleetTimeline?
    @State private var problem: Problem?
    @State private var hover: Hover?

    private enum Problem: Equatable {
        /// The core has no such route yet.
        case old
        case failed(String)
    }

    private struct Hover: Equatable {
        var agent: String
        var span: FleetTimeline.Span
    }

    private static let ranges: [(hours: Int, title: String)] = [(1, "1 hour"), (6, "6 hours"), (24, "24 hours"), (168, "7 days")]
    private static let hoursKey = "office.timeline.hours"
    private static var savedHours: Int {
        let h = AppModel.defaults.integer(forKey: hoursKey)
        return ranges.contains { $0.hours == h } ? h : 6
    }

    // Column widths shared by the axis and every lane, so they line up.
    private static let nameWidth: CGFloat = 132, totalWidth: CGFloat = 72, laneHeight: CGFloat = 22

    var body: some View {
        VStack(spacing: 0) {
            header
            Divider()
            switch problem {
            case .old:
                ContentUnavailableView("The core is older than the timeline", systemImage: "clock.badge.exclamationmark",
                                       description: Text("Restart the core to bring it up to date with this app."))
            case .failed(let why) where timeline == nil:
                ContentUnavailableView("Could not read the timeline", systemImage: "exclamationmark.triangle", description: Text(why))
            default:
                if let timeline { lanes(timeline) } else { ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity) }
            }
        }
        .background(Color(nsColor: .windowBackgroundColor))
        .task(id: hours) {
            // Fresh at once for a new range, then kept up while the view shows.
            while !Task.isCancelled {
                await load()
                try? await Task.sleep(for: .seconds(30))
            }
        }
    }

    private var header: some View {
        HStack(spacing: 12) {
            Picker("Range", selection: $hours) {
                ForEach(Self.ranges, id: \.hours) { Text($0.title).tag($0.hours) }
            }
            .pickerStyle(.segmented)
            .labelsHidden()
            .fixedSize()
            .onChange(of: hours) { AppModel.defaults.set(hours, forKey: Self.hoursKey) }
            Spacer()
            if let hover, let agent = fleet.agents.first(where: { $0.id == hover.agent }) {
                Text("\(agent.name) · \(clock(hover.span.start))–\(clock(hover.span.end)) · \(hover.span.events) \(hover.span.events == 1 ? "event" : "events")")
                    .monospacedDigit()
            } else {
                Text("When each agent was at work, from its tool activity. Hover a stretch for its times.")
                    .foregroundStyle(.secondary)
            }
        }
        .font(.callout)
        .lineLimit(1)
        .padding(.horizontal, 16)
        .padding(.vertical, 8)
    }

    private func lanes(_ t: FleetTimeline) -> some View {
        let look = OfficeLook(instance: model.fleet.look)
        let units = fleet.units.filter { u in fleet.agents.contains { $0.unit == u.id } }
        let quiet = fleet.agents.allSatisfy { (t.agents[$0.id] ?? []).isEmpty }
        return ScrollView {
            LazyVStack(alignment: .leading, spacing: 0, pinnedViews: [.sectionHeaders]) {
                Section {
                    ForEach(units) { unit in
                        Text(look.shortName(unit.name).uppercased())
                            .font(.caption.weight(.semibold))
                            .kerning(0.8)
                            .foregroundStyle(Color(hex: look.hue(unit: unit.id)) ?? .secondary)
                            .padding(.horizontal, 16)
                            .padding(.top, 14)
                            .padding(.bottom, 4)
                        ForEach(fleet.agents.filter { $0.unit == unit.id }) { a in
                            row(a, t)
                        }
                    }
                    if quiet {
                        Text("Nobody reported any activity in this time.")
                            .foregroundStyle(.secondary)
                            .frame(maxWidth: .infinity)
                            .padding(.top, 24)
                    }
                } header: {
                    axis(t)
                }
            }
            .padding(.bottom, 16)
        }
    }

    /// The clock along the top, over the lanes' own width.
    private func axis(_ t: FleetTimeline) -> some View {
        HStack(spacing: 10) {
            Color.clear.frame(width: Self.nameWidth + 18, height: 1)
            GeometryReader { geo in
                let axis = TimelineAxis(from: t.from, to: t.now, width: geo.size.width)
                Canvas { ctx, size in
                    for mark in axis.marks() {
                        let x = axis.x(mark)
                        ctx.draw(Text(label(mark)).font(.caption).monospacedDigit().foregroundStyle(.secondary), at: CGPoint(x: x, y: size.height / 2))
                    }
                }
            }
            .frame(height: 22)
            Text("Worked").font(.caption).foregroundStyle(.secondary).frame(width: Self.totalWidth, alignment: .trailing)
        }
        .padding(.horizontal, 16)
        .padding(.vertical, 4)
        .background(.bar)
        .overlay(alignment: .bottom) { Divider() }
        .accessibilityHidden(true)
    }

    private func row(_ a: FleetAgent, _ t: FleetTimeline) -> some View {
        let spans = t.agents[a.id] ?? []
        let worked = FleetTimeline.duration(t.worked(a.id))
        return HStack(spacing: 10) {
            StateDot(agent: a)
            Text(a.name).frame(width: Self.nameWidth, alignment: .leading).lineLimit(1)
            GeometryReader { geo in
                let axis = TimelineAxis(from: t.from, to: t.now, width: geo.size.width)
                Canvas { ctx, size in
                    ctx.fill(Path(roundedRect: CGRect(origin: .zero, size: size), cornerRadius: 5), with: .color(.primary.opacity(0.06)))
                    for mark in axis.marks() {
                        ctx.fill(Path(CGRect(x: axis.x(mark), y: 0, width: 1, height: size.height)), with: .color(.primary.opacity(0.08)))
                    }
                    let work = Color(hex: OfficePalette.busy) ?? .purple
                    for s in spans {
                        let bar = axis.bar(s)
                        let lit = hover?.agent == a.id && hover?.span == s
                        ctx.fill(Path(roundedRect: CGRect(x: bar.x, y: 3, width: bar.width, height: size.height - 6), cornerRadius: 3),
                                 with: .color(work.opacity(lit ? 1 : 0.75)))
                    }
                }
                .onContinuousHover { phase in
                    switch phase {
                    case .active(let p):
                        // Thin bars are hard to hit: a few points either side count.
                        let slop = Int64(Double(t.now - t.from) / Double(max(1, geo.size.width)) * 4)
                        let found = axis.span(at: axis.time(at: p.x), in: spans, slop: slop).map { Hover(agent: a.id, span: $0) }
                        if found != nil || hover?.agent == a.id { hover = found }
                    case .ended:
                        if hover?.agent == a.id { hover = nil }
                    }
                }
            }
            .frame(height: Self.laneHeight)
            Text(worked.isEmpty ? "—" : worked)
                .monospacedDigit()
                .foregroundStyle(worked.isEmpty ? .tertiary : .secondary)
                .frame(width: Self.totalWidth, alignment: .trailing)
                .lineLimit(1)
        }
        .padding(.horizontal, 16)
        .padding(.vertical, 5)
        .background(selection == a.id ? Color.accentColor.opacity(0.14) : .clear)
        .contentShape(Rectangle())
        .onTapGesture(count: 2) {
            selection = a.id
            open(a.id)
        }
        .onTapGesture { selection = selection == a.id ? nil : a.id }
        .accessibilityElement(children: .ignore)
        .accessibilityLabel("\(a.name), \(OfficeLook.statusText(a, serverNow: t.now)). \(worked.isEmpty ? "No activity" : "Worked \(worked) in \(spans.count) \(spans.count == 1 ? "stretch" : "stretches")") in the last \(Self.ranges.first { $0.hours == hours }?.title ?? "\(hours) hours").")
        .accessibilityAddTraits(.isButton)
        .accessibilityAction { selection = a.id }
    }

    private func clock(_ ms: Int64) -> String {
        Date(timeIntervalSince1970: Double(ms) / 1000).formatted(date: .omitted, time: .shortened)
    }

    /// A mark's label: the time within a day's range, the day beyond it.
    private func label(_ ms: Int64) -> String {
        let d = Date(timeIntervalSince1970: Double(ms) / 1000)
        return hours > 24 ? d.formatted(.dateTime.weekday(.abbreviated).day()) : d.formatted(date: .omitted, time: .shortened)
    }

    private func load() async {
        do {
            timeline = try await model.fleet.client.fleetTimeline(hours: hours)
            problem = nil
        } catch CoreClient.Failure.notFound {
            problem = .old
        } catch is CancellationError {
        } catch {
            problem = .failed(String(describing: error))
        }
    }
}
