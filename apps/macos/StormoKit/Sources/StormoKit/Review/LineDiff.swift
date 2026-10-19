import Foundation

/// A unified line diff (old → new) in hunks with context, for the Review section.
public struct LineDiff: Sendable, Equatable {
    public enum Kind: Sendable, Equatable { case same, removed, added }

    public struct Line: Sendable, Equatable, Identifiable {
        public var kind: Kind
        public var text: String
        /// 1-based line numbers in the old and new text (nil on the side it is not in).
        public var old: Int?
        public var new: Int?
        public var id: String { "\(old ?? 0):\(new ?? 0)" }
    }

    public struct Hunk: Sendable, Equatable, Identifiable {
        public var lines: [Line]
        /// Unchanged lines skipped before this hunk.
        public var skippedBefore: Int
        public var id: String { lines.first?.id ?? "" }
    }

    public var hunks: [Hunk]
    /// Unchanged lines after the last hunk.
    public var skippedAfter: Int
    public var added: Int
    public var removed: Int

    public init(old: String, new: String, context: Int = 3) {
        let a = Self.lines(old), b = Self.lines(new)
        var all: [Line] = []
        var removedAt = Set<Int>(), insertedAt = Set<Int>()
        for change in b.difference(from: a) {
            switch change {
            case .remove(let o, _, _): removedAt.insert(o)
            case .insert(let o, _, _): insertedAt.insert(o)
            }
        }
        var i = 0, j = 0
        while i < a.count || j < b.count {
            if i < a.count, removedAt.contains(i) {
                all.append(Line(kind: .removed, text: a[i], old: i + 1, new: nil)); i += 1
            } else if j < b.count, insertedAt.contains(j) {
                all.append(Line(kind: .added, text: b[j], old: nil, new: j + 1)); j += 1
            } else {
                all.append(Line(kind: .same, text: a[i], old: i + 1, new: j + 1)); i += 1; j += 1
            }
        }
        added = insertedAt.count
        removed = removedAt.count

        // Keep changed lines and `context` lines around them; the rest collapses.
        var keep = [Bool](repeating: false, count: all.count)
        for (k, l) in all.enumerated() where l.kind != .same {
            for c in max(0, k - context)...min(all.count - 1, k + context) { keep[c] = true }
        }
        hunks = []
        var skipped = 0, current: [Line] = []
        for (k, l) in all.enumerated() {
            if keep[k] {
                current.append(l)
            } else {
                if !current.isEmpty { hunks.append(Hunk(lines: current, skippedBefore: skipped)); current = []; skipped = 0 }
                skipped += 1
            }
        }
        if !current.isEmpty { hunks.append(Hunk(lines: current, skippedBefore: skipped)); skipped = 0 }
        skippedAfter = skipped
    }

    public var isEmpty: Bool { added == 0 && removed == 0 }

    static func lines(_ s: String) -> [String] {
        if s.isEmpty { return [] }
        var parts = s.components(separatedBy: "\n")
        if parts.last == "" { parts.removeLast() }
        return parts
    }
}
