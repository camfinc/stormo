import Foundation
import Observation

/// An agent's voice and its voice connection, through `stormo` (docs/api.md): `voice show|try|pick`
/// and `connections voices|models|check`. agent.yaml's `voice:` itself is edited as a form with a
/// `ConfigEditor`; `try` and `pick` read the saved file. Nothing here is kept on disk.
@MainActor @Observable
public final class VoiceStore {
    public typealias Problem = ConfigEditor.Problem

    /// What is running, one at a time per store.
    public enum Action: Equatable, Sendable { case showing, trying, picking, checking }

    /// The agent's voice as saved, its pending pick and whether its key has a value.
    public private(set) var shown: VoiceShow?
    /// The last sample spoken (`voice try`).
    public private(set) var sample: VoiceSample?
    /// The agent's answer when it picked its voice (`voice pick`).
    public private(set) var pickReply: ChatReply?
    /// What a voice connection's key may do (`connections check`).
    public private(set) var keyCheck: VoiceKeyCheck?
    public private(set) var action: Action?
    /// The last refused action (show, try, pick, check): the CLI's code and message, as they came.
    public private(set) var problem: Problem?

    /// The connection's speech models; nil until loaded.
    public private(set) var models: [ConnectionModel]?
    public private(set) var modelsProblem: Problem?

    /// The voices the picker shows for `voicesQuery`, free ones first; nil until loaded.
    public private(set) var voices: [ProviderVoice]?
    public private(set) var voicesQuery: VoiceQuery?
    public private(set) var voicesProblem: Problem?
    public private(set) var loadingVoices = false

    public init() {}

    // MARK: The agent's voice

    public func show(_ agent: String, with cli: StormoCLI) async {
        await perform(.showing) {
            self.shown = try await cli.run(["voice", "show", agent], as: VoiceShow.self)
        }
    }

    /// Speaks a sample in the agent's saved voice (else its pending pick); costs the provider's characters.
    @discardableResult
    public func trySample(_ agent: String, text: String = "", with cli: StormoCLI) async -> VoiceSample? {
        var command = ["voice", "try", agent]
        let t = text.trimmingCharacters(in: .whitespacesAndNewlines)
        if !t.isEmpty { command += ["--text", t] }
        sample = nil
        await perform(.trying) {
            self.sample = try await cli.run(command, as: VoiceSample.self)
        }
        return sample
    }

    /// Asks the running agent to pick its own voice (a conversation turn: it can take minutes;
    /// cancelling the task ends it), then reads its voice again for the pick.
    public func pick(_ agent: String, with cli: StormoCLI) async {
        pickReply = nil
        await perform(.picking) {
            self.pickReply = try await cli.run(["voice", "pick", agent], as: ChatReply.self)
        }
        if pickReply != nil { await show(agent, with: cli) }
    }

    /// Checks what the connection's key may do, without spending.
    public func check(_ connection: String, agent: String? = nil, with cli: StormoCLI) async {
        var command = ["connections", "check", connection]
        if let agent, !agent.isEmpty { command += ["--agent", agent] }
        keyCheck = nil
        await perform(.checking) {
            self.keyCheck = try await cli.run(command, as: VoiceKeyCheck.self)
        }
    }

    // MARK: The connection's catalog

    public func loadModels(_ connection: String, agent: String? = nil, with cli: StormoCLI) async {
        var command = ["connections", "models", connection]
        if let agent, !agent.isEmpty { command += ["--agent", agent] }
        do {
            models = try await cli.run(command, as: [ConnectionModel].self)
            modelsProblem = nil
        } catch {
            if Task.isCancelled { return }
            modelsProblem = ConfigEditor.problem(error)
        }
    }

    /// Lists the voices for query; a later query wins over an earlier one still running.
    public func loadVoices(_ query: VoiceQuery, connection: String, agent: String?, with cli: StormoCLI) async {
        voicesQuery = query
        loadingVoices = true
        defer { if voicesQuery == query { loadingVoices = false } }
        do {
            let list = try await cli.run(query.command(connection: connection, agent: agent), as: [ProviderVoice].self)
            guard voicesQuery == query else { return }
            voices = Self.sorted(list, for: agent)
            voicesProblem = nil
        } catch {
            guard voicesQuery == query, !Task.isCancelled else { return }
            voices = []
            voicesProblem = ConfigEditor.problem(error)
        }
    }

    /// Voices no other agent speaks with first, then the ones in use, each in the provider's order.
    public nonisolated static func sorted(_ voices: [ProviderVoice], for agent: String?) -> [ProviderVoice] {
        voices.filter { $0.users(besides: agent).isEmpty } + voices.filter { !$0.users(besides: agent).isEmpty }
    }

    // MARK: State

    /// Clears the last problem (it was shown, or a new action is starting).
    public func problemReset() { problem = nil }

    private func perform(_ a: Action, _ body: () async throws -> Void) async {
        action = a
        problem = nil
        defer { if action == a { action = nil } }
        do {
            try await body()
        } catch {
            // A cancelled pick ends its process: nothing to report.
            if Task.isCancelled { return }
            problem = ConfigEditor.problem(error)
        }
    }
}
