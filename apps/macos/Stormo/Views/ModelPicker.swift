import StormoKit
import SwiftUI

/// "Choose…" next to a model field: the models the connection offers (stormo connections models),
/// searchable; picking one fills the field, which still takes any name typed.
struct ModelChooser: View {
    @Environment(AppModel.self) private var model
    let connection: String
    let agentID: String
    @Binding var value: String
    @State private var open = false

    var body: some View {
        Button("Choose…") { open = true }
            .help("The models \(connection) offers")
            .popover(isPresented: $open, arrowEdge: .trailing) {
                ModelList(connection: connection, agentID: agentID) { id in
                    value = id
                    open = false
                }
                .environment(model)
            }
            #if DEBUG
            // STORMO_CHOOSE_MODEL=<connection> opens this chooser (with STORMO_SNAPSHOT, a visual check).
            .task {
                if ProcessInfo.processInfo.environment["STORMO_CHOOSE_MODEL"] == connection {
                    try? await Task.sleep(for: .seconds(2))
                    open = true
                }
            }
            #endif
    }
}

private struct ModelList: View {
    @Environment(AppModel.self) private var model
    let connection: String
    let agentID: String
    let pick: (String) -> Void
    @State private var models: [ConnectionModel]?
    @State private var failure: String?
    @State private var search = ""

    private var shown: [ConnectionModel] {
        guard let models else { return [] }
        let q = search.trimmingCharacters(in: .whitespaces).lowercased()
        return q.isEmpty ? models : models.filter { $0.id.lowercased().contains(q) || ($0.name?.lowercased().contains(q) ?? false) }
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            Text("\(connection)'s models").font(.headline)
            TextField("Search", text: $search, prompt: Text("Search models"))
                .textFieldStyle(.roundedBorder)
            if let failure {
                Label(failure, systemImage: "exclamationmark.triangle").foregroundStyle(.orange)
                    .fixedSize(horizontal: false, vertical: true)
                Spacer()
            } else if models == nil {
                ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity)
            } else {
                List(shown) { m in
                    Button { pick(m.id) } label: {
                        HStack {
                            VStack(alignment: .leading, spacing: 1) {
                                Text(m.id).monospaced()
                                if let n = m.name, n != m.id { Text(n).font(.caption).foregroundStyle(.secondary) }
                            }
                            Spacer()
                            if let c = m.contextLength, c > 0 {
                                Text("\(c / 1000)k").font(.caption).foregroundStyle(.secondary).monospacedDigit()
                            }
                        }
                        .contentShape(Rectangle())
                    }
                    .buttonStyle(.plain)
                }
                .listStyle(.inset)
                Text("\(shown.count) of \(models?.count ?? 0)").font(.caption).foregroundStyle(.tertiary)
            }
        }
        .padding(12)
        .frame(width: 420, height: 440)
        .task {
            guard let cli = model.cli else { return }
            do {
                models = try await cli.run(["connections", "models", connection, "--agent", agentID], as: [ConnectionModel].self)
            } catch {
                failure = error.localizedDescription
            }
        }
    }
}
