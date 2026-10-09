import AppKit
import SpriteKit
import StormoKit

/// Everything that does not move, drawn once per floor plan: walls, floors, furniture, desks.
/// What changes (screens, chairs, lights, people) are nodes on top (OfficeScene).
enum BuildingArt {
    static let wallColor = "#2a3746", hall = "#161c23", ink = "#e8eef5", ink2 = "#a9b6c4", ink3 = "#6b7b8c", line = "#243241"

    struct Input {
        var plan: FloorPlan
        var look: OfficeLook
        var agents: [String: FleetAgent]
        var groupName: String
    }

    static func texture(_ input: Input) -> SKTexture {
        Art.texture(input.plan.size) { ctx in draw(ctx, input) }
    }

    static func draw(_ ctx: CGContext, _ input: Input) {
        let plan = input.plan
        Art.fill(ctx, CGRect(origin: .zero, size: plan.size), Art.color(wallColor), radius: 6)
        Art.stroke(ctx, CGRect(origin: .zero, size: plan.size), Art.color("#3a4b5e"), width: 1, radius: 6)
        for (_, rect) in plan.halls { hallway(ctx, rect) }
        for office in plan.offices { room(ctx, office, input.look) }
        serverRoom(ctx, plan, groupName: input.look.shortName(input.groupName))
        lobby(ctx, plan)
        for ws in plan.workstations {
            workstation(ctx, ws, agent: ws.agent.flatMap { input.agents[$0] }, look: input.look)
        }
    }

    // MARK: Hallways

    static func hallway(_ ctx: CGContext, _ r: CGRect) {
        Art.fill(ctx, r, Art.color(hall))
        let runner = r.insetBy(dx: 11, dy: 14)
        ctx.saveGState()
        ctx.clip(to: runner)
        var y = runner.minY
        var odd = false
        while y < runner.maxY {
            Art.fill(ctx, CGRect(x: runner.minX, y: y, width: runner.width, height: 10), Art.color(odd ? "#1b232d" : "#1f2833"))
            y += 10
            odd.toggle()
        }
        ctx.restoreGState()
        Art.stroke(ctx, runner, Art.color("#2a3644"), width: 2, radius: 3)
        let exit = CGRect(x: r.midX - 12, y: r.minY + 6, width: 24, height: 9)
        ctx.saveGState()
        ctx.setShadow(offset: .zero, blur: 10, color: Art.color("#22c55e", 0.55))
        Art.fill(ctx, exit, Art.color("#16a34a"), radius: 2)
        ctx.restoreGState()
    }

    // MARK: Offices

    static func floorMaterial(_ ctx: CGContext, _ r: CGRect, _ kind: String) {
        ctx.saveGState()
        ctx.clip(to: r)
        switch kind {
        case "wood":
            Art.fill(ctx, r, Art.color("#2c241e"))
            var x = r.minX + 37
            while x < r.maxX {
                Art.fill(ctx, CGRect(x: x, y: r.minY, width: 1, height: r.height), Art.color("#221c17"))
                x += 38
            }
            // Board ends, staggered, so it reads as planks.
            var col = 0
            x = r.minX
            while x < r.maxX {
                var y = r.minY + CGFloat((col * 61) % 140)
                while y < r.maxY {
                    Art.fill(ctx, CGRect(x: x, y: y, width: 37, height: 1), Art.color("#241d18"))
                    y += 140
                }
                x += 38
                col += 1
            }
        case "carpet":
            let tile: CGFloat = 28
            var y = r.minY, row = 0
            while y < r.maxY {
                var x = r.minX, col = 0
                while x < r.maxX {
                    Art.fill(ctx, CGRect(x: x, y: y, width: tile, height: tile), Art.color((row + col) % 2 == 0 ? "#18202b" : "#1b2431"))
                    x += tile
                    col += 1
                }
                y += tile
                row += 1
            }
        case "concrete":
            let g = CGGradient(colorsSpace: CGColorSpace(name: CGColorSpace.sRGB), colors: [Art.color("#2a2d31"), Art.color("#1d2024")] as CFArray, locations: [0, 1])!
            ctx.drawRadialGradient(g, startCenter: CGPoint(x: r.minX + r.width * 0.3, y: r.minY + r.height * 0.2), startRadius: 0,
                                   endCenter: CGPoint(x: r.minX + r.width * 0.3, y: r.minY + r.height * 0.2), endRadius: max(r.width, r.height), options: [.drawsAfterEndLocation])
            var y = r.minY + 4
            while y < r.maxY {
                var x = r.minX + 4
                while x < r.maxX {
                    Art.circle(ctx, CGPoint(x: x, y: y), 0.8, Art.color("#ffffff", 0.04))
                    x += 9
                }
                y += 9
            }
        case "library":
            Art.fill(ctx, r, Art.color("#14211c"))
            ctx.setStrokeColor(Art.color("#172620"))
            ctx.setLineWidth(6 / sqrt(2) * 1.4)
            var d = -r.height
            while d < r.width + r.height {
                ctx.move(to: CGPoint(x: r.minX + d, y: r.maxY))
                ctx.addLine(to: CGPoint(x: r.minX + d + r.height, y: r.minY))
                d += 12 * sqrt(2)
            }
            ctx.strokePath()
        default:
            Art.fill(ctx, r, Art.color("#171d24"))
        }
        // Warm ceiling-light pools.
        for (fx, fy, a) in [(0.28, 0.38, 0.07), (0.72, 0.38, 0.07), (0.28, 0.78, 0.055), (0.72, 0.78, 0.055)] {
            Art.glow(ctx, center: CGPoint(x: r.minX + r.width * fx, y: r.minY + r.height * fy), radius: 0.26 * max(r.width, r.height), Art.color("#fff3dc", a))
        }
        ctx.restoreGState()
    }

    static func room(_ ctx: CGContext, _ o: FloorPlan.Office, _ look: OfficeLook) {
        floorMaterial(ctx, o.rect, o.floor)
        for w in o.windows { window(ctx, w) }
        door(ctx, o.door, opensInto: o.side == .left ? .left : .right)
        sign(ctx, at: CGPoint(x: o.rect.minX + 16, y: o.rect.minY + 12), name: look.shortName(o.unit.name), desc: o.unit.description ?? "",
             hue: o.hue, maxWidth: o.rect.width - 170)
        wallArt(ctx, o.wall, topRight: CGPoint(x: o.rect.maxX - 44, y: o.rect.minY + 8))
        plant(ctx, CGRect(x: o.rect.maxX - 38, y: o.rect.minY + 8, width: 30, height: 30))
    }

    static func window(_ ctx: CGContext, _ r: CGRect) {
        let vertical = r.height > r.width
        ctx.saveGState()
        ctx.setShadow(offset: .zero, blur: 18, color: Art.color("#7dd3fc", 0.2))
        var p: CGFloat = 0
        let length = vertical ? r.height : r.width
        while p < length {
            let seg = min(52, length - p)
            let pane = vertical ? CGRect(x: r.minX, y: r.minY + p, width: r.width, height: seg) : CGRect(x: r.minX + p, y: r.minY, width: seg, height: r.height)
            Art.fill(ctx, pane, Art.color("#a5e3ff", 0.4))
            p += 56
        }
        ctx.restoreGState()
    }

    enum DoorSwing { case left, right, up }

    /// An opening in the wall with the leaf swung into the room and its arc on the floor.
    static func door(_ ctx: CGContext, _ r: CGRect, opensInto: DoorSwing) {
        Art.fill(ctx, r, Art.color(hall))
        let leaf = Art.color("#8193a7"), arc = Art.color("#8193a7", 0.33)
        ctx.saveGState()
        ctx.setLineDash(phase: 0, lengths: [3, 3])
        ctx.setStrokeColor(arc)
        ctx.setLineWidth(1)
        switch opensInto {
        case .left:  // hinge on the room's right wall, leaf into the room
            let hinge = CGPoint(x: r.minX, y: r.minY + 2)
            ctx.addArc(center: hinge, radius: 54, startAngle: .pi, endAngle: .pi / 2, clockwise: true)
            ctx.strokePath()
            ctx.restoreGState()
            Art.line(ctx, hinge, CGPoint(x: hinge.x - 54, y: hinge.y), leaf, width: 3)
        case .right:
            let hinge = CGPoint(x: r.maxX, y: r.minY + 2)
            ctx.addArc(center: hinge, radius: 54, startAngle: 0, endAngle: .pi / 2, clockwise: false)
            ctx.strokePath()
            ctx.restoreGState()
            Art.line(ctx, hinge, CGPoint(x: hinge.x + 54, y: hinge.y), leaf, width: 3)
        case .up:  // a door in a bottom wall, opening up into the room above
            let hinge = CGPoint(x: r.minX + 2, y: r.minY)
            ctx.addArc(center: hinge, radius: 54, startAngle: -.pi / 2, endAngle: 0, clockwise: false)
            ctx.strokePath()
            ctx.restoreGState()
            Art.line(ctx, hinge, CGPoint(x: hinge.x, y: hinge.y - 54), leaf, width: 3)
        }
    }

    static func sign(_ ctx: CGContext, at p: CGPoint, name: String, desc: String, hue: String, maxWidth: CGFloat) {
        let label = Art.attributed(name.uppercased(), Art.font(11.5, weight: .semibold, mono: true), Art.nsColor(hue), kern: 1.8)
        let size = label.size()
        let pill = CGRect(x: p.x, y: p.y, width: size.width + 16, height: size.height + 6)
        Art.fill(ctx, pill, Art.mix("#0b1117", hue, 0.14), radius: 4)
        Art.stroke(ctx, pill, Art.color(hue, 0.3), width: 1, radius: 4)
        Art.text(label, at: CGPoint(x: p.x + 8, y: p.y + 3))
        if !desc.isEmpty {
            let d = Art.attributed(desc, Art.font(12, weight: .regular), Art.nsColor(ink3))
            Art.text(d, at: CGPoint(x: pill.maxX + 10, y: p.y + 4), maxWidth: max(0, maxWidth - pill.width - 10))
        }
    }

    static func wallArt(_ ctx: CGContext, _ kind: String, topRight: CGPoint) {
        switch kind {
        case "map":
            let r = CGRect(x: topRight.x - 92, y: topRight.y, width: 92, height: 30)
            Art.stroke(ctx, r.insetBy(dx: -2, dy: -2), Art.color("#6b4f36"), width: 2)
            Art.fill(ctx, r, Art.color("#1e3a5f"))
            for (cx, cy, rx, ry) in [(0.24, 0.38, 0.14, 0.3), (0.33, 0.72, 0.09, 0.26), (0.58, 0.34, 0.18, 0.24), (0.6, 0.7, 0.08, 0.22), (0.8, 0.7, 0.12, 0.18)] {
                ctx.setFillColor(Art.color("#3f7a5c"))
                ctx.fillEllipse(in: CGRect(x: r.minX + r.width * (cx - rx / 2), y: r.minY + r.height * (cy - ry / 2), width: r.width * rx, height: r.height * ry))
            }
            Art.circle(ctx, CGPoint(x: r.minX + r.width * 0.22, y: r.minY + r.height * 0.4), 1.6, Art.color("#f87171"))
            Art.circle(ctx, CGPoint(x: r.minX + r.width * 0.63, y: r.minY + r.height * 0.62), 1.6, Art.color("#f87171"))
        case "board":
            let r = CGRect(x: topRight.x - 92, y: topRight.y, width: 92, height: 10)
            Art.stroke(ctx, r.insetBy(dx: -2, dy: -2), Art.color("#46586b"), width: 2)
            Art.fill(ctx, r, Art.color("#dbe3ea"))
            Art.line(ctx, CGPoint(x: r.minX + 10, y: r.minY + 4), CGPoint(x: r.minX + 50, y: r.minY + 4), Art.color("#2563eb"), width: 2, dash: [6, 3])
        case "moodboard":
            let r = CGRect(x: topRight.x - 84, y: topRight.y, width: 84, height: 26)
            Art.stroke(ctx, r.insetBy(dx: -2, dy: -2), Art.color("#6b4f36"), width: 2)
            Art.fill(ctx, r, Art.color("#b08a5e"))
            for (x, y, w, h, c) in [(6.0, 5.0, 14.0, 10.0, "#f472b6"), (26, 9, 12, 12, "#fbbf24"), (44, 4, 16, 10, "#60a5fa"), (64, 10, 12, 12, "#e8eef5")] {
                Art.fill(ctx, CGRect(x: r.minX + x, y: r.minY + y, width: w, height: h), Art.color(c))
            }
        default:  // shelves
            let r = CGRect(x: topRight.x - 120, y: topRight.y, width: 120, height: 12)
            Art.stroke(ctx, r.insetBy(dx: -2, dy: -2), Art.color("#4a3524"), width: 2)
            books(ctx, r, horizontal: true)
        }
    }

    /// Book spines in a strip.
    static func books(_ ctx: CGContext, _ r: CGRect, horizontal: Bool) {
        let colors = [("#b45309", 4.0), ("#1d4ed8", 4.0), ("#15803d", 3.0), ("#9f1239", 4.0), ("#a16207", 3.0), ("#3f2a1c", 2.0)]
        var p: CGFloat = 0
        var i = 0
        let length = horizontal ? r.width : r.height
        while p < length {
            let (c, w) = colors[i % colors.count]
            let seg = min(CGFloat(w), length - p)
            Art.fill(ctx, horizontal ? CGRect(x: r.minX + p, y: r.minY, width: seg, height: r.height) : CGRect(x: r.minX, y: r.minY + p, width: r.width, height: seg), Art.color(c))
            p += CGFloat(w)
            i += 1
        }
    }

    static func plant(_ ctx: CGContext, _ r: CGRect) {
        let c = CGPoint(x: r.midX, y: r.midY), s = r.width / 30
        Art.circle(ctx, c, 15 * s, Art.color("#2a2018"))
        Art.circle(ctx, c, 12 * s, Art.color("#3f2f22"))
        Art.circle(ctx, CGPoint(x: r.minX + r.width * 0.45, y: r.minY + r.height * 0.7), 6 * s, Art.color("#16a34a"))
        Art.circle(ctx, CGPoint(x: r.minX + r.width * 0.68, y: r.minY + r.height * 0.4), 6 * s, Art.color("#22c55e"))
        Art.circle(ctx, CGPoint(x: r.minX + r.width * 0.35, y: r.minY + r.height * 0.35), 5 * s, Art.color("#4ade80"))
    }

    // MARK: Server room

    static func serverRoom(_ ctx: CGContext, _ plan: FloorPlan, groupName: String) {
        let r = plan.server
        Art.fill(ctx, r, Art.color("#1a2530"))
        ctx.saveGState()
        ctx.clip(to: r)
        // Raised floor: 30-point tiles with dotted vents.
        var x = r.minX
        while x < r.maxX {
            Art.fill(ctx, CGRect(x: x, y: r.minY, width: 1, height: r.height), Art.color("#0b1117", 0.5))
            x += 30
        }
        var y = r.minY
        while y < r.maxY {
            Art.fill(ctx, CGRect(x: r.minX, y: y, width: r.width, height: 1), Art.color("#0b1117", 0.5))
            y += 30
        }
        y = r.minY + 5
        while y < r.maxY {
            x = r.minX + 5
            while x < r.maxX {
                Art.circle(ctx, CGPoint(x: x, y: y), 0.7, Art.color("#ffffff", 0.05))
                x += 10
            }
            y += 10
        }
        Art.glow(ctx, center: CGPoint(x: r.midX, y: r.midY), radius: r.width * 0.6, Art.color("#67e8f9", 0.08))
        ctx.restoreGState()
        // Glass walls to the hallways.
        for gx in [r.minX - 6, r.maxX] {
            ctx.saveGState()
            ctx.setShadow(offset: .zero, blur: 14, color: Art.color("#67e8f9", 0.27))
            var p: CGFloat = 0
            while p < r.height {
                Art.fill(ctx, CGRect(x: gx, y: r.minY + p, width: 6, height: min(60, r.height - p)), Art.color("#8be9ff", 0.4))
                p += 63
            }
            ctx.restoreGState()
        }
        door(ctx, plan.serverDoor, opensInto: .up)
        Art.fill(ctx, CGRect(x: plan.serverDoor.minX + 66, y: plan.serverDoor.minY - 12, width: 6, height: 9), Art.color("#0b1117"), radius: 1)
        Art.fill(ctx, CGRect(x: plan.serverDoor.minX + 66, y: plan.serverDoor.minY - 6, width: 6, height: 3), Art.color("#22c55e"))
        // Cable tray.
        let tray = CGRect(x: r.minX + r.width * 0.12, y: r.minY + 46, width: r.width * 0.76, height: 7)
        Art.line(ctx, CGPoint(x: tray.minX, y: tray.midY), CGPoint(x: tray.maxX, y: tray.midY), Art.color("#facc15", 0.19), width: 7, dash: [6, 4])
        Art.stroke(ctx, tray, Art.color("#facc15", 0.25), width: 1, radius: 2)
        sign(ctx, at: CGPoint(x: r.minX + 16, y: r.minY + 12), name: "Server room", desc: "\(groupName) · authorised staff only", hue: "#67e8f9", maxWidth: r.width - 32)
        for rack in plan.racks { rackFrame(ctx, rack) }
        // Cooling unit (its fan turns: a node) and an extinguisher.
        let crac = OfficeGeometry.crac(plan)
        Art.shadowed(ctx, blur: 14, offset: CGSize(width: 0, height: 6)) {
            Art.vgradient(ctx, crac, Art.color("#cbd5e1"), Art.color("#94a3b8"), radius: 4)
        }
        Art.fill(ctx, CGRect(x: r.minX + 12, y: r.maxY - 30, width: 9, height: 16), Art.color("#dc2626"), radius: 3)
    }

    static func rackFrame(_ ctx: CGContext, _ rack: FloorPlan.Rack) {
        let r = rack.rect
        Art.shadowed(ctx, blur: 24, offset: CGSize(width: 0, height: 10), alpha: 0.8) {
            Art.vgradient(ctx, r, Art.color("#1f2b38"), Art.color("#121a23"), radius: 6)
        }
        Art.stroke(ctx, r.insetBy(dx: -2, dy: -2), Art.color("#3b4b5c"), width: 2, radius: 7)
        for u in OfficeGeometry.rackUnits(rack) {
            Art.fill(ctx, u, Art.color("#0b1117"), radius: 2)
            Art.fill(ctx, CGRect(x: u.minX + (rack.kind == .core ? 38 : 22), y: u.midY - 1, width: u.width - (rack.kind == .core ? 44 : 26), height: 2), Art.color("#233140"), radius: 1)
        }
        let label: String
        switch rack.kind {
        case .net: label = "NET"
        case .core: label = "SWARM CORE"
        case .fleet: label = "AGENTS"
        case .ups: label = "UPS"
        }
        let t = Art.attributed(label, Art.font(rack.kind == .core ? 10 : 8.5, weight: .bold, mono: true), Art.nsColor(rack.kind == .core ? ink2 : ink3), kern: 1.4)
        let s = t.size()
        Art.text(t, at: CGPoint(x: r.midX - s.width / 2, y: OfficeGeometry.rackLabelY(rack)))
    }

    // MARK: Lobby

    static func lobby(_ ctx: CGContext, _ plan: FloorPlan) {
        let r = plan.lobby
        Art.fill(ctx, r, Art.color("#21262c"))
        ctx.saveGState()
        ctx.clip(to: r)
        var y = r.minY
        var row = 0
        while y < r.maxY {
            var x = r.minX + (row % 2 == 0 ? 0 : 3.5)
            while x < r.maxX {
                Art.circle(ctx, CGPoint(x: x, y: y), 0.9, Art.color("#ffffff", 0.035))
                x += 7
            }
            y += 11
            row += 1
        }
        Art.glow(ctx, center: CGPoint(x: r.midX, y: r.midY), radius: r.width * 0.55, Art.color("#fff3dc", 0.05))
        ctx.restoreGState()
        sign(ctx, at: CGPoint(x: r.minX + 16, y: r.minY + 12), name: "Lobby · kitchen", desc: "", hue: "#34d399", maxWidth: r.width)
        for (side, d) in plan.lobbyDoors { door(ctx, d, opensInto: side == .left ? .right : .left) }
        for c in plan.clocks {
            Art.shadowed(ctx, blur: 10, offset: CGSize(width: 0, height: 4)) {
                Art.vgradient(ctx, c, Art.color("#0a0d12"), Art.color("#05070a"), radius: 4)
            }
            Art.stroke(ctx, c.insetBy(dx: -2, dy: -2), Art.color("#3a4b5e"), width: 2, radius: 5)
        }
        // Kitchen.
        Art.shadowed(ctx) {
            Art.vgradient(ctx, plan.fridge, Art.color("#e2e8f0"), Art.color("#cbd5e1"), radius: 3)
            Art.vgradient(ctx, plan.counter, Art.color("#5b6573"), Art.color("#48515d"), radius: 3)
        }
        Art.fill(ctx, CGRect(x: plan.fridge.maxX - 3, y: plan.fridge.minY, width: 3, height: plan.fridge.height), Art.color("#94a3b8"))
        Art.fill(ctx, plan.sink, Art.color("#cbd5e1"), radius: 4)
        Art.fill(ctx, plan.sink.insetBy(dx: 2, dy: 2), Art.color("#94a3b8"), radius: 3)
        Art.fill(ctx, plan.espresso, Art.color("#111827"), radius: 3)
        Art.circle(ctx, CGPoint(x: plan.fruit.midX, y: plan.fruit.midY), 7.5, Art.color("#e5e7eb"))
        Art.circle(ctx, CGPoint(x: plan.fruit.minX + 5, y: plan.fruit.minY + 6), 3, Art.color("#ef4444"))
        Art.circle(ctx, CGPoint(x: plan.fruit.minX + 10, y: plan.fruit.minY + 8), 3, Art.color("#facc15"))
        Art.circle(ctx, CGPoint(x: plan.fruit.minX + 7, y: plan.fruit.minY + 10.5), 3, Art.color("#84cc16"))
        let cc = CGPoint(x: plan.cooler.midX, y: plan.cooler.midY)
        Art.circle(ctx, cc, 9, Art.color("#e2e8f0"))
        Art.circle(ctx, cc, 8, Art.color("#38bdf8"))
        Art.circle(ctx, cc, 5, Art.color("#bae6fd"))
        // Seating.
        for t in plan.cafeTables {
            for (dx, dy) in [(-12.0, 9.0), (30.0, 9.0), (9.0, 31.0)] {
                let chair = CGRect(x: t.minX + dx, y: t.minY + dy, width: 12, height: 12)
                ctx.setFillColor(Art.color("#1b242e"))
                ctx.fillEllipse(in: chair)
                ctx.setStrokeColor(Art.color("#26313d"))
                ctx.setLineWidth(2)
                ctx.strokeEllipse(in: chair.insetBy(dx: 1, dy: 1))
            }
            Art.shadowed(ctx, blur: 12, offset: CGSize(width: 0, height: 6)) {
                ctx.setFillColor(Art.color("#3a4a5b"))
                ctx.fillEllipse(in: t)
            }
        }
        Art.shadowed(ctx, blur: 14, offset: CGSize(width: 0, height: 6)) {
            Art.fill(ctx, plan.sofa, Art.color("#2b3a4a"), radius: 9)
        }
        Art.fill(ctx, CGRect(x: plan.sofa.minX, y: plan.sofa.maxY - 9, width: plan.sofa.width, height: 9), Art.color("#223040"), radius: 6)
        Art.shadowed(ctx, blur: 10) { Art.fill(ctx, plan.coffeeTable, Art.color("#4a3b2e"), radius: 5) }
        Art.shadowed(ctx, blur: 10) { Art.vgradient(ctx, plan.reception, Art.color("#5b6b7c"), Art.color("#3a4a5b"), radius: 6) }
        // The main entrance: double doors in the outer wall.
        Art.fill(ctx, plan.entrance, Art.color("#21262c"))
        for (hinge, start, end, cw) in [(CGPoint(x: plan.entrance.minX + 2, y: plan.entrance.minY), CGFloat.pi / 2, CGFloat.pi * 1.5, true),
                                        (CGPoint(x: plan.entrance.maxX - 2, y: plan.entrance.minY), CGFloat.pi / 2, -CGFloat.pi / 2, false)] {
            ctx.saveGState()
            ctx.setLineDash(phase: 0, lengths: [3, 3])
            ctx.setStrokeColor(Art.color("#8193a7", 0.33))
            ctx.setLineWidth(1)
            ctx.addArc(center: hinge, radius: 40, startAngle: start + .pi, endAngle: end + .pi, clockwise: cw)
            ctx.strokePath()
            ctx.restoreGState()
            Art.line(ctx, hinge, CGPoint(x: hinge.x, y: hinge.y - 40), Art.color("#8193a7"), width: 3)
        }
        let mat = Art.attributed("WELCOME", Art.font(7, weight: .bold, mono: true), Art.nsColor(ink3), kern: 1.2)
        let ms = mat.size()
        Art.text(mat, at: CGPoint(x: plan.entrance.midX - ms.width / 2, y: plan.entrance.minY - ms.height - 4))
        plant(ctx, CGRect(x: r.maxX - 40, y: r.maxY - 48, width: 30, height: 30))
        plant(ctx, CGRect(x: r.minX + 10, y: r.maxY - 48, width: 30, height: 30))
    }

    // MARK: Workstations

    static func workstation(_ ctx: CGContext, _ ws: FloorPlan.Workstation, agent: FleetAgent?, look: OfficeLook) {
        let o = ws.rect.origin
        let deskRect = CGRect(x: o.x + 4, y: o.y, width: 148, height: 62)
        let deskTop = deskTopColor(ws, look: look)
        ctx.saveGState()
        if agent == nil { ctx.setAlpha(0.6) }
        Art.shadowed(ctx, blur: 18, offset: CGSize(width: 0, height: 6), alpha: 0.5) {
            Art.vgradient(ctx, deskRect, Art.mix(deskTop, "#ffffff", 0.08), Art.color(deskTop), radius: 5)
        }
        Art.fill(ctx, CGRect(x: deskRect.minX, y: deskRect.maxY - 3, width: deskRect.width, height: 3), Art.color("#000000", 0.25))
        let desk = agent.map { look.desk(for: $0) }
        for m in OfficeGeometry.monitors(ws, count: desk?.apps.count ?? 1) {
            ctx.saveGState()
            ctx.translateBy(x: m.rect.midX, y: m.rect.midY)
            ctx.rotate(by: m.angle)
            let local = CGRect(center: .zero, size: m.rect.size)
            Art.fill(ctx, CGRect(x: -7, y: local.maxY + 2, width: 14, height: 4), Art.color("#3b4b5c"), radius: 2)
            Art.fill(ctx, local, Art.color("#0b1117"), radius: 4)
            Art.stroke(ctx, local.insetBy(dx: -0.75, dy: -0.75), Art.color("#3b4b5c"), width: 1.5, radius: 4)
            ctx.restoreGState()
        }
        let kbd = CGRect(x: o.x + 78 - 17, y: deskRect.maxY - 15, width: 34, height: 8)
        Art.fill(ctx, kbd, Art.color("#1c2630"), radius: 2)
        Art.stroke(ctx, kbd, Art.color("#3b4b5c"), width: 1, radius: 2)
        let mouse = CGRect(x: o.x + 100, y: deskRect.maxY - 15, width: 5, height: 7)
        Art.fill(ctx, mouse, Art.color("#1c2630"), radius: 2.5)
        if let desk {
            for (i, p) in desk.props.enumerated() { prop(ctx, p, slot: i, desk: deskRect, hue: look.hue(unit: ws.unit)) }
            side(ctx, desk.side, ws: ws.rect, hue: look.hue(unit: ws.unit))
        }
        if let agent {
            let t = Art.attributed(agent.name.uppercased(), Art.font(9.5, weight: .semibold, mono: true), Art.nsColor(ink3), kern: 1.1)
            let s = t.size()
            let plaque = CGRect(x: o.x + 78 - s.width / 2 - 6, y: deskRect.maxY + 10 - s.height - 2, width: s.width + 12, height: s.height + 2)
            Art.fill(ctx, plaque, Art.color("#0d141c"), radius: 3)
            Art.stroke(ctx, plaque, Art.color(line), width: 1, radius: 3)
            Art.text(t, at: CGPoint(x: plaque.minX + 6, y: plaque.minY + 1))
        }
        ctx.restoreGState()
    }

    static func deskTopColor(_ ws: FloorPlan.Workstation, look: OfficeLook) -> String {
        guard ws.office != nil else { return "#3a4756" }
        switch look.floor(unit: ws.unit) {
        case "wood": return "#4c3b2c"
        case "carpet": return "#364352"
        case "concrete": return "#4a4540"
        case "library": return "#41382b"
        default: return "#3a4756"
        }
    }

    /// The six prop slots around the monitors and keyboard (top-left of the prop).
    static func slot(_ i: Int, size: CGSize, desk: CGRect) -> CGPoint {
        switch i {
        case 0: CGPoint(x: desk.minX + 7, y: desk.minY + 6)
        case 1: CGPoint(x: desk.maxX - 7 - size.width, y: desk.minY + 6)
        case 2: CGPoint(x: desk.minX + 9, y: desk.maxY - 7 - size.height)
        case 3: CGPoint(x: desk.maxX - 9 - size.width, y: desk.maxY - 7 - size.height)
        case 4: CGPoint(x: desk.minX + 30, y: desk.maxY - 6 - size.height)
        default: CGPoint(x: desk.maxX - 30 - size.width, y: desk.minY + 40)
        }
    }

    static func prop(_ ctx: CGContext, _ name: String, slot i: Int, desk: CGRect, hue: String) {
        let sizes: [String: CGSize] = [
            "folders": .init(width: 14, height: 20), "ticket": .init(width: 22, height: 10), "id-card": .init(width: 11, height: 15),
            "headset": .init(width: 18, height: 12), "phone": .init(width: 17, height: 13), "globe": .init(width: 15, height: 15),
            "stamp": .init(width: 9, height: 9), "laptop": .init(width: 26, height: 17), "duck": .init(width: 11, height: 11),
            "notes": .init(width: 11, height: 11), "mug": .init(width: 10, height: 10), "tablet": .init(width: 24, height: 16),
            "camera": .init(width: 17, height: 11), "swatches": .init(width: 14, height: 18), "books": .init(width: 18, height: 14),
            "chart": .init(width: 15, height: 19), "magnifier": .init(width: 13, height: 13), "notebook": .init(width: 14, height: 18),
            "lamp": .init(width: 13, height: 13), "plant": .init(width: 12, height: 12), "photo": .init(width: 12, height: 10),
        ]
        let size = sizes[name] ?? CGSize(width: 10, height: 10)
        let r = CGRect(origin: slot(i, size: size, desk: desk), size: size)
        let c = CGPoint(x: r.midX, y: r.midY)
        func rotated(_ deg: CGFloat, _ draw: () -> Void) {
            ctx.saveGState()
            ctx.translateBy(x: c.x, y: c.y)
            ctx.rotate(by: deg * .pi / 180)
            ctx.translateBy(x: -c.x, y: -c.y)
            draw()
            ctx.restoreGState()
        }
        switch name {
        case "folders":
            for (k, col) in ["#60a5fa", "#fbbf24", "#60a5fa", "#fbbf24"].enumerated() {
                Art.fill(ctx, CGRect(x: r.minX, y: r.minY + CGFloat(k) * 5, width: r.width, height: 4), Art.color(col), radius: 1)
            }
        case "ticket":
            rotated(-12) {
                Art.fill(ctx, r, Art.color("#f1f5f9"), radius: 1)
                Art.fill(ctx, CGRect(x: r.minX, y: r.minY, width: 4, height: r.height), Art.color("#2563eb"))
                Art.line(ctx, CGPoint(x: r.minX + r.width * 0.69, y: r.minY), CGPoint(x: r.minX + r.width * 0.69, y: r.maxY), Art.color("#94a3b8"), width: 0.6, dash: [1, 1])
            }
        case "id-card":
            rotated(8) {
                Art.fill(ctx, r, Art.color("#1e3a8a"), radius: 2)
                Art.circle(ctx, CGPoint(x: r.midX, y: r.minY + r.height * 0.42), 2.2, Art.color("#fbbf24"))
            }
        case "headset":
            ctx.setStrokeColor(Art.color("#0f172a"))
            ctx.setLineWidth(3)
            ctx.addArc(center: CGPoint(x: r.midX, y: r.maxY), radius: r.width / 2 - 1, startAngle: .pi, endAngle: 0, clockwise: false)
            ctx.strokePath()
            Art.fill(ctx, CGRect(x: r.minX - 4, y: r.maxY - 4, width: 6, height: 7), Art.color("#1e293b"), radius: 3)
            Art.fill(ctx, CGRect(x: r.maxX - 2, y: r.maxY - 4, width: 6, height: 7), Art.color("#1e293b"), radius: 3)
        case "phone":
            Art.fill(ctx, CGRect(x: r.minX + 1, y: r.minY - 3, width: r.width - 2, height: 4), Art.color("#0b1117"), radius: 2)
            Art.fill(ctx, r, Art.color("#1f2937"), radius: 3)
            for k in 0..<3 { for j in 0..<2 { Art.circle(ctx, CGPoint(x: r.minX + 5 + CGFloat(k) * 4, y: r.minY + 6 + CGFloat(j) * 3), 0.8, Art.color("#64748b")) } }
        case "globe":
            Art.circle(ctx, c, 9.5, Art.color("#a16207"))
            Art.circle(ctx, c, 7.5, Art.color("#2563eb"))
            Art.circle(ctx, CGPoint(x: r.minX + r.width * 0.35, y: r.minY + r.height * 0.4), 3, Art.color("#4ade80"))
            Art.circle(ctx, CGPoint(x: r.minX + r.width * 0.66, y: r.minY + r.height * 0.66), 2.5, Art.color("#4ade80"))
        case "stamp":
            Art.circle(ctx, c, 6.5, Art.color("#7f1d1d"))
            Art.circle(ctx, c, 4.5, Art.color("#dc2626"))
        case "laptop":
            Art.fill(ctx, r, Art.color("#cbd5e1"), radius: 2)
            Art.fill(ctx, CGRect(x: r.minX, y: r.minY, width: r.width, height: r.height * 0.45), Art.color("#94a3b8"), radius: 2)
            Art.fill(ctx, CGRect(x: r.minX + 3, y: r.minY + 2, width: r.width - 6, height: 5), Art.color("#1e293b"))
        case "duck":
            Art.circle(ctx, c, 5.5, Art.color("#facc15"))
            Art.fill(ctx, CGRect(x: r.maxX - 1, y: r.minY + 3, width: 4, height: 3), Art.color("#f97316"), radius: 1.5)
        case "notes":
            rotated(6) { Art.shadowed(ctx, blur: 0, offset: CGSize(width: 1, height: 1), alpha: 0.3) { Art.fill(ctx, r, Art.color("#fde047")) } }
        case "mug":
            ctx.setStrokeColor(Art.color("#e8eef5"))
            ctx.setLineWidth(1.5)
            ctx.addArc(center: CGPoint(x: r.maxX, y: c.y), radius: 2.5, startAngle: -.pi / 2, endAngle: .pi / 2, clockwise: false)
            ctx.strokePath()
            Art.circle(ctx, c, 5, Art.color("#e8eef5"))
            Art.circle(ctx, c, 3, Art.color("#6b4226"))
        case "tablet":
            Art.fill(ctx, r, Art.color("#374151"), radius: 3)
            Art.fill(ctx, r.insetBy(dx: 1, dy: 1), Art.color("#111827"), radius: 2.5)
            Art.line(ctx, CGPoint(x: r.maxX + 3, y: r.minY + 2), CGPoint(x: r.maxX + 7, y: r.maxY + 2), Art.color("#e5e7eb"), width: 2)
        case "camera":
            Art.fill(ctx, r, Art.color("#1f2937"), radius: 2)
            Art.circle(ctx, CGPoint(x: r.minX + r.width * 0.6, y: c.y), 4.5, Art.color("#475569"))
            Art.circle(ctx, CGPoint(x: r.minX + r.width * 0.6, y: c.y), 3, Art.color("#0b1117"))
        case "swatches":
            for (k, col) in ["#fde68a", "#fb7185", "#a78bfa"].enumerated() {
                let fan = CGRect(x: r.minX + CGFloat(2 - k) * 4, y: r.minY, width: 6, height: 18)
                ctx.saveGState()
                ctx.translateBy(x: fan.midX, y: fan.maxY)
                ctx.rotate(by: (-20 + CGFloat(2 - k) * 10) * .pi / 180)
                Art.fill(ctx, CGRect(x: -3, y: -18, width: 6, height: 18), Art.color(col), radius: 2)
                ctx.restoreGState()
            }
        case "books":
            for (k, col) in ["#b45309", "#1d4ed8", "#15803d"].enumerated() {
                Art.fill(ctx, CGRect(x: r.minX, y: r.minY + CGFloat(k) * 4.7, width: r.width, height: 4.6), Art.color(col), radius: 1)
            }
        case "chart":
            rotated(-6) {
                Art.fill(ctx, r, Art.color("#f1f5f9"))
                for (k, h) in [4.0, 7.0, 10.0].enumerated() {
                    Art.fill(ctx, CGRect(x: r.minX + 3 + CGFloat(k) * 3.5, y: r.maxY - 3 - h, width: 2.4, height: h), Art.color("#f59e0b"))
                }
            }
        case "magnifier":
            Art.line(ctx, CGPoint(x: r.maxX - 3, y: r.maxY - 3), CGPoint(x: r.maxX + 2, y: r.maxY + 2), Art.color("#78350f"), width: 2)
            Art.circle(ctx, CGPoint(x: r.minX + 5.5, y: r.minY + 5.5), 5.5, Art.color("#cbd5e1"))
            Art.circle(ctx, CGPoint(x: r.minX + 5.5, y: r.minY + 5.5), 3.8, Art.color("#93c5fd", 0.35))
        case "notebook":
            Art.fill(ctx, r, Art.color("#7c2d12"), radius: 2)
            Art.fill(ctx, CGRect(x: r.minX + 10, y: r.minY, width: 1, height: r.height), Art.color("#111111"))
        case "lamp":
            Art.glow(ctx, center: c, radius: 18, Art.color("#fbbf24", 0.25))
            Art.circle(ctx, c, 6.5, Art.color("#f59e0b"))
            Art.circle(ctx, c, 3, Art.color("#fef3c7"))
        case "plant":
            Art.circle(ctx, c, 6, Art.color("#7c4a2d"))
            Art.circle(ctx, c, 5, Art.color("#15803d"))
            Art.circle(ctx, c, 3, Art.color("#4ade80"))
        case "photo":
            rotated(-5) {
                Art.fill(ctx, r.insetBy(dx: -2, dy: -2), Art.color("#e8eef5"))
                Art.fill(ctx, r, Art.color("#22c55e"))
                Art.fill(ctx, CGRect(x: r.minX, y: r.minY, width: r.width, height: r.height * 0.55), Art.color("#60a5fa"))
            }
        default:
            break
        }
    }

    /// The one floor item beside the desk.
    static func side(_ ctx: CGContext, _ name: String, ws: CGRect, hue: String) {
        let right = ws.maxX + 2
        switch name {
        case "suitcase":
            let r = CGRect(x: right - 18, y: ws.minY + 72, width: 18, height: 26)
            ctx.setStrokeColor(Art.color("#94a3b8"))
            ctx.setLineWidth(2)
            ctx.addPath(CGPath(roundedRect: CGRect(x: r.minX + 5, y: r.minY - 4, width: 8, height: 6), cornerWidth: 3, cornerHeight: 3, transform: nil))
            ctx.strokePath()
            Art.shadowed(ctx) { Art.fill(ctx, r, Art.mix("#1e293b", hue, 0.55), radius: 4) }
            for x in [r.minX + 5, r.minX + 12] { Art.fill(ctx, CGRect(x: x, y: r.minY, width: 1, height: r.height), Art.color("#000000", 0.2)) }
        case "plant":
            plant(ctx, CGRect(x: right - 28, y: ws.minY + 72, width: 28, height: 28))
        case "bin":
            let c = CGPoint(x: right - 7, y: ws.minY + 79)
            Art.circle(ctx, c, 7, Art.color("#334155"))
            Art.circle(ctx, c, 4.5, Art.color("#0b1117"))
        case "ring-light":
            let c = CGPoint(x: right - 13, y: ws.minY + 85)
            ctx.saveGState()
            ctx.setShadow(offset: .zero, blur: 14, color: Art.color("#ffffff", 0.67))
            ctx.setStrokeColor(Art.color("#f8fafc"))
            ctx.setLineWidth(3)
            ctx.strokeEllipse(in: CGRect(center: c, size: CGSize(width: 23, height: 23)))
            ctx.restoreGState()
        case "shelf":
            let r = CGRect(x: right - 14, y: ws.minY + 66, width: 14, height: 44)
            Art.stroke(ctx, r.insetBy(dx: -2, dy: -2), Art.color("#4a3524"), width: 2, radius: 3)
            books(ctx, r, horizontal: false)
        case "whiteboard":
            let r = CGRect(x: right - 6, y: ws.minY + 64, width: 6, height: 46)
            Art.stroke(ctx, r.insetBy(dx: -2, dy: -2), Art.color("#475569"), width: 2)
            Art.fill(ctx, r, Art.color("#e2e8f0"))
        default:
            break
        }
    }
}

/// Positions shared by the building texture and the nodes drawn over it.
enum OfficeGeometry {
    struct Monitor {
        var rect: CGRect
        var angle: CGFloat
    }

    /// The monitors on a desk: one 58×34, or two 44×30 angled toward the chair.
    static func monitors(_ ws: FloorPlan.Workstation, count: Int) -> [Monitor] {
        let cx = ws.rect.minX + 78, top = ws.rect.minY + 5
        if count < 2 {
            return [Monitor(rect: CGRect(x: cx - 29, y: top, width: 58, height: 34), angle: 0)]
        }
        return [Monitor(rect: CGRect(x: cx - 45.5, y: top, width: 44, height: 30), angle: -7 * .pi / 180),
                Monitor(rect: CGRect(x: cx + 1.5, y: top, width: 44, height: 30), angle: 7 * .pi / 180)]
    }

    /// The chair at rest (its centre is the seat).
    static func chair(_ ws: FloorPlan.Workstation) -> CGRect {
        CGRect(x: ws.rect.minX + 58, y: ws.rect.minY + 70, width: 40, height: 36)
    }

    static func crac(_ plan: FloorPlan) -> CGRect {
        CGRect(x: plan.server.maxX - 40, y: plan.server.minY + plan.server.height * 0.46, width: 30, height: 56)
    }

    /// The rack-mounted units in a rack (the fleet rack holds blades instead).
    static func rackUnits(_ rack: FloorPlan.Rack) -> [CGRect] {
        let r = rack.rect
        switch rack.kind {
        case .net: return (0..<7).map { CGRect(x: r.minX + 6, y: r.minY + 7 + CGFloat($0) * 16, width: r.width - 12, height: 12) }
        case .ups: return (0..<5).map { CGRect(x: r.minX + 6, y: r.minY + 7 + CGFloat($0) * 16, width: r.width - 12, height: 12) }
        case .core: return (0..<6).map { CGRect(x: r.minX + 10, y: r.minY + 10 + CGFloat($0) * 19, width: r.width - 20, height: 14) }
        case .fleet: return []
        }
    }

    /// The LEDs on a unit: 4 small lights at its left.
    static func leds(_ unit: CGRect, core: Bool) -> [CGPoint] {
        let step: CGFloat = core ? 8 : 6, start = unit.minX + (core ? 8 : 5.5)
        return (0..<4).map { CGPoint(x: start + CGFloat($0) * step, y: unit.midY) }
    }

    static func rackLabelY(_ rack: FloorPlan.Rack) -> CGFloat {
        switch rack.kind {
        case .core: rack.rect.minY + 10 + 6 * 19 + 22
        case .fleet: rack.rect.maxY - 18
        default: rack.rect.maxY - 18
        }
    }

    /// The in-flight slots under the core rack's units.
    static func slots(_ rack: FloorPlan.Rack, count: Int) -> [CGRect] {
        let n = max(1, count), w: CGFloat = 14, gap: CGFloat = 6
        let total = CGFloat(n) * w + CGFloat(n - 1) * gap
        let y = rack.rect.minY + 10 + 6 * 19 + 6
        return (0..<n).map { CGRect(x: rack.rect.midX - total / 2 + CGFloat($0) * (w + gap), y: y, width: w, height: 6) }
    }

    /// One blade per agent container in the fleet rack.
    static func blades(_ rack: FloorPlan.Rack, count: Int) -> [CGRect] {
        (0..<count).map { CGRect(x: rack.rect.minX + 6, y: rack.rect.minY + 8 + CGFloat($0) * 18, width: rack.rect.width - 12, height: 14) }
    }
}
