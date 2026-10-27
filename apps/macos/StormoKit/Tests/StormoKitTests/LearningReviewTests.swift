import Foundation
import Testing
@testable import StormoKit

@Suite struct LearningReviewDecoding {
    @Test func snapshotPersistenceDoesNotInventEvidence() throws {
        let evidence = try JSONDecoder().decode(LearningEvidence.self, from: Data(#"{"revision":"r1","unknown":true,"attempts":0,"confirmations":0,"failures":0,"corrections":0,"evidenceTypes":null}"#.utf8))
        #expect(evidence.unknown && evidence.confirmations == 0 && evidence.evidenceTypes.isEmpty)
    }
    @Test func bridgeDecodesStatusEventsAndPrivateCandidate() throws {
        let body = #"""
        {"state":"ready","sessions":[{"id":"s1","messages":2,"ended":false}],"session":"s1","data":{
          "events":[{"eventId":"event1","taskRoot":"task1","subjectKind":"skill","subjectId":"subject1","subjectRevision":"revision1","at":"2026-10-26T00:00:00Z","audience":"agent","sourceType":"runtime","origin":"check1","executionStatus":"succeeded","taskOutcome":"unknown"}],
          "candidates":[{"id":"c1","text":"Check outcomes.","checks":"Assertion","conditions":"Before retrying","exceptions":"","audience":"dm:example","sensitivity":"private","reviewDate":"2026-11-26","action":"new","relatedIds":null,"eventRefs":["event1"],"root":"task1"}],
          "status":{"reflectionEnabled":false,"callsUsed":1,"callsLimit":100,"eventsUsed":1,"eventsLimit":4096,"candidatesUsed":1,"candidatesLimit":100,"exportsUsed":0,"exportsLimit":100,"exportedIds":[],"feedbackAllowed":true,"exportAllowed":false}}}
        """#
        let view = try JSONDecoder().decode(LearningReviewView.self, from: Data(body.utf8))
        let data = try #require(view.data)
        #expect(view.session == "s1" && data.events[0].acceptsFeedback)
        #expect(data.events[0].executionStatus == "succeeded" && data.events[0].taskOutcome == "unknown")
        #expect(!data.candidates[0].canExport && data.candidates[0].relatedIds.isEmpty)
        #expect(!data.status.reflectionEnabled && data.status.callsLimit == 100)
    }
    @Test func olderBackgroundPayloadDecodesWithoutNewControls() throws {
        let background = try JSONDecoder().decode(BackgroundLearningReview.self, from: Data(#"{"enabled":true,"pending":1,"completed":0,"failed":0,"interrupted":0,"callsUsed":1,"tokensReserved":512}"#.utf8))
        #expect(background.callsLimit == 8 && background.tokensLimit == 4096)
        #expect(background.jobs == nil && background.pending == 1)
    }

    @Test func controlRequestBindsExactRevisionWithoutFeedbackFields() throws {
        let request = LearningFeedback.control("approve_patch", id: "patch-1", revision: "base-r")
        let body = try #require(try JSONSerialization.jsonObject(with: JSONEncoder().encode(request)) as? [String: String])
        #expect(body == ["action": "approve_patch", "id": "patch-1", "expectedRevision": "base-r"])
        let prune = try #require(try JSONSerialization.jsonObject(with: JSONEncoder().encode(LearningFeedback.control("prune_jobs"))) as? [String: String])
        #expect(prune == ["action": "prune_jobs"])
        let patch = try JSONDecoder().decode(LearningSkillPatch.self, from: Data(#"{"id":"p","name":"example","status":"conflict","before":"old","after":"new","expectedRevision":"base","currentRevision":"changed","audience":"agent","createdAt":"2026-10-27","canApprove":false,"canRollback":false}"#.utf8))
        #expect(!patch.canApprove && patch.expectedRevision != patch.currentRevision)
    }

    @Test func feedbackUsesStableOpaqueOriginAndBoundsUnicodeSummary() throws {
        let origin = LearningFeedback.origin(for: "event1")
        #expect(origin.count == 71 && origin.allSatisfy { $0.isLetter || $0.isNumber || $0 == "-" })
        #expect(origin == LearningFeedback.origin(for: "event1") && origin != LearningFeedback.origin(for: "event2"))
        let summary = LearningFeedback.boundedSummary(String(repeating: "corrección ✅ ", count: 400))
        #expect(summary.utf8.count <= 2000 && !summary.isEmpty)
        let request = LearningFeedback.feedback(event: "event1", outcome: "failed", origin: origin, summary: summary)
        let first = try JSONEncoder().encode(request), retry = try JSONEncoder().encode(request)
        let a = try #require(try JSONSerialization.jsonObject(with: first) as? [String: String])
        let b = try #require(try JSONSerialization.jsonObject(with: retry) as? [String: String])
        #expect(a == b && a["origin"] == origin && a["outcome"] == "failed" && a["exportId"] == nil)
        let exported = try #require(try JSONSerialization.jsonObject(with: JSONEncoder().encode(LearningFeedback.export("c1"))) as? [String: String])
        #expect(exported == ["exportId": "c1"])
    }
}
