package core

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/camfinc/stormo/pkg/instance"
)

// selfSigned writes a certificate for ::1 and returns a pool trusting it.
func selfSigned(t *testing.T, dir string) *x509.CertPool {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "core"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.IPv6loopback}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	os.WriteFile(filepath.Join(dir, "core.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(filepath.Join(dir, "core.key"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return pool
}

func TestPublicListenerServesTLSWithTokens(t *testing.T) {
	probe, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skip("no IPv6 loopback")
	}
	probe.Close()
	_, inst := fixture(t)
	pool := selfSigned(t, inst.Root)
	inst.CoreListen = "::1"
	inst.CoreTLS = instance.CoreTLS{Cert: "core.pem", Key: "core.key"}
	const owner = "owner-token-0123456789"
	c, err := StartCore(CoreOptions{Inst: inst, Port: 0, NoFleet: true, DB: testDB(t), Keys: testKeys(t), OwnerToken: owner, NoMirror: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Public == nil || !c.PublicTLS || c.Public.Port != c.Addr.Port {
		t.Fatalf("public %v tls %v", c.Public, c.PublicTLS)
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	base := fmt.Sprintf("https://[::1]:%d", c.Addr.Port)
	get := func(p, tok string) int {
		t.Helper()
		req, _ := http.NewRequest("GET", base+p, nil)
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		r, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	if code := get("/health", ""); code != 200 {
		t.Errorf("/health over TLS: %d", code)
	}
	if code := get("/api/messages", ""); code != 401 {
		t.Errorf("messages without a token: %d", code)
	}
	if code := get("/api/messages", owner); code != 200 {
		t.Errorf("messages with the owner token: %d", code)
	}
	// Plain HTTP on the TLS port is refused.
	if r, err := http.Get(fmt.Sprintf("http://[::1]:%d/health", c.Addr.Port)); err == nil && r.StatusCode == 200 {
		t.Error("plain HTTP answered on the TLS listener")
	}
	req, _ := http.NewRequest("GET", base+"/api/core", nil)
	req.Header.Set("Authorization", "Bearer "+owner)
	r, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var v coreView
	json.NewDecoder(r.Body).Decode(&v)
	r.Body.Close()
	if !v.TLS || v.Listen == "" {
		t.Errorf("/api/core: %+v", v)
	}
}
