import Foundation

// How the office looks and labels things. These rules match the web office (pkg/core/ui/app.js) so
// the app and a browser show the same colours, desks and words for the same fleet.

/// What an agent is doing, as the office shows it.
public enum OfficeMode: String, Sendable, CaseIterable {
    case idle, busy, arriving, alert, unknown, away

    /// At (or on the way to) the desk.
    public var present: Bool { self != .away }

    public var label: String {
        switch self {
        case .idle: "At desk"
        case .busy: "Working"
        case .arriving: "Arriving"
        case .alert: "Needs attention"
        case .unknown: "Unknown"
        case .away: "Off shift"
        }
    }

    /// The ring colour of the name tag (#rrggbb).
    public var color: String {
        switch self {
        case .idle: OfficePalette.ok
        case .busy: OfficePalette.busy
        case .arriving: OfficePalette.warn
        case .alert: OfficePalette.bad
        case .unknown, .away: OfficePalette.off
        }
    }
}

public enum OfficePalette {
    public static let ok = "#34d399"
    public static let busy = "#a78bfa"
    public static let warn = "#fbbf24"
    public static let bad = "#f87171"
    public static let off = "#4b5867"
    public static let gold = "#facc15"
    public static let groupHue = "#34d399"
    /// Unit hues when stormo.yaml does not set one.
    public static let unitHues = ["#2dd4bf", "#818cf8", "#f472b6", "#fbbf24", "#60a5fa", "#fb923c", "#a3e635", "#e879f9"]
}

/// A desk as the office draws it: the persona's `desk:`, else its unit's, else the default.
public struct ResolvedDesk: Sendable, Equatable {
    public var app: String
    /// What each monitor shows (1 or 2).
    public var apps: [String]
    public var props: [String]
    public var side: String
}

/// A person's colours and hair: the persona's `sprite:`, else picks seeded by the agent id.
public struct ResolvedSprite: Sendable, Equatable {
    public var skin: String
    public var hair: String
    public var hairStyle: String
    public var shirt: String
    public var pants: String
    public var accessory: String
}

/// The look rules for one instance (its stormo.yaml office block).
public struct OfficeLook: Sendable {
    public static let apps: Set<String> = ["records", "inbox", "code", "design", "charts", "chat"]
    public static let props: Set<String> = [
        "folders", "ticket", "id-card", "headset", "phone", "globe", "stamp", "laptop", "duck", "notes", "mug",
        "tablet", "camera", "swatches", "books", "chart", "magnifier", "notebook", "lamp", "plant", "photo",
    ]
    public static let sides: Set<String> = ["suitcase", "plant", "bin", "ring-light", "shelf", "whiteboard"]
    public static let wallArt = ["map", "board", "moodboard", "shelves"]
    public static let floors = ["wood", "carpet", "concrete", "library"]
    public static let hairStyles = ["short", "fade", "long", "updo", "curly", "bald"]
    public static let accessories = ["headset", "glasses", "cap", "earrings"]

    static let defaultDesk = Desk(props: ["notes", "mug", "plant"], app: "chat", apps: nil, screens: 1, side: "bin")

    public var instance: InstanceLook?

    public init(instance: InstanceLook?) {
        self.instance = instance
    }

    /// FNV-1a over the string's UTF-16 code units, as the web office hashes ids.
    public static func hash(_ s: String) -> UInt32 {
        var h: UInt32 = 2_166_136_261
        for scalar in s.unicodeScalars {
            let unit = UInt32(String(scalar).utf16.first ?? 0)
            h = (h ^ unit) &* 16_777_619
        }
        return h
    }

    private func unitLook(_ unit: String) -> InstanceLook.Unit? { instance?.units[unit] }

    public func hue(unit: String) -> String {
        if let h = unitLook(unit)?.hue { return h }
        if unit == "group" { return OfficePalette.groupHue }
        return OfficePalette.unitHues[Int(Self.hash(unit) % UInt32(OfficePalette.unitHues.count))]
    }

    public func wall(unit: String) -> String {
        if let w = unitLook(unit)?.wall, Self.wallArt.contains(w) { return w }
        return Self.wallArt[Int(Self.hash(unit) % UInt32(Self.wallArt.count))]
    }

    public func floor(unit: String) -> String {
        if let f = unitLook(unit)?.floor, Self.floors.contains(f) { return f }
        return Self.floors[Int(Self.hash(unit) % UInt32(Self.floors.count))]
    }

    public func desk(for agent: FleetAgent) -> ResolvedDesk {
        let unitDesk = unitLook(agent.unit)?.desk
        let baseApp = unitDesk?.app ?? Self.defaultDesk.app!
        let baseScreens = unitDesk?.screens ?? Self.defaultDesk.screens!
        let baseProps = unitDesk?.props ?? Self.defaultDesk.props!
        let baseSide = unitDesk?.side ?? Self.defaultDesk.side!
        let d = agent.desk
        let listed = (d?.apps ?? (d?.app.map { [$0] } ?? [])).filter { Self.apps.contains($0) }
        let app = listed.first ?? baseApp
        let screens = max(1, min(2, d?.screens ?? (listed.count > 1 ? 2 : (d?.app != nil ? 1 : baseScreens))))
        let props = ((d?.props?.isEmpty == false ? d?.props : nil) ?? baseProps).filter { Self.props.contains($0) }
        let side = d?.side.flatMap { Self.sides.contains($0) ? $0 : nil } ?? baseSide
        return ResolvedDesk(app: app, apps: (0..<screens).map { $0 < listed.count ? listed[$0] : app },
                            props: Array(props.prefix(6)), side: side)
    }

    public func sprite(for agent: FleetAgent) -> ResolvedSprite {
        let h = Self.hash(agent.id)
        func pick(_ xs: [String], _ shift: UInt32) -> String { xs[Int((h >> shift) % UInt32(xs.count))] }
        let s = agent.sprite
        return ResolvedSprite(
            skin: s?.skin ?? pick(["#f1c7a5", "#d9a47e", "#c08a62", "#8d5a3b", "#5c3a24"], 1),
            hair: s?.hair ?? pick(["#1f1a17", "#3a2a20", "#6b4423", "#a0522d", "#d6b370", "#2b2b2b"], 4),
            hairStyle: s?.hairStyle ?? pick(["short", "long", "updo", "curly", "fade"], 7),
            shirt: s?.shirt ?? hue(unit: agent.unit),
            pants: s?.pants ?? "#2a2f38",
            accessory: s?.accessory ?? "none")
    }

    /// A unit's name without the org in front ("Acme Sales" → "Sales").
    public func shortName(_ name: String) -> String {
        guard let org = instance?.org, !org.isEmpty, name.lowercased().hasPrefix(org.lowercased() + " ") else { return name }
        return String(name.dropFirst(org.count + 1))
    }

    // MARK: Status

    public static func mode(of a: FleetAgent) -> OfficeMode {
        switch a.state {
        case "running":
            if a.health == "unhealthy" || !(a.detail ?? "").isEmpty || a.needsAttention { return .alert }
            if let o = a.office { return o.working ? .busy : .idle }
            let working = (a.activity?.activeAgents ?? 0) > 0 || a.activity?.gatewayBusy == true || !(a.activity?.runningJobs ?? []).isEmpty
            return working ? .busy : .idle
        case "starting", "restarting": return .arriving
        case "unknown": return .unknown
        default: return .away
        }
    }

    static let sources = ["slack": "Slack", "cron": "scheduled job", "api_server": "API run", "api": "API run", "telegram": "Telegram",
                          "cli": "terminal", "discord": "Discord", "whatsapp": "WhatsApp"]

    public static func sourceLabel(_ s: String?) -> String {
        guard let s, !s.isEmpty else { return "a task" }
        return sources[s] ?? s.replacingOccurrences(of: "_", with: " ").replacingOccurrences(of: "-", with: " ")
    }

    /// A scheduled job when that is all that runs, else the newest session's source.
    public static func workLabel(_ a: FleetAgent) -> String {
        if let job = a.activity?.runningJobs?.first, (a.activity?.activeAgents ?? 0) == 0 { return job }
        return sourceLabel(a.activity?.source)
    }

    /// "4m", "2h", "just now": since the later of the last conversation and the last job.
    public static func idleFor(_ a: FleetAgent, serverNow: Int64) -> String {
        let t = [a.activity?.lastActive, a.activity?.lastJobAt].compactMap { $0.flatMap(epochMillis) }.max() ?? 0
        guard t > 0 else { return "" }
        let m = Int((Double(serverNow - t) / 60000).rounded())
        if m < 1 { return "just now" }
        if m < 60 { return "\(m)m" }
        if m < 2880 { return "\(Int((Double(m) / 60).rounded()))h" }
        return "\(Int((Double(m) / 1440).rounded()))d"
    }

    /// The line under the agent's name: what it is doing, not where its body is.
    public static func statusText(_ a: FleetAgent, serverNow: Int64) -> String {
        let mode = mode(of: a)
        switch mode {
        case .busy: return "Working · \(workLabel(a))"
        case .idle:
            var s = "Idle"
            let idle = idleFor(a, serverNow: serverNow)
            if !idle.isEmpty { s += " \(idle)" }
            if let label = a.office?.label, !label.isEmpty { s += " · \(label)" }
            return s
        case .arriving: return "Arriving"
        case .alert: return "Needs attention"
        case .unknown: return "Status unknown"
        case .away: return "Off shift"
        }
    }

    /// RFC 3339 → epoch milliseconds.
    public static func epochMillis(_ iso: String) -> Int64? {
        let strategies: [Date.ISO8601FormatStyle] = [
            .iso8601.year().month().day().time(includingFractionalSeconds: true),
            .iso8601,
        ]
        for s in strategies {
            if let d = try? Date(iso, strategy: s) { return Int64(d.timeIntervalSince1970 * 1000) }
        }
        return nil
    }
}
