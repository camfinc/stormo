import CryptoKit
import Foundation
import Security

/// Trusts what the system trusts, plus one pinned certificate (SHA-256 of its DER) the user chose
/// to trust on first use. It remembers the leaf it last saw, so the app can show its fingerprint.
public final class PinningDelegate: NSObject, URLSessionDelegate, @unchecked Sendable {
    public let pin: String?
    private let lock = NSLock()
    private var seen: String?

    public init(pin: String?) { self.pin = pin }

    /// The fingerprint of the last certificate the server presented.
    public var lastFingerprint: String? {
        lock.lock(); defer { lock.unlock() }
        return seen
    }

    public static func fingerprint(_ der: Data) -> String {
        SHA256.hash(data: der).map { String(format: "%02X", $0) }.joined(separator: ":")
    }

    public func urlSession(_ session: URLSession, didReceive challenge: URLAuthenticationChallenge,
                           completionHandler: @escaping (URLSession.AuthChallengeDisposition, URLCredential?) -> Void) {
        guard challenge.protectionSpace.authenticationMethod == NSURLAuthenticationMethodServerTrust,
              let trust = challenge.protectionSpace.serverTrust else {
            return completionHandler(.performDefaultHandling, nil)
        }
        if let chain = SecTrustCopyCertificateChain(trust) as? [SecCertificate], let leaf = chain.first {
            let fp = Self.fingerprint(SecCertificateCopyData(leaf) as Data)
            lock.lock(); seen = fp; lock.unlock()
            if let pin, pin == fp { return completionHandler(.useCredential, URLCredential(trust: trust)) }
        }
        if SecTrustEvaluateWithError(trust, nil) {
            return completionHandler(.useCredential, URLCredential(trust: trust))
        }
        completionHandler(.cancelAuthenticationChallenge, nil)
    }
}
