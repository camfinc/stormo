import CoreGraphics
import Foundation

/// Where a person is drawn this frame.
public struct PersonFrame: Sendable, Equatable {
    public var point: CGPoint
    public var pose: Pose
    public var facing: Facing
    /// Off shift: nobody at the desk, the chair tucked in.
    public var away: Bool
    /// On the move this frame.
    public var walking: Bool

    public init(point: CGPoint, pose: Pose, facing: Facing, away: Bool = false, walking: Bool = false) {
        self.point = point
        self.pose = pose
        self.facing = facing
        self.away = away
        self.walking = walking
    }
}

/// Where the robot is drawn this frame.
public struct RobotFrame: Sendable, Equatable {
    public var point: CGPoint
    public var facingLeft: Bool
    public var rolling: Bool
    /// At a desk, writing its note.
    public var writing: Bool
}

/// Plays the core's office timeline back by server time (pkg/core/office.go decides; every viewer
/// shows the same thing). A new `seq` starts a phase from where it is in time, so a viewer who opens
/// mid-walk joins mid-walk; a walk back starts from where the person is.
public final class OfficePlayback {
    private struct Walk {
        var path: [CGPoint]
        var start: Int64
        var end: Int64
        /// What to hold once the walk is over.
        var then: Hold
    }

    private enum Hold {
        case seated
        case spot(FloorPlan.Spot)
        case away
    }

    private struct Person {
        var seq = -1
        var walk: Walk?
        var hold: Hold = .seated
    }

    private var people: [String: Person] = [:]
    public var reduceMotion = false

    public init() {}

    /// Everyone re-joins the timeline (a new floor plan moves every chair).
    public func reset() { people = [:] }

    private static let lobbyTrips: Set<String> = ["coffee", "water", "sofa"]
    private static let trips: Set<String> = ["coffee", "water", "sofa", "window", "stretch"]

    public func frame(for agent: FleetAgent, plan: FloorPlan, now: Int64) -> PersonFrame? {
        guard let seat = plan.workstation(of: agent.id)?.seat else { return nil }
        var p = people[agent.id] ?? Person()
        defer { people[agent.id] = p }
        let mode = OfficeLook.mode(of: agent)

        guard let o = agent.office else {
            // A core without the office simulation: at the desk or gone.
            p.walk = nil
            return PersonFrame(point: seat, pose: mode == .busy ? .sitType : .sitIdle, facing: .up, away: !mode.present, walking: false)
        }

        if o.seq != p.seq {
            let first = p.seq < 0
            p.seq = o.seq
            start(&p, o, agent: agent.id, plan: plan, seat: seat, now: now, first: first)
        }

        // Walking this frame?
        if let w = p.walk {
            if now < w.end, !reduceMotion, w.path.count > 1 {
                let f = Double(now - w.start) / Double(max(1, w.end - w.start))
                let (pt, _) = Polyline.point(w.path, at: f)
                return PersonFrame(point: pt, pose: .walk, facing: Polyline.facing(w.path, at: f), away: false, walking: true)
            }
            p.hold = w.then
            p.walk = nil
        }
        switch p.hold {
        case .seated:
            let pose: Pose = o.activity == "relax" ? .sitRelax : (o.working ? .sitType : .sitIdle)
            return PersonFrame(point: seat, pose: pose, facing: .up, away: o.activity == "away", walking: false)
        case .spot(let s):
            return PersonFrame(point: s.point, pose: s.pose, facing: s.facing, away: false, walking: false)
        case .away:
            return PersonFrame(point: seat, pose: .sitIdle, facing: .up, away: true, walking: false)
        }
    }

    /// Where this person is now, for a walk that turns back halfway.
    private func here(_ p: Person, now: Int64) -> (CGPoint, [CGPoint])? {
        guard let w = p.walk, now < w.end, w.path.count > 1 else { return nil }
        let f = Double(now - w.start) / Double(max(1, w.end - w.start))
        let (pt, seg) = Polyline.point(w.path, at: f)
        return (pt, Array(w.path.prefix(seg)).reversed())
    }

    private func start(_ p: inout Person, _ o: OfficeState, agent: String, plan: FloorPlan, seat: CGPoint, now: Int64, first: Bool) {
        let end = o.end ?? o.start
        switch o.activity {
        case "away":
            p.walk = nil
            p.hold = .away
            return
        case "desk", "relax":
            // Already walking back on the same clock: let that walk finish.
            if !first, let w = p.walk, now < w.end { return }
            p.walk = nil
            p.hold = .seated
            return
        default: break
        }
        let entrance = plan.spot("entrance", for: agent)
        let toEntrance = entrance.flatMap { plan.path(for: agent, to: $0) }

        if o.activity == "arrive" {
            guard let toEntrance else { p.walk = nil; p.hold = .seated; return }
            p.walk = Walk(path: toEntrance.reversed(), start: o.start, end: end, then: .seated)
            return
        }
        if o.activity == "leave" {
            let from = o.from ?? ""
            let spot = Self.trips.contains(from) ? plan.spot(from, for: agent, slot: o.slot) : nil
            let lobbyTrip = Self.lobbyTrips.contains(from)
            var out: [CGPoint]
            if let (pt, back) = here(p, now: now) {
                out = [pt] + (spot != nil && lobbyTrip ? [] : back)
            } else if let spot, lobbyTrip {
                out = [spot.point]
            } else {
                out = [seat]
            }
            let tail: [CGPoint] = lobbyTrip ? (entrance.map { [$0.point] } ?? []) : Array((toEntrance ?? []).dropFirst())
            let path = out + tail
            guard entrance != nil, path.count > 1 else { p.walk = nil; p.hold = .away; return }
            p.walk = Walk(path: path, start: first ? o.start : now, end: end, then: .away)
            return
        }

        // Trips: going, there, returning.
        guard let spot = plan.spot(o.activity, for: agent, slot: o.slot), let path = plan.path(for: agent, to: spot) else {
            p.walk = nil
            p.hold = .seated
            return
        }
        switch o.phase {
        case "going":
            p.walk = Walk(path: path, start: o.start, end: end, then: .spot(spot))
        case "there":
            p.walk = nil
            p.hold = .spot(spot)
        default:  // returning
            if !first, let (pt, back) = here(p, now: now) {
                p.walk = Walk(path: [pt] + back, start: now, end: end, then: .seated)
            } else {
                p.walk = Walk(path: path.reversed(), start: first ? o.start : now, end: end, then: .seated)
            }
        }
    }

    // MARK: The sync robot

    /// The robot's place: docked, at a desk writing, or rolling between them through the lobby hub.
    public func robotFrame(_ r: RobotState, plan: FloorPlan, now: Int64) -> RobotFrame {
        let dockPoint = plan.toHub("dock")?.first ?? plan.dock
        func at(_ node: String?) -> CGPoint {
            guard let node, let pts = plan.toHub(node) else { return dockPoint }
            return pts[0]
        }
        switch r.phase {
        case "docked":
            return RobotFrame(point: dockPoint, facingLeft: false, rolling: false, writing: false)
        case "there":
            return RobotFrame(point: at(r.target), facingLeft: false, rolling: false, writing: true)
        default:
            guard let from = r.from, let route = plan.robotRoute(from: from, to: r.target ?? "dock"),
                  let start = r.start, let end = r.end, !reduceMotion, now < end
            else {
                return RobotFrame(point: r.phase == "returning" ? dockPoint : at(r.target), facingLeft: false, rolling: false, writing: false)
            }
            let f = Double(now - start) / Double(max(1, end - start))
            let (pt, _) = Polyline.point(route, at: f)
            let facing = Polyline.facing(route, at: f)
            return RobotFrame(point: pt, facingLeft: facing == .left, rolling: true, writing: false)
        }
    }
}
