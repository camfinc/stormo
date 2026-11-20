package needletail

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"math/rand"
	"testing"

	"github.com/camfinc/stormo/internal/needletail/attach"
	"github.com/camfinc/stormo/pkg/engine"
)

// Files that would not fit the request go with their largest images as the model is shown them;
// files that fit go as they are, and nothing but an image is changed.
func TestChatFilesFit(t *testing.T) {
	room := filesRoom
	t.Cleanup(func() { filesRoom = room })
	src := image.NewNRGBA(image.Rect(0, 0, attach.MaxSide+400, 200))
	r := rand.New(rand.NewSource(1))
	for i := range src.Pix {
		src.Pix[i] = uint8(r.Intn(256))
	}
	for i := 3; i < len(src.Pix); i += 4 {
		src.Pix[i] = 255
	}
	var big bytes.Buffer
	if err := png.Encode(&big, src); err != nil {
		t.Fatal(err)
	}
	note := bytes.Repeat([]byte("OggS"), 1000)
	files := []engine.File{{Name: "photo.png", MIME: "image/png", Data: big.Bytes(), Marked: true}, {Name: "voice-note.ogg", MIME: "audio/ogg", Data: note}}

	got := chatFiles(context.Background(), files)
	if len(got) != 2 || !bytes.Equal(got[0].Data, big.Bytes()) || !bytes.Equal(got[1].Data, note) {
		t.Fatal("files that fit were changed")
	}

	filesRoom = big.Len() / 2
	got = chatFiles(context.Background(), files)
	cfg, _, err := image.DecodeConfig(bytes.NewReader(got[0].Data))
	if err != nil || got[0].MIME != "image/jpeg" || max(cfg.Width, cfg.Height) != attach.MaxSide || len(got[0].Data) >= big.Len() || !got[0].Marked || got[0].Name != "photo.png" {
		t.Fatalf("photo not prepared: %s %dx%d, %d bytes (%v)", got[0].MIME, cfg.Width, cfg.Height, len(got[0].Data), err)
	}
	if !bytes.Equal(got[1].Data, note) || got[1].MIME != "audio/ogg" {
		t.Error("the voice note was changed")
	}
	if len(chatFiles(context.Background(), nil)) != 0 {
		t.Error("files from none")
	}

	// two images each within the model's bounds, too large together: re-encoded smaller
	small := image.NewNRGBA(image.Rect(0, 0, 900, 600))
	for i := range small.Pix {
		small.Pix[i] = uint8(r.Intn(256))
	}
	for i := 3; i < len(small.Pix); i += 4 {
		small.Pix[i] = 255
	}
	var mid bytes.Buffer
	if err := png.Encode(&mid, small); err != nil {
		t.Fatal(err)
	}
	two := []engine.File{{Name: "photo.png", MIME: "image/png", Data: mid.Bytes()}, {Name: "quoted-photo.png", MIME: "image/png", Data: mid.Bytes()}}
	filesRoom = mid.Len() * 3 / 2
	got = chatFiles(context.Background(), two)
	if total := len(got[0].Data) + len(got[1].Data); total > filesRoom || got[0].MIME != "image/jpeg" {
		t.Fatalf("two images: %d bytes (room %d), %s", total, filesRoom, got[0].MIME)
	}
}
