import Foundation

/// A start refused because the agent's secret values are not set on this Mac, read from the CLI's
/// error ("<id>: missing secrets A, B in <path> (…)" or "<id>: missing secrets for a local run: A, B (…)").
public struct MissingSecrets: Sendable, Equatable {
    public var agent: String
    public var names: [String]

    public init(agent: String, names: [String]) {
        self.agent = agent
        self.names = names
    }

    public init?(error message: String) {
        let pattern = #"^(\S+): missing secrets (?:for a local run: )?(.+?)(?: in .+)? \(stormo secrets"#
        guard let re = try? NSRegularExpression(pattern: pattern, options: [.dotMatchesLineSeparators]),
              let m = re.firstMatch(in: message, range: NSRange(message.startIndex..., in: message)),
              let id = Range(m.range(at: 1), in: message), let list = Range(m.range(at: 2), in: message)
        else { return nil }
        agent = String(message[id])
        names = message[list].split(separator: ",").map { $0.trimmingCharacters(in: .whitespacesAndNewlines) }.filter { !$0.isEmpty }
        if names.isEmpty { return nil }
    }
}
