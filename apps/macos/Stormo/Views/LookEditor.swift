import AppKit
import SpriteKit
import StormoKit
import SwiftUI

/// An agent's look: its office character (colours, hair, accessory) with a live preview, what sits
/// on its desk, and the description its portrait is baked from. Used by the New Agent wizard and the
/// agent editor's Look page; both save it through `stormo` (personas/<id>/persona.md).
struct LookEditor: View {
    @Binding var look: LookDraft

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            HStack(alignment: .top, spacing: 18) {
                FigurePreview(sprite: look.resolved)
                    .frame(width: 96, height: 120)
                    .background(.quaternary.opacity(0.5), in: .rect(cornerRadius: 10))
                    .accessibilityLabel("Preview of the office character")
                Grid(alignment: .leading, horizontalSpacing: 12, verticalSpacing: 8) {
                    GridRow {
                        label("Skin")
                        HStack(spacing: 6) {
                            ForEach(LookDraft.skins, id: \.self) { c in
                                Swatch(hex: c, selected: look.skin == c) { look.skin = c }
                            }
                            HexColorPicker(hex: $look.skin)
                        }
                    }
                    GridRow {
                        label("Hair")
                        HStack {
                            Picker("Hair style", selection: $look.hairStyle) {
                                ForEach(OfficeLook.hairStyles, id: \.self) { Text($0.capitalized).tag($0) }
                            }
                            .labelsHidden().frame(width: 110)
                            HexColorPicker(hex: $look.hair)
                        }
                    }
                    GridRow {
                        label("Clothes")
                        HStack {
                            HexColorPicker(hex: $look.shirt).help("Shirt")
                            HexColorPicker(hex: $look.pants).help("Trousers")
                        }
                    }
                    GridRow {
                        label("Extra")
                        Picker("Accessory", selection: $look.accessory) {
                            ForEach(LookDraft.accessories, id: \.self) { Text($0 == "none" ? "None" : $0.capitalized).tag($0) }
                        }
                        .labelsHidden().frame(width: 110)
                    }
                }
            }

            VStack(alignment: .leading, spacing: 6) {
                Text("Desk").font(.callout.weight(.medium))
                HStack {
                    Picker("Screens", selection: $look.screens) {
                        Text("1 screen").tag(1)
                        Text("2 screens").tag(2)
                    }
                    .labelsHidden().frame(width: 110)
                    ForEach(look.apps.indices, id: \.self) { i in
                        Picker("Screen \(i + 1)", selection: $look.apps[i]) {
                            ForEach(LookDraft.apps, id: \.self) { Text($0.capitalized).tag($0) }
                        }
                        .labelsHidden().frame(width: 100)
                    }
                    Picker("Beside the desk", selection: $look.side) {
                        ForEach(LookDraft.sides, id: \.self) { Text(title($0)).tag($0) }
                    }
                    .labelsHidden().frame(width: 120)
                    .help("Beside the desk")
                }
                FlowChips(all: LookDraft.props, selected: $look.props, limit: LookDraft.maxProps)
                Text("Up to \(LookDraft.maxProps) things on the desk, in the order you pick them.")
                    .font(.caption).foregroundStyle(.secondary)
            }

            VStack(alignment: .leading, spacing: 6) {
                Text("Portrait description").font(.callout.weight(.medium))
                TextEditor(text: $look.description)
                    .font(.body)
                    .frame(minHeight: 60)
                    .scrollContentBackground(.hidden)
                    .padding(6)
                    .background(.background, in: .rect(cornerRadius: 8))
                    .overlay(RoundedRectangle(cornerRadius: 8).strokeBorder(.quaternary))
                Text("Who the portrait shows (face, hair, clothes, what they hold). A portrait is baked from it separately; until then chats show its initials on its shirt colour.")
                    .font(.caption).foregroundStyle(.secondary)
            }
        }
    }

    private func label(_ s: String) -> some View {
        Text(s).font(.callout).foregroundStyle(.secondary).gridColumnAlignment(.trailing)
    }

    private func title(_ s: String) -> String { s.replacingOccurrences(of: "-", with: " ").capitalized }
}

/// A colour well bound to "#rrggbb".
private struct HexColorPicker: View {
    @Binding var hex: String

    var body: some View {
        ColorPicker("", selection: Binding(get: { Color(nsColor: Self.color(hex)) },
                                           set: { hex = Self.hex($0) }), supportsOpacity: false)
            .labelsHidden()
    }

    static func color(_ hex: String) -> NSColor {
        let v = UInt32(hex.dropFirst(), radix: 16) ?? 0
        return NSColor(srgbRed: CGFloat(v >> 16 & 0xff) / 255, green: CGFloat(v >> 8 & 0xff) / 255, blue: CGFloat(v & 0xff) / 255, alpha: 1)
    }

    static func hex(_ c: Color) -> String {
        let n = NSColor(c).usingColorSpace(.sRGB) ?? .black
        func b(_ x: CGFloat) -> Int { Int((max(0, min(1, x)) * 255).rounded()) }
        return String(format: "#%02x%02x%02x", b(n.redComponent), b(n.greenComponent), b(n.blueComponent))
    }
}

private struct Swatch: View {
    let hex: String
    let selected: Bool
    let action: () -> Void

    var body: some View {
        Button(action: action) {
            Circle().fill(Color(nsColor: HexColorPicker.color(hex)))
                .frame(width: 18, height: 18)
                .overlay(Circle().strokeBorder(selected ? Color.accentColor : .clear, lineWidth: 2).padding(-3))
        }
        .buttonStyle(.plain)
        .accessibilityLabel("Skin tone \(hex)")
    }
}

/// Toggleable chips; the selection keeps the order they were picked in.
private struct FlowChips: View {
    let all: [String]
    @Binding var selected: [String]
    let limit: Int

    var body: some View {
        LazyVGrid(columns: [GridItem(.adaptive(minimum: 84), spacing: 6)], alignment: .leading, spacing: 6) {
            ForEach(all, id: \.self) { name in
                let on = selected.contains(name)
                Button {
                    if on { selected.removeAll { $0 == name } } else if selected.count < limit { selected.append(name) }
                } label: {
                    Text(name.replacingOccurrences(of: "-", with: " "))
                        .font(.caption)
                        .frame(maxWidth: .infinity)
                        .padding(.vertical, 4)
                        .background(on ? Color.accentColor.opacity(0.25) : Color.secondary.opacity(0.1), in: .capsule)
                        .overlay(Capsule().strokeBorder(on ? Color.accentColor : .clear))
                }
                .buttonStyle(.plain)
                .disabled(!on && selected.count >= limit)
            }
        }
    }
}

/// The office character, standing and facing front, drawn to an image as its colours change.
private struct FigurePreview: View {
    let sprite: ResolvedSprite

    var body: some View {
        if let image = Self.render(sprite) {
            Image(decorative: image, scale: 2).interpolation(.none)
        }
    }

    @MainActor private static let renderer = SKView(frame: CGRect(x: 0, y: 0, width: 96, height: 120))

    @MainActor static func render(_ sprite: ResolvedSprite) -> CGImage? {
        let root = SKNode()
        let p = PersonNode(agentID: "preview", sprite: sprite)
        p.setScale(3.2)
        p.apply(PersonFrame(point: .zero, pose: .stand, facing: .down), reduceMotion: true)
        root.addChild(p)
        let box = CGRect(x: -96, y: -120, width: 192, height: 240)
        return renderer.texture(from: root, crop: box)?.cgImage()
    }
}
