import Foundation
import Observation

/// One configuration file open in an editor (docs/api.md, `config`): loaded with `config show`,
/// saved whole with `config write --if-hash`, so the engine validates every save and refuses one
/// made over a version that changed on disk since.
@MainActor @Observable
public final class ConfigEditor {
    /// A refused load or save: the CLI's code (conflict, invalid, readonly, …) and its message.
    public struct Problem: Equatable, Sendable {
        public var code: String
        public var message: String
    }

    public let path: String
    /// The text being edited.
    public var text = ""
    /// The version on disk the edits are based on.
    public private(set) var file: ConfigFile?
    public private(set) var problem: Problem?
    public private(set) var busy = false

    public init(path: String) { self.path = path }

    public var isDirty: Bool { file.map { $0.text != text } ?? false }
    /// The file changed on disk since it was loaded: reload (dropping the edits) to go on.
    public var isStale: Bool { problem?.code == "conflict" }

    /// Reads the file, replacing any edits.
    public func load(with cli: StormoCLI) async {
        busy = true
        defer { busy = false }
        do {
            let f = try await cli.run(["config", "show", path], as: ConfigFile.self)
            file = f
            text = f.text
            problem = nil
        } catch {
            problem = Self.problem(error)
        }
    }

    /// Writes the edits; true when the file now holds them. Typing during the save is kept.
    public func save(with cli: StormoCLI) async -> Bool {
        guard let base = file, isDirty, !busy else { return false }
        busy = true
        defer { busy = false }
        do {
            file = try await cli.run(["config", "write", path, "--if-hash", base.hash], as: ConfigFile.self, stdin: Data(text.utf8))
            problem = nil
            return true
        } catch {
            problem = Self.problem(error)
            return false
        }
    }

    /// Drops the edits.
    public func revert() {
        if let file { text = file.text }
        if !isStale { problem = nil }
    }

    static func problem(_ error: Error) -> Problem {
        if let e = error as? CLIError { return Problem(code: e.code, message: e.errorDescription ?? e.message) }
        return Problem(code: "failed", message: error.localizedDescription)
    }
}
