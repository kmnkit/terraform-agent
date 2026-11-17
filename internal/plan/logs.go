package plan

import (
	"encoding/json"
	"regexp"
	"strings"
	"unicode/utf8"
)

var ansiPattern = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

const (
	errorMarker = "Error: "
	boxOpen     = "│"
	boxClose    = "╵"
)

// ExtractErrorBlocks returns the error blocks of a plan log, at most
// maxBytes in total (cut on a rune boundary, later blocks dropped).
// PROVISIONAL (Phase 0 B): the real HCP plan log format is unverified. JSON
// lines with @level "error" are used when present, otherwise boxed or
// blank-line-terminated text blocks.
func ExtractErrorBlocks(log string, maxBytes int) []string {
	lines := strings.Split(ansiPattern.ReplaceAllString(log, ""), "\n")
	blocks := jsonErrorBlocks(lines)
	if len(blocks) == 0 {
		blocks = textErrorBlocks(lines)
	}
	return fitBytes(blocks, maxBytes)
}

func jsonErrorBlocks(lines []string) []string {
	var blocks []string
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
		if json.Unmarshal([]byte(l), &e) != nil || e.Level != "error" {
			continue
		}
		block := strings.TrimSpace(e.Diagnostic.Summary + "\n" + e.Diagnostic.Detail)
		if block != "" {
			blocks = append(blocks, block)
		}
	}
	return blocks
}

func textErrorBlocks(lines []string) []string {
	var blocks, cur []string
	boxed := false
	flush := func() {
		if len(cur) > 0 {
			blocks = append(blocks, strings.TrimSpace(strings.Join(cur, "\n")))
		}
		cur, boxed = nil, false
	}
	for _, l := range lines {
		switch {
		case len(cur) == 0:
			if strings.Contains(l, errorMarker) {
				boxed = strings.HasPrefix(l, boxOpen)
				cur = append(cur, unbox(l))
			}
		case boxed && strings.HasPrefix(l, boxClose), !boxed && strings.TrimSpace(l) == "":
			flush()
		default:
			cur = append(cur, unbox(l))
		}
	}
	flush()
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
