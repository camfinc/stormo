import AppKit
import SpriteKit

/// Drawing for the office: everything is drawn once in Core Graphics (points, y down, like the
/// floor plan) at a retina-plus scale and handed to SpriteKit as textures.
enum Art {
    /// Pixels per point in textures: sharp on a Retina display at up to 1.5× camera zoom.
    static let scale: CGFloat = 3

    /// A texture of `size` points, drawn with y growing down.
    static func texture(_ size: CGSize, _ draw: (CGContext) -> Void) -> SKTexture {
        let w = max(1, Int(ceil(size.width * scale))), h = max(1, Int(ceil(size.height * scale)))
        let ctx = CGContext(data: nil, width: w, height: h, bitsPerComponent: 8, bytesPerRow: 0,
                            space: CGColorSpace(name: CGColorSpace.sRGB)!, bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue)!
        ctx.translateBy(x: 0, y: CGFloat(h))
        ctx.scaleBy(x: scale, y: -scale)
        ctx.setShouldAntialias(true)
        let previous = NSGraphicsContext.current
        NSGraphicsContext.current = NSGraphicsContext(cgContext: ctx, flipped: true)
        draw(ctx)
        NSGraphicsContext.current = previous
        let t = SKTexture(cgImage: ctx.makeImage()!)
        t.filteringMode = .linear
        return t
    }

    /// A sprite showing `texture`, sized in points, its top-left at `rect`'s origin in floor (y-down) space.
    static func sprite(_ texture: SKTexture, at rect: CGRect, floorHeight: CGFloat) -> SKSpriteNode {
        let node = SKSpriteNode(texture: texture, size: rect.size)
        node.position = CGPoint(x: rect.midX, y: floorHeight - rect.midY)
        return node
    }

    // MARK: Colours

    static func color(_ hex: String, _ alpha: CGFloat = 1) -> CGColor {
        var s = hex
        if s.hasPrefix("#") { s.removeFirst() }
        var a = alpha
        if s.count == 8, let v = UInt32(s.suffix(2), radix: 16) {
            a *= CGFloat(v) / 255
            s = String(s.prefix(6))
        }
        guard s.count == 6, let v = UInt32(s, radix: 16) else { return CGColor(gray: 0.5, alpha: alpha) }
        return CGColor(srgbRed: CGFloat((v >> 16) & 0xff) / 255, green: CGFloat((v >> 8) & 0xff) / 255, blue: CGFloat(v & 0xff) / 255, alpha: a)
    }

    static func nsColor(_ hex: String, _ alpha: CGFloat = 1) -> NSColor { NSColor(cgColor: color(hex, alpha)) ?? .gray }

    /// `a` blended toward `b` by t (0…1): CSS color-mix.
    static func mix(_ a: String, _ b: String, _ t: CGFloat) -> CGColor {
        let ca = color(a).components ?? [0, 0, 0, 1], cb = color(b).components ?? [0, 0, 0, 1]
        return CGColor(srgbRed: ca[0] + (cb[0] - ca[0]) * t, green: ca[1] + (cb[1] - ca[1]) * t, blue: ca[2] + (cb[2] - ca[2]) * t, alpha: 1)
    }

    // MARK: Shapes

    static func fill(_ ctx: CGContext, _ rect: CGRect, _ color: CGColor, radius: CGFloat = 0) {
        ctx.setFillColor(color)
        ctx.addPath(CGPath(roundedRect: rect, cornerWidth: min(radius, rect.width / 2), cornerHeight: min(radius, rect.height / 2), transform: nil))
        ctx.fillPath()
    }

    static func stroke(_ ctx: CGContext, _ rect: CGRect, _ color: CGColor, width: CGFloat, radius: CGFloat = 0) {
        ctx.setStrokeColor(color)
        ctx.setLineWidth(width)
        ctx.addPath(CGPath(roundedRect: rect.insetBy(dx: width / 2, dy: width / 2), cornerWidth: radius, cornerHeight: radius, transform: nil))
        ctx.strokePath()
    }

    static func circle(_ ctx: CGContext, _ c: CGPoint, _ r: CGFloat, _ color: CGColor) {
        ctx.setFillColor(color)
        ctx.fillEllipse(in: CGRect(x: c.x - r, y: c.y - r, width: 2 * r, height: 2 * r))
    }

    static func line(_ ctx: CGContext, _ a: CGPoint, _ b: CGPoint, _ color: CGColor, width: CGFloat, dash: [CGFloat] = []) {
        ctx.saveGState()
        ctx.setStrokeColor(color)
        ctx.setLineWidth(width)
        ctx.setLineCap(.round)
        if !dash.isEmpty { ctx.setLineDash(phase: 0, lengths: dash) }
        ctx.move(to: a)
        ctx.addLine(to: b)
        ctx.strokePath()
        ctx.restoreGState()
    }

    /// A vertical gradient filling `rect` (optionally rounded).
    static func vgradient(_ ctx: CGContext, _ rect: CGRect, _ top: CGColor, _ bottom: CGColor, radius: CGFloat = 0) {
        ctx.saveGState()
        ctx.addPath(CGPath(roundedRect: rect, cornerWidth: min(radius, rect.width / 2), cornerHeight: min(radius, rect.height / 2), transform: nil))
        ctx.clip()
        let g = CGGradient(colorsSpace: CGColorSpace(name: CGColorSpace.sRGB), colors: [top, bottom] as CFArray, locations: [0, 1])!
        ctx.drawLinearGradient(g, start: CGPoint(x: rect.midX, y: rect.minY), end: CGPoint(x: rect.midX, y: rect.maxY), options: [])
        ctx.restoreGState()
    }

    /// A soft round glow (light pools, screen glow).
    static func glow(_ ctx: CGContext, center: CGPoint, radius: CGFloat, _ color: CGColor) {
        let clear = color.copy(alpha: 0)!
        let g = CGGradient(colorsSpace: CGColorSpace(name: CGColorSpace.sRGB), colors: [color, clear] as CFArray, locations: [0, 1])!
        ctx.drawRadialGradient(g, startCenter: center, startRadius: 0, endCenter: center, endRadius: radius, options: [])
    }

    /// A drop shadow under whatever `draw` paints.
    static func shadowed(_ ctx: CGContext, blur: CGFloat = 8, offset: CGSize = CGSize(width: 0, height: 4), alpha: CGFloat = 0.55, _ draw: () -> Void) {
        ctx.saveGState()
        // The context is flipped (y down): a positive height moves the shadow down.
        ctx.setShadow(offset: CGSize(width: offset.width, height: -offset.height), blur: blur, color: CGColor(gray: 0, alpha: alpha))
        draw()
        ctx.restoreGState()
    }

    // MARK: Text

    static func font(_ size: CGFloat, weight: NSFont.Weight = .semibold, mono: Bool = false) -> NSFont {
        mono ? .monospacedSystemFont(ofSize: size, weight: weight) : .systemFont(ofSize: size, weight: weight)
    }

    static func attributed(_ s: String, _ font: NSFont, _ color: NSColor, kern: CGFloat = 0) -> NSAttributedString {
        NSAttributedString(string: s, attributes: [.font: font, .foregroundColor: color, .kern: kern])
    }

    /// Draws text with its top-left at `at`; returns its size.
    @discardableResult
    static func text(_ s: NSAttributedString, at: CGPoint, maxWidth: CGFloat = .greatestFiniteMagnitude) -> CGSize {
        let size = s.size()
        let w = min(size.width, maxWidth)
        s.draw(with: CGRect(x: at.x, y: at.y, width: w, height: size.height), options: [.usesLineFragmentOrigin, .truncatesLastVisibleLine])
        return CGSize(width: w, height: size.height)
    }
}

extension CGRect {
    init(center: CGPoint, size: CGSize) {
        self.init(x: center.x - size.width / 2, y: center.y - size.height / 2, width: size.width, height: size.height)
    }
}
