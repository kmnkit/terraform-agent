package plan

import (
	"strings"
	"testing"
	"unicode/utf8"
)

const boxedLog = "Terraform v1.9.0\n" +
	"\x1b[31m╷\x1b[0m\n" +
	"\x1b[31m│\x1b[0m \x1b[1mError: \x1b[0mInvalid value\n" +
	"│\n" +
	"│   on main.tf line 3:\n" +
	"╵\n" +
	"some noise\n" +
	"╷\n" +
	"│ Error: Second problem\n" +
	"│ detail two\n" +
	"╵\n"

func TestExtractErrorBlocksBoxed(t *testing.T) {
	got := ExtractErrorBlocks(boxedLog, 10000)
	if len(got) != 2 {
		t.Fatalf("blocks = %q", got)
	}
	if !strings.HasPrefix(got[0], "Error: Invalid value") || !strings.Contains(got[0], "on main.tf line 3:") {
		t.Errorf("block0 = %q", got[0])
	}
	if strings.Contains(got[0], "\x1b") || strings.Contains(got[0], "│") || strings.Contains(got[0], "some noise") {
		t.Errorf("block0 not clean: %q", got[0])
	}
	if !strings.HasPrefix(got[1], "Error: Second problem") || !strings.Contains(got[1], "detail two") {
		t.Errorf("block1 = %q", got[1])
	}
}

func TestExtractErrorBlocksUnboxedEndsAtBlankLine(t *testing.T) {
	got := ExtractErrorBlocks("ok\nError: boom\n  more\n\ntrailing\n", 1000)
	if len(got) != 1 || got[0] != "Error: boom\n  more" {
		t.Fatalf("blocks = %q", got)
	}
}

func TestExtractErrorBlocksByteCap(t *testing.T) {
	got := ExtractErrorBlocks(boxedLog, 25)
	total := 0
	for _, b := range got {
		total += len(b)
		if !utf8.ValidString(b) {
			t.Errorf("invalid utf8: %q", b)
		}
	}
	if total > 25 || len(got) == 0 {
		t.Fatalf("total=%d blocks=%q", total, got)
	}
}

func TestExtractErrorBlocksRuneBoundary(t *testing.T) {
	got := ExtractErrorBlocks("Error: 가나다라마바사\n", 11)
	if len(got) != 1 || !utf8.ValidString(got[0]) || len(got[0]) > 11 {
		t.Fatalf("blocks = %q", got)
	}
}

func TestExtractErrorBlocksNone(t *testing.T) {
	if got := ExtractErrorBlocks("all fine\nPlan: 1 to add\n", 100); len(got) != 0 {
		t.Fatalf("blocks = %q", got)
	}
	if got := ExtractErrorBlocks(boxedLog, 0); len(got) != 0 {
		t.Fatalf("blocks = %q", got)
	}
}

func TestExtractErrorBlocksIgnoresMidLineMarker(t *testing.T) {
	log := "  # google_x.y will be created\n" +
		"  + resource \"google_x\" \"y\" {\n" +
		"      + documentation = \"Error: high latency\"\n" +
		"      + password      = \"SUPERSECRET-VALUE\"\n" +
		"    }\n\n" +
		"Plan: 1 to add\n"
	if got := ExtractErrorBlocks(log, 1000); len(got) != 0 {
		t.Fatalf("blocks = %q", got)
	}
}

func TestExtractErrorBlocksIgnoresWarningBox(t *testing.T) {
	log := "╷\n" +
		"│ Warning: Deprecated argument\n" +
		"│ \n" +
		"│ Error: this is only quoted detail\n" +
		"│ SUPERSECRET-VALUE\n" +
		"╵\n" +
		"╷\n" +
		"│ Error: Real\n" +
		"│ real detail\n" +
		"╵\n"
	got := ExtractErrorBlocks(log, 1000)
	if len(got) != 1 || !strings.HasPrefix(got[0], "Error: Real") || strings.Contains(got[0], "SUPERSECRET") {
		t.Fatalf("blocks = %q", got)
	}
}

func TestExtractErrorBlocksJSONLogNeverFallsBackToText(t *testing.T) {
	log := `{"@level":"info","@message":"plan"}` + "\n" +
		"Error: plain text inside json log\nSUPERSECRET-VALUE\n\n"
	if got := ExtractErrorBlocks(log, 1000); len(got) != 0 {
		t.Fatalf("blocks = %q", got)
	}
}

func TestExtractErrorBlocksStripsOSC(t *testing.T) {
	log := "\x1b]8;;http://evil\x07Error: linked\x1b]8;;\x07 text\n\n"
	got := ExtractErrorBlocks(log, 1000)
	if len(got) != 1 || got[0] != "Error: linked text" {
		t.Fatalf("blocks = %q", got)
	}
}

func TestExtractErrorBlocksJSONLines(t *testing.T) {
	log := `{"@level":"info","@message":"Terraform 1.9"}` + "\n" +
		`{"@level":"error","@message":"Error: x","diagnostic":{"severity":"error","summary":"Bad thing","detail":"because"}}` + "\n"
	got := ExtractErrorBlocks(log, 1000)
	if len(got) != 1 || got[0] != "Bad thing\nbecause" {
		t.Fatalf("blocks = %q", got)
	}
}
