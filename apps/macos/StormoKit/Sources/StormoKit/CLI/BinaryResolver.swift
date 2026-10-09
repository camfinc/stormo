import Foundation

/// A `stormo` binary the app could use, probed with `version --json`.
public struct BinaryCandidate: Sendable, Equatable, Identifiable {
    public enum Source: String, Sendable, Codable, CaseIterable {
        /// Inside the app: always present, same release as the app.
        case bundled
        /// `stormo` on the shell's PATH.
        case path
        /// ~/.local/bin, /opt/homebrew/bin, /usr/local/bin.
        case usual
        /// The instance's own engine checkout: <instance>/stormo/bin/stormo.
        case vendored
        /// Chosen in Settings.
        case custom
    }

    public var url: URL
    /// The file it is, symlinks resolved: two candidates with the same file are one.
    public var resolved: URL
    public var source: Source
    public var info: VersionInfo?
    /// Why it cannot be used (too old, not runnable), nil when usable.
    public var problem: String?

    public var id: String { resolved.path }
    public var usable: Bool { problem == nil && info != nil }
}

/// Finds and probes the `stormo` binaries on this Mac (docs/macos-app.md, Binary resolution).
public struct BinaryResolver: Sendable {
    public var bundled: URL?
    public var environment: [String: String]
    public var home: URL

    public static let usualDirectories = ["~/.local/bin", "/opt/homebrew/bin", "/usr/local/bin"]

    public init(bundled: URL?, environment: [String: String], home: URL = FileManager.default.homeDirectoryForCurrentUser) {
        self.bundled = bundled
        self.environment = environment
        self.home = home
    }

    /// Where binaries might be, in preference order, before probing.
    public func locations(instance: URL?, custom: URL?) -> [(URL, BinaryCandidate.Source)] {
        var out: [(URL, BinaryCandidate.Source)] = []
        if let custom { out.append((custom, .custom)) }
        for dir in (environment["PATH"] ?? "").split(separator: ":") where !dir.isEmpty {
            out.append((URL(filePath: String(dir)).appending(path: "stormo"), .path))
        }
        for dir in Self.usualDirectories {
            let expanded = dir.hasPrefix("~/") ? home.appending(path: String(dir.dropFirst(2))) : URL(filePath: dir)
            out.append((expanded.appending(path: "stormo"), .usual))
        }
        if let instance { out.append((instance.appending(path: "stormo/bin/stormo"), .vendored)) }
        if let bundled { out.append((bundled, .bundled)) }
        return out
    }

    /// Every distinct existing binary, probed. A file reached two ways keeps its first source,
    /// except that the bundled binary is always reported as bundled.
    public func candidates(instance: URL?, custom: URL? = nil) async -> [BinaryCandidate] {
        var seen: [String: Int] = [:]
        var found: [BinaryCandidate] = []
        for (url, source) in locations(instance: instance, custom: custom) {
            guard FileManager.default.isExecutableFile(atPath: url.path) else { continue }
            let resolved = url.resolvingSymlinksInPath()
            if let i = seen[resolved.path] {
                if source == .bundled { found[i].source = .bundled }
                continue
            }
            seen[resolved.path] = found.count
            found.append(BinaryCandidate(url: url, resolved: resolved, source: source))
        }
        return await withTaskGroup(of: (Int, BinaryCandidate).self) { group in
            for (i, c) in found.enumerated() {
                group.addTask { (i, await probe(c)) }
            }
            var probed = found
            for await (i, c) in group { probed[i] = c }
            return probed
        }
    }

    /// Runs `version --json`; a binary without it (or with an API outside the supported range) is
    /// marked with the problem.
    public func probe(_ candidate: BinaryCandidate) async -> BinaryCandidate {
        var c = candidate
        let cli = StormoCLI(executable: c.url, environment: environment, instance: nil)
        do {
            let info = try await cli.run(["version"], as: VersionInfo.self)
            c.info = info
            if !StormoAPI.supported.contains(info.api) {
                c.problem = "speaks API \(info.api); this app needs \(StormoAPI.supported.lowerBound)–\(StormoAPI.supported.upperBound)"
            }
        } catch {
            c.problem = "too old for this app (no version --json)"
        }
        return c
    }

    /// Auto's choice (docs/macos-app.md): the binary behind the instance's running core, else an
    /// installed CLI, else the bundled one.
    public static func autoChoice(_ candidates: [BinaryCandidate], runningCoreExe: String?) -> BinaryCandidate? {
        let usable = candidates.filter(\.usable)
        if let exe = runningCoreExe {
            let real = URL(filePath: exe).resolvingSymlinksInPath().path
            if let c = usable.first(where: { $0.resolved.path == real }) { return c }
        }
        for source in [BinaryCandidate.Source.path, .usual, .vendored] {
            if let c = usable.first(where: { $0.source == source }) { return c }
        }
        return usable.first(where: { $0.source == .bundled }) ?? usable.first
    }
}
