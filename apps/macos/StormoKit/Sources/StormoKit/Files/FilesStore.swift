import Foundation
import Observation

/// The Files section: the shared space the agents mount, browsed read-only with the owner token.
/// `group` is shared by every agent; each unit's layer by that unit's agents only. Held in memory
/// only: file names and contents can name clients.
@MainActor @Observable
public final class FilesStore {
    /// `group` first, then each unit.
    public private(set) var layers: [String] = []
    /// The directory shown, as agents see it: `/shared/<layer>/…`.
    public private(set) var directory: String?
    public private(set) var entries: [WorkdirEntry] = []
    /// The file selected in `directory`, its content and what the core knows of it.
    public private(set) var selected: String?
    public private(set) var file: WorkdirFile?
    public private(set) var info: WorkdirFileInfo?
    public private(set) var loading = false
    public var problem: String?

    public var client: CoreClient
    public var instanceRoot: URL? {
        didSet { if instanceRoot != oldValue { clear() } }
    }

    public init(client: CoreClient, instanceRoot: URL?) {
        self.client = client
        self.instanceRoot = instanceRoot
    }

    /// `/shared/<layer>`.
    public nonisolated static func root(of layer: String) -> String { "/shared/\(layer)" }

    /// The layer of a `/shared/<layer>/…` path.
    public nonisolated static func layer(of path: String) -> String? {
        let parts = path.split(separator: "/")
        guard parts.count >= 2, parts[0] == "shared" else { return nil }
        return String(parts[1])
    }

    /// `/shared/<layer>` and each directory below it, for a path bar: (name, path).
    public nonisolated static func crumbs(of path: String) -> [(name: String, path: String)] {
        let parts = path.split(separator: "/").map(String.init)
        guard parts.count >= 2 else { return [] }
        var out: [(String, String)] = []
        var p = "/shared"
        for name in parts.dropFirst() {
            p += "/" + name
            out.append((name, p))
        }
        return out
    }

    public var layer: String? { directory.flatMap(Self.layer(of:)) }

    /// The selected file's name in `directory`.
    public var selectedName: String? {
        guard let s = selected, let d = directory, s.hasPrefix(d + "/") else { return nil }
        return String(s.dropFirst(d.count + 1))
    }

    public func loadLayers() async {
        guard let owner = token() else { return }
        do {
            layers = try await client.workdirLayers(owner: owner)
            problem = nil
            if directory == nil, let first = layers.first { await open(directory: Self.root(of: first)) }
        } catch {
            problem = Self.describe(error)
        }
    }

    public func open(directory path: String) async {
        guard let owner = token() else { return }
        loading = true
        defer { loading = false }
        do {
            let es = try await client.workdirList(path: path, owner: owner)
            directory = path
            entries = es.sorted { ($0.dir ? 0 : 1, $0.name.localizedLowercase) < ($1.dir ? 0 : 1, $1.name.localizedLowercase) }
            selected = nil; file = nil; info = nil
            problem = nil
        } catch {
            problem = Self.describe(error)
        }
    }

    /// Opens a directory entry, or selects a file entry and loads its content and history.
    public func choose(_ entry: WorkdirEntry) async {
        guard let dir = directory else { return }
        let path = dir + "/" + entry.name
        if entry.dir { return await open(directory: path) }
        guard let owner = token() else { return }
        selected = path
        file = nil; info = nil
        do {
            async let f = client.workdirFile(path: path, owner: owner)
            async let i = client.workdirInfo(path: path, owner: owner)
            let (ff, ii) = try await (f, i)
            guard selected == path else { return }
            file = ff; info = ii
            problem = nil
        } catch {
            if selected == path { problem = Self.describe(error) }
        }
    }

    public func reload() async {
        let keep = selectedName
        await loadLayers()
        if let d = directory { await open(directory: d) }
        if let keep, let e = entries.first(where: { $0.name == keep }) { await choose(e) }
    }

    public func clear() {
        layers = []; directory = nil; entries = []
        selected = nil; file = nil; info = nil; problem = nil
    }

    private func token() -> String? {
        guard let root = instanceRoot else { return nil }
        guard let t = ReviewStore.ownerToken(root: root) else {
            problem = "No owner token yet: start the core once to mint it."
            return nil
        }
        return t
    }

    nonisolated static func describe(_ error: Error) -> String {
        switch error {
        case CoreClient.Failure.notFound: return "This core has no files API: restart it with the app's stormo."
        default: return ReviewStore.describe(error)
        }
    }
}
