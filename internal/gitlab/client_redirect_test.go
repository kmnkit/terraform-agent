package gitlab

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func clientFor(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c, err := NewClient(srv.URL, tokenFile(t, testToken), 42, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCrossHostRedirectDropsToken(t *testing.T) {
	var storageToken atomic.Value
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		storageToken.Store(r.Header.Get("PRIVATE-TOKEN"))
		fmt.Fprint(w, "artifact")
	}))
	t.Cleanup(storage.Close)
	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("PRIVATE-TOKEN") != testToken {
			t.Errorf("origin must receive token")
		}
		http.Redirect(w, r, storage.URL+"/blob", http.StatusFound)
	}))
	t.Cleanup(gl.Close)
	c := clientFor(t, gl)
	got, err := c.JobArtifact(context.Background(), 1, "plan.json")
	if err != nil || string(got) != "artifact" {
		t.Fatalf("JobArtifact = %q, %v", got, err)
	}
	if v, _ := storageToken.Load().(string); v != "" {
		t.Fatalf("token forwarded to other host: %q", v)
	}
}

func TestSameHostRedirectKeepsToken(t *testing.T) {
	var final atomic.Value
	var gl *httptest.Server
	gl = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/moved") {
			final.Store(r.Header.Get("PRIVATE-TOKEN"))
			fmt.Fprint(w, "ok")
			return
		}
		http.Redirect(w, r, gl.URL+"/moved", http.StatusFound)
	}))
	t.Cleanup(gl.Close)
	c := clientFor(t, gl)
	if _, err := c.JobArtifact(context.Background(), 1, "plan.json"); err != nil {
		t.Fatal(err)
	}
	if v, _ := final.Load().(string); v != testToken {
		t.Fatalf("same-origin redirect lost token: %q", v)
	}
}

func TestHTTPSToHTTPRedirectRefused(t *testing.T) {
	var hit atomic.Bool
	plain := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		hit.Store(true)
	}))
	t.Cleanup(plain.Close)
	gl := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL, http.StatusFound)
	}))
	t.Cleanup(gl.Close)
	c := clientFor(t, gl)
	_, err := c.JobArtifact(context.Background(), 1, "plan.json")
	if err == nil || strings.Contains(err.Error(), testToken) {
		t.Fatalf("err = %v", err)
	}
	if hit.Load() {
		t.Fatal("downgrade target was contacted")
	}
}

func TestTooManyRedirectsRefused(t *testing.T) {
	var calls atomic.Int32
	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Redirect(w, r, r.URL.Path+"x", http.StatusFound)
	}))
	t.Cleanup(gl.Close)
	c := clientFor(t, gl)
	if _, err := c.JobArtifact(context.Background(), 1, "p"); err == nil {
		t.Fatal("redirect loop must fail")
	}
	if calls.Load() > int32(maxRedirects)+1 {
		t.Fatalf("calls = %d", calls.Load())
	}
}

func TestNewClientDoesNotMutateCallerClient(t *testing.T) {
	hc := &http.Client{}
	if _, err := NewClient("http://x", tokenFile(t, testToken), 1, hc); err != nil {
		t.Fatal(err)
	}
	if hc.CheckRedirect != nil {
		t.Fatal("caller's client was mutated")
	}
}

func TestRetryResendsPostBody(t *testing.T) {
	var bodies []string
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if len(bodies) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, `{"id":1}`)
	}))
	if _, err := c.CreateNote(context.Background(), 3, "hello"); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 || bodies[0] != `{"body":"hello"}` || bodies[1] != bodies[0] {
		t.Fatalf("bodies = %q", bodies)
	}
}

func TestContextCancelDuringBackoffReturnsPromptly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)
	c := clientFor(t, srv) // real sleep
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.CurrentUser(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("took %v", time.Since(start))
	}
}
