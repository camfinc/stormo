package instance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadCore(t *testing.T, core string) (*Instance, error) {
	t.Helper()
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "stormo.yaml"), []byte("name: Acme\nslug: acme\ncore:\n"+core), 0o644)
	return Load(root)
}

func TestCoreListenNeedsTLSOrAPrivateAddress(t *testing.T) {
	for core, want := range map[string]string{
		"  listen: 10.0.0.5\n":                                  "needs core.tls",
		"  listen: 10.0.0.5\n  tls: off\n":                      "",
		"  listen: 100.101.102.103\n  tls: off\n":               "", // Tailscale
		"  listen: 203.0.113.7\n  tls: off\n":                   "only allowed on a private address",
		"  listen: 0.0.0.0\n  tls: off\n":                       "every interface",
		"  listen: 0.0.0.0\n  tls: {cert: c.pem, key: k.pem}\n": "",
		"  listen: core.example\n  tls: off\n":                  "must be an IP address",
		"  tls: {cert: c.pem}\n":                                "both cert and key",
		"  url: core.example:18600\n":                           "http(s) URL",
		"  urls: {aws: ftp://x}\n":                              "core.urls.aws",
	} {
		_, err := loadCore(t, core)
		switch {
		case want == "" && err != nil:
			t.Errorf("%q: %v", core, err)
		case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
			t.Errorf("%q: %v, want %q", core, err, want)
		}
	}
}

func TestCoreURLForPlacements(t *testing.T) {
	i, err := loadCore(t, "  url: https://core.example/\n  urls: {aws: \"https://core.internal:18600\"}\n")
	if err != nil {
		t.Fatal(err)
	}
	if u := i.CoreURLFor("aws"); u != "https://core.internal:18600" {
		t.Errorf("aws: %s", u)
	}
	if u := i.CoreURLFor("local"); u != "https://core.example" {
		t.Errorf("local: %s", u)
	}
}

func TestCoreStorageIsAnS3URI(t *testing.T) {
	if i, err := loadCore(t, "  storage: s3://acme-shared/workdir/\n"); err != nil || i.CoreStorage != "s3://acme-shared/workdir" {
		t.Errorf("%v %+v", err, i)
	}
	if _, err := loadCore(t, "  storage: /mnt/efs\n"); err == nil || !strings.Contains(err.Error(), "s3://bucket/prefix") {
		t.Errorf("a path: %v", err)
	}
}
