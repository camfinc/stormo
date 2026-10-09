import StormoKit
import SwiftUI

/// File › New Instance…: `stormo new instance` in ~/Library/Application Support/Stormo/Instances
/// (or a folder the user picks), then `secrets init`, then it becomes the active instance.
struct NewInstanceSheet: View {
    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss

    @State private var name = ""
    @State private var org = ""
    @State private var slug = ""
    @State private var slugEdited = false
    @State private var template = "empty"
    @State private var parent = InstanceLocation.defaultParent
    @State private var choosingFolder = false
    @State private var creating = false
    @State private var problem: String?

    private var folder: URL { parent.appending(path: slug, directoryHint: .isDirectory) }
    private var folderTaken: Bool {
        guard !slug.isEmpty, let items = try? FileManager.default.contentsOfDirectory(atPath: folder.path) else { return false }
        return items.contains { $0 != ".DS_Store" }
    }
    private var canCreate: Bool {
        !name.trimmingCharacters(in: .whitespaces).isEmpty && InstanceLocation.isValidSlug(slug) && !folderTaken && !creating
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            Form {
                Section {
                    TextField("Name", text: $name, prompt: Text("Acme Swarm"))
                        .onChange(of: name) { _, new in
                            if !slugEdited { slug = InstanceLocation.slug(from: new) }
                        }
                    TextField("Organisation", text: $org, prompt: Text(name.isEmpty ? "Acme" : name))
                    TextField("Slug", text: Binding(get: { slug }, set: { slug = $0; slugEdited = true }), prompt: Text("acme"))
                        .monospaced()
                } footer: {
                    Text("The slug names the instance's folder and, later, its cloud resources. Lowercase letters, digits and hyphens.")
                        .foregroundStyle(.secondary)
                }
                Section("Start from") {
                    Picker("Template", selection: $template) {
                        VStack(alignment: .leading) {
                            Text("Empty")
                            Text("Your organisation: the stormo.yaml and the group unit. Add units and agents next.")
                                .font(.caption).foregroundStyle(.secondary)
                        }
                        .tag("empty")
                        VStack(alignment: .leading) {
                            Text("Example")
                            Text("Acme's demo setup (two units, three agents) under your name, to explore.")
                                .font(.caption).foregroundStyle(.secondary)
                        }
                        .tag("example")
                    }
                    .pickerStyle(.radioGroup)
                    .labelsHidden()
                }
                Section("Location") {
                    LabeledContent {
                        Button("Change…") { choosingFolder = true }
                    } label: {
                        PathText(path: slug.isEmpty ? parent.path : folder.path)
                    }
                    if folderTaken {
                        Text("That folder already has files in it. Choose another slug or location.")
                            .foregroundStyle(.orange)
                    }
                }
                if let problem {
                    Text(problem).foregroundStyle(.red)
                }
            }
            .formStyle(.grouped)
            HStack {
                if creating {
                    ProgressView().controlSize(.small)
                    Text("Creating…").foregroundStyle(.secondary)
                }
                Spacer()
                Button("Cancel", role: .cancel) { dismiss() }
                    .keyboardShortcut(.cancelAction)
                Button("Create") { Task { await create() } }
                    .keyboardShortcut(.defaultAction)
                    .disabled(!canCreate)
            }
            .padding()
        }
        .frame(width: 520)
        .fileImporter(isPresented: $choosingFolder, allowedContentTypes: [.folder]) { result in
            if case .success(let url) = result { parent = url }
        }
    }

    private func create() async {
        creating = true
        problem = nil
        defer { creating = false }
        if let error = await model.createInstance(name: name, org: org, slug: slug, template: template, at: folder) {
            problem = error
        } else {
            dismiss()
        }
    }
}
