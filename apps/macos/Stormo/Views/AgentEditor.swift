import AppKit
import StormoKit
import SwiftUI

/// An agent's editor in the Agents section: its settings as a form, its behaviour (SOUL.md) and its
/// agent.yaml as text. Everything goes through `stormo config` (docs/api.md): the form saves the
/// fields it changed as a merge patch, so the file keeps its comments; the engine validates every
/// save and refuses one made over a file that changed on disk. After a save the agent is checked,
/// and a running agent picks the change up on restart.
struct AgentEditor: View {
    enum Page: String, CaseIterable, Identifiable {
        case settings, behaviour, yaml
        var id: Self { self }
        var title: String {
            switch self {
            case .settings: "Settings"
            case .behaviour: "Behaviour"
            case .yaml: "agent.yaml"
            }
        }
    }

    enum Check: Equatable {
        case running
        case passed(CheckedAgent?)
        case failed(String)
    }

    @Environment(AppModel.self) private var model
    let agentID: String
    @State private var page = Page.settings
    @State private var definition: ConfigEditor
    @State private var soul: ConfigEditor
    @State private var check: Check?
    @State private var confirmMigrate = false
    @State private var migration: MigrateResult?

    init(agentID: String) {
        self.agentID = agentID
        _definition = State(initialValue: ConfigEditor(path: "agents/\(agentID)/agent.yaml"))
        _soul = State(initialValue: ConfigEditor(path: "agents/\(agentID)/SOUL.md"))
    }

    private var editor: ConfigEditor { page == .behaviour ? soul : definition }
    private var agent: FleetAgent? { model.fleet.agent(agentID) }
    private var name: String { definition.doc?[["name"]]?.string ?? agent?.name ?? agentID }

    var body: some View {
        VStack(spacing: 0) {
            if let problem = editor.problem {
                ProblemBanner(problem: problem, stale: editor.isStale, oldBinary: editor.file == nil) {
                    Task { await reload() }
                }
            } else if page == .yaml && definition.isFormDirty {
                NoticeBanner(text: "The settings have unsaved changes. Save or revert them to edit the file as text.")
            } else if page == .settings && definition.isTextDirty {
                NoticeBanner(text: "agent.yaml has unsaved text changes. Save or revert them to use the form.")
            }
            content
            if let check, !editor.isDirty {
                Divider()
                checkBar(check)
                    .padding(.horizontal, 14)
                    .padding(.vertical, 8)
            }
        }
        .navigationTitle(name)
        .navigationSubtitle(page == .behaviour ? "agents/\(agentID)/SOUL.md" : "agents/\(agentID)/agent.yaml")
        .toolbar {
            ToolbarItem(placement: .principal) {
                Picker("Page", selection: $page) {
                    ForEach(Page.allCases) { p in
                        Text(p.title + (dirty(p) ? " •" : "")).tag(p)
                    }
                }
                .pickerStyle(.segmented)
                .labelsHidden()
            }
            ToolbarItemGroup(placement: .primaryAction) {
                Button("Revert", systemImage: "arrow.uturn.backward") { editor.revert() }
                    .help("Drop the unsaved changes")
                    .disabled(!editor.isDirty)
                Button("Save", systemImage: "checkmark") { Task { await save() } }
                    .keyboardShortcut("s")
                    .help("Save through stormo, which validates it first")
                    .disabled(!editor.isDirty || editor.busy)
            }
        }
        .task(id: model.cli?.instance) { await load() }
        .confirmationDialog("Migrate \(name) to agent.yaml format 1?", isPresented: $confirmMigrate) {
            Button("Migrate") { Task { await runMigrate() } }
        } message: {
            Text("""
            agent.yaml gets model, memory, limits and schedules (comments kept); Hermes' files move into engine/hermes/; \
            skills and scripts say $AGENT_HOME. If \(name) is stopped, its naps and its own secret values move into \
            agents/\(agentID)/data/ too (secrets.local.yaml is rewritten without its comments). \
            Changes the files only: check them with git before committing; a running agent picks them up on restart.
            """)
        }
        .sheet(item: $migration) { r in
            MigrationSummary(result: r, restart: agent?.state == "running" ? { Task { await model.run("restart", agents: [agentID]) } } : nil)
        }
    }

    private func runMigrate() async {
        guard let cli = model.cli else { return }
        definition.problemReset()
        do {
            let results = try await cli.run(["migrate", "agent", agentID], as: [MigrateResult].self)
            migration = results.first
        } catch {
            definition.report(error)
        }
        await load()
        await model.fleet.refresh()
    }

    @ViewBuilder private var content: some View {
        switch page {
        case .settings:
            if let options = definition.file?.options, definition.doc != nil {
                AgentForm(editor: definition, options: options, agent: agent, agentID: agentID) { confirmMigrate = true }
                    .disabled(definition.isTextDirty || definition.busy || definition.isStale)
            } else if definition.file != nil {
                ContentUnavailableView("agent.yaml does not parse", systemImage: "exclamationmark.triangle",
                                       description: Text("Fix it in the agent.yaml tab; the form comes back once it parses."))
            } else if definition.problem == nil {
                ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity)
            } else {
                Spacer()
            }
        case .behaviour:
            TextPane(editor: soul, wraps: true, editable: !soul.isStale)
        case .yaml:
            TextPane(editor: definition, wraps: false, editable: !definition.isFormDirty && !definition.isStale)
        }
    }

    private func dirty(_ p: Page) -> Bool {
        switch p {
        case .settings: definition.isFormDirty
        case .behaviour: soul.isDirty
        case .yaml: definition.isTextDirty
        }
    }

    @ViewBuilder private func checkBar(_ check: Check) -> some View {
        HStack(spacing: 8) {
            switch check {
            case .running:
                ProgressView().controlSize(.small)
                Text("Saved. Checking \(name)…").foregroundStyle(.secondary)
                Spacer()
            case .passed(let row):
                Image(systemName: "checkmark.circle.fill").foregroundStyle(.green)
                Text("Saved and checked" + (row.map { ": \($0.files) files, \($0.skills) skills" } ?? "") + ".")
                Spacer()
                if agent?.state == "running" {
                    Text("Restart to use it.").foregroundStyle(.secondary)
                    Button("Restart \(name)") { Task { await model.run("restart", agents: [agentID]) } }
                }
            case .failed(let message):
                Image(systemName: "exclamationmark.triangle.fill").foregroundStyle(.orange)
                Text("Saved, but the check failed: \(message)").textSelection(.enabled).lineLimit(3)
                Spacer()
            }
        }
        .font(.callout)
    }

    private func load() async {
        guard let cli = model.cli else { return }
        await definition.load(with: cli)
        await soul.load(with: cli)
    }

    private func reload() async {
        guard let cli = model.cli else { return }
        await editor.load(with: cli)
    }

    private func save() async {
        guard let cli = model.cli else { return }
        check = nil
        guard await editor.save(with: cli) else { return }
        check = .running
        do {
            check = .passed(try await cli.run(["check", agentID], as: [CheckedAgent].self).first)
        } catch {
            check = .failed(error.localizedDescription)
        }
        await model.fleet.refresh()
    }
}

// MARK: The form

/// agent.yaml as a form. It edits the loaded document in place, field by field, so keys it does not
/// show (a channel's local overrides, anything newer than this app) are kept; every choice comes
/// from the engine (`options`).
private struct AgentForm: View {
    @Bindable var editor: ConfigEditor
    let options: AgentOptions
    let agent: FleetAgent?
    let agentID: String
    let migrate: () -> Void

    typealias Path = [JSONValue.Key]

    var body: some View {
        Form {
            header
            identity
            engine
            addChannel
            ForEach(channelIndices, id: \.self) { i in channel(i) }
            actions
            secrets
            optionalSecrets
            environment
            state
            schedules
            limits
            learning
            deploy
        }
        .formStyle(.grouped)
    }

    /// agent.yaml's format: 1 is engine-neutral (model:, memory:, limits:, schedules:); 0 is the
    /// original layout, edited at its own paths until it is migrated.
    private var format: Int { self.get(["format"])?.int.map(Int.init) ?? 0 }
    private var modelPath: Path { format >= 1 ? ["model"] : ["engine"] }
    private var modelNameKey: JSONValue.Key { format >= 1 ? "name" : "model" }
    private var localNameKey: JSONValue.Key { format >= 1 ? "name" : "model" }

    // MARK: Sections

    private var header: some View {
        Section {
            HStack(spacing: 14) {
                if let agent {
                    AgentBadge(agent: agent, size: 56)
                }
                VStack(alignment: .leading, spacing: 2) {
                    Text(get(["name"])?.string ?? agentID).font(.title2).fontWeight(.semibold)
                    Text("id \(agentID) · the folder's name, fixed").font(.caption).foregroundStyle(.secondary)
                }
            }
            .padding(.vertical, 4)
            if format < 1 {
                HStack(alignment: .firstTextBaseline) {
                    Label("This agent uses agent.yaml format 0: its schedules and limits are still in Hermes' own files.",
                          systemImage: "arrow.up.circle")
                        .foregroundStyle(.secondary)
                    Spacer()
                    Button("Migrate to Format 1…", action: migrate)
                }
            }
        }
    }

    private var identity: some View {
        Section("Identity") {
            TextField("Name", text: text(["name"], keepEmpty: true))
            TextField("Role", text: text(["role"]), prompt: Text("What the agent does, in a sentence or two"), axis: .vertical)
                .lineLimit(2...6)
            Picker("Unit", selection: text(["unit"], keepEmpty: true)) {
                ForEach(choices(options.units.filter { $0.id != "group" }.map(\.id), current: get(["unit"])?.string), id: \.self) { id in
                    Text(options.units.first { $0.id == id }?.name ?? id).tag(id)
                }
            }
            Picker("Persona", selection: persona) {
                Text("None").tag("")
                ForEach(choices(options.personas, current: get(["persona", "path"])?.string), id: \.self) { p in
                    Text(p.split(separator: "/").last.map(String.init) ?? p).tag(p)
                }
            }
        }
    }

    private var engine: some View {
        Section {
            Picker("Engine", selection: text(["engine", "kind"], keepEmpty: true)) {
                ForEach(choices(options.engines, current: get(["engine", "kind"])?.string), id: \.self) { Text($0).tag($0) }
            }
            TextField("Version", text: text(["engine", "version"], keepEmpty: true))
            TextField("Image tag", text: text(["engine", "image_tag"]), prompt: Text("the pinned engine image"))
            if let conns = options.connections {
                Picker("Provider", selection: provider) {
                    ForEach(choices(conns.filter(\.api).map(\.name), current: get(modelPath + ["provider"])?.string), id: \.self) { n in
                        Text(conns.first { $0.name == n }.map { "\($0.name) (\($0.label))" } ?? n).tag(n)
                    }
                }
            } else {
                TextField("Provider", text: text(modelPath + ["provider"]), prompt: Text("openrouter"))
            }
            LabeledContent("Model") {
                HStack {
                    TextField("Model", text: text(modelPath + [modelNameKey], keepEmpty: true), prompt: Text("provider/model"))
                        .labelsHidden().multilineTextAlignment(.trailing)
                    ModelChooser(connection: get(modelPath + ["provider"])?.string ?? "openrouter", agentID: agentID,
                                 value: text(modelPath + [modelNameKey], keepEmpty: true))
                }
            }
            Toggle("Use a ChatGPT account on the core when running locally", isOn: local)
            if get(modelPath + ["local"]) != nil {
                if format >= 1, let conns = options.connections?.filter({ !$0.api }), conns.count > 1 {
                    Picker("ChatGPT account", selection: text(["model", "local", "connection"])) {
                        ForEach(conns) { c in Text(c.name).tag(c.name == "chatgpt" ? "" : c.name) }
                    }
                }
                LabeledContent("Local model") {
                    HStack {
                        TextField("Local model", text: text(modelPath + ["local", localNameKey], keepEmpty: true), prompt: Text("the gateway's name for it"))
                            .labelsHidden().multilineTextAlignment(.trailing)
                        ModelChooser(connection: format >= 1 ? (get(["model", "local", "connection"])?.string ?? "chatgpt") : "chatgpt",
                                     agentID: agentID, value: text(modelPath + ["local", localNameKey], keepEmpty: true))
                    }
                }
            }
        } header: {
            Text("Model")
        }
    }

    private var channelIndices: [Int] { Array((get(["channels"])?.array ?? []).indices) }

    private func channel(_ i: Int) -> some View {
        let base: Path = ["channels", .index(i)]
        let kind = get(base + ["kind"])?.string ?? "?"
        let slack = kind == "slack"
        let needs = options.channels.first { $0.kind == kind }?.secrets ?? []
        let missing = needs.filter { !declared($0) }
        return Section {
            StringListRows(title: "Allowed users", items: strings(base + ["allowed_users"]),
                           prompt: slack ? "Slack member ID (U…)" : "Telegram user id", suggestions: [], monospaced: true)
            Toggle("Anyone may message", isOn: toggle(base + ["allow_all_users"], default: false))
            if slack {
                Picker("Messages from other bots", selection: text(base + ["allow_bots"])) {
                    Text("Default").tag("")
                    ForEach(options.allowBots, id: \.self) { Text($0).tag($0) }
                }
            }
            TextField("Home channel", text: text(base + ["home_channel"]), prompt: Text("where cron results go"))
            if slack {
                Toggle("Owns the workspace's slash commands", isOn: toggle(base + ["slash_commands"], default: false))
            }
        } header: {
            HStack {
                Label(kind.capitalized, systemImage: slack ? "number" : "paperplane")
                Spacer()
                Button("Remove", systemImage: "trash", role: .destructive) { set(base, nil) }
                    .labelStyle(.iconOnly)
                    .buttonStyle(.borderless)
                    .help("Remove the \(kind.capitalized) channel")
            }
        } footer: {
            if !missing.isEmpty {
                Label("Needs \(missing.joined(separator: ", ")) under Secrets or Optional secrets.", systemImage: "exclamationmark.triangle")
                    .foregroundStyle(.orange)
            }
        }
    }

    private var addChannel: some View {
        let present = Set((get(["channels"])?.array ?? []).compactMap { $0[["kind"]]?.string })
        let addable = options.channels.filter { !present.contains($0.kind) }
        return Section {
            if addable.isEmpty {
                Text("Every channel kind is set up.").foregroundStyle(.secondary)
            } else {
                if present.isEmpty { Text("No channels: the agent works from cron jobs and other agents only.").foregroundStyle(.secondary) }
                HStack {
                    ForEach(addable) { c in
                        Button("Add \(c.kind.capitalized)", systemImage: "plus") {
                            var list = get(["channels"])?.array ?? []
                            list.append(.object(["kind": .string(c.kind)]))
                            set(["channels"], .array(list))
                        }
                    }
                }
            }
        } header: {
            SectionTitle("Channels", note: "Where people reach the agent. Each channel's settings follow.")
        }
    }

    private var actions: some View {
        let unit = get(["unit"])?.string ?? ""
        let chosen = strings(["actions"])
        let available = options.actions.filter { $0.unit == unit || $0.unit == "group" }
        let foreign = chosen.wrappedValue.filter { name in !available.contains { $0.name == name } }
        return Section {
            if available.isEmpty && foreign.isEmpty {
                Text("No bridge actions for this unit.").foregroundStyle(.secondary)
            }
            ForEach(available) { a in
                Toggle(isOn: member(a.name, of: chosen)) {
                    VStack(alignment: .leading, spacing: 2) {
                        HStack(spacing: 6) {
                            Text(a.name).monospaced()
                            if a.mutates {
                                Text("changes data").font(.caption2).padding(.horizontal, 5).padding(.vertical, 1)
                                    .background(Capsule().fill(.orange.opacity(0.18)))
                            }
                        }
                        if !a.description.isEmpty { Text(a.description).font(.caption).foregroundStyle(.secondary) }
                    }
                }
            }
            ForEach(foreign, id: \.self) { name in
                Toggle(isOn: member(name, of: chosen)) {
                    Label("\(name): not available to this unit", systemImage: "exclamationmark.triangle").foregroundStyle(.orange)
                }
            }
        } header: {
            SectionTitle("Bridge actions", note: "Actions of the agent's unit and of group.")
        }
    }

    private var secrets: some View {
        Section {
            StringListRows(title: nil, items: strings(["secrets"]), prompt: "ENV_VAR_NAME",
                           suggestions: options.secrets, monospaced: true)
        } header: {
            SectionTitle("Secrets", note: "Names only. Values live in secrets.local.yaml and AWS Secrets Manager.")
        }
    }

    private var optionalSecrets: some View {
        let list = get(["optional_secrets"])?.array ?? []
        return Section {
            ForEach(list.indices, id: \.self) { i in
                let base: Path = ["optional_secrets", .index(i)]
                HStack(alignment: .firstTextBaseline) {
                    TextField("Name", text: text(base + ["name"], keepEmpty: true), prompt: Text("ENV_VAR_NAME"))
                        .monospaced()
                        .labelsHidden()
                    Spacer()
                    MultiPicker(title: "Skills", all: options.skills, selection: strings(base + ["skills"]))
                    MultiPicker(title: "Actions", all: strings(["actions"]).wrappedValue, selection: strings(base + ["actions"]))
                    Button("Remove", systemImage: "minus.circle") { remove(base, emptying: ["optional_secrets"]) }
                        .labelStyle(.iconOnly).buttonStyle(.borderless)
                }
            }
            Button("Add Optional Secret", systemImage: "plus") {
                set(["optional_secrets"], .array(list + [.object(["name": .string("")])]))
            }
        } header: {
            SectionTitle("Optional secrets", note: "Without a value, the skills and actions an optional secret gates are left out instead of failing.")
        }
    }

    private var environment: some View {
        let env = get(["env"])?.object ?? [:]
        return Section {
            ForEach(env.keys.sorted(), id: \.self) { key in
                HStack {
                    Text(key).monospaced().lineLimit(1).truncationMode(.middle).frame(width: 260, alignment: .leading)
                    TextField("Value", text: text(["env", .key(key)], keepEmpty: true)).labelsHidden().monospaced()
                    Button("Remove", systemImage: "minus.circle") { remove(["env", .key(key)], emptying: ["env"]) }
                        .labelStyle(.iconOnly).buttonStyle(.borderless)
                }
            }
            PairAdder(first: "NAME", second: "value") { k, v in set(["env", .key(k)], .string(v)) }
        } header: {
            SectionTitle("Environment", note: "Plain settings. A credential goes under Secrets, never here.")
        }
    }

    private var state: some View {
        let list = get(["state"])?.array ?? []
        return Section {
            ForEach(list.indices, id: \.self) { i in
                HStack {
                    Text(list[i][["name"]]?.string ?? "").monospaced().lineLimit(1).frame(width: 260, alignment: .leading)
                    Text(list[i][["env"]]?.string ?? "").monospaced().foregroundStyle(.secondary)
                    Spacer()
                    Button("Remove", systemImage: "minus.circle") { remove(["state", .index(i)], emptying: ["state"]) }
                        .labelStyle(.iconOnly).buttonStyle(.borderless)
                }
            }
            PairAdder(first: "name", second: "ENV_VAR") { n, e in
                set(["state"], .array(list + [.object(["name": .string(n), "env": .string(e)])]))
            }
        } header: {
            SectionTitle("State", note: "Directories kept across restarts by naps; each one's path is in its env var.")
        }
    }

    @ViewBuilder private var limits: some View {
        if format >= 1 {
            Section {
                LabeledContent("Steps per turn") {
                    TextField("Steps per turn", value: int(["limits", "turns"]), format: .number, prompt: Text("engine default"))
                        .labelsHidden().multilineTextAlignment(.trailing).frame(maxWidth: 110)
                }
                Picker("Reasoning", selection: text(["limits", "reasoning"])) {
                    Text("Engine default").tag("")
                    ForEach(choices(options.reasoning ?? [], current: get(["limits", "reasoning"])?.string), id: \.self) { Text($0).tag($0) }
                }
                LabeledContent("Terminal command timeout") {
                    TextField("Terminal command timeout", value: int(["limits", "command_timeout"]), format: .number, prompt: Text("engine default"))
                        .labelsHidden().multilineTextAlignment(.trailing).frame(maxWidth: 110)
                    Text("seconds").foregroundStyle(.secondary)
                }
                LabeledContent("Scheduled script timeout") {
                    TextField("Scheduled script timeout", value: int(["limits", "script_timeout"]), format: .number, prompt: Text("engine default"))
                        .labelsHidden().multilineTextAlignment(.trailing).frame(maxWidth: 110)
                    Text("seconds").foregroundStyle(.secondary)
                }
            } header: {
                SectionTitle("Limits", note: "How far one turn may go. Empty leaves the engine's own setting.")
            }
        }
    }

    @ViewBuilder private var schedules: some View {
        if format >= 1 {
            let list = get(["schedules"])?.array ?? []
            Section {
                if list.isEmpty {
                    Text("No scheduled jobs.").foregroundStyle(.secondary)
                }
                ForEach(list.indices, id: \.self) { i in
                    ScheduleRows(form: self, path: ["schedules", .index(i)])
                }
                Button("Add Schedule", systemImage: "plus") {
                    let id = String(UUID().uuidString.lowercased().replacingOccurrences(of: "-", with: "").prefix(12))
                    set(["schedules"], .array(list + [.object(["id": .string(id), "name": .string("New schedule"), "every": .string("1h"), "prompt": .string("")])]))
                }
            } header: {
                SectionTitle("Schedules", note: "Jobs the agent runs on its own. Run times and failures are kept by the engine, not here.")
            }
        } else if get(["engine"]) != nil {
            Section {
                Text("Format 0 keeps scheduled jobs in Hermes' own file. Migrate to format 1 to edit them here.")
                    .foregroundStyle(.secondary)
            } header: {
                Text("Schedules")
            }
        }
    }

    private var learning: some View {
        Section("Learning") {
            LabeledContent("Memory limit") {
                TextField("Memory limit", value: int(format >= 1 ? ["memory", "agent"] : ["learning", "memory_char_limit"]), format: .number, prompt: Text("default"))
                    .labelsHidden().multilineTextAlignment(.trailing).frame(maxWidth: 110)
                Text("characters").foregroundStyle(.secondary)
            }
            LabeledContent("User profile limit") {
                TextField("User profile limit", value: int(format >= 1 ? ["memory", "user"] : ["learning", "user_char_limit"]), format: .number, prompt: Text("default"))
                    .labelsHidden().multilineTextAlignment(.trailing).frame(maxWidth: 110)
                Text("characters").foregroundStyle(.secondary)
            }
            LabeledContent("Seed fill") {
                Slider(value: double(["learning", "seed_fill"], default: 0.6), in: 0...1, step: 0.05)
                    .frame(maxWidth: 220)
                Text(get(["learning", "seed_fill"])?.double.map { $0.formatted(.percent.precision(.fractionLength(0))) } ?? "default")
                    .monospacedDigit().foregroundStyle(.secondary).frame(width: 60, alignment: .trailing)
            }
            LabeledContent("Nap every") {
                TextField("Nap every", value: int(["learning", "nap_interval_seconds"]), format: .number, prompt: Text("default"))
                    .labelsHidden().multilineTextAlignment(.trailing).frame(maxWidth: 110)
                Text(get(["learning", "nap_interval_seconds"])?.int.map {
                    "seconds (\(Duration.seconds($0).formatted(.units(allowed: [.hours, .minutes], width: .abbreviated))))"
                } ?? "seconds").foregroundStyle(.secondary)
            }
        }
    }

    @ViewBuilder private var deploy: some View {
        if let deploy = get(["deploy"])?.object, !deploy.isEmpty {
            Section {
                ForEach(deploy.keys.sorted(), id: \.self) { key in
                    LabeledContent(key) { Text(describe(deploy[key] ?? .null)).monospaced().textSelection(.enabled) }
                }
            } header: {
                SectionTitle("Deploy", note: "Cloud settings name deployed resources, so the app shows them but does not change them.")
            }
        }
    }

    // MARK: Bindings into the document

    fileprivate func get(_ path: Path) -> JSONValue? { editor.doc?[path] }

    fileprivate func set(_ path: Path, _ value: JSONValue?) { editor.doc?.set(path, value) }

    /// Removes path, and the container too when that leaves it empty and it was not in the file.
    fileprivate func remove(_ path: Path, emptying container: Path) {
        set(path, nil)
        let left = get(container)
        if (left?.object?.isEmpty ?? false) || (left?.array?.isEmpty ?? false), editor.file?.doc?[container] == nil {
            set(container, nil)
        }
    }

    private func declared(_ name: String) -> Bool {
        strings(["secrets"]).wrappedValue.contains(name)
            || (get(["optional_secrets"])?.array ?? []).contains { $0[["name"]]?.string == name }
    }

    /// A scalar as text. Typing the text it already shows changes nothing, so a number or a
    /// boolean keeps its type; empty removes the key unless keepEmpty.
    fileprivate func text(_ path: Path, keepEmpty: Bool = false) -> Binding<String> {
        Binding {
            get(path).map(describe) ?? ""
        } set: { s in
            if let v = get(path), describe(v) == s { return }
            set(path, s.isEmpty && !keepEmpty ? nil : .string(s))
        }
    }

    fileprivate func int(_ path: Path) -> Binding<Int?> {
        Binding {
            get(path)?.int.map(Int.init)
        } set: { n in
            set(path, n.map { .int(Int64($0)) })
        }
    }

    private func double(_ path: Path, default d: Double) -> Binding<Double> {
        Binding {
            get(path)?.double ?? d
        } set: { x in
            set(path, .double((x * 100).rounded() / 100))
        }
    }

    /// A boolean that is absent when it equals its default and the file did not set it.
    fileprivate func toggle(_ path: Path, default d: Bool) -> Binding<Bool> {
        Binding {
            get(path)?.bool ?? d
        } set: { b in
            set(path, b == d && editor.file?.doc?[path] == nil ? nil : .bool(b))
        }
    }

    /// A list of scalars as strings; items that read the same keep their original values.
    fileprivate func strings(_ path: Path) -> Binding<[String]> {
        Binding {
            (get(path)?.array ?? []).map(describe)
        } set: { list in
            let old = get(path)?.array ?? []
            if list.isEmpty && editor.file?.doc?[path] == nil {
                set(path, nil)
                return
            }
            set(path, .array(list.map { s in old.first { describe($0) == s } ?? .string(s) }))
        }
    }

    private func member(_ item: String, of list: Binding<[String]>) -> Binding<Bool> {
        Binding {
            list.wrappedValue.contains(item)
        } set: { on in
            var items = list.wrappedValue.filter { $0 != item }
            if on { items.append(item) }
            list.wrappedValue = items
        }
    }

    private var persona: Binding<String> {
        Binding {
            get(["persona", "path"])?.string ?? ""
        } set: { p in
            if p.isEmpty { set(["persona"], nil) } else { set(["persona", "path"], .string(p)) }
        }
    }

    /// The agent's API connection; choosing one adds its key's name to secrets: (the manifest
    /// must declare it).
    private var provider: Binding<String> {
        Binding {
            get(modelPath + ["provider"])?.string ?? "openrouter"
        } set: { name in
            set(modelPath + ["provider"], .string(name))
            if let key = options.connections?.first(where: { $0.name == name })?.key {
                let list = strings(["secrets"])
                if !list.wrappedValue.contains(key) && !(get(["optional_secrets"])?.array ?? []).contains(where: { $0[["name"]]?.string == key }) {
                    list.wrappedValue.append(key)
                }
            }
        }
    }

    private var local: Binding<Bool> {
        Binding {
            get(modelPath + ["local"]) != nil
        } set: { on in
            if on {
                let model = get(modelPath + [modelNameKey])?.string?.split(separator: "/").last.map(String.init) ?? ""
                let key = format >= 1 ? "name" : "model"
                set(modelPath + ["local"], editor.file?.doc?[modelPath + ["local"]] ?? .object(["via": .string("core"), key: .string(model)]))
            } else {
                set(modelPath + ["local"], nil)
            }
        }
    }

    /// The choices, plus the current value when it is not one of them (so the picker shows it).
    fileprivate func choices(_ all: [String], current: String?) -> [String] {
        guard let current, !current.isEmpty, !all.contains(current) else { return all }
        return all + [current]
    }
}

private func describe(_ v: JSONValue) -> String {
    switch v {
    case .null: ""
    case .bool(let b): b ? "true" : "false"
    case .int(let i): String(i)
    case .double(let d): String(d)
    case .string(let s): s
    case .array(let a): a.map(describe).joined(separator: ", ")
    case .object(let o): o.keys.sorted().map { "\($0): \(describe(o[$0] ?? .null))" }.joined(separator: ", ")
    }
}

// MARK: Pieces

extension MigrateResult: @retroactive Identifiable {
    public var id: String { agent }
}

/// What `stormo migrate agent` changed, after the fact.
private struct MigrationSummary: View {
    let result: MigrateResult
    let restart: (() -> Void)?
    @Environment(\.dismiss) private var dismiss

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            Label("\(result.agent) is on agent.yaml format \(result.to)", systemImage: "checkmark.circle.fill")
                .font(.headline)
            ScrollView {
                VStack(alignment: .leading, spacing: 4) {
                    ForEach(result.changes, id: \.self) { Text("• " + $0).textSelection(.enabled) }
                }
                .frame(maxWidth: .infinity, alignment: .leading)
            }
            .frame(maxHeight: 260)
            if result.data == "running" {
                Text("It is running, so its data (naps, secret values) stayed where it was. Stop it and migrate again to move it.")
                    .foregroundStyle(.secondary)
            }
            Text("Review the changed files with git before you commit them.").foregroundStyle(.secondary)
            HStack {
                Spacer()
                if let restart {
                    Button("Restart \(result.agent)") {
                        restart()
                        dismiss()
                    }
                }
                Button("Done") { dismiss() }.keyboardShortcut(.defaultAction)
            }
        }
        .padding(20)
        .frame(width: 520)
    }
}

/// One schedule in the form, collapsed to a summary line.
private struct ScheduleRows: View {
    let form: AgentForm
    let path: [JSONValue.Key]
    @State private var open = false

    private var interval: Bool { form.get(path + ["every"]) != nil || form.get(path + ["cron"]) == nil }
    private var runsAgent: Bool { form.get(path + ["agent"])?.bool ?? true }

    var body: some View {
        DisclosureGroup(isExpanded: $open) {
            TextField("Name", text: form.text(path + ["name"]))
            Picker("Runs", selection: Binding {
                interval
            } set: { every in
                guard every != interval else { return }
                form.set(path + [every ? "cron" : "every"], nil)
                form.set(path + [every ? "every" : "cron"], .string(every ? "1h" : "0 9 * * *"))
            }) {
                Text("Every").tag(true)
                Text("On a cron schedule").tag(false)
            }
            .pickerStyle(.segmented)
            if interval {
                TextField("Every", text: form.text(path + ["every"], keepEmpty: true), prompt: Text("30m, 2h, 1d"))
            } else {
                TextField("Cron", text: form.text(path + ["cron"], keepEmpty: true), prompt: Text("minute hour day month weekday"))
                    .monospaced()
            }
            Toggle("Enabled", isOn: form.toggle(path + ["enabled"], default: true))
            TextField("Note", text: form.text(path + ["note"]), prompt: Text("why it is paused, or anything worth knowing"))
            Toggle("Runs the agent", isOn: form.toggle(path + ["agent"], default: true))
            if runsAgent {
                TextField("Prompt", text: form.text(path + ["prompt"]), prompt: Text("What the agent does each time"), axis: .vertical)
                    .lineLimit(3...12)
                LabeledContent("Skills") {
                    MultiPicker(title: "Skills", all: form.options.skills.map { $0.split(separator: "/").last.map(String.init) ?? $0 },
                                selection: form.strings(path + ["skills"]))
                }
            }
            Picker(runsAgent ? "Script first" : "Script", selection: form.text(path + ["script"])) {
                Text("None").tag("")
                ForEach(form.choices(form.options.scripts ?? [], current: form.get(path + ["script"])?.string), id: \.self) { Text($0).tag($0) }
            }
            Picker("Only when", selection: form.text(path + ["monitor"])) {
                Text("Always").tag("")
                ForEach(form.choices(form.options.scripts ?? [], current: form.get(path + ["monitor"])?.string), id: \.self) { Text("\($0) says so").tag($0) }
            }
            if runsAgent {
                TextField("Model", text: form.text(path + ["model"]), prompt: Text("the agent's own"))
                TextField("Provider", text: form.text(path + ["provider"]), prompt: Text("the agent's own"))
            }
            TextField("Deliver to", text: form.text(path + ["deliver"]), prompt: Text("slack, telegram, local"))
            TextField("Failures to", text: form.text(path + ["on_failure"]), prompt: Text("where a failed run is reported"))
            StringListRows(title: "Tools", items: form.strings(path + ["tools"]), prompt: "toolset (empty: the agent's own)", suggestions: [], monospaced: true)
            HStack {
                Text("id \(form.get(path + ["id"])?.string ?? "")").font(.caption).foregroundStyle(.tertiary).monospaced()
                Spacer()
                Button("Remove Schedule", systemImage: "trash", role: .destructive) { form.remove(path, emptying: ["schedules"]) }
                    .buttonStyle(.borderless)
            }
        } label: {
            HStack {
                Text(form.get(path + ["name"])?.string ?? "Schedule").fontWeight(.medium)
                Text(summary).foregroundStyle(.secondary)
                Spacer()
                if form.get(path + ["enabled"])?.bool == false {
                    Text("paused").font(.caption).padding(.horizontal, 6).padding(.vertical, 1)
                        .background(Capsule().fill(.secondary.opacity(0.15)))
                }
            }
        }
    }

    private var summary: String {
        if let e = form.get(path + ["every"])?.string { return "every \(e)" }
        if let c = form.get(path + ["cron"])?.string { return c }
        return ""
    }
}

/// A section's title with a line on what it is for.
struct SectionTitle: View {
    let title: String
    let note: String
    init(_ title: String, note: String) {
        self.title = title
        self.note = note
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 2) {
            Text(title)
            Text(note).font(.caption).fontWeight(.regular).foregroundStyle(.secondary)
        }
    }
}

/// A list of strings as rows in a section: each with a remove button, then a field to add one
/// (with suggestions to pick from).
private struct StringListRows: View {
    let title: String?
    @Binding var items: [String]
    let prompt: String
    let suggestions: [String]
    var monospaced = false
    @State private var draft = ""

    var body: some View {
        if let title {
            LabeledContent(title) {
                if items.isEmpty { Text("none").foregroundStyle(.tertiary) }
            }
        }
        ForEach(Array(items.enumerated()), id: \.offset) { i, item in
            HStack {
                Text(item).monospaced(monospaced).textSelection(.enabled)
                Spacer()
                Button("Remove", systemImage: "minus.circle") { items.remove(at: i) }
                    .labelStyle(.iconOnly).buttonStyle(.borderless)
            }
            .padding(.leading, title == nil ? 0 : 12)
        }
        HStack {
            TextField("Add", text: $draft, prompt: Text(prompt))
                .labelsHidden()
                .monospaced(monospaced)
                .onSubmit(add)
            let more = suggestions.filter { !items.contains($0) }
            if !more.isEmpty {
                Menu {
                    ForEach(more, id: \.self) { s in Button(s) { items.append(s) } }
                } label: {
                    Image(systemName: "list.bullet")
                }
                .menuStyle(.borderlessButton)
                .fixedSize()
                .help("Pick one declared elsewhere in the instance")
            }
            Button("Add", systemImage: "plus.circle", action: add)
                .labelStyle(.iconOnly).buttonStyle(.borderless)
                .disabled(draft.trimmingCharacters(in: .whitespaces).isEmpty)
        }
        .padding(.leading, title == nil ? 0 : 12)
    }

    private func add() {
        let s = draft.trimmingCharacters(in: .whitespaces)
        guard !s.isEmpty, !items.contains(s) else { return }
        items.append(s)
        draft = ""
    }
}

/// Two fields and an Add button, for a new environment variable or state directory.
private struct PairAdder: View {
    let first: String
    let second: String
    let add: (String, String) -> Void
    @State private var a = ""
    @State private var b = ""

    var body: some View {
        HStack {
            TextField(first, text: $a, prompt: Text(first)).labelsHidden().monospaced().frame(width: 260)
            TextField(second, text: $b, prompt: Text(second)).labelsHidden().monospaced()
                .onSubmit(commit)
            Button("Add", systemImage: "plus.circle", action: commit)
                .labelStyle(.iconOnly).buttonStyle(.borderless)
                .disabled(a.trimmingCharacters(in: .whitespaces).isEmpty)
        }
    }

    private func commit() {
        let k = a.trimmingCharacters(in: .whitespaces)
        guard !k.isEmpty else { return }
        add(k, b)
        a = ""
        b = ""
    }
}

/// A pop-up of checkboxes over a list, summarised as a count.
private struct MultiPicker: View {
    let title: String
    let all: [String]
    @Binding var selection: [String]

    var body: some View {
        Menu {
            if all.isEmpty { Text("None to choose from") }
            ForEach(all, id: \.self) { item in
                Toggle(item, isOn: Binding {
                    selection.contains(item)
                } set: { on in
                    selection = selection.filter { $0 != item } + (on ? [item] : [])
                })
            }
        } label: {
            Text(selection.isEmpty ? "No \(title.lowercased())"
                 : "\(selection.count) \(selection.count == 1 ? String(title.lowercased().dropLast()) : title.lowercased())")
        }
        .fixedSize()
        .help("\(title) this secret gates")
    }
}

private struct ProblemBanner: View {
    let problem: ConfigEditor.Problem
    let stale: Bool
    let oldBinary: Bool
    let reload: () -> Void

    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 8) {
            Image(systemName: "exclamationmark.triangle.fill").foregroundStyle(.red)
            Text(problem.code == "usage" && oldBinary
                 ? "This stormo cannot edit agents (it has no `config` command). Update it, or choose the bundled one in Settings › Command line."
                 : problem.message)
                .textSelection(.enabled)
                .frame(maxWidth: .infinity, alignment: .leading)
            if stale {
                Button("Reload", action: reload)
                    .help("Load the version on disk; your unsaved changes to this file are dropped")
            }
        }
        .font(.callout)
        .padding(.horizontal, 14)
        .padding(.vertical, 10)
        .background(.red.opacity(0.08))
    }
}

private struct NoticeBanner: View {
    let text: String

    var body: some View {
        Label(text, systemImage: "info.circle")
            .font(.callout)
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(.horizontal, 14)
            .padding(.vertical, 10)
            .background(.yellow.opacity(0.1))
    }
}

private struct TextPane: View {
    @Bindable var editor: ConfigEditor
    let wraps: Bool
    let editable: Bool

    var body: some View {
        PlainTextEditor(text: $editor.text, editable: editable && editor.file != nil, wraps: wraps)
    }
}

/// Monospaced plain text with every automatic substitution off: smart quotes or dashes would
/// change what YAML means. Without wrapping, long lines scroll sideways (a wrapped YAML comment
/// reads like a key).
struct PlainTextEditor: NSViewRepresentable {
    @Binding var text: String
    var editable = true
    var wraps = true

    func makeNSView(context: Context) -> NSScrollView {
        let scroll = NSTextView.scrollableTextView()
        let view = scroll.documentView as! NSTextView
        view.isRichText = false
        view.allowsUndo = true
        view.usesFindBar = true
        view.isIncrementalSearchingEnabled = true
        view.font = .monospacedSystemFont(ofSize: NSFont.systemFontSize, weight: .regular)
        view.textContainerInset = NSSize(width: 10, height: 12)
        view.isAutomaticQuoteSubstitutionEnabled = false
        view.isAutomaticDashSubstitutionEnabled = false
        view.isAutomaticTextReplacementEnabled = false
        view.isAutomaticSpellingCorrectionEnabled = false
        view.isContinuousSpellCheckingEnabled = false
        view.isAutomaticLinkDetectionEnabled = false
        view.isAutomaticDataDetectionEnabled = false
        view.smartInsertDeleteEnabled = false
        if !wraps {
            scroll.hasHorizontalScroller = true
            view.isHorizontallyResizable = true
            view.maxSize = NSSize(width: CGFloat.greatestFiniteMagnitude, height: CGFloat.greatestFiniteMagnitude)
            view.textContainer?.widthTracksTextView = false
            view.textContainer?.containerSize = NSSize(width: CGFloat.greatestFiniteMagnitude, height: CGFloat.greatestFiniteMagnitude)
        }
        view.string = text
        view.delegate = context.coordinator
        return scroll
    }

    func updateNSView(_ scroll: NSScrollView, context: Context) {
        context.coordinator.text = $text
        guard let view = scroll.documentView as? NSTextView else { return }
        view.isEditable = editable
        if view.string != text {
            view.string = text
        }
    }

    func makeCoordinator() -> Coordinator { Coordinator(text: $text) }

    final class Coordinator: NSObject, NSTextViewDelegate {
        var text: Binding<String>
        init(text: Binding<String>) { self.text = text }

        func textDidChange(_ notification: Notification) {
            guard let view = notification.object as? NSTextView else { return }
            text.wrappedValue = view.string
        }
    }
}
