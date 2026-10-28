import Foundation
import Observation

/// The instance's units for the Units section (docs/api.md, `config units`), and the files that
/// make a new one: a unit is its units/<id>/unit.yaml, its knowledge the Markdown files beside it.
/// Each file is then edited with a `ConfigEditor`.
@MainActor @Observable
public final class UnitCatalog {
    public private(set) var units: [UnitInfo] = []
    public private(set) var problem: ConfigEditor.Problem?
    public private(set) var loaded = false

    public init() {}

    public func unit(_ id: String) -> UnitInfo? { units.first { $0.id == id } }

    public func load(with cli: StormoCLI) async {
        do {
            units = try await cli.run(["config", "units"], as: [UnitInfo].self)
            problem = nil
        } catch {
            problem = ConfigEditor.problem(error)
        }
        loaded = true
    }

    /// A new unit's id: its name as a slug.
    public nonisolated static func unitID(from name: String) -> String { InstanceLocation.slug(from: name) }

    /// A knowledge file's name from what was typed: a slug with `.md` (it becomes a heading).
    public nonisolated static func knowledgeName(from typed: String) -> String {
        var base = typed.trimmingCharacters(in: .whitespaces)
        if base.lowercased().hasSuffix(".md") { base = String(base.dropLast(3)) }
        let slug = InstanceLocation.slug(from: base)
        return slug.isEmpty ? "" : slug + ".md"
    }

    /// unit.yaml for a new unit. Values are JSON strings, which YAML reads as double-quoted scalars.
    public nonisolated static func unitYAML(id: String, name: String, description: String) -> String {
        func q(_ s: String) -> String {
            let data = (try? JSONEncoder().encode(s)) ?? Data("\"\"".utf8)
            return String(decoding: data, as: UTF8.self)
        }
        return "id: \(id)\nname: \(q(name))\ndescription: \(q(description))\n"
    }

    /// Creates units/<id>/unit.yaml; the unit it made, or nil with `problem` set.
    public func createUnit(id: String, name: String, description: String, with cli: StormoCLI) async -> UnitInfo? {
        let body = Self.unitYAML(id: id, name: name, description: description)
        guard await create("units/\(id)/unit.yaml", body: body, with: cli) else { return nil }
        return unit(id)
    }

    /// Creates units/<unit>/knowledge/<name>; its path, or nil with `problem` set.
    public func createKnowledge(unit: String, name: String, with cli: StormoCLI) async -> String? {
        let path = "units/\(unit)/knowledge/\(name)"
        let title = String(name.dropLast(3)).replacingOccurrences(of: "-", with: " ").capitalized
        guard await create(path, body: "# \(title)\n\n", with: cli) else { return nil }
        return path
    }

    private func create(_ path: String, body: String, with cli: StormoCLI) async -> Bool {
        do {
            _ = try await cli.run(["config", "create", path], as: ConfigFile.self, stdin: Data(body.utf8))
            problem = nil
        } catch {
            problem = ConfigEditor.problem(error)
            return false
        }
        await load(with: cli)
        return true
    }

    /// Clears the last problem (it was shown, or a new action is starting).
    public func problemReset() { problem = nil }
}
