import Foundation

/// A JSON document that keeps integers apart from decimals, so a value read from YAML as `2048`
/// goes back as `2048`, never `2048.0` (which YAML would read as a float).
public enum JSONValue: Sendable, Hashable, Codable {
    case null
    case bool(Bool)
    case int(Int64)
    case double(Double)
    case string(String)
    case array([JSONValue])
    case object([String: JSONValue])

    public init(from decoder: Decoder) throws {
        let c = try decoder.singleValueContainer()
        if c.decodeNil() { self = .null }
        else if let b = try? c.decode(Bool.self) { self = .bool(b) }
        else if let i = try? c.decode(Int64.self) { self = .int(i) }
        else if let d = try? c.decode(Double.self) { self = .double(d) }
        else if let s = try? c.decode(String.self) { self = .string(s) }
        else if let a = try? c.decode([JSONValue].self) { self = .array(a) }
        else { self = .object(try c.decode([String: JSONValue].self)) }
    }

    public func encode(to encoder: Encoder) throws {
        var c = encoder.singleValueContainer()
        switch self {
        case .null: try c.encodeNil()
        case .bool(let b): try c.encode(b)
        case .int(let i): try c.encode(i)
        case .double(let d): try c.encode(d)
        case .string(let s): try c.encode(s)
        case .array(let a): try c.encode(a)
        case .object(let o): try c.encode(o)
        }
    }

    public var string: String? { if case .string(let s) = self { s } else { nil } }
    public var bool: Bool? { if case .bool(let b) = self { b } else { nil } }
    public var int: Int64? {
        switch self {
        case .int(let i): i
        case .double(let d) where d.rounded() == d: Int64(exactly: d)
        default: nil
        }
    }
    public var double: Double? {
        switch self {
        case .int(let i): Double(i)
        case .double(let d): d
        default: nil
        }
    }
    public var array: [JSONValue]? { if case .array(let a) = self { a } else { nil } }
    public var object: [String: JSONValue]? { if case .object(let o) = self { o } else { nil } }

    /// Equal as data: 2 and 2.0 are the same number.
    public func same(as other: JSONValue) -> Bool {
        switch (self, other) {
        case (.int, .double), (.double, .int): return double == other.double
        case (.array(let a), .array(let b)): return a.count == b.count && zip(a, b).allSatisfy { $0.same(as: $1) }
        case (.object(let a), .object(let b)):
            return a.count == b.count && a.allSatisfy { k, v in b[k].map { v.same(as: $0) } ?? false }
        default: return self == other
        }
    }

    // MARK: Paths

    public enum Key: Hashable, Sendable, ExpressibleByStringLiteral, ExpressibleByIntegerLiteral {
        case key(String)
        case index(Int)
        public init(stringLiteral value: String) { self = .key(value) }
        public init(integerLiteral value: Int) { self = .index(value) }
    }

    /// The value at path, nil when any step is missing.
    public subscript(path: [Key]) -> JSONValue? {
        var v: JSONValue? = self
        for k in path {
            switch (k, v) {
            case (.key(let name), .object(let o)?): v = o[name]
            case (.index(let i), .array(let a)?) where a.indices.contains(i): v = a[i]
            default: return nil
            }
        }
        return v
    }

    /// Sets the value at path, making the objects on the way; nil removes an object's key.
    public mutating func set(_ path: [Key], _ value: JSONValue?) {
        guard let first = path.first else {
            self = value ?? .null
            return
        }
        let rest = Array(path.dropFirst())
        switch first {
        case .key(let name):
            var o = object ?? [:]
            if rest.isEmpty {
                o[name] = value
            } else {
                var child = o[name] ?? .object([:])
                child.set(rest, value)
                o[name] = child
            }
            self = .object(o)
        case .index(let i):
            guard var a = array, a.indices.contains(i) else { return }
            if rest.isEmpty {
                if let value { a[i] = value } else { a.remove(at: i) }
            } else {
                a[i].set(rest, value)
            }
            self = .array(a)
        }
    }
}

/// JSON merge patches (RFC 7386), as `stormo config apply` takes them.
public enum MergePatch {
    /// The patch that turns `old` into `new`: only what changed, `null` for a removed key, arrays
    /// whole. nil when they are the same.
    public static func diff(from old: JSONValue, to new: JSONValue) -> JSONValue? {
        if old.same(as: new) { return nil }
        guard let o = old.object, let n = new.object else { return new }
        var patch: [String: JSONValue] = [:]
        for key in Set(o.keys).union(n.keys) {
            switch (o[key], n[key]) {
            case (_?, nil): patch[key] = .null
            case (nil, let v?): patch[key] = v
            case (let a?, let b?):
                if let d = diff(from: a, to: b) { patch[key] = d }
            default: break
            }
        }
        return patch.isEmpty ? nil : .object(patch)
    }

    /// The patch as the bytes `config apply` reads: keys sorted, so new keys land in a stable order.
    public static func data(_ patch: JSONValue) throws -> Data {
        let e = JSONEncoder()
        e.outputFormatting = [.sortedKeys, .withoutEscapingSlashes]
        return try e.encode(patch)
    }
}
