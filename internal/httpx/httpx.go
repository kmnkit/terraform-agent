// Package httpx provides HTTP clients that ignore HTTP_PROXY/HTTPS_PROXY and
// speak HTTP/1.1 only.
package httpx

import (
	"crypto/tls"
	"net/http"
	"time"
)

// NewClient returns a client with its own transport that never uses env
// proxies or HTTP/2.
func NewClient(timeout time.Duration) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	harden(tr)
	return &http.Client{Timeout: timeout, Transport: tr}
}

// DisableEnvProxy makes library clients using http.DefaultTransport ignore env
// proxies and HTTP/2. Call it before any request uses the default transport.
func DisableEnvProxy() {
	if tr, ok := http.DefaultTransport.(*http.Transport); ok {
		harden(tr)
	}
}

// harden drops env proxies and HTTP/2. HTTP/2 is off because its transport
// logs every request header (tokens included) through the standard logger
// when GODEBUG=http2debug is set; a non-nil empty TLSNextProto keeps net/http
// from enabling it.
func harden(tr *http.Transport) {
	tr.Proxy = nil
	tr.ForceAttemptHTTP2 = false
	tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
}
