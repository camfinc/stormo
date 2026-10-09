import StormoKit
import SwiftUI

/// Agents › New Agent…: a step-by-step wizard over `stormo new agent` (docs/api.md). Each step asks
/// one plain question with sensible defaults (the instance's usual engine and model); Advanced
/// reveals the technical fields. After creating, it asks for the keys the agent still lacks
/// (`secrets set <id> NAME`, value on stdin) and offers to start it or open its full editor.
struct NewAgentWizard: View {
    enum Step: Int, CaseIterable, Identifiable {
        case basics, team, brain, channels, personality, review, keys
        var id: Self { self }
        var title: String {
            switch self {
            case .basics: "Name & job"
            case .team: "Team"
            case .brain: "Intelligence"
            case .channels: "Where it talks"
            case .personality: "Personality"
            case .review: "Review"
            case .keys: "Connect"
            }
        }
        var symbol: String {
            switch self {
            case .basics: "person.text.rectangle"
            case .team: "person.3"
            case .brain: "brain"
            case .channels: "bubble.left.and.bubble.right"
            case .personality: "theatermasks"
            case .review: "checklist"
            case .keys: "key"
            }
        }
    }

    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss
    /// Opens the new agent's full editor.
    let open: (String) -> Void

    @AppStorage("newAgent.advanced") private var advanced = false
    @State private var step = Step.basics
    @State private var reached = Step.basics
    @State private var options: NewAgentOptions?
    @State private var loadProblem: String?
    @State private var draft = NewAgentDraft()
    @State private var creating = false
    @State private var problem: String?
    @State private var created: NewAgentResult?

    var body: some View {
        HStack(spacing: 0) {
            sidebar
            Divider()
            VStack(spacing: 0) {
                if let options {
                    ScrollView {
                        VStack(alignment: .leading, spacing: 0) { page(options) }
                            .frame(maxWidth: .infinity, alignment: .leading)
                            .padding(28)
                    }
                    .scrollBounceBehavior(.basedOnSize)
                } else if let loadProblem {
                    ContentUnavailableView("Can’t start the wizard", systemImage: "exclamationmark.triangle",
                                           description: Text(loadProblem))
                } else {
                    ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity)
                }
                Divider()
                footer
            }
        }
        .frame(width: 780, height: 580)
        .task(id: model.cli?.instance) { await load() }
    }

    // MARK: Chrome

    private var sidebar: some View {
        VStack(alignment: .leading, spacing: 4) {
            Text("New Agent").font(.title3.bold()).padding(.bottom, 12)
            ForEach(Step.allCases) { s in
                let done = s.rawValue < reached.rawValue || created != nil && s != .keys
                Button {
                    step = s
                } label: {
                    HStack(spacing: 10) {
                        Image(systemName: done && s != step ? "checkmark.circle.fill" : s.symbol)
                            .foregroundStyle(done && s != step ? Color.green : s == step ? Color.accentColor : .secondary)
                            .frame(width: 20)
                        Text(s.title).foregroundStyle(s.rawValue <= reached.rawValue ? .primary : .tertiary)
                        Spacer()
                    }
                    .padding(.vertical, 6).padding(.horizontal, 8)
                    .background(s == step ? Color.accentColor.opacity(0.12) : .clear, in: .rect(cornerRadius: 6))
                    .contentShape(Rectangle())
                }
                .buttonStyle(.plain)
                .disabled(created != nil ? s != .keys : s.rawValue > reached.rawValue)
            }
            Spacer()
            Toggle("Show advanced options", isOn: $advanced)
                .toggleStyle(.switch).controlSize(.small)
                .help("Engine, ID, channel and model details. Everything stays editable later too.")
        }
        .padding(18)
        .frame(width: 210)
        .background(.background.secondary)
    }

    private var footer: some View {
        HStack {
            if creating {
                ProgressView().controlSize(.small)
                Text("Creating \(draft.name)…").foregroundStyle(.secondary)
            } else if let problem {
                Label(problem, systemImage: "exclamationmark.circle").foregroundStyle(.red).lineLimit(2)
            } else if let hint = currentProblem, step != .keys {
                Text(hint).foregroundStyle(.secondary).lineLimit(2)
            }
            Spacer()
            if created == nil {
                Button("Cancel", role: .cancel) { dismiss() }.keyboardShortcut(.cancelAction)
                if step != .basics {
                    Button("Back") { move(-1) }
                }
                if step == .review {
                    Button("Create Agent") { Task { await create() } }
                        .keyboardShortcut(.defaultAction)
                        .disabled(creating || firstBlockedStep != nil)
                } else {
                    Button("Continue") { move(1) }
                        .keyboardShortcut(.defaultAction)
                        .disabled(options == nil || currentProblem != nil)
                }
            } else {
                Button("Done") { dismiss() }.keyboardShortcut(.cancelAction)
                Button("Open Editor") {
                    if let id = created?.id { dismiss(); open(id) }
                }
                .keyboardShortcut(.defaultAction)
            }
        }
        .padding(.horizontal, 20).padding(.vertical, 14)
    }

    private func move(_ by: Int) {
        problem = nil
        guard let next = Step(rawValue: step.rawValue + by), next != .keys else { return }
        step = next
        if next.rawValue > reached.rawValue { reached = next }
    }

    private func problem(at s: Step) -> String? {
        guard let options else { return nil }
        switch s {
        case .basics: return draft.nameProblem(taken: options.takenIds)
        case .team: return draft.unitProblem(units: options.options.units.map(\.id))
        case .brain: return draft.brainProblem
        default: return nil
        }
    }
    private var currentProblem: String? { problem(at: step) }
    private var firstBlockedStep: Step? { Step.allCases.first { problem(at: $0) != nil } }

    @ViewBuilder private func page(_ o: NewAgentOptions) -> some View {
        switch step {
        case .basics: BasicsPage(draft: $draft, advanced: advanced)
        case .team: TeamPage(draft: $draft, units: o.options.units, advanced: advanced)
        case .brain: BrainPage(draft: $draft, options: o, advanced: advanced)
        case .channels: ChannelsPage(draft: $draft, options: o.options, advanced: advanced)
        case .personality: PersonalityPage(draft: $draft, personas: o.options.personas, advanced: advanced)
        case .review: ReviewPage(draft: draft, options: o, advanced: advanced) { step = $0 }
        case .keys:
            if let created { KeysPage(result: created) }
        }
    }

    // MARK: Engine

    private func load() async {
        guard options == nil else { return }
        guard let cli = model.cli else {
            loadProblem = "No usable stormo binary. Choose one in Settings › Command line."
            return
        }
        loadProblem = nil
        do {
            let o = try await cli.run(["new", "agent", "--options"], as: NewAgentOptions.self)
            draft = NewAgentDraft(options: o)
            options = o
            #if DEBUG
            // STORMO_NEW_AGENT=<step> opens on that step with a sample draft (with STORMO_SNAPSHOT, a visual check).
            if let raw = ProcessInfo.processInfo.environment["STORMO_NEW_AGENT"],
               let s = Step.allCases.first(where: { "\($0)" == raw }), s != .keys {
                draft.name = "Concierge"
                draft.starter = "support"
                draft.role = NewAgentDraft.starters[0].role
                draft.channels["slack"] = .init()
                step = s
                reached = .review
            }
            #endif
        } catch {
            loadProblem = "This stormo can’t create agents yet (\(error.localizedDescription)). Update the command-line tool."
        }
    }

    private func create() async {
        guard let cli = model.cli else { return }
        creating = true
        problem = nil
        defer { creating = false }
        do {
            let body = try JSONEncoder().encode(draft.spec)
            // The engine mints the agent's own generated keys; the rest are asked for next.
            created = try await cli.run(["new", "agent"], as: NewAgentResult.self, stdin: body)
            step = .keys
            reached = .keys
            await model.fleet.refresh()
        } catch {
            problem = error.localizedDescription
        }
    }
}

// MARK: - Pieces

/// A step's headline and one-line explanation.
private struct PageHeader: View {
    let title: String
    let subtitle: String
    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            Text(title).font(.title2.bold())
            Text(subtitle).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
        }
        .padding(.bottom, 18)
    }
}

/// A selectable card with an icon, a title and a line of description.
private struct ChoiceCard: View {
    let symbol: String
    let title: String
    var detail: String?
    let selected: Bool
    let action: () -> Void
    var body: some View {
        Button(action: action) {
            HStack(alignment: .top, spacing: 12) {
                Image(systemName: symbol).font(.title2).frame(width: 30)
                    .foregroundStyle(selected ? Color.accentColor : .secondary)
                VStack(alignment: .leading, spacing: 3) {
                    Text(title).fontWeight(.semibold)
                    if let detail {
                        Text(detail).font(.callout).foregroundStyle(.secondary)
                            .multilineTextAlignment(.leading).fixedSize(horizontal: false, vertical: true)
                    }
                }
                Spacer(minLength: 0)
                Image(systemName: selected ? "checkmark.circle.fill" : "circle")
                    .foregroundStyle(selected ? Color.accentColor : Color.secondary.opacity(0.5))
            }
            .padding(12)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(.background, in: .rect(cornerRadius: 10))
            .overlay(RoundedRectangle(cornerRadius: 10).strokeBorder(selected ? Color.accentColor : Color.secondary.opacity(0.25),
                                                                     lineWidth: selected ? 2 : 1))
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
    }
}

/// The technical fields of a step: open when the global switch is on, else one click away.
private struct Advanced<Content: View>: View {
    let on: Bool
    @ViewBuilder let content: Content
    @State private var expanded = false
    var body: some View {
        DisclosureGroup(isExpanded: Binding(get: { on || expanded }, set: { expanded = $0 })) {
            VStack(alignment: .leading, spacing: 10) { content }
                .padding(.top, 8)
        } label: {
            Label("Advanced", systemImage: "slider.horizontal.3").foregroundStyle(.secondary)
        }
        .padding(.top, 18)
    }
}

private struct Field<Content: View>: View {
    let label: String
    var note: String?
    @ViewBuilder let content: Content
    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            Text(label).font(.callout.weight(.medium))
            content
            if let note { Text(note).font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true) }
        }
    }
}

// MARK: - Steps

private struct BasicsPage: View {
    @Binding var draft: NewAgentDraft
    let advanced: Bool
    private let columns = [GridItem(.flexible(), spacing: 10), GridItem(.flexible(), spacing: 10)]

    var body: some View {
        PageHeader(title: "Let’s meet your new agent",
                   subtitle: "Give it a name and pick what it will mostly do. You can change everything later.")
        Field(label: "Name") {
            TextField("Name", text: $draft.name, prompt: Text("e.g. Front Desk, Max, Research Bot"))
                .textFieldStyle(.roundedBorder).font(.title3).labelsHidden()
        }
        Text("What will it do?").font(.callout.weight(.medium)).padding(.top, 16).padding(.bottom, 4)
        LazyVGrid(columns: columns, spacing: 10) {
            ForEach(NewAgentDraft.starters) { s in
                ChoiceCard(symbol: s.symbol, title: s.title, selected: draft.starter == s.id) {
                    let previous = NewAgentDraft.starters.first { $0.id == draft.starter }?.role ?? ""
                    draft.starter = s.id
                    if draft.role.isEmpty || draft.role == previous { draft.role = s.role }
                }
            }
        }
        Field(label: "In a sentence", note: "This becomes the first line of its instructions.") {
            TextField("Role", text: $draft.role, prompt: Text("What the agent does, for whom"), axis: .vertical)
                .lineLimit(2...4).textFieldStyle(.roundedBorder).labelsHidden()
        }
        .padding(.top, 14)
        Advanced(on: advanced) {
            Field(label: "ID", note: "Names the agent's folder (agents/\(draft.id.isEmpty ? "…" : draft.id)) and its services. Lowercase letters, digits and hyphens; it can’t change later.") {
                TextField("ID", text: $draft.customID, prompt: Text(InstanceLocation.slug(from: draft.name).isEmpty ? "front-desk" : InstanceLocation.slug(from: draft.name)))
                    .textFieldStyle(.roundedBorder).monospaced().labelsHidden()
            }
        }
    }
}

private struct TeamPage: View {
    @Binding var draft: NewAgentDraft
    let units: [AgentOptions.Unit]
    let advanced: Bool
    private var teams: [AgentOptions.Unit] { units.filter { $0.id != "group" } }

    var body: some View {
        PageHeader(title: "Which team does it join?",
                   subtitle: "Agents work inside a team (a unit): they share its documents and knowledge, and only see their own team’s files.")
        VStack(spacing: 10) {
            ForEach(teams) { u in
                ChoiceCard(symbol: "person.3", title: u.name, detail: u.description, selected: !draft.createUnit && draft.unit == u.id) {
                    draft.createUnit = false
                    draft.unit = u.id
                }
            }
            ChoiceCard(symbol: "plus.circle", title: "Create a new team",
                       detail: teams.isEmpty ? "There are no teams yet, so start one." : "For work that doesn’t fit an existing team.",
                       selected: draft.createUnit) { draft.createUnit = true }
        }
        if draft.createUnit {
            VStack(alignment: .leading, spacing: 12) {
                Field(label: "Team name") {
                    TextField("Team name", text: $draft.newUnitName, prompt: Text("e.g. Customer Care")).textFieldStyle(.roundedBorder).labelsHidden()
                }
                Field(label: "What the team does") {
                    TextField("Description", text: $draft.newUnitDescription, prompt: Text("One line"), axis: .vertical)
                        .lineLimit(1...3).textFieldStyle(.roundedBorder).labelsHidden()
                }
                if advanced, !draft.newUnitID.isEmpty {
                    Text("Folder: units/\(draft.newUnitID)").font(.caption).monospaced().foregroundStyle(.secondary)
                }
            }
            .padding(.top, 14)
        }
    }
}

private struct BrainPage: View {
    @Binding var draft: NewAgentDraft
    let options: NewAgentOptions
    let advanced: Bool
    @State private var custom = false

    private var api: [AgentOptions.Connection] { (options.options.connections ?? []).filter(\.api) }
    private var plans: [AgentOptions.Connection] { (options.options.connections ?? []).filter { !$0.api } }
    private var hasDefault: Bool { options.defaults.model.name != nil }
    private var usingDefault: Bool {
        let d = options.defaults.model
        return hasDefault && !custom && draft.model == d.name && draft.provider == (d.provider ?? "")
            && draft.useLocal == (d.localName != nil) && (!draft.useLocal || draft.localModel == d.localName)
    }

    var body: some View {
        PageHeader(title: "What powers its thinking?",
                   subtitle: "The AI model the agent uses to understand and answer. If you’re not sure, keep what your other agents use.")
        VStack(spacing: 10) {
            if hasDefault {
                ChoiceCard(symbol: "star", title: "Same as my other agents (recommended)",
                           detail: summary(options.defaults.model), selected: usingDefault) {
                    custom = false
                    let d = options.defaults.model
                    draft.model = d.name ?? ""
                    draft.provider = d.provider ?? ""
                    draft.localModel = d.localName ?? ""
                    draft.localConnection = d.localConnection ?? ""
                    draft.useLocal = d.localName != nil
                }
            }
            ChoiceCard(symbol: "slider.horizontal.below.square.filled.and.square", title: "Choose a model",
                       detail: "Pick the account and the model yourself.", selected: !usingDefault) { custom = true }
        }
        if !usingDefault {
            VStack(alignment: .leading, spacing: 14) {
                Field(label: "AI account", note: "Accounts are set up under Providers.") {
                    Picker("AI account", selection: $draft.provider) {
                        ForEach(api) { c in Text("\(c.name) — \(c.label)").tag(c.name) }
                    }
                    .labelsHidden()
                }
                Field(label: "Model") {
                    HStack {
                        TextField("Model", text: $draft.model, prompt: Text("provider/model")).textFieldStyle(.roundedBorder).labelsHidden()
                        if !draft.provider.isEmpty { ModelChooser(connection: draft.provider, agentID: "", value: $draft.model) }
                    }
                }
                if !plans.isEmpty {
                    Toggle("Also use a ChatGPT plan through the core", isOn: $draft.useLocal)
                    if draft.useLocal {
                        Field(label: "ChatGPT account") {
                            Picker("ChatGPT account", selection: $draft.localConnection) {
                                Text("Default").tag("")
                                ForEach(plans) { c in Text(c.name).tag(c.name) }
                            }
                            .labelsHidden()
                        }
                        Field(label: "Plan model", note: "Runs locally on your plan; the model above is used in the cloud.") {
                            HStack {
                                TextField("Plan model", text: $draft.localModel, prompt: Text("the gateway’s name for it")).textFieldStyle(.roundedBorder).labelsHidden()
                                ModelChooser(connection: draft.localConnection.isEmpty ? "chatgpt" : draft.localConnection, agentID: "", value: $draft.localModel)
                            }
                        }
                    }
                }
            }
            .padding(.top, 14)
        }
        Advanced(on: advanced || draft.engineVersion.isEmpty) {
            Field(label: "Engine") {
                Picker("Engine", selection: $draft.engineKind) {
                    ForEach(options.options.engines, id: \.self) { Text($0).tag($0) }
                }
                .labelsHidden()
            }
            Field(label: "Engine version") {
                TextField("Version", text: $draft.engineVersion, prompt: Text("e.g. 0.21.5")).textFieldStyle(.roundedBorder).labelsHidden()
            }
            Field(label: "Image tag", note: "The pinned engine image. Never “latest”.") {
                TextField("Image tag", text: $draft.imageTag, prompt: Text("e.g. v2026.9.24")).textFieldStyle(.roundedBorder).monospaced().labelsHidden()
            }
        }
    }

    private func summary(_ m: NewAgentOptions.Model) -> String {
        var s = "\(m.name ?? "") via \(m.provider ?? "openrouter")"
        if let l = m.localName { s += ", and \(l) on the ChatGPT plan" }
        return s
    }
}

private struct ChannelsPage: View {
    @Binding var draft: NewAgentDraft
    let options: AgentOptions
    let advanced: Bool

    private func info(_ kind: String) -> (title: String, symbol: String, detail: String) {
        switch kind {
        case "slack": ("Slack", "number", "Team members message it in channels and DMs.")
        case "telegram": ("Telegram", "paperplane", "People chat with it in Telegram.")
        default: (kind.capitalized, "bubble.left", "")
        }
    }

    var body: some View {
        PageHeader(title: "Where can people reach it?",
                   subtitle: "Choose any you like, or none for now: it can still run scheduled work and be chatted with from this app.")
        VStack(spacing: 10) {
            ForEach(options.channels) { c in
                let i = info(c.kind)
                let on = draft.channels[c.kind] != nil
                VStack(alignment: .leading, spacing: 0) {
                    ChoiceCard(symbol: i.symbol, title: i.title,
                               detail: i.detail + (c.secrets.isEmpty ? "" : " Needs: \(c.secrets.joined(separator: ", ")) (you’ll add these at the end)."),
                               selected: on) {
                        draft.channels[c.kind] = on ? nil : .init()
                    }
                    if on && advanced {
                        channelSettings(c.kind).padding(.leading, 54).padding(.vertical, 10)
                    }
                }
            }
        }
        if !advanced && !draft.channels.isEmpty {
            Text("Who may talk to it, home channel and bot rules are under “Show advanced options”, or in the editor later.")
                .font(.caption).foregroundStyle(.secondary).padding(.top, 10)
        }
    }

    @ViewBuilder private func channelSettings(_ kind: String) -> some View {
        let binding = Binding(get: { draft.channels[kind] ?? .init() }, set: { draft.channels[kind] = $0 })
        VStack(alignment: .leading, spacing: 10) {
            Field(label: "Allowed users", note: kind == "slack" ? "Slack member IDs (U…), separated by commas. Empty: anyone in the workspace." : "User IDs, separated by commas. Empty: anyone.") {
                TextField("Allowed users", text: binding.allowedUsers).textFieldStyle(.roundedBorder).monospaced().labelsHidden()
            }
            if kind == "slack" {
                Field(label: "Home channel", note: "Where results of scheduled jobs are posted (a channel ID, C…).") {
                    TextField("Home channel", text: binding.homeChannel).textFieldStyle(.roundedBorder).monospaced().labelsHidden()
                }
                Field(label: "Messages from other bots") {
                    Picker("Messages from other bots", selection: binding.allowBots) {
                        Text("Default").tag("")
                        ForEach(options.allowBots, id: \.self) { Text($0).tag($0) }
                    }
                    .labelsHidden()
                }
            }
        }
    }
}

private struct PersonalityPage: View {
    @Binding var draft: NewAgentDraft
    let personas: [String]
    let advanced: Bool

    var body: some View {
        PageHeader(title: "How should it sound?",
                   subtitle: "Pick a tone. We’ll write its first instructions for you; edit them now or any time.")
        VStack(spacing: 8) {
            ForEach(NewAgentDraft.Tone.allCases) { t in
                ChoiceCard(symbol: t == .friendly ? "face.smiling" : t == .professional ? "briefcase" : "text.alignleft",
                           title: t.title, detail: t.summary, selected: draft.tone == t) { draft.tone = t }
            }
        }
        if !personas.isEmpty {
            Field(label: "Look", note: "A persona gives the agent a face in the office and in chats.") {
                Picker("Persona", selection: $draft.persona) {
                    Text("None").tag("")
                    ForEach(personas, id: \.self) { Text($0.replacingOccurrences(of: "personas/", with: "").capitalized).tag($0) }
                }
                .labelsHidden().frame(maxWidth: 260, alignment: .leading)
            }
            .padding(.top, 16)
        }
        HStack {
            Text("Instructions").font(.callout.weight(.medium))
            Spacer()
            if draft.editedSoul != nil {
                Button("Rewrite from my answers") { draft.editedSoul = nil }.controlSize(.small)
            }
        }
        .padding(.top, 16)
        TextEditor(text: Binding(get: { draft.soul }, set: { draft.editedSoul = $0 }))
            .font(.body.monospaced())
            .frame(minHeight: 160)
            .scrollContentBackground(.hidden)
            .padding(8)
            .background(.background, in: .rect(cornerRadius: 8))
            .overlay(RoundedRectangle(cornerRadius: 8).strokeBorder(.quaternary))
        Text("Saved as SOUL.md: what the agent reads before every conversation.")
            .font(.caption).foregroundStyle(.secondary)
    }
}

private struct ReviewPage: View {
    let draft: NewAgentDraft
    let options: NewAgentOptions
    let advanced: Bool
    let go: (NewAgentWizard.Step) -> Void

    private var team: String {
        draft.createUnit ? "\(draft.newUnitName) (new)" : options.options.units.first { $0.id == draft.unit }?.name ?? draft.unit
    }
    private var channels: String {
        draft.channels.isEmpty ? "None yet" : draft.channels.keys.sorted().map(\.capitalized).joined(separator: ", ")
    }

    var body: some View {
        PageHeader(title: "Ready to create \(draft.name)?",
                   subtitle: "Check the summary. Nothing starts running yet: you’ll connect accounts next, then start it when you’re ready.")
        VStack(spacing: 0) {
            row("Name", draft.name, .basics)
            row("Job", draft.role.isEmpty ? "—" : draft.role, .basics)
            row("Team", team, .team)
            row("Model", draft.model + (draft.useLocal ? " · plan: \(draft.localModel)" : ""), .brain)
            row("Channels", channels, .channels)
            row("Tone", draft.tone.title + (draft.persona.isEmpty ? "" : " · " + draft.persona), .personality)
        }
        .background(.background, in: .rect(cornerRadius: 10))
        .overlay(RoundedRectangle(cornerRadius: 10).strokeBorder(.quaternary))
        let needed = draft.neededSecrets(options.options)
        if !needed.isEmpty {
            Label("It will need: \(needed.joined(separator: ", ")). You can add the ones that are missing on the next step.",
                  systemImage: "key").font(.callout).foregroundStyle(.secondary).padding(.top, 14)
        }
        Advanced(on: advanced) {
            Text("Sent to `stormo new agent`; the engine fills empty fields with your usual engine and model and checks the result before writing agents/\(draft.id)/.")
                .font(.caption).foregroundStyle(.secondary)
            Text(specJSON).font(.caption.monospaced()).textSelection(.enabled)
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(10).background(.quaternary.opacity(0.4), in: .rect(cornerRadius: 6))
        }
    }

    private var specJSON: String {
        let enc = JSONEncoder()
        enc.outputFormatting = [.prettyPrinted, .sortedKeys, .withoutEscapingSlashes]
        return (try? enc.encode(draft.spec)).flatMap { String(data: $0, encoding: .utf8) } ?? ""
    }

    private func row(_ label: String, _ value: String, _ step: NewAgentWizard.Step) -> some View {
        VStack(spacing: 0) {
            HStack(alignment: .firstTextBaseline) {
                Text(label).foregroundStyle(.secondary).frame(width: 90, alignment: .leading)
                Text(value).frame(maxWidth: .infinity, alignment: .leading).lineLimit(3)
                Button("Change") { go(step) }.buttonStyle(.link).controlSize(.small)
            }
            .padding(.horizontal, 14).padding(.vertical, 10)
            if label != "Tone" { Divider().padding(.leading, 14) }
        }
    }
}

private struct KeysPage: View {
    @Environment(AppModel.self) private var model
    let result: NewAgentResult
    @State private var values: [String: String] = [:]
    @State private var saved = Set<String>()
    @State private var problem: String?
    @State private var busy = false
    @State private var started = false

    private var missing: [String] { result.missing ?? [] }

    var body: some View {
        PageHeader(title: "\(result.name) is created",
                   subtitle: missing.isEmpty
                       ? "Everything it needs is already connected. Start it whenever you like."
                       : "Last step: paste the keys it needs. They’re stored only on this Mac (secrets.local.yaml), never in its files.")
        if !missing.isEmpty {
            VStack(alignment: .leading, spacing: 12) {
                ForEach(missing, id: \.self) { name in
                    Field(label: name, note: hint(name)) {
                        HStack {
                            SecureField(name, text: Binding(get: { values[name] ?? "" }, set: { values[name] = $0 }))
                                .textFieldStyle(.roundedBorder).labelsHidden()
                                .disabled(saved.contains(name))
                            if saved.contains(name) {
                                Label("Saved", systemImage: "checkmark.circle.fill").foregroundStyle(.green)
                            }
                        }
                    }
                }
                HStack {
                    Button("Save Keys") { Task { await save() } }
                        .disabled(busy || values.filter { !$0.value.isEmpty && !saved.contains($0.key) }.isEmpty)
                    Text("Skip any you don’t have yet; add them later in Settings or with `stormo secrets set \(result.id) NAME`.")
                        .font(.caption).foregroundStyle(.secondary)
                }
            }
        }
        if let problem {
            Label(problem, systemImage: "exclamationmark.triangle").foregroundStyle(.orange).padding(.top, 8)
        }
        Divider().padding(.vertical, 18)
        HStack(spacing: 12) {
            Button {
                Task {
                    started = true
                    await model.run("start", agents: [result.id])
                }
            } label: {
                Label(started ? "Starting…" : "Start \(result.name)", systemImage: "play.fill")
            }
            .controlSize(.large)
            .disabled(started || !model.core.state.isRunning)
            Text(!model.core.state.isRunning ? "Start the core first to run agents."
                 : saved.count < missing.count ? "Some keys are still missing: it may not connect until they’re added." : "Runs it on this Mac.")
                .font(.caption).foregroundStyle(.secondary)
        }
        Text("Created \(result.files.joined(separator: ", ")).").font(.caption).foregroundStyle(.tertiary).padding(.top, 14)
    }

    private func hint(_ name: String) -> String? {
        switch name {
        case "SLACK_BOT_TOKEN": "From your Slack app › OAuth & Permissions (starts with xoxb-)."
        case "SLACK_APP_TOKEN": "From your Slack app › Basic Information › App-Level Tokens (starts with xapp-)."
        case "TELEGRAM_BOT_TOKEN": "From @BotFather in Telegram."
        case "OPENROUTER_API_KEY": "From openrouter.ai › Keys. Shared agents usually already have one under Providers."
        default: nil
        }
    }

    private func save() async {
        guard let cli = model.cli else { return }
        busy = true
        problem = nil
        defer { busy = false }
        for (name, value) in values where !value.isEmpty && !saved.contains(name) {
            do {
                _ = try await cli.run(["secrets", "set", result.id, name], as: [String: String].self,
                                      stdin: Data(value.trimmingCharacters(in: .whitespacesAndNewlines).utf8))
                saved.insert(name)
                values[name] = nil
            } catch {
                problem = "\(name): \(error.localizedDescription)"
            }
        }
    }
}
