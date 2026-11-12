import StormoKit
import SwiftUI

/// The agent editor's Voice page: agent.yaml's `voice:` as a form over the editor's document, saved
/// with the rest of the form (a merge patch), and what `stormo voice` does with the saved voice: a
/// sample to hear (`voice try`), the agent picking its own (`voice pick`), and the pick it made
/// waiting for review (`voice show`).
struct VoicePane: View {
    @Environment(AppModel.self) private var model
    @Bindable var editor: ConfigEditor
    let options: AgentOptions
    let agentID: String
    let name: String
    let running: Bool

    @State private var store = VoiceStore()
    /// The account's voices, to show the current one's labels and sample.
    @State private var account = VoiceStore()
    /// The voice chosen in this session (it may not be among the account's yet).
    @State private var chosen: ProviderVoice?
    @State private var choosing = false
    @State private var line = ""
    @State private var picking: Task<Void, Never>?

    typealias Path = [JSONValue.Key]

    private var connections: [AgentOptions.Connection] { options.voiceConnections ?? [] }
    private var provider: String { read(["provider"])?.string ?? connections.first?.name ?? "elevenlabs" }
    private var connection: AgentOptions.Connection? { connections.first { $0.name == provider } }
    private var providerLabel: String { connection?.label.replacingOccurrences(of: " (voice)", with: "") ?? provider }
    private var voiceID: String? { read(["id"])?.string.flatMap { $0.isEmpty ? nil : $0 } }
    /// The voice as saved: what `voice try` and `voice pick` use.
    private var saved: JSONValue? { editor.file?.doc?[["voice"]] }
    private var voiceDirty: Bool { !(saved ?? .null).same(as: editor.doc?[["voice"]] ?? .null) }

    var body: some View {
        if editor.doc?[["voice"]]?.object == nil {
            noVoice
        } else {
            Form {
                if let p = store.problem { problem(p) }
                if store.shown?.voice != nil, store.shown?.keySet == false { keyMissing }
                voiceSection
                trySection
                speech
                speaking
                remove
            }
            .formStyle(.grouped)
            .task(id: editor.file?.hash) { await show() }
            .task(id: provider) { await loadCatalog() }
            .sheet(isPresented: $choosing) {
                VoiceChooser(connection: provider, agentID: agentID, language: read(["language"])?.string ?? "", current: voiceID) { v in
                    chosen = v
                    if var doc = editor.doc {
                        VoiceDoc.choose(&doc, id: v.id, name: v.name)
                        editor.doc = doc
                    }
                }
            }
            .onDisappear { picking?.cancel() }
            #if DEBUG
            // STORMO_CHOOSE_VOICE opens the chooser (with STORMO_EDIT_PAGE=voice and STORMO_SNAPSHOT, a visual check).
            .task {
                if ProcessInfo.processInfo.environment["STORMO_CHOOSE_VOICE"] != nil {
                    try? await Task.sleep(for: .seconds(1))
                    choosing = true
                }
            }
            #endif
        }
    }

    // MARK: Sections

    private var noVoice: some View {
        ContentUnavailableView {
            Label("\(name) has no voice", systemImage: "waveform.slash")
        } description: {
            Text(connections.isEmpty
                 ? "This instance has no voice connection, or this stormo predates voice. Update it to give agents a voice."
                 : "With a voice, \(name) can answer with a voice note when it decides to, and understands the voice notes people send. It picks a voice of its own unless you choose one.")
        } actions: {
            Button("Give \(name) a Voice") {
                guard let c = connections.first, var doc = editor.doc else { return }
                VoiceDoc.give(&doc, provider: c.name, key: c.key)
                editor.doc = doc
            }
            .buttonStyle(.borderedProminent)
            .disabled(connections.isEmpty)
        }
    }

    private func problem(_ p: VoiceStore.Problem) -> some View {
        Section {
            HStack(alignment: .firstTextBaseline) {
                Label(p.code == "not_running" ? "\(name) isn’t running here. It picks its voice while running: start it, then ask again." : p.message,
                      systemImage: "exclamationmark.triangle.fill")
                    .foregroundStyle(.orange).textSelection(.enabled)
                Spacer()
                if p.code == "not_running" {
                    Button("Start \(name)") { Task { await model.run("start", agents: [agentID]); store.problemReset() } }
                        .disabled(!model.core.state.isRunning)
                }
                Button("Dismiss", systemImage: "xmark") { store.problemReset() }
                    .labelStyle(.iconOnly).buttonStyle(.borderless)
            }
        }
    }

    private var keyMissing: some View {
        Section {
            Label("\(connection?.key ?? "The voice key") has no value for \(name). Set it under Providers › Voice, or in its Secret values.",
                  systemImage: "key").foregroundStyle(.orange)
        }
    }

    private var current: ProviderVoice? {
        guard let id = voiceID else { return nil }
        if let chosen, chosen.id == id { return chosen }
        return account.voices?.first { $0.id == id }
    }

    private var voiceSection: some View {
        Section {
            if let id = voiceID {
                HStack(alignment: .top, spacing: 10) {
                    PlayButton(url: current?.previewURL, label: "Play the voice's sample").padding(.top, 1)
                    VStack(alignment: .leading, spacing: 3) {
                        Text(read(["name"])?.string ?? current?.name ?? id).fontWeight(.medium)
                        if let labels = current?.labels, !labels.isEmpty {
                            Text(labels.joined(separator: " · ")).font(.caption).foregroundStyle(.secondary)
                        }
                        Text("id \(id)").font(.caption).monospaced().foregroundStyle(.tertiary).textSelection(.enabled)
                    }
                    Spacer()
                    Button("Choose…") { choosing = true }
                    Button("Clear") { clear() }
                        .help("Forget this voice: \(name) picks one again")
                }
            } else {
                HStack(alignment: .top, spacing: 10) {
                    Image(systemName: "sparkles").foregroundStyle(.secondary).frame(width: 20)
                    VStack(alignment: .leading, spacing: 3) {
                        Text("Picks its own").fontWeight(.medium)
                        Text("No voice chosen: \(name) picks one itself the first time it wants to speak, or now with Let \(name) Pick.")
                            .font(.caption).foregroundStyle(.secondary)
                    }
                    Spacer()
                    Button("Choose…") { choosing = true }
                }
                if let p = store.shown?.proposed, saved?[["id"]]?.string?.isEmpty ?? true { proposed(p) }
                pickRow
            }
        } header: {
            SectionTitle("Voice", note: "Who \(name) sounds like in its voice notes, from \(providerLabel).")
        }
    }

    /// The voice the agent picked for itself: in use already, waiting for review.
    private func proposed(_ p: VoiceShow.Proposed) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: 10) {
            Image(systemName: "hourglass").foregroundStyle(.orange).frame(width: 20)
            VStack(alignment: .leading, spacing: 2) {
                Text("\(name) picked \(p.name ?? p.id)").fontWeight(.medium)
                Text("It speaks with it already; the pick awaits review with its next nap (Review).")
                    .font(.caption).foregroundStyle(.secondary)
            }
            Spacer()
            Button("Try", systemImage: "play.fill") { Task { await trySample() } }
                .disabled(store.action != nil || model.cli == nil || editor.isDirty)
                .help("Hear a line in the picked voice (costs a few characters)")
        }
    }

    @ViewBuilder private var pickRow: some View {
        let canPick = saved?[["speak"]]?.string != "never" && (saved?[["id"]]?.string?.isEmpty ?? true) && saved != nil
        HStack(alignment: .firstTextBaseline) {
            if store.action == .picking {
                ProgressView().controlSize(.small)
                Text("\(name) is listening to voices and picking one…").foregroundStyle(.secondary)
                Spacer()
                Button("Stop") { picking?.cancel() }
            } else {
                Button("Let \(name) Pick") { pick() }
                    .disabled(!canPick || editor.isDirty || store.action != nil || model.cli == nil)
                Text(editor.isDirty ? "Save first: it picks with the saved settings."
                     : !canPick ? (saved?[["speak"]]?.string == "never" ? "It never speaks, so it has nothing to pick." : "")
                     : running ? "A conversation turn: it searches, listens and chooses. Its pick is in use at once."
                     : "\(name) picks while running: start it first.")
                    .font(.caption).foregroundStyle(.secondary)
                Spacer()
            }
        }
        if let reply = store.pickReply?.reply, store.action != .picking {
            Text("“\(reply)”").font(.callout).foregroundStyle(.secondary).textSelection(.enabled)
        }
    }

    private var trySection: some View {
        let hasVoice = (saved?[["id"]]?.string.map { !$0.isEmpty } ?? false) || store.shown?.proposed != nil
        return Section {
            TextField("Line", text: $line, prompt: Text("Empty: \(name) introduces itself"), axis: .vertical)
                .lineLimit(1...3)
                .onChange(of: line) { if line.count > 300 { line = String(line.prefix(300)) } }
            HStack(spacing: 10) {
                Button("Try a Line") { Task { await trySample() } }
                    .disabled(!hasVoice || editor.isDirty || store.action != nil || model.cli == nil)
                if store.action == .trying { ProgressView().controlSize(.small) }
                if let s = store.sample {
                    AudioClip(url: URL(filePath: s.path))
                    Text("\(s.chars) characters").font(.caption).foregroundStyle(.tertiary)
                }
                Spacer()
            }
            Text(editor.isDirty ? "Save first: a line is spoken with the saved voice and settings."
                 : !hasVoice ? "Choose a voice, or let \(name) pick one, to hear it."
                 : "Costs a few \(providerLabel) characters (a line of up to 300).")
                .font(.caption).foregroundStyle(.secondary)
        } header: {
            SectionTitle("Try a line", note: "Hear \(name) in its voice, with its model and settings.")
        }
    }

    private var speech: some View {
        Section {
            if let models = store.models {
                Picker("Model", selection: text(["model"])) {
                    Text("Default (\(models.first { $0.id == VoiceDoc.defaultModel }?.name ?? VoiceDoc.defaultModel))").tag("")
                    Divider()
                    ForEach(modelChoices(models)) { m in
                        Text(modelLabel(m)).tag(m.id)
                    }
                }
            } else {
                LabeledContent("Model") {
                    TextField("Model", text: text(["model"]), prompt: Text("default (\(VoiceDoc.defaultModel))"))
                        .labelsHidden().multilineTextAlignment(.trailing).monospaced()
                }
                if let p = store.modelsProblem {
                    Text("Couldn’t list the models: \(p.message)").font(.caption).foregroundStyle(.secondary)
                }
            }
            setting("Stability", ["settings", "stability"], note: "Lower is more expressive, higher more even")
            setting("Similarity", ["settings", "similarity"], note: "How closely it keeps to the original voice")
        } header: {
            SectionTitle("Speech", note: "Empty settings keep the voice's own.")
        }
    }

    private var speaking: some View {
        Section {
            Picker("Speaks", selection: Binding {
                read(["speak"])?.string ?? "auto"
            } set: {
                set(["speak"], .string($0))
            }) {
                Text("With a voice note when it decides to").tag("auto")
                Text("Never (it only listens)").tag("never")
            }
            Toggle("Listens: understands the voice notes people send", isOn: Binding {
                read(["listen"])?.bool ?? true
            } set: { on in
                set(["listen"], on || saved?[["listen"]] != nil ? .bool(on) : nil)
            })
            Picker("Language", selection: text(["language"])) {
                Text("Detected").tag("")
                Divider()
                ForEach(languages, id: \.self) { Text("\(ProviderVoice.languageName($0)) (\($0))").tag($0) }
            }
            LabeledContent("Longest note") {
                TextField("Longest note", value: Binding {
                    read(["max_seconds"])?.int.map(Int.init)
                } set: {
                    set(["max_seconds"], $0.map { .int(Int64($0)) })
                }, format: .number, prompt: Text("\(VoiceDoc.seconds.default)"))
                .labelsHidden().multilineTextAlignment(.trailing).frame(maxWidth: 80)
                Text("seconds (\(VoiceDoc.seconds.min) to \(VoiceDoc.seconds.max))").foregroundStyle(.secondary)
            }
            if connections.count > 1 {
                Picker("Provider", selection: Binding { provider } set: { name in
                    set(["provider"], .string(name))
                    if let key = connections.first(where: { $0.name == name })?.key, var doc = editor.doc {
                        VoiceDoc.declare(&doc, key)
                        editor.doc = doc
                    }
                }) {
                    ForEach(connections) { Text("\($0.name) (\($0.label))").tag($0.name) }
                }
            }
        } header: {
            SectionTitle("Speaking and listening", note: "The language is the one people's notes are in; empty detects it.")
        }
    }

    private var remove: some View {
        Section {
            HStack {
                Text("Without a voice \(name) answers in text only and doesn’t transcribe voice notes. \(connection?.key ?? "Its key") stays declared under Secrets.")
                    .font(.caption).foregroundStyle(.secondary)
                Spacer()
                Button("Remove Voice", role: .destructive) { editor.doc?.set(["voice"], nil) }
            }
        }
    }

    @ViewBuilder private func setting(_ title: String, _ path: Path, note: String) -> some View {
        let value = read(path)?.double
        LabeledContent {
            Slider(value: Binding {
                value ?? 0.5
            } set: { x in
                set(path, .double((x * 100).rounded() / 100))
            }, in: 0...1, step: 0.05)
            .frame(maxWidth: 220)
            Text(value.map { $0.formatted(.number.precision(.fractionLength(2))) } ?? "its own")
                .monospacedDigit().foregroundStyle(.secondary).frame(width: 56, alignment: .trailing)
            Button("Reset", systemImage: "arrow.uturn.backward.circle") { removeSetting(path) }
                .labelStyle(.iconOnly).buttonStyle(.borderless)
                .opacity(value == nil ? 0 : 1)
                .disabled(value == nil)
                .help("Use the voice's own")
        } label: {
            VStack(alignment: .leading, spacing: 1) {
                Text(title)
                Text(note).font(.caption).foregroundStyle(.secondary)
            }
        }
    }

    // MARK: Choices

    /// The models, with the current one when the provider does not list it.
    private func modelChoices(_ models: [ConnectionModel]) -> [ConnectionModel] {
        guard let m = read(["model"])?.string, !m.isEmpty, !models.contains(where: { $0.id == m }) else { return models }
        return models + [ConnectionModel(id: m)]
    }

    private func modelLabel(_ m: ConnectionModel) -> String {
        var s = m.name ?? m.id
        if let lang = read(["language"])?.string, !lang.isEmpty, let langs = m.languages, !langs.isEmpty, !langs.contains(lang) {
            s += " (no \(ProviderVoice.languageName(lang)))"
        }
        return s
    }

    /// What the models speak (else common ones), with the current language.
    private var languages: [String] {
        var all = Set((store.models ?? []).flatMap { $0.languages ?? [] })
        if all.isEmpty { all = Set(VoiceChooser.languages) }
        if let l = read(["language"])?.string, !l.isEmpty { all.insert(l) }
        return all.sorted { ProviderVoice.languageName($0) < ProviderVoice.languageName($1) }
    }

    // MARK: Bindings into voice:

    private func read(_ path: Path) -> JSONValue? { editor.doc?[["voice"] + path] }

    private func set(_ path: Path, _ value: JSONValue?) { editor.doc?.set(["voice"] + path, value) }

    /// A string; empty removes the key.
    private func text(_ path: Path) -> Binding<String> {
        Binding {
            read(path)?.string ?? ""
        } set: { s in
            set(path, s.isEmpty ? nil : .string(s))
        }
    }

    /// Removes a setting, and settings: too when that empties it and the file had none.
    private func removeSetting(_ path: Path) {
        set(path, nil)
        if read(["settings"])?.object?.isEmpty ?? false, saved?[["settings"]] == nil { set(["settings"], nil) }
    }

    private func clear() {
        guard var doc = editor.doc else { return }
        VoiceDoc.clear(&doc)
        editor.doc = doc
        chosen = nil
    }

    // MARK: Actions

    private func show() async {
        guard let cli = model.cli else { return }
        await store.show(agentID, with: cli)
    }

    private func loadCatalog() async {
        guard let cli = model.cli else { return }
        await store.loadModels(provider, agent: agentID, with: cli)
        await account.loadVoices(VoiceQuery(limit: 100), connection: provider, agent: agentID, with: cli)
    }

    private func trySample() async {
        guard let cli = model.cli else { return }
        if let s = await store.trySample(agentID, text: line, with: cli) {
            VoicePlayer.shared.play(URL(filePath: s.path))
        }
    }

    private func pick() {
        guard let cli = model.cli else { return }
        picking = Task { await store.pick(agentID, with: cli) }
    }
}
