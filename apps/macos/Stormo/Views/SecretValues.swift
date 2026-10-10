import StormoKit
import SwiftUI

/// The agent's secret values in its Settings form: which declared names have one and where it comes
/// from (`secrets status`), and Set… to store one (`secrets set`, value on stdin). Saves at once,
/// outside agent.yaml's Save; values are never read back.
struct SecretValuesSection: View {
    @Environment(AppModel.self) private var model
    let agentID: String
    /// The declared names, so the list follows unsaved edits to them after a save.
    let declared: [String]
    @State private var rows: [SecretStatus] = []
    @State private var failure: String?
    @State private var editing: SecretStatus?

    var body: some View {
        Section {
            if let failure {
                Label(failure, systemImage: "exclamationmark.triangle").foregroundStyle(.orange)
            }
            ForEach(rows.filter { $0.kind != "local" }) { row in
                HStack(alignment: .firstTextBaseline) {
                    Circle().fill(color(row)).frame(width: 8, height: 8)
                    VStack(alignment: .leading, spacing: 2) {
                        Text(row.name).monospaced()
                        Text(detail(row)).font(.caption).foregroundStyle(.secondary)
                    }
                    Spacer()
                    Button(row.isSet ? "Replace…" : "Set…") { editing = row }
                }
            }
        } header: {
            SectionTitle("Secret values", note: "Stored on this Mac (the agent's data folder or secrets.local.yaml), never in its files. A running agent uses a new value after a restart; deployed agents need stormo secrets push.")
        }
        .task(id: declared) { await load() }
        .sheet(item: $editing) { row in
            SetSecretSheet(agentID: agentID, row: row) { Task { await load() } }
        }
    }

    private func color(_ r: SecretStatus) -> Color {
        if r.isSet { return .green }
        return r.kind == "optional" ? .secondary : .orange
    }

    private func detail(_ r: SecretStatus) -> String {
        var s = switch r.source {
        case "agent": "Set for this agent"
        case "unit": "From its unit"
        case "shared": "Shared value"
        default: r.kind == "optional" ? "Not set (optional: what it gates stays off)" : "Missing"
        }
        if !r.local.isEmpty { s += " · local runs use a local override" }
        return s
    }

    private func load() async {
        guard let cli = model.cli else { return }
        do {
            rows = try await cli.run(["secrets", "status", agentID], as: [SecretStatus].self)
            failure = nil
        } catch {
            failure = "Could not read the secrets: \(error.localizedDescription)"
        }
    }
}

/// Set… for one secret: the value from a secure field to `secrets set` on stdin.
private struct SetSecretSheet: View {
    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss
    let agentID: String
    let row: SecretStatus
    let saved: () -> Void
    @State private var value = ""
    @State private var shared = false
    @State private var failure: String?

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            Text(row.name).font(.title3).fontWeight(.semibold).monospaced()
            SecureField("Value", text: $value, prompt: Text("paste the value"))
            Picker("Store for", selection: $shared) {
                Text("This agent").tag(false)
                Text("Every agent (shared)").tag(true)
            }
            .pickerStyle(.radioGroup)
            Text(shared
                 ? "A shared value is used by every agent that declares \(row.name) and has no value of its own."
                 : "Only \(agentID) uses it; it wins over a shared value.")
                .font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            if !row.local.isEmpty {
                Label("A local override exists in secrets.local.yaml (local:); local runs keep using it.", systemImage: "info.circle")
                    .font(.callout).foregroundStyle(.secondary)
            }
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
        guard let cli = model.cli else { return }
        do {
            _ = try await cli.run(["secrets", "set", shared ? "shared" : agentID, row.name], as: [String: String].self,
                                  stdin: Data(value.trimmingCharacters(in: .whitespaces).utf8))
            saved()
            dismiss()
        } catch {
            failure = error.localizedDescription
        }
    }
}
