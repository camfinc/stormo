package bridge

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/manifest"
)

type doer func(*http.Request) (*http.Response, error)

func (d doer) Do(r *http.Request) (*http.Response, error) { return d(r) }

func setup(t *testing.T) (*instance.Instance, *manifest.Agent, []*Action) {
	t.Helper()
	inst, err := instance.Load("../../examples/minimal")
	if err != nil {
		t.Fatal(err)
	}
	inst.BridgeActions = "bridge/actions.yaml"
	reg, err := Load(inst)
	if err != nil {
		t.Fatal(err)
	}
	a, err := manifest.Load(inst.Root, "atlas", inst.Names.Secret)
	if err != nil {
		t.Fatal(err)
	}
	return inst, a, reg
}

func TestActionsAreUnitScopedAndRecheckedAtCallTime(t *testing.T) {
	_, a, reg := setup(t)
	tools, err := MCPTools(a, reg)
	if err != nil || len(tools) != 1 || tools[0].Name != "sales_crm_deals_list" {
		t.Fatalf("%v %v", tools, err)
	}
	outsider := *a
	outsider.ID, outsider.Unit = "nova", "support"
	if _, err := ActionsFor(&outsider, reg); err == nil || !strings.Contains(err.Error(), "belongs to unit sales") {
		t.Fatal(err)
	}
	var url string
	d := doer(func(r *http.Request) (*http.Response, error) {
		url = r.URL.String()
		if r.Header.Get("Authorization") != "Bearer t0k" {
			t.Fatalf("auth %q", r.Header.Get("Authorization"))
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("[]"))}, nil
	})
	if _, err := Invoke(a, reg, "sales.crm.deals.list", map[string]any{"search": "", "dateFrom": "2026-10-06"}, Context{Env: map[string]string{"CRM_API_TOKEN": "t0k"}, Doer: d}); err != nil {
		t.Fatal(err)
	}
	if url != "https://crm.example.com/api/deals?dateFrom=2026-10-06" {
		t.Fatal(url)
	}
	if _, err := Invoke(a, reg, "sales.crm.deals.list", nil, Context{Doer: d}); err == nil || !strings.Contains(err.Error(), "CRM_API_TOKEN is not configured on the bridge") {
		t.Fatal(err)
	}
	if _, err := Invoke(&outsider, nil, "sales.crm.deals.list", nil, Context{}); err == nil {
		t.Fatal("outsider invoked")
	}
}

func TestQueryFollowsSchemaOrder(t *testing.T) {
	_, a, reg := setup(t)
	var url string
	d := doer(func(r *http.Request) (*http.Response, error) {
		url = r.URL.RawQuery
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})
	if _, err := Invoke(a, reg, "sales.crm.deals.list", map[string]any{"search": "a b", "dateTo": "z", "dateFrom": "y"}, Context{Env: map[string]string{"CRM_API_TOKEN": "x"}, Doer: d}); err != nil {
		t.Fatal(err)
	}
	if url != "dateFrom=y&dateTo=z&search=a+b" {
		t.Fatal(url)
	}
}

func TestActionsMustBeYAML(t *testing.T) {
	inst, err := instance.Load("../../examples/minimal")
	if err != nil {
		t.Fatal(err)
	}
	inst.BridgeActions = "bridge/actions.json"
	if _, err := Load(inst); err == nil || !strings.Contains(err.Error(), "bridge.actions must name a YAML file, got bridge/actions.json") {
		t.Fatal(err)
	}
	inst.BridgeActions = ""
	if reg, err := Load(inst); err != nil || len(reg) != 0 {
		t.Fatal(reg, err)
	}
}
