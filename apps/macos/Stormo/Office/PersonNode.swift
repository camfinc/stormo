import AppKit
import SpriteKit
import StormoKit

/// A person in the office: the same figure as the web office's (a 40×60 drawing shown at 1.15×),
/// seen from the front, back or side, with arms and legs as separate nodes that turn about the
/// shoulder and hip, so poses are SKActions.
final class PersonNode: SKNode {
    let agentID: String
    private let figure = SKNode()
    private var views: [String: FacingView] = [:]
    private var pose: Pose?
    private var facingKey = ""
    private var reduceMotion = false
    private let ring = SKShapeNode(ellipseOf: CGSize(width: 34, height: 10))

    /// Viewbox (x right, y down) → figure space (y up, origin at the person's anchor).
    static func local(_ x: CGFloat, _ y: CGFloat) -> CGPoint { CGPoint(x: x - 20, y: 33.04 - y) }

    init(agentID: String, sprite: ResolvedSprite) {
        self.agentID = agentID
        super.init()
        name = "agent:\(agentID)"
        ring.strokeColor = .clear
        ring.lineWidth = 2
        ring.position = CGPoint(x: 0, y: -27)
        ring.zPosition = -1
        addChild(ring)
        figure.setScale(1.15)
        addChild(figure)
        let art = FigureArt.textures(sprite)
        for key in ["front", "back", "side"] {
            let v = FacingView(key: key, art: art)
            v.root.isHidden = true
            figure.addChild(v.root)
            views[key] = v
        }
    }

    @available(*, unavailable) required init?(coder: NSCoder) { fatalError() }

    /// The ring under the feet while selected.
    func setSelected(_ on: Bool, color: NSColor) {
        ring.strokeColor = on ? color : .clear
        ring.fillColor = on ? color.withAlphaComponent(0.18) : .clear
    }

    func apply(_ frame: PersonFrame, reduceMotion: Bool) {
        isHidden = frame.away
        let key: String
        switch frame.facing {
        case .down: key = "front"
        case .up: key = "back"
        case .left, .right: key = "side"
        }
        figure.xScale = frame.facing == .left ? -1.15 : 1.15
        let changed = key != facingKey || frame.pose != pose || reduceMotion != self.reduceMotion
        guard changed else { return }
        facingKey = key
        pose = frame.pose
        self.reduceMotion = reduceMotion
        for (k, v) in views { v.root.isHidden = k != key }
        views[key]?.pose(frame.pose, figure: figure, still: reduceMotion)
    }
}

/// One facing's parts and its poses.
private final class FacingView {
    let root = SKNode()
    let legL: SKSpriteNode, legR: SKSpriteNode
    let seatedLegs: SKSpriteNode?
    let body: SKSpriteNode
    let armL: SKSpriteNode?, armR: SKSpriteNode?, armS: SKSpriteNode?
    let cup: SKSpriteNode?
    let head: SKSpriteNode
    let shadow: SKSpriteNode
    let side: Bool

    init(key: String, art: FigureArt.Set) {
        side = key == "side"
        func full(_ t: SKTexture, z: CGFloat) -> SKSpriteNode {
            let n = SKSpriteNode(texture: t, size: CGSize(width: 40, height: 60))
            n.position = PersonNode.local(20, 30)
            n.zPosition = z
            return n
        }
        func pivoted(_ t: SKTexture, _ px: CGFloat, _ py: CGFloat, z: CGFloat) -> SKSpriteNode {
            let n = SKSpriteNode(texture: t, size: CGSize(width: 40, height: 60))
            n.anchorPoint = CGPoint(x: px / 40, y: 1 - py / 60)
            n.position = PersonNode.local(px, py)
            n.zPosition = z
            return n
        }
        shadow = full(art.shadow, z: 0)
        if side {
            legL = pivoted(art.sideLegL, 18, 41, z: 1)
            legR = pivoted(art.sideLegR, 20, 41, z: 1)
            seatedLegs = nil
            body = full(art.sideBody, z: 2)
            armS = pivoted(art.sideArm, 20, 26.5, z: 3)
            armL = nil
            armR = nil
            cup = nil
            head = full(art.sideHead, z: 4)
        } else {
            legL = pivoted(art.legL, 17, 41, z: 1)
            legR = pivoted(art.legR, 23, 41, z: 1)
            seatedLegs = full(art.seatedLegs, z: 1)
            body = full(key == "front" ? art.frontBody : art.backBody, z: 2)
            armL = pivoted(art.armL, 10.1, 26.5, z: 3)
            armR = pivoted(art.armR, 29.9, 26.5, z: 3)
            armS = nil
            cup = key == "front" ? full(art.cup, z: 3.5) : nil
            head = full(key == "front" ? art.frontHead : art.backHead, z: 4)
        }
        for n in [shadow, legL, legR, body, head] { root.addChild(n) }
        for n in [seatedLegs, armL, armR, armS, cup].compactMap({ $0 }) { root.addChild(n) }
    }

    private static func deg(_ d: CGFloat) -> CGFloat { -d * .pi / 180 }  // CSS clockwise → SpriteKit

    private func reset() {
        for n in [legL, legR, body, head, shadow, seatedLegs, armL, armR, armS, cup].compactMap({ $0 }) {
            n.removeAllActions()
            n.zRotation = 0
            n.yScale = 1
        }
        legL.position.y = PersonNode.local(0, 41).y
        legR.position.y = PersonNode.local(0, 41).y
        root.removeAllActions()
        root.position = .zero
        root.zRotation = 0
    }

    private func swing(_ node: SKNode?, from a: CGFloat, to b: CGFloat, period: TimeInterval, reversed: Bool = false, still: Bool) {
        guard let node else { return }
        let (x, y) = reversed ? (b, a) : (a, b)
        node.zRotation = Self.deg(x)
        guard !still else { return }
        let go = SKAction.rotate(toAngle: Self.deg(y), duration: period, shortestUnitArc: false)
        let back = SKAction.rotate(toAngle: Self.deg(x), duration: period, shortestUnitArc: false)
        go.timingMode = .easeInEaseOut
        back.timingMode = .easeInEaseOut
        node.run(.repeatForever(.sequence([go, back])))
    }

    func pose(_ pose: Pose, figure: SKNode, still: Bool) {
        reset()
        let seated = pose.seated || pose == .sofa
        legL.isHidden = seated
        legR.isHidden = seated
        shadow.isHidden = seated
        seatedLegs?.isHidden = pose != .sofa
        cup?.isHidden = pose != .coffee

        switch pose {
        case .sitIdle:
            armL?.zRotation = Self.deg(152)
            armR?.zRotation = Self.deg(-152)
        case .sitType:
            swing(armL, from: 165, to: 172, period: 0.26, still: still)
            swing(armR, from: -165, to: -172, period: 0.26, reversed: true, still: still)
        case .sitRelax:
            root.zRotation = Self.deg(-7)
            root.position.y = -3
            armL?.zRotation = Self.deg(-148)
            armR?.zRotation = Self.deg(148)
        case .walk:
            if !still {
                root.run(.repeatForever(.sequence([.moveBy(x: 0, y: 1.5, duration: 0.26), .moveBy(x: 0, y: -1.5, duration: 0.26)])))
            }
            if side {
                swing(legL, from: -24, to: 24, period: 0.52, still: still)
                swing(legR, from: -24, to: 24, period: 0.52, reversed: true, still: still)
                swing(armS, from: -32, to: 32, period: 0.52, reversed: true, still: still)
            } else {
                if !still {
                    let lift = SKAction.group([.moveBy(x: 0, y: 2.5, duration: 0), .scaleY(to: 0.88, duration: 0)])
                    let drop = SKAction.group([.moveBy(x: 0, y: -2.5, duration: 0), .scaleY(to: 1, duration: 0)])
                    let step = SKAction.sequence([lift, .wait(forDuration: 0.26), drop, .wait(forDuration: 0.26)])
                    legL.run(.repeatForever(step))
                    legR.run(.sequence([.wait(forDuration: 0.26), .repeatForever(step)]))
                }
                swing(armL, from: -12, to: 12, period: 0.52, still: still)
                swing(armR, from: -12, to: 12, period: 0.52, reversed: true, still: still)
            }
        case .coffee:
            armR?.zRotation = 0
            if !still, let armR {
                let up = SKAction.rotate(toAngle: Self.deg(-150), duration: 0.56)
                let down = SKAction.rotate(toAngle: 0, duration: 0.7)
                armR.run(.repeatForever(.sequence([.wait(forDuration: 4.9), up, .wait(forDuration: 0.84), down])))
            }
        case .stretch:
            swing(armL, from: 150, to: 188, period: 2.4, still: still)
            swing(armR, from: -150, to: -188, period: 2.4, still: still)
        case .stand:
            armL?.zRotation = Self.deg(4)
            armR?.zRotation = Self.deg(-4)
        case .sofa:
            armL?.zRotation = Self.deg(-22)
            armR?.zRotation = Self.deg(22)
        }
    }
}

/// The figure's parts as textures, per set of colours (the web office's SVG, part by part).
enum FigureArt {
    struct Set {
        var shadow, legL, legR, seatedLegs, frontBody, backBody, armL, armR, cup, frontHead, backHead: SKTexture
        var sideLegL, sideLegR, sideBody, sideArm, sideHead: SKTexture
    }

    nonisolated(unsafe) private static var cache: [String: Set] = [:]

    static func textures(_ s: ResolvedSprite) -> Set {
        let key = [s.skin, s.hair, s.hairStyle, s.shirt, s.pants, s.accessory].joined(separator: "|")
        if let hit = cache[key] { return hit }
        let made = make(s)
        cache[key] = made
        return made
    }

    private static func part(_ draw: @escaping (CGContext) -> Void) -> SKTexture {
        // 40×60 viewbox, drawn at the figure's 1.15× on top of the usual texture scale.
        let size = CGSize(width: 40 * 1.15, height: 60 * 1.15)
        return Art.texture(size) { ctx in
            ctx.scaleBy(x: 1.15, y: 1.15)
            draw(ctx)
        }
    }

    // Paints.
    private struct Paint {
        var skin, hair, shirt, pants: CGColor
        let shoe = Art.color("#15171b"), eye = Art.color("#161616"), accFill = Art.color("#16181d"), gold = Art.color("#fbbf24")
    }

    private static func rect(_ ctx: CGContext, _ x: CGFloat, _ y: CGFloat, _ w: CGFloat, _ h: CGFloat, _ rx: CGFloat, _ c: CGColor) {
        Art.fill(ctx, CGRect(x: x, y: y, width: w, height: h), c, radius: rx)
    }

    private static func dot(_ ctx: CGContext, _ x: CGFloat, _ y: CGFloat, _ r: CGFloat, _ c: CGColor) {
        Art.circle(ctx, CGPoint(x: x, y: y), r, c)
    }

    private static func path(_ ctx: CGContext, _ d: String, fill: CGColor? = nil, stroke: CGColor? = nil, width: CGFloat = 1) {
        let p = SVGPath.parse(d)
        if let fill {
            ctx.setFillColor(fill)
            ctx.addPath(p)
            ctx.fillPath()
        }
        if let stroke {
            ctx.setStrokeColor(stroke)
            ctx.setLineWidth(width)
            ctx.setLineCap(.round)
            ctx.addPath(p)
            ctx.strokePath()
        }
    }

    // The web office's hair and accessory shapes (pkg/core/ui/app.js), by view.
    private static let hairFront: [String: (CGContext, CGColor) -> Void] = [
        "short": { c, h in path(c, "M11 13.5Q10.5 4 20 4Q29.5 4 29 13.5Q26.5 8.5 20 8.8Q13.5 8.5 11 13.5Z", fill: h) },
        "fade": { c, h in path(c, "M11.6 12Q12 5 20 5Q28 5 28.4 12Q25.5 8.6 20 9Q14.5 8.6 11.6 12Z", fill: h) },
        "long": { c, h in path(c, "M10.6 14Q10 4 20 4Q30 4 29.4 14L30 25Q27.4 26 27 22.5L27 12Q20 9 13 12L13 22.5Q12.6 26 10 25Z", fill: h) },
        "updo": { c, h in
            dot(c, 20, 3.6, 3.6, h)
            path(c, "M11 13.5Q10.5 4.5 20 4.5Q29.5 4.5 29 13.5Q26.5 8.5 20 8.8Q13.5 8.5 11 13.5Z", fill: h)
        },
        "curly": { c, h in for (x, y, r) in [(13.0, 9.0, 3.6), (17, 6, 3.8), (23, 6, 3.8), (27, 9, 3.6), (11.5, 13, 2.6), (28.5, 13, 2.6)] { dot(c, x, y, r, h) } },
    ]
    private static let hairBack: [String: (CGContext, CGColor, CGColor) -> Void] = [
        "short": { c, h, skin in
            dot(c, 20, 13, 9, h)
            c.setFillColor(skin)
            c.fillEllipse(in: CGRect(x: 16, y: 19.5, width: 8, height: 3.6))
        },
        "fade": { c, h, _ in dot(c, 20, 12.4, 8.6, h) },
        "long": { c, h, _ in
            dot(c, 20, 13, 9.4, h)
            path(c, "M11 14L10.5 29Q20 32 29.5 29L29 14Z", fill: h)
        },
        "updo": { c, h, _ in dot(c, 20, 13, 9, h); dot(c, 20, 6, 4, h) },
        "curly": { c, h, _ in for (x, y, r) in [(20.0, 13.0, 9.0), (12, 10, 3.4), (28, 10, 3.4), (14, 18, 3), (26, 18, 3), (20, 5, 3.6)] { dot(c, x, y, r, h) } },
    ]
    private static let hairSide: [String: (CGContext, CGColor) -> Void] = [
        "short": { c, h in path(c, "M11 15Q10 4 20 4.5Q27 5 28.6 10.5Q22 8.5 19 10.5Q17.5 14 18.4 18.4Q13 19 11 15Z", fill: h) },
        "fade": { c, h in path(c, "M11.6 13Q11.5 5 20 5.2Q26.5 5.5 28 10Q22 8.8 19.4 10.4Q18 13.5 18.6 16.5Q13.6 17 11.6 13Z", fill: h) },
        "long": { c, h in path(c, "M11 15Q10 4 20 4.5Q27 5 28.6 10.5Q22 8.5 19.5 10.5Q18.5 16 19 27Q12 27 10.6 24Z", fill: h) },
        "updo": { c, h in
            dot(c, 12, 8, 3.6, h)
            path(c, "M11 15Q10 4 20 4.5Q27 5 28.6 10.5Q22 8.5 19 10.5Q17.5 14 18.4 18.4Q13 19 11 15Z", fill: h)
        },
        "curly": { c, h in for (x, y, r) in [(13.0, 10.0, 4.0), (18, 6, 4), (24, 6.5, 3.4), (12.5, 16, 3.2)] { dot(c, x, y, r, h) } },
    ]

    private static func accessory(_ c: CGContext, _ kind: String, view: String, _ p: Paint) {
        let stroke = Art.color("#111111")
        switch (kind, view) {
        case ("headset", "front"):
            path(c, "M10.8 13.5Q10.8 3.4 20 3.4Q29.2 3.4 29.2 13.5", stroke: stroke, width: 1.6)
            rect(c, 8.8, 11.6, 3.2, 5.4, 1.2, p.accFill)
            path(c, "M10.6 16.6Q12 20 16.4 19.6", stroke: stroke, width: 1.6)
        case ("headset", "back"):
            path(c, "M11 12.4Q20 5 29 12.4", stroke: stroke, width: 1.6)
            rect(c, 8.8, 11.6, 3.2, 5.4, 1.2, p.accFill)
        case ("headset", "side"):
            path(c, "M13 8Q20 2.6 25 6", stroke: stroke, width: 1.6)
            dot(c, 18.6, 14.6, 2.4, p.accFill)
            path(c, "M18.6 16.6Q20 20 25.4 19.4", stroke: stroke, width: 1.6)
        case ("glasses", "front"):
            c.setStrokeColor(stroke)
            c.setLineWidth(1.6)
            c.strokeEllipse(in: CGRect(x: 14.5, y: 12.7, width: 4.6, height: 4.6))
            c.strokeEllipse(in: CGRect(x: 20.9, y: 12.7, width: 4.6, height: 4.6))
            path(c, "M19.1 15H20.9", stroke: stroke, width: 1.6)
        case ("glasses", "side"):
            c.setStrokeColor(stroke)
            c.setLineWidth(1.6)
            c.strokeEllipse(in: CGRect(x: 22.8, y: 12.3, width: 4.4, height: 4.4))
            path(c, "M22.8 14.2H19", stroke: stroke, width: 1.6)
        case ("cap", "front"):
            path(c, "M10.8 12Q11 3.6 20 3.6Q29 3.6 29.2 12Z", fill: p.accFill)
            rect(c, 14, 10.6, 12, 2.6, 1.2, p.accFill)
        case ("cap", "back"):
            dot(c, 20, 12, 8.8, p.accFill)
        case ("cap", "side"):
            path(c, "M11 12Q11 4 20 4Q28 4.4 28.6 10Z", fill: p.accFill)
            rect(c, 24, 9.4, 8, 2.4, 1.2, p.accFill)
        case ("earrings", "front"), ("earrings", "back"):
            dot(c, 11, 17.5, 1.1, p.gold)
            dot(c, 29, 17.5, 1.1, p.gold)
        case ("earrings", "side"):
            dot(c, 18.6, 18, 1.1, p.gold)
        default:
            break
        }
    }

    private static func make(_ s: ResolvedSprite) -> Set {
        let p = Paint(skin: Art.color(s.skin), hair: Art.color(s.hair), shirt: Art.color(s.shirt), pants: Art.color(s.pants))
        let hairStyle = s.hairStyle
        func leg(_ x: CGFloat) -> SKTexture {
            part { c in
                rect(c, x, 40, 5, 13, 2, p.pants)
                rect(c, x - 0.5, 51, 6, 4, 2, p.shoe)
            }
        }
        func arm(_ x: CGFloat, hand: CGFloat) -> SKTexture {
            part { c in
                rect(c, x, 25, 5, 14, 2.5, p.shirt)
                dot(c, hand, 39.4, 2.4, p.skin)
            }
        }
        func torso(_ c: CGContext, front: Bool) {
            rect(c, 11, 24, 18, 19, 6, p.shirt)
            rect(c, 17.5, 21, 5, 4, 0, p.skin)
            if front { path(c, "M17 24.4L20 28L23 24.4", stroke: Art.color("#ffffff", 0.4), width: 1) }
        }
        return Set(
            shadow: part { c in
                c.setFillColor(Art.color("#000000", 0.5))
                c.fillEllipse(in: CGRect(x: 7.8, y: 53.9, width: 24.3, height: 6.1))
            },
            legL: leg(14.4), legR: leg(20.6),
            seatedLegs: part { c in
                rect(c, 14.4, 40, 5, 7, 2, p.pants)
                rect(c, 20.6, 40, 5, 7, 2, p.pants)
                rect(c, 13.9, 45, 6, 4, 2, p.shoe)
                rect(c, 20.1, 45, 6, 4, 2, p.shoe)
            },
            frontBody: part { c in torso(c, front: true) },
            backBody: part { c in torso(c, front: false) },
            armL: arm(7.6, hand: 10.1), armR: arm(27.4, hand: 29.9),
            cup: part { c in
                rect(c, 27.6, 36, 5, 5.6, 1, Art.color("#f8fafc"))
                rect(c, 28.2, 36.6, 3.8, 1.6, 0, Art.color("#6b4226"))
            },
            frontHead: part { c in
                dot(c, 20, 14, 9, p.skin)
                hairFront[hairStyle]?(c, p.hair)
                dot(c, 16.8, 15, 1.15, p.eye)
                dot(c, 23.2, 15, 1.15, p.eye)
                path(c, "M18 18.6Q20 19.9 22 18.6", stroke: Art.color("#6b3a2a"), width: 0.9)
                accessory(c, s.accessory, view: "front", p)
            },
            backHead: part { c in
                dot(c, 20, 14, 9, p.skin)
                hairBack[hairStyle]?(c, p.hair, p.skin)
                accessory(c, s.accessory, view: "back", p)
            },
            sideLegL: part { c in
                rect(c, 15.5, 40, 5.5, 13, 2, p.pants)
                rect(c, 15.5, 51, 8, 4, 2, p.shoe)
            },
            sideLegR: part { c in
                rect(c, 17.5, 40, 5.5, 13, 2, p.pants)
                rect(c, 17.5, 51, 8, 4, 2, p.shoe)
            },
            sideBody: part { c in
                rect(c, 13, 24, 14, 19, 6, p.shirt)
                rect(c, 17.5, 21, 5, 4, 0, p.skin)
            },
            sideArm: part { c in
                rect(c, 17.5, 25, 5, 14, 2.5, p.shirt)
                dot(c, 20, 39.4, 2.4, p.skin)
            },
            sideHead: part { c in
                dot(c, 20, 14, 9, p.skin)
                dot(c, 28.6, 15.6, 1.4, p.skin)
                hairSide[hairStyle]?(c, p.hair)
                dot(c, 25, 14.6, 1.15, p.eye)
                accessory(c, s.accessory, view: "side", p)
            })
    }
}
