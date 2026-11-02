import SwiftUI
import CoreImage.CIFilterBuiltins
import StormoKit

struct WhatsAppSetup: View {
    @Environment(AppModel.self) private var model
    let agentID: String
    @State private var status: WhatsAppStatus?
    @State private var problem: String?
    @State private var busy = false
    @State private var confirmUnlink = false

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            Label(status?.state.replacingOccurrences(of: "_", with: " ").capitalized ?? "Not linked", systemImage: "message")
            Text("Save your channel settings, then link a dedicated WhatsApp account. The agent must run locally on this core. Keep both running for replies. Direct text chats only.")
                .font(.callout).foregroundStyle(.secondary)
            if let code = status?.qr, let image = qrImage(code) {
                Image(nsImage: image).interpolation(.none).resizable().frame(width: 240, height: 240)
                    .padding(12).background(.white)
                    .accessibilityLabel("WhatsApp device pairing QR code")
                Text("On the bot’s phone, open WhatsApp → Settings → Linked devices → Link a device, then scan this code.")
                    .font(.callout)
            }
            if let error = problem ?? status?.error {
                Label(error, systemImage: "exclamationmark.triangle").foregroundStyle(.orange).font(.callout)
            }
            HStack {
                Button(status?.state == "connected" ? "Refresh status" : "Link / reconnect") {
                    Task { await update(status?.state == "connected" ? "GET" : "POST") }
                }.disabled(busy || !model.core.state.isRunning)
                Button("Unlink", role: .destructive) { confirmUnlink = true }
                    .disabled(busy || !model.core.state.isRunning)
                if busy { ProgressView().controlSize(.small) }
            }
            Text("Unofficial WhatsApp integration. WhatsApp may restrict linked accounts. Use a dedicated number and allow only the people who should control this agent.")
                .font(.caption).foregroundStyle(.secondary)
        }
        .confirmationDialog("Unlink this agent’s WhatsApp account?", isPresented: $confirmUnlink) {
            Button("Unlink WhatsApp", role: .destructive) { Task { await update("DELETE") } }
        } message: { Text("Replies stop and the saved device session is removed. You can link again by scanning a new code.") }
        .task(id: agentID) {
            while !Task.isCancelled {
                if !busy && model.core.state.isRunning { await update("GET", polling: true) }
                do { try await Task.sleep(for: .seconds(3)) } catch { break }
            }
        }
    }

    @MainActor private func update(_ method: String, polling: Bool = false) async {
        if !polling { busy = true }
        defer { if !polling { busy = false } }
        do {
            status = try await model.slack.client.with(timeout: 30).whatsApp(agent: agentID, method: method)
            problem = nil
        } catch { problem = error.localizedDescription }
    }

    private func qrImage(_ code: String) -> NSImage? {
        let filter = CIFilter.qrCodeGenerator()
        filter.message = Data(code.utf8)
        guard let output = filter.outputImage,
              let cg = CIContext().createCGImage(output, from: output.extent) else { return nil }
        return NSImage(cgImage: cg, size: NSSize(width: cg.width, height: cg.height))
    }
}
