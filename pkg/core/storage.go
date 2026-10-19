package core

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/camfinc/stormo/pkg/aws"
)

// Mirror keeps the shared space in S3 as well as in the core's workdir (stormo.yaml
// core.storage: s3://bucket/prefix, docs/remote-core.md §5): pulled when the core starts, so a
// core on a new host begins with what was there, then pushed every interval and when it stops.
// The core is S3's only writer. A push deletes what is gone from the workdir only after a pull
// succeeded, so an empty workdir on a fresh host never empties the bucket.
type Mirror struct {
	Dir    string
	URI    string
	Region string
	CLI    aws.CLI
	mu     sync.Mutex
	pulled bool
}

func (m *Mirror) run(args ...string) error {
	full := append([]string{"s3", "sync"}, args...)
	if m.Region != "" {
		full = append(full, "--region", m.Region)
	}
	r := m.CLI(append(full, "--only-show-errors", "--exclude", "*/.fs_write-*", "--exclude", "*/.stormo-sync-*"), nil)
	if r.Code != 0 {
		return fmt.Errorf("aws s3 sync: %s", strings.TrimSpace(r.Stderr))
	}
	return nil
}

// Pull brings S3's copy into the workdir (nothing in the workdir is deleted).
func (m *Mirror) Pull() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.run(m.URI, m.Dir); err != nil {
		return err
	}
	m.pulled = true
	return nil
}

// Push mirrors the workdir to S3, deletions included once a pull succeeded.
func (m *Mirror) Push() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	args := []string{m.Dir, m.URI}
	if m.pulled {
		args = append(args, "--delete")
	}
	return m.run(args...)
}

// Run pulls once, then pushes every interval until stop, and once more when it closes.
func (m *Mirror) Run(stop <-chan struct{}, every time.Duration) {
	if err := m.Pull(); err != nil {
		log.Printf("storage: %v (pushes keep S3's extra files until a pull succeeds)", err)
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-stop:
			if err := m.Push(); err != nil {
				log.Printf("storage: %v", err)
			}
			return
		case <-t.C:
			if !m.pulled {
				_ = m.Pull()
			}
			if err := m.Push(); err != nil {
				log.Printf("storage: %v", err)
			}
		}
	}
}
