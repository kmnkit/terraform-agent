package plan

import (
	"encoding/json"
	"regexp"
	"strings"
	"unicode/utf8"
)

// ansiPattern matches CSI sequences and OSC sequences (BEL or ST terminated).
var ansiPattern = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)`)

const (
	errorPrefix = "Error: "
	boxTop      = "╷"
	boxOpen     = "│"
	boxClose    = "╵"
)

// ExtractErrorBlocks returns the error blocks of a plan log, at most
// maxBytes in total (cut on a rune boundary, later blocks dropped).
// PROVISIONAL (Phase 0 B): the real HCP plan log format is unverified. If the
// log is a JSON-lines log (any line with an "@level"), only JSON error
// diagnostics are used and text is never parsed. Otherwise a block starts only
// at a line that begins with "Error: " (after an optional box prefix), so
// plan-diff lines and Warning boxes can never open one.
func ExtractErrorBlocks(log string, maxBytes int) []string {
	lines := strings.Split(ansiPattern.ReplaceAllString(log, ""), "\n")
	blocks, isJSONLog := jsonErrorBlocks(lines)
	if !isJSONLog {
		blocks = textErrorBlocks(lines)
	}
	return fitBytes(blocks, maxBytes)
}

func jsonErrorBlocks(lines []string) (blocks []string, isJSONLog bool) {
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if !strings.HasPrefix(l, "{") {
			continue
		}
		var e struct {
			Level      string `json:"@level"`
			Diagnostic struct {
				Summary string `json:"summary"`
				Detail  string `json:"detail"`
			} `json:"diagnostic"`
		}
		if json.Unmarshal([]byte(l), &e) != nil || e.Level == "" {
			continue
		}
		isJSONLog = true
		if block := strings.TrimSpace(e.Diagnostic.Summary + "\n" + e.Diagnostic.Detail); e.Level == "error" && block != "" {
			blocks = append(blocks, block)
		}
	}
	return blocks, isJSONLog
}

// textErrorBlocks scans text lines. Boxed blocks end at "╵"; unboxed blocks
// end at a blank line. Any other box (for example Warning) is skipped whole.
func textErrorBlocks(lines []string) []string {
	var blocks, cur []string
	boxed, skipping := false, false
	for _, l := range lines {
		switch {
		case len(cur) > 0:
			if (boxed && strings.HasPrefix(l, boxClose)) || (!boxed && strings.TrimSpace(l) == "") {
				blocks = append(blocks, strings.TrimSpace(strings.Join(cur, "\n")))
				cur = nil
				continue
			}
			cur = append(cur, unbox(l))
		case skipping:
			skipping = !strings.HasPrefix(l, boxClose)
		case strings.HasPrefix(unbox(l), errorPrefix):
			boxed = strings.HasPrefix(l, boxOpen)
			cur = []string{unbox(l)}
		case strings.HasPrefix(l, boxOpen):
			skipping = true
		}
	}
	if len(cur) > 0 {
		blocks = append(blocks, strings.TrimSpace(strings.Join(cur, "\n")))
	}
	return blocks
}

// unbox drops the "│ " box prefix and trims trailing space.
func unbox(l string) string {
	if rest, ok := strings.CutPrefix(l, boxOpen); ok {
		l = strings.TrimPrefix(rest, " ")
	}
	return strings.TrimRight(l, " \t\r")
}

func fitBytes(blocks []string, maxBytes int) []string {
	var out []string
	remaining := maxBytes
	for _, b := range blocks {
		if remaining <= 0 {
			break
		}
		if len(b) > remaining {
			b = cutRunes(b, remaining)
		}
		if b == "" {
			break
		}
		out = append(out, b)
		remaining -= len(b)
	}
	return out
}

func cutRunes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
