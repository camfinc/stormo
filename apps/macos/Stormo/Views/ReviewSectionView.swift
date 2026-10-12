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
                if stale { Tag("older version", tint: .orange) }
                ForEach(skill.pii, id: \.self) { Tag("PII: \($0)", tint: .red) }
                Spacer()
                Button("Reject") { decide(.reject) }
                Button("Accept") { decide(.accept) }
                    .disabled(!skill.pii.isEmpty || stale)
            }
            .controlSize(.small)
            if stale {
                Label("The agent changed a version of this skill from before the repo's latest edit\(skill.base.map { " (it booted from \($0))" } ?? ""). Red lines below include the repo's newer edits, not only the agent's. Accepting would undo them, so merge what you want by hand, then reject this.", systemImage: "exclamationmark.triangle.fill")
                    .font(.caption)
                    .foregroundStyle(.orange)
                    .fixedSize(horizontal: false, vertical: true)
            }
            ForEach(skill.files) { file in
                let diff = file.text.map { LineDiff(old: file.current ?? "", new: $0) }
                DisclosureGroup {
                    if let diff {
                        DiffView(diff: diff)
                    } else {
                        Text("\(file.size) bytes, not shown (binary or over 64 KB)").font(.caption).foregroundStyle(.secondary)
                    }
                } label: {
                    HStack(spacing: 8) {
                        Text(file.path).font(.callout.monospaced())
                        if let diff {
                            if diff.added > 0 { Text("+\(diff.added)").foregroundStyle(.green) }
                            if diff.removed > 0 { Text("−\(diff.removed)").foregroundStyle(.red) }
                            if diff.isEmpty { Text("unchanged").foregroundStyle(.secondary) }
                        }
                    }
                    .font(.caption.monospaced())
                }
            }
        }
        .padding(.vertical, 2)
    }

    private var stale: Bool { skill.stale ?? false }

    private func decide(_ verdict: ReviewDecision.Verdict) {
        Task { await model.review.decide(.skill(skill.skill, of: agent, verdict)) }
    }
}

/// A unified diff: removed lines red, added green, a few lines of context, the rest collapsed.
private struct DiffView: View {
    let diff: LineDiff

    var body: some View {
        ScrollView(.horizontal) {
            VStack(alignment: .leading, spacing: 0) {
                ForEach(diff.hunks) { hunk in
                    if hunk.skippedBefore > 0 { skipped(hunk.skippedBefore) }
                    ForEach(hunk.lines) { row($0) }
                }
                if diff.skippedAfter > 0 { skipped(diff.skippedAfter) }
            }
            .font(.caption.monospaced())
            .textSelection(.enabled)
        }
        .frame(maxHeight: 420)
        .background(.background.opacity(0.6), in: RoundedRectangle(cornerRadius: 6))
    }

    private func row(_ l: LineDiff.Line) -> some View {
        HStack(spacing: 0) {
            Text(l.old.map(String.init) ?? "").frame(width: 34, alignment: .trailing).foregroundStyle(.tertiary)
            Text(l.new.map(String.init) ?? "").frame(width: 34, alignment: .trailing).foregroundStyle(.tertiary)
            Text(sign(l.kind)).frame(width: 18).foregroundStyle(tint(l.kind) ?? .secondary)
            Text(l.text.isEmpty ? " " : l.text).fixedSize()
            Spacer(minLength: 0)
        }
        .padding(.vertical, 1)
        .background(tint(l.kind)?.opacity(0.14) ?? .clear)
    }

    private func skipped(_ n: Int) -> some View {
        Text("⋯ \(n) unchanged line\(n == 1 ? "" : "s")")
            .foregroundStyle(.secondary)
            .padding(.vertical, 3).padding(.leading, 86)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(.quaternary.opacity(0.5))
    }

    private func sign(_ k: LineDiff.Kind) -> String {
        switch k { case .same: " "; case .removed: "−"; case .added: "+" }
    }

    private func tint(_ k: LineDiff.Kind) -> Color? {
        switch k { case .same: nil; case .removed: .red; case .added: .green }
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
