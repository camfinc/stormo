// The media worker's adapter: ffmpeg operations on one file for the capability contract
// (docs/capabilities.md): GET /health, POST /run {job, agent, input, files, out}. Standard
// library only: the image builds it with no module.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const font = "/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf"

// formats are the cover canvases: width and height.
var formats = map[string][2]int{"story": {1080, 1920}, "portrait": {1080, 1350}, "square": {1080, 1080}, "landscape": {1920, 1080}}

type job struct {
	Job   int64    `json:"job"`
	Files []string `json:"files"`
	Out   string   `json:"out"`
	Input struct {
		Op       string          `json:"op"`
		At       json.RawMessage `json:"at"`
		Count    int             `json:"count"`
		N        int             `json:"n"`
		Formats  []string        `json:"formats"`
		Fit      string          `json:"fit"`
		Focus    []float64       `json:"focus"`
		LUFS     *float64        `json:"lufs"`
		TruePeak *float64        `json:"true_peak"`
		Name     string          `json:"name"`
	} `json:"input"`
}

type jobError struct {
	code int
	msg  string
}

func (e jobError) Error() string { return e.msg }

func bad(format string, a ...any) error { return jobError{400, fmt.Sprintf(format, a...)} }

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") })
	mux.HandleFunc("POST /run", func(w http.ResponseWriter, r *http.Request) {
		var j job
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&j); err != nil {
			http.Error(w, "body: "+err.Error(), 400)
			return
		}
		out, err := run(r.Context(), j)
		if err != nil {
			code := 422
			var je jobError
			if errors.As(err, &je) {
				code = je.code
			}
			http.Error(w, err.Error(), code)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	})
	log.Printf("media: listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}

func run(ctx context.Context, j job) (map[string]any, error) {
	if len(j.Files) == 0 {
		return nil, bad("give the video or audio file (files)")
	}
	src := j.Files[0]
	name := safeName(j.Input.Name, strings.TrimSuffix(filepath.Base(src), filepath.Ext(src)))
	writes := j.Input.Op != "probe" && j.Input.Op != "loudness"
	if writes && j.Out == "" {
		return nil, bad("no out folder for the files this writes")
	}
	p, err := probe(ctx, src)
	if err != nil {
		return nil, err
	}
	switch j.Input.Op {
	case "probe":
		return p.view(), nil
	case "loudness":
		if !p.audio {
			return nil, bad("%s has no audio", filepath.Base(src))
		}
		return loudness(ctx, src)
	case "qa":
		out := p.view()
		if p.audio {
			l, err := loudness(ctx, src)
			if err != nil {
				return nil, err
			}
			out["integrated_lufs"], out["true_peak"] = l["integrated_lufs"], l["true_peak"]
		}
		files, err := stills(ctx, src, spread(p.duration, max(1, min(orInt(j.Input.Count, 3), 12))), j.Out, name)
		if err != nil {
			return nil, err
		}
		out["files"] = files
		return out, nil
	case "stills":
		if !p.video {
			return nil, bad("%s has no video", filepath.Base(src))
		}
		times := []float64{}
		if len(j.Input.At) > 0 {
			if err := json.Unmarshal(j.Input.At, &times); err != nil {
				return nil, bad("at is a list of seconds")
			}
		} else {
			times = spread(p.duration, max(1, min(orInt(j.Input.Count, 3), 24)))
		}
		files, err := stills(ctx, src, clampTimes(times, p.duration), j.Out, name)
		if err != nil {
			return nil, err
		}
		return map[string]any{"files": files}, nil
	case "master":
		if !p.audio {
			return nil, bad("%s has no audio to master", filepath.Base(src))
		}
		return master(ctx, src, j, name)
	case "cover_candidates":
		if !p.video {
			return nil, bad("%s has no video", filepath.Base(src))
		}
		res, err := analyse(ctx, src, p.duration, max(1, min(orInt(j.Input.N, 9), 12)))
		if err != nil {
			return nil, err
		}
		sheet := name + "-candidates.jpg"
		if err := contactSheet(ctx, src, res.Candidates, filepath.Join(j.Out, sheet)); err != nil {
			return nil, err
		}
		return map[string]any{"duration_seconds": round(p.duration), "cuts": res.Cuts, "candidates": res.Candidates, "best": res.Best,
			"best_at": res.BestAt, "sheet": sheet, "files": []string{sheet}}, nil
	case "cover":
		if !p.video {
			return nil, bad("%s has no video", filepath.Base(src))
		}
		return cover(ctx, src, p.duration, j, name)
	}
	return nil, bad("op is probe, qa, stills, loudness, master, cover_candidates or cover")
}

func orInt(v, d int) int {
	if v <= 0 {
		return d
	}
	return v
}

var unsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func safeName(s, d string) string {
	n := strings.TrimLeft(unsafe.ReplaceAllString(s, "-"), ".-")
	if n == "" {
		n = strings.TrimLeft(unsafe.ReplaceAllString(d, "-"), ".-")
	}
	if n == "" {
		n = "media"
	}
	return n
}

func round(f float64) float64 { return math.Round(f*1000) / 1000 }

func ff(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "ffmpeg", append([]string{"-nostdin", "-hide_banner"}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if len(msg) > 800 {
			msg = msg[len(msg)-800:]
		}
		return out.Bytes(), jobError{422, "ffmpeg: " + msg}
	}
	return append(out.Bytes(), errb.Bytes()...), nil
}

// ---- probe and loudness

type probed struct {
	duration      float64
	size          int64
	width, height int
	fps           string
	vcodec        string
	acodec        string
	video, audio  bool
}

func (p probed) view() map[string]any {
	m := map[string]any{"duration_seconds": round(p.duration), "size_mb": math.Round(float64(p.size)/1e4) / 100, "has_audio": p.audio}
	if p.video {
		m["width"], m["height"], m["fps"], m["video_codec"] = p.width, p.height, p.fps, p.vcodec
	}
	if p.audio {
		m["audio_codec"] = p.acodec
	}
	return m
}

func probe(ctx context.Context, src string) (probed, error) {
	cmd := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-show_entries", "format=duration,size:stream=codec_type,codec_name,width,height,r_frame_rate,disposition",
		"-of", "json", src)
	b, err := cmd.Output()
	if err != nil {
		return probed{}, jobError{422, "ffprobe could not read " + filepath.Base(src)}
	}
	var j struct {
		Format struct {
			Duration string `json:"duration"`
			Size     string `json:"size"`
		} `json:"format"`
		Streams []struct {
			Type        string         `json:"codec_type"`
			Codec       string         `json:"codec_name"`
			W           int            `json:"width"`
			H           int            `json:"height"`
			FPS         string         `json:"r_frame_rate"`
			Disposition map[string]int `json:"disposition"`
		} `json:"streams"`
	}
	_ = json.Unmarshal(b, &j)
	p := probed{}
	p.duration, _ = strconv.ParseFloat(j.Format.Duration, 64)
	p.size, _ = strconv.ParseInt(j.Format.Size, 10, 64)
	for _, s := range j.Streams {
		switch {
		case s.Type == "video" && !p.video && s.Disposition["attached_pic"] == 0:
			p.video, p.width, p.height, p.fps, p.vcodec = true, s.W, s.H, s.FPS, s.Codec
		case s.Type == "audio" && !p.audio:
			p.audio, p.acodec = true, s.Codec
		}
	}
	if p.duration <= 0 {
		return p, jobError{422, filepath.Base(src) + ": unknown duration"}
	}
	return p, nil
}

var loudJSON = regexp.MustCompile(`(?s)\{[^{}]*"input_i"[^{}]*\}`)

func loudnormStats(ctx context.Context, src, filter string) (map[string]string, error) {
	out, err := ff(ctx, "-nostats", "-i", src, "-vn", "-af", filter, "-f", "null", "-")
	if err != nil {
		return nil, err
	}
	blobs := loudJSON.FindAll(out, -1)
	if len(blobs) == 0 {
		return nil, jobError{422, "ffmpeg measured no loudness"}
	}
	m := map[string]string{}
	_ = json.Unmarshal(blobs[len(blobs)-1], &m)
	return m, nil
}

func num(s string) float64 {
	f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return math.Round(f*100) / 100
}

func loudness(ctx context.Context, src string) (map[string]any, error) {
	m, err := loudnormStats(ctx, src, "loudnorm=print_format=json")
	if err != nil {
		return nil, err
	}
	return map[string]any{"integrated_lufs": num(m["input_i"]), "true_peak": num(m["input_tp"]), "loudness_range": num(m["input_lra"])}, nil
}

func master(ctx context.Context, src string, j job, name string) (map[string]any, error) {
	lufs, tp := -16.0, -1.5
	if j.Input.LUFS != nil {
		lufs = *j.Input.LUFS
	}
	if j.Input.TruePeak != nil {
		tp = *j.Input.TruePeak
	}
	if lufs < -40 || lufs > -5 || tp < -9 || tp > 0 {
		return nil, bad("lufs from -40 to -5, true_peak from -9 to 0")
	}
	target := fmt.Sprintf("I=%g:TP=%g:LRA=11", lufs, tp)
	m, err := loudnormStats(ctx, src, "loudnorm="+target+":print_format=json")
	if err != nil {
		return nil, err
	}
	ext := filepath.Ext(src)
	if ext == "" {
		ext = ".mp4"
	}
	dst := name + ".mastered" + ext
	af := fmt.Sprintf("loudnorm=%s:measured_I=%s:measured_TP=%s:measured_LRA=%s:measured_thresh=%s:offset=%s:linear=true:print_format=json",
		target, m["input_i"], m["input_tp"], m["input_lra"], m["input_thresh"], m["target_offset"])
	args := []string{"-nostats", "-y", "-i", src, "-c:v", "copy", "-af", af, "-ar", "48000"}
	if ext == ".mp4" || ext == ".mov" || ext == ".m4a" {
		args = append(args, "-c:a", "aac", "-b:a", "192k")
	}
	out, err := ff(ctx, append(args, filepath.Join(j.Out, dst))...)
	if err != nil {
		return nil, err
	}
	res := map[string]any{"files": []string{dst}, "before": map[string]any{"integrated_lufs": num(m["input_i"]), "true_peak": num(m["input_tp"])}}
	if blobs := loudJSON.FindAll(out, -1); len(blobs) > 0 {
		after := map[string]string{}
		_ = json.Unmarshal(blobs[len(blobs)-1], &after)
		res["after"] = map[string]any{"integrated_lufs": num(after["output_i"]), "true_peak": num(after["output_tp"])}
	}
	return res, nil
}

// ---- stills

func spread(dur float64, n int) []float64 {
	out := []float64{}
	for k := 1; k <= n; k++ {
		out = append(out, math.Round(dur*float64(k)/float64(n+1)*100)/100)
	}
	return out
}

func clampTimes(ts []float64, dur float64) []float64 {
	out := []float64{}
	for _, t := range ts {
		if len(out) == 24 {
			break
		}
		out = append(out, math.Min(math.Max(0, t), math.Max(0, dur-0.05)))
	}
	return out
}

func stills(ctx context.Context, src string, times []float64, dir, name string) ([]string, error) {
	files := []string{}
	for _, t := range times {
		f := fmt.Sprintf("%s-%gs.png", name, t)
		if _, err := ff(ctx, "-v", "error", "-y", "-ss", fmt.Sprintf("%.3f", t), "-i", src, "-frames:v", "1", filepath.Join(dir, f)); err != nil {
			return nil, err
		}
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			files = append(files, f)
		}
	}
	if len(files) > 1 {
		sheet := name + "-contact.png"
		args := []string{"-v", "error", "-y"}
		var fc strings.Builder
		for i, f := range files {
			args = append(args, "-i", filepath.Join(dir, f))
			fmt.Fprintf(&fc, "[%d]scale=360:-2[s%d];", i, i)
		}
		for i := range files {
			fmt.Fprintf(&fc, "[s%d]", i)
		}
		fmt.Fprintf(&fc, "hstack=inputs=%d", len(files))
		if _, err := ff(ctx, append(args, "-filter_complex", fc.String(), filepath.Join(dir, sheet))...); err == nil {
			files = append(files, sheet)
		}
	}
	return files, nil
}

// ---- cover moments (the same scoring as a scripted cover lane: sharp, contrasty, well exposed,
// distinct; black, blown-out and flat frames never offered while anything else is)

type candidate struct {
	N          int     `json:"n"`
	At         float64 `json:"at"`
	Score      float64 `json:"score"`
	Sharpness  float64 `json:"sharpness"`
	Contrast   float64 `json:"contrast"`
	Brightness float64 `json:"brightness"`
	AfterCut   bool    `json:"after_cut"`
	sig        []float64
}

type analysis struct {
	Candidates []*candidate
	Cuts       int
	Best       int
	BestAt     float64
}

func gray(ctx context.Context, src string, t float64, width int) (int, int, []byte, bool) {
	cmd := exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-v", "error", "-ss", fmt.Sprintf("%.3f", t), "-i", src, "-frames:v", "1",
		"-vf", fmt.Sprintf("scale=%d:-2,format=gray", width), "-f", "rawvideo", "-")
	b, err := cmd.Output()
	if err != nil || len(b) < width*2 {
		return 0, 0, nil, false
	}
	return width, len(b) / width, b, true
}

func stats(w, h int, d []byte) (mean, std, sharp float64) {
	n := w * h
	var sum float64
	for _, v := range d[:n] {
		sum += float64(v)
	}
	mean = sum / float64(n)
	var sq float64
	for _, v := range d[:n] {
		sq += (float64(v) - mean) * (float64(v) - mean)
	}
	std = math.Sqrt(sq / float64(n))
	var lap float64
	m := 0
	for y := 1; y < h-1; y += 2 {
		for x := 1; x < w-1; x += 2 {
			i := y*w + x
			v := 4*float64(d[i]) - float64(d[i-1]) - float64(d[i+1]) - float64(d[i-w]) - float64(d[i+w])
			lap += v * v
			m++
		}
	}
	return mean, std, lap / float64(max(1, m))
}

func signature(w, h int, d []byte) []float64 {
	const grid = 8
	out := make([]float64, 0, grid*grid)
	for gy := 0; gy < grid; gy++ {
		for gx := 0; gx < grid; gx++ {
			y0, y1, x0, x1 := gy*h/grid, (gy+1)*h/grid, gx*w/grid, (gx+1)*w/grid
			var tot float64
			for y := y0; y < y1; y++ {
				for x := x0; x < x1; x++ {
					tot += float64(d[y*w+x])
				}
			}
			out = append(out, tot/float64(max(1, (y1-y0)*(x1-x0))))
		}
	}
	return out
}

func unlike(a, b []float64) bool {
	var s float64
	for i := range a {
		s += math.Abs(a[i] - b[i])
	}
	return s/float64(len(a)) > 10
}

func sceneCuts(ctx context.Context, src string, dur float64) []float64 {
	if dur > 600 {
		return nil // long files: evenly spread samples only
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-hide_banner", "-nostats", "-i", src, "-an", "-vf", "scale=320:-2,select='gt(scene,0.3)',showinfo", "-f", "null", "-")
	var errb bytes.Buffer
	cmd.Stderr = &errb
	_ = cmd.Run()
	seen := map[float64]bool{}
	cuts := []float64{}
	for _, m := range regexp.MustCompile(`pts_time:([\d.]+)`).FindAllStringSubmatch(errb.String(), -1) {
		f, _ := strconv.ParseFloat(m[1], 64)
		f = math.Round(f*1000) / 1000
		if !seen[f] {
			seen[f] = true
			cuts = append(cuts, f)
		}
	}
	sort.Float64s(cuts)
	return cuts
}

func candidateTimes(dur float64, cuts []float64) []float64 {
	lo, hi := dur*0.03, dur*0.97
	bounds := []float64{0}
	for _, c := range cuts {
		if c > 0 && c < dur {
			bounds = append(bounds, c)
		}
	}
	bounds = append(bounds, dur)
	ts := []float64{}
	for i := 0; i+1 < len(bounds); i++ {
		if bounds[i+1]-bounds[i] >= 0.6 {
			ts = append(ts, (bounds[i]+bounds[i+1])/2)
		}
	}
	const samples = 16
	for i := 0; i < samples; i++ {
		ts = append(ts, lo+(hi-lo)*(float64(i)+0.5)/samples)
	}
	sort.Float64s(ts)
	out := []float64{}
	for _, t := range ts {
		if t < lo || t > hi {
			continue
		}
		if len(out) == 0 || t-out[len(out)-1] >= 0.4 {
			out = append(out, math.Round(t*100)/100)
		}
	}
	if len(out) == 0 {
		out = []float64{math.Round(dur*50) / 100}
	}
	return out
}

func analyse(ctx context.Context, src string, dur float64, n int) (*analysis, error) {
	cuts := sceneCuts(ctx, src, dur)
	type raw struct {
		t                float64
		mean, std, sharp float64
		sig              []float64
	}
	var rs []raw
	for _, t := range candidateTimes(dur, cuts) {
		w, h, d, ok := gray(ctx, src, t, 160)
		if !ok {
			continue
		}
		m, s, sh := stats(w, h, d)
		rs = append(rs, raw{t, m, s, sh, signature(w, h, d)})
	}
	if len(rs) == 0 {
		return nil, jobError{422, "could not read frames"}
	}
	sharps := []float64{}
	bestStd := 0.0
	for _, r := range rs {
		sharps = append(sharps, r.sharp)
		bestStd = math.Max(bestStd, r.std)
	}
	sort.Float64s(sharps)
	bestSharp := sharps[int(float64(len(sharps))*0.9)]
	all := []*candidate{}
	for _, r := range rs {
		sc := 0.0
		if r.mean >= 18 && r.mean <= 240 && r.std >= 8 {
			exposure := math.Max(0, 1-math.Abs(r.mean-120)/120)
			sc = 0.55*math.Min(1, r.sharp/math.Max(bestSharp, 1e-9)) + 0.30*math.Min(1, r.std/math.Max(bestStd, 1e-9)) + 0.15*exposure
		}
		after := false
		for _, c := range cuts {
			after = after || (r.t-c >= 0 && r.t-c < 1.5)
		}
		all = append(all, &candidate{At: r.t, Score: math.Round(sc*1e4) / 1e4, Sharpness: math.Round(r.sharp*10) / 10, Contrast: math.Round(r.std*10) / 10,
			Brightness: math.Round(r.mean*10) / 10, AfterCut: after, sig: r.sig})
	}
	byScore := append([]*candidate{}, all...)
	sort.SliceStable(byScore, func(i, j int) bool { return byScore[i].Score > byScore[j].Score })
	anyGood := byScore[0].Score > 0
	gap := dur / (float64(n) * 2.5)
	chosen := []*candidate{}
	for _, distinct := range []bool{true, false} {
		for _, c := range byScore {
			if len(chosen) == n {
				break
			}
			taken, near, alike := false, false, false
			for _, o := range chosen {
				taken = taken || o == c
				near = near || math.Abs(c.At-o.At) < gap
				alike = alike || !unlike(c.sig, o.sig)
			}
			if taken || near || (c.Score == 0 && anyGood) || (distinct && alike) {
				continue
			}
			chosen = append(chosen, c)
		}
	}
	sort.Slice(chosen, func(i, j int) bool { return chosen[i].At < chosen[j].At })
	best := chosen[0]
	for i, c := range chosen {
		c.N = i + 1
		if c.Score > best.Score {
			best = c
		}
	}
	return &analysis{Candidates: chosen, Cuts: len(cuts), Best: best.N, BestAt: best.At}, nil
}

func contactSheet(ctx context.Context, src string, cands []*candidate, dst string) error {
	dir, err := os.MkdirTemp("", "sheet-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	for i, c := range cands {
		label := fmt.Sprintf("#%d  %.1fs", c.N, c.At)
		vf := fmt.Sprintf("scale='if(gt(iw,ih),480,300)':-2,drawtext=fontfile=%s:text='%s':x=12:y=h-th-14:fontsize=26:fontcolor=white:box=1:boxcolor=black@0.65:boxborderw=8", font, label)
		if _, err := ff(ctx, "-v", "error", "-y", "-ss", fmt.Sprintf("%.3f", c.At), "-i", src, "-frames:v", "1", "-vf", vf, filepath.Join(dir, fmt.Sprintf("t-%02d.jpg", i+1))); err != nil {
			return err
		}
	}
	cols := 3
	if len(cands) <= 4 {
		cols = len(cands)
	}
	rows := (len(cands) + cols - 1) / cols
	_, err = ff(ctx, "-v", "error", "-y", "-start_number", "1", "-i", filepath.Join(dir, "t-%02d.jpg"),
		"-vf", fmt.Sprintf("tile=%dx%d:padding=6:margin=6:color=white", cols, rows), "-frames:v", "1", "-q:v", "3", dst)
	return err
}

// ---- covers

func fitFilter(sw, sh, w, h int, fit string, fx, fy float64) (string, string) {
	aSrc, aDst := float64(sw)/float64(sh), float64(w)/float64(h)
	if fit == "" || fit == "auto" {
		fit = "crop"
		if math.Max(aSrc/aDst, aDst/aSrc) > 1.6 {
			fit = "blur"
		}
	}
	cw, ch := float64(sw), float64(sw)/aDst
	if aSrc > aDst {
		cw, ch = float64(sh)*aDst, float64(sh)
	}
	x, y := (float64(sw)-cw)*fx, (float64(sh)-ch)*fy
	crop := fmt.Sprintf("crop=%d:%d:%d:%d,scale=%d:%d:flags=lanczos,setsar=1", int(cw), int(ch), int(x), int(y), w, h)
	if fit == "crop" {
		return crop, "crop"
	}
	fg := fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease:flags=lanczos", w, h)
	return fmt.Sprintf("split[a][b];[a]%s,boxblur=30:2,eq=brightness=-0.08[bg];[b]%s[fg];[bg][fg]overlay=(W-w)/2:(H-h)/2,setsar=1", crop, fg), "blur"
}

func cover(ctx context.Context, src string, dur float64, j job, name string) (map[string]any, error) {
	fmts := j.Input.Formats
	if len(fmts) == 0 {
		fmts = []string{"story"}
	}
	for _, f := range fmts {
		if _, ok := formats[f]; !ok {
			return nil, bad("formats are story, portrait, square and landscape, not %q", f)
		}
	}
	if j.Input.Fit != "" && j.Input.Fit != "auto" && j.Input.Fit != "crop" && j.Input.Fit != "blur" {
		return nil, bad("fit is auto, crop or blur")
	}
	fx, fy := 0.5, 0.5
	if len(j.Input.Focus) > 0 {
		fx = math.Min(1, math.Max(0, j.Input.Focus[0]))
	}
	if len(j.Input.Focus) > 1 {
		fy = math.Min(1, math.Max(0, j.Input.Focus[1]))
	}
	var at float64
	picked := "given"
	if len(j.Input.At) > 0 && string(j.Input.At) != "null" {
		if err := json.Unmarshal(j.Input.At, &at); err != nil {
			return nil, bad("at is the moment in seconds")
		}
		at = math.Min(math.Max(0, at), math.Max(0, dur-0.05))
	} else {
		res, err := analyse(ctx, src, dur, 9)
		if err != nil {
			return nil, err
		}
		at, picked = res.BestAt, "best score"
	}
	tmp, err := os.MkdirTemp("", "cover-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	frame := filepath.Join(tmp, "frame.png")
	if _, err := ff(ctx, "-v", "error", "-y", "-ss", fmt.Sprintf("%.3f", at), "-i", src, "-frames:v", "1", frame); err != nil {
		return nil, err
	}
	b, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=width,height", "-of", "csv=p=0", frame).Output()
	if err != nil {
		return nil, jobError{422, "could not read the frame"}
	}
	var sw, sh int
	fmt.Sscanf(strings.TrimSpace(string(b)), "%d,%d", &sw, &sh)
	if sw == 0 || sh == 0 {
		return nil, jobError{422, "the frame has no size"}
	}
	covers := []map[string]any{}
	files := []string{}
	for _, f := range fmts {
		wh := formats[f]
		vf, used := fitFilter(sw, sh, wh[0], wh[1], j.Input.Fit, fx, fy)
		dst := fmt.Sprintf("%s-cover-%s.jpg", name, f)
		flag := "-vf"
		if strings.Contains(vf, "[") {
			flag = "-filter_complex"
		}
		if _, err := ff(ctx, "-v", "error", "-y", "-i", frame, flag, vf, "-frames:v", "1", "-q:v", "2", filepath.Join(j.Out, dst)); err != nil {
			return nil, err
		}
		covers = append(covers, map[string]any{"format": f, "file": dst, "width": wh[0], "height": wh[1], "fit": used})
		files = append(files, dst)
	}
	return map[string]any{"at": round(at), "picked": picked, "covers": covers, "files": files}, nil
}
