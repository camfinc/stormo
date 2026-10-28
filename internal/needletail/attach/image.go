package attach

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"net/http"
	"sync"
	"sync/atomic"
)

// MaxDecodes is how many images the runtime decodes at once, whatever conversation asks: a decode
// of a 50-megapixel image holds hundreds of MB, so they wait for each other (Codex finding 1).
const MaxDecodes = 1

var (
	decodeSlots = make(chan struct{}, MaxDecodes)
	decodes     atomic.Int64 // decodes done (or begun)
	decodeMu    sync.Mutex
	inflight    int
	peak        int
)

// Decodes is how many images the runtime has decoded since it started (a counter for tests and
// logs); PeakDecodes the most that ran at once.
func Decodes() int64 { return decodes.Load() }

// PeakDecodes is the most decodes that ran at once.
func PeakDecodes() int {
	decodeMu.Lock()
	defer decodeMu.Unlock()
	return peak
}

// decodeSlot waits for a decode slot (or ctx); release gives it back.
func decodeSlot(ctx context.Context) (release func(), err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case decodeSlots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	decodes.Add(1)
	decodeMu.Lock()
	inflight++
	peak = max(peak, inflight)
	decodeMu.Unlock()
	return func() {
		decodeMu.Lock()
		inflight--
		decodeMu.Unlock()
		<-decodeSlots
	}, nil
}

// Bounds of an image shown to the model.
const (
	// MaxSide is the longest side an image is shown at (px): a larger one is downscaled.
	MaxSide = 1568
	// MaxImageBytes is the most an image shown to the model may hold once prepared.
	MaxImageBytes = 4 << 20
	// maxPixels is the largest image decoded (a decompression bomb is refused before it is).
	maxPixels = 50_000_000
)

// Image is an upload as the model is shown it.
type Image struct {
	MIME          string
	Data          []byte
	Width, Height int
}

// ImageSize is an image's width and height from its header alone (no decode); 0, 0 when they
// cannot be read (a WebP, a damaged file).
func ImageSize(data []byte) (int, int) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0
	}
	return cfg.Width, cfg.Height
}

// Sniff is a file's media type from its first bytes (net/http's sniffer): what the bytes are,
// whatever the sender said.
func Sniff(b []byte) string {
	t := http.DetectContentType(b)
	if i := bytes.IndexByte([]byte(t), ';'); i >= 0 {
		t = t[:i]
	}
	return t
}

// ShowableImage is whether a media type is one the model may be shown.
func ShowableImage(mime string) bool {
	switch mime {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	}
	return false
}

// PrepareImage is an upload's bytes as the model is shown them: what they really are (sniffed)
// must be a PNG, JPEG, GIF or WebP; a PNG, JPEG or GIF over MaxSide or MaxImageBytes is decoded
// (refused past maxPixels, before decoding) and downscaled to MaxSide on its longest side,
// re-encoded as JPEG (PNG when it has transparency; a GIF's first frame). A WebP cannot be
// decoded here (no module for it): it is shown as it is when within MaxImageBytes, else refused.
func PrepareImage(data []byte) (Image, error) { return PrepareImageCtx(context.Background(), data) }

// PrepareImageCtx is PrepareImage, waiting for one of the runtime's MaxDecodes decode slots (or
// ctx) before it decodes: a check of the bytes and a small image's header need none.
func PrepareImageCtx(ctx context.Context, data []byte) (Image, error) {
	mime := Sniff(data)
	if !ShowableImage(mime) {
		return Image{}, fmt.Errorf("not an image the model can be shown (%s)", mime)
	}
	if mime == "image/webp" {
		if len(data) > MaxImageBytes {
			return Image{}, fmt.Errorf("a WebP image over %d MB cannot be shown (it cannot be downscaled here)", MaxImageBytes>>20)
		}
		return Image{MIME: mime, Data: data}, nil
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return Image{}, fmt.Errorf("the image cannot be read: %v", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > maxPixels {
		return Image{}, fmt.Errorf("the image is %dx%d, too large to decode", cfg.Width, cfg.Height)
	}
	if max(cfg.Width, cfg.Height) <= MaxSide && len(data) <= MaxImageBytes && mime != "image/gif" {
		return Image{MIME: mime, Data: data, Width: cfg.Width, Height: cfg.Height}, nil
	}
	release, err := decodeSlot(ctx)
	if err != nil {
		return Image{}, fmt.Errorf("the image was not prepared: %v", err)
	}
	defer release()
	var src image.Image
	switch mime {
	case "image/png":
		src, err = png.Decode(bytes.NewReader(data))
	case "image/jpeg":
		src, err = jpeg.Decode(bytes.NewReader(data))
	case "image/gif":
		src, err = gif.Decode(bytes.NewReader(data))
	}
	if err != nil {
		return Image{}, fmt.Errorf("the image cannot be read: %v", err)
	}
	dst := Downscale(src, MaxSide)
	var buf bytes.Buffer
	out := "image/jpeg"
	if opaque(dst) {
		err = jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 85})
	} else {
		out = "image/png"
		err = (&png.Encoder{CompressionLevel: png.BestSpeed}).Encode(&buf, dst)
	}
	if err != nil {
		return Image{}, err
	}
	if buf.Len() > MaxImageBytes {
		return Image{}, errors.New("the image is too large to show, even downscaled")
	}
	b := dst.Bounds()
	return Image{MIME: out, Data: buf.Bytes(), Width: b.Dx(), Height: b.Dy()}, nil
}

func opaque(img image.Image) bool {
	if o, ok := img.(interface{ Opaque() bool }); ok {
		return o.Opaque()
	}
	return false
}

// Downscale is src with its longest side at most side (box filter: each pixel the mean of the
// source pixels it covers); src itself when it is small enough.
func Downscale(src image.Image, side int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if max(w, h) <= side {
		return src
	}
	dw, dh := side, side
	if w >= h {
		dh = max(1, h*side/w)
	} else {
		dw = max(1, w*side/h)
	}
	dst := image.NewNRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < dh; y++ {
		y0, y1 := b.Min.Y+y*h/dh, b.Min.Y+(y+1)*h/dh
		y1 = max(y1, y0+1)
		for x := 0; x < dw; x++ {
			x0, x1 := b.Min.X+x*w/dw, b.Min.X+(x+1)*w/dw
			x1 = max(x1, x0+1)
			var r, g, bl, a, n uint64
			for sy := y0; sy < y1; sy++ {
				for sx := x0; sx < x1; sx++ {
					c := color.NRGBA64Model.Convert(src.At(sx, sy)).(color.NRGBA64)
					r += uint64(c.R)
					g += uint64(c.G)
					bl += uint64(c.B)
					a += uint64(c.A)
					n++
				}
			}
			dst.SetNRGBA(x, y, color.NRGBA{R: uint8(r / n >> 8), G: uint8(g / n >> 8), B: uint8(bl / n >> 8), A: uint8(a / n >> 8)})
		}
	}
	return dst
}
