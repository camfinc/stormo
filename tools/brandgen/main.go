// Command brandgen writes Stormo's logo files (docs/brand/*.svg) from one spiral: dots evenly
// spaced along an Archimedean curve, growing and cooling from teal to indigo, plus the monoline
// wordmark, and the macOS app icon. Edit the parameters here and regenerate every file together:
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
