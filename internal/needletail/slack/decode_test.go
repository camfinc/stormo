package slack

import (
	"bytes"
	"image"
	"image/png"
	"strings"
	"testing"
	"time"

	"github.com/camfinc/stormo/internal/needletail/attach"
	"github.com/camfinc/stormo/internal/needletail/slack/slacktest"
)

// Finding 1 (Codex): an image is decoded (and downscaled) only when it will be shown: a model
// that is not given images, or a turn that already has its images, never decodes one (each
// decode holds up to hundreds of MB); the store keeps it all the same.
func TestImagesDecodedOnlyWhenShown(t *testing.T) {
	var b bytes.Buffer
	_ = png.Encode(&b, image.NewNRGBA(image.Rect(0, 0, 2400, 30))) // wide: shown only downscaled
	pic := b.Bytes()
	sees := false
	files := &attach.Store{Home: t.TempDir()}
	r := newRig(t, func(o *Options) { o.Files = files; o.Sees = func() bool { return sees } })
	ids := []string{"F0IMG00001", "F0IMG00002", "F0IMG00003", "F0IMG00004", "F0IMG00005", "F0IMG00006"}
	var objs []map[string]any
	for _, id := range ids {
		r.fake.Files[id] = slacktest.FileBody{Body: pic, ContentType: "image/png"}
		objs = append(objs, map[string]any{"id": id, "name": id + ".png", "mimetype": "image/png", "filetype": "png", "size": len(pic), "mode": "hosted", "url_private_download": r.fake.FileURL(id)})
	}
	before := attach.Decodes()
	r.fake.Send("e1", slacktest.Event{Channel: dm, ChannelType: "im", User: alice, Text: "look", TS: "1700000500.000100", Files: objs[:1]})
	r.fake.WaitPosts(1, 5*time.Second)
	if n := attach.Decodes() - before; n != 0 {
		t.Errorf("a blind model's image was decoded %d times", n)
	}
	if in := r.turns.got(DMSessionID(dm)); len(in) != 1 || !strings.Contains(in[0], "this model is not shown images") {
		t.Errorf("turn input %q", in)
	}
	// with vision, at most 4 a turn are decoded (one message of 5 images: the fifth is not)
	sees = true
	before = attach.Decodes()
	r.fake.Send("e2", slacktest.Event{Channel: dm, ChannelType: "im", User: alice, Text: "more", TS: "1700000500.000200", Files: objs[1:]})
	r.fake.WaitPosts(2, 5*time.Second)
	if n := attach.Decodes() - before; n != 4 {
		t.Errorf("%d decodes for 5 images, 4 shown", n)
	}
}
