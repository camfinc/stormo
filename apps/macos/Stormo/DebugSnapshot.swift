#if DEBUG
import AppKit

/// Debug builds only: with STORMO_SNAPSHOT=<dir>, the app writes each of its windows to
/// <dir>/<n>-<title>.png after a few seconds and quits. A visual check of the UI with no Screen
/// Recording permission (an app may always draw its own windows).
enum DebugSnapshot {
    static func scheduleIfRequested() {
        guard let dir = ProcessInfo.processInfo.environment["STORMO_SNAPSHOT"], !dir.isEmpty else { return }
        let delay = Double(ProcessInfo.processInfo.environment["STORMO_SNAPSHOT_DELAY"] ?? "") ?? 6
        Task { @MainActor in
            try? await Task.sleep(for: .seconds(delay))
            let out = URL(filePath: dir)
            try? FileManager.default.createDirectory(at: out, withIntermediateDirectories: true)
            for (i, window) in NSApp.windows.enumerated() where window.isVisible {
                guard let view = window.contentView?.superview ?? window.contentView,
                      let rep = view.bitmapImageRepForCachingDisplay(in: view.bounds) else { continue }
                view.cacheDisplay(in: view.bounds, to: rep)
                let name = window.title.isEmpty ? "window" : window.title.replacingOccurrences(of: "/", with: "-")
                try? rep.representation(using: .png, properties: [:])?.write(to: out.appending(path: "\(i)-\(name).png"))
            }
            NSApp.terminate(nil)
        }
    }
}
#endif
