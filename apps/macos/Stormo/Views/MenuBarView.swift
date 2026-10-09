import StormoKit
import SwiftUI

struct MenuBarView: View {
    @Environment(AppModel.self) private var model
    @Environment(\.openWindow) private var openWindow

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack {
                Text(model.instances.active?.name ?? "Stormo").font(.headline)
                Spacer()
                CoreStatusBadge(state: model.core.state)
            }
            if model.core.state.isRunning {
                let s = model.fleet.summary
                HStack(spacing: 14) {
                    Count(value: s.onShift, label: "On shift")
                    Count(value: s.working, label: "Working")
                    Count(value: s.attention, label: "Attention", tint: s.attention > 0 ? .red : nil)
                    Count(value: s.toReview, label: "To review")
                }
                Divider()
                ForEach(model.fleet.fleet?.agents ?? []) { a in
                    HStack(spacing: 8) {
                        StateDot(agent: a)
                        Text(a.name).frame(width: 80, alignment: .leading)
                        Text(a.statusLine).foregroundStyle(.secondary).lineLimit(1)
                        Spacer()
                    }
                    .font(.callout)
                }
            } else {
                CoreToggleButton()
            }
            Divider()
            HStack {
                Button("Open Stormo") {
                    openWindow(id: "main")
                    NSApp.activate()
                }
                Spacer()
                SettingsLink { Text("Settings…") }
                Button("Quit") { NSApp.terminate(nil) }
            }
        }
        .padding(14)
        .frame(width: 340)
    }
}

private struct Count: View {
    let value: Int
    let label: String
    var tint: Color?

    var body: some View {
        VStack(spacing: 2) {
            Text("\(value)").font(.title3.monospacedDigit()).fontWeight(.semibold).foregroundStyle(tint ?? .primary)
            Text(label).font(.caption).foregroundStyle(.secondary)
        }
    }
}
