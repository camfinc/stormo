package core

import (
	"fmt"
	"net"
	"net/http"
	"slices"
	"testing"
)

func TestBridgeHosts(t *testing.T) {
	bridge := func() string { return "172.17.0.1" }
	none := func() string { return "" }
	cases := []struct {
		name, env, goos string
		iface           func() string
		want            []string
	}{
		{"linux defaults to docker0", "", "linux", bridge, []string{"172.17.0.1"}},
		{"linux without docker0", "", "linux", none, nil},
		{"macOS forwards host.docker.internal to loopback", "", "darwin", bridge, nil},
		{"explicit list, loopback dropped", " 10.0.0.5, 127.0.0.1,localhost,::1,,fd00::1 ", "darwin", none, []string{"10.0.0.5", "fd00::1"}},
		{"none turns it off", "none", "linux", bridge, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := bridgeHosts(c.env, c.goos, c.iface); !slices.Equal(got, c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestBridgeListenerServesOnlyTheGateway(t *testing.T) {
	// ::1 stands in for the docker0 address: a second address next to 127.0.0.1.
	probe, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skip("no IPv6 loopback")
	}
	probe.Close()
	_, inst := fixture(t)
	c, err := StartCore(CoreOptions{Inst: inst, Port: 0, NoFleet: true, Bridge: []string{"::1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if len(c.Bridges) != 1 || c.Bridges[0].Port != c.Addr.Port {
		t.Fatalf("bridges %v, loopback %v: want one on the same port", c.Bridges, c.Addr)
	}
	status := func(base, p string) int {
		t.Helper()
		r, err := http.Get(base + p)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	bridge := fmt.Sprintf("http://[::1]:%d", c.Addr.Port)
	loop := fmt.Sprintf("http://127.0.0.1:%d", c.Addr.Port)
	for p, want := range map[string]int{"/health": 200, "/v1/models": 401, "/": 404, "/api/fleet": 404, "/api/gateway": 404} {
		if got := status(bridge, p); got != want {
			t.Errorf("bridge %s: %d, want %d", p, got, want)
		}
	}
	if got := status(loop, "/"); got != 200 {
		t.Errorf("loopback /: %d, want 200 (the office stays on loopback)", got)
	}
}

func TestBridgeListenerFailureFreesThePort(t *testing.T) {
	_, inst := fixture(t)
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := free.Addr().(*net.TCPAddr).Port
	free.Close()
	if _, err := StartCore(CoreOptions{Inst: inst, Port: port, NoFleet: true, Bridge: []string{"192.0.2.1"}}); err == nil {
		t.Fatal("listening on an address this host lacks should fail")
	}
	again, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("loopback port still held after a failed start: %v", err)
	}
	again.Close()
}
