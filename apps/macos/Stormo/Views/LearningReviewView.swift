import StormoKit
import SwiftUI

/// Deliberate owner feedback and proposal selection, through the core only.
struct AgentLearningReview: View {
    @Environment(AppModel.self) private var model
    let agent: ReviewAgent
    @State private var selectedSession = ""
    private var review: ReviewStore { model.review }
    private var view: LearningReviewView? { review.learningViews[agent.id] }
    private var busy: Bool { review.learningLoading.contains(agent.id) || review.learningPending.contains(agent.id) }

    var body: some View {
        Section {
            HStack {
                Text("Task outcomes and reflection").fontWeight(.medium)
                Spacer()
                if busy { ProgressView().controlSize(.small).accessibilityLabel("Loading learning review") }
                Button("Reload", systemImage: "arrow.clockwise") {
                    Task { await review.loadLearning(agent: agent.id, session: selectedSession.isEmpty ? nil : selectedSession) }
                }.disabled(busy)
            }
            if let problem = review.learningProblems[agent.id] {
                Label(problem, systemImage: "exclamationmark.triangle").foregroundStyle(.orange)
                    .fixedSize(horizontal: false, vertical: true)
            }
            if let view {
                if view.state != "ready" {
                    Label(view.message ?? unavailableMessage(view.state), systemImage: "info.circle")
                        .foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                } else if view.sessions.isEmpty {
                    Text("No sessions yet. Run a task before recording feedback.").foregroundStyle(.secondary)
                } else {
                    Picker("Session", selection: $selectedSession) {
                        Text("Choose a session").tag("")
                        ForEach(view.sessions) { session in
                            Text("\(session.displayTitle.prefix(70)) · \(session.sourceLabel)").tag(session.id)
                        }
                    }
                    .disabled(busy)
                    .onChange(of: selectedSession) { _, session in
                        Task { await review.loadLearning(agent: agent.id, session: session.isEmpty ? nil : session) }
                    }
                    if let data = view.data, view.session == selectedSession {
                        status(data.status)
                        if let operation = data.operation {
                            Label(operation.message, systemImage: operation.ok ? "checkmark.circle" : "exclamationmark.triangle")
                                .font(.callout).foregroundStyle(operation.ok ? Color.secondary : Color.orange)
                                .fixedSize(horizontal: false, vertical: true)
                        }
                        if data.events.isEmpty {
                            Text("No observed outcomes in this session. Views and successful commands do not establish task success.")
                                .foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                        } else {
                            DisclosureGroup("Observed events (\(data.events.count))") {
                                ForEach(data.events) { event in
                                    LearningEventRow(agent: agent.id, session: selectedSession, event: event,
                                                     allowed: data.status.feedbackAllowed, busy: busy)
                                }
                            }
                        }
                        if data.candidates.isEmpty {
                            Text("No reflection candidates. An empty result is normal; ordinary turns do not need a lesson.")
                                .font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                        } else {
                            ForEach(data.candidates) { candidate in
                                LearningCandidateRow(agent: agent.id, session: selectedSession, candidate: candidate,
                                                     status: data.status, busy: busy)
                            }
                        }
                    } else if selectedSession.isEmpty {
                        Text("Choose the task's session to inspect its evidence and review candidates.").foregroundStyle(.secondary)
                    }
                }
            } else if !busy {
                Button("Load sessions") { Task { await review.loadLearning(agent: agent.id) } }
            }
        } header: {
            SectionTitle(agent.name, note: agent.unit)
        }
        .task(id: agent.id + review.contextID.uuidString) {
            selectedSession = review.learningSessions[agent.id] ?? ""
            if view == nil { await review.loadLearning(agent: agent.id) }
        }
    }

    private func status(_ status: LearningReviewStatus) -> some View {
        VStack(alignment: .leading, spacing: 4) {
            Label(status.reflectionEnabled ? "Reflection enabled · proposals only" : "Reflection off",
                  systemImage: status.reflectionEnabled ? "lightbulb" : "lightbulb.slash")
            Text("Calls \(status.callsUsed)/\(status.callsLimit) · events \(status.eventsUsed)/\(status.eventsLimit) · candidates \(status.candidatesUsed)/\(status.candidatesLimit) · exports \(status.exportsUsed)/\(status.exportsLimit)")
                .font(.caption).monospacedDigit().foregroundStyle(.secondary)
            Text("Budgets and configuration are read-only here. Export selects a proposal for the normal review cycle; it does not activate memory or skills.")
                .font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
        }
    }
    private func unavailableMessage(_ state: String) -> String {
        switch state {
        case "unsupported": "This engine does not support outcome review."
        case "missing_credential": "The owner feedback connection is not configured on the core."
        case "unavailable": "This agent is unavailable. Start it, then reload."
        default: "Learning review is unavailable. Reload to try again."
        }
    }
}

private struct LearningEventRow: View {
    @Environment(AppModel.self) private var model
    let agent: String
    let session: String
    let event: LearningOutcome
    let allowed: Bool
    let busy: Bool
    @State private var outcome = "succeeded"
    @State private var summary = ""
    @State private var sent = false

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            Text("\(event.subjectKind.capitalized) · \(event.sourceType.replacingOccurrences(of: "_", with: " "))").fontWeight(.medium)
            Text("Execution: \(event.executionStatus) · task: \(event.taskOutcome)")
                .font(.callout).foregroundStyle(.secondary)
            LabeledContent("Audience", value: event.audience).font(.caption)
            DisclosureGroup("Revision and provenance") {
                LabeledContent("Revision", value: event.subjectRevision)
                LabeledContent("Subject", value: event.subjectId)
                LabeledContent("Event", value: event.eventId)
                LabeledContent("Observed", value: event.at)
            }.font(.caption).textSelection(.enabled)
            if event.acceptsFeedback {
                if sent {
                    Label("Feedback recorded", systemImage: "checkmark.circle").font(.callout).foregroundStyle(.secondary)
                } else if allowed {
                    DisclosureGroup("Record task feedback") {
                        Picker("Verified task result", selection: $outcome) {
                            Text("Succeeded").tag("succeeded")
                            Text("Failed").tag("failed")
                            Text("Corrected").tag("corrected")
                        }.disabled(busy)
                        TextField("Optional selected summary (no secrets)", text: $summary, axis: .vertical)
                            .lineLimit(2...5).disabled(busy)
                            .onChange(of: summary) { _, text in summary = LearningFeedback.boundedSummary(text) }
                            .accessibilityLabel("Optional task evidence summary, maximum 2000 bytes")
                        HStack {
                            Text("\(summary.utf8.count)/2000 bytes").font(.caption).monospacedDigit().foregroundStyle(.secondary)
                            Spacer()
                            Button("Record Feedback") {
                                let request = LearningFeedback.feedback(event: event.eventId, outcome: outcome, origin: LearningFeedback.origin(for: event.eventId), summary: summary)
                                Task { sent = await model.review.sendLearning(request, agent: agent, session: session) }
                            }.disabled(busy)
                        }
                        Text("Confirm only checks you can support. A successful command alone is not a successful task. Retries use the same source identifier.")
                            .font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                    }
                } else {
                    Label("Feedback is unavailable for this session.", systemImage: "lock").font(.caption).foregroundStyle(.secondary)
                }
            }
        }.padding(.vertical, 4)
    }
}

private struct LearningCandidateRow: View {
    @Environment(AppModel.self) private var model
    let agent: String
    let session: String
    let candidate: LearningCandidate
    let status: LearningReviewStatus
    let busy: Bool
    private var exported: Bool { status.exportedIds.contains(candidate.id) }

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            Text(candidate.text).textSelection(.enabled)
            Text("\(candidate.action.capitalized) proposal · \(candidate.audience) · \(candidate.sensitivity)")
                .font(.caption).foregroundStyle(.secondary)
            if let mistake = candidate.previousMistake, !mistake.isEmpty { LabeledContent("Previous mistake", value: mistake) }
            LabeledContent("Checks", value: candidate.checks)
            LabeledContent("Applies when", value: candidate.conditions)
            if !candidate.exceptions.isEmpty { LabeledContent("Exceptions", value: candidate.exceptions) }
            LabeledContent("Review date", value: candidate.reviewDate)
            DisclosureGroup("Source events and related records") {
                Text(candidate.eventRefs.joined(separator: "\n")).textSelection(.enabled)
                if !candidate.relatedIds.isEmpty { Text("Related: " + candidate.relatedIds.joined(separator: ", ")).textSelection(.enabled) }
            }.font(.caption)
            HStack {
                Text(exported ? "Selected for the next review cycle" : "Proposal only · awaiting your selection")
                    .font(.caption).foregroundStyle(.secondary)
                Spacer()
                Button(exported ? "Exported" : "Export for Review") {
                    Task { _ = await model.review.sendLearning(.export(candidate.id), agent: agent, session: session) }
                }
                .disabled(busy || exported || !status.exportAllowed || !candidate.canExport)
                .help(candidate.canExport ? "Selects this procedural proposal for the next dream and normal review." : "Private or sensitive candidates cannot be exported.")
            }
        }.font(.callout).padding(.vertical, 4)
    }
}

struct LearningEvidenceNote: View {
    let evidence: LearningEvidence?
    var body: some View {
        VStack(alignment: .leading, spacing: 2) {
            if let evidence {
                Text("\(evidence.attempts) attempts · \(evidence.confirmations) verified confirmations · \(evidence.failures) failures · \(evidence.corrections) corrections")
                if evidence.unknown { Text("Evidence unknown; snapshot sightings do not confirm this lesson.") }
                if !evidence.evidenceTypes.isEmpty { Text("Sources: " + evidence.evidenceTypes.joined(separator: ", ")) }
                if let at = evidence.validatedAt { Text("Last validated: " + at) }
                DisclosureGroup("Evidence revision") { Text(evidence.revision).textSelection(.enabled) }
            } else {
                Text("Evidence unknown; snapshot sightings do not confirm this lesson.")
            }
        }.font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
    }
}
