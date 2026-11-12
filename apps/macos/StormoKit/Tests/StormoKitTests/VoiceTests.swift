import Foundation
import Testing

@testable import StormoKit

// Shapes as this checkout's bin/stormo prints them (ids, links and owners made up).
private let checkJSON = """
    {"permissions":[{"name":"speech_to_text","for":"transcribing voice notes","allowed":true},
    {"name":"text_to_speech","for":"speaking","allowed":true},
    {"name":"voices_write","for":"adding a Voice Library voice to the account","allowed":false}],
    "characters":{"used":35128,"limit":600000}}
    """

private let voicesJSON = """
    [{"id":"AcmeVoice1","name":"Ada – Warm","category":"professional","gender":"female","age":"middle_aged","accent":"peninsular",
    "descriptive":"confident","useCase":"social_media","language":"es","description":"A warm voice.",
    "previewUrl":"https://example.com/voices/AcmeVoice1.mp3","inAccount":true,"usedBy":["atlas","nova"]},
    {"id":"AcmeVoice2","name":"Bruno","gender":"male","age":"old","language":"es","previewUrl":"http://example.com/insecure.mp3",
    "inAccount":false,"owner":"0f0f","usedBy":[]},
    {"id":"AcmeVoice3","name":"Cleo","inAccount":true,"usedBy":["scout"]}]
    """

private let modelsJSON = """
    [{"id":"eleven_flash_v2","name":"Eleven Flash v2","languages":["en"],"maxChars":30000},
    {"id":"eleven_v4","name":"Eleven v4","languages":["en","es"],"maxChars":10000}]
    """

private func decode<T: Decodable>(_ type: T.Type, _ json: String) throws -> T {
    try JSONDecoder().decode(type, from: Data(json.utf8))
}

@Suite struct VoiceDecoding {
    @Test func keyCheck() throws {
        let c = try decode(VoiceKeyCheck.self, checkJSON)
        #expect(c.permissions.map(\.name) == ["speech_to_text", "text_to_speech", "voices_write"])
        #expect(c.permissions[0].purpose == "transcribing voice notes" && !c.permissions[2].allowed)
        #expect(c.characters == .init(used: 35128, limit: 600000))
        let bare = try decode(VoiceKeyCheck.self, #"{"permissions":[]}"#)
        #expect(bare.characters == nil)
    }

    @Test func voices() throws {
        let v = try decode([ProviderVoice].self, voicesJSON)
        #expect(v[0].labels == ["female", "middle aged", "peninsular", "Spanish", "confident", "social media"])
        #expect(v[0].previewURL?.host == "example.com" && v[0].users(besides: "atlas") == ["nova"])
        #expect(v[1].previewURL == nil && v[1].owner == "0f0f" && !v[1].inAccount)
        #expect(v[2].labels.isEmpty && v[2].description == nil)
        #expect(VoiceStore.sorted(v, for: "scout").map(\.id) == ["AcmeVoice2", "AcmeVoice3", "AcmeVoice1"])
        #expect(VoiceStore.sorted(v, for: nil).map(\.id) == ["AcmeVoice2", "AcmeVoice1", "AcmeVoice3"])
    }

    @Test func speechModels() throws {
        let m = try decode([ConnectionModel].self, modelsJSON)
        #expect(m[1].languages == ["en", "es"] && m[1].maxChars == 10000 && m[1].contextLength == nil)
    }

    @Test func show() throws {
        let none = try decode(VoiceShow.self, #"{"agent":"atlas","voice":null,"proposed":null,"keySet":false}"#)
        #expect(none.voice == nil && none.proposed == nil)
        let s = try decode(VoiceShow.self, """
            {"agent":"atlas","voice":{"provider":"elevenlabs","id":"AcmeVoice1","name":"Ada","model":"eleven_v4",
            "settings":{"stability":0.4},"speak":"auto","listen":false,"language":"es","max_seconds":90},
            "proposed":{"id":"AcmeVoice2"},"keySet":true}
            """)
        #expect(s.voice?.maxSeconds == 90 && s.voice?.settings?.stability == 0.4 && s.voice?.settings?.similarity == nil)
        #expect(s.voice?.listen == false && s.proposed?.name == nil && s.keySet)
    }

    @Test func chatAudio() throws {
        let plain = try decode(ChatReply.self, #"{"agent":"atlas","session":"s1","reply":"Hi"}"#)
        #expect(plain.audio == nil)
        let note = try decode(ChatReply.self, #"{"agent":"atlas","session":"s1","reply":"Hi","audio":{"path":"/tmp/acme/.swarm/voice/a.mp3","mime":"audio/mpeg","withText":false}}"#)
        #expect(note.audio == ChatAudio(path: "/tmp/acme/.swarm/voice/a.mp3", withText: false))
        #expect(note.audio?.url.isFileURL == true)
    }
}

@Suite struct VoiceQueries {
    @Test func arguments() {
        #expect(VoiceQuery().command(connection: "elevenlabs", agent: nil) == ["connections", "voices", "elevenlabs"])
        let q = VoiceQuery(library: true, search: " warm ", language: "es", gender: "female", accent: "", limit: 500)
        #expect(q.command(connection: "elevenlabs", agent: "atlas")
                == ["connections", "voices", "elevenlabs", "--agent", "atlas", "--library", "--search", "warm", "--language", "es",
                    "--gender", "female", "--limit", "100"])
    }
}

@Suite struct VoiceDocEdits {
    private let base: JSONValue = .object(["name": .string("Atlas"), "secrets": .array([.string("OPENROUTER_API_KEY")])])

    @Test func giveDeclaresTheKeyOnce() {
        var doc = base
        #expect(VoiceDoc.summary(doc) == "No voice")
        VoiceDoc.give(&doc, provider: "elevenlabs", key: "ELEVENLABS_API_KEY")
        #expect(doc[["voice"]] == .object(["provider": .string("elevenlabs"), "speak": .string("auto"), "listen": .bool(true)]))
        #expect(doc[["secrets"]] == .array([.string("OPENROUTER_API_KEY"), .string("ELEVENLABS_API_KEY")]))
        #expect(VoiceDoc.summary(doc) == "Picks its own")
        VoiceDoc.give(&doc, provider: "elevenlabs", key: "ELEVENLABS_API_KEY")
        #expect(doc[["secrets"]]?.array?.count == 2)

        var optional: JSONValue = .object(["optional_secrets": .array([.object(["name": .string("ELEVENLABS_API_KEY")])])])
        VoiceDoc.give(&optional, provider: "elevenlabs", key: "ELEVENLABS_API_KEY")
        #expect(optional[["secrets"]] == nil)
    }

    @Test func chooseAndClear() {
        var doc = base
        VoiceDoc.give(&doc, provider: "elevenlabs", key: nil)
        VoiceDoc.choose(&doc, id: "AcmeVoice1", name: "Ada\nWarm")
        #expect(doc[["voice", "id"]] == .string("AcmeVoice1") && doc[["voice", "name"]] == .string("Ada Warm"))
        #expect(VoiceDoc.summary(doc) == "Ada Warm")
        // Saved as a merge patch: only voice: changes.
        #expect(MergePatch.diff(from: base, to: doc)?.object?.keys.sorted() == ["voice"])
        VoiceDoc.clear(&doc)
        #expect(doc[["voice", "id"]] == nil && doc[["voice", "name"]] == nil && doc[["voice", "speak"]] == .string("auto"))
    }
}

@Suite struct NewAgentVoice {
    private func options(voice: Bool) throws -> NewAgentOptions {
        let conns = voice ? #","voiceConnections":[{"name":"elevenlabs","kind":"elevenlabs","label":"ElevenLabs (voice)","api":true,"key":"ELEVENLABS_API_KEY"}]"# : ""
        return try decode(NewAgentOptions.self, """
            {"options":{"units":[{"id":"sales","name":"Acme Sales"}],"actions":[],"skills":[],"personas":[],"engines":["needletail"],
            "channels":[],"allowBots":[],"secrets":[],"connections":[{"name":"openrouter","kind":"openrouter","label":"OpenRouter","api":true,"key":"OPENROUTER_API_KEY"}]\(conns)},
            "defaults":{"unit":"sales","engine":{"kind":"needletail","version":"0.5.2"},"model":{"name":"openai/gpt-6-luna","provider":"openrouter"}},
            "takenIds":[]}
            """)
    }

    private func voiceJSON(_ d: NewAgentDraft) throws -> [String: Any]? {
        let json = try JSONSerialization.jsonObject(with: JSONEncoder().encode(d.spec)) as? [String: Any]
        return json?["voice"] as? [String: Any]
    }

    @Test func picksByDefaultWhenTheInstanceHasAVoiceConnection() throws {
        let o = try options(voice: true)
        var d = NewAgentDraft(options: o)
        d.name = "Vox"
        #expect(d.voiceChoice == .pick && d.voiceProblem == nil)
        #expect(try voiceJSON(d)?.keys.sorted() == ["provider"])
        d.voiceLanguage = "es"
        #expect(try voiceJSON(d)?["language"] as? String == "es")
        #expect(d.neededSecrets(o.options) == ["ELEVENLABS_API_KEY", "OPENROUTER_API_KEY"])

        d.voiceChoice = .choose
        #expect(d.voiceProblem?.contains("Choose a voice") == true)
        d.voiceID = "AcmeVoice1"
        d.voiceName = "Ada"
        d.voiceModel = "eleven_v4"
        #expect(d.voiceProblem == nil)
        #expect(try voiceJSON(d)?["id"] as? String == "AcmeVoice1" && voiceJSON(d)?["model"] as? String == "eleven_v4")

        d.voiceChoice = .none
        #expect(try voiceJSON(d) == nil && d.neededSecrets(o.options) == ["OPENROUTER_API_KEY"])
        d.voiceChoice = .pick
        d.voiceLanguage = "Spanish"
        #expect(d.voiceProblem != nil)
    }

    @Test func noVoiceWithoutAVoiceConnection() throws {
        let d = NewAgentDraft(options: try options(voice: false))
        #expect(d.voiceChoice == .none && d.spec.voice == nil)
    }
}

/// A fake stormo that logs each command (after the global flags) and answers from canned results.
private func voiceStormo(_ s: Scratch) throws -> (cli: StormoCLI, log: URL) {
    let log = s.url.appending(path: "commands.log")
    let exe = try s.script("stormo", """
        shift 5
        echo "$*" >> '\(log.path)'
        case "$1 $2" in
          "voice show") echo '{"event":"result","data":{"agent":"atlas","voice":{"provider":"elevenlabs","speak":"auto","listen":true,"max_seconds":120},"proposed":{"id":"AcmeVoice2","name":"Bruno"},"keySet":true}}' ;;
          "voice try") echo '{"event":"result","data":{"agent":"atlas","voice":"AcmeVoice2","text":"Hi","path":"/tmp/acme/.swarm/voice/atlas-1.mp3","chars":2}}' ;;
          "voice pick") echo '{"event":"result","data":{"agent":"atlas","session":"s9","reply":"Bruno: calm and clear."}}' ;;
          "connections check") echo '{"event":"error","code":"unavailable","msg":"elevenlabs: the key lacks the voices_read permission"}'; exit 1 ;;
          "connections models") echo '{"event":"result","data":\(modelsJSON.replacingOccurrences(of: "\n", with: ""))}' ;;
          "connections voices") echo '{"event":"result","data":\(voicesJSON.replacingOccurrences(of: "\n", with: ""))}' ;;
          *) echo '{"event":"error","code":"usage","msg":"unknown command"}'; exit 2 ;;
        esac
        """)
    return (StormoCLI(executable: exe, environment: [:], instance: s.url), log)
}

@Suite struct VoiceStoreCommands {
    @Test @MainActor func runsTheCommandsAndKeepsTheirResults() async throws {
        let s = try Scratch()
        let (cli, log) = try voiceStormo(s)
        let store = VoiceStore()

        await store.show("atlas", with: cli)
        #expect(store.shown?.proposed?.name == "Bruno" && store.shown?.voice?.maxSeconds == 120 && store.problem == nil)

        let sample = await store.trySample("atlas", text: "  Hola  ", with: cli)
        #expect(sample?.path.hasSuffix(".mp3") == true && store.action == nil)

        store.startPick("atlas", with: cli)
        #expect(store.action == .picking)
        for _ in 0..<300 where store.action != nil { try await Task.sleep(for: .milliseconds(10)) }
        #expect(store.pickReply?.reply == "Bruno: calm and clear.")

        await store.loadModels("elevenlabs", agent: "atlas", with: cli)
        #expect(store.models?.map(\.id) == ["eleven_flash_v2", "eleven_v4"])

        let q = VoiceQuery(library: true, language: "es")
        await store.loadVoices(q, connection: "elevenlabs", agent: "atlas", with: cli)
        #expect(store.voices?.first?.id == "AcmeVoice2" && store.voicesQuery == q && !store.loadingVoices)

        await store.check("elevenlabs", with: cli)
        #expect(store.keyCheck == nil && store.problem?.code == "unavailable")
        #expect(store.problem?.message == "elevenlabs: the key lacks the voices_read permission")

        let commands = try String(contentsOf: log, encoding: .utf8).split(separator: "\n").map(String.init)
        #expect(commands == [
            "voice show atlas",
            "voice try atlas --text Hola",
            "voice pick atlas",
            "voice show atlas",
            "connections models elevenlabs --agent atlas",
            "connections voices elevenlabs --agent atlas --library --language es",
            "connections check elevenlabs",
        ])
    }

    @Test @MainActor func aFailedVoiceListShowsItsMessage() async throws {
        let s = try Scratch()
        let exe = try s.script("stormo", """
            echo '{"event":"error","code":"unavailable","msg":"ELEVENLABS_API_KEY has no value yet: set it to use elevenlabs"}'
            exit 1
            """)
        let store = VoiceStore()
        await store.loadVoices(VoiceQuery(), connection: "elevenlabs", agent: nil, with: StormoCLI(executable: exe, environment: [:], instance: s.url))
        #expect(store.voices == [] && store.voicesProblem?.message.contains("has no value yet") == true)
    }
}

/// The voice commands through this checkout's bin/stormo on a copy of the example (no key: nothing
/// reaches the provider).
@Suite struct VoiceRealBinary {
    @Test(.enabled(if: FileManager.default.isExecutableFile(atPath: RealBinary.binary.path)))
    @MainActor func aVoiceThroughTheForm() async throws {
        let s = try Scratch()
        let root = s.url.appending(path: "acme")
        try FileManager.default.copyItem(at: RealBinary.engine.appending(path: "examples/minimal"), to: root)
        let env = ShellEnvironment.compose(shell: nil, process: ProcessInfo.processInfo.environment)
        let cli = StormoCLI(executable: RealBinary.binary, environment: env, instance: root)

        let rows = try await cli.run(["connections"], as: [ConnectionRow].self)
        #expect(rows.filter(\.servesVoice).map(\.name) == ["elevenlabs"])
        #expect(rows.first { $0.name == "openrouter" }?.servesVoice == false)

        let store = VoiceStore()
        await store.show("atlas", with: cli)
        #expect(store.shown?.voice == nil)

        let editor = ConfigEditor(path: "agents/atlas/agent.yaml")
        await editor.load(with: cli)
        let conn = try #require(editor.file?.options?.voiceConnections?.first)
        #expect(conn.name == "elevenlabs" && conn.key == "ELEVENLABS_API_KEY")
        #expect(editor.file?.options?.connections?.contains { $0.name == "elevenlabs" } == false)
        VoiceDoc.give(&editor.doc!, provider: conn.name, key: conn.key)
        editor.doc?.set(["voice", "language"], .string("es"))
        #expect(await editor.save(with: cli))

        await store.show("atlas", with: cli)
        #expect(store.shown?.voice?.maxSeconds == 120 && store.shown?.voice?.language == "es" && store.shown?.keySet == false)

        await store.trySample("atlas", with: cli)
        #expect(store.problem?.code == "unavailable")
        // Not running here (the example's atlas has no local keys either): refused, nothing picked.
        await store.pick("atlas", with: cli)
        #expect(store.problem != nil && store.pickReply == nil)

        VoiceDoc.choose(&editor.doc!, id: "AcmeVoice1", name: "Ada")
        #expect(await editor.save(with: cli))
        await store.show("atlas", with: cli)
        #expect(store.shown?.voice?.id == "AcmeVoice1" && store.shown?.voice?.name == "Ada")
        await store.pick("atlas", with: cli)
        #expect(store.problem?.code == "invalid")

        await store.check("elevenlabs", with: cli)
        #expect(store.problem?.code == "unavailable" && store.problem?.message.contains("ELEVENLABS_API_KEY") == true)
    }

    @Test(.enabled(if: FileManager.default.isExecutableFile(atPath: RealBinary.binary.path)))
    @MainActor func aNewAgentWithAVoice() async throws {
        let s = try Scratch()
        let root = s.url.appending(path: "acme")
        try FileManager.default.copyItem(at: RealBinary.engine.appending(path: "examples/minimal"), to: root)
        let env = ShellEnvironment.compose(shell: nil, process: ProcessInfo.processInfo.environment)
        let cli = StormoCLI(executable: RealBinary.binary, environment: env, instance: root)
        let options = try await cli.run(["new", "agent", "--options"], as: NewAgentOptions.self)

        var picks = NewAgentDraft(options: options)
        picks.name = "Vox"
        picks.voiceLanguage = "es"
        #expect(picks.voiceChoice == .pick)
        let made = try await cli.run(["new", "agent"], as: NewAgentResult.self, stdin: try JSONEncoder().encode(picks.spec))
        #expect(made.secrets.contains("ELEVENLABS_API_KEY") && made.missing?.contains("ELEVENLABS_API_KEY") == true)
        let store = VoiceStore()
        await store.show("vox", with: cli)
        #expect(store.shown?.voice?.id == nil && store.shown?.voice?.speak == "auto" && store.shown?.voice?.language == "es")

        var chosen = NewAgentDraft(options: options)
        chosen.name = "Echo"
        chosen.voiceChoice = .choose
        chosen.voiceID = "AcmeVoice1"
        chosen.voiceName = "Ada"
        chosen.voiceModel = "eleven_v4"
        _ = try await cli.run(["new", "agent"], as: NewAgentResult.self, stdin: try JSONEncoder().encode(chosen.spec))
        await store.show("echo", with: cli)
        #expect(store.shown?.voice?.id == "AcmeVoice1" && store.shown?.voice?.name == "Ada" && store.shown?.voice?.model == "eleven_v4")

        var silent = NewAgentDraft(options: options)
        silent.name = "Mute"
        silent.voiceChoice = .none
        let quiet = try await cli.run(["new", "agent"], as: NewAgentResult.self, stdin: try JSONEncoder().encode(silent.spec))
        #expect(!quiet.secrets.contains("ELEVENLABS_API_KEY"))
        await store.show("mute", with: cli)
        #expect(store.shown?.voice == nil)
    }
}
