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
    @State private var creating = false

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
                HStack {
                    Button("New Instance…") { creating = true }
                    Button("Open Instance…") { importing = true }
                }
            }
        }
        .formStyle(.grouped)
        .sheet(isPresented: $creating) { NewInstanceSheet().environment(model) }
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
                VStack(alignment: .leading, spacing: 6) {
                    if model.followsAuto {
                        Text("Auto: Stormo's own copy, which updates with the app; another binary only when it cannot run. Pick one above to keep it.")
                    } else {
                        HStack {
                            Text("Picked here: stays until you change it.")
                            Button("Back to Auto") { model.useAuto() }
                        }
                    }
                    Text("The pinned binary runs every command and starts the core, so it also decides the sidecar image agents get on their next start.")
                }
                .foregroundStyle(.secondary)
            }
            TerminalSection()
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

/// The terminal's stormo next to the app's, and the way to make the terminal use the app's.
struct TerminalSection: View {
    @Environment(AppModel.self) private var model
    @State private var confirming = false

    var body: some View {
        Section {
            if let note = model.commandLineNote { CommandLineNote(note: note) }
            LabeledContent("Inside Stormo") { versioned(bundled) }
            LabeledContent("`stormo` in the terminal") {
                if let t = model.terminalStormo {
                    VStack(alignment: .trailing, spacing: 2) {
                        versioned(t)
                        if t.url.path != t.resolved.path { PathText(path: t.url.path).font(.caption).foregroundStyle(.secondary) }
                    }
                } else {
                    Text("none on your shell's PATH").foregroundStyle(.secondary)
                }
            }
            LabeledContent("~/.local/bin/stormo") { Text(linkText).foregroundStyle(.secondary).textSelection(.enabled) }
            if let t = model.terminalStormo, case .current = model.linkState,
               t.url.standardizedFileURL.path != AppModel.commandLineLink.path {
                Text("\(t.url.path) comes first on your shell's PATH, so the terminal still runs it.")
                    .font(.caption).foregroundStyle(.orange)
            }
            if case .current = model.linkState, let env = model.environment, let link = model.terminalLink, !link.directoryIsOnPath(env) {
                Text("~/.local/bin is not on your shell's PATH: add `export PATH=\"$HOME/.local/bin:$PATH\"` to your shell profile.")
                    .font(.caption).foregroundStyle(.orange)
            }
            if let problem = model.linkProblem { Text(problem).font(.caption).foregroundStyle(.orange) }
            if offersReplace {
                Button("Use Stormo's copy in the terminal…") { confirming = true }
            }
        } header: {
            Text("Terminal")
        } footer: {
            Text("Stormo links ~/.local/bin/stormo to its own copy when nothing is there, and moves a link to an older Stormo app to this one, so the terminal's stormo updates with the app. Anything else there is yours and changes only here.")
                .foregroundStyle(.secondary)
        }
        .confirmationDialog("Use Stormo's copy in the terminal?", isPresented: $confirming) {
            Button("Replace") { Task { await model.useStormoInTerminal() } }
        } message: {
            Text("\(replaced) It will become a link to \(AppModel.bundledBinary.path), which updates with the app.")
        }
    }

    private var bundled: BinaryCandidate? { model.candidates.first { $0.source == .bundled } }

    /// The link is not this app's, and the app has a binary to link to.
    private var offersReplace: Bool {
        guard model.terminalLink != nil, !CommandLineLink.isEphemeral(AppModel.bundledBinary), let state = model.linkState else { return false }
        if case .current = state { return false }
        return true
    }

    private var linkText: String {
        switch model.linkState {
        case nil: "—"
        case .absent: "nothing there"
        case .current: "links to the copy inside Stormo"
        case .appCopy(let target): "links to another Stormo app: \(target)"
        case .userLink(let target, let broken): "links to \(target)\(broken ? " (missing)" : "")"
        case .file: "a file, not a link"
        }
    }

    /// Names what the replacement removes.
    private var replaced: String {
        switch model.linkState {
        case .userLink(let target, _): "~/.local/bin/stormo now links to \(target); that link is replaced (the file it points to stays)."
        case .appCopy(let target): "~/.local/bin/stormo now links to \(target); that link is replaced."
        case .file: "~/.local/bin/stormo is a file, not a link (installed by hand or by another tool); it is deleted."
        case .absent, .current, nil: "Nothing is at ~/.local/bin/stormo yet."
        }
    }

    @ViewBuilder private func versioned(_ c: BinaryCandidate?) -> some View {
        if let c {
            VStack(alignment: .trailing, spacing: 2) {
                if let info = c.info { Text("\(info.version) · API \(info.api)") } else if let p = c.problem { Text(p).foregroundStyle(.orange) }
                PathText(path: c.resolved.path).font(.caption).foregroundStyle(.secondary)
            }
        } else {
            Text("not found").foregroundStyle(.secondary)
        }
    }
}

/// The once-only note that the app put stormo on the terminal.
struct CommandLineNote: View {
    @Environment(AppModel.self) private var model
    let note: String

    var body: some View {
        HStack(alignment: .firstTextBaseline) {
            Image(systemName: "terminal").foregroundStyle(.tint)
            Text(LocalizedStringKey(note)).textSelection(.enabled)
            Spacer()
            Button("Dismiss", systemImage: "xmark") { model.commandLineNote = nil }
                .labelStyle(.iconOnly)
                .buttonStyle(.borderless)
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
