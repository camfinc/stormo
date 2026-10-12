package engines

import (
	"testing"

	"github.com/camfinc/stormo/pkg/instance"
)

func TestNeedletailResolves(t *testing.T) {
	e, err := Get("needletail", &instance.Instance{})
	if err != nil || e.Kind() != "needletail" {
		t.Fatalf("Get: %v, %v", e, err)
	}
	if _, err := Runtime("needletail"); err != nil {
		t.Fatal(err)
	}
	if _, err := Get("nope", &instance.Instance{}); err == nil {
		t.Error("unknown engine resolved")
	}
}
