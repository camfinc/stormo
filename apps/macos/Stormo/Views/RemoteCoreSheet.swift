import StormoKit
import SwiftUI

/// Connect to a core that runs elsewhere (a VPS, ECS…) by URL and token. Reaching it (public URL,
/// tunnel, VPN) is the network's business; the app only needs an address that answers.
struct RemoteCoreSheet: View {
    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss
    @State private var url = ""
    @State private var token = ""
    @State private var working = false
    @State private var problem: String?
    @State private var untrusted: String?

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            Text("Connect to a Remote Core").font(.title2).fontWeight(.semibold)
            Text("A core on another machine, reached by its URL. Make a token on it with `stormo core token add <name> read` (or operate, to act).")
                .font(.callout).foregroundStyle(.secondary)
            Form {
                TextField("URL", text: $url, prompt: Text("https://core.example:18600"))
                SecureField("Token", text: $token, prompt: Text("stc_…"))
            }
            .formStyle(.grouped)
            .disabled(working)
            if let untrusted {
                VStack(alignment: .leading, spacing: 6) {
                    Label("This Mac does not trust the core's certificate.", systemImage: "lock.trianglebadge.exclamationmark")
                        .foregroundStyle(.orange)
                    Text("Trust it only if this fingerprint matches the core's (`openssl x509 -noout -fingerprint -sha256 -in <cert>` where it runs):")
                        .font(.callout).foregroundStyle(.secondary)
                    Text(untrusted).font(.caption.monospaced()).textSelection(.enabled)
                }
            }
            if let problem {
                Label(problem, systemImage: "exclamationmark.triangle.fill").foregroundStyle(.red).textSelection(.enabled)
            }
            HStack {
                Spacer()
                Button("Cancel", role: .cancel) { dismiss() }
                if let untrusted {
                    Button("Trust and Connect") { connect(pin: untrusted) }.disabled(working)
                } else {
                    Button("Connect") { connect(pin: nil) }
                        .keyboardShortcut(.defaultAction)
                        .disabled(working || url.isEmpty || token.isEmpty)
                }
                if working { ProgressView().controlSize(.small) }
            }
        }
        .padding(20)
        .frame(width: 480)
    }

    private func connect(pin: String?) {
        working = true
        problem = nil
        Task {
            defer { working = false }
            switch await model.connectRemote(url: url, token: token.trimmingCharacters(in: .whitespacesAndNewlines), pin: pin) {
            case .connected: dismiss()
            case .untrusted(let fp): untrusted = fp
            case .failed(let why):
                problem = why
                untrusted = nil
            }
        }
    }
}
