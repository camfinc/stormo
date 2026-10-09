import CoreGraphics
import Foundation

/// The office floor as geometry, in points with y growing down: each unit an office off one of two
/// hallways, the server room over the lobby in the middle, a desk per agent (docs/core.md §3).
/// Pure: the scene draws it, playback walks it, tests check it.
public struct FloorPlan: Sendable {
    public enum Side: String, Sendable { case left, right }

    public struct Office: Sendable, Identifiable {
        public var unit: FleetUnit
        public var rect: CGRect
        public var side: Side
        public var row: Int
        public var span: Int
        public var hue: String
        public var floor: String
        public var wall: String
        /// The opening to the hallway (in the wall between them).
        public var door: CGRect
        /// Window strips in the outer walls.
        public var windows: [CGRect]
        public var id: String { unit.id }
    }

    public struct Workstation: Sendable, Identifiable {
        /// nil for an open desk.
        public var agent: String?
        public var unit: String
        /// The 156×136 workstation box: desk at the top, chair below.
        public var rect: CGRect
        /// Where the person sits: the chair's centre.
        public var seat: CGPoint
        /// The office it is in, nil in the lobby.
        public var office: String?
        public var id: String { agent ?? "vacant-\(unit)-\(Int(rect.minX))-\(Int(rect.minY))" }
    }

    /// Where a person stands for an activity, and how.
    public struct Spot: Sendable, Equatable {
        public var point: CGPoint
        public var pose: Pose
        public var facing: Facing
        /// Reached through the hallways (lobby) or within the room.
        public var inLobby: Bool
    }

    public struct Rack: Sendable {
        public enum Kind: String, Sendable { case net, core, fleet, ups }
        public var kind: Kind
        public var rect: CGRect
    }

    // Workstation and furniture sizes, shared with the scene.
    public static let workstation = CGSize(width: 156, height: 136)
    static let deskGap = CGSize(width: 40, height: 26)
    static let minDesks = 4
    static let wall: CGFloat = 8, gap: CGFloat = 6, hall: CGFloat = 52
    static let officeWidth: CGFloat = 433, middleWidth: CGFloat = 390
    /// The chair centre inside a workstation box.
    public static let seatOffset = CGPoint(x: 78, y: 88)

    public private(set) var size: CGSize = .zero
    public private(set) var offices: [Office] = []
    public private(set) var halls: [Side: CGRect] = [:]
    public private(set) var server: CGRect = .zero
    public private(set) var lobby: CGRect = .zero
    public private(set) var workstations: [Workstation] = []
    public private(set) var racks: [Rack] = []
    public private(set) var lobbyDoors: [Side: CGRect] = [:]
    public private(set) var serverDoor: CGRect = .zero
    public private(set) var entrance: CGRect = .zero
    // Lobby furniture.
    public private(set) var fridge: CGRect = .zero
    public private(set) var counter: CGRect = .zero
    public private(set) var sink: CGRect = .zero
    public private(set) var espresso: CGRect = .zero
    public private(set) var fruit: CGRect = .zero
    public private(set) var cooler: CGRect = .zero
    public private(set) var sofa: CGRect = .zero
    public private(set) var coffeeTable: CGRect = .zero
    public private(set) var cafeTables: [CGRect] = []
    public private(set) var reception: CGRect = .zero
    public private(set) var clocks: [CGRect] = []

    /// `clocks` are the world clocks' city names (stormo.yaml office.clocks), for the lobby wall.
    /// `fitting` is the width/height of the view it fills: the floor takes that shape (taller rows,
    /// or wider offices and middle) so it fills the view edge to edge, within sane limits.
    public init(units: [FleetUnit], agents: [FleetAgent], look: OfficeLook, clocks: [String] = [], fitting aspect: CGFloat? = nil) {
        layout(units: units, agents: agents, look: look, clocks: clocks, aspect: aspect)
    }

    /// The shapes a floor can stretch to; beyond them it is shown with a margin.
    public static let aspectRange: ClosedRange<CGFloat> = 0.6...3.2

    // MARK: Layout

    private static func deskRows(_ n: Int) -> Int { (max(minDesks, n + n % 2) + 1) / 2 }
    private static func deskBlockHeight(rows: Int) -> CGFloat {
        rows == 0 ? 0 : CGFloat(rows) * workstation.height + CGFloat(rows - 1) * deskGap.height
    }
    private static func rackHeight(_ kind: Rack.Kind, agents: Int) -> CGFloat {
        switch kind {
        case .net: 142
        case .core: 185
        case .fleet: 30 + CGFloat(max(1, agents)) * 18
        case .ups: 110
        }
    }

    private mutating func layout(units: [FleetUnit], agents: [FleetAgent], look: OfficeLook, clocks cities: [String], aspect: CGFloat?) {
        let rooms = units.filter { $0.id != "group" }
        let roomIDs = Set(rooms.map(\.id))
        let left = rooms.enumerated().filter { $0.offset % 2 == 0 }.map(\.element)
        let right = rooms.enumerated().filter { $0.offset % 2 == 1 }.map(\.element)
        let rows = max(1, left.count, right.count)
        let lobbyAgents = agents.filter { !roomIDs.contains($0.unit) }

        // Row height: every office fits its desks; the middle column fits the server room and lobby.
        var rowHeight: CGFloat = 320
        for list in [left, right] {
            for (i, u) in list.enumerated() {
                let span = i == list.count - 1 ? rows - i : 1
                let need = 56 + Self.deskBlockHeight(rows: Self.deskRows(agents.filter { $0.unit == u.id }.count)) + 30
                rowHeight = max(rowHeight, (need - CGFloat(span - 1) * Self.gap) / CGFloat(span))
            }
        }
        let rackBlock = [Rack.Kind.net, .core, .fleet, .ups].map { Self.rackHeight($0, agents: agents.count) }.max() ?? 0
        let serverNeed = max(330, 70 + rackBlock + 40)
        let lobbyDeskRows = lobbyAgents.isEmpty ? 0 : (lobbyAgents.count + 1) / 2
        let lobbyBlock = Self.deskBlockHeight(rows: lobbyDeskRows) + (lobbyDeskRows > 0 ? 20 : 0) + 50
        let lobbyNeed = max(300, 100 + lobbyBlock + 70)
        var inner = CGFloat(rows) * rowHeight + CGFloat(rows - 1) * Self.gap
        if inner < serverNeed + Self.gap + lobbyNeed {
            inner = serverNeed + Self.gap + lobbyNeed
            rowHeight = (inner - CGFloat(rows - 1) * Self.gap) / CGFloat(rows)
        }

        // Take the view's shape: more height for the rows, or more width for the offices and middle.
        var officeWidth = Self.officeWidth, middleWidth = Self.middleWidth
        if let aspect, aspect > 0 {
            let a = min(max(aspect, Self.aspectRange.lowerBound), Self.aspectRange.upperBound)
            let naturalW = 2 * Self.wall + 2 * officeWidth + middleWidth + 2 * Self.hall + 4 * Self.gap
            let naturalH = inner + 2 * Self.wall
            if naturalW / naturalH > a {
                inner = naturalW / a - 2 * Self.wall
                rowHeight = (inner - CGFloat(rows - 1) * Self.gap) / CGFloat(rows)
            } else {
                let extra = naturalH * a - naturalW
                officeWidth += extra * 0.36
                middleWidth += extra * 0.28
            }
        }
        let x0 = Self.wall
        let xHallL = x0 + officeWidth + Self.gap
        let xMid = xHallL + Self.hall + Self.gap
        let xHallR = xMid + middleWidth + Self.gap
        let xRight = xHallR + Self.hall + Self.gap
        size = CGSize(width: xRight + officeWidth + Self.wall, height: inner + 2 * Self.wall)
        halls = [.left: CGRect(x: xHallL, y: Self.wall, width: Self.hall, height: inner),
                 .right: CGRect(x: xHallR, y: Self.wall, width: Self.hall, height: inner)]

        // Offices, and their desks.
        for (side, list) in [(Side.left, left), (Side.right, right)] {
            for (i, u) in list.enumerated() {
                let span = i == list.count - 1 ? rows - i : 1
                let y = Self.wall + CGFloat(i) * (rowHeight + Self.gap)
                let h = CGFloat(span) * rowHeight + CGFloat(span - 1) * Self.gap
                let rect = CGRect(x: side == .left ? x0 : xRight, y: y, width: officeWidth, height: h)
                let door = side == .left
                    ? CGRect(x: rect.maxX, y: rect.midY - 30, width: Self.gap, height: 60)
                    : CGRect(x: rect.minX - Self.gap, y: rect.midY - 30, width: Self.gap, height: 60)
                var windows = [side == .left
                    ? CGRect(x: rect.minX - Self.wall, y: rect.minY + h * 0.14, width: Self.wall, height: h * 0.72)
                    : CGRect(x: rect.maxX, y: rect.minY + h * 0.14, width: Self.wall, height: h * 0.72)]
                let stripX = side == .left ? rect.minX + rect.width * 0.08 : rect.maxX - rect.width * 0.54
                if i == 0 { windows.append(CGRect(x: stripX, y: rect.minY - Self.wall, width: rect.width * 0.46, height: Self.wall)) }
                if i + span == rows { windows.append(CGRect(x: stripX, y: rect.maxY, width: rect.width * 0.46, height: Self.wall)) }
                offices.append(Office(unit: u, rect: rect, side: side, row: i, span: span, hue: look.hue(unit: u.id),
                                      floor: look.floor(unit: u.id), wall: look.wall(unit: u.id), door: door, windows: windows))
                let members = agents.filter { $0.unit == u.id }.map(\.id)
                let total = max(Self.minDesks, members.count + members.count % 2)
                let block = Self.deskBlockHeight(rows: (total + 1) / 2)
                let top = rect.minY + 56 + ((rect.height - 86) - block) / 2
                placeDesks((0..<total).map { $0 < members.count ? members[$0] : nil }, unit: u.id, office: u.id,
                           centerX: rect.midX, top: top)
            }
        }

        // The middle column: the server room over the lobby.
        let serverH = max(serverNeed, min(inner - Self.gap - lobbyNeed, (inner - Self.gap) * 1.1 / 2.1))
        server = CGRect(x: xMid, y: Self.wall, width: middleWidth, height: serverH)
        lobby = CGRect(x: xMid, y: server.maxY + Self.gap, width: middleWidth, height: inner - Self.gap - serverH)
        serverDoor = CGRect(x: server.midX - 30, y: server.maxY, width: 60, height: Self.gap)

        let kinds: [Rack.Kind] = [.net, .core, .fleet, .ups]
        let widths: [Rack.Kind: CGFloat] = [.net: 46, .core: 132, .fleet: 74, .ups: 46]
        let totalW = kinds.reduce(CGFloat(0)) { $0 + widths[$1]! } + 9 * CGFloat(kinds.count - 1)
        let bottom = server.minY + 70 + (server.height - 110 + rackBlock) / 2
        var rx = server.midX - totalW / 2
        for k in kinds {
            let h = Self.rackHeight(k, agents: agents.count)
            racks.append(Rack(kind: k, rect: CGRect(x: rx, y: bottom - h, width: widths[k]!, height: h)))
            rx += widths[k]! + 9
        }

        // Lobby: clocks on the top wall beside the sign, or on their own row when they do not fit;
        // the kitchen below them; desks and seating in the middle.
        let clockWidths = cities.map { max(56, CGFloat($0.count) * 5.6 + 22) }
        let clocksWidth = clockWidths.reduce(0, +) + 6 * CGFloat(max(0, clockWidths.count - 1))
        let ownRow = clocksWidth > lobby.width - 190
        if ownRow {
            let spare = max(0, (lobby.width - 28 - clocksWidth) / CGFloat(max(1, clockWidths.count - 1)))
            var cx = lobby.minX + 14
            for w in clockWidths {
                clocks.append(CGRect(x: cx, y: lobby.minY + 38, width: w, height: 30))
                cx += w + 6 + spare
            }
        } else {
            var cx = lobby.maxX - 12
            for w in clockWidths.reversed() {
                clocks.insert(CGRect(x: cx - w, y: lobby.minY + 7, width: w, height: 30), at: 0)
                cx -= w + 6
            }
        }
        let ky = lobby.minY + (ownRow ? 84 : 48)
        fridge = CGRect(x: lobby.minX + 14, y: ky, width: 26, height: 34)
        cooler = CGRect(x: lobby.maxX - 14 - 18, y: ky, width: 18, height: 18)
        counter = CGRect(x: fridge.maxX + 8, y: ky, width: cooler.minX - 8 - fridge.maxX - 8, height: 24)
        sink = CGRect(x: counter.minX + counter.width * 0.14, y: ky + 5, width: 18, height: 13)
        espresso = CGRect(x: counter.minX + counter.width * 0.48, y: ky + 2, width: 14, height: 18)
        fruit = CGRect(x: counter.maxX - counter.width * 0.12 - 15, y: ky + 4, width: 15, height: 15)
        let blockTop = lobby.minY + 100 + ((lobby.height - 170) - lobbyBlock) / 2
        if !lobbyAgents.isEmpty {
            placeDesks(lobbyAgents.map(\.id), unit: "group", office: nil, centerX: lobby.midX, top: blockTop)
        }
        let seatY = blockTop + lobbyBlock - 25
        let items: [CGFloat] = [50, 84, 34, 50]
        var sx = lobby.midX - (items.reduce(0, +) + 22 * 3) / 2
        for (i, w) in items.enumerated() {
            switch i {
            case 1: sofa = CGRect(x: sx, y: seatY - 15, width: 84, height: 30)
            case 2: coffeeTable = CGRect(x: sx, y: seatY - 10, width: 34, height: 20)
            default: cafeTables.append(CGRect(x: sx + 10, y: seatY - 15, width: 30, height: 30))
            }
            sx += w + 22
        }
        reception = CGRect(x: lobby.midX - 128, y: lobby.maxY - 34 - 16, width: 80, height: 16)
        entrance = CGRect(x: lobby.midX - 46, y: lobby.maxY, width: 92, height: Self.wall)
        let doorY = lobby.maxY - lobby.height * 0.3 - 60
        lobbyDoors = [.left: CGRect(x: lobby.minX - Self.gap, y: doorY, width: Self.gap, height: 60),
                      .right: CGRect(x: lobby.maxX, y: doorY, width: Self.gap, height: 60)]
    }

    private mutating func placeDesks(_ ids: [String?], unit: String, office: String?, centerX: CGFloat, top: CGFloat) {
        let w = Self.workstation
        let left = centerX - w.width - Self.deskGap.width / 2
        for (i, id) in ids.enumerated() {
            let col = CGFloat(i % 2), row = CGFloat(i / 2)
            let origin = CGPoint(x: left + col * (w.width + Self.deskGap.width), y: top + row * (w.height + Self.deskGap.height))
            let rect = CGRect(origin: origin, size: w)
            workstations.append(Workstation(agent: id, unit: unit, rect: rect,
                                            seat: CGPoint(x: origin.x + Self.seatOffset.x, y: origin.y + Self.seatOffset.y), office: office))
        }
    }

    // MARK: Places

    public func workstation(of agent: String) -> Workstation? { workstations.first { $0.agent == agent } }
    public func office(_ id: String?) -> Office? { offices.first { $0.unit.id == id } }
    public var coreRack: CGRect { racks.first { $0.kind == .core }?.rect ?? .zero }

    /// Where an agent goes for an activity (coffee, water, sofa, window, stretch, entrance).
    /// `slot` puts people side by side. nil when this floor has no such place for it.
    public func spot(_ name: String, for agent: String, slot: Int = 0) -> Spot? {
        guard let ws = workstation(of: agent) else { return nil }
        let side: CGFloat = slot == 0 ? 0 : (slot % 2 == 1 ? 1 : -1) * CGFloat((slot + 1) / 2) * 26
        switch name {
        case "coffee":
            return Spot(point: CGPoint(x: espresso.midX + side, y: espresso.midY + 30), pose: .coffee, facing: .down, inLobby: true)
        case "water":
            return Spot(point: CGPoint(x: cooler.midX - 6 + side, y: cooler.midY + 30), pose: .coffee, facing: .down, inLobby: true)
        case "sofa":
            return Spot(point: CGPoint(x: sofa.midX - 18 + CGFloat(slot % 2) * 36, y: sofa.midY - 8), pose: .sofa, facing: .down, inLobby: true)
        case "window":
            guard let room = office(ws.office), let w = room.windows.first else { return nil }
            let fromLeft = room.side == .left
            let y = min(max(ws.seat.y, room.rect.minY + 70), room.rect.maxY - 40) + side
            return Spot(point: CGPoint(x: w.midX + (fromLeft ? 34 : -34), y: y), pose: .stand,
                        facing: fromLeft ? .left : .right, inLobby: false)
        case "stretch":
            return Spot(point: CGPoint(x: ws.seat.x + 34, y: ws.seat.y + 18), pose: .stretch, facing: .up, inLobby: false)
        case "entrance":
            return Spot(point: CGPoint(x: entrance.midX, y: entrance.midY - 6), pose: .walk, facing: .down, inLobby: true)
        default:
            return nil
        }
    }

    /// Seat to spot: up from the chair into the aisle, then for the lobby out through the office
    /// door, along the hallway and in through the lobby's side door.
    public func path(for agent: String, to spot: Spot) -> [CGPoint]? {
        guard let ws = workstation(of: agent) else { return nil }
        let seat = ws.seat
        let aisle = CGPoint(x: seat.x, y: seat.y + 40)
        guard spot.inLobby, let room = office(ws.office) else {
            return [seat, aisle, CGPoint(x: spot.point.x, y: aisle.y), spot.point]
        }
        guard let hall = halls[room.side], let ld = lobbyDoors[room.side] else { return nil }
        let d = CGPoint(x: room.door.midX, y: room.door.midY)
        let l = CGPoint(x: ld.midX, y: ld.midY)
        let inward: CGFloat = room.side == .left ? -28 : 28
        return [seat, aisle, CGPoint(x: d.x + inward, y: aisle.y), CGPoint(x: d.x + inward, y: d.y), d,
                CGPoint(x: hall.midX, y: d.y), CGPoint(x: hall.midX, y: l.y), l, CGPoint(x: l.x - inward, y: l.y),
                CGPoint(x: spot.point.x, y: l.y), spot.point]
    }

    /// The robot's dock: in front of the core rack.
    public var dock: CGPoint { CGPoint(x: coreRack.midX, y: coreRack.maxY + 26) }

    /// From a place to the lobby hub: "dock", or the spot beside an agent's chair.
    public func toHub(_ node: String) -> [CGPoint]? {
        let sd = CGPoint(x: serverDoor.midX, y: serverDoor.midY)
        let hub = CGPoint(x: sd.x, y: lobby.minY + 96)
        if node == "dock" {
            return [dock, CGPoint(x: sd.x, y: sd.y - 26), sd, CGPoint(x: sd.x, y: sd.y + 26), hub]
        }
        guard let ws = workstation(of: node) else { return nil }
        let spot = CGPoint(x: ws.seat.x + 46, y: ws.seat.y + 4)
        guard let room = office(ws.office) else { return [spot, hub] }
        guard let hall = halls[room.side], let ld = lobbyDoors[room.side] else { return nil }
        let d = CGPoint(x: room.door.midX, y: room.door.midY)
        let l = CGPoint(x: ld.midX, y: ld.midY)
        let inward: CGFloat = room.side == .left ? -28 : 28
        let aisle = ws.seat.y + 40
        return [spot, CGPoint(x: spot.x, y: aisle), CGPoint(x: d.x + inward, y: aisle), CGPoint(x: d.x + inward, y: d.y), d,
                CGPoint(x: hall.midX, y: d.y), CGPoint(x: hall.midX, y: l.y), l, CGPoint(x: l.x - inward, y: l.y),
                CGPoint(x: l.x - inward, y: hub.y), hub]
    }

    /// From one place to another through the lobby hub.
    public func robotRoute(from: String, to: String) -> [CGPoint]? {
        guard let a = toHub(from), let b = toHub(to) else { return nil }
        return a + b.reversed().dropFirst()
    }
}

public enum Pose: String, Sendable {
    case sitIdle, sitType, sitRelax, walk, coffee, stretch, stand, sofa

    public var seated: Bool { self == .sitIdle || self == .sitType || self == .sitRelax }
}

public enum Facing: String, Sendable {
    case up, down, left, right

    /// The way a segment from a to b goes.
    public static func along(_ a: CGPoint, _ b: CGPoint) -> Facing {
        let dx = b.x - a.x, dy = b.y - a.y
        return abs(dx) > abs(dy) ? (dx > 0 ? .right : .left) : (dy > 0 ? .down : .up)
    }
}

/// Polylines: lengths and points along them.
public enum Polyline {
    public static func length(_ pts: [CGPoint]) -> CGFloat {
        zip(pts, pts.dropFirst()).reduce(0) { $0 + hypot($1.1.x - $1.0.x, $1.1.y - $1.0.y) }
    }

    /// The point at fraction f of the length, and the index of the segment it is on (1-based end).
    public static func point(_ pts: [CGPoint], at f: Double) -> (point: CGPoint, segment: Int) {
        guard pts.count > 1 else { return (pts.first ?? .zero, 0) }
        var d = CGFloat(max(0, min(1, f))) * length(pts)
        for i in 1..<pts.count {
            let a = pts[i - 1], b = pts[i]
            let seg = hypot(b.x - a.x, b.y - a.y)
            if d <= seg {
                let t = seg > 0 ? d / seg : 1
                return (CGPoint(x: a.x + (b.x - a.x) * t, y: a.y + (b.y - a.y) * t), i)
            }
            d -= seg
        }
        return (pts[pts.count - 1], pts.count - 1)
    }

    /// The rest of a path from fraction f on: the point there, then the later corners.
    public static func rest(_ pts: [CGPoint], from f: Double) -> [CGPoint] {
        let (p, i) = point(pts, at: f)
        return [p] + pts.dropFirst(max(i, 1))
    }

    /// The facing on the segment the point at fraction f lies on.
    public static func facing(_ pts: [CGPoint], at f: Double) -> Facing {
        let (_, i) = point(pts, at: f)
        guard i >= 1, i < pts.count else { return .down }
        var j = i
        while j < pts.count, hypot(pts[j].x - pts[j - 1].x, pts[j].y - pts[j - 1].y) < 1 { j += 1 }
        return j < pts.count ? Facing.along(pts[j - 1], pts[j]) : .down
    }
}
