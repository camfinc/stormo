import StormoKit
import SwiftUI

/// The shared space the agents work in, read-only: `group` (every agent) and each unit's layer
/// (that unit's agents only), with who wrote each file and the locks on it. Through the core's
/// workdir API with the owner token; nothing is written or cached.
struct FilesView: View {
    @Environment(AppModel.self) private var model

    private var files: FilesStore { model.files }

    var body: some View {
        Group {
            if !model.core.state.isRunning {
                ContentUnavailableView("Core stopped", systemImage: "server.rack", description: Text("Start the core to browse the agents' files."))
            } else if files.layers.isEmpty, let problem = files.problem {
                ContentUnavailableView("Can't load the files", systemImage: "exclamationmark.triangle", description: Text(problem))
            } else if files.layers.isEmpty {
                ProgressView()
            } else {
                HSplitView {
                    LayerList()
                        .frame(minWidth: 170, idealWidth: 200, maxWidth: 260)
                    DirectoryList()
                        .frame(minWidth: 260, idealWidth: 320)
                    FilePreview()
                        .frame(minWidth: 300, maxWidth: .infinity)
                }
            }
        }
        .navigationTitle("Files")
        .alert("Files", isPresented: Binding(get: { files.problem != nil && !files.layers.isEmpty }, set: { if !$0 { model.files.problem = nil } })) {
            Button("OK", role: .cancel) {}
        } message: {
            Text(files.problem ?? "")
        }
        .toolbar {
            ToolbarItem {
                Button("Reload", systemImage: "arrow.clockwise") { Task { await files.reload() } }
                    .disabled(files.loading || !model.core.state.isRunning)
            }
        }
        .task(id: ReloadKey(running: model.core.state.isRunning, root: model.instances.active?.url)) {
            if model.core.state.isRunning { await files.loadLayers() }
        }
    }

    private struct ReloadKey: Equatable {
        let running: Bool
        let root: URL?
    }
}

/// Shared first, then one row per unit with the agents that mount it.
private struct LayerList: View {
    @Environment(AppModel.self) private var model

    var body: some View {
        let files = model.files
        List(selection: Binding(get: { files.layer }, set: { l in
            if let l, l != files.layer { Task { await files.open(directory: FilesStore.root(of: l)) } }
        })) {
            Section("Shared") {
                if files.layers.contains("group") {
                    Label {
                        VStack(alignment: .leading) {
                            Text("Group")
                            Text("Every agent").font(.caption).foregroundStyle(.secondary)
                        }
                    } icon: { Image(systemName: "person.3") }
                    .tag("group")
                }
            }
            Section("Units") {
                ForEach(files.layers.filter { $0 != "group" }, id: \.self) { unit in
                    Label {
                        VStack(alignment: .leading) {
                            Text(unitName(unit))
                            Text(agents(in: unit)).font(.caption).foregroundStyle(.secondary).lineLimit(2)
                        }
                    } icon: { Image(systemName: "person.crop.square") }
                    .tag(unit)
                }
            }
        }
        .listStyle(.sidebar)
    }

    private func unitName(_ id: String) -> String {
        model.fleet.fleet?.units.first { $0.id == id }?.name ?? id
    }

    private func agents(in unit: String) -> String {
        let names = (model.fleet.fleet?.agents ?? []).filter { $0.unit == unit }.map(\.name)
        return names.isEmpty ? "No agents" : names.joined(separator: ", ")
    }
}

private struct DirectoryList: View {
    @Environment(AppModel.self) private var model

    var body: some View {
        let files = model.files
        VStack(spacing: 0) {
            if let dir = files.directory {
                ScrollView(.horizontal, showsIndicators: false) {
                    HStack(spacing: 2) {
                        ForEach(Array(FilesStore.crumbs(of: dir).enumerated()), id: \.offset) { i, c in
                            if i > 0 { Image(systemName: "chevron.right").font(.caption2).foregroundStyle(.tertiary) }
                            Button(c.name) { Task { await files.open(directory: c.path) } }
                                .buttonStyle(.link)
                                .disabled(c.path == dir)
                        }
                    }
                    .padding(.horizontal, 10).padding(.vertical, 6)
                }
                Divider()
            }
            if files.entries.isEmpty {
                ContentUnavailableView("Empty", systemImage: "folder", description: Text("No files here yet."))
            } else {
                List(files.entries, selection: Binding(get: { files.selectedName }, set: { name in
                    if let e = files.entries.first(where: { $0.name == name }) { Task { await files.choose(e) } }
                })) { e in
                    EntryRow(entry: e).tag(e.name)
                }
            }
        }
    }
}

private struct EntryRow: View {
    let entry: WorkdirEntry

    var body: some View {
        HStack {
            Image(systemName: entry.dir ? "folder.fill" : "doc.text")
                .foregroundStyle(entry.dir ? Color.accentColor : .secondary)
            VStack(alignment: .leading, spacing: 1) {
                Text(entry.name).lineLimit(1).truncationMode(.middle)
                if !entry.dir {
                    Text(detail).font(.caption).foregroundStyle(.secondary).lineLimit(1)
                }
            }
            Spacer()
            if let lock = entry.locked {
                Image(systemName: "lock.fill").foregroundStyle(.orange).help("Locked by \(lock)")
            }
        }
    }

    private var detail: String {
        var parts = [ByteCountFormatter.string(fromByteCount: entry.size, countStyle: .file)]
        if let w = entry.lastWriter, !w.isEmpty { parts.append(w) }
        if let m = entry.modified.flatMap(FilePreview.date) { parts.append(m.formatted(.relative(presentation: .named))) }
        return parts.joined(separator: " · ")
    }
}

struct FilePreview: View {
    @Environment(AppModel.self) private var model

    var body: some View {
        let files = model.files
        if files.selected == nil {
            ContentUnavailableView("No file selected", systemImage: "doc", description: Text("Pick a file to read it and see who changed it."))
        } else if let f = files.file {
            VStack(alignment: .leading, spacing: 0) {
                header(f, info: files.info)
                Divider()
                if f.binary == true {
                    ContentUnavailableView("Binary file", systemImage: "doc.zipper", description: Text("\(ByteCountFormatter.string(fromByteCount: f.size, countStyle: .file)), not shown."))
                } else {
                    ScrollView([.vertical, .horizontal]) {
                        Text(f.text ?? "")
                            .font(.system(.body, design: .monospaced))
                            .textSelection(.enabled)
                            .frame(maxWidth: .infinity, alignment: .topLeading)
                            .padding(12)
                    }
                    if f.truncated == true {
                        Divider()
                        Text("Showing the first MiB of \(ByteCountFormatter.string(fromByteCount: f.size, countStyle: .file)).")
                            .font(.caption).foregroundStyle(.secondary).padding(8)
                    }
                }
            }
        } else {
            ProgressView()
        }
    }

    @ViewBuilder private func header(_ f: WorkdirFile, info: WorkdirFileInfo?) -> some View {
        VStack(alignment: .leading, spacing: 4) {
            Text(f.path).font(.headline).textSelection(.enabled).lineLimit(2).truncationMode(.middle)
            HStack(spacing: 12) {
                Text(ByteCountFormatter.string(fromByteCount: f.size, countStyle: .file))
                if let d = Self.date(f.modified) { Text(d.formatted(date: .abbreviated, time: .shortened)) }
                if let c = info?.creator, !c.isEmpty { Text("Created by \(c)") }
                if let w = info?.lastWriter, !w.isEmpty { Text("Last written by \(w)") }
            }
            .font(.caption).foregroundStyle(.secondary)
            ForEach(info?.locks ?? [], id: \.path) { l in
                Label("\(l.owner) holds a \(l.kind) lock\(l.reason.map { ": \($0)" } ?? "")", systemImage: "lock.fill")
                    .font(.caption).foregroundStyle(.orange)
            }
            if let h = info?.history, !h.isEmpty {
                DisclosureGroup("History (\(h.count))") {
                    ForEach(Array(h.enumerated()), id: \.offset) { _, e in
                        HStack {
                            Text(e.kind).frame(width: 64, alignment: .leading)
                            Text(e.agent.isEmpty ? "—" : e.agent).frame(width: 90, alignment: .leading)
                            Text(e.how).foregroundStyle(.secondary)
                            Spacer()
                            if let d = Self.date(e.at) { Text(d.formatted(.relative(presentation: .named))).foregroundStyle(.secondary) }
                        }
                        .font(.caption)
                    }
                }
                .font(.caption)
            }
        }
        .padding(12)
    }

    static func date(_ s: String) -> Date? {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        return f.date(from: s) ?? ISO8601DateFormatter().date(from: s)
    }
}
