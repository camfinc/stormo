import Foundation
import Observation

/// What an agent's Slack bot sees (`GET /api/agents/<id>/slack`): people and channels by name, so
/// allowed users, a home channel or a delivery target are picked instead of typed as ids.
public struct SlackDirectory: Decodable, Sendable, Equatable {
    public struct Team: Decodable, Sendable, Equatable {
        public let id: String
        public let name: String
    }

    public struct Person: Decodable, Sendable, Equatable, Identifiable {
        public let id: String
        public let name: String
        public var realName: String?
        public var handle: String?
        public var bot: Bool?
        public var guest: Bool?
        public var owner: Bool?
        public var tz: String?

        public var isBot: Bool { bot ?? false }
        public var isGuest: Bool { guest ?? false }

        /// "Ana (Ana Alves)" when the display name hides the real one; guests and bots say so.
        public var label: String {
            var s = name
            if let real = realName, !real.isEmpty, real != name { s += " (\(real))" }
            if isGuest { s += " · guest" }
            return s
        }
    }

    public struct Channel: Decodable, Sendable, Equatable, Identifiable {
        public let id: String
        public let name: String
        public var `private`: Bool?
        public let member: Bool

        public var isPrivate: Bool { `private` ?? false }
    }

    public struct DM: Decodable, Sendable, Equatable {
        public let id: String
        public let user: String
    }

    public let team: Team
    public let bot: Person
    public let people: [Person]
    public let channels: [Channel]
    public let dms: [DM]
    public var fetchedAt: String?
    public var partial: [String]?

    public var humans: [Person] { people.filter { !$0.isBot } }
    public var bots: [Person] { people.filter { $0.isBot && $0.id != bot.id } }

    public func person(_ id: String) -> Person? { people.first { $0.id == id } }

    /// The bot's DM with a person, when it has one.
    public func dm(with user: String) -> DM? { dms.first { $0.user == user } }

    /// A readable name for an id from agent.yaml: a person, a bot, "#channel" or "DM with …";
    /// nil when the id is not in the directory.
    public func label(for id: String) -> String? {
        if let p = person(id) { return p.isBot ? "\(p.name) · bot" : p.label }
        if let c = channels.first(where: { $0.id == id }) { return "#\(c.name)" }
        if let d = dms.first(where: { $0.id == id }) { return "DM with \(person(d.user)?.name ?? d.user)" }
        return nil
    }

    /// A delivery target (`slack`, `slack:<id>`, `local`, a comma-separated list) in words.
    public func describe(target: String) -> String? {
        let parts = target.split(separator: ",").map { $0.trimmingCharacters(in: .whitespaces) }.filter { !$0.isEmpty }
        guard !parts.isEmpty else { return nil }
        let words = parts.map { part -> String in
            switch part {
            case "slack", "home": return "the home channel"
            case "local": return "nowhere (recorded only)"
            case "origin": return "where the job was made"
            default:
                let fields = part.split(separator: ":").map(String.init)
                if fields.first == "slack", fields.count >= 2 {
                    return (label(for: fields[1]) ?? fields[1]) + (fields.count > 2 ? " (a thread)" : "")
                }
                return part
            }
        }
        return words.joined(separator: ", ")
    }
}

extension CoreClient {
    /// The agent's Slack directory. `notFound` when the agent has no Slack bot token (or the core
    /// predates the route); `refused(502, …)` when Slack refuses its token.
    public func slackDirectory(agent: String, refresh: Bool = false) async throws -> SlackDirectory {
        var url = base.appending(path: "api/agents/\(agent)/slack")
        if refresh { url = url.appending(queryItems: [URLQueryItem(name: "refresh", value: "1")]) }
        return try JSONDecoder().decode(SlackDirectory.self, from: try await sendAuthorized(URLRequest(url: url)))
    }

    /// The bot's DM channel with a person (opened when missing; nothing is posted).
    public func openSlackDM(agent: String, user: String) async throws -> String {
        var r = URLRequest(url: base.appending(path: "api/agents/\(agent)/slack/dm"))
        r.httpMethod = "POST"
        r.setValue("application/json", forHTTPHeaderField: "Content-Type")
        r.httpBody = try JSONEncoder().encode(["user": user])
        struct Body: Decodable { let channel: String }
        return try JSONDecoder().decode(Body.self, from: try await sendAuthorized(r)).channel
    }
}

/// Each agent's Slack directory for this session, read through the core (never written to disk).
@MainActor @Observable
public final class SlackDirectoryStore {
    public var client: CoreClient
    public private(set) var directories: [String: SlackDirectory] = [:]
    /// Why an agent has none: no Slack token yet, Slack refused it, the core is down.
    public private(set) var problems: [String: String] = [:]
    public private(set) var loading: Set<String> = []

    public init(client: CoreClient) { self.client = client }

    public func directory(_ agent: String) -> SlackDirectory? { directories[agent] }

    /// Loads once per session unless `refresh`.
    public func load(_ agent: String, refresh: Bool = false) async {
        if !refresh, directories[agent] != nil || loading.contains(agent) { return }
        loading.insert(agent)
        defer { loading.remove(agent) }
        do {
            directories[agent] = try await client.with(timeout: 60).slackDirectory(agent: agent, refresh: refresh)
            problems[agent] = nil
        } catch {
            directories[agent] = nil
            problems[agent] = Self.describe(error)
        }
    }

    /// The bot's DM with a person, from the directory or opened through the core.
    public func dm(agent: String, user: String) async throws -> String {
        if let d = directories[agent]?.dm(with: user) { return d.id }
        let id = try await client.with(timeout: 20).openSlackDM(agent: agent, user: user)
        await load(agent, refresh: true)
        return id
    }

    public func clear() {
        directories = [:]
        problems = [:]
    }

    nonisolated static func describe(_ error: Error) -> String {
        switch error {
        case CoreClient.Failure.notFound: return "No Slack directory: the agent has no Slack bot token yet (or the core predates it)."
        case CoreClient.Failure.unreachable: return "The core is not running."
        case CoreClient.Failure.refused(let code, let message): return code == 502 ? "Slack refused the bot token: \(message)" : message
        default: return error.localizedDescription
        }
    }
}
