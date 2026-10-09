import Foundation
import Observation

/// One configuration file open in an editor (docs/api.md, `config`): loaded with `config show`,
/// saved whole with `config write --if-hash`, or, for an agent.yaml edited as a form, as a merge
/// patch with `config apply --if-hash`; the engine validates every save and refuses one made over a
/// version that changed on disk since.
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
    /// The document being edited as a form (agent.yaml), nil when the file does not parse.
    public var doc: JSONValue?
    /// The version on disk the edits are based on.
    public private(set) var file: ConfigFile?
    public private(set) var problem: Problem?
    public private(set) var busy = false

    public init(path: String) { self.path = path }

    public var isDirty: Bool { isTextDirty || isFormDirty }
    public var isTextDirty: Bool { file.map { $0.text != text } ?? false }
    public var isFormDirty: Bool { patch != nil }
    /// What a form save would send: the changes from the loaded document, nil when none.
    public var patch: JSONValue? {
        guard let base = file?.doc, let doc else { return nil }
        return MergePatch.diff(from: base, to: doc)
    }
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
            doc = f.doc
            problem = nil
        } catch {
            problem = Self.problem(error)
        }
    }

    /// Writes the edits, the form's as a patch or the text whole (not both: one is reverted first);
    /// true when the file now holds them. The written file is loaded back, so the form and the text
    /// agree again.
    public func save(with cli: StormoCLI) async -> Bool {
        guard let base = file, isDirty, !busy else { return false }
        if isFormDirty && isTextDirty {
            problem = Problem(code: "usage", message: "Changed both as a form and as text: revert one of them before saving.")
            return false
        }
        busy = true
        defer { busy = false }
        do {
            let saved: ConfigFile
            if let patch {
                saved = try await cli.run(["config", "apply", path, "--if-hash", base.hash], as: ConfigFile.self, stdin: try MergePatch.data(patch))
            } else {
                saved = try await cli.run(["config", "write", path, "--if-hash", base.hash], as: ConfigFile.self, stdin: Data(text.utf8))
            }
            file = saved
            text = saved.text
            doc = saved.doc
            problem = nil
            return true
        } catch {
            problem = Self.problem(error)
            return false
        }
    }

    /// Drops the edits.
    public func revert() {
        if let file {
            text = file.text
            doc = file.doc
        }
        if !isStale { problem = nil }
    }

    /// Clears the last problem (a new action is starting).
    public func problemReset() { problem = nil }

    /// Shows error as this editor's problem (an action on its file failed).
    public func report(_ error: Error) { problem = Self.problem(error) }

    static func problem(_ error: Error) -> Problem {
        if let e = error as? CLIError { return Problem(code: e.code, message: e.errorDescription ?? e.message) }
        return Problem(code: "failed", message: error.localizedDescription)
    }
}
