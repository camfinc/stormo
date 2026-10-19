import Foundation
import Testing
@testable import StormoKit

@Suite struct FilesPaths {
    @Test func layerAndCrumbs() {
        #expect(FilesStore.layer(of: "/shared/sales/plans/q4.md") == "sales")
        #expect(FilesStore.layer(of: "/elsewhere/x") == nil)
        #expect(FilesStore.crumbs(of: "/shared/group/plans").map(\.path) == ["/shared/group", "/shared/group/plans"])
        #expect(FilesStore.crumbs(of: "/shared/group/plans").map(\.name) == ["group", "plans"])
    }

    @Test func entriesDecodeWithOmittedFields() throws {
        let es = try JSONDecoder().decode([WorkdirEntry].self, from: Data(#"[{"name":"plans","dir":true},{"name":"a.md","size":3,"lastWriter":"atlas","locked":"atlas (hard)"}]"#.utf8))
        #expect(es[0].dir && es[0].size == 0 && es[0].lastWriter == nil)
        #expect(!es[1].dir && es[1].size == 3 && es[1].locked == "atlas (hard)")
    }
}

@Suite(.serialized) struct RealFiles {
    @Test(.enabled(if: FileManager.default.isExecutableFile(atPath: RealBinary.binary.path)))
    @MainActor func browsesGroupAndUnitLayers() async throws {
        let s = try Scratch()
        let instance = s.url.appending(path: "acme")
        try FileManager.default.copyItem(at: RealBinary.engine.appending(path: "examples/minimal"), to: instance)
        let workdir = instance.appending(path: "workdir")
        try FileManager.default.createDirectory(at: workdir.appending(path: "group/plans"), withIntermediateDirectories: true)
        try FileManager.default.createDirectory(at: workdir.appending(path: "support"), withIntermediateDirectories: true)
        try "Q4: ship it.\n".write(to: workdir.appending(path: "group/plans/q4.md"), atomically: true, encoding: .utf8)
        try "support only\n".write(to: workdir.appending(path: "support/brief.md"), atomically: true, encoding: .utf8)

        let port = 18690 + Int.random(in: 0..<9)
        var env = ShellEnvironment.compose(shell: nil, process: ProcessInfo.processInfo.environment)
        env["SWARM_CORE_PORT"] = String(port)
        let cli = StormoCLI(executable: RealBinary.binary, environment: env, instance: instance)
        let supervisor = CoreSupervisor(client: CoreClient(port: port), instanceRoot: instance)
        await supervisor.start(with: cli)
        #expect(supervisor.state.isRunning, "\(supervisor.state) \(supervisor.lastError ?? "")")

        await #expect(throws: CoreClient.Failure.self) { try await CoreClient(port: port).workdirLayers(owner: "not-the-owner") }

        let store = FilesStore(client: CoreClient(port: port), instanceRoot: instance)
        await store.loadLayers()
        #expect(store.problem == nil, "\(store.problem ?? "")")
        #expect(store.layers == ["group", "sales", "support"])
        #expect(store.directory == "/shared/group" && store.entries.map(\.name) == ["plans"])

        await store.choose(try #require(store.entries.first))
        #expect(store.directory == "/shared/group/plans")
        await store.choose(try #require(store.entries.first { $0.name == "q4.md" }))
        #expect(store.file?.text == "Q4: ship it.\n" && store.info?.exists == true && store.selectedName == "q4.md")

        await store.open(directory: "/shared/support")
        #expect(store.entries.map(\.name) == ["brief.md"])
        await store.open(directory: "/shared/finance")
        #expect(store.problem?.contains("not a layer") == true)

        await supervisor.stop(with: cli)
    }
}
