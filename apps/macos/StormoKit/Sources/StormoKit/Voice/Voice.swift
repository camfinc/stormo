import Foundation

// An agent's voice and its voice connection's catalog (docs/api.md: `voice show|try|pick`,
// `connections voices|check|models`). Voice ids, names and preview links are the provider's; the
// connection's key stays in the CLI.

/// agent.yaml `voice:` as `voice show` reads it (docs/instances.md, Voice).
public struct AgentVoice: Codable, Sendable, Equatable {
    public struct Settings: Codable, Sendable, Equatable {
        /// 0 to 1; nil: the voice's own.
        public var stability: Double?
        public var similarity: Double?
    }
    /// A voice connection (stormo.yaml connections:).
    public var provider: String
    /// The provider's voice id; nil: none chosen yet (the agent picks one itself).
    public var id: String?
    public var name: String?
    /// The provider's speech model; nil: the runtime's default.
    public var model: String?
    public var settings: Settings?
    /// auto (the agent decides when a reply is a voice note) | never
    public var speak: String
    /// Transcribes the voice notes people send.
    public var listen: Bool
    /// ISO 639 code for listening; nil: detected.
    public var language: String?
    /// The longest one spoken note may be (10 to 300).
    public var maxSeconds: Int?

    enum CodingKeys: String, CodingKey {
        case provider, id, name, model, settings, speak, listen, language
        case maxSeconds = "max_seconds"
    }
}

/// `voice show <agent>`.
public struct VoiceShow: Codable, Sendable, Equatable {
    /// A voice the agent picked for itself, in use and waiting for review.
    public struct Proposed: Codable, Sendable, Equatable {
        public var id: String
        public var name: String?
    }
    public var agent: String
    /// nil: the agent has no voice.
    public var voice: AgentVoice?
    public var proposed: Proposed?
    /// The voice connection's key has a value for this agent.
    public var keySet: Bool
}

/// `voice try <agent>`: a sample in the agent's voice, saved for playing.
public struct VoiceSample: Codable, Sendable, Equatable {
    public var agent: String
    /// The voice id it was spoken in.
    public var voice: String
    public var text: String
    /// An MP3 under the instance's .swarm/voice/.
    public var path: String
    /// What the provider billed.
    public var chars: Int
}

/// One voice of `connections voices <name> [--library]`.
public struct ProviderVoice: Codable, Sendable, Equatable, Identifiable {
    public var id: String
    public var name: String
    /// premade, cloned, generated, professional…
    public var category: String?
    public var gender: String?
    public var age: String?
    public var accent: String?
    public var descriptive: String?
    public var useCase: String?
    public var language: String?
    public var description: String?
    /// The provider's own sample, free to play.
    public var previewUrl: String?
    /// In the account's voices (a library voice may be too).
    public var inAccount: Bool
    /// A library voice's sharer.
    public var owner: String?
    /// Agents whose voice.id it is.
    public var usedBy: [String]

    enum CodingKeys: String, CodingKey {
        case id, name, category, gender, age, accent, descriptive, useCase, language, description, previewUrl, inAccount, owner, usedBy
    }

    public init(id: String, name: String, gender: String? = nil, age: String? = nil, accent: String? = nil, language: String? = nil,
                description: String? = nil, previewUrl: String? = nil, inAccount: Bool = true, usedBy: [String] = []) {
        self.id = id; self.name = name; self.gender = gender; self.age = age; self.accent = accent; self.language = language
        self.description = description; self.previewUrl = previewUrl; self.inAccount = inAccount; self.usedBy = usedBy
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        name = try c.decodeIfPresent(String.self, forKey: .name) ?? id
        category = try c.decodeIfPresent(String.self, forKey: .category)
        gender = try c.decodeIfPresent(String.self, forKey: .gender)
        age = try c.decodeIfPresent(String.self, forKey: .age)
        accent = try c.decodeIfPresent(String.self, forKey: .accent)
        descriptive = try c.decodeIfPresent(String.self, forKey: .descriptive)
        useCase = try c.decodeIfPresent(String.self, forKey: .useCase)
        language = try c.decodeIfPresent(String.self, forKey: .language)
        description = try c.decodeIfPresent(String.self, forKey: .description)
        previewUrl = try c.decodeIfPresent(String.self, forKey: .previewUrl)
        inAccount = try c.decodeIfPresent(Bool.self, forKey: .inAccount) ?? false
        owner = try c.decodeIfPresent(String.self, forKey: .owner)
        usedBy = try c.decodeIfPresent([String].self, forKey: .usedBy) ?? []
    }

    /// The sample to play, when it has one.
    public var previewURL: URL? {
        guard let s = previewUrl, let u = URL(string: s), u.scheme == "https" else { return nil }
        return u
    }

    /// Its labels as words people read ("middle_aged" → "middle aged"), in a stable order.
    public var labels: [String] {
        [gender, age, accent, language.map(Self.languageName), descriptive, useCase]
            .compactMap { $0?.replacingOccurrences(of: "_", with: " ").replacingOccurrences(of: "-", with: " ") }
            .filter { !$0.isEmpty }
    }

    /// The agents other than `agent` that speak with it.
    public func users(besides agent: String?) -> [String] { usedBy.filter { $0 != agent } }

    /// A language code's name in English ("es" → "Spanish"); the code when unknown.
    public nonisolated static func languageName(_ code: String) -> String {
        Locale(identifier: "en").localizedString(forLanguageCode: code) ?? code
    }
}

/// `connections check <name>`: what a voice connection's key may do.
public struct VoiceKeyCheck: Codable, Sendable, Equatable {
    public struct Permission: Codable, Sendable, Equatable, Identifiable {
        /// speech_to_text, text_to_speech, voices_read, voices_write, models_read, user_read
        public var name: String
        /// What Stormo uses it for.
        public var purpose: String
        public var allowed: Bool
        public var id: String { name }

        enum CodingKeys: String, CodingKey {
            case name, allowed
            case purpose = "for"
        }
    }
    public struct Characters: Codable, Sendable, Equatable {
        public var used: Int
        public var limit: Int
    }
    public var permissions: [Permission]
    /// This period's characters, when the key may read the account.
    public var characters: Characters?
}

/// What `connections voices` is asked: the account's voices or the Voice Library's, narrowed.
public struct VoiceQuery: Equatable, Hashable, Sendable {
    public var library = false
    public var search = ""
    public var language = ""
    public var gender = ""
    public var age = ""
    public var accent = ""
    /// 1 to 100.
    public var limit = 30

    public init(library: Bool = false, search: String = "", language: String = "", gender: String = "", age: String = "",
                accent: String = "", limit: Int = 30) {
        self.library = library; self.search = search; self.language = language; self.gender = gender; self.age = age
        self.accent = accent; self.limit = limit
    }

    /// The command for `connection`, asked with `agent`'s key when there is one (else the shared key).
    public func command(connection: String, agent: String?) -> [String] {
        var c = ["connections", "voices", connection]
        if let agent, !agent.isEmpty { c += ["--agent", agent] }
        if library { c.append("--library") }
        for (flag, value) in [("--search", search), ("--language", language), ("--gender", gender), ("--age", age), ("--accent", accent)] {
            let v = value.trimmingCharacters(in: .whitespaces)
            if !v.isEmpty { c += [flag, v] }
        }
        if limit != 30 { c += ["--limit", String(max(1, min(100, limit)))] }
        return c
    }
}

/// `voice:` edits on an agent.yaml document, as the editor's Voice page makes them.
public enum VoiceDoc {
    /// The speech model the runtime uses when `voice.model` is empty.
    public static let defaultModel = "eleven_v4"
    /// voice.max_seconds' bounds and default.
    public static let seconds = (min: 10, max: 300, default: 120)

    /// Gives the agent a voice that picks itself (`speak: auto`, listening) through `provider`, and
    /// declares the connection's key under secrets: unless it already is (as the model's provider does).
    public static func give(_ doc: inout JSONValue, provider: String, key: String?) {
        doc.set(["voice"], .object(["provider": .string(provider), "speak": .string("auto"), "listen": .bool(true)]))
        if let key { declare(&doc, key) }
    }

    /// Declares a secret name under secrets: unless secrets: or optional_secrets: has it.
    public static func declare(_ doc: inout JSONValue, _ key: String) {
        let secrets = doc[["secrets"]]?.array ?? []
        let optional = doc[["optional_secrets"]]?.array ?? []
        guard !secrets.contains(.string(key)), !optional.contains(where: { $0[["name"]]?.string == key }) else { return }
        doc.set(["secrets"], .array(secrets + [.string(key)]))
    }

    /// Sets the provider's voice: its id and the name kept in step with it.
    public static func choose(_ doc: inout JSONValue, id: String, name: String) {
        doc.set(["voice", "id"], .string(id))
        let line = name.split(whereSeparator: \.isNewline).joined(separator: " ").trimmingCharacters(in: .whitespaces)
        doc.set(["voice", "name"], line.isEmpty ? nil : .string(String(line.prefix(80))))
    }

    /// Clears the chosen voice, so the agent picks one again.
    public static func clear(_ doc: inout JSONValue) {
        doc.set(["voice", "id"], nil)
        doc.set(["voice", "name"], nil)
    }

    /// One line on the agent's voice for its settings.
    public static func summary(_ doc: JSONValue?) -> String {
        guard let v = doc?[["voice"]], v.object != nil else { return "No voice" }
        if let id = v[["id"]]?.string, !id.isEmpty { return v[["name"]]?.string ?? id }
        return "Picks its own"
    }
}
