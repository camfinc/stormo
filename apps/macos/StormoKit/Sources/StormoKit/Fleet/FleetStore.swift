import Foundation
import Observation

/// The fleet at a glance: what the menu bar and the toolbar count.
public struct FleetSummary: Sendable, Equatable {
    public var total = 0
    /// Containers running.
    public var onShift = 0
    /// Working at their desk right now (the office's view).
    public var working = 0
    /// Unhealthy, or a platform asking for attention.
    public var attention = 0
    /// Agents with learnings or skill proposals waiting for review.
    public var toReview = 0

    public init() {}

    public init(_ fleet: Fleet) {
        total = fleet.agents.count
        for a in fleet.agents {
            if a.state == "running" { onShift += 1 }
            if a.office?.working == true { working += 1 }
            if a.needsAttention { attention += 1 }
            if (a.pendingLearnings ?? 0) + (a.pendingSkills ?? 0) > 0 { toReview += 1 }
        }
    }
}

extension FleetAgent {
    /// Unhealthy, or a platform (Slack…) reporting it needs attention: the office's red `!`.
    public var needsAttention: Bool {
        if health == "unhealthy" { return true }
        return activity?.platforms?.values.contains(where: \.needsAttention) ?? false
    }

    /// One line for lists: "Working · Slack", "Idle", "Stopped".
    public var statusLine: String {
        if let label = office?.label, !label.isEmpty { return label }
        switch state {
        case "running": return office?.working == true ? "Working" : "On shift"
        case "starting", "restarting": return "Starting"
        case "exited": return "Exited"
        case "stopped": return "Off shift"
        default: return state.capitalized
        }
    }
}

/// The latest `/api/fleet`, polled by the app while the core runs.
@MainActor @Observable
public final class FleetStore {
    public private(set) var fleet: Fleet?
    public private(set) var summary = FleetSummary()
    /// Core clock minus this Mac's, in milliseconds (the office plays back by the core's time).
    public private(set) var clockOffset: Int64 = 0
    public private(set) var lastError: String?
    /// The instance's look (/api/instance); nil on a core without it.
    public private(set) var look: InstanceLook?
    public var client: CoreClient

    public init(client: CoreClient) {
        self.client = client
    }

    public func refresh() async {
        do {
            let f = try await client.fleet()
            fleet = f
            summary = FleetSummary(f)
            clockOffset = f.now - Int64(Date().timeIntervalSince1970 * 1000)
            lastError = nil
            if look == nil { look = try? await client.instance() }
        } catch {
            lastError = String(describing: error)
        }
    }

    public func clear() {
        fleet = nil
        look = nil
        summary = FleetSummary()
    }

    public func agent(_ id: String) -> FleetAgent? { fleet?.agents.first { $0.id == id } }
}
