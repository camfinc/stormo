import Foundation
import CryptoKit

/// Exact-revision evidence, separate from snapshot persistence and tool activity.
public struct LearningEvidence: Codable, Sendable, Equatable {
    public var revision: String
    public var unknown: Bool
    public var attempts: Int
    public var confirmations: Int
    public var failures: Int
    public var corrections: Int
    public var evidenceTypes: [String]
    public var validatedAt: String?
    enum CodingKeys: String, CodingKey { case revision, unknown, attempts, confirmations, failures, corrections, evidenceTypes, validatedAt }
    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        revision = try c.decodeIfPresent(String.self, forKey: .revision) ?? ""
        unknown = try c.decodeIfPresent(Bool.self, forKey: .unknown) ?? true
        attempts = try c.decodeIfPresent(Int.self, forKey: .attempts) ?? 0
        confirmations = try c.decodeIfPresent(Int.self, forKey: .confirmations) ?? 0
        failures = try c.decodeIfPresent(Int.self, forKey: .failures) ?? 0
        corrections = try c.decodeIfPresent(Int.self, forKey: .corrections) ?? 0
        evidenceTypes = try c.decodeIfPresent([String].self, forKey: .evidenceTypes) ?? []
        validatedAt = try c.decodeIfPresent(String.self, forKey: .validatedAt)
    }
}

/// Owner-only core bridge. Runtime credentials never cross this boundary.
public struct LearningReviewView: Codable, Sendable, Equatable {
    public var state: String
    public var message: String?
    public var sessions: [Conversation]
    public var session: String?
    public var data: LearningReviewData?
}
public struct LearningReviewData: Codable, Sendable, Equatable {
    public var events: [LearningOutcome]
    public var candidates: [LearningCandidate]
    public var status: LearningReviewStatus
    public var operation: LearningReviewOperation?
}
public struct LearningReviewOperation: Codable, Sendable, Equatable {
    public var ok: Bool
    public var code: String
    public var message: String
    public var feedbackSaved: Bool
}
public struct LearningReviewStatus: Codable, Sendable, Equatable {
    public var reflectionEnabled: Bool
    public var callsUsed: Int
    public var callsLimit: Int
    public var eventsUsed: Int
    public var eventsLimit: Int
    public var candidatesUsed: Int
    public var candidatesLimit: Int
    public var exportsUsed: Int
    public var exportsLimit: Int
    public var exportedIds: [String]
    public var feedbackAllowed: Bool
    public var exportAllowed: Bool
}
public struct LearningOutcome: Codable, Sendable, Equatable, Identifiable {
    public var eventId: String
    public var taskRoot: String
    public var subjectKind: String
    public var subjectId: String
    public var subjectRevision: String
    public var at: String
    public var audience: String
    public var sourceType: String
    public var origin: String
    public var executionStatus: String
    public var taskOutcome: String
    public var evidenceRef: String?
    public var id: String { eventId }
    public var acceptsFeedback: Bool { sourceType == "runtime" || sourceType == "view" }
}
public struct LearningCandidate: Codable, Sendable, Equatable, Identifiable {
    public var id: String
    public var text: String
    public var previousMistake: String?
    public var checks: String
    public var conditions: String
    public var exceptions: String
    public var audience: String
    public var sensitivity: String
    public var reviewDate: String
    public var action: String
    public var relatedIds: [String]
    public var eventRefs: [String]
    public var root: String
    public var canExport: Bool { audience == "agent" && (sensitivity == "public" || sensitivity == "procedural") }
    enum CodingKeys: String, CodingKey { case id, text, previousMistake, checks, conditions, exceptions, audience, sensitivity, reviewDate, action, relatedIds, eventRefs, root }
    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        text = try c.decode(String.self, forKey: .text)
        previousMistake = try c.decodeIfPresent(String.self, forKey: .previousMistake)
        checks = try c.decode(String.self, forKey: .checks)
        conditions = try c.decode(String.self, forKey: .conditions)
        exceptions = try c.decodeIfPresent(String.self, forKey: .exceptions) ?? ""
        audience = try c.decode(String.self, forKey: .audience)
        sensitivity = try c.decode(String.self, forKey: .sensitivity)
        reviewDate = try c.decode(String.self, forKey: .reviewDate)
        action = try c.decode(String.self, forKey: .action)
        relatedIds = try c.decodeIfPresent([String].self, forKey: .relatedIds) ?? []
        eventRefs = try c.decodeIfPresent([String].self, forKey: .eventRefs) ?? []
        root = try c.decode(String.self, forKey: .root)
    }

}
public struct LearningFeedback: Codable, Sendable, Equatable {
    public var eventId: String?
    public var outcome: String?
    public var origin: String?
    public var summary: String?
    public var exportId: String?

    public static func feedback(event: String, outcome: String, origin: String, summary: String) -> Self {
        Self(eventId: event, outcome: outcome, origin: origin, summary: summary)
    }
    public static func export(_ id: String) -> Self { Self(exportId: id) }
    /// One opaque owner-feedback source per observed event, stable across reloads and
    /// app restarts. Repeating this check cannot manufacture independent corroboration.
    public static func origin(for eventID: String) -> String {
        "native-" + SHA256.hash(data: Data(eventID.utf8)).map { String(format: "%02x", $0) }.joined()
    }
    public static func boundedSummary(_ text: String) -> String {
        var result = ""
        for character in text {
            guard result.utf8.count + String(character).utf8.count <= 2000 else { break }
            result.append(character)
        }
        return result
    }
}
