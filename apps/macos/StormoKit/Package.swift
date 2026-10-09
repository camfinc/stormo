// swift-tools-version: 6.2
// StormoKit: everything the Stormo app does that is not a view (docs/macos-app.md, docs/api.md).
import PackageDescription

let package = Package(
    name: "StormoKit",
    platforms: [.macOS(.v26)],
    products: [
        .library(name: "StormoKit", targets: ["StormoKit"])
    ],
    targets: [
        .target(name: "StormoKit"),
        .testTarget(
            name: "StormoKitTests",
            dependencies: ["StormoKit"],
            resources: [.copy("Fixtures")]
        ),
    ]
)
