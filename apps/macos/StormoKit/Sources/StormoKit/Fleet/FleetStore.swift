import Foundation
import Observation

/// The fleet at a glance: what the menu bar and the toolbar count.
public struct FleetSummary: Sendable, Equatable {
    public var total = 0
    // The counts below are the office's modes (OfficeLook.mode), as the web office counts them: an
    // agent is in one of working, idle and attention, never two.
    /// At work or on the way in.
    public var onShift = 0
    /// Working at their desk right now.
    public var working = 0
    /// On shift with nothing to do.
    public var idle = 0
    /// The office's red `!`: unhealthy, a platform asking for attention, or not running unasked.
    public var attention = 0
    /// Agents with learnings or skill proposals waiting for review.
    public var toReview = 0
    /// Everything waiting for review, over all agents.
    public var reviewItems = 0

    public init() {}

    public init(_ fleet: Fleet) {
        total = fleet.agents.count
        for a in fleet.agents {
            let mode = OfficeLook.mode(of: a)
            if mode.present, mode != .unknown { onShift += 1 }
            if mode == .busy { working += 1 }
            if mode == .idle { idle += 1 }
            if mode.needsAttention { attention += 1 }
            if a.toReview > 0 { toReview += 1 }
            reviewItems += a.toReview
        }
    }
}

extension FleetAgent {
    /// Everything of its waiting for review: lessons, skills, runtime schedules and settings.
    public var toReview: Int { (pendingLearnings ?? 0) + (pendingSkills ?? 0) + (pendingChanges ?? 0) }

    /// The office's red `!` (OfficeLook.mode): unhealthy, a detail to read, a platform (Slack…)
    /// reporting it needs attention, or a container that is not running without having been stopped.
    public var needsAttention: Bool { OfficeLook.mode(of: self).needsAttention }

    var platformNeedsAttention: Bool {
        activity?.platforms?.values.contains(where: \.needsAttention) ?? false
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
    /// Deleted agents, hidden until the core's registry (rescanned on a timer) drops them too.
    private var deleted: Set<String> = []

    public init(client: CoreClient) {
        self.client = client
    }

    public func refresh() async {
        do {
            var f = try await client.fleet()
            deleted.formIntersection(f.agents.map(\.id))
            f.agents.removeAll { deleted.contains($0.id) }
            fleet = f
            summary = FleetSummary(f)
            clockOffset = f.now - Int64(Date().timeIntervalSince1970 * 1000)
            lastError = nil
            if look == nil { look = try? await client.instance() }
        } catch {
            lastError = String(describing: error)
        }
    }

    /// Drops a deleted agent at once instead of at the core's next rescan.
    public func forget(_ id: String) {
        deleted.insert(id)
        fleet?.agents.removeAll { $0.id == id }
        if let fleet { summary = FleetSummary(fleet) }
    }

    public func clear() {
        fleet = nil
        look = nil
        summary = FleetSummary()
    }

    public func agent(_ id: String) -> FleetAgent? { fleet?.agents.first { $0.id == id } }
}
