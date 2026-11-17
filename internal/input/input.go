// Package input validates the only caller-supplied inputs: MR iid, head SHA
// and pipeline ID, plus the Cloud Run single-task guarantee.
package input

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"regexp"
	"strconv"
)

const (
	envTaskCount = "CLOUD_RUN_TASK_COUNT"
	envTaskIndex = "CLOUD_RUN_TASK_INDEX"
)

var decimalPattern = regexp.MustCompile(`^[1-9][0-9]*$`)

var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Input is the validated, immutable set of job inputs.
type Input struct {
	MRIID      int
	SHA        string
	PipelineID int
}

// Parse validates args and the Cloud Run task env. getenv is only called for
// CLOUD_RUN_TASK_COUNT and CLOUD_RUN_TASK_INDEX.
func Parse(args []string, getenv func(string) string) (Input, error) {
	if err := checkTask(getenv); err != nil {
		return Input{}, err
	}
	fs := flag.NewFlagSet("job", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	mrIIDRaw := fs.String("mr-iid", "", "")
	sha := fs.String("sha", "", "")
	pipelineIDRaw := fs.String("pipeline-id", "", "")
	if err := fs.Parse(args); err != nil {
		return Input{}, fmt.Errorf("input: parse flags: %w", err)
	}
	if fs.NArg() > 0 {
		return Input{}, errors.New("input: positional arguments are not allowed")
	}
	mrIID, err := parseDecimal("mr-iid", *mrIIDRaw)
	if err != nil {
		return Input{}, err
	}
	if !shaPattern.MatchString(*sha) {
		return Input{}, errors.New("input: --sha must be 40 lowercase hex characters")
	}
	pipelineID, err := parseDecimal("pipeline-id", *pipelineIDRaw)
	if err != nil {
		return Input{}, err
	}
	return Input{MRIID: mrIID, SHA: *sha, PipelineID: pipelineID}, nil
}

// parseDecimal accepts only plain base-10 positive integers (no prefixes,
// underscores, signs or leading zeros).
func parseDecimal(name, raw string) (int, error) {
	if !decimalPattern.MatchString(raw) {
		return 0, fmt.Errorf("input: --%s must be a positive decimal integer, got %q", name, raw)
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("input: --%s out of range: %w", name, err)
	}
	return n, nil
}

// checkTask fails closed: Cloud Run always sets both variables.
func checkTask(getenv func(string) string) error {
	if v := getenv(envTaskCount); v != "1" {
		return fmt.Errorf("input: %s must be \"1\", got %q", envTaskCount, v)
	}
	if v := getenv(envTaskIndex); v != "0" {
		return fmt.Errorf("input: %s must be \"0\", got %q", envTaskIndex, v)
	}
	return nil
}
