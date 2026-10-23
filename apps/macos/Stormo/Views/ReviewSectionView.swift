import StormoKit
import SwiftUI

/// What the dream proposed for each agent: lessons, skills, and the schedules and settings it changed
/// at runtime, to accept or reject; accepted lessons to promote; what the automatic review said; and
/// a restart so decisions apply. The same decisions as `stormo learn`, through the core's review API
/// with the owner token (the browser review page's replacement).
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
                        Section { ReviewModeNote(view: view) }
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
        .alert("Review", isPresented: Binding(get: { review.problem != nil && review.view != nil }, set: { if !$0 { model.review.problem = nil } })) {
            Button("OK", role: .cancel) {}
        } message: {
            Text(review.problem ?? "")
        }
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
            if let schedules = agent.schedules { SchedulesRow(agent: agent.id, schedules: schedules) }
            ForEach(agent.runtimeSettings) { SettingRow(agent: agent.id, setting: $0) }
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
            SectionTitle(agent.name, note: [agent.unit, agent.state == "running" ? (agent.busy ? "working" : "on shift") : "off shift",
                                            agent.autoReview == false ? "no automatic review" : ""].filter { !$0.isEmpty }.joined(separator: " · "))
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
                Spacer()
                if lesson.status == "proposed" {
                    if busy { ProgressView().controlSize(.small) }
                    Button("Reject") { decide(.reject) }
                        .disabled(busy)
                    Button("Accept") { decide(.accept) }
                        .disabled(!lesson.pii.isEmpty || busy)
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
            if lesson.status == "proposed" { ReviewNote(ai: lesson.ai, held: lesson.held) }
        }
        .padding(.vertical, 2)
    }

    private var busy: Bool { model.review.deciding.contains(ReviewDecision.lessons([lesson.id], of: agent, .accept).key) }

    private func decide(_ verdict: ReviewDecision.Verdict, scope: String? = nil) {
        Task { await model.review.decide(.lessons([lesson.id], of: agent, verdict, scope: scope)) }
    }
}

private struct SkillRow: View {
    @Environment(AppModel.self) private var model
    let agent: String
    let skill: ReviewSkill
    /// Files whose diff is shown. Not a DisclosureGroup: in a Form row it takes the row's clicks,
    /// so Reject and Accept toggled the diff instead of deciding.
    @State private var open: Set<String>?

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack(spacing: 6) {
                Image(systemName: "wand.and.stars").foregroundStyle(.secondary)
                Text(skill.skill).fontWeight(.medium)
                Tag(skill.new ? "new skill" : "change")
                if stale { Tag("older version", tint: .orange) }
                ForEach(skill.pii, id: \.self) { Tag("PII: \($0)", tint: .red) }
                Spacer()
                if busy { ProgressView().controlSize(.small) }
                Button("Reject") { decide(.reject) }
                    .disabled(busy)
                Button("Accept") { decide(.accept) }
                    .disabled(!skill.pii.isEmpty || stale || busy)
            }
            .controlSize(.small)
            ReviewNote(ai: skill.ai, held: skill.held)
            if stale {
                Label("The agent changed a version of this skill from before the repo's latest edit\(skill.base.map { " (it booted from \($0))" } ?? ""). Red lines below include the repo's newer edits, not only the agent's. Accepting would undo them, so merge what you want by hand, then reject this.", systemImage: "exclamationmark.triangle.fill")
                    .font(.caption)
                    .foregroundStyle(.orange)
                    .fixedSize(horizontal: false, vertical: true)
            }
            ForEach(skill.files) { file in
                let diff = file.text.map { LineDiff(old: file.current ?? "", new: $0) }
                let shown = (open ?? Set(skill.files.prefix(1).map(\.path))).contains(file.path)
                Button {
                    var set = open ?? Set(skill.files.prefix(1).map(\.path))
                    if shown { set.remove(file.path) } else { set.insert(file.path) }
                    open = set
                } label: {
                    HStack(spacing: 8) {
                        Image(systemName: "chevron.right")
                            .rotationEffect(.degrees(shown ? 90 : 0))
                            .foregroundStyle(.secondary)
                        Text(file.path).font(.callout.monospaced())
                        if let diff {
                            if diff.added > 0 { Text("+\(diff.added)").foregroundStyle(.green) }
                            if diff.removed > 0 { Text("−\(diff.removed)").foregroundStyle(.red) }
                            if diff.isEmpty { Text("unchanged").foregroundStyle(.secondary) }
                        }
                    }
                    .font(.caption.monospaced())
                    .contentShape(Rectangle())
                }
                .buttonStyle(.plain)
                if shown {
                    if let diff {
                        DiffView(diff: diff)
                    } else {
                        Text("\(file.size) bytes, not shown (binary or over 64 KB)").font(.caption).foregroundStyle(.secondary)
                    }
                }
            }
        }
        .padding(.vertical, 2)
    }

    private var stale: Bool { skill.stale ?? false }

    private var busy: Bool { model.review.deciding.contains(ReviewDecision.skill(skill.skill, of: agent, .accept).key) }

    private func decide(_ verdict: ReviewDecision.Verdict) {
        Task { await model.review.decide(.skill(skill.skill, of: agent, verdict)) }
    }
}

/// Whether the automatic review is on, and what it asks.
private struct ReviewModeNote: View {
    let view: ReviewView

    var body: some View {
        if view.review == "manual" || (view.review == nil && !view.autoAccept) {
            Label("Automatic review is off (core.learning.review: manual): every proposal waits for you.", systemImage: "person")
                .font(.callout).foregroundStyle(.secondary)
        } else {
            VStack(alignment: .leading, spacing: 4) {
                Label("Automatic review is on: after each learning cycle a model approves what is obviously safe and passes the rest to you. It never rejects. What is here waits for you.", systemImage: "checkmark.shield")
                    .font(.callout)
                Text(details).font(.caption).foregroundStyle(.secondary)
            }
        }
    }

    private var details: String {
        var parts = ["Rules in code always keep client data, people's profiles, scripts and permission changes for you"]
        if let c = view.reviewConnection { parts.append("model: \(view.reviewModel ?? "the first \(c) lists")") }
        if let p = view.reviewPrompt { parts.append("your rules: \(p)") }
        if view.autoRestart { parts.append("idle agents restart to apply") }
        return parts.joined(separator: " · ")
    }
}

/// Why a proposal is here: the rules held it for a person, or the review's model passed it on.
private struct ReviewNote: View {
    let ai: ReviewAI?
    let held: String?

    var body: some View {
        if let held {
            Label("Held for you by the rules: \(held)", systemImage: "hand.raised")
                .font(.caption).foregroundStyle(.orange)
                .fixedSize(horizontal: false, vertical: true)
        } else if let ai, !ai.approve {
            Label("The review passed it to you: \(ai.reason)", systemImage: "questionmark.circle")
                .font(.caption).foregroundStyle(.secondary)
                .help("\(ai.model), \(ai.at)")
                .fixedSize(horizontal: false, vertical: true)
        }
    }
}

/// The schedules the agent changed at runtime, as one proposal.
private struct SchedulesRow: View {
    @Environment(AppModel.self) private var model
    let agent: String
    let schedules: ReviewSchedules
    /// Scripts whose text is shown (a button, not a DisclosureGroup: see SkillRow).
    @State private var open: Set<String> = []

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack(spacing: 6) {
                Image(systemName: "calendar.badge.clock").foregroundStyle(.secondary)
                Text("Scheduled jobs").fontWeight(.medium)
                Tag("\(schedules.runtimeJobs.count) created at runtime")
                if !scripts.isEmpty { Tag("\(scripts.count) script\(scripts.count == 1 ? "" : "s")", tint: .orange) }
                ForEach(schedules.pii, id: \.self) { Tag("redacted \($0)", tint: .red) }
                Spacer()
                if busy { ProgressView().controlSize(.small) }
                Button("Reject") { decide(.reject) }
                    .disabled(busy)
                    .help("Drops the jobs it created at runtime when it next starts.")
                Button("Accept") { decide(.accept) }
                    .disabled(!schedules.pii.isEmpty || stale || busy)
                    .help(!schedules.pii.isEmpty ? "Redacted: copy what you want into agent.yaml by hand, then reject."
                        : stale ? "The repo changed a script after the agent edited it: merge what you want by hand, then reject."
                        : "Writes these jobs to agent.yaml's schedules, and their scripts to the agent's scripts folder.")
            }
            .controlSize(.small)
            ReviewNote(ai: schedules.ai, held: schedules.held)
            ForEach(schedules.jobs) { job in
                VStack(alignment: .leading, spacing: 2) {
                    HStack(spacing: 6) {
                        Text(job.name ?? job.id).font(.callout)
                        Text(job.when).font(.caption.monospaced()).foregroundStyle(.secondary)
                        if !job.enabled { Tag("paused") }
                        if schedules.runtimeJobs.contains(job.id) { Tag("new", tint: .blue) }
                        if let d = job.deliver, !d.isEmpty { Tag("→ \(d)") }
                        if job.script != nil || job.monitor != nil { Tag("runs a script", tint: .orange) }
                    }
                    if let p = job.prompt { Text(p).font(.caption).foregroundStyle(.secondary).lineLimit(3).textSelection(.enabled) }
                }
            }
            ForEach(scripts) { script in
                let diff = script.text.map { LineDiff(old: script.current ?? "", new: $0) }
                let shown = open.contains(script.name)
                Button {
                    if shown { open.remove(script.name) } else { open.insert(script.name) }
                } label: {
                    HStack(spacing: 8) {
                        Image(systemName: "chevron.right")
                            .rotationEffect(.degrees(shown ? 90 : 0))
                            .foregroundStyle(.secondary)
                        Text("scripts/\(script.name)").font(.callout.monospaced())
                        Tag(script.status == "changed" ? "change" : "new script", tint: script.status == "changed" ? .orange : .blue)
                        if script.stale ?? false { Tag("older version", tint: .orange) }
                        if let diff {
                            if diff.added > 0 { Text("+\(diff.added)").foregroundStyle(.green) }
                            if diff.removed > 0 { Text("−\(diff.removed)").foregroundStyle(.red) }
                        }
                    }
                    .font(.caption.monospaced())
                    .contentShape(Rectangle())
                }
                .buttonStyle(.plain)
                if shown {
                    if let diff {
                        DiffView(diff: diff)
                    } else {
                        Text("\(script.size) bytes, not shown (binary or over 64 KB)").font(.caption).foregroundStyle(.secondary)
                    }
                }
            }
        }
        .padding(.vertical, 2)
    }

    private var scripts: [ReviewScript] { schedules.scripts ?? [] }
    private var stale: Bool { scripts.contains { $0.stale ?? false } }

    private var busy: Bool { model.review.deciding.contains(ReviewDecision.schedules(of: agent, .accept).key) }

    private func decide(_ verdict: ReviewDecision.Verdict) {
        Task { await model.review.decide(.schedules(of: agent, verdict)) }
    }
}

/// A setting the agent changed at runtime (a home channel, approved commands, an MCP server).
private struct SettingRow: View {
    @Environment(AppModel.self) private var model
    let agent: String
    let setting: ReviewSetting

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack(spacing: 6) {
                Image(systemName: "slider.horizontal.3").foregroundStyle(.secondary)
                Text(setting.key).font(.callout.monospaced())
                if setting.sensitive == true { Tag("widens what it may do", tint: .orange) }
                ForEach(setting.pii, id: \.self) { Tag("redacted \($0)", tint: .red) }
                Spacer()
                if busy { ProgressView().controlSize(.small) }
                Button("Reject") { decide(.reject) }
                    .disabled(busy)
                Button("Accept") { decide(.accept) }
                    .disabled(!setting.pii.isEmpty || busy)
                    .help("Writes it to the agent's files in the repo; it applies when the agent restarts.")
            }
            .controlSize(.small)
            Grid(alignment: .leading, horizontalSpacing: 8, verticalSpacing: 2) {
                GridRow {
                    Text("now").foregroundStyle(.secondary)
                    Text(setting.current ?? "not set in the repo").foregroundStyle(setting.current == nil ? .tertiary : .primary)
                }
                GridRow {
                    Text("proposed").foregroundStyle(.secondary)
                    Text(setting.value)
                }
            }
            .font(.caption.monospaced())
            .textSelection(.enabled)
            ReviewNote(ai: setting.ai, held: setting.held)
        }
        .padding(.vertical, 2)
    }

    private var busy: Bool { model.review.deciding.contains(ReviewDecision.settings([setting.key], of: agent, .accept).key) }

    private func decide(_ verdict: ReviewDecision.Verdict) {
        Task { await model.review.decide(.settings([setting.key], of: agent, verdict)) }
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
        // Its full height: it scrolls only sideways, and a Form row sizes to what it reports (a
        // flexible height let the row collapse and the diff draw over the rows above).
        .fixedSize(horizontal: false, vertical: true)
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
