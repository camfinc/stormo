import Foundation

/// The environment `stormo` runs with. An app opened from Finder gets launchd's PATH, where docker
/// and aws do not resolve, so the app asks the user's shell what a terminal would see and keeps
/// only what stormo and its tools need (docs/macos-app.md, Environment).
public enum ShellEnvironment {
    /// Variables taken from the shell. SWARM_TARGET is deliberately absent: the app runs locally only.
    public static let kept: Set<String> = [
        "PATH", "HOME", "USER", "LOGNAME", "LANG", "LC_ALL", "TMPDIR", "SHELL",
        "DOCKER_HOST", "DOCKER_CONTEXT", "SSH_AUTH_SOCK",
        "AWS_PROFILE", "AWS_REGION", "AWS_DEFAULT_REGION", "AWS_CONFIG_FILE", "AWS_SHARED_CREDENTIALS_FILE",
        "SWARM_CORE_PORT", "SWARM_SECRETS_FILE", "SWARM_REPOS_DIR", "STORMO_INSTANCE", "STORMO_SRC",
    ]

    /// Appended to PATH when missing: where Homebrew and OrbStack put their tools.
    public static func fallbackPath(home: String) -> [String] {
        ["/opt/homebrew/bin", "/usr/local/bin", "\(home)/.orbstack/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin"]
    }

    static let marker = "__STORMO_ENV_7f3a__"

    /// The shell command that prints the environment between markers, NUL-separated.
    static func script() -> String { "printf '%s' '\(marker)'; /usr/bin/env -0; printf '%s' '\(marker)'" }

    /// Parses the shell's output: the NUL-separated KEY=value block between the two markers. Noise a
    /// profile prints before or after (a greeting, a motd) is ignored.
    public static func parse(_ output: Data) -> [String: String]? {
        let m = Data(marker.utf8)
        guard let first = output.range(of: m),
              let last = output.range(of: m, options: .backwards, in: first.upperBound..<output.endIndex)
        else { return nil }
        var env: [String: String] = [:]
        for entry in output[first.upperBound..<last.lowerBound].split(separator: 0) {
            guard let s = String(data: Data(entry), encoding: .utf8), let eq = s.firstIndex(of: "="), eq != s.startIndex else { continue }
            env[String(s[..<eq])] = String(s[s.index(after: eq)...])
        }
        return env
    }

    /// The environment for stormo: the kept variables from the shell (else from this process), and a
    /// PATH that always includes the fallback directories.
    public static func compose(shell: [String: String]?, process: [String: String]) -> [String: String] {
        var env: [String: String] = [:]
        for key in kept {
            if let v = shell?[key] ?? process[key] { env[key] = v }
        }
        let home = env["HOME"] ?? process["HOME"] ?? NSHomeDirectory()
        env["HOME"] = home
        var path = (env["PATH"] ?? "").split(separator: ":").map(String.init).filter { !$0.isEmpty }
        for dir in fallbackPath(home: home) where !path.contains(dir) { path.append(dir) }
        env["PATH"] = path.joined(separator: ":")
        return env
    }

    /// Asks the login shell (`-ilc`: .zprofile and .zshrc both count), stdin from /dev/null, killed
    /// after the timeout. Nil when the shell fails or prints nothing usable.
    public static func capture(shell: String, timeout: Duration = .seconds(5)) async -> [String: String]? {
        let process = Process()
        process.executableURL = URL(filePath: shell)
        process.arguments = ["-ilc", script()]
        process.standardInput = FileHandle.nullDevice
        let out = Pipe()
        process.standardOutput = out
        process.standardError = FileHandle.nullDevice
        let box = UncheckedProcess(process)
        do { try process.run() } catch { return nil }
        let killer = Task {
            try? await Task.sleep(for: timeout)
            if box.process.isRunning { box.process.terminate() }
        }
        let data = await Task.detached { out.fileHandleForReading.readDataToEndOfFile() }.value
        box.process.waitUntilExit()
        killer.cancel()
        return parse(data)
    }

    /// The environment for stormo on this Mac: the user's shell, or this process's as a fallback.
    public static func current() async -> [String: String] {
        let process = ProcessInfo.processInfo.environment
        let shell = process["SHELL"].flatMap { $0.isEmpty ? nil : $0 } ?? "/bin/zsh"
        return compose(shell: await capture(shell: shell), process: process)
    }

    /// Finds a tool on the environment's PATH.
    public static func which(_ tool: String, in env: [String: String]) -> URL? {
        for dir in (env["PATH"] ?? "").split(separator: ":") {
            let url = URL(filePath: String(dir)).appending(path: tool)
            if FileManager.default.isExecutableFile(atPath: url.path) { return url }
        }
        return nil
    }
}

private final class UncheckedProcess: @unchecked Sendable {
    let process: Process
    init(_ process: Process) { self.process = process }
}
