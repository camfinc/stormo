import StormoKit
import SwiftUI

/// A page pushed in the Core section.
enum CoreRoute: String, Hashable {
    case devices
}

struct CoreView: View {
    @Environment(AppModel.self) private var model
    @State private var path: [CoreRoute] = Self.initialPath

    var body: some View {
        NavigationStack(path: $path) {
            form
                .navigationDestination(for: CoreRoute.self) { route in
                    switch route {
                    case .devices: LinkedDevicesView()
                    }
                }
        }
    }

    /// Debug builds can open a page at launch (STORMO_CORE_PAGE=devices), for a visual check.
    private static var initialPath: [CoreRoute] {
        #if DEBUG
        if let r = ProcessInfo.processInfo.environment["STORMO_CORE_PAGE"].flatMap(CoreRoute.init(rawValue:)) { return [r] }
        #endif
        return []
    }

    private var devicesSummary: String {
        let d = model.devices
        guard model.core.state.isRunning else { return "" }
        guard d.loaded else { return d.problem == nil ? "" : "Unavailable" }
        var parts = ["\(d.linked.count) linked"]
        if !d.pairings.isEmpty { parts.append("\(d.pairings.count) waiting for a code") }
        return parts.joined(separator: " · ")
    }

    private var form: some View {
        Form {
            Section {
                LabeledContent("Status") { CoreStatusBadge(state: model.core.state) }
                if let c = model.core.state.info {
                    LabeledContent("Version", value: c.version)
                    LabeledContent("Instance", value: c.instance.name)
                    LabeledContent("Binary") { PathText(path: c.exe) }
                    LabeledContent("Process", value: "pid \(c.pid), port \(c.port)")
                    if let started = Date(isoString: c.startedAt) {
                        LabeledContent("Started") { Text(started, format: .relative(presentation: .named)) }
                    }
                }
                if model.core.isOutdated(comparedTo: model.pinned?.info) {
                    Text("This core is an older build of the pinned binary. Restart it to update.")
                        .foregroundStyle(.orange)
                }
                if model.core.runsOtherBinary(than: model.pinned?.info), let pinned = model.pinned {
                    Text("This core was started by another stormo than the pinned one (\(pinned.resolved.path)). Restart it to run the pinned one.")
                        .foregroundStyle(.secondary)
                }
                if case .incompatible = model.core.state {
                    Text("This core is older than the app and cannot be read. Restart it with the pinned stormo.")
                        .foregroundStyle(.orange)
                }
                HStack {
                    CoreToggleButton()
                    if model.core.state.isRunning, !model.core.isOutdated(comparedTo: model.pinned?.info) {
                        RestartCoreButton()
                    }
                    if case .otherInstance(let c) = model.core.state {
                        Text("Stop it from \(c.instance.name)'s folder, or with `stormo core down` there.")
                            .foregroundStyle(.secondary)
                    }
                }
            }
            ChatGPTSection()
            Section {
                NavigationLink(value: CoreRoute.devices) {
                    LabeledContent("Linked devices") { Text(devicesSummary) }
                }
                .task(id: model.core.state.isRunning) {
                    if model.core.state.isRunning { await model.devices.load() }
                }
            } header: {
                SectionTitle("Devices", note: "Watches and phones that reach the fleet through the device gateway, with what each may do.")
            }
            Section("Command line") {
                if let note = model.commandLineNote { CommandLineNote(note: note) }
                if let p = model.pinned, let info = p.info {
                    LabeledContent("Pinned binary") { PathText(path: p.resolved.path) }
                    LabeledContent("Version", value: "\(info.version) (API \(info.api))")
                } else {
                    Text("No usable stormo binary is pinned for this instance.").foregroundStyle(.orange)
                }
                SettingsLink { Text("Command line settings…") }
            }
        }
        .formStyle(.grouped)
        .navigationTitle("Core")
    }
}

/// A path that shortens the home directory and can be revealed in Finder.
struct PathText: View {
    let path: String

    var body: some View {
        let home = FileManager.default.homeDirectoryForCurrentUser.path
        Text(path.hasPrefix(home) ? "~" + path.dropFirst(home.count) : path)
            .textSelection(.enabled)
            .lineLimit(1)
            .truncationMode(.middle)
            .contextMenu {
                Button("Show in Finder") { NSWorkspace.shared.activateFileViewerSelecting([URL(filePath: path)]) }
            }
    }
}
