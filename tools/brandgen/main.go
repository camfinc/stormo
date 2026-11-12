// Command brandgen writes Stormo's logo files (docs/brand/*.svg) from one spiral: dots evenly
// spaced along an Archimedean curve, growing and cooling from teal to indigo, plus the monoline
// wordmark, the macOS app icon and the README's diagram in the same colours. Edit the parameters
// here and regenerate every file together:
//
//	go run ./tools/brandgen [-out docs/brand] [-icon apps/macos/Stormo/AppIcon.icon]
//
// social-preview.png is a browser render of social-preview.svg at 1280×640 (docs/brand/README.md).
package main

import (
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"strings"
)

const (
	teal, indigo, tileBG = "#2dd4bf", "#818cf8", "#0d141c"
	inkDark, inkLight    = "#e8eef5", "#0d141c"
	head                 = `<svg xmlns="http://www.w3.org/2000/svg"`
)

type dot struct {
	x, y, r float64
	color   string
}

// spiral is the parameters of one cut of the mark.
type spiral struct {
	step, rmin, rgrow, rmax, a0, b0 float64
}

var (
	full    = spiral{step: 5.2, rmin: 0.9, rgrow: 3.1, rmax: 22, a0: 1.0, b0: 2.55}
	favicon = spiral{step: 6.6, rmin: 2.4, rgrow: 2.8, rmax: 20, a0: 1.5, b0: 2.9} // nine larger dots, legible at 16 px
)

func mix(c1, c2 string, t float64) string {
	var a, b [3]int
	fmt.Sscanf(c1, "#%02x%02x%02x", &a[0], &a[1], &a[2])
	fmt.Sscanf(c2, "#%02x%02x%02x", &b[0], &b[1], &b[2])
	out := "#"
	for i := range a {
		out += fmt.Sprintf("%02x", int(math.Round(float64(a[i])+float64(b[i]-a[i])*t)))
	}
	return out
}

func (s spiral) dots() []dot {
	type pt struct{ th, r float64 }
	pts := []pt{}
	for th := 0.0; ; {
		r := s.a0 + s.b0*th
		if r > s.rmax {
			break
		}
		pts = append(pts, pt{th, r})
		th += s.step / math.Max(r, 2.5)
	}
	out := make([]dot, len(pts))
	for i, p := range pts {
		t := float64(i) / float64(len(pts)-1)
		out[i] = dot{p.r * math.Cos(p.th-1.2), p.r * math.Sin(p.th-1.2), s.rmin + t*s.rgrow, mix(teal, indigo, t)}
	}
	return out
}

// fit scales and moves the dots into a box×box square with pad on each side.
func fit(ds []dot, box, pad float64) []dot {
	x0, y0, x1, y1 := math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
	for _, d := range ds {
		x0, y0 = math.Min(x0, d.x-d.r), math.Min(y0, d.y-d.r)
		x1, y1 = math.Max(x1, d.x+d.r), math.Max(y1, d.y+d.r)
	}
	k := (box - 2*pad) / math.Max(x1-x0, y1-y0)
	dx, dy := box/2-(x0+x1)/2*k, box/2-(y0+y1)/2*k
	out := make([]dot, len(ds))
	for i, d := range ds {
		out[i] = dot{dx + d.x*k, dy + d.y*k, d.r * k, d.color}
	}
	return out
}

func (d dot) circle() string {
	return fmt.Sprintf(`<circle cx="%.2f" cy="%.2f" r="%.2f" fill="%s"/>`, d.x, d.y, d.r, d.color)
}

// centred is the dots fitted into a box×box square, as SVG circles.
func centred(ds []dot, box, pad float64) string {
	var b strings.Builder
	for _, d := range fit(ds, box, pad) {
		b.WriteString(d.circle())
	}
	return b.String()
}

// appIcon is the macOS app's Icon Composer document (AppIcon.icon): the tile colour as its fill
// and every dot of the mark as its own opaque glass layer on Apple's 1024-point canvas, so each
// dot catches light separately and the overlapping outer dots stay distinct. The first layer is
// frontmost: the centre of the spiral sits on top, as in the flat mark.
func appIcon(ds []dot) map[string]string {
	var r, g, b int
	fmt.Sscanf(tileBG, "#%02x%02x%02x", &r, &g, &b)
	files := map[string]string{}
	layers := []string{}
	for i, d := range fit(ds, 1024, 180) {
		name := fmt.Sprintf("dot%02d", i)
		files["Assets/"+name+".svg"] = fmt.Sprintf(`%s viewBox="0 0 1024 1024" width="1024" height="1024">%s</svg>`, head, d.circle())
		layers = append(layers, fmt.Sprintf(`        {
          "glass" : true,
          "image-name" : "%s.svg",
          "name" : "%s"
        }`, name, name))
	}
	files["icon.json"] = fmt.Sprintf(`{
  "fill" : {
    "solid" : "srgb:%.5f,%.5f,%.5f,1.00000"
  },
  "groups" : [
    {
      "layers" : [
%s
      ],
      "shadow" : {
        "kind" : "neutral",
        "opacity" : 0.5
      },
      "translucency" : {
        "enabled" : false,
        "value" : 0.5
      }
    }
  ],
  "supported-platforms" : {
    "squares" : [
      "macOS"
    ]
  }
}`, float64(r)/255, float64(g)/255, float64(b)/255, strings.Join(layers, ",\n"))
	return files
}

// wordmark is "stormo" in monoline strokes: x-height 12..40, stroke 6, round caps.
func wordmark(ink string) (string, float64) {
	letters := []struct {
		d string
		w float64
	}{
		{"M21 15 Q18 12 11 12 Q2 12 2 19 Q2 25.5 11 26 Q21 26.5 21 33 Q21 40 11 40 Q4 40 1 36", 22}, // s
		{"M6 2 V33 Q6 40 13 40 M0 13 H13", 14},                                                      // t
		{"M14 12 a14 14 0 1 0 0.01 0 Z", 28},                                                        // o
		{"M2 40 V12 M2 24 Q2 12 15 12", 16},                                                         // r
		{"M2 40 V12 M2 23 Q2 12 11 12 Q20 12 20 23 V40 M20 23 Q20 12 29 12 Q38 12 38 23 V40", 40},   // m
		{"M14 12 a14 14 0 1 0 0.01 0 Z", 28},                                                        // o
	}
	var b strings.Builder
	x := 3.0
	for _, l := range letters {
		fmt.Fprintf(&b, `<path transform="translate(%g 0)" d="%s" fill="none" stroke="%s" stroke-width="6" stroke-linecap="round" stroke-linejoin="round"/>`, x, l.d, ink)
		x += l.w + 8
	}
	return b.String(), x
}

func main() {
	out := flag.String("out", "docs/brand", "directory to write into")
	icon := flag.String("icon", "apps/macos/Stormo/AppIcon.icon", "the macOS app icon to write")
	flag.Parse()
	mark := full.dots()
	files := map[string]string{
		"mark.svg":      fmt.Sprintf(`%s viewBox="0 0 64 64"><title>Stormo</title><rect width="64" height="64" rx="14" fill="%s"/>%s</svg>`, head, tileBG, centred(mark, 64, 10)),
		"favicon.svg":   fmt.Sprintf(`%s viewBox="0 0 64 64"><rect width="64" height="64" rx="14" fill="%s"/>%s</svg>`, head, tileBG, centred(favicon.dots(), 64, 8)),
		"mark-bare.svg": fmt.Sprintf(`%s viewBox="0 0 64 64"><title>Stormo</title>%s</svg>`, head, centred(mark, 64, 4)),
	}
	for theme, ink := range map[string]string{"light": inkLight, "dark": inkDark} {
		wm, width := wordmark(ink)
		files["wordmark-"+theme+".svg"] = fmt.Sprintf(`%s viewBox="0 0 %g 46"><title>stormo</title>%s</svg>`, head, width, wm)
		// Lockup: the bare mark 64 units tall, the wordmark at 0.82 of its height, centred on it.
		s := 0.82 * 64 / 46
		files["logo-"+theme+".svg"] = fmt.Sprintf(`%s viewBox="0 0 %.1f 64"><title>Stormo</title>%s<g transform="translate(82 %.2f) scale(%.4f)">%s</g></svg>`,
			head, 64+18+width*s, centred(mark, 64, 4), 32-23*s, s, wm)
		files["flow-"+theme+".svg"] = flow(flowThemes[theme])
	}
	wm, _ := wordmark(inkDark)
	files["social-preview.svg"] = fmt.Sprintf(`%s viewBox="0 0 1280 640" width="1280" height="640"><rect width="1280" height="640" fill="%s"/>`+
		`<g transform="translate(250 190) scale(4.1)">%s</g><g transform="translate(560 262) scale(2.6)">%s</g>`+
		`<text x="563" y="420" fill="#8b98a8" font-family="Helvetica Neue, Arial, sans-serif" font-size="30">An agent swarm engine</text></svg>`,
		head, tileBG, centred(mark, 64, 2), wm)
	files["dmg-background.svg"] = dmgBackground(mark)
	write(*out, files)
	// A spiral with fewer dots must not leave stale layers behind.
	if err := os.RemoveAll(filepath.Join(*icon, "Assets")); err != nil {
		log.Fatal(err)
	}
	write(*icon, appIcon(mark))
}

// dmgBackground is the installer window's picture, 640×400 points: the app at (170, 190) and the
// Applications link at (470, 190) (apps/macos/scripts/dmg.sh places them), joined by a row of
// dots (Finder draws icon labels black over a picture, so each sits on a light chip) that grows and cools like the spiral, over a faint cut of the mark.
func dmgBackground(mark []dot) string {
	var arrow strings.Builder
	const n = 7
	for i := range n {
		t := float64(i) / (n - 1)
		arrow.WriteString(dot{262 + t*116, 190, 2.2 + t*3.2, mix(teal, indigo, t)}.circle())
	}
	return fmt.Sprintf(`%[1]s viewBox="0 0 640 400" width="640" height="400">`+
		`<defs><radialGradient id="glow" cx="0.5" cy="0.45" r="0.7"><stop offset="0" stop-color="#16212d"/><stop offset="1" stop-color="%[2]s"/></radialGradient></defs>`+
		`<rect width="640" height="400" fill="url(#glow)"/>`+
		`<g opacity="0.045" transform="translate(140 10)">%[3]s</g>%[6]s`+
		`<rect x="115" y="252" width="110" height="22" rx="11" fill="%[5]s" opacity="0.9"/><rect x="415" y="252" width="110" height="22" rx="11" fill="%[5]s" opacity="0.9"/>`+
		`<path d="M390 182 l10 8 -10 8" fill="none" stroke="%[4]s" stroke-width="3" stroke-linecap="round" stroke-linejoin="round"/>`+
		`<text x="320" y="330" text-anchor="middle" fill="#8b98a8" font-family="Helvetica Neue, Arial, sans-serif" font-size="14">Drag Stormo to Applications to install</text></svg>`,
		head, tileBG, centred(mark, 360, 0), indigo, inkDark, arrow.String())
}

// flowTheme is the README diagram's colours for GitHub's light or dark page: out is the way to a
// running agent, back is what it learned coming home. bg is GitHub's page colour, so the diagram's
// own background disappears there yet keeps it legible when the browser picks the other file (the
// system's scheme and the GitHub theme disagree).
type flowTheme struct{ bg, ink, muted, out, back string }

var flowThemes = map[string]flowTheme{
	"light": {"#ffffff", inkLight, "#5b6672", mix(teal, inkLight, 0.3), mix(indigo, inkLight, 0.2)},
	"dark":  {"#0d1117", inkDark, "#8b98a8", teal, indigo},
}

// flow is the README's "How it fits together": the instance is built into a baseline, a task
// rehydrates from it and its latest nap, naps come back through the dream and a person's review
// into the ledger, and accepted lessons ship with the next build. Three lanes, as the README's
// words have them; the arrows cool from teal (out) to indigo (back) like the spiral.
func flow(t flowTheme) string {
	var b strings.Builder
	f := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	const (
		r1, r3 = 128.0, 296.0 // the centre lines of the top and bottom rows, cards 60 tall
		top    = r1 + 30      // the top row's bottom edge
		bottom = r3 - 30      // the bottom row's top edge
		lanes  = r3 + 60      // the lanes' height; the legend sits below
	)
	card := func(x, cy, w float64, title, sub, edge string) {
		f(`<rect x="%g" y="%g" width="%g" height="60" rx="10" class="card"%s/>`, x, cy-30, w, edge)
		f(`<text x="%g" y="%g" text-anchor="middle" class="t">%s</text>`, x+w/2, cy-4, title)
		f(`<text x="%g" y="%g" text-anchor="middle" class="s">%s</text>`, x+w/2, cy+15, sub)
	}
	arrow := func(color, marker, d string, a ...any) {
		f(`<path d="%s" fill="none" stroke="%s" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" marker-end="url(#%s)"/>`, fmt.Sprintf(d, a...), color, marker)
	}
	// A size of its own, so the README's <img> fills the column (max-width: 100%) in every browser.
	f(`%[1]s viewBox="0 0 940 %[2]g" width="940" height="%[2]g" role="img"><title>How Stormo fits together</title>`, head, lanes+42)
	f(`<defs><style>text{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI","Helvetica Neue",Arial,sans-serif;fill:%[1]s}`+
		`.t{font-size:15px;font-weight:600}.s{font-size:12px;fill:%[2]s}.m{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:13px}`+
		`.h{font-size:12px;font-weight:600;letter-spacing:1.4px}.card{fill:%[1]s;fill-opacity:.05;stroke:%[1]s;stroke-opacity:.22}</style>`,
		t.ink, t.muted)
	for _, m := range []struct{ id, color string }{{"out", t.out}, {"back", t.back}} {
		f(`<marker id="%s" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse"><path d="M1 1 L9 5 L1 9" fill="none" stroke="%s" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/></marker>`, m.id, m.color)
	}
	f(`<linearGradient id="cool" gradientUnits="userSpaceOnUse" x1="0" y1="%g" x2="0" y2="%g"><stop offset="0" stop-color="%s"/><stop offset="1" stop-color="%s"/></linearGradient></defs>`, top, r3, t.out, t.back)
	f(`<rect width="940" height="%g" rx="14" fill="%s"/>`, lanes+42, t.bg)

	for _, l := range []struct {
		x, w       float64
		title, sub string
	}{{0, 244, "INSTANCE", "plain files in git"}, {260, 304, "STORMO", "one Go binary"}, {580, 360, "WHERE AGENTS RUN", "local compose or ECS task"}} {
		f(`<rect x="%g" y="0" width="%g" height="%g" rx="14" fill="%s" fill-opacity=".03"/>`, l.x, l.w, lanes, t.ink)
		f(`<text x="%g" y="34" text-anchor="middle" class="h">%s</text><text x="%g" y="52" text-anchor="middle" class="s">%s</text>`, l.x+l.w/2, l.title, l.x+l.w/2, l.sub)
	}

	// Out: the instance's files, built into a baseline, booted with the latest nap.
	f(`<rect x="18" y="%g" width="208" height="84" rx="10" class="card"/>`, r1-42)
	for i, name := range []string{"stormo.yaml", "units/", "agents/"} {
		f(`<text x="42" y="%g" class="m">%s</text>`, r1-16+float64(i)*20, name)
	}
	card(274, r1, 118, "build", "for its engine", "")
	card(430, r1, 118, "baseline", "the starting home", "")
	card(598, r1, 148, "rehydrate", "baseline + latest nap", "")
	card(784, r1, 138, "agent", "Hermes or needletail", "")
	for _, x := range [][2]float64{{226, 272}, {392, 428}, {548, 596}, {746, 782}} {
		arrow(t.out, "out", "M%g %g H%g", x[0], r1, x[1])
	}

	// The sidecar's naps, cooling from out to back.
	pill := (top + r3) / 2
	f(`<path d="M853 %g V%g" fill="none" stroke="url(#cool)" stroke-width="2" stroke-linecap="round"/>`, top, pill-16)
	f(`<rect x="823" y="%g" width="60" height="28" rx="14" class="card" style="stroke:url(#cool);stroke-opacity:1"/>`, pill-14)
	f(`<text x="853" y="%g" text-anchor="middle" class="t" style="font-size:14px">nap</text>`, pill+5)
	arrow(t.back, "back", "M853 %g V%g H748", pill+14, r3)
	card(598, r3, 148, "naps", "dir or S3", "")
	arrow(t.back, "back", "M672 %g V%g", bottom, top+2)
	f(`<text x="684" y="%g" class="s">latest nap</text>`, (top+bottom)/2+4)

	// Back: the dream proposes, a person reviews, accepted lessons ship with the next build.
	card(430, r3, 118, "dream", "PII scrubbed", "")
	card(274, r3, 118, "review", "a person decides", fmt.Sprintf(` style="stroke:%s;stroke-opacity:1"`, t.back))
	f(`<rect x="18" y="%g" width="208" height="60" rx="10" class="card"/>`, bottom)
	f(`<text x="122" y="%g" text-anchor="middle" class="m">learnings/ledger.jsonl</text><text x="122" y="%g" text-anchor="middle" class="s">proposed lessons</text>`, r3-4, r3+15)
	for _, x := range [][2]float64{{596, 550}, {430, 394}, {274, 228}} {
		arrow(t.back, "back", "M%g %g H%g", x[0], r3, x[1])
	}
	arrow(t.back, "back", "M60 %g V%g", bottom, r1+44)
	f(`<text x="74" y="%g" class="s">accepted lessons</text><text x="74" y="%g" class="s">ship with the next build</text>`, (r1+42+bottom)/2-4, (r1+42+bottom)/2+12)

	// Legend.
	f(`<path d="M300 %[1]g H330" stroke="%[2]s" stroke-width="2" stroke-linecap="round" marker-end="url(#out)"/><text x="340" y="%[3]g" class="s">out: build and run</text>`, lanes+24, t.out, lanes+28)
	f(`<path d="M500 %[1]g H530" stroke="%[2]s" stroke-width="2" stroke-linecap="round" marker-end="url(#back)"/><text x="540" y="%[3]g" class="s">back: learn and review</text>`, lanes+24, t.back, lanes+28)
	b.WriteString(`</svg>`)
	return b.String()
}

func write(dir string, files map[string]string) {
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			log.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body+"\n"), 0o644); err != nil {
			log.Fatal(err)
		}
	}
	fmt.Printf("wrote %d files to %s\n", len(files), dir)
}
