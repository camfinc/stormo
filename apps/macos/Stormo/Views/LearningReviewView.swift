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
                        LearningControls(agent: agent.id, session: selectedSession, data: data, busy: busy)
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

/// What the agent's runtime recorded about a proposal's exact version, when it recorded anything
/// (an engine that records no outcomes, like Hermes, shows nothing). The review's model reads the
/// same counts beside the change; they decide nothing on their own.
struct LearningEvidenceNote: View {
    let evidence: LearningEvidence?
    var body: some View {
        if let evidence, !evidence.evidenceTypes.isEmpty {
            VStack(alignment: .leading, spacing: 2) {
                if evidence.failures > 0 || evidence.corrections > 0 {
                    Label("Recorded \(evidence.failures) failures and \(evidence.corrections) corrections for this version", systemImage: "exclamationmark.triangle")
                        .foregroundStyle(.orange)
                }
                Text("\(evidence.attempts) attempts · \(evidence.confirmations) verified confirmations · \(evidence.failures) failures · \(evidence.corrections) corrections")
                Text("Sources: " + evidence.evidenceTypes.joined(separator: ", "))
                if let at = evidence.validatedAt { Text("Last validated: " + at) }
            }.font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
        }
    }
}

private struct LearningControls: View {
    @Environment(AppModel.self) private var model
    let agent: String
    let session: String
    let data: LearningReviewData
    let busy: Bool
    @State private var confirmation: LearningFeedback?
    @State private var confirmationText = ""

    var body: some View {
        Group {
            if let background = data.background {
                DisclosureGroup("Background jobs · \(background.enabled ? "enabled" : "disabled")") {
                    Text("Home budget, rolling 24 hours: calls \(background.callsUsed)/\(background.callsLimit) · reserved output tokens \(background.tokensReserved)/\(background.tokensLimit)")
                        .font(.caption).monospacedDigit()
                    Text("Session jobs: \(background.pending) pending · \(background.completed) complete · \(background.failed) failed · \(background.interrupted) interrupted. Candidates below are scoped to this conversation and remain proposals.")
                        .font(.caption).foregroundStyle(.secondary)
                    ForEach(background.jobs ?? []) { job in
                        VStack(alignment: .leading, spacing: 4) {
                            Text("\(job.kind) · \(job.state)").fontWeight(.medium)
                            Text("\(job.id) · attempts \(job.attempts) · owner retries \(job.ownerRetries) · \(job.resultCode)").font(.caption).textSelection(.enabled)
                            HStack {
                                if ["pending", "reserved"].contains(job.state) { control("Cancel job", "cancel_job", id: job.id) }
                                if ["failed", "interrupted", "cancelled"].contains(job.state) {
                                    control("Retry job…", "retry_job", id: job.id, confirm: "Retrying may spend another model call and reserved output tokens from the home budget.")
                                }
                            }
                        }.padding(.vertical, 4)
                    }
                    control("Prune terminal job details", "prune_jobs")
                    ForEach(background.candidates ?? []) { candidate in
                        Text(candidate.text).textSelection(.enabled)
                        Text("Proposal only · \(candidate.audience) · \(candidate.checks)").font(.caption).foregroundStyle(.secondary)
                    }
                }
            }
            if let skills = data.skills {
                DisclosureGroup("Skill proposals and maintenance") {
                    Text("Review exact replacements before approving. Approval changes the local executable skill; repository promotion is separate.").font(.caption).foregroundStyle(.secondary)
                    ForEach(skills.patches ?? []) { patch in
                        VStack(alignment: .leading, spacing: 6) {
                            Text("\(patch.name) · \(patch.status)").fontWeight(.medium)
                            Text("Audience: \(patch.audience) · base: \(patch.expectedRevision) · current: \(patch.currentRevision)").font(.caption).textSelection(.enabled)
                            if let reason = patch.reason { Text(reason).font(.caption) }
                            if let checks = patch.checks { LabeledContent("Checks", value: checks) }
                            if let conditions = patch.conditions { LabeledContent("Applies when", value: conditions) }
                            if let refs = patch.evidenceRefs, !refs.isEmpty {
                                DisclosureGroup("Evidence references") { Text(refs.joined(separator: "\n")).font(.caption.monospaced()).textSelection(.enabled) }
                            }
                            DisclosureGroup("Replacement preview") {
                                Text("Before").fontWeight(.medium)
                                Text(patch.before).font(.system(.caption, design: .monospaced)).textSelection(.enabled)
                                Text("After").fontWeight(.medium)
                                Text(patch.after).font(.system(.caption, design: .monospaced)).textSelection(.enabled)
                            }
                            HStack {
                                if patch.canApprove { control("Approve…", "approve_patch", id: patch.id, revision: patch.expectedRevision, confirm: "Activate the previewed replacement for \(patch.name)? Its base revision must still match.") }
                                if patch.status == "proposed" || patch.status == "pending" { control("Reject", "reject_patch", id: patch.id) }
                                if patch.canRollback { control("Roll back…", "rollback_patch", id: patch.id, revision: patch.currentRevision, confirm: "Restore the previous revision of \(patch.name)? The current revision must still match.") }
                            }
                        }.padding(.vertical, 6)
                    }
                    control("Scan maintenance", "scan_maintenance")
                    Text("Maintenance \(skills.maintenanceEnabled ? "enabled" : "disabled") · last scan: \(skills.lastScanAt ?? "never")").font(.caption).foregroundStyle(.secondary)
                    ForEach(skills.maintenance ?? []) { item in
                        Text("\(item.name) · \(item.action): \(item.reason)").font(.callout)
                        if item.action == "archive_review" { control("Archive \(item.name)…", "archive_skill", id: item.name, revision: item.revision, confirm: "Archive \(item.name)? It will no longer be available as an active skill. You can restore it from archives.") }
                    }
                    ForEach(skills.archives ?? []) { item in
                        HStack {
                            Text("\(item.name) · \(item.status)")
                            if item.status == "archived" { control("Restore", "restore_skill", id: item.id) }
                        }
                    }
                }
            }
        }
        .disabled(busy)
        .confirmationDialog("Confirm learning change", isPresented: Binding(get: { confirmation != nil }, set: { if !$0 { confirmation = nil } })) {
            Button("Confirm") { if let request = confirmation { send(request) }; confirmation = nil }
            Button("Cancel", role: .cancel) { confirmation = nil }
        } message: { Text(confirmationText) }
    }
    private func control(_ label: String, _ action: String, id: String? = nil, revision: String? = nil, confirm: String? = nil) -> some View {
        Button(label) {
            let request = LearningFeedback.control(action, id: id, revision: revision)
            if let confirm { confirmationText = confirm; confirmation = request } else { send(request) }
        }.disabled(busy)
    }
    private func send(_ request: LearningFeedback) {
        Task { _ = await model.review.sendLearning(request, agent: agent, session: session) }
    }
}
