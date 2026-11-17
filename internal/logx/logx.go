// Package logx builds a slog logger that masks registered secret strings.
package logx

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
)

const mask = "***"

// New returns a text logger writing to w. Every registered secret that appears
// in a message, attribute key or value, or error string is replaced by "***".
// Empty secrets are ignored.
func New(w io.Writer, secrets []string) *slog.Logger {
	var pairs []string
	for _, s := range secrets {
		if s != "" {
			pairs = append(pairs, s, mask)
		}
	}
	m := &maskingHandler{
		inner:    slog.NewTextHandler(w, nil),
		replacer: strings.NewReplacer(pairs...),
		enabled:  len(pairs) > 0,
	}
	return slog.New(m)
}

type maskingHandler struct {
	inner    slog.Handler
	replacer *strings.Replacer
	enabled  bool
}

func (h *maskingHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *maskingHandler) Handle(ctx context.Context, r slog.Record) error {
	if !h.enabled {
		return h.inner.Handle(ctx, r)
	}
	out := slog.NewRecord(r.Time, r.Level, h.replacer.Replace(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(h.maskAttr(a))
		return true
	})
	return h.inner.Handle(ctx, out)
}

func (h *maskingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	masked := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		masked[i] = h.maskAttr(a)
	}
	return &maskingHandler{inner: h.inner.WithAttrs(masked), replacer: h.replacer, enabled: h.enabled}
}

func (h *maskingHandler) WithGroup(name string) slog.Handler {
	return &maskingHandler{inner: h.inner.WithGroup(h.replacer.Replace(name)), replacer: h.replacer, enabled: h.enabled}
}

func (h *maskingHandler) maskAttr(a slog.Attr) slog.Attr {
	if !h.enabled {
		return a
	}
	a.Value = a.Value.Resolve()
	key := h.replacer.Replace(a.Key)
	switch a.Value.Kind() {
	case slog.KindString:
		return slog.String(key, h.replacer.Replace(a.Value.String()))
	case slog.KindGroup:
		group := a.Value.Group()
		masked := make([]any, len(group))
		for i, g := range group {
			masked[i] = h.maskAttr(g)
		}
		return slog.Group(key, masked...)
	case slog.KindAny:
		return slog.String(key, h.replacer.Replace(fmt.Sprint(a.Value.Any())))
	default:
		return slog.Attr{Key: key, Value: a.Value}
	}
}
