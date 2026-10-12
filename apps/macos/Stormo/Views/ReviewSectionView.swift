import StormoKit
import SwiftUI

/// What the dream proposed for each agent: lessons and skills to accept or reject, accepted lessons
/// to promote, and a restart so decisions apply. The same decisions as `stormo learn`, through the
/// core's review API with the owner token (the browser review page's replacement).
struct ReviewSectionView: View {
    @Environment(AppModel.self) private var model
    @State private var showAll = false

    private var review: ReviewStore { model.review }

    var body: some View {
        Group {
            if !model.core.state.isRunning {
                ContentUnavailableView("Core stopped", systemImage: "server.rack", description: Text("Start the core to review what the agents learned."))
            } else if let view = review.view {
                let agents = view.agents.filter { showAll || $0.pending > 0 || $0.unapplied }
                if agents.isEmpty {
                    ContentUnavailableView("Nothing to review", systemImage: "checkmark.circle", description: Text("Proposals appear here after an agent's dream."))
                } else {
                    Form {
                        if let problem = review.problem { problemSection(problem) }
                        if view.autoAccept {
                            Section { Text("Auto-accept is on: agent-scoped memory lessons seen \(view.autoAcceptMinSeen)+ times with no PII flag are accepted on their own.").font(.callout).foregroundStyle(.secondary) }
                        }
                        ForEach(agents) { AgentReview(agent: $0, showAccepted: showAll) }
                    }
                    .formStyle(.grouped)
                }
            } else if let problem = review.problem {
                ContentUnavailableView("Can't load the review", systemImage: "exclamationmark.triangle", description: Text(problem))
            } else {
                ProgressView()
            }
        }
        .navigationTitle("Review")
        .toolbar {
            ToolbarItem {
                Toggle("Show All Agents", systemImage: "line.3.horizontal.decrease.circle", isOn: $showAll)
                    .help("Also show agents with nothing pending, and their accepted lessons")
            }
            ToolbarItem {
                Button("Reload", systemImage: "arrow.clockwise") { Task { await review.load() } }
                    .disabled(review.loading || !model.core.state.isRunning)
            }
        }
        .task(id: ReloadKey(running: model.core.state.isRunning, root: model.instances.active?.url, pending: model.fleet.summary.toReview)) {
            if model.core.state.isRunning { await review.load() }
        }
    }

    private struct ReloadKey: Equatable {
        let running: Bool
        let root: URL?
        let pending: Int
    }

    private func problemSection(_ problem: String) -> some View {
        Section { Label(problem, systemImage: "exclamationmark.triangle.fill").foregroundStyle(.red).textSelection(.enabled) }
    }
}

private struct AgentReview: View {
    @Environment(AppModel.self) private var model
    let agent: ReviewAgent
    let showAccepted: Bool

    private var review: ReviewStore { model.review }

    var body: some View {
        Section {
            if agent.unapplied { restartRow }
            ForEach(agent.proposedLessons) { LessonRow(agent: agent.id, lesson: $0) }
            ForEach(agent.skills) { SkillRow(agent: agent.id, skill: $0) }
            if agent.proposedLessons.count > 1 {
                HStack {
                    Spacer()
                    Button("Reject All Lessons") { decide(.reject) }
                    Button("Accept All Lessons") { decide(.accept) }
                }
                .controlSize(.small)
            }
            if showAccepted, !agent.acceptedLessons.isEmpty {
                DisclosureGroup("Accepted (\(agent.acceptedLessons.count))") {
                    ForEach(agent.acceptedLessons) { LessonRow(agent: agent.id, lesson: $0) }
                }
            }
        } header: {
            SectionTitle(agent.name, note: [agent.unit, agent.state == "running" ? (agent.busy ? "working" : "on shift") : "off shift"].filter { !$0.isEmpty }.joined(separator: " · "))
        }
    }

    private var restartRow: some View {
        HStack {
            Label("Accepted changes apply after a restart.", systemImage: "arrow.clockwise.circle").foregroundStyle(.secondary)
            Spacer()
            if review.restarting.contains(agent.id) {
                ProgressView().controlSize(.small)
            } else if agent.state == "running" {
                Button(agent.busy ? "Restart (Busy)" : "Restart") { Task { await review.restart(agent: agent.id) } }
                    .help(agent.busy ? "It is working now; restarting interrupts it." : "Restarts the local container so the decisions apply.")
            } else {
                Text("Applies when it starts").font(.caption).foregroundStyle(.secondary)
            }
        }
    }

    private func decide(_ verdict: ReviewDecision.Verdict) {
        let ids = agent.proposedLessons.map(\.id)
        Task { await review.decide(.lessons(ids, of: agent.id, verdict)) }
    }
}

private struct LessonRow: View {
    @Environment(AppModel.self) private var model
    let agent: String
    let lesson: ReviewLesson

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            Text(lesson.text).textSelection(.enabled)
            HStack(spacing: 6) {
                Tag(lesson.kind)
                Tag(lesson.scope)
                if lesson.seenCount > 1 { Tag("seen \(lesson.seenCount)×") }
                ForEach(lesson.pii, id: \.self) { Tag("PII: \($0)", tint: .red) }
                if lesson.autoAcceptable, lesson.status == "proposed" { Tag("auto-acceptable", tint: .green) }
                Spacer()
                if lesson.status == "proposed" {
                    Button("Reject") { decide(.reject) }
                    Button("Accept") { decide(.accept) }
                        .disabled(!lesson.pii.isEmpty)
                        .help(lesson.pii.isEmpty ? "" : "Flagged for personal data: reject it, or fix the ledger with `stormo learn`.")
                } else if lesson.scope == "agent" || lesson.scope == "unit" {
                    Menu("Promote") {
                        if lesson.scope == "agent" { Button("To Unit") { decide(.promote, scope: "unit") } }
                        Button("To Group") { decide(.promote, scope: "group") }
                    }
                    .fixedSize()
                }
            }
            .controlSize(.small)
        }
        .padding(.vertical, 2)
    }

    private func decide(_ verdict: ReviewDecision.Verdict, scope: String? = nil) {
        Task { await model.review.decide(.lessons([lesson.id], of: agent, verdict, scope: scope)) }
    }
}

private struct SkillRow: View {
    @Environment(AppModel.self) private var model
    let agent: String
    let skill: ReviewSkill

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack(spacing: 6) {
                Image(systemName: "wand.and.stars").foregroundStyle(.secondary)
                Text(skill.skill).fontWeight(.medium)
                Tag(skill.new ? "new skill" : "change")
                ForEach(skill.pii, id: \.self) { Tag("PII: \($0)", tint: .red) }
                Spacer()
                Button("Reject") { decide(.reject) }
                Button("Accept") { decide(.accept) }
                    .disabled(!skill.pii.isEmpty)
            }
            .controlSize(.small)
            ForEach(skill.files) { file in
                DisclosureGroup {
                    if let current = file.current {
                        Text(current.isEmpty ? "Now: (not in the skill)" : "Now:").font(.caption).foregroundStyle(.secondary)
                        if !current.isEmpty { code(current).opacity(0.7) }
                        Text("Proposed:").font(.caption).foregroundStyle(.secondary)
                    }
                    code(file.text ?? "(\(file.size) bytes, not shown)")
                } label: {
                    Text(file.path).font(.callout.monospaced())
                }
            }
        }
        .padding(.vertical, 2)
    }

    private func code(_ s: String) -> some View {
        ScrollView(.horizontal) {
            Text(s).font(.caption.monospaced()).textSelection(.enabled).fixedSize(horizontal: true, vertical: false)
        }
        .frame(maxHeight: 260)
    }

    private func decide(_ verdict: ReviewDecision.Verdict) {
        Task { await model.review.decide(.skill(skill.skill, of: agent, verdict)) }
    }
}

private struct Tag: View {
    let text: String
    var tint: Color = .secondary

    init(_ text: String, tint: Color = .secondary) {
        self.text = text
        self.tint = tint
    }

    var body: some View {
        Text(text).font(.caption).foregroundStyle(tint)
            .padding(.horizontal, 6).padding(.vertical, 1)
            .background(tint.opacity(0.12), in: Capsule())
    }
}
