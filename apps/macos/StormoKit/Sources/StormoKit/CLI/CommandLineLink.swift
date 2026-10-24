import Foundation

/// The terminal's `stormo`: a symlink (`~/.local/bin/stormo` in the app) to the copy inside the app,
/// so the command updates with the app. The app creates it when nothing is there and retargets it
/// when it points into a Stormo app (this copy or an older one); anything else is the user's, and is
/// replaced only when they ask (Settings › Command Line).
public struct CommandLineLink: Sendable {
    /// Where the link lives.
    public var link: URL
    /// The binary inside this app.
    public var bundled: URL

    public init(link: URL, bundled: URL) {
        self.link = link
        self.bundled = bundled
    }

    /// What is at the link path.
    public enum State: Equatable, Sendable {
        /// Nothing.
        case absent
        /// A symlink to this app's binary.
        case current
        /// A symlink into a Stormo app's Contents/Helpers that is not this one (maybe gone).
        case appCopy(target: String)
        /// A symlink anywhere else (a dev checkout, Homebrew…), possibly broken.
        case userLink(target: String, broken: Bool)
        /// A regular file, a directory or anything that is not a symlink.
        case file
    }

    /// What the app does by itself at launch.
    public enum Outcome: Equatable, Sendable {
        /// The link was made where nothing was.
        case created
        /// An older app's link now points at this app.
        case retargeted(from: String)
        /// Already this app's link.
        case unchanged
        /// Someone else's: left alone.
        case left(State)
        /// This app runs from a place that goes away (App Translocation, a disk image): no link to it.
        case skipped
        /// The link could not be written.
        case failed(String)
    }

    public func state() -> State {
        let fm = FileManager.default
        guard let attrs = try? fm.attributesOfItem(atPath: link.path) else { return .absent }
        guard attrs[.type] as? FileAttributeType == .typeSymbolicLink,
              let raw = try? fm.destinationOfSymbolicLink(atPath: link.path)
        else { return .file }
        let target = raw.hasPrefix("/") ? raw : link.deletingLastPathComponent().appending(path: raw).standardizedFileURL.path
        if Self.samePath(target, bundled.path) { return .current }
        if Self.isAppHelper(target) { return .appCopy(target: target) }
        return .userLink(target: target, broken: !fm.fileExists(atPath: target))
    }

    /// Creates the link, or retargets an app's, and says what it did. Never touches anything else.
    public func reconcile() -> Outcome {
        let current = state()
        switch current {
        case .current:
            return .unchanged
        case .userLink, .file:
            return .left(current)
        case .absent, .appCopy:
            if Self.isEphemeral(bundled) { return .skipped }
            do {
                try install()
            } catch {
                return .failed(error.localizedDescription)
            }
            if case .appCopy(let from) = current { return .retargeted(from: from) }
            return .created
        }
    }

    /// Points the link at this app's binary, replacing whatever is there (a symlink or a file) in
    /// one step: a temporary link next to it renamed over it. Creates the directory when needed.
    public func install() throws {
        let fm = FileManager.default
        let dir = link.deletingLastPathComponent()
        try fm.createDirectory(at: dir, withIntermediateDirectories: true)
        let temp = dir.appending(path: ".\(link.lastPathComponent).stormo-\(UUID().uuidString)")
        try fm.createSymbolicLink(atPath: temp.path, withDestinationPath: bundled.path)
        if rename(temp.path, link.path) != 0 {
            let code = errno
            try? fm.removeItem(at: temp)
            throw CocoaError(.fileWriteUnknown, userInfo: [
                NSFilePathErrorKey: link.path,
                NSUnderlyingErrorKey: NSError(domain: NSPOSIXErrorDomain, code: Int(code)),
                NSLocalizedDescriptionKey: "Could not write \(link.path): \(String(cString: strerror(code)))",
            ])
        }
    }

    /// Whether a terminal with this environment finds commands in the link's directory.
    public func directoryIsOnPath(_ environment: [String: String]) -> Bool {
        let dir = link.deletingLastPathComponent().standardizedFileURL.path
        let home = environment["HOME"]
        return (environment["PATH"] ?? "").split(separator: ":").contains { entry in
            var p = String(entry)
            if p.hasPrefix("~/"), let home { p = home + p.dropFirst(1) }
            return Self.samePath(p, dir)
        }
    }

    /// A binary inside some app's Contents/Helpers: `<…>.app/Contents/Helpers/stormo`.
    public static func isAppHelper(_ path: String) -> Bool {
        let parts = URL(filePath: path).standardizedFileURL.pathComponents
        guard parts.count >= 4 else { return false }
        let tail = parts.suffix(4)
        return tail[tail.startIndex].hasSuffix(".app") && Array(tail.dropFirst()) == ["Contents", "Helpers", "stormo"]
    }

    /// An app running translocated or from a read-only volume (an opened disk image): its path goes
    /// away, so a link to it would break.
    public static func isEphemeral(_ bundled: URL) -> Bool {
        if bundled.path.contains("/AppTranslocation/") { return true }
        let readOnly = try? bundled.resourceValues(forKeys: [.volumeIsReadOnlyKey]).volumeIsReadOnly
        return readOnly == true
    }

    static func samePath(_ a: String, _ b: String) -> Bool {
        func norm(_ p: String) -> String {
            var s = URL(filePath: p).standardizedFileURL.path
            while s.count > 1, s.hasSuffix("/") { s.removeLast() }
            return s
        }
        return norm(a) == norm(b)
    }
}
