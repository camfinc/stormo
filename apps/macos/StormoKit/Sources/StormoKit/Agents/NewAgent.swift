import Foundation

/// `new agent --options`: what a new agent's wizard may offer, and what it starts from.
public struct NewAgentOptions: Codable, Sendable, Equatable {
    public struct Engine: Codable, Sendable, Equatable {
        public var kind: String?
        public var version: String?
        public var imageTag: String?
    }
    public struct Model: Codable, Sendable, Equatable {
        public var name: String?
        public var provider: String?
        public var localName: String?
        public var localConnection: String?
    }
    public struct Defaults: Codable, Sendable, Equatable {
        public var unit: String?
        public var engine: Engine
        public var model: Model
    }
    public var options: AgentOptions
    public var defaults: Defaults
    public var takenIds: [String]
}

/// The JSON `new agent` reads on stdin (docs/api.md). Secret values never go here.
public struct NewAgentSpec: Codable, Sendable, Equatable {
    public struct Unit: Codable, Sendable, Equatable {
        public var id: String
        public var name: String
        public var description: String
    }
    public struct Channel: Codable, Sendable, Equatable {
        public var kind: String
        public var allowedUsers: [String]?
        public var homeChannel: String?
        public var allowBots: String?
    }
    public var id: String
    public var name: String
    public var role: String?
    public var unit: String?
    public var newUnit: Unit?
    public var persona: String?
    public var engine: NewAgentOptions.Engine
    public var model: NewAgentOptions.Model
    public var channels: [Channel]
    public var soul: String
}

/// What `new agent` made.
public struct NewAgentResult: Codable, Sendable, Equatable {
    public var id: String
    public var name: String
    public var unit: String
    public var files: [String]
    public var secrets: [String]
    /// Declared names with no value yet (absent from an older stormo).
    public var missing: [String]?
}

/// A new agent as the wizard collects it, in plain choices; `spec` turns it into what the engine
/// takes. Empty advanced fields leave the engine's defaults (the instance's usual engine and model).
public struct NewAgentDraft: Equatable, Sendable {
    public enum Tone: String, CaseIterable, Identifiable, Sendable {
        case friendly, professional, concise
        public var id: Self { self }
        public var title: String { rawValue.capitalized }
        public var summary: String {
            switch self {
            case .friendly: "Warm and approachable, plain words."
            case .professional: "Polished and precise, like a trusted colleague."
            case .concise: "Short answers, straight to the point."
            }
        }
        var rule: String {
            switch self {
            case .friendly: "- Be warm and approachable; use plain words and a friendly tone."
            case .professional: "- Be polished and precise; write like a trusted senior colleague."
            case .concise: "- Keep answers short: lead with the answer, add detail only when asked."
            }
        }
    }

    /// A starting point for what the agent does; the role stays editable.
    public struct Starter: Identifiable, Sendable, Equatable {
        public let id: String
        public let title: String
        public let symbol: String
        public let role: String
    }

    public static let starters: [Starter] = [
        Starter(id: "support", title: "Customer support", symbol: "bubble.left.and.bubble.right",
                role: "Answers customer questions, solves common problems and hands the rest to the team."),
        Starter(id: "sales", title: "Sales assistant", symbol: "chart.line.uptrend.xyaxis",
                role: "Follows up on leads, prepares quotes and keeps deals moving."),
        Starter(id: "research", title: "Researcher", symbol: "magnifyingglass",
                role: "Researches topics, competitors and trends and reports what matters."),
        Starter(id: "operations", title: "Operations", symbol: "gearshape.2",
                role: "Watches routine work, flags what is late and keeps the team on schedule."),
        Starter(id: "content", title: "Content creator", symbol: "paintbrush",
                role: "Drafts posts, copy and ideas in the team's voice."),
        Starter(id: "custom", title: "Something else", symbol: "sparkles", role: ""),
    ]

    public struct ChannelDraft: Equatable, Sendable {
        public var allowedUsers = ""
        public var homeChannel = ""
        public var allowBots = ""
        public init() {}
    }

    public var name = ""
    public var role = ""
    public var starter: String?
    /// Empty: derived from the name.
    public var customID = ""

    public var unit = ""
    public var createUnit = false
    public var newUnitName = ""
    public var newUnitDescription = ""

    public var tone = Tone.friendly
    public var persona = ""
    /// nil until the person edits SOUL.md; until then it follows name, role and tone.
    public var editedSoul: String?

    public var provider = ""
    public var model = ""
    public var useLocal = false
    public var localModel = ""
    public var localConnection = ""
    public var engineKind = ""
    public var engineVersion = ""
    public var imageTag = ""

    public var channels: [String: ChannelDraft] = [:]

    public init() {}

    /// Starts from the instance's defaults.
    public init(options: NewAgentOptions) {
        let d = options.defaults
        unit = d.unit ?? ""
        createUnit = unit.isEmpty
        provider = d.model.provider ?? ""
        model = d.model.name ?? ""
        localModel = d.model.localName ?? ""
        localConnection = d.model.localConnection ?? ""
        useLocal = !localModel.isEmpty
        engineKind = d.engine.kind ?? options.options.engines.first ?? ""
        engineVersion = d.engine.version ?? ""
        imageTag = d.engine.imageTag ?? ""
    }

    public var id: String {
        customID.isEmpty ? InstanceLocation.slug(from: name) : customID
    }

    public var newUnitID: String { InstanceLocation.slug(from: newUnitName) }

    public var soul: String { editedSoul ?? starterSoul }

    /// Matches the engine's starter (scaffold.StarterSoul) plus the chosen tone.
    public var starterSoul: String {
        let n = name.trimmingCharacters(in: .whitespaces)
        let r = role.trimmingCharacters(in: .whitespacesAndNewlines)
        return """
            # \(n.isEmpty ? "Agent" : n)

            \(r.isEmpty ? "Describe what \(n.isEmpty ? "the agent" : n) does here." : r)

            ## How you work

            \(tone.rule)
            - Ask when a request is ambiguous instead of guessing.
            - Say what you did and what you could not do.
            - Never share credentials, and never act outside what you were asked to do.

            """
    }

    // MARK: Problems, per wizard step (nil: fine to go on)

    public func nameProblem(taken: [String]) -> String? {
        if name.trimmingCharacters(in: .whitespaces).isEmpty { return "Give your agent a name." }
        if id.isEmpty || !InstanceLocation.isValidSlug(id) {
            return "The ID must start with a letter and use lowercase letters, digits and hyphens."
        }
        if taken.contains(id) { return "An agent called “\(id)” already exists. Choose another name or ID." }
        return nil
    }

    public func unitProblem(units: [String]) -> String? {
        if createUnit {
            if newUnitName.trimmingCharacters(in: .whitespaces).isEmpty { return "Name the new team." }
            if newUnitID.isEmpty || newUnitID == "group" { return "Choose another team name." }
            if units.contains(newUnitID) { return "A team called “\(newUnitID)” already exists; pick it instead." }
            return nil
        }
        return unit.isEmpty || unit == "group" ? "Pick a team for the agent." : nil
    }

    public var brainProblem: String? {
        if engineKind.isEmpty { return "Choose an engine (Advanced)." }
        if engineVersion.trimmingCharacters(in: .whitespaces).isEmpty {
            return "No other agent to copy the engine version from: set it under Advanced."
        }
        if model.trimmingCharacters(in: .whitespaces).isEmpty { return "Choose a model." }
        if useLocal && localModel.trimmingCharacters(in: .whitespaces).isEmpty { return "Choose the ChatGPT plan model." }
        return nil
    }

    // MARK: Spec

    public var spec: NewAgentSpec {
        func trimmed(_ s: String) -> String? {
            let t = s.trimmingCharacters(in: .whitespacesAndNewlines)
            return t.isEmpty ? nil : t
        }
        let chans = channels.keys.sorted().map { kind -> NewAgentSpec.Channel in
            let c = channels[kind] ?? ChannelDraft()
            let users = c.allowedUsers.split(whereSeparator: { $0 == "," || $0.isWhitespace }).map(String.init)
            return NewAgentSpec.Channel(kind: kind, allowedUsers: users.isEmpty ? nil : users,
                                        homeChannel: trimmed(c.homeChannel), allowBots: trimmed(c.allowBots))
        }
        return NewAgentSpec(
            id: id,
            name: name.trimmingCharacters(in: .whitespaces),
            role: trimmed(role),
            unit: createUnit ? nil : unit,
            newUnit: createUnit ? .init(id: newUnitID, name: newUnitName.trimmingCharacters(in: .whitespaces),
                                        description: newUnitDescription.trimmingCharacters(in: .whitespacesAndNewlines)) : nil,
            persona: trimmed(persona),
            engine: .init(kind: trimmed(engineKind), version: trimmed(engineVersion), imageTag: trimmed(imageTag)),
            model: .init(name: trimmed(model), provider: trimmed(provider),
                         localName: useLocal ? trimmed(localModel) : nil,
                         localConnection: useLocal ? trimmed(localConnection) : nil),
            channels: chans,
            soul: soul)
    }

    /// Secret names the agent will need, before creating it (channels and the model's connection).
    public func neededSecrets(_ options: AgentOptions) -> [String] {
        var out = Set<String>()
        for kind in channels.keys {
            out.formUnion(options.channels.first { $0.kind == kind }?.secrets ?? [])
        }
        if let key = options.connections?.first(where: { $0.name == provider })?.key { out.insert(key) }
        return out.sorted()
    }
}
