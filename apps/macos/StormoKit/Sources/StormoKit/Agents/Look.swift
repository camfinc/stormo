import Foundation

/// An agent's look as `look show` / `look set` carry it (docs/api.md): its office character, its desk
/// and the description a portrait is baked from.
public struct LookSpec: Codable, Sendable, Equatable {
    public struct Desk: Codable, Sendable, Equatable {
        public var apps: [String]?
        public var screens: Int?
        public var props: [String]?
        public var side: String?
    }
    public var description: String
    public var sprite: Sprite?
    public var desk: Desk?
}

/// `look show <agent>`.
public struct AgentLook: Codable, Sendable, Equatable {
    public var agent: String
    /// The persona folder, empty when the agent has none yet.
    public var path: String
    /// persona.md's hash, for `look set --if-hash`.
    public var hash: String
    public var look: LookSpec
    public var editable: Bool
    public var reason: String?
    /// Other agents with the same persona: an edit changes them too.
    public var shared: [String]?
}

/// A look as the editor holds it: every field set, so the preview always has something to draw.
/// Seeded from what the office would draw for this agent anyway (colours picked by its id, its
/// unit's hue), so defining a look starts from the character people already see.
public struct LookDraft: Equatable, Sendable {
    public var description = ""
    public var skin: String
    public var hair: String
    public var hairStyle: String
    public var shirt: String
    public var pants: String
    public var accessory: String
    /// What each monitor shows; its count is the number of screens (1 or 2).
    public var apps: [String]
    public var props: [String]
    public var side: String

    public static let skins = ["#f1c7a5", "#d9a47e", "#c08a62", "#8d5a3b", "#5c3a24"]
    public static let accessories = ["none"] + OfficeLook.accessories
    public static let apps = ["chat", "inbox", "records", "code", "design", "charts"]
    public static let props = OfficeLook.props.sorted()
    public static let sides = OfficeLook.sides.sorted()
    public static let maxProps = 6

    /// What the office draws for agent `id` in `unit` with `look` (nil: nothing set yet).
    public init(id: String, unit: String, look: LookSpec?, office: OfficeLook) {
        let s = office.sprite(id: id, unit: unit, look?.sprite)
        skin = s.skin
        hair = s.hair
        hairStyle = s.hairStyle
        shirt = s.shirt
        pants = s.pants
        accessory = s.accessory
        description = look?.description ?? ""
        let d = look?.desk
        var a = (d?.apps ?? []).filter { Self.apps.contains($0) }
        if a.isEmpty { a = ["chat"] }
        if (d?.screens ?? a.count) >= 2 && a.count == 1 { a.append(a[0]) }
        apps = Array(a.prefix(2))
        props = d?.props.map { Array($0.filter(OfficeLook.props.contains).prefix(Self.maxProps)) } ?? ["notes", "mug", "plant"]
        side = d?.side.flatMap { OfficeLook.sides.contains($0) ? $0 : nil } ?? "bin"
    }

    public var screens: Int {
        get { apps.count }
        set { apps = newValue >= 2 ? [apps[0], apps.count > 1 ? apps[1] : apps[0]] : [apps[0]] }
    }

    public var resolved: ResolvedSprite {
        ResolvedSprite(skin: skin, hair: hair, hairStyle: hairStyle, shirt: shirt, pants: pants, accessory: accessory)
    }

    public var spec: LookSpec {
        LookSpec(description: description.trimmingCharacters(in: .whitespacesAndNewlines),
                 sprite: Sprite(skin: skin, hair: hair, shirt: shirt, pants: pants, hairStyle: hairStyle, accessory: accessory),
                 desk: .init(apps: apps, screens: apps.count, props: props, side: side))
    }
}
