import StormoKit
import SwiftUI

struct CoreView: View {
    @Environment(AppModel.self) private var model

    var body: some View {
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
            Section("Command line") {
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
