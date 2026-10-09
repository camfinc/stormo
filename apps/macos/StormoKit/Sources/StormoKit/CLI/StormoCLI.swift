import Foundation

/// One line of `stormo --json` output (docs/api.md).
public enum CLIEvent: Sendable, Equatable {
    case step(String)
    case authURL(String)
    /// The raw line, decoded on demand into the command's result type.
    case result(Data)
    case error(code: String, message: String)

    private struct Line: Decodable {
        var event: String
        var msg: String?
        var url: String?
        var code: String?
    }

    /// Parses one output line; nil for anything that is not an event (never expected on stdout).
    public init?(line: some StringProtocol) {
        let data = Data(line.utf8)
        guard let l = try? JSONDecoder().decode(Line.self, from: data) else { return nil }
        switch l.event {
        case "step": self = .step(l.msg ?? "")
        case "auth_url": self = .authURL(l.url ?? "")
        case "result": self = .result(data)
        case "error": self = .error(code: l.code ?? "failed", message: l.msg ?? "")
        default: return nil
        }
    }

    /// The result's data as T.
    public func decode<T: Decodable>(_: T.Type) throws -> T? {
        guard case .result(let data) = self else { return nil }
        return try JSONDecoder().decode(Envelope<T>.self, from: data).data
    }

    private struct Envelope<T: Decodable>: Decodable { var data: T }
}

/// A failed `stormo` command: its error event, or its exit status and the end of its stderr.
public struct CLIError: Error, Equatable, LocalizedError {
    public var code: String
    public var message: String
    public var status: Int32
    public var stderr: String

    public var errorDescription: String? { message.isEmpty ? "stormo exited with status \(status)" : message }
}

/// Runs one `stormo` binary for one instance, always with `--json`, `--target local` and an
/// explicit `--instance` (docs/macos-app.md, Environment). Remote targets are out of scope.
public struct StormoCLI: Sendable {
    public let executable: URL
    public let environment: [String: String]
    public let instance: URL?

    public init(executable: URL, environment: [String: String], instance: URL?) {
        self.executable = executable
        self.environment = environment
        self.instance = instance
    }

    /// The full argument list for a command.
    public func arguments(_ command: [String]) -> [String] {
        var argv = ["--json", "--target", "local"]
        if let instance { argv += ["--instance", instance.path] }
        return argv + command
    }

    /// The process environment for a command.
    public func processEnvironment() -> [String: String] {
        var env = environment
        env.removeValue(forKey: "SWARM_TARGET")
        if let instance { env["STORMO_INSTANCE"] = instance.path }
        return env
    }

    /// Streams the command's events. An error event or a non-zero exit ends the stream with a CLIError;
    /// cancelling the consumer terminates the process.
    public func events(_ command: [String]) -> AsyncThrowingStream<CLIEvent, Error> {
        let process = Process()
        process.executableURL = executable
        process.arguments = arguments(command)
        process.environment = processEnvironment()
        if let instance { process.currentDirectoryURL = instance }
        let out = Pipe()
        let err = Pipe()
        process.standardOutput = out
        process.standardError = err
        process.standardInput = FileHandle.nullDevice
        let box = ProcessBox(process)

        return AsyncThrowingStream { continuation in
            let exited = AsyncStream<Int32>.makeStream()
            process.terminationHandler = { p in
                exited.continuation.yield(p.terminationStatus)
                exited.continuation.finish()
            }
            let stderrTail = StderrTail()
            let task = Task {
                do {
                    try box.process.run()
                } catch {
                    continuation.finish(throwing: CLIError(code: "launch", message: "cannot run \(executable.path): \(error.localizedDescription)", status: -1, stderr: ""))
                    return
                }
                let errReader = Task {
                    for try await chunk in err.fileHandleForReading.bytes.lines { await stderrTail.append(chunk) }
                }
                var failure: CLIEvent?
                do {
                    for try await line in out.fileHandleForReading.bytes.lines {
                        guard let event = CLIEvent(line: line) else { continue }
                        if case .error = event { failure = event }
                        continuation.yield(event)
                    }
                } catch {}
                var status: Int32 = -1
                for await s in exited.stream { status = s }
                _ = await errReader.result
                let tail = await stderrTail.text
                if case .error(let code, let message) = failure {
                    continuation.finish(throwing: CLIError(code: code, message: message, status: status, stderr: tail))
                } else if status != 0 {
                    continuation.finish(throwing: CLIError(code: "failed", message: tail.split(separator: "\n").last.map(String.init) ?? "", status: status, stderr: tail))
                } else {
                    continuation.finish()
                }
            }
            continuation.onTermination = { reason in
                if case .cancelled = reason {
                    task.cancel()
                    if box.process.isRunning { box.process.terminate() }
                }
            }
        }
    }

    /// Runs a command to its result, reporting other events (steps, the sign-in page) as they come.
    public func run<T: Decodable & Sendable>(_ command: [String], as type: T.Type, onEvent: @Sendable (CLIEvent) -> Void = { _ in }) async throws -> T {
        var value: T?
        for try await event in events(command) {
            if let v = try event.decode(T.self) { value = v } else { onEvent(event) }
        }
        guard let value else {
            throw CLIError(code: "no_result", message: "stormo \(command.joined(separator: " ")) gave no result", status: 0, stderr: "")
        }
        return value
    }
}

/// Process is not Sendable; the stream's task is its only user after launch.
private final class ProcessBox: @unchecked Sendable {
    let process: Process
    init(_ process: Process) { self.process = process }
}

/// The last lines of a command's stderr, for error messages.
private actor StderrTail {
    private var lines: [String] = []
    func append(_ line: String) {
        lines.append(line)
        if lines.count > 200 { lines.removeFirst(lines.count - 200) }
    }
    var text: String { lines.joined(separator: "\n") }
}
