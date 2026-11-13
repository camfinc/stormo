import CoreGraphics
import Foundation

/// `GET /api/timeline`: when each agent was at work over the last hours. Times are the core's
/// epoch milliseconds; an agent with no events is absent.
public struct FleetTimeline: Codable, Sendable, Equatable {
    /// A stretch of work: hook events no further apart than `gapMs`.
    public struct Span: Codable, Sendable, Equatable {
        public var start: Int64
        public var end: Int64
        public var events: Int

        public init(start: Int64, end: Int64, events: Int) {
            self.start = start; self.end = end; self.events = events
        }
    }

    public var now: Int64
    public var from: Int64
    public var gapMs: Int64
    public var agents: [String: [Span]]

    public init(now: Int64, from: Int64, gapMs: Int64, agents: [String: [Span]]) {
        self.now = now; self.from = from; self.gapMs = gapMs; self.agents = agents
    }

    /// How long an agent worked in the window. A lone event still took a moment: each stretch
    /// counts for at least `floorMs`.
    public func worked(_ agent: String, floorMs: Int64 = 30_000) -> Int64 {
        (agents[agent] ?? []).reduce(0) { $0 + max(floorMs, $1.end - $1.start) }
    }

    /// "1h 12m", "8m", "under a minute"; "" for none.
    public static func duration(_ ms: Int64) -> String {
        guard ms > 0 else { return "" }
        let m = Int((Double(ms) / 60_000).rounded())
        if m < 1 { return "under a minute" }
        if m < 60 { return "\(m)m" }
        return m % 60 == 0 ? "\(m / 60)h" : "\(m / 60)h \(m % 60)m"
    }
}

/// The timeline's time axis over a width: where a moment falls, where the marks go.
public struct TimelineAxis: Sendable, Equatable {
    public var from: Int64
    public var to: Int64
    public var width: CGFloat

    public init(from: Int64, to: Int64, width: CGFloat) {
        self.from = from
        self.to = max(to, from + 1)
        self.width = max(width, 1)
    }

    public func x(_ t: Int64) -> CGFloat {
        CGFloat(Double(min(max(t, from), to) - from) / Double(to - from)) * width
    }

    /// The moment under an x.
    public func time(at x: CGFloat) -> Int64 {
        from + Int64(Double(min(max(x, 0), width) / width) * Double(to - from))
    }

    /// A stretch's bar: never thinner than `minWidth`, so a lone event still shows.
    public func bar(_ s: FleetTimeline.Span, minWidth: CGFloat = 2) -> (x: CGFloat, width: CGFloat) {
        let a = x(s.start), b = x(s.end)
        let w = max(minWidth, b - a)
        return (min(a, width - w), w)
    }

    /// Marks on round clock times in `zone`, as many as fit at `spacing` points apart: every
    /// 5, 10, 15 or 30 minutes, or 1, 2, 3, 6, 12 or 24 hours.
    public func marks(spacing: CGFloat = 90, zone: TimeZone = .current) -> [Int64] {
        let steps: [Int64] = [5, 10, 15, 30, 60, 120, 180, 360, 720, 1440].map { $0 * 60_000 }
        let want = Double(to - from) / Double(max(1, width / spacing))
        let step = steps.first { Double($0) >= want } ?? steps[steps.count - 1]
        // Round in the zone's own clock, so a mark is on the hour there (and on midnight for days).
        let offset = Int64(zone.secondsFromGMT(for: Date(timeIntervalSince1970: Double(from) / 1000))) * 1000
        var t = ((from + offset) / step + 1) * step - offset
        var out: [Int64] = []
        while t < to {
            out.append(t)
            t += step
        }
        return out
    }

    /// The stretch under a moment, with `slop` either side for thin bars.
    public func span(at t: Int64, in spans: [FleetTimeline.Span], slop: Int64) -> FleetTimeline.Span? {
        spans.first { t >= $0.start - slop && t <= $0.end + slop }
    }
}
