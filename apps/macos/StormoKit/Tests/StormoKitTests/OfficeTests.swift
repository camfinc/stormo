import CoreGraphics
import Foundation
import Testing

@testable import StormoKit

private func demo() throws -> (Fleet, OfficeLook) {
    let fleet = try JSONDecoder().decode(Fleet.self, from: fixture("api_fleet"))
    let look = OfficeLook(instance: try JSONDecoder().decode(InstanceLook.self, from: fixture("api_instance")))
    return (fleet, look)
}

private func agent(_ id: String, unit: String = "sales", state: String = "running", office: OfficeState? = nil) -> FleetAgent {
    FleetAgent(id: id, name: id.capitalized, unit: unit, state: state, office: office)
}

@Suite struct Look {
    @Test func hashAndPicksMatchTheWebOffice() throws {
        // Reference values from app.js's hash (FNV-1a) and picks.
        #expect(OfficeLook.hash("atlas") == 3_407_328_204)
        #expect(OfficeLook.hash("scout") == 3_273_829_637)
        let bare = OfficeLook(instance: nil)
        #expect(bare.hue(unit: "sales") == "#e879f9" && bare.wall(unit: "sales") == "shelves" && bare.floor(unit: "sales") == "library")
        #expect(bare.hue(unit: "group") == OfficePalette.groupHue)
        let s = bare.sprite(for: agent("scout"))
        #expect(s.skin == "#8d5a3b" && s.hair == "#1f1a17" && s.hairStyle == "fade" && s.shirt == "#e879f9")
    }

    @Test func instanceLookWins() throws {
        let (fleet, look) = try demo()
        #expect(look.hue(unit: "sales") == "#2dd4bf" && look.floor(unit: "product") == "concrete" && look.wall(unit: "research") == "shelves")
        let byte = try #require(fleet.agents.first { $0.id == "byte" })
        // Two screens, no app of its own: the unit's app on both.
        #expect(look.desk(for: byte) == ResolvedDesk(app: "design", apps: ["design", "design"], props: ["laptop", "duck", "notes", "mug"], side: "whiteboard"))
        #expect(look.sprite(for: byte).hairStyle == "bald" && look.sprite(for: byte).accessory == "cap")
        #expect(look.shortName("Acme Sales") == "Sales" && look.shortName("Commons") == "Commons")
    }

    @Test func modesAndWords() {
        var a = agent("atlas", office: OfficeState(activity: "desk", phase: "seated", start: 0, end: nil, from: nil, slot: 0, label: "", working: true, seq: 1))
        a.activity = Activity(activeAgents: 1, gatewayBusy: false, source: "slack")
        #expect(OfficeLook.mode(of: a) == .busy && OfficeLook.statusText(a, serverNow: 0) == "Working · Slack")
        a.office?.working = false
        a.office?.label = "getting coffee"
        a.activity?.lastActive = "2026-10-09T04:00:00Z"
        let now = OfficeLook.epochMillis("2026-10-09T04:12:00Z")!
        #expect(OfficeLook.statusText(a, serverNow: now) == "Idle 12m · getting coffee")
        a.health = "unhealthy"
        #expect(OfficeLook.mode(of: a) == .alert)
        // Stopped on purpose is off shift; a container that is there and not running asks for a look.
        #expect(OfficeLook.mode(of: agent("x", state: "stopped")) == .away)
        let gone = agent("x", state: "exited")
        #expect(OfficeLook.mode(of: gone) == .down && gone.needsAttention && OfficeLook.statusText(gone, serverNow: 0) == "Exited")
        #expect(!OfficeMode.down.present)
        #expect(OfficeLook.sourceLabel("api_server") == "API run" && OfficeLook.sourceLabel("my_bot") == "my bot")
    }

    /// The counts are the modes: nobody is in two of working, idle and attention.
    @Test func summaryCountsEachAgentOnce() throws {
        let working = OfficeState(activity: "desk", phase: "seated", start: 0, end: nil, from: nil, slot: 0, label: "", working: true, seq: 1)
        var alerting = agent("sage", office: working)
        alerting.detail = "nap sidecar exited"
        var idle = agent("byte", office: working)
        idle.office?.working = false
        idle.pendingLearnings = 3
        var busy = agent("echo", office: working)
        busy.pendingSkills = 1
        let fleet = [alerting, idle, busy, agent("nova", state: "stopped"), agent("scout", state: "exited")]
        var f = try JSONDecoder().decode(Fleet.self, from: fixture("api_fleet"))
        f.agents = fleet
        let s = FleetSummary(f)
        #expect(s.total == 5 && s.onShift == 3 && s.working == 1 && s.idle == 1 && s.attention == 2)
        #expect(s.working + s.idle + s.attention + 1 == s.total)
        #expect(s.toReview == 2 && s.reviewItems == 4)
    }
}

@Suite struct Camera {
    @Test func zoomNeverLeavesWhatTheCameraShows() {
        var c = OfficeCamera(fit: 1)
        #expect(c.scale == 1 && c.maxZoom == 1.6)
        for _ in 0..<10 { c.setZoom(c.zoom * 1.25) }
        #expect(c.zoom == 1.6 && abs(c.scale - OfficeCamera.closest) < 1e-9)
        // One step out moves the camera at once: nothing was spent past the limit.
        c.setZoom(c.zoom / 1.25)
        #expect(c.scale > OfficeCamera.closest)
        for _ in 0..<10 { c.setZoom(c.zoom / 1.25) }
        #expect(c.zoom == 1 && !c.isZoomed)
        // A view too large for the floor: a margin, and no zoom to offer.
        let big = OfficeCamera(fit: 0.5, zoom: 3)
        #expect(big.scale == 0.6 && big.maxZoom == 1 && big.zoom == 1)
    }

    @Test func detailFollowsTheDistance() {
        #expect(OfficeDetail(cameraScale: 0.7) == .near && OfficeDetail(cameraScale: 1.4) == .mid && OfficeDetail(cameraScale: 2.4) == .far)
        #expect(OfficeDetail.labelScale(cameraScale: 0.7) == 1)
        // Out at 2 floor points per view point a label still shows at 0.9 of its size.
        #expect(abs(OfficeDetail.labelScale(cameraScale: 2) / 2 - 0.9) < 1e-9)
    }

    @Test func roomsCountTheirPeopleOnce() {
        let working = OfficeState(activity: "desk", phase: "seated", start: 0, end: nil, from: nil, slot: 0, label: "", working: true, seq: 1)
        var idle = agent("byte", office: working)
        idle.office?.working = false
        idle.pendingLearnings = 2
        let s = RoomSummary([agent("echo", office: working), idle, agent("nova", state: "stopped"), agent("scout", state: "exited")])
        #expect(s.total == 4 && s.working == 1 && s.idle == 1 && s.attention == 1 && s.offShift == 1 && s.toReview == 2)
        #expect(s.words == "1 working, 1 idle, 1 needs attention, 1 off shift, 2 to review")
        #expect(RoomSummary([]).words == "nobody here")
    }
}

@Suite struct Timeline {
    @Test func decodesAndAddsUpTheWork() throws {
        let json = #"{"now":7200000,"from":0,"gapMs":120000,"agents":{"atlas":[{"start":600000,"end":1320000,"events":9},{"start":3600000,"end":3600000,"events":1}]}}"#
        let t = try JSONDecoder().decode(FleetTimeline.self, from: Data(json.utf8))
        #expect(t.agents["atlas"]?.count == 2 && t.agents["nova"] == nil)
        // 12 minutes, and a lone event counted as half a minute.
        #expect(t.worked("atlas") == 750_000 && t.worked("nova") == 0)
        #expect(FleetTimeline.duration(750_000) == "13m" && FleetTimeline.duration(0) == "" && FleetTimeline.duration(20_000) == "under a minute")
        #expect(FleetTimeline.duration(72 * 60_000) == "1h 12m" && FleetTimeline.duration(120 * 60_000) == "2h")
    }

    @Test func axisPlacesMomentsAndRoundMarks() {
        let hour: Int64 = 3_600_000
        let axis = TimelineAxis(from: 10 * hour + 20 * 60_000, to: 16 * hour + 20 * 60_000, width: 600)
        #expect(axis.x(axis.from) == 0 && axis.x(axis.to) == 600 && axis.x(13 * hour + 20 * 60_000) == 300)
        #expect(axis.x(0) == 0 && axis.x(99 * hour) == 600)
        #expect(axis.time(at: 300) == 13 * hour + 20 * 60_000)
        // Six hours over 600 points: a mark an hour, on the hour.
        let utc = TimeZone(identifier: "UTC")!
        #expect(axis.marks(zone: utc) == (11...16).map { Int64($0) * hour })
        // Half-hour zones round on their own clock.
        let kolkata = TimeZone(identifier: "Asia/Kolkata")!
        #expect(axis.marks(zone: kolkata).allSatisfy { ($0 + 19_800_000) % hour == 0 })
        // A week: a mark a day.
        let week = TimelineAxis(from: 0, to: 7 * 24 * hour, width: 600)
        #expect(week.marks(zone: utc) == (1...6).map { Int64($0) * 24 * hour })
        // A lone event is still a bar, inside the lane.
        let lone = axis.bar(.init(start: axis.to, end: axis.to, events: 1))
        #expect(lone.width == 2 && lone.x == 598)
        let spans = [FleetTimeline.Span(start: 11 * hour, end: 11 * hour + 60_000, events: 3)]
        #expect(axis.span(at: 11 * hour - 30_000, in: spans, slop: 60_000) == spans[0] && axis.span(at: 12 * hour, in: spans, slop: 60_000) == nil)
    }
}

@Suite struct Floor {
    @Test func everyDeskIsInItsOfficeAndRoutesConnect() throws {
        let (fleet, look) = try demo()
        let plan = FloorPlan(units: fleet.units, agents: fleet.agents, look: look, clocks: ["Lisbon", "New York"])
        #expect(plan.offices.map(\.unit.id) == ["product", "sales", "research", "support"])
        #expect(plan.offices.filter { $0.side == .left }.map(\.unit.id) == ["product", "sales"])
        for a in fleet.agents {
            let ws = try #require(plan.workstation(of: a.id), "\(a.id)")
            let room = try #require(plan.office(ws.office))
            #expect(room.rect.contains(ws.rect), "\(a.id)")
            #expect(room.unit.id == a.unit)
            for trip in ["coffee", "water", "sofa", "window", "stretch"] {
                let spot = try #require(plan.spot(trip, for: a.id), "\(a.id) \(trip)")
                let path = try #require(plan.path(for: a.id, to: spot))
                #expect(path.first == ws.seat && path.last == spot.point)
                // Corners only: every leg is horizontal or vertical.
                for (p, q) in zip(path, path.dropFirst()) { #expect(p.x == q.x || p.y == q.y, "\(a.id) \(trip)") }
            }
            let route = try #require(plan.robotRoute(from: "dock", to: a.id))
            #expect(route.first == plan.dock)
            #expect(route.last == CGPoint(x: ws.seat.x + 46, y: ws.seat.y + 4))
        }
        // Each office has at least one row of two desks; an odd count gets one open desk.
        for o in plan.offices {
            let desks = plan.workstations.filter { $0.office == o.unit.id }
            #expect(desks.count >= 2 && desks.count % 2 == 0 && desks.filter { $0.agent == nil }.count <= 1)
        }
        #expect(plan.lobby.minY > plan.server.maxY && plan.racks.count == 4 && plan.clocks.count == 2)
        #expect(plan.size.width == 1400)
        // Two short clocks fit beside the lobby sign; four long ones get their own row, kitchen below.
        #expect(plan.clocks.allSatisfy { $0.minY < plan.lobby.minY + 10 })
        let crowded = FloorPlan(units: fleet.units, agents: fleet.agents, look: look, clocks: ["San Francisco", "New York", "Lisbon", "Singapore"])
        #expect(crowded.clocks.allSatisfy { $0.minY > crowded.lobby.minY + 30 && crowded.lobby.contains($0) })
        #expect(crowded.fridge.minY >= crowded.clocks[0].maxY)
        #expect(crowded.clocks[0].width > 56)
    }

    @Test func takesTheViewsShape() throws {
        let (fleet, look) = try demo()
        for aspect in [0.8, 1.2, 1.77, 2.6] as [CGFloat] {
            let plan = FloorPlan(units: fleet.units, agents: fleet.agents, look: look, clocks: ["Lisbon"], fitting: aspect)
            #expect(abs(plan.size.width / plan.size.height - aspect) < 0.01, "\(aspect)")
            for ws in plan.workstations where ws.office != nil {
                #expect(plan.office(ws.office)!.rect.contains(ws.rect), "\(aspect)")
            }
            for a in fleet.agents { #expect(plan.path(for: a.id, to: plan.spot("coffee", for: a.id)!) != nil) }
            #expect(abs(plan.lobby.maxY + 8 - plan.size.height) < 0.001)
        }
        // Past the limits it keeps a sane shape.
        let narrow = FloorPlan(units: fleet.units, agents: fleet.agents, look: look, fitting: 0.2)
        #expect(abs(narrow.size.width / narrow.size.height - FloorPlan.aspectRange.lowerBound) < 0.01)
    }

    @Test func lobbyAgentsAndOddUnits() {
        let units = [FleetUnit(id: "group", name: "G"), FleetUnit(id: "a", name: "A"), FleetUnit(id: "b", name: "B"), FleetUnit(id: "c", name: "C")]
        let agents = [agent("core", unit: "group"), agent("x", unit: "a")] + (1...7).map { agent("c\($0)", unit: "c") }
        let plan = FloorPlan(units: units, agents: agents, look: OfficeLook(instance: nil))
        // a and c on the left, b on the right running down both rows.
        #expect(plan.office("b")?.span == 2)
        #expect(plan.lobby.contains(plan.workstation(of: "core")!.rect))
        #expect(plan.spot("window", for: "core") == nil)
        #expect(plan.office("c")!.rect.contains(plan.workstation(of: "c7")!.rect))
    }

    @Test func polyline() {
        let pts = [CGPoint(x: 0, y: 0), CGPoint(x: 10, y: 0), CGPoint(x: 10, y: 10)]
        #expect(Polyline.length(pts) == 20)
        #expect(Polyline.point(pts, at: 0.75).point == CGPoint(x: 10, y: 5))
        #expect(Polyline.facing(pts, at: 0.25) == .right && Polyline.facing(pts, at: 0.75) == .down)
        #expect(Polyline.rest(pts, from: 0.25) == [CGPoint(x: 5, y: 0), CGPoint(x: 10, y: 0), CGPoint(x: 10, y: 10)])
    }
}

@Suite struct Play {
    func office(_ activity: String, _ phase: String, start: Int64, end: Int64?, seq: Int, working: Bool = false) -> OfficeState {
        OfficeState(activity: activity, phase: phase, start: start, end: end, from: nil, slot: 0, label: "", working: working, seq: seq)
    }

    @Test func joinsMidWalkThenHoldsTheSpotThenTurnsBack() throws {
        let (fleet, look) = try demo()
        let plan = FloorPlan(units: fleet.units, agents: fleet.agents, look: look)
        let seat = plan.workstation(of: "atlas")!.seat
        let spot = plan.spot("coffee", for: "atlas")!
        let playback = OfficePlayback()
        var atlas = fleet.agents.first { $0.id == "atlas" }!

        atlas.office = office("coffee", "going", start: 0, end: 10_000, seq: 5)
        let mid = try #require(playback.frame(for: atlas, plan: plan, now: 5_000))
        #expect(mid.walking && mid.pose == .walk && mid.point != seat && mid.point != spot.point)
        let there = try #require(playback.frame(for: atlas, plan: plan, now: 10_001))
        #expect(!there.walking && there.point == spot.point && there.pose == .coffee)

        // Work arrives while walking out again: the walk back starts where the person is.
        atlas.office = office("coffee", "going", start: 20_000, end: 30_000, seq: 6)
        let out = try #require(playback.frame(for: atlas, plan: plan, now: 25_000))
        atlas.office = office("coffee", "returning", start: 25_000, end: 27_000, seq: 7, working: true)
        let back = try #require(playback.frame(for: atlas, plan: plan, now: 25_000))
        #expect(back.walking && abs(back.point.x - out.point.x) < 1 && abs(back.point.y - out.point.y) < 1)
        let home = try #require(playback.frame(for: atlas, plan: plan, now: 27_001))
        #expect(home.point == seat && home.pose == .sitType && !home.away)
    }

    @Test func awayAndReduceMotion() throws {
        let (fleet, look) = try demo()
        let plan = FloorPlan(units: fleet.units, agents: fleet.agents, look: look)
        let playback = OfficePlayback()
        playback.reduceMotion = true
        var scout = fleet.agents.first { $0.id == "scout" }!
        scout.office = office("arrive", "going", start: 0, end: 10_000, seq: 1)
        let f = try #require(playback.frame(for: scout, plan: plan, now: 1_000))
        #expect(!f.walking && f.point == plan.workstation(of: "scout")!.seat)
        scout.office = office("away", "seated", start: 0, end: nil, seq: 2)
        #expect(try #require(playback.frame(for: scout, plan: plan, now: 2_000)).away)
    }

    @Test func robotRollsThroughTheHub() throws {
        let (fleet, look) = try demo()
        let plan = FloorPlan(units: fleet.units, agents: fleet.agents, look: look)
        let playback = OfficePlayback()
        let going = RobotState(phase: "going", from: "dock", target: "atlas", start: 0, end: 7_000, seq: 1)
        let mid = playback.robotFrame(going, plan: plan, now: 3_500)
        #expect(mid.rolling && !mid.writing)
        let there = playback.robotFrame(RobotState(phase: "there", from: "dock", target: "atlas", seq: 2), plan: plan, now: 8_000)
        #expect(there.writing && there.point == plan.toHub("atlas")!.first)
        #expect(playback.robotFrame(RobotState(phase: "docked", seq: 3), plan: plan, now: 9_000).point == plan.dock)
    }
}

@Suite struct Paths {
    @Test func parsesCompactRelativeData() {
        #expect(SVGPath.tokenize("l2.4-3.4") == [.command("l"), .number(2.4), .number(-3.4)])
        #expect(SVGPath.tokenize("M23 18.4h4M1.5.5") == [.command("M"), .number(23), .number(18.4), .command("h"), .number(4), .command("M"), .number(1.5), .number(0.5)])
        let box = SVGPath.parse("M19.1 15H20.9").boundingBox
        #expect(abs(box.minX - 19.1) < 0.001 && abs(box.maxX - 20.9) < 0.001)
        let hair = SVGPath.parse("M11 13.5Q10.5 4 20 4Q29.5 4 29 13.5Q26.5 8.5 20 8.8Q13.5 8.5 11 13.5Z")
        #expect(hair.boundingBox.width > 15 && hair.boundingBox.height > 5)
    }
}
