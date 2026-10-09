import AppKit
import SpriteKit
import StormoKit

/// What the office's inspector shows.
enum OfficeSelection: Hashable {
    case agent(String)
    case core
    case robot
    case vacant(String)
}

/// The office floor as a SpriteKit scene. The building is one texture; desks' screens and chairs,
/// the racks' lights, clocks, people and the robot are nodes over it, moved by server time.
final class OfficeScene: SKScene {
    var onSelect: (OfficeSelection?) -> Void = { _ in }
    var menuForAgent: (String) -> NSMenu? = { _ in nil }
    var selection: OfficeSelection? { didSet { showSelection() } }
    var reduceMotion = false {
        didSet {
            guard reduceMotion != oldValue else { return }
            playback.reduceMotion = reduceMotion
            refresh(force: true)
        }
    }

    private(set) var plan: FloorPlan?
    private let world = SKNode()
    private let cam = SKCameraNode()
    private let playback = OfficePlayback()
    private var fleet: Fleet?
    private var look = OfficeLook(instance: nil)
    private var clockOffset: Int64 = 0
    private var avatars: [String: NSImage] = [:]
    private var rosterKey = ""

    private var people: [String: PersonNode] = [:]
    private var chairs: [String: SKSpriteNode] = [:]
    private var chairState: [String: String] = [:]
    private var screens: [String: [ScreenNode]] = [:]
    private var stickies: [String: SKSpriteNode] = [:]
    private var tags: [String: SKSpriteNode] = [:]
    private var tagKeys: [String: String] = [:]
    private var bubbles: [String: SKSpriteNode] = [:]
    private var reviewMarkers: [String: SKSpriteNode] = [:]
    private var alertMarkers: [String: SKSpriteNode] = [:]
    private var arcs: [String: SKShapeNode] = [:]
    private var robot: RobotNode?
    private var coreLEDs: [SKSpriteNode] = []
    private var slots: [SKSpriteNode] = []
    private var blades: [SKSpriteNode] = []
    private var rackSub = SKSpriteNode()
    private var rackHighlight = SKShapeNode()
    private var clocks: [(node: SKSpriteNode, clock: InstanceLook.Clock, rect: CGRect)] = []
    private var lastSecond = 0
    private var userZoom: CGFloat = 1
    private var panOffset = CGPoint.zero

    override init(size: CGSize) {
        super.init(size: size)
        scaleMode = .resizeFill
        // The floor fills the view (it is laid out to the view's shape); while a resize is under way
        // the gap, if any, is the colour of its outer wall, so it reads as more wall.
        backgroundColor = Art.nsColor(BuildingArt.wallColor)
        addChild(world)
        addChild(cam)
        camera = cam
    }

    @available(*, unavailable) required init?(coder: NSCoder) { fatalError() }

    override func didChangeSize(_ oldSize: CGSize) {
        super.didChangeSize(oldSize)
        placeCamera()
        // Once the resize settles, lay the floor out again for the new shape.
        guard let plan, size.width > 0, size.height > 0 else { return }
        let want = min(max(size.width / size.height, FloorPlan.aspectRange.lowerBound), FloorPlan.aspectRange.upperBound)
        guard abs(plan.size.width / plan.size.height - want) > 0.01 else { return }
        removeAction(forKey: "relayout")
        run(.sequence([.wait(forDuration: 0.3), .run { [weak self] in self?.rebuild() }]), withKey: "relayout")
    }

    // MARK: Data

    func apply(fleet: Fleet, look: OfficeLook, clockOffset: Int64, avatars: [String: NSImage]) {
        let avatarsChanged = Set(avatars.keys) != Set(self.avatars.keys)
        self.fleet = fleet
        self.look = look
        self.clockOffset = clockOffset
        self.avatars = avatars
        let key = Self.rosterKey(fleet, look)
        if key != rosterKey {
            rosterKey = key
            rebuild()
        }
        refresh(force: avatarsChanged)
    }

    /// Everything the building texture and the nodes' make-up depend on.
    private static func rosterKey(_ f: Fleet, _ look: OfficeLook) -> String {
        let units = f.units.map { "\($0.id):\($0.name):\($0.description ?? "")" }.joined(separator: ",")
        let agents = f.agents.map { a in
            let d = look.desk(for: a), s = look.sprite(for: a)
            return [a.id, a.unit, a.name, d.apps.joined(separator: "+"), d.props.joined(separator: "+"), d.side,
                    s.skin, s.hair, s.hairStyle, s.shirt, s.pants, s.accessory].joined(separator: ":")
        }.joined(separator: ",")
        let inst = look.instance.map { i in "\(i.name)|\(i.org)|\(i.clocks.map(\.city))|\(i.units.keys.sorted().map { "\($0)=\(i.units[$0]!)" })" } ?? ""
        return "\(units)|\(agents)|\(f.robot != nil)|\(inst)"
    }

    var serverNow: Int64 { Int64(Date().timeIntervalSince1970 * 1000) + clockOffset }

    /// Floor (y down) → scene (y up).
    private func sp(_ p: CGPoint) -> CGPoint { CGPoint(x: p.x, y: (plan?.size.height ?? 0) - p.y) }

    // MARK: Building the floor

    private func rebuild() {
        guard let fleet else { return }
        world.removeAllChildren()
        people = [:]; chairs = [:]; chairState = [:]; screens = [:]; stickies = [:]; tags = [:]; tagKeys = [:]
        bubbles = [:]; reviewMarkers = [:]; alertMarkers = [:]; arcs = [:]; robot = nil; coreLEDs = []; slots = []; blades = []; clocks = []
        playback.reset()

        let clockList = look.instance?.clocks ?? []
        let aspect = size.width > 0 && size.height > 0 ? size.width / size.height : nil
        let plan = FloorPlan(units: fleet.units, agents: fleet.agents, look: look, clocks: clockList.map(\.city), fitting: aspect)
        self.plan = plan
        let byID = Dictionary(uniqueKeysWithValues: fleet.agents.map { ($0.id, $0) })
        let groupName = fleet.units.first { $0.id == "group" }?.name ?? "Commons"

        let building = SKSpriteNode(texture: BuildingArt.texture(.init(plan: plan, look: look, agents: byID, groupName: groupName)), size: plan.size)
        building.anchorPoint = .zero
        building.zPosition = 0
        world.addChild(building)

        for ws in plan.workstations {
            let hit = SKSpriteNode(color: .clear, size: CGSize(width: ws.rect.width, height: ws.rect.height - 20))
            hit.position = sp(CGPoint(x: ws.rect.midX, y: ws.rect.midY - 10))
            hit.zPosition = 0.5
            hit.name = ws.agent.map { "desk:\($0)" } ?? "vacant:\(ws.unit)"
            world.addChild(hit)
            let chair = SKSpriteNode(texture: Self.chairTexture, size: CGSize(width: 48, height: 46))
            chair.position = sp(ws.seat)
            chair.zPosition = 1
            if ws.agent == nil { chair.alpha = 0.6; chair.position.y += 6 }
            world.addChild(chair)
            guard let id = ws.agent, let agent = byID[id] else { continue }
            chairs[id] = chair
            let desk = look.desk(for: agent)
            let hue = look.hue(unit: agent.unit)
            screens[id] = OfficeGeometry.monitors(ws, count: desk.apps.count).enumerated().map { i, m in
                let inner = m.rect.insetBy(dx: 3, dy: 3)
                let node = ScreenNode(app: desk.apps[i], size: inner.size, hue: hue)
                node.position = sp(CGPoint(x: m.rect.midX, y: m.rect.midY))
                node.zRotation = -m.angle
                node.zPosition = 2
                world.addChild(node)
                return node
            }
            let sticky = SKSpriteNode(texture: LabelArt.sticky())
            sticky.size = sticky.texture!.size().applying(CGAffineTransform(scaleX: 1 / Art.scale, y: 1 / Art.scale))
            sticky.position = sp(CGPoint(x: ws.rect.minX + 78, y: ws.rect.minY + 26))
            sticky.zRotation = 4 * .pi / 180
            sticky.zPosition = 3
            sticky.isHidden = true
            world.addChild(sticky)
            stickies[id] = sticky

            let person = PersonNode(agentID: id, sprite: look.sprite(for: agent))
            person.zPosition = 10
            person.position = sp(ws.seat)
            person.isAccessibilityElement = true
            person.accessibilityRole = NSAccessibility.Role.button.rawValue
            world.addChild(person)
            people[id] = person
            for (dict, z) in [(\OfficeScene.tags, 20.0), (\OfficeScene.bubbles, 21), (\OfficeScene.reviewMarkers, 22), (\OfficeScene.alertMarkers, 22)] {
                let n = SKSpriteNode()
                n.zPosition = z
                n.isHidden = true
                world.addChild(n)
                self[keyPath: dict][id] = n
            }
            tags[id]?.anchorPoint = CGPoint(x: 0.5, y: 1)
            tags[id]?.name = "tag:\(id)"
            bubbles[id]?.anchorPoint = CGPoint(x: 0, y: 1)
            reviewMarkers[id]?.anchorPoint = CGPoint(x: 0, y: 1)
            alertMarkers[id]?.anchorPoint = CGPoint(x: 1, y: 1)
        }

        buildRacks(plan, agents: fleet.agents.count)
        for (i, rect) in plan.clocks.enumerated() where i < clockList.count {
            let n = SKSpriteNode()
            n.size = rect.size
            n.position = sp(CGPoint(x: rect.midX, y: rect.midY))
            n.zPosition = 2
            world.addChild(n)
            clocks.append((n, clockList[i], rect))
        }
        lastSecond = 0
        if fleet.robot != nil {
            let r = RobotNode()
            r.zPosition = 23  // over name tags and bubbles: its note is read while it writes
            r.position = sp(plan.dock)
            world.addChild(r)
            robot = r
        }
        placeCamera()
        showSelection()
    }

    private static let chairTexture: SKTexture = Art.texture(CGSize(width: 48, height: 46)) { ctx in
        let r = CGRect(x: 4, y: 3, width: 40, height: 36)
        let path = CGMutablePath()
        path.addPath(CGPath(roundedRect: r, cornerWidth: 13, cornerHeight: 13, transform: nil))
        Art.shadowed(ctx, blur: 8, offset: CGSize(width: 0, height: 4), alpha: 0.4) {
            ctx.setFillColor(Art.color("#1b242e"))
            ctx.addPath(path)
            ctx.fillPath()
        }
        Art.stroke(ctx, r, Art.color("#26313d"), width: 2, radius: 13)
    }

    private static let ledTexture: SKTexture = Art.texture(CGSize(width: 8, height: 8)) { ctx in
        ctx.setShadow(offset: .zero, blur: 3, color: .white)
        Art.circle(ctx, CGPoint(x: 4, y: 4), 1.8, .white)
    }

    private func buildRacks(_ plan: FloorPlan, agents: Int) {
        for rack in plan.racks {
            if rack.kind == .core {
                let hit = SKSpriteNode(color: .clear, size: rack.rect.size)
                hit.position = sp(CGPoint(x: rack.rect.midX, y: rack.rect.midY))
                hit.zPosition = 2.5
                hit.name = "rack"
                world.addChild(hit)
                rackHighlight = SKShapeNode(rect: CGRect(origin: .zero, size: rack.rect.size.applying(.init(scaleX: 1, y: 1))).insetBy(dx: -4, dy: -4), cornerRadius: 9)
                rackHighlight.position = sp(CGPoint(x: rack.rect.minX, y: rack.rect.maxY))
                rackHighlight.strokeColor = Art.nsColor("#e8eef5", 0.7)
                rackHighlight.lineWidth = 2
                rackHighlight.zPosition = 2.4
                rackHighlight.isHidden = true
                world.addChild(rackHighlight)
                rackSub.anchorPoint = CGPoint(x: 0.5, y: 1)
                rackSub.position = sp(CGPoint(x: rack.rect.midX, y: OfficeGeometry.rackLabelY(rack) + 15))
                rackSub.zPosition = 2
                world.addChild(rackSub)
            }
            for unit in OfficeGeometry.rackUnits(rack) {
                for (i, p) in OfficeGeometry.leds(unit, core: rack.kind == .core).enumerated() {
                    let led = SKSpriteNode(texture: Self.ledTexture, size: CGSize(width: rack.kind == .core ? 9 : 7, height: rack.kind == .core ? 9 : 7))
                    led.position = sp(p)
                    led.zPosition = 2
                    led.colorBlendFactor = 1
                    switch rack.kind {
                    case .ups:
                        led.color = Art.nsColor("#60a5fa")
                        led.alpha = 0.8
                    case .net:
                        led.color = Art.nsColor(i == 1 ? OfficePalette.warn : OfficePalette.ok)
                    case .core:
                        led.color = Art.nsColor(OfficePalette.ok)
                        coreLEDs.append(led)
                    case .fleet:
                        break
                    }
                    if rack.kind != .ups, !reduceMotion {
                        let period = i == 2 ? 1.7 : (i == 0 ? 3.1 : 2.4)
                        let blink = SKAction.sequence([.fadeAlpha(to: 0.25, duration: 0), .wait(forDuration: period * 0.4),
                                                       .fadeAlpha(to: 1, duration: 0), .wait(forDuration: period * 0.3),
                                                       .fadeAlpha(to: 0.45, duration: 0), .wait(forDuration: period * 0.3)])
                        led.run(.sequence([.wait(forDuration: Double(OfficeLook.hash("\(unit.minY)-\(i)") % 100) / 100 * period), .repeatForever(blink)]))
                    }
                    world.addChild(led)
                }
            }
        }
        let crac = OfficeGeometry.crac(plan)
        let fan = SKSpriteNode(texture: Self.fanTexture, size: CGSize(width: 20, height: 20))
        fan.position = sp(CGPoint(x: crac.midX, y: crac.midY))
        fan.zPosition = 2
        if !reduceMotion { fan.run(.repeatForever(.rotate(byAngle: -2 * .pi, duration: 1.2))) }
        world.addChild(fan)
    }

    private static let fanTexture: SKTexture = Art.texture(CGSize(width: 20, height: 20)) { ctx in
        let c = CGPoint(x: 10, y: 10)
        for i in 0..<6 {
            ctx.setFillColor(Art.color(i % 2 == 0 ? "#475569" : "#94a3b8"))
            ctx.move(to: c)
            ctx.addArc(center: c, radius: 10, startAngle: CGFloat(i) * .pi / 3, endAngle: CGFloat(i + 1) * .pi / 3, clockwise: false)
            ctx.fillPath()
        }
    }

    // MARK: Refreshing from the fleet

    private func refresh(force: Bool = false) {
        guard let fleet, let plan else { return }
        let gw = fleet.gateway
        let now = serverNow
        for a in fleet.agents {
            let mode = OfficeLook.mode(of: a)
            for s in screens[a.id] ?? [] { s.set(mode, still: reduceMotion) }
            updateTag(a, mode: mode, now: now, force: force)
            let r = (a.pendingLearnings ?? 0) + (a.pendingSkills ?? 0)
            if let m = reviewMarkers[a.id] {
                m.isHidden = r == 0
                if r > 0 { setTexture(m, LabelArt.marker("\(r)", alert: false)) }
            }
            if let m = alertMarkers[a.id] {
                m.isHidden = mode != .alert
                if mode == .alert { setTexture(m, LabelArt.marker("!", alert: true)) }
            }
            if let b = bubbles[a.id] {
                b.isHidden = mode != .busy
                if mode == .busy { setTexture(b, LabelArt.bubble(OfficeLook.workLabel(a), thinking: (gw.active?[a.id] ?? 0) > 0)) }
            }
            people[a.id]?.alpha = mode == .unknown ? 0.55 : 1
            people[a.id]?.accessibilityLabel = "\(a.name), \(OfficeLook.statusText(a, serverNow: now))"
        }
        // The core rack: its colour says the plan's state; slots say calls in flight.
        let limited = gw.planLimitedUntil != nil
        let rackColor = limited ? OfficePalette.bad : (gw.login == "ok" ? OfficePalette.ok : OfficePalette.warn)
        for led in coreLEDs { led.color = Art.nsColor(rackColor) }
        let sub: String
        if limited, let until = gw.planLimitedUntil.flatMap(OfficeLook.epochMillis) {
            sub = "plan limit · \(Date(timeIntervalSince1970: Double(until) / 1000).formatted(date: .omitted, time: .shortened))"
        } else {
            sub = gw.login == "ok" ? "Using ChatGPT plan" : "not signed in"
        }
        if let core = plan.racks.first(where: { $0.kind == .core }) {
            setTexture(rackSub, LabelArt.rackSub(sub, color: rackColor, width: core.rect.width + 20))
            if slots.count != gw.concurrency {
                slots.forEach { $0.removeFromParent() }
                slots = OfficeGeometry.slots(core, count: gw.concurrency).map { r in
                    let n = SKSpriteNode(color: Art.nsColor("#233140"), size: r.size)
                    n.position = sp(CGPoint(x: r.midX, y: r.midY))
                    n.zPosition = 2
                    world.addChild(n)
                    return n
                }
            }
            for (i, s) in slots.enumerated() { s.color = Art.nsColor(i < gw.inflight ? OfficePalette.busy : "#233140") }
        }
        if let fleetRack = plan.racks.first(where: { $0.kind == .fleet }) {
            let rects = OfficeGeometry.blades(fleetRack, count: fleet.agents.count)
            if blades.count != rects.count {
                blades.forEach { $0.removeFromParent() }
                blades = rects.map { r in
                    let n = SKSpriteNode()
                    n.size = r.size
                    n.position = sp(CGPoint(x: r.midX, y: r.midY))
                    n.zPosition = 2
                    world.addChild(n)
                    return n
                }
            }
            for (i, a) in fleet.agents.enumerated() where i < blades.count {
                blades[i].texture = LabelArt.blade(a.id, color: OfficeLook.mode(of: a).color, size: rects[i].size)
            }
        }
        // Model calls in flight through the core: a dotted arc from the rack to the agent's screen.
        for a in fleet.agents {
            let active = (gw.active?[a.id] ?? 0) > 0
            if active, arcs[a.id] == nil {
                let n = SKShapeNode()
                n.strokeColor = Art.nsColor(look.hue(unit: a.unit), 0.9)
                n.lineWidth = 2.5
                n.lineCap = .round
                n.zPosition = 9
                world.addChild(n)
                arcs[a.id] = n
            } else if !active, let n = arcs[a.id] {
                n.removeFromParent()
                arcs[a.id] = nil
            }
        }
        showSelection()
    }

    private func setTexture(_ node: SKSpriteNode, _ t: SKTexture) {
        guard node.texture !== t else { return }
        node.texture = t
        node.size = t.size().applying(CGAffineTransform(scaleX: 1 / Art.scale, y: 1 / Art.scale))
    }

    private func updateTag(_ a: FleetAgent, mode: OfficeMode, now: Int64, force: Bool) {
        guard let node = tags[a.id] else { return }
        let status = OfficeLook.statusText(a, serverNow: now)
        let key = "\(status)|\(mode.color)|\(avatars[a.id] != nil)"
        guard force || tagKeys[a.id] != key else { return }
        tagKeys[a.id] = key
        setTexture(node, LabelArt.tag(name: a.name, status: status, ring: mode.color, hue: look.hue(unit: a.unit), avatar: avatars[a.id]))
        node.isHidden = false
    }

    // MARK: Every frame

    override func update(_ currentTime: TimeInterval) {
        guard let fleet, let plan else { return }
        let now = serverNow
        for a in fleet.agents {
            guard let person = people[a.id], let frame = playback.frame(for: a, plan: plan, now: now) else { continue }
            person.position = sp(frame.point)
            person.apply(frame, reduceMotion: reduceMotion)
            person.zPosition = frame.walking || !frame.pose.seated ? 11 : 10
            let p = frame.point
            if let t = tags[a.id] {
                t.position = sp(CGPoint(x: p.x, y: p.y + 24))
                t.isHidden = frame.away
            }
            bubbles[a.id]?.position = sp(CGPoint(x: p.x + 9, y: p.y - 50))
            followMarkers(a.id, p, time: currentTime)
            if frame.away { bubbles[a.id]?.isHidden = true }
            stickies[a.id]?.isHidden = !frame.away
            moveChair(a.id, frame)
            if let arc = arcs[a.id], let screen = screens[a.id]?.first, let core = plan.racks.first(where: { $0.kind == .core }) {
                let from = sp(CGPoint(x: core.rect.midX, y: core.rect.midY)), to = screen.position
                let path = CGMutablePath()
                path.move(to: from)
                path.addQuadCurve(to: to, control: CGPoint(x: (from.x + to.x) / 2, y: max(from.y, to.y) + 60))
                let phase = reduceMotion ? 0 : CGFloat(currentTime.truncatingRemainder(dividingBy: 0.9) / 0.9) * -28
                arc.path = path.copy(dashingWithPhase: phase, lengths: [2, 12])
            }
        }
        if let robot, let r = fleet.robot {
            let f = playback.robotFrame(r, plan: plan, now: now)
            robot.position = sp(f.point)
            robot.apply(f, note: noteText(r.note), still: reduceMotion)
        }
        let second = Int(Date().timeIntervalSince1970)
        if second != lastSecond {
            lastSecond = second
            tickClocks(now: now)
            if second % 15 == 0 {
                for a in fleet.agents { updateTag(a, mode: OfficeLook.mode(of: a), now: now, force: false) }
            }
        }
    }

    /// Markers ride above the person's head, bobbing gently (1.8 s).
    private func followMarkers(_ id: String, _ p: CGPoint, time: TimeInterval) {
        let base = sp(CGPoint(x: p.x, y: p.y - 46))
        let bob = reduceMotion ? 0 : 1.5 * (1 - cos(time * 2 * .pi / 1.8))
        reviewMarkers[id]?.position = CGPoint(x: base.x - 35, y: base.y + bob)
        alertMarkers[id]?.position = CGPoint(x: base.x + 35, y: base.y + bob)
    }

    private func moveChair(_ id: String, _ f: PersonFrame) {
        guard let chair = chairs[id], let seat = plan?.workstation(of: id)?.seat else { return }
        let state = f.away ? "away" : f.pose == .sitRelax ? "relax" : f.pose.seated ? "seated" : "empty"
        guard state != chairState[id] else { return }
        let first = chairState[id] == nil
        chairState[id] = state
        var p = sp(seat), angle: CGFloat = 0
        switch state {
        case "away": p.y += 6
        case "relax": p.y -= 3; angle = -7 * .pi / 180
        case "empty": p.x -= 7; p.y -= 8; angle = 12 * .pi / 180
        default: break
        }
        chair.removeAllActions()
        if first || reduceMotion {
            chair.position = p
            chair.zRotation = angle
        } else {
            chair.run(.group([.move(to: p, duration: 0.5), .rotate(toAngle: angle, duration: 0.5, shortestUnitArc: true)]))
        }
    }

    private func tickClocks(now: Int64) {
        let date = Date(timeIntervalSince1970: Double(now) / 1000)
        for c in clocks {
            guard let tz = TimeZone(identifier: c.clock.tz) else { continue }
            var cal = Calendar(identifier: .gregorian)
            cal.timeZone = tz
            let h = cal.component(.hour, from: date), m = cal.component(.minute, from: date)
            let time = String(format: "%02d:%02d", h, m)
            let night = h < 7 || h >= 19
            let on = LabelArt.clock(city: c.clock.city, time: time, night: night, colon: true, size: c.rect.size)
            let off = LabelArt.clock(city: c.clock.city, time: time, night: night, colon: false, size: c.rect.size)
            if c.node.userData?["time"] as? String != time {
                c.node.userData = ["time": time]
                c.node.removeAllActions()
                c.node.texture = on
                if !reduceMotion {
                    c.node.run(.repeatForever(.animate(with: [on, off], timePerFrame: 0.5)))
                }
            }
        }
    }

    private func noteText(_ n: RobotNote?) -> String {
        guard let n else { return "" }
        let name = fleet?.agents.first { $0.id == n.agent }?.name ?? n.agent
        var parts: [String] = []
        if let c = n.changed {
            if c.learning > 0 { parts.append("\(c.learning) learning") }
            if c.state > 0 { parts.append("\(c.state) state") }
            if c.raw > 0 { parts.append("\(c.raw) conversation log\(c.raw == 1 ? "" : "s")") }
        }
        if let r = n.removed, r > 0 { parts.append("\(r) removed") }
        let what = n.reason == "shutdown" ? "shutdown nap" : (n.naps ?? 1) > 1 ? "\(n.naps!) naps" : "nap"
        return "\(name)'s \(what) · \(parts.isEmpty ? "no changes" : parts.joined(separator: ", "))"
    }

    // MARK: Selection and input

    private func showSelection() {
        for (id, p) in people {
            let on = selection == .agent(id)
            let mode = fleet?.agents.first { $0.id == id }.map(OfficeLook.mode(of:)) ?? .unknown
            p.setSelected(on, color: Art.nsColor(mode.color))
        }
        rackHighlight.isHidden = selection != .core
    }

    /// What is under a point in the scene.
    func hit(at point: CGPoint) -> OfficeSelection? {
        for node in nodes(at: point) {
            var n: SKNode? = node
            while let cur = n {
                if let name = cur.name {
                    if name.hasPrefix("agent:") { return .agent(String(name.dropFirst(6))) }
                    if name.hasPrefix("tag:") { return .agent(String(name.dropFirst(4))) }
                    if name == "robot" { return .robot }
                }
                n = cur.parent
            }
        }
        for node in nodes(at: point) {
            guard let name = node.name else { continue }
            if name.hasPrefix("desk:") { return .agent(String(name.dropFirst(5))) }
            if name.hasPrefix("vacant:") { return .vacant(String(name.dropFirst(7))) }
            if name == "rack" { return .core }
        }
        return nil
    }

    override func mouseUp(with event: NSEvent) {
        let p = event.location(in: self)
        if event.clickCount == 2, hit(at: p) == nil {
            resetZoom()
            return
        }
        let h = hit(at: p)
        selection = h == selection ? nil : h
        onSelect(selection)
    }

    override func keyDown(with event: NSEvent) {
        if event.keyCode == 53, selection != nil {  // Escape: back to the overview
            selection = nil
            onSelect(nil)
        } else {
            super.keyDown(with: event)
        }
    }

    override func rightMouseDown(with event: NSEvent) {
        guard let view, case .agent(let id)? = hit(at: event.location(in: self)), let menu = menuForAgent(id) else { return }
        NSMenu.popUpContextMenu(menu, with: event, for: view)
    }

    // MARK: Camera

    private func fitScale() -> CGFloat {
        guard let plan, size.width > 0, size.height > 0 else { return 1 }
        return max(plan.size.width / size.width, plan.size.height / size.height)
    }

    /// Fit the floor to the view, then the user's zoom (pinch) and pan (scroll).
    func placeCamera() {
        guard let plan else { return }
        let fit = fitScale()
        let s = min(fit * 1.2, max(1 / 1.6, fit / userZoom))
        cam.setScale(s)
        let visible = CGSize(width: size.width * s, height: size.height * s)
        // Keep the floor in view.
        let maxX = max(0, (plan.size.width - visible.width) / 2 + 40), maxY = max(0, (plan.size.height - visible.height) / 2 + 40)
        panOffset = CGPoint(x: min(maxX, max(-maxX, panOffset.x)), y: min(maxY, max(-maxY, panOffset.y)))
        cam.position = CGPoint(x: plan.size.width / 2 + panOffset.x, y: plan.size.height / 2 + panOffset.y)
    }

    func zoom(by factor: CGFloat) {
        userZoom = min(4, max(1, userZoom * factor))
        if userZoom == 1 { panOffset = .zero }
        placeCamera()
    }

    func pan(dx: CGFloat, dy: CGFloat) {
        let s = cam.xScale
        panOffset.x -= dx * s
        panOffset.y += dy * s
        placeCamera()
    }

    func resetZoom() {
        userZoom = 1
        panOffset = .zero
        placeCamera()
    }
}
