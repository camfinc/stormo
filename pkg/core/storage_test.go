package core

import (
	"strings"
	"testing"

	"github.com/camfinc/stormo/pkg/aws"
)

func TestMirrorDeletesInS3OnlyAfterAPull(t *testing.T) {
	var calls []string
	fail := true
	m := &Mirror{Dir: "/w", URI: "s3://acme/shared", Region: "eu-west-1", CLI: func(args []string, _ *string) aws.Result {
		calls = append(calls, strings.Join(args, " "))
		if fail && args[2] == "s3://acme/shared" {
			return aws.Result{Code: 1, Stderr: "AccessDenied"}
		}
		return aws.Result{}
	}}
	if err := m.Pull(); err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("pull: %v", err)
	}
	_ = m.Push()
	if strings.Contains(calls[1], "--delete") {
		t.Errorf("push before a good pull deletes: %s", calls[1])
	}
	fail = false
	_ = m.Pull()
	_ = m.Push()
	if !strings.HasPrefix(calls[2], "s3 sync s3://acme/shared /w") || !strings.Contains(calls[3], "s3 sync /w s3://acme/shared --delete") || !strings.Contains(calls[3], "--region eu-west-1") {
		t.Errorf("calls: %q", calls)
	}
}
