// Package httpx provides HTTP clients that ignore HTTP_PROXY/HTTPS_PROXY.
package httpx

import (
	"net/http"
	"time"
)

// NewClient returns a client with its own transport that never uses env proxies.
func NewClient(timeout time.Duration) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	return &http.Client{Timeout: timeout, Transport: tr}
}

// DisableEnvProxy makes library clients using http.DefaultTransport ignore env proxies.
func DisableEnvProxy() {
	if tr, ok := http.DefaultTransport.(*http.Transport); ok {
		tr.Proxy = nil
	}
}
