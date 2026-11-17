package httpx

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// fakeHost is a non-loopback name: Go's ProxyFromEnvironment bypasses the
// proxy for localhost/loopback targets, which would make the test vacuous.
const fakeHost = "target.example.test"

func redirectingDial(addr string) func(context.Context, string, string) (net.Conn, error) {
	var d net.Dialer
	return func(ctx context.Context, network, _ string) (net.Conn, error) {
		return d.DialContext(ctx, network, addr)
	}
}

func get(t *testing.T, c *http.Client, url string) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	_ = resp.Body.Close()
}

// One proxy and one env setup for all subtests: ProxyFromEnvironment caches
// the environment on first use, so the env must not change between subtests.
func TestEnvProxyIgnored(t *testing.T) {
	var hits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer proxy.Close()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		t.Setenv(k, proxy.URL)
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	addr := target.Listener.Addr().String()
	_, port, _ := net.SplitHostPort(addr)
	url := "http://" + fakeHost + ":" + port + "/"

	t.Run("control uses proxy", func(t *testing.T) {
		before := hits.Load()
		c := &http.Client{Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment, DialContext: redirectingDial(proxy.Listener.Addr().String()),
		}}
		get(t, c, url)
		if hits.Load() == before {
			t.Fatal("control request did not hit the proxy; test would be vacuous")
		}
	})
	t.Run("NewClient bypasses proxy", func(t *testing.T) {
		before := hits.Load()
		c := NewClient(5 * time.Second)
		c.Transport.(*http.Transport).DialContext = redirectingDial(addr)
		get(t, c, url)
		if got := hits.Load() - before; got != 0 {
			t.Fatalf("proxy hit %d times", got)
		}
	})
	t.Run("DisableEnvProxy affects default transport", func(t *testing.T) {
		orig := http.DefaultTransport
		clone := orig.(*http.Transport).Clone()
		clone.DialContext = redirectingDial(addr)
		http.DefaultTransport = clone
		defer func() { http.DefaultTransport = orig }()
		before := hits.Load()
		DisableEnvProxy()
		get(t, http.DefaultClient, url)
		if got := hits.Load() - before; got != 0 {
			t.Fatalf("proxy hit %d times", got)
		}
	})
}

func TestNewClient_Timeout(t *testing.T) {
	if got := NewClient(3 * time.Second).Timeout; got != 3*time.Second {
		t.Fatalf("timeout = %v", got)
	}
}

func TestNewClient_OwnTransport(t *testing.T) {
	tr := NewClient(time.Second).Transport.(*http.Transport)
	if tr == http.DefaultTransport.(*http.Transport) {
		t.Fatal("NewClient must not share the default transport")
	}
	if tr.Proxy != nil {
		t.Fatal("Proxy must be nil")
	}
}
