import AppKit
import StormoKit
import SwiftUI

/// An agent's definition (agent.yaml) and behaviour (SOUL.md), edited through `stormo config`
/// (docs/api.md): the engine validates every save and refuses one made over a file that changed on
/// disk; after a save the agent is checked, and a running agent picks the change up on restart.
struct AgentEditor: View {
    /// The window group; its value is the agent id, so each agent gets one editor window.
    static let windowID = "agent-editor"

    enum Page: String, CaseIterable, Identifiable {
        case definition, behaviour
        var id: Self { self }
        var title: String { self == .definition ? "Definition" : "Behaviour" }
        var file: String { self == .definition ? "agent.yaml" : "SOUL.md" }
    }

    enum Check: Equatable {
        case running
        case passed(CheckedAgent?)
        case failed(String)
    }

    @Environment(AppModel.self) private var model
    let agentID: String
    @State private var page = Page.definition
    @State private var editors: [Page: ConfigEditor] = [:]
    @State private var check: Check?

    private var editor: ConfigEditor? { editors[page] }
    private var agent: FleetAgent? { model.fleet.agent(agentID) }

    var body: some View {
        VStack(spacing: 0) {
            if let editor {
                EditorPane(editor: editor, wraps: page == .behaviour)
                    .id(page) // its own undo history per file
            } else {
                ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity)
            }
            Divider()
            statusBar
                .padding(.horizontal, 12)
                .padding(.vertical, 8)
        }
        .navigationTitle(agent?.name ?? agentID)
        .navigationSubtitle("agents/\(agentID)/\(page.file)")
        .toolbar {
            ToolbarItem(placement: .principal) {
                Picker("File", selection: $page) {
                    ForEach(Page.allCases) { p in
                        Text(p.title + (editors[p]?.isDirty == true ? " •" : "")).tag(p)
                    }
                }
                .pickerStyle(.segmented)
                .labelsHidden()
            }
            ToolbarItemGroup(placement: .primaryAction) {
                Button("Revert", systemImage: "arrow.uturn.backward") { editor?.revert() }
                    .disabled(editor?.isDirty != true)
                Button("Save", systemImage: "checkmark") { Task { await save() } }
                    .keyboardShortcut("s")
                    .disabled(editor?.isDirty != true || editor?.busy == true)
            }
        }
        .task(id: model.cli?.instance) { await load() }
        .frame(minWidth: 560, minHeight: 420)
    }

    @ViewBuilder private var statusBar: some View {
        HStack(spacing: 8) {
            if let problem = editor?.problem {
                Image(systemName: "exclamationmark.triangle.fill").foregroundStyle(.red)
                Text(problem.code == "usage" && editor?.file == nil
                     ? "This stormo cannot edit agents (it has no `config` command). Update it, or choose the bundled one in Settings › Command line."
                     : problem.message)
                    .textSelection(.enabled).lineLimit(3)
                Spacer()
                if editor?.isStale == true {
                    Button("Reload") { Task { await reload() } }
                        .help("Load the version on disk; your edits to this file are dropped")
                }
            } else if editor?.isDirty == true {
                Text("Edited").foregroundStyle(.secondary)
                Spacer()
            } else if let check {
                checkStatus(check)
            } else {
                Text(page == .definition ? "Saved through stormo, which validates the manifest first. deploy: is read-only here."
                                         : "How the agent behaves, in Markdown. It reaches the agent on its next start.")
                    .foregroundStyle(.secondary)
                Spacer()
            }
        }
        .font(.callout)
        .frame(minHeight: 22)
    }

    @ViewBuilder private func checkStatus(_ check: Check) -> some View {
        switch check {
        case .running:
            ProgressView().controlSize(.small)
            Text("Saved. Checking \(agent?.name ?? agentID)…").foregroundStyle(.secondary)
            Spacer()
        case .passed(let row):
            Image(systemName: "checkmark.circle.fill").foregroundStyle(.green)
            Text("Saved and checked" + (row.map { ": \($0.files) files, \($0.skills) skills" } ?? "") + ".")
            Spacer()
            if agent?.state == "running" {
                Text("Restart to use it.").foregroundStyle(.secondary)
                Button("Restart \(agent?.name ?? agentID)") { Task { await model.run("restart", agents: [agentID]) } }
            }
        case .failed(let message):
            Image(systemName: "exclamationmark.triangle.fill").foregroundStyle(.orange)
            Text("Saved, but the check failed: \(message)").textSelection(.enabled).lineLimit(3)
            Spacer()
        }
    }

    private func load() async {
        guard let cli = model.cli else { return }
        for p in Page.allCases {
            let e = editors[p] ?? ConfigEditor(path: "agents/\(agentID)/\(p.file)")
            editors[p] = e
            await e.load(with: cli)
        }
    }

    private func reload() async {
        guard let cli = model.cli, let editor else { return }
        await editor.load(with: cli)
    }

    private func save() async {
        guard let cli = model.cli, let editor else { return }
        check = nil
        guard await editor.save(with: cli) else { return }
        check = .running
        do {
            check = .passed(try await cli.run(["check", agentID], as: [CheckedAgent].self).first)
        } catch {
            check = .failed(error.localizedDescription)
        }
        await model.fleet.refresh()
    }
}

private struct EditorPane: View {
    @Bindable var editor: ConfigEditor
    let wraps: Bool

    var body: some View {
        PlainTextEditor(text: $editor.text, editable: editor.file != nil && !editor.isStale, wraps: wraps)
    }
}

/// Monospaced plain text with every automatic substitution off: smart quotes or dashes would
/// change what YAML means. Without wrapping, long lines scroll sideways (a wrapped YAML comment
/// reads like a key).
struct PlainTextEditor: NSViewRepresentable {
    @Binding var text: String
    var editable = true
    var wraps = true

    func makeNSView(context: Context) -> NSScrollView {
        let scroll = NSTextView.scrollableTextView()
        let view = scroll.documentView as! NSTextView
        view.isRichText = false
        view.allowsUndo = true
        view.usesFindBar = true
        view.isIncrementalSearchingEnabled = true
        view.font = .monospacedSystemFont(ofSize: NSFont.systemFontSize, weight: .regular)
        view.textContainerInset = NSSize(width: 10, height: 12)
        view.isAutomaticQuoteSubstitutionEnabled = false
        view.isAutomaticDashSubstitutionEnabled = false
        view.isAutomaticTextReplacementEnabled = false
        view.isAutomaticSpellingCorrectionEnabled = false
        view.isContinuousSpellCheckingEnabled = false
        view.isAutomaticLinkDetectionEnabled = false
        view.isAutomaticDataDetectionEnabled = false
        view.smartInsertDeleteEnabled = false
        if !wraps {
            scroll.hasHorizontalScroller = true
            view.isHorizontallyResizable = true
            view.maxSize = NSSize(width: CGFloat.greatestFiniteMagnitude, height: CGFloat.greatestFiniteMagnitude)
            view.textContainer?.widthTracksTextView = false
            view.textContainer?.containerSize = NSSize(width: CGFloat.greatestFiniteMagnitude, height: CGFloat.greatestFiniteMagnitude)
        }
        view.string = text
        view.delegate = context.coordinator
        return scroll
    }

    func updateNSView(_ scroll: NSScrollView, context: Context) {
        context.coordinator.text = $text
        guard let view = scroll.documentView as? NSTextView else { return }
        view.isEditable = editable
        if view.string != text {
            view.string = text
        }
    }

    func makeCoordinator() -> Coordinator { Coordinator(text: $text) }

    final class Coordinator: NSObject, NSTextViewDelegate {
        var text: Binding<String>
        init(text: Binding<String>) { self.text = text }

        func textDidChange(_ notification: Notification) {
            guard let view = notification.object as? NSTextView else { return }
            text.wrappedValue = view.string
        }
    }
}
