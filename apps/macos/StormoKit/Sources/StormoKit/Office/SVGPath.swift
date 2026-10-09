import CoreGraphics
import Foundation

/// The subset of SVG path data the office's figures use: M L H V Q C Z, absolute and relative.
public enum SVGPath {
    public static func parse(_ d: String) -> CGPath {
        let path = CGMutablePath()
        var tokens = tokenize(d)[...]
        var cmd: Character = "M"
        var cur = CGPoint.zero, startPt = CGPoint.zero

        func num() -> CGFloat {
            guard case .number(let n)? = tokens.first else { return 0 }
            tokens.removeFirst()
            return n
        }
        func point(_ rel: Bool) -> CGPoint {
            let x = num(), y = num()
            return rel ? CGPoint(x: cur.x + x, y: cur.y + y) : CGPoint(x: x, y: y)
        }

        while let t = tokens.first {
            if case .command(let c) = t {
                cmd = c
                tokens.removeFirst()
                if c == "Z" || c == "z" {
                    path.closeSubpath()
                    cur = startPt
                    continue
                }
            }
            let rel = cmd.isLowercase
            switch cmd.uppercased() {
            case "M":
                cur = point(rel)
                startPt = cur
                path.move(to: cur)
                cmd = rel ? "l" : "L"  // further pairs are lines
            case "L":
                cur = point(rel)
                path.addLine(to: cur)
            case "H":
                let x = num()
                cur = CGPoint(x: rel ? cur.x + x : x, y: cur.y)
                path.addLine(to: cur)
            case "V":
                let y = num()
                cur = CGPoint(x: cur.x, y: rel ? cur.y + y : y)
                path.addLine(to: cur)
            case "Q":
                let c1 = point(rel), p = point(rel)
                path.addQuadCurve(to: p, control: c1)
                cur = p
            case "C":
                let c1 = point(rel), c2 = point(rel), p = point(rel)
                path.addCurve(to: p, control1: c1, control2: c2)
                cur = p
            default:
                tokens.removeFirst()
            }
        }
        return path
    }

    enum Token: Equatable {
        case command(Character)
        case number(CGFloat)
    }

    static func tokenize(_ d: String) -> [Token] {
        var out: [Token] = []
        var numberText = ""
        func flush() {
            if let n = Double(numberText) { out.append(.number(CGFloat(n))) }
            numberText = ""
        }
        for ch in d {
            if ch.isLetter && ch != "e" && ch != "E" {
                flush()
                out.append(.command(ch))
            } else if ch == "-" && !numberText.isEmpty && numberText.last != "e" {
                flush()
                numberText = "-"
            } else if ch == "." && numberText.contains(".") {
                flush()
                numberText = "."
            } else if ch.isNumber || ch == "." || ch == "-" || ch == "e" || ch == "E" {
                numberText.append(ch)
            } else {
                flush()
            }
        }
        flush()
        return out
    }
}
