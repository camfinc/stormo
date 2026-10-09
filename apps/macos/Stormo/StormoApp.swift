import StormoKit
import SwiftUI

@main
struct StormoApp: App {
    @State private var model = AppModel()

    init() {
        #if DEBUG
        DebugSnapshot.scheduleIfRequested()
        #endif
    }

    var body: some Scene {
        Window("Stormo", id: "main") {
            MainView()
                .environment(model)
                .task { await model.start() }
        }
        .defaultSize(width: 1120, height: 700)
        .commands { StormoCommands(model: model) }

        MenuBarExtra {
            MenuBarView()
                .environment(model)
        } label: {
            MenuBarLabel(summary: model.fleet.summary, running: model.core.state.isRunning)
        }
        .menuBarExtraStyle(.window)

        Settings {
            SettingsView()
                .environment(model)
        }
    }
}

struct StormoCommands: Commands {
    let model: AppModel
    @FocusedValue(\.openInstance) private var openInstance

    var body: some Commands {
        CommandGroup(replacing: .newItem) {
            Button("Open Instance…") { openInstance?() }
                .keyboardShortcut("o")
                .disabled(openInstance == nil)
        }
        CommandMenu("Core") {
            Button("Start Core") { Task { await model.startCore() } }
                .disabled(model.core.state != .down)
            Button("Restart Core") { Task { await model.restartCore() } }
                .disabled(!canRestart)
            Button("Stop Core") { Task { await model.stopCore() } }
                .disabled(!model.core.state.isRunning)
        }
    }
}

extension StormoCommands {
    private var canRestart: Bool {
        guard model.cli != nil else { return false }
        switch model.core.state {
        case .running, .incompatible: return true
        default: return false
        }
    }
}

extension FocusedValues {
    @Entry var openInstance: (() -> Void)?
}

struct MenuBarLabel: View {
    let summary: FleetSummary
    let running: Bool

    var body: some View {
        if running {
            Label("\(summary.working)", systemImage: summary.attention > 0 ? "tornado.circle.fill" : "tornado")
                .labelStyle(.titleAndIcon)
        } else {
            Image(systemName: "tornado")
        }
    }
}
