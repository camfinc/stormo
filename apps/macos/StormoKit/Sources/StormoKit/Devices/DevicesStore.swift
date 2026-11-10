import Foundation
import Observation

/// The Linked Devices page: the devices and pending pairings the core keeps, linking one by the code
/// it shows, and changing or revoking one. Held in memory only (rows carry IP addresses).
@MainActor @Observable
public final class DevicesStore {
    public private(set) var devices: [LinkedDevice] = []
    public private(set) var pairings: [DevicePairing] = []
    /// The list has been read from this core at least once.
    public private(set) var loaded = false
    /// Why the list cannot be read now (an older core, the core down, a token without read scope).
    public private(set) var problem: String?
    /// The device gateway (the ingress capability) when the core has one; nil when it has none or
    /// its capabilities could not be read (`gatewayKnown`).
    public private(set) var gateway: CapabilityInfo?
    public private(set) var gatewayKnown = false

    /// The pending pairing the owner looked up by its code, and that code as the device shows it.
    public private(set) var preview: DevicePairing?
    public private(set) var previewCode: String?
    /// Why the last look-up, link or denial failed, in the core's words.
    public var linkProblem: String?
    /// Why the last revocation failed, in the core's words.
    public var actionProblem: String?
    /// Calls on their way: "lookup", "link", "deny", or a device's id.
    public private(set) var busy: Set<String> = []

    public var client: CoreClient {
        didSet { if client.base != oldValue.base || client.token != oldValue.token { clear() } }
    }

    public init(client: CoreClient) { self.client = client }

    /// Linked now, by name.
    public var linked: [LinkedDevice] {
        devices.filter { !$0.isRevoked }.sorted { $0.name.localizedStandardCompare($1.name) == .orderedAscending }
    }

    /// Revoked, the latest first.
    public var revoked: [LinkedDevice] {
        devices.filter(\.isRevoked).sorted { ($0.revoked ?? 0) > ($1.revoked ?? 0) }
    }

    /// No device was ever linked and none waits: the page explains how to set the gateway up.
    public var isEmpty: Bool { devices.isEmpty && pairings.isEmpty }

    public func load() async {
        do {
            let list = try await client.devices()
            devices = list.devices
            pairings = list.pairings.sorted { ($0.created ?? 0) > ($1.created ?? 0) }
            loaded = true
            problem = nil
        } catch CoreClient.Failure.notFound, CoreClient.Failure.refused(404, _) {
            // An older core answers a route it lacks with an explained 404.
            problem = Self.noDevicesAPI
        } catch {
            problem = Self.describe(error)
        }
        if let caps = try? await client.capabilities().capabilities {
            gateway = Self.gateway(in: caps)
            gatewayKnown = true
        } else {
            gatewayKnown = false
        }
    }

    /// The device gateway among the core's capabilities: an ingress one from the device-gateway
    /// template first, else any ingress one.
    public nonisolated static func gateway(in capabilities: [CapabilityInfo]) -> CapabilityInfo? {
        let ingress = capabilities.filter(\.isIngress)
        return ingress.first { $0.template == "device-gateway" } ?? ingress.first
    }

    /// Reads what the pending pairing with this code claims (changing nothing). False when there is
    /// none: `linkProblem` says why.
    @discardableResult
    public func lookUp(code: String) async -> Bool {
        let code = PairingCode.format(code)
        busy.insert("lookup")
        defer { busy.remove("lookup") }
        do {
            preview = try await client.previewPairing(code: code)
            previewCode = code
            linkProblem = nil
            return true
        } catch {
            preview = nil
            previewCode = nil
            linkProblem = Self.describe(error)
            return false
        }
    }

    /// Links the looked-up device. `name` empty or the one it gave keeps the name it gave.
    @discardableResult
    public func link(name: String, grants: DeviceGrants) async -> LinkedDevice? {
        guard let code = previewCode else { return nil }
        let trimmed = name.trimmingCharacters(in: .whitespacesAndNewlines)
        busy.insert("link")
        defer { busy.remove("link") }
        do {
            let device = try await client.linkDevice(code: code, name: trimmed.isEmpty || trimmed == preview?.name ? nil : trimmed, grants: grants)
            cancelLink()
            await load()
            return device
        } catch {
            linkProblem = Self.describe(error)
            return nil
        }
    }

    /// Refuses the looked-up device: it is told, and must pair again to try once more.
    @discardableResult
    public func deny() async -> Bool {
        guard let code = previewCode else { return false }
        busy.insert("deny")
        defer { busy.remove("deny") }
        do {
            _ = try await client.denyPairing(code: code)
            cancelLink()
            await load()
            return true
        } catch {
            linkProblem = Self.describe(error)
            return false
        }
    }

    /// Forgets the looked-up pairing (it stays pending on the core until it expires).
    public func cancelLink() {
        preview = nil
        previewCode = nil
        linkProblem = nil
    }

    /// Renames a device or changes its grants (nil: unchanged). Returns the problem, nil when done.
    public func update(_ id: String, name: String?, grants: DeviceGrants?) async -> String? {
        busy.insert(id)
        defer { busy.remove(id) }
        do {
            let device = try await client.updateDevice(id, name: name, grants: grants)
            if let i = devices.firstIndex(where: { $0.id == id }) { devices[i] = device }
            return nil
        } catch {
            return Self.describe(error)
        }
    }

    /// Revokes a device: its sessions stop at their next call.
    @discardableResult
    public func revoke(_ id: String) async -> Bool {
        busy.insert(id)
        defer { busy.remove(id) }
        do {
            _ = try await client.revokeDevice(id)
            actionProblem = nil
            await load()
            return true
        } catch {
            actionProblem = Self.describe(error)
            return false
        }
    }

    public func clear() {
        devices = []
        pairings = []
        loaded = false
        problem = nil
        gateway = nil
        gatewayKnown = false
        cancelLink()
        actionProblem = nil
    }

    nonisolated static let noDevicesAPI = "This core has no linked devices yet: restart it with the app's stormo."

    /// A failed call as the owner reads it: the core's own explanation when it gave one.
    nonisolated static func describe(_ error: Error) -> String {
        switch error {
        case CoreClient.Failure.refused(_, let message): message
        case CoreClient.Failure.notFound: noDevicesAPI
        case CoreClient.Failure.unreachable: "The core does not answer."
        case CoreClient.Failure.status(let code): "The core answered HTTP \(code)."
        default: error.localizedDescription
        }
    }
}
