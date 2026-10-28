import StormoKit
import SwiftUI

/// The Units section: the instance's units, each with its settings (unit.yaml as a form), its
/// knowledge (the Markdown files compiled into its agents) and unit.yaml as text. Everything goes
/// through `stormo config` (docs/api.md), so it works with the core stopped; after a save the
/// agents the unit reaches are checked, and running ones pick the change up on restart.
struct UnitsView: View {
    @Environment(AppModel.self) private var model
    @State private var catalog = UnitCatalog()
    @State private var selection: String?
    /// The open unit has unsaved edits: switching units waits until they are saved or reverted.
    @State private var dirty = false
    @State private var blockedSwitch = false
    @State private var creating = false

    var body: some View {
        Group {
            if model.cli == nil {
                ContentUnavailableView("No stormo command", systemImage: "terminal",
                                       description: Text("Choose one in Settings › Command line to edit units."))
            } else if !catalog.loaded {
                ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity)
            } else if catalog.units.isEmpty, let p = catalog.problem {
                ContentUnavailableView("Units cannot be listed", systemImage: "exclamationmark.triangle",
                                       description: Text(p.code == "usage"
                                           ? "This stormo cannot edit units (its `config` command is older than this app). Update it, or choose the bundled one in Settings › Command line."
                                           : p.message))
            } else {
                HSplitView {
                    list.frame(minWidth: 190, idealWidth: 220, maxWidth: 300, maxHeight: .infinity)
                    detail.frame(minWidth: 420, maxWidth: .infinity, maxHeight: .infinity)
                }
                .frame(maxWidth: .infinity, maxHeight: .infinity)
            }
        }
        .navigationTitle("Units")
        .toolbar {
            ToolbarItem(placement: .navigation) {
                Button("New Unit…", systemImage: "plus") { creating = true }
                    .help("Add a unit: a team with its own shared documents and knowledge")
                    .disabled(model.cli == nil || dirty)
            }
        }
        .task(id: model.cli?.instance) {
            guard let cli = model.cli else { return }
            await catalog.load(with: cli)
            if selection.flatMap(catalog.unit) == nil { selection = catalog.units.first(where: { $0.id != "group" })?.id ?? catalog.units.first?.id }
        }
        .sheet(isPresented: $creating) {
            NewUnitSheet(catalog: catalog) { id in selection = id }
        }
        .alert("Save or revert first", isPresented: $blockedSwitch) {
            Button("OK", role: .cancel) {}
        } message: {
            Text("This unit has unsaved changes.")
        }
    }

    private var list: some View {
        List(selection: Binding(get: { selection }, set: { id in
            guard id != selection else { return }
            if dirty { blockedSwitch = true } else { selection = id }
        })) {
            ForEach(catalog.units) { u in
                VStack(alignment: .leading, spacing: 2) {
                    HStack(spacing: 4) {
                        Text(u.name).fontWeight(.medium).lineLimit(1)
                        if u.problem != nil {
                            Image(systemName: "exclamationmark.triangle.fill").foregroundStyle(.orange).help(u.problem ?? "")
                        }
                    }
                    Text(u.summary).font(.caption).foregroundStyle(.secondary).lineLimit(1)
                }
                .padding(.vertical, 2)
                .tag(Optional(u.id))
            }
        }
        .listStyle(.sidebar)
    }

    @ViewBuilder private var detail: some View {
        if let id = selection, let unit = catalog.unit(id) {
            UnitEditor(unit: unit, catalog: catalog, dirty: $dirty).id(id)
        } else {
            ContentUnavailableView("Select a unit", systemImage: "rectangle.3.group")
        }
    }
}

extension UnitInfo {
    var compiledKnowledge: Int { knowledge.filter(\.compiled).count }
    var summary: String {
        let who = id == "group" ? "every agent" : agents.count == 1 ? "1 agent" : "\(agents.count) agents"
        let files = compiledKnowledge == 1 ? "1 knowledge file" : "\(compiledKnowledge) knowledge files"
        return "\(id) · \(who) · \(files)"
    }
}

// MARK: Editor

private struct UnitEditor: View {
    enum Page: String, CaseIterable, Identifiable {
        case settings, knowledge, yaml
        var id: Self { self }
        var title: String {
            switch self {
            case .settings: "Settings"
            case .knowledge: "Knowledge"
            case .yaml: "unit.yaml"
            }
        }
    }

    enum Check: Equatable {
        case running
        case passed(Int)
        case failed(String)
        /// Saved a file no agent reads (README.md).
        case notCompiled
    }

    @Environment(AppModel.self) private var model
    let unit: UnitInfo
    let catalog: UnitCatalog
    @Binding var dirty: Bool
    @State private var page = Page.settings
    @State private var definition: ConfigEditor
    /// The knowledge file open on the Knowledge page.
    @State private var document: ConfigEditor?
    @State private var check: Check?
    @State private var newFile = false
    @State private var confirmDelete = false
    @State private var blockedSwitch = false

    init(unit: UnitInfo, catalog: UnitCatalog, dirty: Binding<Bool>) {
        self.unit = unit
        self.catalog = catalog
        _dirty = dirty
        _definition = State(initialValue: ConfigEditor(path: unit.path))
        #if DEBUG
        // STORMO_UNITS_PAGE=<page> opens on that page (with STORMO_SECTION=units and STORMO_SNAPSHOT, a visual check).
        if let p = ProcessInfo.processInfo.environment["STORMO_UNITS_PAGE"].flatMap(Page.init(rawValue:)) { _page = State(initialValue: p) }
        #endif
    }

    private var editor: ConfigEditor? { page == .knowledge ? document : definition }
    private var anyDirty: Bool { definition.isDirty || document?.isDirty == true }

    var body: some View {
        VStack(spacing: 0) {
            if let editor, let problem = editor.problem {
                ConfigProblemBanner(problem: problem, stale: editor.isStale, oldBinary: editor.file == nil) {
                    Task { await reload(editor) }
                }
            } else if page == .yaml && definition.isFormDirty {
                NoticeBanner(text: "The settings have unsaved changes. Save or revert them to edit the file as text.")
            } else if page == .settings && definition.isTextDirty {
                NoticeBanner(text: "unit.yaml has unsaved text changes. Save or revert them to use the form.")
            }
            content
            if let check, editor?.isDirty != true {
                Divider()
                checkBar(check)
                    .padding(.horizontal, 14)
                    .padding(.vertical, 8)
            }
        }
        .navigationSubtitle(subtitle)
        .toolbar {
            ToolbarItem(placement: .principal) {
                Picker("Page", selection: $page) {
                    ForEach(Page.allCases) { p in
                        Text(p.title + (pageDirty(p) ? " •" : "")).tag(p)
                    }
                }
                .pickerStyle(.segmented)
                .labelsHidden()
            }
            ToolbarItemGroup(placement: .primaryAction) {
                Button("Revert", systemImage: "arrow.uturn.backward") { editor?.revert() }
                    .help("Drop the unsaved changes")
                    .disabled(editor?.isDirty != true)
                Button("Save", systemImage: "checkmark") { Task { await save() } }
                    .keyboardShortcut("s")
                    .help("Save through stormo, which validates it first")
                    .disabled(editor?.isDirty != true || editor?.busy == true)
            }
        }
        .task(id: model.cli?.instance) {
            guard let cli = model.cli else { return }
            await definition.load(with: cli)
            if document == nil, let first = unit.knowledge.first(where: \.compiled) ?? unit.knowledge.first {
                await open(first.path)
            }
        }
        .onChange(of: anyDirty, initial: true) { dirty = anyDirty }
        .sheet(isPresented: $newFile) {
            NewKnowledgeSheet(unit: unit, catalog: catalog) { path in Task { await open(path) } }
        }
        .confirmationDialog("Delete \(document?.path.split(separator: "/").last.map(String.init) ?? "this file")?",
                            isPresented: $confirmDelete) {
            Button("Delete", role: .destructive) { Task { await deleteDocument() } }
        } message: {
            Text("The file is removed from the instance folder (git keeps its history). Agents lose it at their next check and restart.")
        }
        .alert("Save or revert first", isPresented: $blockedSwitch) {
            Button("OK", role: .cancel) {}
        } message: {
            Text("The open knowledge file has unsaved changes.")
        }
    }

    private var subtitle: String {
        switch page {
        case .knowledge: document?.path ?? "units/\(unit.id)/knowledge/"
        default: unit.path
        }
    }

    private func pageDirty(_ p: Page) -> Bool {
        switch p {
        case .settings: definition.isFormDirty
        case .knowledge: document?.isDirty == true
        case .yaml: definition.isTextDirty
        }
    }

    @ViewBuilder private var content: some View {
        switch page {
        case .settings:
            if definition.doc != nil {
                UnitForm(editor: definition, unit: unit, knowledge: { page = .knowledge })
                    .disabled(definition.isTextDirty || definition.busy || definition.isStale)
            } else if definition.file != nil {
                ContentUnavailableView("unit.yaml does not parse", systemImage: "exclamationmark.triangle",
                                       description: Text("Fix it in the unit.yaml tab; the form comes back once it parses."))
            } else if definition.problem == nil {
                ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity)
            } else {
                Spacer()
            }
        case .knowledge:
            knowledge
        case .yaml:
            TextPane(editor: definition, wraps: false, editable: !definition.isFormDirty && !definition.isStale)
        }
    }

    // MARK: Knowledge

    private var knowledge: some View {
        HSplitView {
            VStack(spacing: 0) {
                List(selection: Binding(get: { document?.path }, set: { path in
                    guard let path, path != document?.path else { return }
                    if document?.isDirty == true { blockedSwitch = true } else { Task { await open(path) } }
                })) {
                    ForEach(unit.knowledge) { k in
                        VStack(alignment: .leading, spacing: 1) {
                            Text(k.name).lineLimit(1)
                            Text(k.compiled ? ByteCountFormatter.string(fromByteCount: Int64(k.size), countStyle: .file) : "For people · not given to agents")
                                .font(.caption).foregroundStyle(.secondary)
                        }
                        .tag(Optional(k.path))
                    }
                }
                .listStyle(.sidebar)
                Divider()
                HStack(spacing: 2) {
                    Button { newFile = true } label: { Image(systemName: "plus").frame(width: 20, height: 18) }
                        .help("New knowledge file")
                    Button { confirmDelete = true } label: { Image(systemName: "minus").frame(width: 20, height: 18) }
                        .help("Delete the selected file")
                        .disabled(document?.file == nil || document?.busy == true)
                    Spacer()
                }
                .buttonStyle(.borderless)
                .padding(6)
            }
            .frame(minWidth: 170, idealWidth: 200, maxWidth: 260, maxHeight: .infinity)

            Group {
                if let document {
                    VStack(spacing: 0) {
                        if unit.knowledge.first(where: { $0.path == document.path })?.compiled == false {
                            NoticeBanner(text: "README.md is for people: it is never compiled into the agents. Put what they should know in another file.")
                        }
                        TextPane(editor: document, wraps: true, editable: !document.isStale)
                    }
                } else {
                    ContentUnavailableView {
                        Label("No knowledge yet", systemImage: "book.closed")
                    } description: {
                        Text(unit.id == "group"
                             ? "Markdown files here are given to every agent: shared policy, tone, facts."
                             : "Markdown files here are given to every agent in \(unit.name): policy, tone, facts.")
                    } actions: {
                        Button("New File…") { newFile = true }
                    }
                }
            }
            .frame(minWidth: 320, maxWidth: .infinity, maxHeight: .infinity)
        }
    }

    private func open(_ path: String) async {
        guard let cli = model.cli else { return }
        let e = ConfigEditor(path: path)
        document = e
        await e.load(with: cli)
    }

    private func deleteDocument() async {
        guard let cli = model.cli, let document else { return }
        let compiled = unit.knowledge.first(where: { $0.path == document.path })?.compiled ?? true
        check = nil
        guard await document.delete(with: cli) else { return }
        self.document = nil
        await catalog.load(with: cli)
        if let next = catalog.unit(unit.id)?.knowledge.first { await open(next.path) }
        if compiled { await checkAgents() }
    }

    // MARK: Saving

    private func reload(_ editor: ConfigEditor) async {
        guard let cli = model.cli else { return }
        await editor.load(with: cli)
    }

    private func save() async {
        guard let cli = model.cli, let editor else { return }
        check = nil
        guard await editor.save(with: cli) else { return }
        await catalog.load(with: cli)
        if editor !== definition, unit.knowledge.first(where: { $0.path == editor.path })?.compiled == false {
            check = .notCompiled
            return
        }
        await checkAgents()
    }

    /// Builds the agents the unit reaches: its knowledge is compiled into them.
    private func checkAgents() async {
        guard let cli = model.cli else { return }
        let agents = catalog.unit(unit.id)?.agents ?? unit.agents
        guard !agents.isEmpty else {
            check = .passed(0)
            return
        }
        check = .running
        do {
            check = .passed(try await cli.run(["check"] + agents, as: [CheckedAgent].self).count)
        } catch {
            check = .failed(error.localizedDescription)
        }
        await model.fleet.refresh()
    }

    private var runningAgents: [String] {
        unit.agents.filter { model.fleet.agent($0)?.state == "running" }
    }

    @ViewBuilder private func checkBar(_ check: Check) -> some View {
        HStack(spacing: 8) {
            switch check {
            case .running:
                ProgressView().controlSize(.small)
                Text("Saved. Checking \(unit.agents.count == 1 ? "its agent" : "its \(unit.agents.count) agents")…").foregroundStyle(.secondary)
                Spacer()
            case .passed(let n):
                Image(systemName: "checkmark.circle.fill").foregroundStyle(.green)
                Text(n == 0 ? "Saved. No agent is in this unit yet." : "Saved and checked \(n == 1 ? "1 agent" : "\(n) agents").")
                Spacer()
                if !runningAgents.isEmpty {
                    Text("Restart to use it.").foregroundStyle(.secondary)
                    Button(runningAgents.count == 1 ? "Restart \(model.fleet.agent(runningAgents[0])?.name ?? runningAgents[0])" : "Restart \(runningAgents.count) Agents") {
                        Task { await model.run("restart", agents: runningAgents) }
                    }
                }
            case .failed(let message):
                Image(systemName: "exclamationmark.triangle.fill").foregroundStyle(.orange)
                Text("Saved, but the check failed: \(message)").textSelection(.enabled).lineLimit(3)
                Spacer()
            case .notCompiled:
                Image(systemName: "checkmark.circle.fill").foregroundStyle(.green)
                Text("Saved. README.md is not given to agents, so nothing to check.")
                Spacer()
            }
        }
        .font(.callout)
    }
}

// MARK: The form

/// unit.yaml as a form: its name and description. The id is the folder's name and stays; keys the
/// form does not show are kept.
private struct UnitForm: View {
    @Environment(AppModel.self) private var model
    @Bindable var editor: ConfigEditor
    let unit: UnitInfo
    let knowledge: () -> Void

    var body: some View {
        Form {
            Section {
                VStack(alignment: .leading, spacing: 2) {
                    Text(editor.doc?[["name"]]?.string ?? unit.name).font(.title2).fontWeight(.semibold)
                    Text("id \(unit.id) · the folder's name, fixed").font(.caption).foregroundStyle(.secondary)
                }
                .padding(.vertical, 4)
            }
            Section("Unit") {
                TextField("Name", text: text("name"))
                TextField("Description", text: text("description"), prompt: Text("What the unit does, in a line"), axis: .vertical)
                    .lineLimit(2...5)
            }
            Section {
                if unit.id == "group" {
                    Text("Every agent belongs to group as well as to its own unit.").foregroundStyle(.secondary)
                } else if unit.agents.isEmpty {
                    Text("No agent is in this unit yet. Move one here from its settings (Agents › Edit › Unit), or create one in it.")
                        .foregroundStyle(.secondary)
                } else {
                    ForEach(unit.agents, id: \.self) { id in
                        if let a = model.fleet.agent(id) {
                            HStack(spacing: 8) {
                                AgentBadge(agent: a, size: 22)
                                Text(a.name)
                                Spacer()
                                StateDot(agent: a)
                            }
                        } else {
                            Text(id)
                        }
                    }
                }
            } header: {
                SectionTitle("Agents", note: "They share the unit's documents (/shared/\(unit.id)), bridge actions and knowledge.")
            }
            Section {
                LabeledContent("Files") {
                    HStack {
                        Text(unit.compiledKnowledge == 1 ? "1 file" : "\(unit.compiledKnowledge) files").foregroundStyle(.secondary)
                        Button("Edit Knowledge…", action: knowledge)
                    }
                }
            } header: {
                SectionTitle("Knowledge", note: "Markdown compiled into each of these agents at its next check, with the lessons promoted to the unit.")
            }
        }
        .formStyle(.grouped)
    }

    private func text(_ key: JSONValue.Key) -> Binding<String> {
        Binding(get: { editor.doc?[[key]]?.string ?? "" },
                set: { editor.doc?.set([key], .string($0)) })
    }
}

// MARK: Sheets

private struct NewUnitSheet: View {
    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss
    let catalog: UnitCatalog
    let created: (String) -> Void
    @State private var name = ""
    @State private var customID = ""
    @State private var description = ""
    @State private var busy = false

    private var id: String { customID.isEmpty ? UnitCatalog.unitID(from: name) : customID }
    private var idProblem: String? {
        if id.isEmpty { return name.isEmpty ? nil : "The name needs a letter for the id." }
        if UnitCatalog.unitID(from: id) != id { return "Lowercase letters, digits and hyphens." }
        if catalog.unit(id) != nil { return "units/\(id) already exists." }
        return nil
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            Form {
                Section {
                    TextField("Name", text: $name, prompt: Text("e.g. Customer Care"))
                    TextField("Description", text: $description, prompt: Text("One line"), axis: .vertical)
                        .lineLimit(2...4)
                    TextField("ID", text: $customID, prompt: Text(UnitCatalog.unitID(from: name).isEmpty ? "from the name" : UnitCatalog.unitID(from: name)))
                } header: {
                    SectionTitle("New Unit", note: "A team of agents with its own shared documents, bridge actions and knowledge. The id names its folder and cannot change later.")
                } footer: {
                    if let idProblem {
                        Text(idProblem).foregroundStyle(.red).font(.caption)
                    } else if !id.isEmpty {
                        Text("Creates units/\(id)/unit.yaml.").font(.caption).foregroundStyle(.secondary)
                    }
                }
            }
            .formStyle(.grouped)
            if let p = catalog.problem {
                Text(p.message).foregroundStyle(.red).font(.callout).textSelection(.enabled).padding(.horizontal, 20)
            }
            HStack {
                Spacer()
                Button("Cancel", role: .cancel) { dismiss() }.keyboardShortcut(.cancelAction)
                Button("Create") { Task { await create() } }
                    .keyboardShortcut(.defaultAction)
                    .disabled(busy || id.isEmpty || idProblem != nil || name.trimmingCharacters(in: .whitespaces).isEmpty)
            }
            .padding(20)
        }
        .frame(width: 460)
        .onAppear { catalog.problemReset() }
    }

    private func create() async {
        guard let cli = model.cli else { return }
        busy = true
        defer { busy = false }
        if await catalog.createUnit(id: id, name: name.trimmingCharacters(in: .whitespaces),
                                    description: description.trimmingCharacters(in: .whitespacesAndNewlines), with: cli) != nil {
            created(id)
            dismiss()
        }
    }
}

private struct NewKnowledgeSheet: View {
    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss
    let unit: UnitInfo
    let catalog: UnitCatalog
    let created: (String) -> Void
    @State private var name = ""
    @State private var busy = false

    private var file: String { UnitCatalog.knowledgeName(from: name) }
    private var exists: Bool { unit.knowledge.contains { $0.name == file } }

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            Form {
                Section {
                    TextField("Title", text: $name, prompt: Text("e.g. Pricing rules"))
                } header: {
                    SectionTitle("New Knowledge File", note: "Each file becomes a section of what \(unit.id == "group" ? "every agent" : "the unit's agents") know.")
                } footer: {
                    if exists {
                        Text("\(file) already exists.").foregroundStyle(.red).font(.caption)
                    } else if !file.isEmpty {
                        Text("Creates units/\(unit.id)/knowledge/\(file).").font(.caption).foregroundStyle(.secondary)
                    }
                }
            }
            .formStyle(.grouped)
            if let p = catalog.problem {
                Text(p.message).foregroundStyle(.red).font(.callout).textSelection(.enabled).padding(.horizontal, 20)
            }
            HStack {
                Spacer()
                Button("Cancel", role: .cancel) { dismiss() }.keyboardShortcut(.cancelAction)
                Button("Create") { Task { await create() } }
                    .keyboardShortcut(.defaultAction)
                    .disabled(busy || file.isEmpty || exists)
            }
            .padding(20)
        }
        .frame(width: 420)
        .onAppear { catalog.problemReset() }
    }

    private func create() async {
        guard let cli = model.cli else { return }
        busy = true
        defer { busy = false }
        if let path = await catalog.createKnowledge(unit: unit.id, name: file, with: cli) {
            created(path)
            dismiss()
        }
    }
}
