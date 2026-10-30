import Foundation
import Testing
@testable import StormoKit

@Suite struct Capabilities {
    // `capability show` as the CLI answers it (an example instance, one capability from a template).
    static let file = #"""
    {"path":"stormo.yaml","hash":"b4489c","capabilities":[{"name":"media.tools","spec":{"idle_minutes":5,"template":"media"},
     "effective":{"name":"media.tools","description":"Work on a video or audio file with ffmpeg.","kind":"service","template":"media",
      "image":"stormo-cap-media:2218ba945f89","placement":"local","port":8080,"health":"/health","idleMinutes":5,"bootMinutes":2,
      "memoryMB":2048,"docs":"Give one video.","files":"both","concurrency":2,"timeoutMinutes":20,"costUSD":0},"agents":["atlas"]}]}
    """#

    // `GET /api/capabilities` from a core that loaded the same file, its worker awake.
    static let core = #"""
    {"capabilities":[{"name":"media.tools","description":"Work on a video or audio file with ffmpeg.","kind":"service","template":"media",
      "image":"stormo-cap-media:2218ba945f89","placement":"local","port":8080,"health":"/health","idleMinutes":5,"bootMinutes":2,
      "memoryMB":2048,"files":"both","concurrency":2,"timeoutMinutes":20,"costUSD":0,"queued":0,"running":1,"agents":["atlas"],
      "worker":"awake","service":{"state":"awake","since":1793377784060,"lastUsed":1793377790000,"running":1}}],
     "spend":[{"agent":"atlas","capability":"media.tools","jobs":3,"failed":0,"usd":0}],"since":1790000000000}
    """#

    @Test func decodesTheFileAndTheCore() throws {
        let f = try JSONDecoder().decode(CapabilitiesFile.self, from: Data(Self.file.utf8))
        #expect(f.capabilities.first?.spec["template"] == .string("media"))
        #expect(f.capabilities.first?.effective.idleMinutes == 5 && f.capabilities.first?.agents == ["atlas"])
        let v = try JSONDecoder().decode(CapabilitiesView.self, from: Data(Self.core.utf8))
        let c = try #require(v.capabilities.first)
        #expect(c.worker == "awake" && c.service?.running == 1 && c.isLocalService)
    }

    @Test func anOlderCoreStillDecodes() throws {
        let old = #"{"capabilities":[{"name":"x","description":"","kind":"http","concurrency":1,"costUSD":0.4,"queued":0,"running":0,"agents":[]}],"spend":[]}"#
        let v = try JSONDecoder().decode(CapabilitiesView.self, from: Data(old.utf8))
        #expect(v.capabilities.first?.worker == nil && v.capabilities.first?.isLocalService == false)
    }

    @Test func aRestartIsPendingOnlyWhenTheFileDiffersFromTheCore() throws {
        let f = try JSONDecoder().decode(CapabilitiesFile.self, from: Data(Self.file.utf8))
        var v = try JSONDecoder().decode(CapabilitiesView.self, from: Data(Self.core.utf8))
        #expect(f.pendingRestart(core: v).isEmpty, "queue, grants and worker state are not settings")
        v.capabilities[0].idleMinutes = 10
        #expect(f.pendingRestart(core: v) == ["media.tools"])
        v.capabilities[0].idleMinutes = 5
        var extra = v.capabilities[0]
        extra.name = "web.browse"
        v.capabilities.append(extra)
        #expect(f.pendingRestart(core: v) == ["web.browse"], "removed from the file, still loaded")
        var g = f
        g.capabilities[0].name = "video.render"
        #expect(g.pendingRestart(core: try JSONDecoder().decode(CapabilitiesView.self, from: Data(Self.core.utf8))) == ["media.tools", "video.render"])
    }

    @Test func aJobWithItsOutput() throws {
        let json = #"""
        {"job":12,"capability":"media.tools","agent":"atlas","status":"running","preview":"{\"op\":\"qa\"}","costUSD":0,"created":1,"started":2,
         "inputs":["/shared/sales/clip.mp4"],"out":"/shared/sales/capabilities/media-tools/job-12","files":[],"notify":true,
         "output":{"duration_seconds":2,"files":["clip-1s.png"]},"note":"output cut at 64 KB"}
        """#
        let j = try JSONDecoder().decode(CapabilityJob.self, from: Data(json.utf8))
        #expect(j.isCancellable && j.inputs == ["/shared/sales/clip.mp4"])
        guard case .object(let o)? = j.output else { Issue.record("output"); return }
        #expect(o["duration_seconds"] == .int(2))
        let done = #"{"job":1,"capability":"x","agent":"a","status":"done","costUSD":0,"created":1}"#
        #expect(try JSONDecoder().decode(CapabilityJob.self, from: Data(done.utf8)).isCancellable == false)
    }

    @Test func agentFormOptionsAndTemplates() throws {
        let opts = #"""
        {"units":[],"actions":[],"skills":[],"personas":[],"engines":["needletail"],"channels":[],"allowBots":[],"secrets":[],
         "capabilities":[{"name":"media.tools","description":"ffmpeg"},{"name":"x.y","description":"","unavailable":"no template"}]}
        """#
        let o = try JSONDecoder().decode(AgentOptions.self, from: Data(opts.utf8))
        #expect(o.capabilities?.map(\.name) == ["media.tools", "x.y"] && o.capabilities?.last?.unavailable != nil)
        let t = try JSONDecoder().decode(CapabilityTemplate.self, from: Data(#"{"name":"browser","description":"Chromium","files":"both"}"#.utf8))
        #expect(t.suggestedName == "web.browse")
    }
}
