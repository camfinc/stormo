import StormoKit
import SwiftUI

/// Linked devices (docs/devices.md): watches and phones that reach the fleet from anywhere through
/// the device gateway while the core stays on loopback. A device links only with the code it shows,
/// typed here: a pending pairing alone links nothing, so the list of them has no approve button.
/// What each device may reach (its grants) is changed here; revoking one ends its sessions at their
/// next call.
struct LinkedDevicesView: View {
    @Environment(AppModel.self) private var model
    @State private var code = ""
    @State private var name = ""
    @State private var grants = DeviceGrants(status: true)
    /// What the last link or denial did, until the code is typed again.
    @State private var done: String?
    @State private var editing: LinkedDevice?
    @State private var revoking: LinkedDevice?
    @State private var showRevoked = false

    private var store: DevicesStore { model.devices }
    private var agents: [FleetAgent] { model.fleet.fleet?.agents ?? [] }

    var body: some View {
        Group {
            if !model.core.state.isRunning {
                ContentUnavailableView("Core stopped", systemImage: "server.rack", description: Text("Start the core to see and link devices."))
            } else if !store.loaded, let problem = store.problem {
                ContentUnavailableView("Can't load linked devices", systemImage: "exclamationmark.triangle", description: Text(problem))
            } else if !store.loaded {
                ProgressView()
            } else {
                content
            }
        }
        .navigationTitle("Linked Devices")
        .sheet(item: $editing) { EditDeviceSheet(device: $0) }
        .confirmationDialog("Revoke \(revoking?.name ?? "this device")?",
                            isPresented: Binding(get: { revoking != nil }, set: { if !$0 { revoking = nil } }),
                            presenting: revoking) { d in
            Button("Revoke \(d.name)", role: .destructive) { Task { await store.revoke(d.id) } }
            Button("Cancel", role: .cancel) {}
        } message: { _ in
            Text("It loses access at its next call. To use it again it has to pair from the start and be linked with a new code.")
        }
        .task {
            // A visual check of the link form or a device's sheet (with STORMO_CORE_PAGE=devices and STORMO_SNAPSHOT).
            #if DEBUG
            let env = ProcessInfo.processInfo.environment
            guard env["STORMO_DEVICE_CODE"] != nil || env["STORMO_EDIT_DEVICE"] != nil else { return }
            // After the first load: the store is cleared when the app points it at the instance's core.
            while !store.loaded && !Task.isCancelled { try? await Task.sleep(for: .milliseconds(200)) }
            if let c = env["STORMO_DEVICE_CODE"] {
                code = PairingCode.format(c)
                lookUp()
            }
            if let id = env["STORMO_EDIT_DEVICE"] { editing = store.devices.first { $0.id == id } }
            #endif
        }
        .task(id: model.core.state.isRunning) {
            // Pairings come and expire, and last-seen moves: read again while the page is shown.
            while model.core.state.isRunning && !Task.isCancelled {
                await store.load()
                try? await Task.sleep(for: .seconds(5))
            }
        }
    }

    private var content: some View {
        Form {
            if let problem = store.problem {
                Section { Label(problem, systemImage: "exclamationmark.triangle.fill").foregroundStyle(.red) }
            }
            if let problem = store.actionProblem {
                Section { Label(problem, systemImage: "xmark.octagon.fill").foregroundStyle(.red).textSelection(.enabled) }
            }
            gatewaySection
            linkSection
            if let p = store.preview, let c = store.previewCode {
                claimsSection(p, code: c)
                linkAsSection(p)
            }
            if store.isEmpty {
                emptySection
            } else {
                if !store.pairings.isEmpty { pendingSection }
                linkedSection
                if !store.revoked.isEmpty { revokedSection }
            }
        }
        .formStyle(.grouped)
    }

    // MARK: The gateway

    @ViewBuilder
    private var gatewaySection: some View {
        if let g = store.gateway {
            Section {
                LabeledContent {
                    GatewayState(capability: g)
                } label: {
                    Text(g.name)
                    if let p = g.publish, !p.isEmpty { Text("Published at \(p)").monospaced() }
                }
                if !(g.worker == "awake" || g.worker == "booting") {
                    Text("Offline: no device reaches the fleet until the gateway is brought online again (Capabilities).")
                        .font(.callout).foregroundStyle(.orange)
                }
                if let e = g.service?.error, !e.isEmpty {
                    Text("Last start failed: \(e)").font(.callout).foregroundStyle(.red).lineLimit(3)
                }
            } header: {
                SectionTitle("Device gateway", note: "The only part of the fleet the internet reaches. Taking it offline in Capabilities cuts every device off at once.")
            }
        } else if store.gatewayKnown && !store.isEmpty {
            Section {
                Label("No device gateway: linked devices can't reach the fleet. docs/devices.md says how to set one up.", systemImage: "exclamationmark.triangle.fill")
                    .foregroundStyle(.orange)
            }
        }
    }

    // MARK: Linking

    private var lookingUp: Bool { store.busy.contains("lookup") }
    private var deciding: Bool { store.busy.contains("link") || store.busy.contains("deny") }

    /// What is typed, shown like the device shows it; typing again drops the looked-up pairing.
    private var codeBinding: Binding<String> {
        Binding(get: { code }, set: { new in
            let formatted = PairingCode.format(new)
            code = formatted
            done = nil
            if store.previewCode != formatted { store.cancelLink() }
        })
    }

    private var linkSection: some View {
        Section {
            HStack(spacing: 8) {
                Text("Code on the device")
                Spacer()
                if lookingUp { ProgressView().controlSize(.small) }
                TextField("Code on the device", text: codeBinding, prompt: Text("XXXX-XXXX"))
                    .labelsHidden()
                    .textFieldStyle(.roundedBorder)
                    .font(.title3.monospaced())
                    .multilineTextAlignment(.center)
                    .frame(width: 150)
                    .onSubmit { lookUp() }
                Button("Look Up") { lookUp() }
                    .disabled(!PairingCode.isComplete(code) || lookingUp || deciding)
            }
            if let p = PairingCode.problem(code) {
                Text(p).font(.caption).foregroundStyle(.red)
            }
            if let e = store.linkProblem {
                Label(e, systemImage: "xmark.octagon.fill").foregroundStyle(.red).textSelection(.enabled)
            }
            if let done {
                Label(done, systemImage: "checkmark.circle.fill").foregroundStyle(.green)
            }
        } header: {
            SectionTitle("Link a device", note: "Open Stormo on the watch or phone: it shows a code. Type that code here; nothing links without it.")
        }
    }

    private func claimsSection(_ p: DevicePairing, code: String) -> some View {
        Section {
            FingerprintCheck(fingerprint: p.fingerprint)
            LabeledContent("Name it gives", value: p.name)
            LabeledContent("Model", value: p.model.nonEmpty ?? "Not given")
            LabeledContent("System", value: p.os.nonEmpty ?? "Not given")
            if let v = p.appVersion.nonEmpty { LabeledContent("Stormo app", value: v) }
            LabeledContent("Asked from", value: place(p.ip, p.country) ?? "Unknown")
            LabeledContent("Code") {
                HStack(spacing: 6) {
                    Text(code).monospaced()
                    Text("·").foregroundStyle(.tertiary)
                    ExpiryText(expires: p.expiresDate)
                }
            }
        } header: {
            SectionTitle("What the device claims", note: "Name, model and system are what it says about itself; the address is where its request reached the gateway from.")
        }
    }

    private func linkAsSection(_ p: DevicePairing) -> some View {
        Section {
            TextField("Name", text: $name, prompt: Text(p.name))
            if let problem = nameProblem(name, allowEmpty: true) {
                Text(problem).font(.caption).foregroundStyle(.red)
            }
            DeviceGrantsEditor(grants: $grants, agents: agents)
                .id(p.id)
            TimelineView(.periodic(from: .now, by: 1)) { context in
                let expired = p.isExpired(at: context.date)
                HStack {
                    Button("Deny", role: .destructive) { Task { await deny() } }
                        .disabled(deciding || expired)
                        .help("Refuse it: the device is told and has to pair again.")
                    Spacer()
                    if deciding { ProgressView().controlSize(.small) }
                    Button("Cancel") {
                        store.cancelLink()
                        code = ""
                    }
                    .disabled(deciding)
                    Button("Link") { Task { await link() } }
                        .buttonStyle(.borderedProminent)
                        .disabled(deciding || expired || nameProblem(name, allowEmpty: true) != nil)
                        .help(expired ? "The code expired: have the device pair again." : "Link it with these grants.")
                }
            }
        } header: {
            SectionTitle("Link it as", note: "What it may reach. The core checks every call it makes against these; change them any time.")
        }
    }

    private func lookUp() {
        guard PairingCode.isComplete(code), !lookingUp, !deciding else { return }
        done = nil
        Task {
            if await store.lookUp(code: code), let p = store.preview {
                name = p.name
                grants = DeviceGrants(status: true)
            }
        }
    }

    private func link() async {
        let as_ = name.trimmingCharacters(in: .whitespacesAndNewlines)
        if let d = await store.link(name: as_, grants: grants) {
            done = "Linked \(d.name). The device finishes pairing by itself in a few seconds."
            code = ""
            name = ""
        }
    }

    private func deny() async {
        if await store.deny() {
            done = "Denied. The device is told, and has to pair again to ask once more."
            code = ""
            name = ""
        }
    }

    // MARK: Lists

    private var pendingSection: some View {
        Section {
            ForEach(store.pairings) { PairingRow(pairing: $0) }
        } header: {
            SectionTitle("Waiting for a code", note: "Devices that asked to pair. To link one, type the code it shows above: the list never holds the codes.")
        }
    }

    private var linkedSection: some View {
        Section {
            if store.linked.isEmpty {
                Text("No device is linked now.").foregroundStyle(.secondary)
            }
            ForEach(store.linked) { d in
                DeviceRow(device: d, agentName: agentName, busy: store.busy.contains(d.id),
                          edit: { editing = d }, revoke: { revoking = d })
            }
        } header: {
            SectionTitle("Linked", note: "What each device may reach, and when it last called in.")
        }
    }

    private var revokedSection: some View {
        Section {
            DisclosureGroup("Revoked (\(store.revoked.count))", isExpanded: $showRevoked) {
                ForEach(store.revoked) { RevokedRow(device: $0) }
            }
        }
    }

    private var emptySection: some View {
        Section {
            VStack(alignment: .leading, spacing: 10) {
                Label("No linked devices yet", systemImage: "applewatch.radiowaves.left.and.right")
                    .font(.headline)
                Text("A watch or phone reaches the fleet from anywhere through the device gateway: a small capability that is the only thing the internet can reach. The core stays on loopback, keeps the list of devices and checks every call against the device's grants.")
                if store.gateway == nil {
                    VStack(alignment: .leading, spacing: 4) {
                        Text("To set it up:").fontWeight(.medium)
                        Text("1. In stormo.yaml, set `core.loopback_trust: false` and declare a capability from the `device-gateway` template with a `publish` address (like `127.0.0.1:18650`).")
                        Text("2. Restart the core, then point a tunnel or reverse proxy at that address, serving only `/device/v1/`.")
                        Text("3. Open Stormo on the device and type the code it shows above.")
                    }
                } else {
                    Text("The gateway is set up. Open Stormo on the device and type the code it shows above.")
                }
                HStack(alignment: .firstTextBaseline) {
                    Text("Every step, the tunnel options and the threat model: docs/devices.md in the Stormo repository.")
                        .foregroundStyle(.secondary)
                    if let url = docsURL {
                        Spacer()
                        Button("Open devices.md") { NSWorkspace.shared.open(url) }
                    }
                }
            }
            .font(.callout)
            .fixedSize(horizontal: false, vertical: true)
            .padding(.vertical, 4)
        }
    }

    /// docs/devices.md when the instance has the engine's docs beside it.
    private var docsURL: URL? {
        guard let root = model.instances.active?.folder else { return nil }
        return ["docs/devices.md", "stormo/docs/devices.md"].map { root.appending(path: $0) }
            .first { FileManager.default.fileExists(atPath: $0.path) }
    }

    private func agentName(_ id: String) -> String { model.fleet.agent(id)?.name ?? id }
}

// MARK: Rows

/// The device's key fingerprint, large, with the check the owner must make before linking.
private struct FingerprintCheck: View {
    let fingerprint: String?

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            Label("Check that this fingerprint matches the one on the watch", systemImage: "exclamationmark.shield.fill")
                .foregroundStyle(.orange).fontWeight(.medium)
            Text(fingerprint.nonEmpty ?? "No fingerprint given")
                .font(.system(size: 26, weight: .semibold, design: .monospaced))
                .textSelection(.enabled)
            Text("The device shows it with the code. If it differs, another device asked with this code: deny it.")
                .font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(12)
        .background(RoundedRectangle(cornerRadius: 8).fill(.orange.opacity(0.08)))
    }
}

private struct PairingRow: View {
    let pairing: DevicePairing

    var body: some View {
        HStack(alignment: .top, spacing: 12) {
            DeviceSymbol(model: pairing.model, os: pairing.os)
            VStack(alignment: .leading, spacing: 3) {
                HStack(alignment: .firstTextBaseline, spacing: 8) {
                    Text(pairing.name.isEmpty ? "Unnamed device" : pairing.name).fontWeight(.medium)
                    if let kind = describe(pairing.model, pairing.os) { Text(kind).font(.caption).foregroundStyle(.secondary) }
                }
                Group {
                    if let created = pairing.created.map(Date.init(milliseconds:)) {
                        Text("Asked \(created, format: .relative(presentation: .named))\(place(pairing.ip, pairing.country).map { " from \($0)" } ?? "")")
                    } else if let where_ = place(pairing.ip, pairing.country) {
                        Text("Asked from \(where_)")
                    }
                }
                .font(.caption).foregroundStyle(.secondary)
                if let fp = pairing.fingerprint.nonEmpty {
                    Text("Fingerprint \(fp)").font(.caption.monospaced()).foregroundStyle(.secondary).textSelection(.enabled)
                }
            }
            Spacer()
            ExpiryText(expires: pairing.expiresDate).font(.caption)
        }
        .padding(.vertical, 2)
    }
}

private struct DeviceRow: View {
    let device: LinkedDevice
    let agentName: (String) -> String
    let busy: Bool
    let edit: () -> Void
    let revoke: () -> Void

    var body: some View {
        HStack(alignment: .top, spacing: 12) {
            DeviceSymbol(model: device.model, os: device.os)
            VStack(alignment: .leading, spacing: 3) {
                HStack(alignment: .firstTextBaseline, spacing: 8) {
                    Text(device.name).fontWeight(.medium)
                    if let kind = describe(device.model, device.os) { Text(kind).font(.caption).foregroundStyle(.secondary) }
                }
                Text(device.grants.summary(name: agentName)).font(.callout)
                    .foregroundStyle(device.grants.isEmpty ? .secondary : .primary)
                Group {
                    if let seen = device.lastSeenDate {
                        Text("Last seen \(seen, format: .relative(presentation: .named))\(place(device.lastIp, device.country).map { " from \($0)" } ?? "")")
                    } else {
                        Text("Not seen since it was linked")
                    }
                }
                .font(.caption).foregroundStyle(.secondary)
                HStack(spacing: 6) {
                    if let fp = device.fingerprint.nonEmpty {
                        Text("Fingerprint \(fp)").monospaced().textSelection(.enabled)
                        Text("·").foregroundStyle(.tertiary)
                    }
                    Text(attestation(device.attestation))
                        .help("Certificates the device sent about its key when it paired. They are recorded, not verified yet.")
                }
                .font(.caption).foregroundStyle(.secondary)
            }
            Spacer()
            Button("Edit…", action: edit)
                .disabled(busy)
                .help("Rename it or change what it may reach.")
            Button("Revoke…", role: .destructive, action: revoke)
                .disabled(busy)
                .help("Unlink it: its sessions stop at their next call.")
        }
        .padding(.vertical, 2)
    }

    private func attestation(_ n: Int?) -> String {
        switch n ?? 0 {
        case 0: "No attestation"
        case 1: "1 attestation certificate"
        case let n: "\(n) attestation certificates"
        }
    }
}

private struct RevokedRow: View {
    let device: LinkedDevice

    var body: some View {
        HStack(alignment: .top, spacing: 12) {
            DeviceSymbol(model: device.model, os: device.os, faded: true)
            VStack(alignment: .leading, spacing: 2) {
                HStack(alignment: .firstTextBaseline, spacing: 8) {
                    Text(device.name)
                    if let kind = describe(device.model, device.os) { Text(kind).font(.caption).foregroundStyle(.secondary) }
                }
                Group {
                    if let when = device.revokedDate {
                        Text("Revoked \(when, format: .dateTime.day().month().year().hour().minute())\(device.revokedReason.nonEmpty.map { ": \($0)" } ?? "")")
                    }
                    if let fp = device.fingerprint.nonEmpty { Text("Fingerprint \(fp)").monospaced() }
                }
                .font(.caption).foregroundStyle(.secondary)
            }
        }
        .padding(.vertical, 1)
    }
}

/// Online, starting or offline, as the gateway's worker is.
private struct GatewayState: View {
    let capability: CapabilityInfo

    var body: some View {
        let (text, color): (String, Color) = switch capability.worker {
        case "awake": ("Online", .green)
        case "booting": ("Starting", .orange)
        default: ("Offline", .secondary)
        }
        Label {
            Text(text)
        } icon: {
            Circle().fill(color).frame(width: 9, height: 9)
        }
        .foregroundStyle(.secondary)
    }
}

/// "Expires in 9:41", counting down, then "Expired".
private struct ExpiryText: View {
    let expires: Date?

    var body: some View {
        if let expires {
            TimelineView(.periodic(from: .now, by: 1)) { context in
                let left = expires.timeIntervalSince(context.date)
                if left <= 0 {
                    Text("Expired").foregroundStyle(.red)
                } else {
                    Text("Expires in \(Duration.seconds(left.rounded(.up)).formatted(.time(pattern: .minuteSecond)))")
                        .monospacedDigit()
                }
            }
        }
    }
}

private struct DeviceSymbol: View {
    let model: String?
    let os: String?
    var faded = false

    var body: some View {
        let s = ((model ?? "") + " " + (os ?? "")).lowercased()
        Image(systemName: s.contains("watch") || s.contains("wear") ? "applewatch" : "smartphone")
            .font(.title2)
            .foregroundStyle(faded ? .tertiary : .secondary)
            .frame(width: 28)
    }
}

// MARK: Grants

/// Status, Review and Chat (none, every agent, or picked ones). Agents a device was granted that
/// the fleet no longer has stay listed, so saving never drops a grant silently.
struct DeviceGrantsEditor: View {
    @Binding var grants: DeviceGrants
    let agents: [FleetAgent]
    @State private var mode: DeviceGrants.Chat

    init(grants: Binding<DeviceGrants>, agents: [FleetAgent]) {
        _grants = grants
        self.agents = agents
        _mode = State(initialValue: grants.wrappedValue.chatAccess)
    }

    private struct Choice: Identifiable {
        let id: String
        let label: String
        let inFleet: Bool
    }

    private var choices: [Choice] {
        var out = agents.map { Choice(id: $0.id, label: $0.name == $0.id ? $0.id : "\($0.name) (\($0.id))", inFleet: true) }
        for id in grants.chat where id != "*" && !agents.contains(where: { $0.id == id }) {
            out.append(Choice(id: id, label: id, inFleet: false))
        }
        return out
    }

    var body: some View {
        Toggle(isOn: $grants.status) {
            Text("Status")
            Text("The fleet at a glance: who runs, who is busy, what waits for review.")
        }
        Toggle(isOn: $grants.review) {
            Text("Review")
            Text("Accept or reject lessons and settings. Skills, schedules, scripts and anything flagged stay in the app.")
        }
        Picker(selection: modeBinding) {
            Text("No agent").tag(DeviceGrants.Chat.none)
            Text("Every agent").tag(DeviceGrants.Chat.all)
            Text("Some agents").tag(DeviceGrants.Chat.some)
        } label: {
            Text("Chat")
            Text("The agents it may talk with.")
        }
        .pickerStyle(.segmented)
        if mode == .some {
            if choices.isEmpty {
                Text("No agents in this instance yet.").foregroundStyle(.secondary)
            }
            ForEach(choices) { c in
                Toggle(isOn: chatBinding(c.id)) {
                    Text(c.label)
                    if !c.inFleet { Text("Not in the fleet now") }
                }
            }
            if grants.chat.isEmpty {
                Text("No agent picked: it can chat with none.").font(.caption).foregroundStyle(.secondary)
            }
        }
    }

    private var modeBinding: Binding<DeviceGrants.Chat> {
        Binding(get: { mode }, set: { m in
            mode = m
            switch m {
            case .none: grants.chat = []
            case .all: grants.chat = ["*"]
            case .some: grants.chat = grants.chat.filter { $0 != "*" }
            }
        })
    }

    private func chatBinding(_ id: String) -> Binding<Bool> {
        Binding(get: { grants.chat.contains(id) }, set: { on in
            if on {
                if !grants.chat.contains(id) { grants.chat.append(id) }
            } else {
                grants.chat.removeAll { $0 == id }
            }
        })
    }
}

// MARK: Edit

/// A linked device's name and grants, saved on the core (`PATCH /api/devices/<id>`).
struct EditDeviceSheet: View {
    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss
    let device: LinkedDevice
    @State private var name: String
    @State private var grants: DeviceGrants
    @State private var busy = false
    @State private var failure: String?

    init(device: LinkedDevice) {
        self.device = device
        _name = State(initialValue: device.name)
        _grants = State(initialValue: device.grants)
    }

    private var trimmed: String { name.trimmingCharacters(in: .whitespacesAndNewlines) }
    private var changed: Bool { trimmed != device.name || grants != device.grants }

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            HStack(spacing: 10) {
                DeviceSymbol(model: device.model, os: device.os)
                VStack(alignment: .leading, spacing: 2) {
                    Text(device.name).font(.title3).fontWeight(.semibold)
                    if let kind = describe(device.model, device.os) { Text(kind).foregroundStyle(.secondary) }
                }
            }
            Form {
                Section {
                    TextField("Name", text: $name)
                    if let problem = nameProblem(name, allowEmpty: false) {
                        Text(problem).font(.caption).foregroundStyle(.red)
                    }
                }
                Section {
                    DeviceGrantsEditor(grants: $grants, agents: model.fleet.fleet?.agents ?? [])
                } header: {
                    SectionTitle("May reach", note: "The core checks every call against these: a change applies at the device's next call.")
                }
            }
            .formStyle(.grouped)
            .frame(minHeight: 380)
            if let failure {
                Label(failure, systemImage: "xmark.octagon.fill").foregroundStyle(.red).textSelection(.enabled)
            }
            HStack {
                Spacer()
                Button("Cancel") { dismiss() }.keyboardShortcut(.cancelAction)
                Button("Save") { Task { await save() } }
                    .keyboardShortcut(.defaultAction)
                    .disabled(busy || !changed || nameProblem(name, allowEmpty: false) != nil)
            }
        }
        .padding(20)
        .frame(width: 560)
    }

    private func save() async {
        busy = true
        defer { busy = false }
        let problem = await model.devices.update(device.id,
                                                 name: trimmed != device.name ? trimmed : nil,
                                                 grants: grants != device.grants ? grants : nil)
        if let problem {
            failure = problem
        } else {
            dismiss()
        }
    }
}

// MARK: Helpers

/// Why a device name can't be used (1 to 64 characters), nil when it can.
private func nameProblem(_ name: String, allowEmpty: Bool) -> String? {
    let n = name.trimmingCharacters(in: .whitespacesAndNewlines)
    if n.isEmpty { return allowEmpty ? nil : "A device needs a name." }
    if n.count > 64 { return "At most 64 characters." }
    return nil
}

/// "Pixel Watch 3 · Wear OS 6", nil when neither is known.
private func describe(_ model: String?, _ os: String?) -> String? {
    let parts = [model, os].compactMap(\.nonEmpty)
    return parts.isEmpty ? nil : parts.joined(separator: " · ")
}

/// "203.0.113.7 · NL", nil when neither is known.
private func place(_ ip: String?, _ country: String?) -> String? {
    let parts = [ip, country].compactMap(\.nonEmpty)
    return parts.isEmpty ? nil : parts.joined(separator: " · ")
}

private extension Optional where Wrapped == String {
    /// The string when it has something in it.
    var nonEmpty: String? {
        guard let s = self?.trimmingCharacters(in: .whitespacesAndNewlines), !s.isEmpty else { return nil }
        return s
    }
}

private extension Date {
    init(milliseconds ms: Int64) { self.init(timeIntervalSince1970: Double(ms) / 1000) }
}
