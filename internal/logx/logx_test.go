package logx

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestLogger_MasksRegisteredSecrets(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, []string{"tok-abc123", "", "other-secret"})

	log.Info("calling with tok-abc123 now",
		"header", "Bearer tok-abc123",
		"err", errors.New("failed: other-secret rejected"),
		slog.Group("g", slog.String("k", "x-tok-abc123-y")),
		"n", 42,
	)
	log.With("pre", "tok-abc123").WithGroup("grp").Warn("second", "v", "other-secret")

	out := buf.String()
	for _, s := range []string{"tok-abc123", "other-secret"} {
		if strings.Contains(out, s) {
			t.Errorf("secret %q leaked:\n%s", s, out)
		}
	}
	if !strings.Contains(out, "calling with") || !strings.Contains(out, "n=42") {
		t.Errorf("non-secret content lost:\n%s", out)
	}
	if !strings.Contains(out, "***") {
		t.Errorf("expected mask marker:\n%s", out)
	}
}

func TestLogger_NoSecretsIsPassthrough(t *testing.T) {
	var buf bytes.Buffer
	New(&buf, nil).Info("hello", "k", "v")
	if !strings.Contains(buf.String(), "hello") || !strings.Contains(buf.String(), "k=v") {
		t.Errorf("got %s", buf.String())
	}
}
