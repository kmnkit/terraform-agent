package staticcheck

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/terraform-agent/internal/model"
)

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	return b
}

func assertResults(t *testing.T, got, want []model.CheckResult) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}
