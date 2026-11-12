import AppKit
import StormoKit
import SwiftUI

/// The instance's connections (stormo.yaml connections:): API providers with their keys, voice
/// providers, tools APIs (Apify, Comfy Cloud), and ChatGPT accounts signed in on the core. Agents
/// pick one in their form (Model › Provider, the core's account for local runs, and their Voice
/// page), or for a tools API declare its key under Secrets or Optional secrets. Everything goes through
/// `stormo connections`, `secrets set` and `core login|logout --connection`.
struct ProvidersView: View {
    @Environment(AppModel.self) private var model
    @State private var rows: [ConnectionRow] = []
    @State private var gateway: GatewayStatus?
    @State private var adding = false
    @State private var settingKey: ConnectionRow?
    @State private var signingIn: SignInTarget?
    @State private var confirmSignOut: ConnectionRow?
    @State private var confirmRemove: ConnectionRow?
    @State private var failure: String?
    /// Check Key results, per voice connection (in memory only).
    @State private var keyChecks: [String: VoiceStore] = [:]

    var body: some View {
        Form {
            if let failure {
                Section { Label(failure, systemImage: "exclamationmark.triangle.fill").foregroundStyle(.red).textSelection(.enabled) }
            }
            Section {
                ForEach(rows.filter { $0.api && $0.servesModels }) { apiRow($0) }
            } header: {
                SectionTitle("API providers", note: "Reached with a key, on this Mac and in the cloud. Each agent picks one in its form.")
            }
            let voice = rows.filter(\.servesVoice)
            if !voice.isEmpty {
                Section {
                    ForEach(voice) { voiceRow($0) }
                } header: {
                    SectionTitle("Voice", note: "Speech for agents with a voice: the voice notes they answer with and the ones people send them. Each agent chooses its voice on its Voice page.")
                }
            }
            let tools = rows.filter(\.servesTools)
            if !tools.isEmpty {
                Section {
                    ForEach(tools) { apiRow($0, icon: "wrench.and.screwdriver") }
                } header: {
                    SectionTitle("Tools", note: "APIs the agents' skills and scripts call with the key: web data, image and video generation. Set the key once here; an agent uses it by declaring the key under Secrets or Optional secrets in its form.")
                }
            }
            Section {
                ForEach(rows.filter { !$0.api }) { chatGPTRow($0) }
            } header: {
                SectionTitle("ChatGPT accounts", note: "Signed in on the core; agents running locally can use one before their cloud model. Each account is its own plan and limit.")
            }
        }
        .formStyle(.grouped)
        .navigationTitle("Providers")
        .toolbar {
            ToolbarItem {
                Button("Add Provider…", systemImage: "plus") { adding = true }
                    .disabled(model.cli == nil)
            }
        }
        .task(id: model.cli?.instance) {
            await load()
            #if DEBUG
            // STORMO_ADD_PROVIDER opens the Add Provider sheet (with STORMO_SNAPSHOT, a visual check).
            if ProcessInfo.processInfo.environment["STORMO_ADD_PROVIDER"] != nil, model.cli != nil { adding = true }
            // STORMO_CHECK_KEY checks the first voice connection's key (with STORMO_SNAPSHOT, a visual check).
            if ProcessInfo.processInfo.environment["STORMO_CHECK_KEY"] != nil, let r = rows.first(where: \.servesVoice) { await checkKey(r) }
            #endif
        }
        .task(id: model.core.state.isRunning) { await loadGateway() }
        .sheet(isPresented: $adding, onDismiss: { Task { await load() } }) {
            AddProviderSheet(taken: Set(rows.map(\.name)))
        }
        .sheet(item: $settingKey, onDismiss: { Task { await load() } }) { SetKeySheet(row: $0) }
        .sheet(item: $signingIn, onDismiss: { Task { await loadGateway() } }) { ChatGPTSignInSheet(connection: $0.name) }
        .confirmationDialog("Sign \(confirmSignOut?.name ?? "") out of ChatGPT?", isPresented: Binding { confirmSignOut != nil } set: { if !$0 { confirmSignOut = nil } }) {
            Button("Sign Out", role: .destructive) { if let r = confirmSignOut { Task { await signOut(r) } } }
        } message: {
            Text("Agents using it fall back to their cloud model until someone signs in again.")
        }
        .confirmationDialog("Remove \(confirmRemove?.name ?? "")?", isPresented: Binding { confirmRemove != nil } set: { if !$0 { confirmRemove = nil } }) {
            Button("Remove", role: .destructive) { if let r = confirmRemove { Task { await remove(r) } } }
        } message: {
            Text("It leaves stormo.yaml. Its key's value stays in the secrets until you remove it there.")
        }
    }

    // MARK: Rows

    private func apiRow(_ r: ConnectionRow, icon: String = "key.horizontal") -> some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack(alignment: .firstTextBaseline) {
                Image(systemName: icon).foregroundStyle(.secondary)
                VStack(alignment: .leading, spacing: 1) {
                    Text(r.name).fontWeight(.medium)
                    Text([r.label, r.baseUrl].compactMap { $0 }.joined(separator: " · ")).font(.caption).foregroundStyle(.secondary)
                }
                Spacer()
                keyStatus(r)
                Button("Set Key…") { settingKey = r }
                removeButton(r)
            }
            usedBy(r)
        }
        .padding(.vertical, 2)
    }

    @ViewBuilder private func keyStatus(_ r: ConnectionRow) -> some View {
        if r.keySet {
            Label("Key set", systemImage: "checkmark.circle.fill").foregroundStyle(.green).font(.callout)
        } else if !r.agentKeys.isEmpty {
            Label("Key set for \(r.agentKeys.joined(separator: ", "))", systemImage: "checkmark.circle").foregroundStyle(.secondary).font(.callout)
        } else {
            Label("No key", systemImage: "exclamationmark.circle").foregroundStyle(.orange).font(.callout)
        }
    }

    /// A voice connection: its key like an API's, and Check Key, which asks the provider what the
    /// key may do without spending anything.
    private func voiceRow(_ r: ConnectionRow) -> some View {
        let check = keyChecks[r.name]
        let hasKey = r.keySet || !r.agentKeys.isEmpty
        return VStack(alignment: .leading, spacing: 4) {
            HStack(alignment: .firstTextBaseline) {
                Image(systemName: "waveform").foregroundStyle(.secondary)
                VStack(alignment: .leading, spacing: 1) {
                    Text(r.name).fontWeight(.medium)
                    Text([r.label, r.baseUrl].compactMap { $0 }.joined(separator: " · ")).font(.caption).foregroundStyle(.secondary)
                }
                Spacer()
                keyStatus(r)
                Button("Check Key") { Task { await checkKey(r) } }
                    .disabled(model.cli == nil || !hasKey || check?.action == .checking)
                    .help(hasKey ? "Ask \(r.label) what this key may do, and the characters used this period. Nothing is spoken or spent."
                                 : "Set the key first")
                Button("Set Key…") { settingKey = r }
                removeButton(r)
            }
            usedBy(r)
            if let check {
                KeyCheckResult(store: check, shared: r.keySet).padding(.leading, 26).padding(.top, 2)
            }
        }
        .padding(.vertical, 2)
    }

    private func chatGPTRow(_ r: ConnectionRow) -> some View {
        // An older core has no connections list: its top-level fields are the default account's.
        let s = gateway?.connections?.first { $0.name == r.name }
            ?? (gateway?.connections == nil && r.name == "chatgpt" ? gateway.map {
                GatewayConnection(name: r.name, login: $0.login, account: $0.account, planLimitedUntil: nil, inflight: 0, queued: 0, concurrency: 0)
            } : nil)
        return VStack(alignment: .leading, spacing: 4) {
            HStack(alignment: .firstTextBaseline) {
                Image(systemName: "person.crop.circle").foregroundStyle(.secondary)
                VStack(alignment: .leading, spacing: 1) {
                    Text(r.name).fontWeight(.medium)
                    Text(status(s)).font(.caption).foregroundStyle(.secondary)
                }
                Spacer()
                if model.core.state.isRunning, s == nil {
                    Button("Restart Core to Serve It") { Task { await model.restartCore(); await loadGateway() } }
                } else if s?.login == "ok" {
                    Button("Sign Out…") { confirmSignOut = r }
                } else {
                    Button("Sign In…") { signingIn = SignInTarget(name: r.name) }
                }
                removeButton(r)
            }
            usedBy(r)
        }
        .padding(.vertical, 2)
    }

    private func status(_ s: GatewayConnection?) -> String {
        guard model.core.state.isRunning else { return "The core is not running" }
        guard let s else { return "Added since the core started" }
        switch s.login {
        case "ok":
            var t = "You're using your ChatGPT plan" + (s.account.map { " (\($0))" } ?? "")
            if let until = s.planLimitedUntil.flatMap(Date.init(isoString:)) {
                t += " · plan limit until \(until.formatted(.dateTime.hour().minute()))"
            }
            return t
        case "relogin_required": return "Signed out by ChatGPT: sign in again"
        default: return "Not signed in"
        }
    }

    @ViewBuilder private func usedBy(_ r: ConnectionRow) -> some View {
        if !r.usedBy.isEmpty {
            Text("Used by \(r.usedBy.joined(separator: ", "))").font(.caption).foregroundStyle(.tertiary).padding(.leading, 26)
        }
    }

    @ViewBuilder private func removeButton(_ r: ConnectionRow) -> some View {
        if r.implicit != true {
            Button("Remove", systemImage: "minus.circle") { confirmRemove = r }
                .labelStyle(.iconOnly).buttonStyle(.borderless)
                .disabled(!r.usedBy.isEmpty)
                .help(r.usedBy.isEmpty ? "Remove this connection" : "In use by \(r.usedBy.joined(separator: ", "))")
        }
    }

    // MARK: Actions

    private func load() async {
        guard let cli = model.cli else { return }
        do {
            rows = try await cli.run(["connections"], as: [ConnectionRow].self)
            failure = nil
        } catch {
            failure = error.localizedDescription
        }
    }

    /// The shared key, else the first agent's own.
    private func checkKey(_ r: ConnectionRow) async {
        guard let cli = model.cli else { return }
        let store = keyChecks[r.name] ?? VoiceStore()
        keyChecks[r.name] = store
        await store.check(r.name, agent: r.keySet ? nil : r.agentKeys.first, with: cli)
    }

    private func loadGateway() async {
        gateway = model.core.state.isRunning ? try? await model.fleet.client.gateway() : nil
    }

    private func signOut(_ r: ConnectionRow) async {
        guard let cli = model.cli else { return }
        do {
            _ = try await cli.run(["core", "logout", "--connection", r.name], as: LoginResult.self)
        } catch {
            failure = error.localizedDescription
        }
        await loadGateway()
    }

    private func remove(_ r: ConnectionRow) async {
        guard let cli = model.cli else { return }
        do {
            rows = try await cli.run(["connections", "remove", r.name], as: [ConnectionRow].self)
        } catch {
            failure = error.localizedDescription
        }
    }
}

/// What a voice connection's key may do: each permission Stormo uses, allowed or missing, and the
/// characters used this period when the key may read the account.
private struct KeyCheckResult: View {
    let store: VoiceStore
    /// The shared key was checked (else an agent's own).
    let shared: Bool

    var body: some View {
        if store.action == .checking {
            HStack(spacing: 6) {
                ProgressView().controlSize(.small)
                Text("Checking the key…").foregroundStyle(.secondary)
            }
            .font(.callout)
        } else if let p = store.problem {
            Label(p.message, systemImage: "xmark.octagon.fill").foregroundStyle(.red).font(.callout).textSelection(.enabled)
        } else if let c = store.keyCheck {
            VStack(alignment: .leading, spacing: 4) {
                ForEach(c.permissions) { p in
                    HStack(alignment: .firstTextBaseline, spacing: 6) {
                        Image(systemName: p.allowed ? "checkmark.circle.fill" : "xmark.circle.fill")
                            .foregroundStyle(p.allowed ? Color.green : Color.orange)
                        Text(title(p.name)).frame(width: 130, alignment: .leading)
                        Text(p.allowed ? p.purpose : "Missing: needed for \(p.purpose)")
                            .foregroundStyle(p.allowed ? Color.secondary : Color.orange)
                    }
                }
                if let ch = c.characters {
                    HStack(spacing: 8) {
                        ProgressView(value: Double(min(ch.used, max(ch.limit, 1))), total: Double(max(ch.limit, 1)))
                            .frame(width: 130)
                            .tint(ch.limit > 0 && Double(ch.used) / Double(ch.limit) > 0.9 ? .orange : .accentColor)
                        Text("\(ch.used.formatted()) of \(ch.limit.formatted()) characters used this period")
                            .foregroundStyle(.secondary)
                    }
                    .padding(.top, 2)
                }
                if !shared {
                    Text("Checked with an agent's own key: no shared one is set.").font(.caption).foregroundStyle(.tertiary)
                }
            }
            .font(.callout)
        }
    }

    private func title(_ name: String) -> String {
        let words = name.replacingOccurrences(of: "_", with: " ")
        return words.prefix(1).uppercased() + words.dropFirst()
    }
}

struct SignInTarget: Identifiable {
    let name: String
    var id: String { name }
}

/// A new connection: its kind, a name agents will pick it by, and for an API its key (stored with
/// `secrets set shared`, from stdin).
struct AddProviderSheet: View {
    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss
    let taken: Set<String>
    // Loaded by the sheet itself: a sheet's content does not see later changes of its presenter's
    // state that the presenter's body never read.
    @State private var kinds: [ConnectionKindInfo] = []
    @State private var kind = "openai"
    @State private var name = ""
    @State private var baseURL = ""
    @State private var keyName = ""
    @State private var keyValue = ""
    @State private var busy = false
    @State private var failure: String?

    private var info: ConnectionKindInfo? { kinds.first { $0.kind == kind } }
    private var effectiveName: String { name.isEmpty ? suggestedName : name }
    private var suggestedName: String {
        var n = kind == "chatgpt" ? "chatgpt-2" : kind, i = 2
        while taken.contains(n) {
            n = "\(kind)-\(i)"
            i += 1
        }
        return n
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            Text("Add a Provider").font(.title3).fontWeight(.semibold)
            Form {
                Picker("Kind", selection: $kind) {
                    ForEach(kinds) { Text($0.label).tag($0.kind) }
                }
                TextField("Name", text: $name, prompt: Text(suggestedName))
                if info?.api == true {
                    TextField("Base URL", text: $baseURL, prompt: Text(info?.baseUrl ?? "https://…/v1"))
                    TextField("Key's secret name", text: $keyName, prompt: Text(info?.key ?? "MYAPI_API_KEY"))
                    SecureField("API key", text: $keyValue, prompt: Text("optional: set it now"))
                }
            }
            .formStyle(.grouped)
            .scrollDisabled(true)
            .frame(height: info?.api == true ? 250 : 130)
            if info?.api == false {
                Text("A second ChatGPT account is its own plan and limit. The core serves it after a restart; then sign in to it.")
                    .font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                PlanTermsNotice(gateway: model.fleet.fleet?.gateway)
            } else if info?.serves == "tools" {
                Text("The key is stored for every agent (shared) in the instance's secrets, never in stormo.yaml. An agent's skills and scripts get it once the agent declares its secret name under Secrets or Optional secrets.")
                    .font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            } else {
                Text("The key is stored for every agent (shared) in the instance's secrets, never in stormo.yaml. Agents using this provider declare its name; their form adds it.")
                    .font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            }
            if let failure {
                Label(failure, systemImage: "xmark.octagon.fill").foregroundStyle(.red).textSelection(.enabled)
            }
            HStack {
                Spacer()
                Button("Cancel") { dismiss() }.keyboardShortcut(.cancelAction)
                Button("Add") { Task { await add() } }
                    .keyboardShortcut(.defaultAction)
                    .disabled(busy || (kind == "custom" && (baseURL.isEmpty || keyName.isEmpty)))
            }
        }
        .padding(20)
        .frame(width: 480)
        .task {
            guard kinds.isEmpty, let cli = model.cli else { return }
            do {
                kinds = try await cli.run(["connections", "kinds"], as: [ConnectionKindInfo].self)
                if !kinds.contains(where: { $0.kind == kind }), let first = kinds.first(where: \.api) { kind = first.kind }
            } catch {
                failure = error.localizedDescription
            }
        }
    }

    private func add() async {
        guard let cli = model.cli else { return }
        busy = true
        defer { busy = false }
        failure = nil
        var args = ["connections", "add", effectiveName, "--kind", kind]
        if !baseURL.isEmpty { args += ["--base-url", baseURL] }
        if !keyName.isEmpty { args += ["--key", keyName] }
        do {
            let rows = try await cli.run(args, as: [ConnectionRow].self)
            if !keyValue.isEmpty, let key = rows.first(where: { $0.name == effectiveName })?.key {
                _ = try await cli.run(["secrets", "set", "shared", key], as: [String: String].self, stdin: Data(keyValue.utf8))
            }
            dismiss()
        } catch {
            failure = error.localizedDescription
        }
    }
}

/// Set Key…: the API key's value for every agent (shared), from a secure field to stdin.
struct SetKeySheet: View {
    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss
    let row: ConnectionRow
    @State private var value = ""
    @State private var failure: String?

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            Text("\(row.name): \(row.key ?? "")").font(.title3).fontWeight(.semibold)
            SecureField("API key", text: $value, prompt: Text("paste the key"))
            Text("Stored as a shared value in the instance's secrets (secrets.local.yaml), for every agent that declares \(row.key ?? "it"). An agent's own value, if it has one, still wins. Push it to AWS with stormo secrets push for deployed agents.")
                .font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            if let failure {
                Label(failure, systemImage: "xmark.octagon.fill").foregroundStyle(.red)
            }
            HStack {
                Spacer()
                Button("Cancel") { dismiss() }.keyboardShortcut(.cancelAction)
                Button("Save") { Task { await save() } }
                    .keyboardShortcut(.defaultAction)
                    .disabled(value.trimmingCharacters(in: .whitespaces).isEmpty)
            }
        }
        .padding(20)
        .frame(width: 440)
    }

    private func save() async {
        guard let cli = model.cli, let key = row.key else { return }
        do {
            _ = try await cli.run(["secrets", "set", "shared", key], as: [String: String].self, stdin: Data(value.trimmingCharacters(in: .whitespaces).utf8))
            dismiss()
        } catch {
            failure = error.localizedDescription
        }
    }
}
