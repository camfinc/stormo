import Foundation

/// One entry of a shared-space directory (`GET /api/workdir/ls`).
public struct WorkdirEntry: Codable, Sendable, Equatable, Identifiable {
    public var name: String
    public var dir: Bool
    public var size: Int64
    public var modified: String?
    public var lastWriter: String?
    /// The holder and kind of a lock covering it ("atlas (hard)").
    public var locked: String?
    public var id: String { name }

    public init(name: String, dir: Bool = false, size: Int64 = 0, modified: String? = nil, lastWriter: String? = nil, locked: String? = nil) {
        self.name = name; self.dir = dir; self.size = size; self.modified = modified; self.lastWriter = lastWriter; self.locked = locked
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        name = try c.decode(String.self, forKey: .name)
        dir = try c.decodeIfPresent(Bool.self, forKey: .dir) ?? false
        size = try c.decodeIfPresent(Int64.self, forKey: .size) ?? 0
        modified = try c.decodeIfPresent(String.self, forKey: .modified)
        lastWriter = try c.decodeIfPresent(String.self, forKey: .lastWriter)
        locked = try c.decodeIfPresent(String.self, forKey: .locked)
    }
}

/// What the core knows of a file (`GET /api/workdir/info`).
public struct WorkdirFileInfo: Codable, Sendable, Equatable {
    public struct Lock: Codable, Sendable, Equatable {
        public var owner: String
        public var path: String
        public var kind: String
        public var reason: String?
        public var expires: String
    }

    public struct Event: Codable, Sendable, Equatable {
        public var at: String
        public var kind: String
        public var agent: String
        public var how: String
    }

    public var path: String
    public var exists: Bool
    public var creator: String?
    public var lastWriter: String?
    public var locks: [Lock]
    public var history: [Event]
}

/// A file's content (`GET /api/workdir/file`): the first MiB, as text unless it is binary.
public struct WorkdirFile: Codable, Sendable, Equatable {
    public var path: String
    public var size: Int64
    public var modified: String
    public var truncated: Bool?
    public var binary: Bool?
    public var text: String?
}
