import CoreGraphics
import Foundation

/// The office's camera: how much floor one point of the view shows. Pure, so the zoom's limits and
/// what the floor draws at each distance are tested without a scene.
public struct OfficeCamera: Sendable, Equatable {
    /// Floor points per view point with the whole floor in view.
    public var fit: CGFloat
    /// The user's zoom over that fit: 1 is the whole floor.
    public private(set) var zoom: CGFloat = 1

    /// The closest the camera gets: textures are drawn for it (Art.scale), closer would blur.
    public static let closest: CGFloat = 1 / 1.6

    public init(fit: CGFloat, zoom: CGFloat = 1) {
        self.fit = max(fit, 0.01)
        self.zoom = 1
        setZoom(zoom)
    }

    /// The scale with the whole floor in view. A view so large that the fit would be closer than
    /// `closest` shows the floor a little smaller instead, with a margin.
    public var fitScale: CGFloat { fit >= Self.closest ? fit : min(fit * 1.2, Self.closest) }

    /// As far in as the zoom goes; 1 when the fit is already as close as the camera gets.
    public var maxZoom: CGFloat { max(1, fitScale / Self.closest) }

    /// Floor points per view point now.
    public var scale: CGFloat { fitScale / zoom }

    /// Zoom never leaves what the camera can show, so no step is ever spent past a limit.
    public mutating func setZoom(_ z: CGFloat) { zoom = min(maxZoom, max(1, z)) }

    public var isZoomed: Bool { zoom > 1.0001 }
}

/// What the floor shows at a distance. Close up, everything; further out, name tags stack and grow
/// so they stay readable; from far away each room is its counts.
public enum OfficeDetail: Sendable, Equatable {
    case near, mid, far

    /// Past `mid` a stacked tag, grown for the distance, would be wider than a desk: neighbours'
    /// tags would cover each other, so the rooms take over.
    public init(cameraScale s: CGFloat) {
        self = s <= 1.1 ? .near : (s <= 1.7 ? .mid : .far)
    }

    /// How much labels grow against the camera: they never show under 0.9 of their drawn size.
    public static func labelScale(cameraScale s: CGFloat) -> CGFloat { min(3, max(1, 0.9 * s)) }
}

/// One room's people by what they are doing: what a room shows from far away.
public struct RoomSummary: Sendable, Equatable {
    public var total = 0
    public var working = 0
    public var idle = 0
    public var attention = 0
    public var offShift = 0
    /// Everything of theirs waiting for review.
    public var toReview = 0

    public init(_ agents: [FleetAgent]) {
        for a in agents {
            total += 1
            toReview += a.toReview
            switch OfficeLook.mode(of: a) {
            case .busy: working += 1
            case .idle, .arriving: idle += 1
            case .alert, .down: attention += 1
            case .away, .unknown: offShift += 1
            }
        }
    }

    /// "2 working, 1 idle, 1 needs attention": for VoiceOver and tooltips.
    public var words: String {
        var parts: [String] = []
        if working > 0 { parts.append("\(working) working") }
        if idle > 0 { parts.append("\(idle) idle") }
        if attention > 0 { parts.append("\(attention) \(attention == 1 ? "needs" : "need") attention") }
        if offShift > 0 { parts.append("\(offShift) off shift") }
        if toReview > 0 { parts.append("\(toReview) to review") }
        return parts.isEmpty ? "nobody here" : parts.joined(separator: ", ")
    }
}
