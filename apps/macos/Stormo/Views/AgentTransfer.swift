import AppKit
import StormoKit
import SwiftUI
import UniformTypeIdentifiers

/// Export… for one agent: its definition, or its definition and its data (naps and secret values,
/// in plain text: the sheet says so and asks for a confirmation first).
struct ExportAgentSheet: View {
    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss
    let agentID: String
    @State private var withData = false
    @State private var understood = false
    @State private var busy = false
    @State private var failure: String?
    @State private var done: ExportResult?

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            Text("Export \(agentID)").font(.title3).fontWeight(.semibold)
            Picker("Export", selection: $withData) {
                Text("Configuration").tag(false)
                Text("Configuration and data").tag(true)
            }
            .pickerStyle(.radioGroup)
            .labelsHidden()
            Text(withData
                 ? "Its folder, persona, unit and bridge actions, plus its naps (memory and conversation history) and every secret value it uses."
                 : "Its folder (agent.yaml, behaviour, skills, scripts, engine files, reviewed learnings), its persona, its unit and the bridge actions it uses. No naps, no secret values.")
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
            if withData {
                VStack(alignment: .leading, spacing: 8) {
                    Label("The file will contain secret values in plain text", systemImage: "exclamationmark.triangle.fill")
                        .foregroundStyle(.orange).fontWeight(.medium)
                    Text("API keys, bot tokens and passwords (shared ones included), and conversation history that can hold client data. Keep it out of email, chat, git and shared drives, delete it once it is imported, and rotate the values if it leaks.")
                        .font(.callout).fixedSize(horizontal: false, vertical: true)
                    Toggle("I understand this file holds secrets", isOn: $understood)
                }
                .padding(12)
                .background(RoundedRectangle(cornerRadius: 8).fill(.orange.opacity(0.08)))
            }
            if let failure {
                Label(failure, systemImage: "xmark.octagon.fill").foregroundStyle(.red).textSelection(.enabled)
            }
            if let done {
                Label("Exported to \(done.path)", systemImage: "checkmark.circle.fill").foregroundStyle(.green).textSelection(.enabled)
            }
            HStack {
                if let done {
                    Button("Show in Finder") { NSWorkspace.shared.activateFileViewerSelecting([URL(filePath: done.path)]) }
                }
                Spacer()
                Button(done == nil ? "Cancel" : "Done") { dismiss() }
                    .keyboardShortcut(.cancelAction)
                if done == nil {
                    Button("Export…") { Task { await export() } }
                        .keyboardShortcut(.defaultAction)
                        .disabled(busy || (withData && !understood))
                }
            }
        }
        .padding(20)
        .frame(width: 480)
    }

    private func export() async {
        guard let cli = model.cli else { return }
        let panel = NSSavePanel()
        panel.nameFieldStringValue = "\(agentID)-\(withData ? "data" : "config").stormo-agent.zip"
        panel.allowedContentTypes = [.zip]
        panel.message = withData ? "This file will contain secret values." : ""
        guard panel.runModal() == .OK, let url = panel.url else { return }
        busy = true
        defer { busy = false }
        failure = nil
        var args = ["export", agentID, "-o", url.path]
        if withData { args.append("--data") }
        do {
            done = try await cli.run(args, as: ExportResult.self)
        } catch {
            failure = error.localizedDescription
        }
    }
}

/// Import…: an exported agent into this instance, under its own id or another, into its unit or
/// another; stormo checks it and keeps nothing if it does not check.
struct ImportAgentSheet: View {
    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss
    let file: URL
    let opened: (String) -> Void
    @State private var asID = ""
    @State private var unit = ""
    @State private var replace = false
    @State private var withActions = false
    @State private var busy = false
    @State private var failure: CLIError?
    @State private var done: ImportResult?

    private var units: [FleetUnit] { (model.fleet.fleet?.units ?? []).filter { $0.id != "group" } }

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            Text("Import \(file.lastPathComponent)").font(.title3).fontWeight(.semibold)
            Form {
                TextField("Agent id", text: $asID, prompt: Text("the exported id"))
                Picker("Unit", selection: $unit) {
                    Text("The exported unit").tag("")
                    ForEach(units) { Text($0.name).tag($0.id) }
                }
                Toggle("Replace an agent with this id", isOn: $replace)
                Toggle("Add bridge actions this instance lacks", isOn: $withActions)
            }
            .formStyle(.grouped)
            .scrollDisabled(true)
            .frame(height: 210)
            if let failure {
                Label(hint(failure), systemImage: "xmark.octagon.fill").foregroundStyle(.red).textSelection(.enabled)
                    .fixedSize(horizontal: false, vertical: true)
            }
            if let done {
                VStack(alignment: .leading, spacing: 4) {
                    Label("Imported \(done.agent) from \(done.from)", systemImage: "checkmark.circle.fill").foregroundStyle(.green)
                    ForEach(done.changes, id: \.self) { Text("• " + $0).font(.callout).foregroundStyle(.secondary) }
                }
            }
            HStack {
                Spacer()
                Button(done == nil ? "Cancel" : "Done") { dismiss() }.keyboardShortcut(.cancelAction)
                if let done {
                    Button("Open \(done.agent)") {
                        opened(done.agent)
                        dismiss()
                    }
                    .keyboardShortcut(.defaultAction)
                } else {
                    Button("Import") { Task { await run() } }
                        .keyboardShortcut(.defaultAction)
                        .disabled(busy)
                }
            }
        }
        .padding(20)
        .frame(width: 500)
    }

    private func hint(_ e: CLIError) -> String {
        switch e.code {
        case "exists": "\(e.message)\nTurn on Replace, or give it another id."
        case "missing_actions": "\(e.message)\nTurn on Add bridge actions to add them."
        default: e.errorDescription ?? e.message
        }
    }

    private func run() async {
        guard let cli = model.cli else { return }
        busy = true
        defer { busy = false }
        failure = nil
        var args = ["import", file.path]
        let id = asID.trimmingCharacters(in: .whitespaces)
        if !id.isEmpty { args += ["--as", id] }
        if !unit.isEmpty { args += ["--unit", unit] }
        if replace { args.append("--replace") }
        if withActions { args.append("--with-actions") }
        do {
            done = try await cli.run(args, as: ImportResult.self)
            await model.fleet.refresh()
        } catch let e as CLIError {
            failure = e
        } catch {
            failure = CLIError(code: "failed", message: error.localizedDescription, status: 1, stderr: "")
        }
    }
}
