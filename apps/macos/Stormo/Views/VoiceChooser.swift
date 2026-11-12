import StormoKit
import SwiftUI

/// Choose…: a voice from the connection's account or its Voice Library (`stormo connections voices`),
/// searched and filtered, each with the provider's sample to play (free). Voices other agents speak
/// with come after the free ones. Used by the agent editor's Voice page and the New Agent wizard.
struct VoiceChooser: View {
    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss
    let connection: String
    /// The agent choosing: its key is asked, and its own voice is not "used by" anyone else. nil in
    /// the wizard (the shared key).
    let agentID: String?
    /// The voice it speaks with now.
    let current: String?
    let choose: (ProviderVoice) -> Void

    @State private var store = VoiceStore()
    @State private var query: VoiceQuery
    @State private var search = ""
    /// Accents seen in the lists so far, to filter by.
    @State private var accents: Set<String> = []

    static let genders = ["female", "male", "neutral"]
    static let ages = ["young", "middle_aged", "old"]
    static let languages = ["en", "es", "pt", "fr", "de", "it", "nl", "pl", "sv", "tr", "ru", "ar", "hi", "ja", "ko", "zh"]

    /// language: the agent's voice.language, the filter it starts with.
    init(connection: String, agentID: String?, language: String, current: String?, choose: @escaping (ProviderVoice) -> Void) {
        self.connection = connection
        self.agentID = agentID
        self.current = current
        self.choose = choose
        _query = State(initialValue: VoiceQuery(language: language))
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            HStack(alignment: .firstTextBaseline) {
                Text("Choose a Voice").font(.title3).fontWeight(.semibold)
                Spacer()
                Picker("Voices", selection: $query.library) {
                    Text("My voices").tag(false)
                    Text("Voice library").tag(true)
                }
                .pickerStyle(.segmented)
                .labelsHidden()
                .fixedSize()
            }
            HStack(spacing: 8) {
                TextField("Search", text: $search, prompt: Text(query.library ? "Search the Voice Library" : "Search your voices"))
                    .textFieldStyle(.roundedBorder)
                    .onSubmit { query.search = search }
                filter("Gender", $query.gender, Self.genders)
                filter("Age", $query.age, Self.ages)
                filter("Accent", $query.accent, accents.sorted())
                filter("Language", $query.language, Self.languages, title: ProviderVoice.languageName)
            }
            list
            HStack {
                if let voices = store.voices, store.voicesProblem == nil {
                    Text(voices.isEmpty ? "" : "\(voices.count) voice\(voices.count == 1 ? "" : "s")" + (query.library ? " from the Voice Library" : ""))
                        .font(.caption).foregroundStyle(.secondary)
                    if voices.count >= query.limit, query.limit < 100 {
                        Button("Show More") { query.limit = 100 }.controlSize(.small)
                    }
                }
                Spacer()
                Text("Samples are \(connection)'s own and free to play.").font(.caption).foregroundStyle(.tertiary)
                Button("Cancel") { dismiss() }.keyboardShortcut(.cancelAction)
            }
        }
        .padding(20)
        .frame(width: 720, height: 600)
        .task(id: query) {
            guard let cli = model.cli else { return }
            await store.loadVoices(query, connection: connection, agent: agentID, with: cli)
            accents.formUnion((store.voices ?? []).compactMap { $0.accent?.lowercased() }.filter { !$0.isEmpty })
        }
        .task(id: search) {
            // Typing settles before the provider is asked.
            try? await Task.sleep(for: .milliseconds(500))
            if !Task.isCancelled, search != query.search { query.search = search }
        }
        .onDisappear { VoicePlayer.shared.stop() }
    }

    @ViewBuilder private var list: some View {
        let voices = store.voices ?? []
        ZStack {
            if let p = store.voicesProblem {
                ContentUnavailableView {
                    Label("Couldn’t list the voices", systemImage: "exclamationmark.triangle")
                } description: {
                    Text(p.message).textSelection(.enabled)
                }
            } else if voices.isEmpty && !store.loadingVoices && store.voices != nil {
                ContentUnavailableView("No voices match", systemImage: "waveform",
                                       description: Text(query.library ? "Try fewer filters or another search." : "Try fewer filters, or look in the Voice Library."))
            } else {
                List(voices) { v in
                    VoiceChoiceRow(voice: v, agentID: agentID, current: v.id == current, library: query.library) {
                        choose(v)
                        dismiss()
                    }
                }
                .listStyle(.inset)
                .opacity(store.loadingVoices ? 0.5 : 1)
            }
            if store.loadingVoices { ProgressView() }
        }
        .frame(maxHeight: .infinity)
        .background(.background, in: .rect(cornerRadius: 8))
        .overlay(RoundedRectangle(cornerRadius: 8).strokeBorder(.quaternary))
    }

    private func filter(_ label: String, _ value: Binding<String>, _ all: [String], title: @escaping (String) -> String = Self.words) -> some View {
        Picker(label, selection: value) {
            Text("Any \(label.lowercased())").tag("")
            Divider()
            ForEach(all.contains(value.wrappedValue) || value.wrappedValue.isEmpty ? all : all + [value.wrappedValue], id: \.self) {
                Text(title($0)).tag($0)
            }
        }
        .labelsHidden()
        .fixedSize()
        .help(label)
    }

    nonisolated static func words(_ s: String) -> String {
        let w = s.replacingOccurrences(of: "_", with: " ")
        return w.prefix(1).uppercased() + w.dropFirst()
    }
}

/// One voice: its sample, name, labels and description, who already speaks with it, Choose.
private struct VoiceChoiceRow: View {
    let voice: ProviderVoice
    let agentID: String?
    let current: Bool
    let library: Bool
    let choose: () -> Void

    var body: some View {
        HStack(alignment: .top, spacing: 10) {
            PlayButton(url: voice.previewURL, label: "Play \(voice.name)'s sample")
                .padding(.top, 1)
            VStack(alignment: .leading, spacing: 3) {
                HStack(spacing: 6) {
                    Text(voice.name).fontWeight(.medium).lineLimit(1)
                    if library && voice.inAccount { badge("In your account", .secondary) }
                    let users = voice.users(besides: agentID)
                    if !users.isEmpty { badge("Used by \(users.joined(separator: ", "))", .orange) }
                }
                if !voice.labels.isEmpty {
                    Text(voice.labels.joined(separator: " · ")).font(.caption).foregroundStyle(.secondary)
                }
                if let d = voice.description, !d.isEmpty {
                    Text(d).font(.caption).foregroundStyle(.tertiary).lineLimit(2).help(d)
                }
            }
            Spacer(minLength: 8)
            if current {
                Label("Current", systemImage: "checkmark").font(.callout).foregroundStyle(.secondary)
            } else {
                Button("Choose", action: choose)
            }
        }
        .padding(.vertical, 3)
    }

    private func badge(_ text: String, _ color: Color) -> some View {
        Text(text).font(.caption2).padding(.horizontal, 5).padding(.vertical, 1)
            .background(Capsule().fill(color.opacity(0.18)))
    }
}
