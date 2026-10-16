import StormoKit
import SwiftUI

/// The heavy or shared calls the core runs for agents (stormo.yaml capabilities:): who may use
/// each, how busy it is, what this month cost, and the latest jobs (scrubbed previews only).
struct CapabilitiesSection: View {
    @Environment(AppModel.self) private var model
    @State private var view: CapabilitiesView?
    @State private var jobs: [CapabilityJob] = []
    @State private var problem: String?

    var body: some View {
        Group {
            if !model.core.state.isRunning {
                ContentUnavailableView("Core stopped", systemImage: "server.rack", description: Text("Start the core to see its capabilities."))
            } else if let view, view.capabilities.isEmpty {
                ContentUnavailableView("No capabilities", systemImage: "shippingbox",
                                       description: Text("Declare them in stormo.yaml capabilities: and grant them in each agent's capabilities:."))
            } else if let view {
                Form {
                    if let problem { Section { Label(problem, systemImage: "exclamationmark.triangle.fill").foregroundStyle(.red) } }
                    Section("Capabilities") {
                        ForEach(view.capabilities) { c in
                            VStack(alignment: .leading, spacing: 4) {
                                HStack {
                                    Text(c.name).fontWeight(.medium)
                                    Text(c.kind + (c.placement.map { " · \($0)" } ?? "")).font(.caption).foregroundStyle(.secondary)
                                    Spacer()
                                    if c.running + c.queued > 0 {
                                        Text("\(c.running) running · \(c.queued) queued").font(.caption.monospacedDigit())
                                    }
                                }
                                if !c.description.isEmpty { Text(c.description).font(.callout).foregroundStyle(.secondary) }
                                Text(c.agents.isEmpty ? "Granted to no agent" : "Granted to " + c.agents.joined(separator: ", "))
                                    .font(.caption).foregroundStyle(.secondary)
                            }
                            .padding(.vertical, 2)
                        }
                    }
                    Section("This month") {
                        if view.spend.isEmpty {
                            Text("No jobs yet").foregroundStyle(.secondary)
                        }
                        ForEach(view.spend) { s in
                            LabeledContent("\(s.agent) · \(s.capability)") {
                                Text("\(s.jobs) jobs\(s.failed > 0 ? " (\(s.failed) failed)" : "") · \(s.usd, format: .currency(code: "USD"))")
                                    .monospacedDigit()
                            }
                        }
                    }
                    Section("Latest jobs") {
                        ForEach(jobs) { j in
                            HStack(alignment: .firstTextBaseline) {
                                Text("#\(j.job)").monospacedDigit().foregroundStyle(.secondary)
                                VStack(alignment: .leading, spacing: 2) {
                                    Text("\(j.agent) · \(j.capability)")
                                    if let p = j.preview, !p.isEmpty { Text(p).font(.caption.monospaced()).foregroundStyle(.secondary).lineLimit(1) }
                                    if let e = j.error, !e.isEmpty { Text(e).font(.caption).foregroundStyle(.red).lineLimit(2) }
                                }
                                Spacer()
                                Text(j.status).foregroundStyle(j.status == "failed" ? Color.red : Color.secondary)
                            }
                        }
                    }
                }
                .formStyle(.grouped)
            } else if let problem {
                ContentUnavailableView("Can't load capabilities", systemImage: "exclamationmark.triangle", description: Text(problem))
            } else {
                ProgressView()
            }
        }
        .navigationTitle("Capabilities")
        .task(id: model.core.state.isRunning) {
            while model.core.state.isRunning && !Task.isCancelled {
                await load()
                try? await Task.sleep(for: .seconds(5))
            }
        }
    }

    private func load() async {
        do {
            view = try await model.core.client.capabilities()
            jobs = try await model.core.client.capabilityJobs()
            problem = nil
        } catch CoreClient.Failure.notFound {
            problem = "This core has no capabilities API: restart it with the app's stormo."
        } catch {
            problem = error.localizedDescription
        }
    }
}
