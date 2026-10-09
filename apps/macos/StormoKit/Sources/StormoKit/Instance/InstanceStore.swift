import Foundation
import Observation

/// An instance the app knows, and the binary pinned for it.
public struct InstanceRecord: Codable, Sendable, Equatable, Identifiable {
    public var root: String
    public var name: String
    public var slug: String
    /// The pinned binary's path (symlinks resolved), nil until first resolved (Auto).
    public var binary: String?
    /// How it was chosen: auto, or a fixed source picked in Settings.
    public var binaryChoice: String?

    public var id: String { root }
    public var url: URL { URL(filePath: root) }

    public init(root: String, name: String, slug: String, binary: String? = nil, binaryChoice: String? = nil) {
        self.root = root
        self.name = name
        self.slug = slug
        self.binary = binary
        self.binaryChoice = binaryChoice
    }
}

/// The instances the user opened, and which one is active (one at a time: one core per machine).
@MainActor @Observable
public final class InstanceStore {
    public private(set) var instances: [InstanceRecord] = []
    public private(set) var activeRoot: String?
    private let defaults: UserDefaults

    private static let listKey = "instances.v1"
    private static let activeKey = "instances.active"

    public init(defaults: UserDefaults = .standard) {
        self.defaults = defaults
        if let data = defaults.data(forKey: Self.listKey),
           let list = try? JSONDecoder().decode([InstanceRecord].self, from: data) {
            instances = list
        }
        activeRoot = defaults.string(forKey: Self.activeKey)
        if active == nil { activeRoot = instances.first?.root }
    }

    public var active: InstanceRecord? { instances.first { $0.root == activeRoot } }

    /// Adds or refreshes an instance (validated by `stormo instance --json`) and makes it active.
    public func add(_ info: InstanceInfo) {
        if let i = instances.firstIndex(where: { $0.root == info.root }) {
            instances[i].name = info.name
            instances[i].slug = info.slug
        } else {
            instances.append(InstanceRecord(root: info.root, name: info.name, slug: info.slug))
        }
        activate(info.root)
    }

    public func activate(_ root: String) {
        guard instances.contains(where: { $0.root == root }) else { return }
        activeRoot = root
        save()
    }

    public func remove(_ root: String) {
        instances.removeAll { $0.root == root }
        if activeRoot == root { activeRoot = instances.first?.root }
        save()
    }

    /// Pins a binary for an instance.
    public func pin(_ root: String, binary: String, choice: String) {
        guard let i = instances.firstIndex(where: { $0.root == root }) else { return }
        instances[i].binary = binary
        instances[i].binaryChoice = choice
        save()
    }

    private func save() {
        if let data = try? JSONEncoder().encode(instances) { defaults.set(data, forKey: Self.listKey) }
        defaults.set(activeRoot, forKey: Self.activeKey)
    }
}
