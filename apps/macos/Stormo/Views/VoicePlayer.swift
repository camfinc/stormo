import AVFoundation
import Observation
import SwiftUI

/// Plays one voice clip at a time: a provider's preview (https, streamed) or a note on this Mac
/// (an MP3 under the instance's .swarm/voice/). Starting another stops the one playing.
@MainActor @Observable
final class VoicePlayer {
    static let shared = VoicePlayer()

    /// What is playing, nil when nothing is.
    private(set) var playing: URL?
    /// The playing clip's position and length, in seconds (length 0 until known).
    private(set) var position: Double = 0
    private(set) var length: Double = 0
    /// The last clip that could not be played, and why.
    private(set) var failed: (url: URL, message: String)?

    @ObservationIgnored private var player: AVPlayer?
    @ObservationIgnored private var ticks: Any?
    @ObservationIgnored private var observers: [NSObjectProtocol] = []

    func isPlaying(_ url: URL?) -> Bool { url != nil && playing == url }

    /// Plays url, or stops it when it is the one playing.
    func toggle(_ url: URL) {
        if playing == url { stop() } else { play(url) }
    }

    func play(_ url: URL) {
        stop()
        failed = nil
        let item = AVPlayerItem(url: url)
        let p = AVPlayer(playerItem: item)
        player = p
        playing = url
        ticks = p.addPeriodicTimeObserver(forInterval: CMTime(value: 1, timescale: 10), queue: .main) { [weak self] time in
            MainActor.assumeIsolated {
                guard let self, self.playing == url else { return }
                self.position = time.seconds.isFinite ? time.seconds : 0
                let d = item.duration.seconds
                if d.isFinite, d > 0 { self.length = d }
            }
        }
        let center = NotificationCenter.default
        observers = [
            center.addObserver(forName: AVPlayerItem.didPlayToEndTimeNotification, object: item, queue: .main) { [weak self] _ in
                MainActor.assumeIsolated { if self?.playing == url { self?.stop() } }
            },
            center.addObserver(forName: AVPlayerItem.failedToPlayToEndTimeNotification, object: item, queue: .main) { [weak self] n in
                let message = (n.userInfo?[AVPlayerItemFailedToPlayToEndTimeErrorKey] as? Error)?.localizedDescription
                MainActor.assumeIsolated { self?.fail(url, message ?? "It stopped playing.") }
            },
        ]
        p.play()
        Task { [weak self] in
            // A link that does not play (gone, refused) never reaches the end.
            for _ in 0..<40 {
                try? await Task.sleep(for: .milliseconds(250))
                guard let self, self.playing == url else { return }
                if item.status == .failed {
                    self.fail(url, item.error?.localizedDescription ?? "It could not be played.")
                    return
                }
                if item.status == .readyToPlay { return }
            }
        }
    }

    func stop() {
        if let ticks { player?.removeTimeObserver(ticks) }
        ticks = nil
        observers.forEach(NotificationCenter.default.removeObserver)
        observers = []
        player?.pause()
        player = nil
        playing = nil
        position = 0
        length = 0
    }

    private func fail(_ url: URL, _ message: String) {
        guard playing == url else { return }
        stop()
        failed = (url, message)
    }
}

/// ▶ / ■ for one clip.
struct PlayButton: View {
    let url: URL?
    var label = "Play"
    /// Stops the clip when the button goes (a sheet closing); a chat row scrolled away keeps playing.
    var stopsWhenGone = true
    private var player: VoicePlayer { .shared }

    var body: some View {
        let on = player.isPlaying(url)
        Button(on ? "Stop" : label, systemImage: on ? "stop.circle.fill" : "play.circle.fill") {
            if let url { player.toggle(url) }
        }
        .labelStyle(.iconOnly)
        .buttonStyle(.borderless)
        .font(.title3)
        .foregroundStyle(url == nil ? Color.secondary : Color.accentColor)
        .disabled(url == nil)
        .help(url == nil ? "No sample to play" : on ? "Stop" : label)
        .onDisappear { if stopsWhenGone, player.isPlaying(url) { player.stop() } }
    }
}

/// A voice note inline: play/stop, how far along, its length.
struct AudioClip: View {
    let url: URL
    private var player: VoicePlayer { .shared }

    var body: some View {
        let on = player.isPlaying(url)
        HStack(spacing: 8) {
            PlayButton(url: url, label: "Play the voice note", stopsWhenGone: false)
            ProgressView(value: on && player.length > 0 ? min(player.position / player.length, 1) : 0)
                .progressViewStyle(.linear)
                .frame(width: 140)
            Text(on ? "\(clock(player.position)) / \(clock(player.length))" : "Voice note")
                .font(.caption).monospacedDigit().foregroundStyle(.secondary)
            if let f = player.failed, f.url == url {
                Image(systemName: "exclamationmark.triangle").foregroundStyle(.orange).help(f.message)
            }
        }
    }

    private func clock(_ s: Double) -> String { Duration.seconds(max(0, s)).formatted(.time(pattern: .minuteSecond)) }
}
