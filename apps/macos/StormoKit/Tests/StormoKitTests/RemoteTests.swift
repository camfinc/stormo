import Foundation
import Testing

@testable import StormoKit

@Suite struct RemoteCores {
    @Test func aRemoteCoreIsRecognisedBySlug() throws {
        let health = try JSONDecoder().decode(Health.self, from: try fixture("health"))
        let core = try JSONDecoder().decode(CoreInfo.self, from: try fixture("api_core"))
        #expect(CoreState.classify(health: health, core: core, instanceRoot: nil, remoteSlug: core.instance.slug).isRunning)
        if case .otherInstance = CoreState.classify(health: health, core: core, instanceRoot: nil, remoteSlug: "someone-else") {} else {
            Issue.record("a core of another slug is another instance")
        }
    }

    @Test func remoteRecordsHaveNoFolder() {
        let r = InstanceRecord(root: InstanceRecord.remoteID("https://core.example:18600"), name: "Acme", slug: "acme", remote: "https://core.example:18600")
        #expect(r.isRemote && r.folder == nil && r.id == "remote:https://core.example:18600")
    }

    @Test func fingerprintsAreColonHex() {
        let fp = PinningDelegate.fingerprint(Data("x".utf8))
        #expect(fp.count == 32 * 3 - 1 && fp.split(separator: ":").count == 32)
    }
}

/// A token minted on a real core (this checkout's bin/stormo) opens what the loopback trust does not.
@Suite(.serialized) struct RealRemote {
    @Test(.enabled(if: FileManager.default.isExecutableFile(atPath: RealBinary.binary.path)))
    @MainActor func tokensReachOwnerRoutes() async throws {
        let s = try Scratch()
        let instance = s.url.appending(path: "acme")
        try FileManager.default.copyItem(at: RealBinary.engine.appending(path: "examples/minimal"), to: instance)
        let port = 18670 + Int.random(in: 0..<9)
        var env = ShellEnvironment.compose(shell: nil, process: ProcessInfo.processInfo.environment)
        env["SWARM_CORE_PORT"] = String(port)
        env["STORMO_HOME"] = s.url.appending(path: "home").path
        let cli = StormoCLI(executable: RealBinary.binary, environment: env, instance: instance)
        let supervisor = CoreSupervisor(client: CoreClient(port: port), instanceRoot: instance)
        await supervisor.start(with: cli)
        #expect(supervisor.state.isRunning, "\(supervisor.state) \(supervisor.lastError ?? "")")
        defer { Task { await supervisor.stop(with: cli) } }

        let owner = try #require(ReviewStore.ownerToken(root: instance))
        let anonymous = CoreClient(port: port)
        await #expect(throws: CoreClient.Failure.self) { try await anonymous.review(owner: "stc_000000000000_" + String(repeating: "0", count: 48)) }

        // As a remote client would: by slug, with a personal read token.
        struct Minted: Decodable { let token: String }
        var req = URLRequest(url: URL(string: "http://127.0.0.1:\(port)/api/tokens")!)
        req.httpMethod = "POST"
        req.setValue("Bearer \(owner)", forHTTPHeaderField: "Authorization")
        req.httpBody = Data(#"{"name":"tester","scope":"read","days":1}"#.utf8)
        let (data, _) = try await URLSession.shared.data(for: req)
        let token = try JSONDecoder().decode(Minted.self, from: data).token
        let remote = CoreSupervisor(client: CoreClient(base: URL(string: "http://127.0.0.1:\(port)")!, token: token), instanceRoot: nil)
        remote.remoteSlug = "acme"
        await remote.refresh()
        #expect(remote.state.isRunning, "\(remote.state)")
        let view = try await remote.client.review(owner: token)
        #expect(view.agents.contains { $0.id == "atlas" })
    }
}
