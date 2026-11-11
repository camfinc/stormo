package link

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeDocker plays docker for a service whose container is an httptest server.
type fakeDocker struct {
	mu      sync.Mutex
	addr    string
	running bool
	calls   []string
	env     []string
}

func (f *fakeDocker) run(_ context.Context, env []string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, strings.Join(args, " "))
	switch args[0] {
	case "run":
		f.running, f.env = true, env
		return []byte("abc123\n"), nil
	case "port":
		return []byte(f.addr + "\n"), nil
	case "inspect":
		if f.running {
			return []byte("true\n"), nil
		}
		return []byte("false\n"), nil
	case "stop", "rm":
		f.running = false
	}
	return nil, nil
}

func (f *fakeDocker) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func TestServicesBootCallAndSleep(t *testing.T) {
	ready := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			if !ready {
				ready = true // the first probe finds it still loading
				w.WriteHeader(503)
				return
			}
		case "/run":
			b, _ := io.ReadAll(r.Body)
			w.Write([]byte(`{"got":` + string(b) + `}`))
		}
	}))
	defer srv.Close()
	f := &fakeDocker{addr: strings.TrimPrefix(srv.URL, "http://")}
	now := time.Unix(1000, 0)
	s := NewServices("acme")
	s.Docker, s.Now = f.run, func() time.Time { return now }
	sp := ServiceSpec{Name: "audio.transcribe", Image: "img:1", Port: 8080, Health: "/health", Env: map[string]string{"API_KEY": "s3cret", "MODEL": "small"},
		MemoryMB: 512, ShmMB: 64, JobsDir: "/tmp/jobs", Idle: 10 * time.Minute, Boot: time.Minute}

	if st := s.State("audio.transcribe"); st.State != "asleep" {
		t.Fatalf("before: %+v", st)
	}
	out, err := s.Call(context.Background(), sp, []byte(`{"job":1}`))
	if err != nil || string(out) != `{"got":{"job":1}}` {
		t.Fatalf("call: %s %v", out, err)
	}
	run := ""
	for _, c := range f.calls {
		if strings.HasPrefix(c, "run ") {
			run = c
		}
	}
	for _, want := range []string{"--name stormo-acme-cap-audio-transcribe", "--cap-drop ALL", "-p 127.0.0.1::8080", "-e API_KEY", "--memory 512m", "--shm-size 64m",
		"-v /tmp/jobs:/jobs", "-v stormo-acme-cap-audio-transcribe-cache:/cache", "label stormo.scope=acme"} {
		if !strings.Contains(run, want) {
			t.Errorf("run lacks %q: %s", want, run)
		}
	}
	if strings.Contains(run, "s3cret") || !strings.Contains(strings.Join(f.env, " "), "API_KEY=s3cret") {
		t.Errorf("secrets go through the environment, never argv: %s / %v", run, f.env)
	}
	if st := s.State("audio.transcribe"); st.State != "awake" || st.Running != 0 {
		t.Fatalf("after: %+v", st)
	}
	// Awake: the next job does not boot again.
	if _, err := s.Call(context.Background(), sp, []byte(`{"job":2}`)); err != nil || f.count("run ") != 1 {
		t.Fatalf("second call: %v, %d boots", err, f.count("run "))
	}
	// Idle under its minutes: kept; past them: asleep.
	now = now.Add(9 * time.Minute)
	s.Reap()
	if s.State("audio.transcribe").State != "awake" {
		t.Fatal("reaped early")
	}
	now = now.Add(2 * time.Minute)
	s.Reap()
	if st := s.State("audio.transcribe"); st.State != "asleep" || f.running {
		t.Fatalf("not reaped: %+v", st)
	}
	// A changed spec reboots it.
	if _, err := s.Call(context.Background(), sp, nil); err != nil {
		t.Fatal(err)
	}
	sp.Env = map[string]string{"MODEL": "large"}
	if _, err := s.Call(context.Background(), sp, nil); err != nil || f.count("run ") != 3 {
		t.Fatalf("changed spec: %v, %d boots", err, f.count("run "))
	}
	if err := s.Sleep("audio.transcribe"); err != nil || s.State("audio.transcribe").State != "asleep" {
		t.Fatalf("sleep: %v", err)
	}
}

func TestServicesBootFailureKeepsTheReason(t *testing.T) {
	f := &fakeDocker{addr: "127.0.0.1:1"}
	s := NewServices("acme")
	s.Docker = func(ctx context.Context, env []string, args ...string) ([]byte, error) {
		out, err := f.run(ctx, env, args...)
		if args[0] == "run" {
			f.running = false // exits at once
		}
		if args[0] == "logs" {
			return []byte("model file missing\n"), nil
		}
		return out, err
	}
	_, err := s.Call(context.Background(), ServiceSpec{Name: "x", Image: "i", Port: 8080, Health: "/health", Boot: 10 * time.Second}, nil)
	if err == nil || !strings.Contains(err.Error(), "model file missing") {
		t.Fatalf("err: %v", err)
	}
	if st := s.State("x"); st.State != "asleep" || !strings.Contains(st.Error, "stopped while booting") {
		t.Fatalf("state: %+v", st)
	}
}

// An always-on ingress worker: a fixed loopback port, a read-only root, no cache, a key made at each
// boot that never counts as a change; never reaped for idling, booted again when it dies, and held
// down by the owner's sleep until a release.
func TestAlwaysOnServiceKeepAndHold(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	f := &fakeDocker{addr: strings.TrimPrefix(srv.URL, "http://")}
	now := time.Unix(1000, 0)
	s := NewServices("acme")
	s.Docker, s.Now = f.run, func() time.Time { return now }
	boots := 0
	sp := ServiceSpec{Name: "devices.gateway", Image: "gw:1", Port: 8080, Health: "/device/v1/health", Boot: time.Minute,
		Publish: "127.0.0.1:18650", ReadOnly: true, NoCache: true, ExtraHosts: []string{"host.docker.internal:host-gateway"},
		BootEnv: func() map[string]string {
			boots++
			return map[string]string{"STORMO_INGRESS_KEY": fmt.Sprintf("sti_key%d", boots)}
		}}
	ctx := context.Background()
	if err := s.Keep(ctx, sp); err != nil {
		t.Fatal(err)
	}
	run := f.calls[slices.IndexFunc(f.calls, func(c string) bool { return strings.HasPrefix(c, "run ") })]
	for _, want := range []string{"-p 127.0.0.1:18650:8080", "--read-only", "--tmpfs /tmp:rw,noexec,nosuid,size=16m", "--add-host host.docker.internal:host-gateway", "-e STORMO_INGRESS_KEY"} {
		if !strings.Contains(run, want) {
			t.Errorf("run lacks %q: %s", want, run)
		}
	}
	if strings.Contains(run, "/cache") || strings.Contains(run, "sti_key") || !slices.Contains(f.env, "STORMO_INGRESS_KEY=sti_key1") {
		t.Errorf("cache volume or key in argv: %s / %v", run, f.env)
	}
	// Running with the same spec: not booted again, though BootEnv would give a new key.
	if err := s.Keep(ctx, sp); err != nil || f.count("run ") != 1 || boots != 1 {
		t.Fatalf("kept running: %v, %d boots", err, f.count("run "))
	}
	now = now.Add(48 * time.Hour)
	s.Reap()
	if s.State("devices.gateway").State != "awake" {
		t.Error("an always-on service was reaped")
	}
	// It died: the next keep boots it with a new key.
	f.mu.Lock()
	f.running = false
	f.mu.Unlock()
	if err := s.Keep(ctx, sp); err != nil || f.count("run ") != 2 || !slices.Contains(f.env, "STORMO_INGRESS_KEY=sti_key2") {
		t.Fatalf("not booted again: %v, %d boots, %v", err, f.count("run "), f.env)
	}
	// The owner's sleep holds it down; a release lets the keeper bring it back.
	if err := s.Sleep("devices.gateway"); err != nil {
		t.Fatal(err)
	}
	if err := s.Keep(ctx, sp); err != nil || f.count("run ") != 2 || s.State("devices.gateway").State != "asleep" {
		t.Fatalf("held: %v, %d boots", err, f.count("run "))
	}
	s.Release("devices.gateway")
	if err := s.Keep(ctx, sp); err != nil || f.count("run ") != 3 {
		t.Fatalf("released: %v, %d boots", err, f.count("run "))
	}
}

// A keep waiting on the boot lock while the owner holds the service asleep does not boot it.
func TestKeepRechecksTheHoldUnderTheBootLock(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	f := &fakeDocker{addr: strings.TrimPrefix(srv.URL, "http://")}
	s := NewServices("acme")
	s.Docker = f.run
	sp := ServiceSpec{Name: "devices.gateway", Image: "gw:1", Port: 8080, Health: "/h", Boot: time.Minute}
	v := s.get(sp.Name)
	v.boot.Lock() // a sleep in progress
	done := make(chan error)
	go func() { done <- s.Keep(context.Background(), sp) }()
	time.Sleep(50 * time.Millisecond)
	s.mu.Lock()
	v.held = true
	s.mu.Unlock()
	v.boot.Unlock()
	if err := <-done; err != nil || f.count("run ") != 0 {
		t.Fatalf("keep booted a held service: %v, %d boots", err, f.count("run "))
	}
}
