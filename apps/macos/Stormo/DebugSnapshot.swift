#if DEBUG
import AppKit

/// Debug builds only: with STORMO_SNAPSHOT=<dir>, the app writes each of its windows to
/// <dir>/<n>-<title>.png after a few seconds and quits. A visual check of the UI with no Screen
/// Recording permission (an app may always draw its own windows).
enum DebugSnapshot {
    static func allSubviews(_ v: NSView?) -> [NSView] {
        guard let v else { return [] }
        return [v] + v.subviews.flatMap(allSubviews)
    }

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
            // The office floor, as SpriteKit renders it (a window capture misses Metal content);
            // STORMO_SNAPSHOT_FRAMES=n also writes n more, two seconds apart, to catch people walking.
            let frames = Int(ProcessInfo.processInfo.environment["STORMO_SNAPSHOT_FRAMES"] ?? "") ?? 0
            for i in 0...frames {
                if i > 0 { try? await Task.sleep(for: .seconds(2)) }
                for window in NSApp.windows {
                    for view in allSubviews(window.contentView) {
                        if let sk = view as? OfficeSKView, let image = sk.floorImage() {
                            if i == 0, let plan = (sk.scene as? OfficeScene)?.plan {
                                print("office floor \(Int(plan.size.width))x\(Int(plan.size.height)) in view \(Int(sk.bounds.width))x\(Int(sk.bounds.height))")
                            }
                            let rep = NSBitmapImageRep(cgImage: image)
                            try? rep.representation(using: .png, properties: [:])?.write(to: out.appending(path: i == 0 ? "office-floor.png" : "office-floor-\(i).png"))
                        }
                    }
                }
            }
            NSApp.terminate(nil)
        }
    }
}
#endif
