import AppKit
import SpriteKit
import StormoKit

// The office's live pieces: screens, the robot, name tags, bubbles, markers, rack lights, clocks.

/// A monitor's screen: dark when off shift, a screensaver when idle, its app lit and flickering
/// while working, red-tinged on alert, blinking while booting.
final class ScreenNode: SKNode {
    private let app: String
    private let size: CGSize
    private let hue: String
    private let background = SKSpriteNode()
    private let content = SKSpriteNode()
    private let saver = SKSpriteNode()
    private let glow = SKSpriteNode()
    private var mode: OfficeMode?
    private var still = false

    init(app: String, size: CGSize, hue: String) {
        self.app = app
        self.size = size
        self.hue = hue
        super.init()
        glow.texture = ScreenArt.glow(size: size)
        glow.size = CGSize(width: size.width + 36, height: size.height + 36)
        glow.zPosition = -1
        glow.blendMode = .add
        background.size = size
        content.size = size
        content.texture = ScreenArt.content(app: app, size: size)
        saver.texture = ScreenArt.saver(hue: hue)
        saver.size = CGSize(width: 15, height: 15)
        for n in [glow, background, content, saver] { addChild(n) }
    }

    @available(*, unavailable) required init?(coder: NSCoder) { fatalError() }

    func set(_ mode: OfficeMode, still: Bool) {
        guard mode != self.mode || still != self.still else { return }
        self.mode = mode
        self.still = still
        for n in [background, content, saver, glow] { n.removeAllActions() }
        background.texture = nil
        background.color = Art.nsColor("#070b0f")
        background.colorBlendFactor = 1
        content.alpha = 0
        saver.isHidden = true
        glow.isHidden = true
        switch mode {
        case .away, .unknown:
            break
        case .idle:
            glow.isHidden = false
            glow.color = Art.nsColor(hue)
            glow.colorBlendFactor = 1
            glow.alpha = 0.3
            saver.isHidden = false
            let pts = [(0.08, 0.18), (0.7, 0.6), (0.4, 0.1), (0.8, 0.3), (0.15, 0.62)].map {
                CGPoint(x: (CGFloat($0.0) - 0.5) * size.width + 4, y: (0.5 - CGFloat($0.1)) * size.height - 4)
            }
            saver.position = pts[0]
            if !still {
                // Drifts through the points and back, like the web office's screensaver.
                let there = pts.dropFirst().map { SKAction.move(to: $0, duration: 7.0 / 4) }
                let back = pts.reversed().dropFirst().map { SKAction.move(to: $0, duration: 7.0 / 4) }
                saver.run(.repeatForever(.sequence(there + back)))
            }
        case .busy, .alert:
            background.texture = ScreenArt.lit(hue: hue, size: size)
            background.colorBlendFactor = 0
            if app == "inbox" { background.color = Art.nsColor("#f8fafc"); background.texture = nil; background.colorBlendFactor = 1 }
            glow.isHidden = false
            glow.color = Art.nsColor(mode == .busy ? OfficePalette.busy : OfficePalette.bad)
            glow.colorBlendFactor = 1
            glow.alpha = mode == .busy ? 0.55 : 0.4
            content.alpha = mode == .busy ? 1 : 0.6
            if mode == .busy, !still {
                content.run(.repeatForever(.sequence([.wait(forDuration: 0.65), .fadeAlpha(to: 0.55, duration: 0), .wait(forDuration: 0.65), .fadeAlpha(to: 1, duration: 0)])))
            }
        case .arriving:
            if !still {
                background.run(.repeatForever(.sequence([
                    .colorize(with: Art.nsColor("#070b0f"), colorBlendFactor: 1, duration: 0), .wait(forDuration: 0.6),
                    .colorize(with: NSColor(cgColor: Art.mix("#070b0f", OfficePalette.warn, 0.2))!, colorBlendFactor: 1, duration: 0), .wait(forDuration: 0.6),
                ])))
            }
        }
    }
}

enum ScreenArt {
    nonisolated(unsafe) private static var cache: [String: SKTexture] = [:]

    private static func cached(_ key: String, _ make: () -> SKTexture) -> SKTexture {
        if let t = cache[key] { return t }
        let t = make()
        cache[key] = t
        return t
    }

    static func glow(size: CGSize) -> SKTexture {
        cached("glow-\(size.width)") {
            let s = CGSize(width: size.width + 36, height: size.height + 36)
            return Art.texture(s) { ctx in
                ctx.setShadow(offset: .zero, blur: 14, color: .white)
                Art.fill(ctx, CGRect(x: 18, y: 18, width: size.width, height: size.height), .white, radius: 3)
            }
        }
    }

    static func saver(hue: String) -> SKTexture {
        cached("saver-\(hue)") {
            Art.texture(CGSize(width: 15, height: 15)) { ctx in
                ctx.setShadow(offset: .zero, blur: 6, color: Art.color(hue))
                Art.fill(ctx, CGRect(x: 4, y: 4, width: 7, height: 7), Art.mix(hue, "#ffffff", 0.2), radius: 2)
            }
        }
    }

    static func lit(hue: String, size: CGSize) -> SKTexture {
        cached("lit-\(hue)-\(size.width)") {
            Art.texture(size) { ctx in Art.vgradient(ctx, CGRect(origin: .zero, size: size), Art.mix("#070b0f", hue, 0.18), Art.color("#070b0f"), radius: 2) }
        }
    }

    /// The app's picture on a screen of `size` (the web office's six screen apps).
    static func content(app: String, size: CGSize) -> SKTexture {
        cached("app-\(app)-\(size.width)") {
            Art.texture(size) { ctx in
                let w = size.width, h = size.height
                @MainActor func bar(_ x: CGFloat, _ y: CGFloat, _ bw: CGFloat, _ bh: CGFloat, _ c: String, r: CGFloat = 1) {
                    Art.fill(ctx, CGRect(x: x, y: y, width: bw, height: bh), Art.color(c), radius: r)
                }
                switch app {
                case "code":
                    for (top, left, frac, c) in [(4.0, 4.0, 0.58, "#c7d2fe"), (8, 9, 0.36, "#f9a8d4"), (12, 9, 0.5, "#c7d2fe"), (16, 14, 0.28, "#86efac"), (20, 9, 0.44, "#c7d2fe"), (24, 4, 0.22, "#c7d2fe")] {
                        bar(left, top * h / 28, w * frac, 2, c)
                    }
                case "records":
                    bar(4, 3, w / 2 - 4, 4, "#2dd4bf")
                    for (top, right, c) in [(10.0, 4.0, "#cbd5e1"), (15, 4, "#fde68a"), (20, w * 0.3, "#cbd5e1"), (25, w * 0.45, "#cbd5e1")] {
                        let y = top * h / 28
                        bar(4, y, w - 4 - right, 3, c)
                        Art.circle(ctx, CGPoint(x: 5.5, y: y + 1.5), 1.5, Art.color("#2dd4bf"))
                    }
                    bar(w * 0.76, 20 * h / 28, 8, 8 * h / 28, "#34d399", r: 2)
                case "design":
                    for (i, c) in ["#f472b6", "#a78bfa", "#fbbf24", "#2dd4bf", "#e8eef5", "#fb7185"].enumerated() {
                        let col = CGFloat(i % 3), row = CGFloat(i / 3)
                        bar(w * (0.04 + col * 0.32), h * (0.08 + row * 0.46), w * 0.28, h * 0.38, c, r: 2)
                    }
                case "charts":
                    for (i, (frac, c)) in [(0.3, "#fbbf24"), (0.52, "#fbbf24"), (0.4, "#60a5fa"), (0.7, "#fbbf24"), (0.58, "#fbbf24"), (0.82, "#34d399")].enumerated() {
                        let bh = (h - 3) * frac
                        bar(w * (0.06 + CGFloat(i) * 0.16), h - 3 - bh, w * 0.11, bh, c)
                    }
                case "inbox":
                    Art.fill(ctx, CGRect(x: 0, y: 0, width: w, height: h), Art.color("#f8fafc"))
                    bar(0, 0, w * 0.08, h, "#1e40af", r: 0)
                    bar(w * 0.08, 0, w * 0.3, h, "#ffffff", r: 0)
                    var y: CGFloat = 1
                    while y < h - 2 {
                        Art.circle(ctx, CGPoint(x: w * 0.08 + w * 0.3 * 0.2, y: y + 1.5), 1.3, Art.color("#34d399"))
                        bar(w * 0.08 + w * 0.3 * 0.38, y + 1, w * 0.3 * 0.48, 1, "#cbd5e1", r: 0)
                        y += 5
                    }
                    bar(w * 0.08, h * 0.21, w * 0.3, 5, "#dbeafe", r: 0)
                    bar(w * 0.42, h * 0.12, w * 0.22, 3, "#e2e8f0")
                    bar(w * 0.5, h * 0.28, w * 0.27, 9, "#dbeafe")
                    bar(w * 0.5, h * 0.28 + 12, w * 0.27, 6, "#dbeafe")
                    bar(w * 0.56, h * 0.7, w * 0.21, 3, "#fcd34d")
                    var py: CGFloat = 0
                    while py < h {
                        bar(w * 0.8, py, w * 0.2, 3, "#f8fafc", r: 0)
                        bar(w * 0.8, py + 3, w * 0.2, 1, "#e2e8f0", r: 0)
                        py += 4
                    }
                default:  // chat
                    for (i, top) in [3.0, 8, 13, 18, 23].enumerated() {
                        let y = top * h / 28
                        if i % 2 == 0 { bar(4, y, w * 0.5, 4, "#94a3b8", r: 2) } else { bar(w - 4 - w * 0.4, y, w * 0.4, 4, "#2dd4bf", r: 2) }
                    }
                }
            }
        }
    }
}

/// The core's sync robot (docs/core.md §3): rolls to a desk when that agent's nap lands.
final class RobotNode: SKNode {
    private let body = SKSpriteNode(texture: RobotArt.body, size: CGSize(width: 30, height: 34))
    private let note = SKSpriteNode()
    private var lastNote = ""
    private var state = ""

    override init() {
        super.init()
        name = "robot"
        // The floor point is the robot's base: (15, 24) in its 30×34 drawing.
        body.anchorPoint = CGPoint(x: 0.5, y: 10.0 / 34)
        addChild(body)
        for (x, c, delay) in [(12.4, "#34d399", 0.0), (15.0, "#67e8f9", 0.7), (17.6, "#fbbf24", 1.4)] {
            let led = SKSpriteNode(color: Art.nsColor(c), size: CGSize(width: 1.8, height: 1.8))
            led.position = CGPoint(x: x - 15, y: 24 - 19.5)
            led.run(.sequence([.wait(forDuration: delay), .repeatForever(.sequence([.fadeAlpha(to: 1, duration: 0), .wait(forDuration: 0.9), .fadeAlpha(to: 0.3, duration: 0), .wait(forDuration: 1.3)]))]))
            body.addChild(led)
        }
        note.anchorPoint = CGPoint(x: 0.5, y: 0)
        note.position = CGPoint(x: 0, y: 28)
        note.isHidden = true
        addChild(note)
        isAccessibilityElement = true
        accessibilityRole = NSAccessibility.Role.button.rawValue
    }

    @available(*, unavailable) required init?(coder: NSCoder) { fatalError() }

    func apply(_ f: RobotFrame, note text: String, still: Bool) {
        body.xScale = f.facingLeft ? -1 : 1
        let s = "\(f.rolling)-\(f.writing)-\(still)"
        if s != state {
            state = s
            body.removeAction(forKey: "roll")
            body.position = .zero
            if f.rolling, !still {
                body.run(.repeatForever(.sequence([.moveBy(x: 0, y: 1, duration: 0.22), .moveBy(x: 0, y: -1, duration: 0.22)])), withKey: "roll")
            }
        }
        let show = f.writing && !text.isEmpty
        note.isHidden = !show
        if show, text != lastNote {
            lastNote = text
            let tex = LabelArt.note(text)
            note.texture = tex
            note.size = tex.size().applying(CGAffineTransform(scaleX: 1 / Art.scale, y: 1 / Art.scale))
        }
        accessibilityLabel = f.writing ? "Sync robot: \(text)" : "Sync robot"
    }
}

enum RobotArt {
    static let body: SKTexture = Art.texture(CGSize(width: 30, height: 34)) { c in
        c.setFillColor(Art.color("#000000", 0.5))
        c.fillEllipse(in: CGRect(x: 6, y: 29.6, width: 18, height: 4.8))
        Art.fill(c, CGRect(x: 8, y: 26, width: 14, height: 5), Art.color("#334155"), radius: 2.5)
        for x in [11.0, 19.0] {
            Art.circle(c, CGPoint(x: x, y: 29.6), 2, Art.color("#64748b"))
            Art.circle(c, CGPoint(x: x, y: 29.6), 1.6, Art.color("#0f172a"))
        }
        Art.fill(c, CGRect(x: 4.6, y: 16, width: 2.6, height: 7), Art.color("#cbd5e1"), radius: 1.3)
        Art.fill(c, CGRect(x: 6, y: 14, width: 18, height: 13), Art.color("#e2e8f0"), radius: 4)
        Art.fill(c, CGRect(x: 10, y: 17, width: 10, height: 5), Art.color("#0f172a"), radius: 1)
        Art.fill(c, CGRect(x: 7, y: 3, width: 16, height: 11), Art.color("#f8fafc"), radius: 4)
        Art.fill(c, CGRect(x: 9, y: 5, width: 12, height: 7), Art.color("#0f172a"), radius: 2)
        Art.fill(c, CGRect(x: 11.2, y: 7.4, width: 2.4, height: 2.4), Art.color("#67e8f9"), radius: 1.2)
        Art.fill(c, CGRect(x: 16.4, y: 7.4, width: 2.4, height: 2.4), Art.color("#67e8f9"), radius: 1.2)
        Art.line(c, CGPoint(x: 15, y: 3), CGPoint(x: 15, y: 0.9), Art.color("#94a3b8"), width: 1)
        Art.circle(c, CGPoint(x: 15, y: 0.9), 1.3, Art.color("#34d399"))
        Art.fill(c, CGRect(x: 21.4, y: 15.4, width: 7, height: 9.4), Art.color("#fef3c7"), radius: 1)
        Art.stroke(c, CGRect(x: 21.4, y: 15.4, width: 7, height: 9.4), Art.color("#92400e"), width: 0.8, radius: 1)
        for (y, w) in [(18.4, 4.0), (20.4, 4), (22.4, 2.6)] { Art.line(c, CGPoint(x: 23, y: y), CGPoint(x: 23 + w, y: y), Art.color("#a16207"), width: 0.6) }
        Art.line(c, CGPoint(x: 26.6, y: 23.6), CGPoint(x: 29, y: 20.2), Art.color("#1e3a8a"), width: 1.2)
    }
}

/// Text on the floor, drawn into textures (sharp at any zoom the camera allows).
enum LabelArt {
    nonisolated(unsafe) private static var cache: [String: SKTexture] = [:]

    private static func cached(_ key: String, _ make: () -> SKTexture) -> SKTexture {
        if let t = cache[key] { return t }
        let t = make()
        if cache.count > 400 { cache.removeAll() }
        cache[key] = t
        return t
    }

    /// The name tag under a person: portrait (or initials), name, what it is doing, ringed in its mode's colour.
    static func tag(name: String, status: String, ring: String, hue: String, avatar: NSImage?) -> SKTexture {
        cached("tag|\(name)|\(status)|\(ring)|\(hue)|\(avatar.map { ObjectIdentifier($0).hashValue } ?? 0)") {
            let nameText = Art.attributed(name, Art.font(11, weight: .bold), Art.nsColor(BuildingArt.ink))
            let statusText = Art.attributed(status, Art.font(11, weight: .semibold), NSColor(cgColor: Art.mix(ring, "#ffffff", 0.25))!)
            let ns = nameText.size(), ss = statusText.size()
            let statusW = min(ss.width, 150)
            let size = CGSize(width: 2 + 18 + 6 + ns.width + 6 + statusW + 9 + 4, height: 22 + 4)
            return Art.texture(size) { ctx in
                let pill = CGRect(x: 2, y: 2, width: size.width - 4, height: 22)
                Art.shadowed(ctx, blur: 8, offset: CGSize(width: 0, height: 3), alpha: 0.5) {
                    Art.fill(ctx, pill, Art.color("#0d141c", 0.95), radius: 11)
                }
                Art.stroke(ctx, pill, Art.mix(BuildingArt.line, ring, 0.45), width: 1, radius: 11)
                let mini = CGRect(x: pill.minX + 2, y: pill.minY + 2, width: 18, height: 18)
                ctx.saveGState()
                ctx.addEllipse(in: mini)
                ctx.clip()
                Art.fill(ctx, mini, Art.mix("#0b1117", hue, 0.35))
                if let avatar {
                    avatar.draw(in: mini.insetBy(dx: -3, dy: -3).offsetBy(dx: 0, dy: 2), from: .zero, operation: .sourceOver, fraction: 1, respectFlipped: true, hints: nil)
                } else {
                    let initials = Art.attributed(String(name.split(separator: " ").compactMap(\.first).prefix(2)).uppercased(), Art.font(8, weight: .bold), Art.nsColor(BuildingArt.ink))
                    let isz = initials.size()
                    Art.text(initials, at: CGPoint(x: mini.midX - isz.width / 2, y: mini.midY - isz.height / 2))
                }
                ctx.restoreGState()
                ctx.setStrokeColor(Art.color(ring))
                ctx.setLineWidth(2)
                ctx.strokeEllipse(in: mini.insetBy(dx: -1, dy: -1))
                Art.text(nameText, at: CGPoint(x: mini.maxX + 6, y: pill.midY - ns.height / 2))
                Art.text(statusText, at: CGPoint(x: mini.maxX + 6 + ns.width + 6, y: pill.midY - ss.height / 2), maxWidth: statusW)
            }
        }
    }

    /// The speech bubble over someone working: where the work comes from.
    static func bubble(_ text: String, thinking: Bool) -> SKTexture {
        cached("bubble|\(text)|\(thinking)") {
            let t = Art.attributed("\(text)  ···", Art.font(10, weight: .bold), Art.nsColor("#0d141c"))
            let s = t.size()
            let size = CGSize(width: s.width + 14, height: s.height + 6)
            return Art.texture(size) { ctx in
                let r = CGRect(origin: .zero, size: size)
                ctx.addPath(CGPath(roundedRect: r, cornerWidth: 9, cornerHeight: 9, transform: nil))
                ctx.setFillColor(Art.color(thinking ? "#ddd6fe" : BuildingArt.ink))
                ctx.fillPath()
                Art.fill(ctx, CGRect(x: 0, y: size.height - 6, width: 6, height: 6), Art.color(thinking ? "#ddd6fe" : BuildingArt.ink), radius: 1.5)
                Art.text(t, at: CGPoint(x: 7, y: 3))
            }
        }
    }

    /// The gold count of learnings waiting for review, or the red "!".
    static func marker(_ text: String, alert: Bool) -> SKTexture {
        cached("marker|\(text)|\(alert)") {
            let t = Art.attributed(text, Art.font(11, weight: .heavy, mono: true), alert ? .white : Art.nsColor("#1a1405"))
            let s = t.size()
            let size = CGSize(width: max(20, s.width + 10) + 8, height: 28)
            return Art.texture(size) { ctx in
                let r = CGRect(x: 4, y: 4, width: size.width - 8, height: 20)
                ctx.setShadow(offset: .zero, blur: 10, color: Art.color(alert ? OfficePalette.bad : OfficePalette.gold, 0.5))
                Art.fill(ctx, r.insetBy(dx: -2, dy: -2), Art.color("#0d141c"), radius: 12)
                Art.fill(ctx, r, Art.color(alert ? OfficePalette.bad : OfficePalette.gold), radius: 10)
                ctx.setShadow(offset: .zero, blur: 0, color: nil)
                Art.text(t, at: CGPoint(x: r.midX - s.width / 2, y: r.midY - s.height / 2))
            }
        }
    }

    static func sticky() -> SKTexture {
        cached("sticky") {
            let t = Art.attributed("OFF SHIFT", Art.font(9, weight: .bold, mono: true), Art.nsColor("#3f3005"), kern: 0.5)
            let s = t.size()
            let size = CGSize(width: s.width + 12, height: s.height + 6)
            return Art.texture(size) { ctx in
                Art.shadowed(ctx, blur: 0, offset: CGSize(width: 1, height: 2), alpha: 0.4) { Art.fill(ctx, CGRect(x: 0, y: 0, width: size.width - 2, height: size.height - 2), Art.color("#fde68a")) }
                Art.text(t, at: CGPoint(x: 5, y: 2))
            }
        }
    }

    static func note(_ text: String) -> SKTexture {
        cached("note|\(text)") {
            let t = Art.attributed(text, Art.font(10.5, weight: .semibold), Art.nsColor("#3f3005"))
            let s = t.size()
            let size = CGSize(width: s.width + 16, height: s.height + 8)
            return Art.texture(size) { ctx in
                Art.fill(ctx, CGRect(origin: .zero, size: size), Art.color("#fef3c7"), radius: 6)
                Art.text(t, at: CGPoint(x: 8, y: 4))
            }
        }
    }

    /// A blade in the fleet rack: the agent's id and a light in its mode's colour.
    static func blade(_ id: String, color: String, size: CGSize) -> SKTexture {
        cached("blade|\(id)|\(color)|\(size.width)") {
            Art.texture(size) { ctx in
                Art.fill(ctx, CGRect(origin: .zero, size: size), Art.color("#0b1117"), radius: 2)
                ctx.saveGState()
                ctx.setShadow(offset: .zero, blur: 6, color: Art.color(color))
                Art.circle(ctx, CGPoint(x: 6.5, y: size.height / 2), 2.5, Art.color(color))
                ctx.restoreGState()
                Art.text(Art.attributed(id.uppercased(), Art.font(8, weight: .semibold, mono: true), Art.nsColor(BuildingArt.ink3)),
                         at: CGPoint(x: 13, y: size.height / 2 - 5.5), maxWidth: size.width - 15)
            }
        }
    }

    /// The core rack's status line ("Using ChatGPT plan").
    static func rackSub(_ text: String, color: String, width: CGFloat) -> SKTexture {
        cached("sub|\(text)|\(color)|\(width)") {
            let t = Art.attributed(text, Art.font(10.5, weight: .medium, mono: true), Art.nsColor(color))
            let s = t.size()
            return Art.texture(CGSize(width: width, height: s.height)) { _ in
                Art.text(t, at: CGPoint(x: max(0, (width - s.width) / 2), y: 0), maxWidth: width)
            }
        }
    }

    /// A world clock's face: city and time; `colon` false draws the blinking colon dimmed.
    static func clock(city: String, time: String, night: Bool, colon: Bool, size: CGSize) -> SKTexture {
        cached("clock|\(city)|\(time)|\(night)|\(colon)") {
            Art.texture(size) { ctx in
                let label = Art.attributed(city.uppercased(), Art.font(7.5, weight: .bold, mono: true), Art.nsColor(BuildingArt.ink3), kern: 0.9)
                let ls = label.size()
                let dotC = night ? "#818cf8" : "#fbbf24"
                let lx = (size.width - ls.width - 8) / 2
                ctx.saveGState()
                ctx.setShadow(offset: .zero, blur: 4, color: Art.color(dotC))
                Art.circle(ctx, CGPoint(x: lx + 2, y: 3 + ls.height / 2), 2, Art.color(dotC))
                ctx.restoreGState()
                Art.text(label, at: CGPoint(x: lx + 8, y: 3))
                let parts = time.split(separator: ":")
                let red = Art.nsColor("#f87171")
                let font = Art.font(15, weight: .bold, mono: true)
                let t = NSMutableAttributedString(attributedString: Art.attributed(String(parts.first ?? ""), font, red))
                t.append(Art.attributed(":", font, colon ? red : red.withAlphaComponent(0.15)))
                t.append(Art.attributed(String(parts.last ?? ""), font, red))
                let ts = t.size()
                ctx.setShadow(offset: .zero, blur: 6, color: Art.color("#f87171", 0.6))
                Art.text(t, at: CGPoint(x: (size.width - ts.width) / 2, y: size.height - ts.height - 2))
            }
        }
    }
}
