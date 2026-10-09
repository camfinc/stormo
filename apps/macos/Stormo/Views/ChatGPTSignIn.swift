import AppKit
import StormoKit
import SwiftUI

/// The core's ChatGPT sign-in: who is signed in, the plan's state, and sign in / sign out, which run
/// `stormo core login` / `core logout` (the core is the only holder of the tokens; docs/core.md §1).
struct ChatGPTSection: View {
    @Environment(AppModel.self) private var model
    @State private var status: GatewayStatus?
    @State private var signingIn = false
    @State private var confirmSignOut = false
    @State private var failure: String?

    private var signedIn: Bool { (status?.login ?? model.fleet.fleet?.gateway.login) == "ok" }

    var body: some View {
        Section {
            LabeledContent("Sign-in") {
                if signedIn {
                    Text(status?.account.map { "You're using your ChatGPT plan (\($0))" } ?? "You're using your ChatGPT plan")
                } else {
                    Text(loginText(status?.login ?? model.fleet.fleet?.gateway.login)).foregroundStyle(.secondary)
                }
            }
            if let g = model.fleet.fleet?.gateway {
                if let until = g.planLimitedUntil.flatMap(Date.init(isoString:)) {
                    LabeledContent("Plan limit") { Text("until \(until, format: .dateTime.hour().minute())") }
                }
                LabeledContent("Model calls", value: "\(g.inflight)/\(g.concurrency) in flight, \(g.queued) queued")
                if let url = g.manageUsageUrl.flatMap(URL.init(string:)) {
                    Link("Manage usage", destination: url)
                }
            }
            if let failure {
                Text(failure).foregroundStyle(.red).textSelection(.enabled)
            }
            HStack {
                if signedIn {
                    Button("Sign Out…") { confirmSignOut = true }
                } else {
                    Button("Sign In with ChatGPT…") { signingIn = true }
                        .buttonStyle(.borderedProminent)
                }
            }
            .disabled(model.cli == nil)
            .task(id: model.core.state.isRunning) { await refresh() }
            .sheet(isPresented: $signingIn, onDismiss: { Task { await refresh() } }) {
                ChatGPTSignInSheet()
            }
            .confirmationDialog("Sign out of ChatGPT?", isPresented: $confirmSignOut) {
                Button("Sign Out", role: .destructive) { Task { await signOut() } }
            } message: {
                Text("Local agents on the core's gateway fall back to their cloud model until someone signs in again.")
            }
        } header: {
            SectionTitle("ChatGPT plan", note: "Local agents on the core's model gateway use this sign-in.")
        }
    }

    private func loginText(_ s: String?) -> String {
        switch s {
        case nil, "missing": "Not signed in"
        case "relogin_required": "Signed out by ChatGPT: sign in again"
        case let s?: s.replacingOccurrences(of: "_", with: " ")
        }
    }

    private func refresh() async {
        guard model.core.state.isRunning else { return }
        status = try? await model.fleet.client.gateway()
    }

    private func signOut() async {
        guard let cli = model.cli else { return }
        failure = nil
        do {
            _ = try await cli.run(["core", "logout"], as: LoginResult.self)
        } catch {
            failure = error.localizedDescription
        }
        await refresh()
        await model.fleet.refresh()
    }
}

/// "Continue in your browser": runs `core login --no-open`, opens the sign-in page it reports, and
/// waits for the browser to come back; Cancel stops the login.
struct ChatGPTSignInSheet: View {
    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss
    /// The ChatGPT connection to sign in to; nil is the core's default one.
    var connection: String?
    @State private var url: URL?
    @State private var task: Task<Void, Never>?
    @State private var result: LoginResult?
    @State private var failure: String?

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            Text(connection.map { "Sign in with ChatGPT (\($0))" } ?? "Sign in with ChatGPT").font(.title3).fontWeight(.semibold)
            if let result {
                Label("You're using your ChatGPT plan" + (result.account.map { " (\($0))" } ?? ""), systemImage: "checkmark.circle.fill")
                    .foregroundStyle(.green)
            } else if let failure {
                Label(failure, systemImage: "xmark.octagon.fill").foregroundStyle(.red).textSelection(.enabled)
            } else {
                HStack(spacing: 10) {
                    ProgressView().controlSize(.small)
                    Text(url == nil ? "Starting the sign-in…" : "Continue in your browser. This window closes itself when you are done.")
                }
                Text("The sign-in stays on this Mac: the core keeps the tokens in the instance's .swarm/core/auth. Whether your use qualifies for the ChatGPT plan is your call under OpenAI's Sign in with ChatGPT terms.")
                    .font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            }
            HStack {
                if let url, result == nil, failure == nil {
                    Button("Open the Page Again") { NSWorkspace.shared.open(url) }
                }
                Spacer()
                if result != nil || failure != nil {
                    Button("Done") { dismiss() }.keyboardShortcut(.defaultAction)
                } else {
                    Button("Cancel") {
                        task?.cancel()
                        dismiss()
                    }
                    .keyboardShortcut(.cancelAction)
                }
            }
        }
        .padding(20)
        .frame(width: 460)
        .onAppear { start() }
        .onDisappear { task?.cancel() }
    }

    private func start() {
        guard let cli = model.cli, task == nil else { return }
        task = Task {
            do {
                let args = ["core", "login", "--no-open"] + (connection.map { ["--connection", $0] } ?? [])
                let r = try await cli.run(args, as: LoginResult.self) { event in
                    if case .authURL(let s) = event, let u = URL(string: s) {
                        Task { @MainActor in
                            url = u
                            NSWorkspace.shared.open(u)
                        }
                    }
                }
                result = r
                await model.fleet.refresh()
                try? await Task.sleep(for: .seconds(1.5))
                if !Task.isCancelled { dismiss() }
            } catch is CancellationError {
            } catch {
                if !Task.isCancelled { failure = error.localizedDescription }
            }
        }
    }
}
