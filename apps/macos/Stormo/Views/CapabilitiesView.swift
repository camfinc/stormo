import StormoKit
import SwiftUI

/// The heavy or shared calls the core runs for agents (stormo.yaml capabilities:): declared from a
/// bundled template, set up, granted to agents, woken or put to sleep, and their jobs followed and
/// cancelled. Declaring and changing edit stormo.yaml (`stormo capability set|remove`, checked whole
/// before it is written); the core loads that at its next restart, which the page offers. Grants
/// edit each agent.yaml and need no restart.
struct CapabilitiesSection: View {
    @Environment(AppModel.self) private var model
    @State private var view: CapabilitiesView?
    @State private var file: CapabilitiesFile?
    @State private var jobs: [CapabilityJob] = []
    @State private var problem: String?
    @State private var adding = false
    @State private var editing: EditTarget?
    @State private var openJob: JobTarget?
    @State private var busy: Set<String> = []
    @State private var restarting = false

    struct EditTarget: Identifiable { let id: String }
    struct JobTarget: Identifiable { let id: Int64 }

    /// What stormo.yaml declares and what the core runs, one row each.
    private var names: [String] {
        let core = view?.capabilities.map(\.name) ?? []
        let declared = file?.capabilities.map(\.name) ?? []
        return Array(Set(core + declared)).sorted()
    }

    private var pending: [String] {
        guard let file, let view else { return [] }
        return file.pendingRestart(core: view)
    }

    var body: some View {
        Group {
            if !model.core.state.isRunning {
                ContentUnavailableView("Core stopped", systemImage: "server.rack", description: Text("Start the core to see its capabilities."))
            } else if view == nil, let problem {
                ContentUnavailableView("Can't load capabilities", systemImage: "exclamationmark.triangle", description: Text(problem))
            } else if view == nil {
                ProgressView()
            } else if names.isEmpty {
                ContentUnavailableView {
                    Label("No capabilities", systemImage: "shippingbox")
                } description: {
                    Text("Capabilities are heavy work the core runs for agents (transcription, a browser, video renders…) in workers that sleep when idle.")
                } actions: {
                    Button("Add Capability…") { adding = true }
                }
            } else {
                content
            }
        }
        .navigationTitle("Capabilities")
        .toolbar {
            ToolbarItem {
                Button("Add Capability…", systemImage: "plus") { adding = true }
                    .disabled(model.cli == nil || file == nil)
            }
        }
        .sheet(isPresented: $adding, onDismiss: { Task { await load() } }) {
            AddCapabilitySheet(taken: Set(names))
        }
        .sheet(item: $editing, onDismiss: { Task { await load() } }) { t in
            CapabilityEditorSheet(name: t.id)
        }
        .sheet(item: $openJob, onDismiss: { Task { await load() } }) { t in
            CapabilityJobSheet(id: t.id)
        }
        .task {
            // A visual check of the sheets (with STORMO_SECTION=capabilities and STORMO_SNAPSHOT).
            let env = ProcessInfo.processInfo.environment
            if let name = env["STORMO_EDIT_CAPABILITY"] { editing = EditTarget(id: name) }
            if let job = env["STORMO_CAPABILITY_JOB"].flatMap(Int64.init) { openJob = JobTarget(id: job) }
            if env["STORMO_ADD_CAPABILITY"] != nil { adding = true }
        }
        .task(id: model.core.state.isRunning) {
            while model.core.state.isRunning && !Task.isCancelled {
                await load()
                try? await Task.sleep(for: .seconds(5))
            }
        }
    }

    private var content: some View {
        Form {
            if let problem { Section { Label(problem, systemImage: "exclamationmark.triangle.fill").foregroundStyle(.red) } }
            if !pending.isEmpty {
                Section {
                    HStack(alignment: .firstTextBaseline) {
                        Label("stormo.yaml changed \(pending.joined(separator: ", ")). The core loads it when it restarts.",
                              systemImage: "arrow.clockwise.circle")
                        Spacer()
                        Button(restarting ? "Restarting…" : "Restart Core") {
                            Task {
                                restarting = true
                                await model.restartCore()
                                restarting = false
                                await load()
                            }
                        }
                        .disabled(restarting)
                    }
                } footer: {
                    Text("Restarting the core briefly pauses every agent's model calls; running capability jobs fail and their workers go to sleep.")
                }
            }
            Section("Capabilities") {
                ForEach(names, id: \.self) { name in row(name) }
            }
            Section("This month") {
                if view?.spend.isEmpty ?? true {
                    Text("No jobs yet").foregroundStyle(.secondary)
                }
                ForEach(view?.spend ?? []) { s in
                    LabeledContent("\(s.agent) · \(s.capability)") {
                        Text("\(s.jobs) jobs\(s.failed > 0 ? " (\(s.failed) failed)" : "") · \(s.usd, format: .currency(code: "USD"))")
                            .monospacedDigit()
                    }
                }
            }
            Section("Latest jobs") {
                if jobs.isEmpty { Text("No jobs yet").foregroundStyle(.secondary) }
                ForEach(jobs) { j in
                    Button { openJob = JobTarget(id: j.job) } label: { JobRow(job: j) }
                        .buttonStyle(.plain)
                }
            }
        }
        .formStyle(.grouped)
    }

    @ViewBuilder
    private func row(_ name: String) -> some View {
        let c = view?.capabilities.first { $0.name == name }
        let d = file?.capabilities.first { $0.name == name }
        HStack(alignment: .top, spacing: 10) {
            VStack(alignment: .leading, spacing: 4) {
                HStack(spacing: 8) {
                    Text(name).fontWeight(.medium)
                    Text(kindLabel(c, d)).font(.caption).foregroundStyle(.secondary)
                    WorkerBadge(capability: c, declared: d)
                    if let c, c.running + c.queued > 0 {
                        Text("\(c.running) running · \(c.queued) queued").font(.caption.monospacedDigit())
                    }
                }
                let description = c?.description ?? d?.effective.description ?? ""
                if !description.isEmpty { Text(description).font(.callout).foregroundStyle(.secondary).lineLimit(2) }
                let agents = d?.agents ?? c?.agents ?? []
                Text(agents.isEmpty ? "Granted to no agent" : "Granted to " + agents.joined(separator: ", "))
                    .font(.caption).foregroundStyle(.secondary)
                if let e = c?.service?.error, !e.isEmpty {
                    Text("Last boot failed: \(e)").font(.caption).foregroundStyle(.red).lineLimit(2)
                }
            }
            Spacer()
            if let c, c.isLocalService {
                let awake = c.worker == "awake" || c.worker == "booting"
                Button(awake ? "Sleep" : "Wake") { Task { await setAwake(name, !awake) } }
                    .disabled(busy.contains(name) || c.worker == "booting" || (awake && c.running > 0))
                    .help(awake && c.running > 0 ? "Jobs run in it: it sleeps once idle." : awake ? "Stop the worker now." : "Boot the worker now, ahead of jobs.")
            }
            Button("Edit…") { editing = EditTarget(id: name) }
                .disabled(d == nil)
                .help(d == nil ? "Not in stormo.yaml any more: restart the core." : "Settings, grants and removal.")
        }
        .padding(.vertical, 2)
    }

    private func kindLabel(_ c: CapabilityInfo?, _ d: DeclaredCapability?) -> String {
        if let t = c?.template ?? d?.effective.template { return "template \(t)" }
        return c?.kind ?? d?.effective.kind ?? ""
    }

    private func setAwake(_ name: String, _ awake: Bool) async {
        busy.insert(name)
        defer { busy.remove(name) }
        do {
            _ = try await model.core.client.setCapabilityAwake(name, awake)
            problem = nil
        } catch {
            problem = "\(awake ? "Wake" : "Sleep") \(name): \(describe(error))"
        }
        await load()
    }

    private func load() async {
        do {
            view = try await model.core.client.capabilities()
            jobs = try await model.core.client.capabilityJobs()
            if let cli = model.cli { file = try await cli.capabilitiesFile() }
            problem = nil
        } catch CoreClient.Failure.notFound {
            problem = "This core has no capabilities API: restart it with the app's stormo."
        } catch {
            problem = describe(error)
        }
    }
}

/// asleep / booting / awake for a service here, "runner" for one elsewhere, and why one cannot run.
private struct WorkerBadge: View {
    let capability: CapabilityInfo?
    let declared: DeclaredCapability?

    var body: some View {
        if let why = capability?.unavailable ?? declared?.effective.unavailable {
            badge("cannot run", .red).help(why)
        } else if capability == nil {
            badge("not loaded", .orange).help("In stormo.yaml; the core loads it when it restarts.")
        } else if let w = capability?.worker {
            switch w {
            case "awake": badge("awake", .green)
            case "booting": badge("booting", .orange)
            case "runner": badge("on a runner", .blue)
            default: badge("asleep", .secondary)
            }
        }
    }

    private func badge(_ text: String, _ color: Color) -> some View {
        Text(text).font(.caption2).padding(.horizontal, 6).padding(.vertical, 1)
            .background(Capsule().fill(color.opacity(0.18)))
    }
}

private struct JobRow: View {
    let job: CapabilityJob

    var body: some View {
        HStack(alignment: .firstTextBaseline) {
            Text("#\(job.job)").monospacedDigit().foregroundStyle(.secondary)
            VStack(alignment: .leading, spacing: 2) {
                Text("\(job.agent) · \(job.capability)")
                if let p = job.preview, !p.isEmpty { Text(p).font(.caption.monospaced()).foregroundStyle(.secondary).lineLimit(1) }
                if let e = job.error, !e.isEmpty { Text(e).font(.caption).foregroundStyle(.red).lineLimit(2) }
            }
            Spacer()
            Text(job.status).foregroundStyle(job.status == "failed" ? Color.red : Color.secondary)
            Image(systemName: "chevron.right").font(.caption).foregroundStyle(.tertiary)
        }
        .contentShape(Rectangle())
    }
}

// MARK: Add

/// A capability from a bundled template: its name and template go into stormo.yaml.
struct AddCapabilitySheet: View {
    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss
    let taken: Set<String>
    @State private var templates: [CapabilityTemplate] = []
    @State private var template = ""
    @State private var name = ""
    @State private var busy = false
    @State private var failure: String?

    private var chosen: CapabilityTemplate? { templates.first { $0.name == template } }
    private var effectiveName: String { name.isEmpty ? (chosen?.suggestedName ?? "") : name }
    private var nameProblem: String? {
        let n = effectiveName
        if n.isEmpty { return nil }
        if taken.contains(n) { return "\(n) is already declared." }
        if n.range(of: #"^[a-z][a-z0-9-]*(\.[a-z][a-z0-9-]*)*$"#, options: .regularExpression) == nil {
            return "Lowercase words joined by dots, like media.render."
        }
        return nil
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            Text("Add a Capability").font(.title3).fontWeight(.semibold)
            Form {
                Picker("Template", selection: $template) {
                    ForEach(templates) { Text($0.name).tag($0.name) }
                }
                TextField("Name", text: $name, prompt: Text(chosen?.suggestedName ?? "area.verb"))
                if let p = nameProblem { Text(p).font(.caption).foregroundStyle(.red) }
                if let t = chosen {
                    Text(t.description).font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                    if let notes = t.notes, !notes.isEmpty {
                        Label(notes, systemImage: "info.circle").font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                    }
                }
            }
            .formStyle(.grouped)
            Text("It goes into stormo.yaml with the template's settings (change them after with Edit…). The core runs it after its next restart; grant it to the agents that should use it.")
                .font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            if let failure {
                Label(failure, systemImage: "xmark.octagon.fill").foregroundStyle(.red).textSelection(.enabled)
            }
            HStack {
                Spacer()
                Button("Cancel") { dismiss() }.keyboardShortcut(.cancelAction)
                Button("Add") { Task { await add() } }
                    .keyboardShortcut(.defaultAction)
                    .disabled(busy || chosen == nil || effectiveName.isEmpty || nameProblem != nil)
            }
        }
        .padding(20)
        .frame(width: 520)
        .task(id: model.cli != nil) {
            guard templates.isEmpty, let cli = model.cli else { return }
            do {
                templates = try await cli.capabilityTemplates()
                template = templates.first { !taken.contains($0.suggestedName) }?.name ?? templates.first?.name ?? ""
            } catch {
                failure = error.localizedDescription
            }
        }
    }

    private func add() async {
        guard let cli = model.cli, let t = chosen else { return }
        busy = true
        defer { busy = false }
        failure = nil
        do {
            let f = try await cli.capabilitiesFile()
            _ = try await cli.setCapability(effectiveName, patch: ["template": .string(t.name)], ifHash: f.hash)
            dismiss()
        } catch {
            failure = error.localizedDescription
        }
    }
}

// MARK: Edit

/// One capability's settings (stormo.yaml), its grants (each agent's agent.yaml) and its removal.
struct CapabilityEditorSheet: View {
    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss
    let name: String
    @State private var file: CapabilitiesFile?
    @State private var description = ""
    @State private var concurrency = 1
    @State private var timeout = 15
    @State private var idle = 10
    @State private var memory = ""
    @State private var cost = ""
    @State private var env: [EnvRow] = []
    @State private var grantBusy: Set<String> = []
    @State private var busy = false
    @State private var failure: String?
    @State private var confirmRemove = false

    struct EnvRow: Identifiable, Equatable {
        let id = UUID()
        var key: String
        var value: String
    }

    private var declared: DeclaredCapability? { file?.capabilities.first { $0.name == name } }
    private var isService: Bool { declared?.effective.kind == "service" }
    private var agents: [String] { (model.fleet.fleet?.agents.map(\.id) ?? []).sorted() }

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            HStack {
                Text(name).font(.title3).fontWeight(.semibold)
                if let t = declared?.effective.template { Text("template \(t)").foregroundStyle(.secondary) }
            }
            if let d = declared {
                Form {
                    Section("Settings") {
                        TextField("Description", text: $description, prompt: Text("what agents read about it"), axis: .vertical).lineLimit(1...4)
                        Stepper("Jobs at once: \(concurrency)", value: $concurrency, in: 1...64)
                        Stepper("Minutes a job may take: \(timeout)", value: $timeout, in: 1...1440)
                        if isService {
                            Stepper("Asleep after \(idle) idle minutes", value: $idle, in: 1...1440)
                            TextField("Memory (MB)", text: $memory, prompt: Text("no limit"))
                        }
                        TextField("Estimated cost per job (USD)", text: $cost, prompt: Text("0"))
                    }
                    if isService {
                        Section {
                            ForEach($env) { $row in
                                HStack {
                                    TextField("NAME", text: $row.key).monospaced().frame(maxWidth: 200)
                                    TextField("value", text: $row.value)
                                    Button("Remove", systemImage: "minus.circle") { env.removeAll { $0.id == row.id } }
                                        .labelStyle(.iconOnly).buttonStyle(.borderless)
                                }
                            }
                            Button("Add Setting", systemImage: "plus") { env.append(EnvRow(key: "", value: "")) }
                        } header: {
                            SectionTitle("Worker settings", note: "Plain values the worker reads from its environment; secrets belong in the instance's secrets, not here.")
                        }
                    }
                    Section {
                        if agents.isEmpty { Text("No agents in this instance.").foregroundStyle(.secondary) }
                        ForEach(agents, id: \.self) { a in
                            Toggle(isOn: Binding(get: { d.agents.contains(a) }, set: { on in Task { await grant(a, on) } })) {
                                Text(a)
                            }
                            .disabled(grantBusy.contains(a) || d.effective.unavailable != nil)
                        }
                    } header: {
                        SectionTitle("Granted to", note: "Saved at once in each agent's agent.yaml; agents see it on their next call, no restart.")
                    }
                    if let docs = d.effective.docs, !docs.isEmpty {
                        Section("What agents read (cap_describe)") {
                            Text(docs).font(.callout).foregroundStyle(.secondary).textSelection(.enabled)
                        }
                    }
                }
                .formStyle(.grouped)
                .frame(minHeight: 420)
            } else if failure == nil {
                ProgressView().frame(maxWidth: .infinity, minHeight: 200)
            }
            if let failure {
                Label(failure, systemImage: "xmark.octagon.fill").foregroundStyle(.red).textSelection(.enabled)
            }
            HStack {
                Button("Remove…", role: .destructive) { confirmRemove = true }
                    .disabled(busy || declared == nil || !(declared?.agents.isEmpty ?? true))
                    .help(declared?.agents.isEmpty ?? true ? "Take it out of stormo.yaml." : "Take it from every agent first.")
                Spacer()
                Button("Cancel") { dismiss() }.keyboardShortcut(.cancelAction)
                Button("Save") { Task { await save() } }
                    .keyboardShortcut(.defaultAction)
                    .disabled(busy || declared == nil || patch.isEmpty)
            }
        }
        .padding(20)
        .frame(width: 560)
        .confirmationDialog("Remove \(name) from stormo.yaml?", isPresented: $confirmRemove) {
            Button("Remove", role: .destructive) { Task { await remove() } }
        } message: {
            Text("Its jobs stop being accepted once the core restarts. Its worker's cache volume stays.")
        }
        .task(id: model.cli != nil) { await load() }
    }

    /// The changes as a merge patch on the stanza (only what differs from what runs now).
    private var patch: [String: JSONValue] {
        guard let e = declared?.effective else { return [:] }
        var p: [String: JSONValue] = [:]
        if description != e.description { p["description"] = .string(description) }
        if concurrency != e.concurrency { p["concurrency"] = .int(Int64(concurrency)) }
        if timeout != e.timeoutMinutes { p["timeout_minutes"] = .int(Int64(timeout)) }
        if isService {
            if idle != (e.idleMinutes ?? 10) { p["idle_minutes"] = .int(Int64(idle)) }
            let mem = Int(memory.trimmingCharacters(in: .whitespaces)) ?? 0
            if mem != (e.memoryMB ?? 0) { p["memory_mb"] = mem == 0 ? .null : .int(Int64(mem)) }
            let now = Dictionary(env.filter { !$0.key.isEmpty }.map { ($0.key, $0.value) }, uniquingKeysWith: { _, b in b })
            if let env = CapabilityInfo.envPatch(from: e.env, to: now) { p["env"] = env }
        }
        let c = Double(cost.trimmingCharacters(in: .whitespaces).replacingOccurrences(of: ",", with: ".")) ?? 0
        if c != e.costUSD { p["cost_usd"] = .double(c) }
        return p
    }

    private func load() async {
        guard let cli = model.cli else { return }
        do {
            file = try await cli.capabilitiesFile()
            guard let e = declared?.effective else {
                failure = "\(name) is not in stormo.yaml."
                return
            }
            description = e.description
            concurrency = e.concurrency
            timeout = e.timeoutMinutes
            idle = e.idleMinutes ?? 10
            memory = e.memoryMB.map(String.init) ?? ""
            cost = e.costUSD == 0 ? "" : String(e.costUSD)
            env = (e.env ?? [:]).sorted { $0.key < $1.key }.map { EnvRow(key: $0.key, value: $0.value) }
            failure = nil
        } catch {
            failure = error.localizedDescription
        }
    }

    private func save() async {
        guard let cli = model.cli, let f = file else { return }
        busy = true
        defer { busy = false }
        do {
            file = try await cli.setCapability(name, patch: patch, ifHash: f.hash)
            dismiss()
        } catch {
            failure = error.localizedDescription
        }
    }

    private func grant(_ agent: String, _ on: Bool) async {
        guard let cli = model.cli else { return }
        grantBusy.insert(agent)
        defer { grantBusy.remove(agent) }
        do {
            try await cli.setCapabilityGrant(name, agent: agent, granted: on)
            file = try await cli.capabilitiesFile()
            failure = nil
        } catch {
            failure = "\(on ? "Granting" : "Taking") \(name) \(on ? "to" : "from") \(agent): \(error.localizedDescription)"
        }
    }

    private func remove() async {
        guard let cli = model.cli, let f = file else { return }
        busy = true
        defer { busy = false }
        do {
            _ = try await cli.removeCapability(name, ifHash: f.hash)
            dismiss()
        } catch {
            failure = error.localizedDescription
        }
    }
}

// MARK: A job

/// One job: who asked, when, what it took and gave (its output scrubbed by the core), and Cancel.
struct CapabilityJobSheet: View {
    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss
    let id: Int64
    @State private var job: CapabilityJob?
    @State private var failure: String?
    @State private var busy = false

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            Text("Job #\(id)").font(.title3).fontWeight(.semibold)
            if let j = job {
                Form {
                    Section {
                        LabeledContent("Capability", value: j.capability)
                        LabeledContent("Agent", value: j.agent)
                        LabeledContent("Status") {
                            Text(j.status).foregroundStyle(j.status == "failed" ? Color.red : Color.primary)
                        }
                        LabeledContent("Started", value: time(j.started ?? j.created))
                        if let f = j.finished { LabeledContent("Finished", value: time(f) + " (" + duration(from: j.started ?? j.created, to: f) + ")") }
                        if let e = j.error, !e.isEmpty { Text(e).foregroundStyle(.red).textSelection(.enabled) }
                    }
                    Section("Input") {
                        if let p = j.preview, !p.isEmpty { Text(p).font(.callout.monospaced()).textSelection(.enabled) }
                        paths("Shared files", j.inputs)
                        paths("Uploaded files", j.uploads.map { $0.map { "\($0) (uploaded)" } })
                    }
                    Section("Output") {
                        if let out = j.out, !out.isEmpty { LabeledContent("Folder", value: out).textSelection(.enabled) }
                        paths("Files written", j.files)
                        if let o = j.output {
                            ScrollView { Text(pretty(o)).font(.caption.monospaced()).textSelection(.enabled).frame(maxWidth: .infinity, alignment: .leading) }
                                .frame(maxHeight: 220)
                        } else if j.status == "done" {
                            Text("No answer kept.").foregroundStyle(.secondary)
                        }
                        if let n = j.note { Text(n).font(.caption).foregroundStyle(.secondary) }
                    }
                }
                .formStyle(.grouped)
                .frame(minHeight: 380)
            } else if failure == nil {
                ProgressView().frame(maxWidth: .infinity, minHeight: 200)
            }
            if let failure {
                Label(failure, systemImage: "xmark.octagon.fill").foregroundStyle(.red).textSelection(.enabled)
            }
            HStack {
                if job?.isCancellable == true {
                    Button("Cancel Job", role: .destructive) { Task { await cancel() } }.disabled(busy)
                        .help("A waiting or queued job never runs; a running one stops. Its agent is told.")
                }
                Spacer()
                Button("Done") { dismiss() }.keyboardShortcut(.defaultAction)
            }
        }
        .padding(20)
        .frame(width: 600)
        .task {
            while !Task.isCancelled {
                await load()
                if let job, !job.isCancellable { break }
                try? await Task.sleep(for: .seconds(3))
            }
        }
    }

    @ViewBuilder
    private func paths(_ label: String, _ list: [String]?) -> some View {
        if let list, !list.isEmpty {
            LabeledContent(label) {
                VStack(alignment: .trailing) { ForEach(list, id: \.self) { Text($0).font(.caption.monospaced()) } }.textSelection(.enabled)
            }
        }
    }

    private func load() async {
        do {
            job = try await model.core.client.capabilityJob(id)
            failure = nil
        } catch {
            failure = describe(error, missing: "No job #\(id) on this core, or the core predates job details: restart it with the app's stormo.")
        }
    }

    private func cancel() async {
        busy = true
        defer { busy = false }
        do {
            job = try await model.core.client.cancelCapabilityJob(id)
        } catch {
            failure = describe(error, missing: "This core cannot cancel jobs yet: restart it with the app's stormo.")
        }
    }

    private func time(_ ms: Int64) -> String {
        Date(timeIntervalSince1970: Double(ms) / 1000).formatted(date: .abbreviated, time: .standard)
    }

    private func duration(from a: Int64, to b: Int64) -> String {
        Duration.milliseconds(max(0, b - a)).formatted(.units(allowed: [.hours, .minutes, .seconds], width: .abbreviated))
    }

    private func pretty(_ v: JSONValue) -> String {
        let enc = JSONEncoder()
        enc.outputFormatting = [.prettyPrinted, .sortedKeys, .withoutEscapingSlashes]
        guard let data = try? enc.encode(v), let s = String(data: data, encoding: .utf8) else { return "" }
        return s
    }
}

/// A failed call as the owner reads it: the core's own explanation when it gave one.
private func describe(_ error: Error, missing: String = "This core predates it: restart it with the app's stormo.") -> String {
    switch error {
    case CoreClient.Failure.refused(_, let message): message
    case CoreClient.Failure.notFound: missing
    case CoreClient.Failure.unreachable: "The core does not answer."
    case CoreClient.Failure.status(let code): "The core answered HTTP \(code)."
    default: error.localizedDescription
    }
}
