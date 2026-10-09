import ServiceManagement
import StormoKit
import SwiftUI

struct SettingsView: View {
    var body: some View {
        TabView {
            Tab("General", systemImage: "gearshape") { GeneralSettings() }
            Tab("Instances", systemImage: "square.stack.3d.up") { InstanceSettings() }
            Tab("Command Line", systemImage: "terminal") { CommandLineSettings() }
            Tab("Tools", systemImage: "wrench.and.screwdriver") { ToolSettings() }
        }
        .frame(width: 560, height: 420)
    }
}

struct GeneralSettings: View {
    @State private var openAtLogin = SMAppService.mainApp.status == .enabled
    @State private var problem: String?

    var body: some View {
        Form {
            Toggle("Open Stormo at login", isOn: $openAtLogin)
                .onChange(of: openAtLogin) { _, on in
                    do {
                        if on { try SMAppService.mainApp.register() } else { try SMAppService.mainApp.unregister() }
                        problem = nil
                    } catch {
                        problem = error.localizedDescription
                        openAtLogin = SMAppService.mainApp.status == .enabled
                    }
                }
            if let problem { Text(problem).foregroundStyle(.red) }
            Text("Quitting Stormo leaves the core running; agents keep working.")
                .foregroundStyle(.secondary)
        }
        .formStyle(.grouped)
    }
}

struct InstanceSettings: View {
    @Environment(AppModel.self) private var model
    @State private var importing = false

    var body: some View {
        Form {
            Section {
                ForEach(model.instances.instances) { record in
                    HStack {
                        VStack(alignment: .leading) {
                            Text(record.name)
                            PathText(path: record.root).font(.caption).foregroundStyle(.secondary)
                        }
                        Spacer()
                        if record.root == model.instances.active?.root {
                            Text("Active").foregroundStyle(.secondary)
                        } else {
                            Button("Activate") { Task { await model.activate(record) } }
                        }
                        Button("Remove", systemImage: "minus.circle", role: .destructive) { model.instances.remove(record.root) }
                            .labelStyle(.iconOnly)
                            .buttonStyle(.borderless)
                    }
                }
            } footer: {
                Button("Open Instance…") { importing = true }
            }
        }
        .formStyle(.grouped)
        .fileImporter(isPresented: $importing, allowedContentTypes: [.folder]) { result in
            if case .success(let url) = result { Task { await model.open(folder: url) } }
        }
    }
}

struct CommandLineSettings: View {
    @Environment(AppModel.self) private var model

    var body: some View {
        Form {
            Section {
                ForEach(model.candidates) { c in
                    HStack(alignment: .firstTextBaseline) {
                        Image(systemName: c.id == model.instances.active?.binary ? "checkmark.circle.fill" : "circle")
                            .foregroundStyle(c.usable ? Color.accentColor : .secondary)
                        VStack(alignment: .leading, spacing: 2) {
                            HStack {
                                Text(label(c.source)).fontWeight(.medium)
                                if let info = c.info { Text("\(info.version) · API \(info.api)").foregroundStyle(.secondary) }
                            }
                            PathText(path: c.resolved.path).font(.caption).foregroundStyle(.secondary)
                            if let problem = c.problem { Text(problem).font(.caption).foregroundStyle(.orange) }
                        }
                        Spacer()
                        if c.usable && c.id != model.instances.active?.binary {
                            Button("Use") { model.pin(c) }
                        }
                    }
                }
            } header: {
                Text("stormo for \(model.instances.active?.name ?? "this instance")")
            } footer: {
                Text("The pinned binary runs every command and starts the core, so it also decides the sidecar image agents get on their next start.")
                    .foregroundStyle(.secondary)
            }
            if model.canInstallCommandLineTool {
                Section {
                    Button("Install the stormo command") { Task { await model.installCommandLineTool() } }
                } footer: {
                    Text("Links ~/.local/bin/stormo to the copy inside Stormo, which updates with the app.")
                        .foregroundStyle(.secondary)
                }
            }
            Button("Look again") { Task { await model.resolveBinary() } }
        }
        .formStyle(.grouped)
    }

    private func label(_ s: BinaryCandidate.Source) -> String {
        switch s {
        case .bundled: "Inside Stormo"
        case .path: "On your PATH"
        case .usual: "Installed"
        case .vendored: "The instance's engine"
        case .custom: "Custom"
        }
    }
}

struct ToolSettings: View {
    @Environment(AppModel.self) private var model

    var body: some View {
        Form {
            if let env = model.environment {
                Section("Tools stormo uses") {
                    ForEach(["docker", "git", "aws"], id: \.self) { tool in
                        LabeledContent(tool) {
                            if let url = ShellEnvironment.which(tool, in: env) {
                                PathText(path: url.path)
                            } else {
                                Text("not found").foregroundStyle(.orange)
                            }
                        }
                    }
                }
                Section("Environment") {
                    ForEach(env.keys.sorted(), id: \.self) { key in
                        LabeledContent(key) { Text(env[key] ?? "").lineLimit(2).textSelection(.enabled).font(.caption.monospaced()) }
                    }
                }
            } else {
                ProgressView("Reading your shell's environment…")
            }
        }
        .formStyle(.grouped)
    }
}
