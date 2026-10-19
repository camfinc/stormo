import AppKit
import StormoKit

/// The standard About panel, with the engine versions this app is driving and the project links.
enum AboutPanel {
    static let project = URL(string: "https://github.com/camfinc/stormo")!

    static func show(model: AppModel) {
        let bundle = Bundle.main
        let marketing = bundle.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String ?? "0.0.0"
        let build = bundle.object(forInfoDictionaryKey: "CFBundleVersion") as? String ?? "1"
        let version = marketing == "0.0.0" ? "Development build (\(build))" : "Version \(marketing) (\(build))"

        var lines: [(String, String)] = []
        if let info = model.pinned?.info {
            lines.append(("Command line", "\(info.version) (API \(info.api))\(info.released ? "" : ", unreleased")"))
        } else {
            lines.append(("Command line", "none pinned"))
        }
        if let core = model.core.state.info {
            lines.append(("Core", "\(core.version), \(core.instance.name)"))
        }
        lines.append(("System", "macOS \(ProcessInfo.processInfo.operatingSystemVersionString)"))

        NSApp.activate()
        NSApp.orderFrontStandardAboutPanel(options: [
            .applicationVersion: version,
            .version: "",
            .credits: credits(lines),
        ])
    }

    private static func credits(_ lines: [(String, String)]) -> NSAttributedString {
        let center = NSMutableParagraphStyle()
        center.alignment = .center
        center.paragraphSpacing = 2
        let small = NSFont.systemFont(ofSize: NSFont.smallSystemFontSize)
        let plain: [NSAttributedString.Key: Any] = [.font: small, .foregroundColor: NSColor.labelColor, .paragraphStyle: center]
        let dim: [NSAttributedString.Key: Any] = [.font: small, .foregroundColor: NSColor.secondaryLabelColor, .paragraphStyle: center]

        let s = NSMutableAttributedString(string: "Run a team of AI agents like a small company.\n\n", attributes: plain)
        for (label, value) in lines {
            s.append(NSAttributedString(string: "\(label): ", attributes: dim))
            s.append(NSAttributedString(string: "\(value)\n", attributes: plain))
        }
        s.append(NSAttributedString(string: "\n", attributes: plain))
        for (title, path) in [("Website", ""), ("Releases", "/releases"), ("Report an issue", "/issues")] {
            var link = plain
            link[.link] = project.appendingPathComponent(path)
            s.append(NSAttributedString(string: title, attributes: link))
            if path != "/issues" { s.append(NSAttributedString(string: "  ·  ", attributes: dim)) }
        }
        return s
    }
}
