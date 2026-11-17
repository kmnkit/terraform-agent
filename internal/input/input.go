// Package input validates the only caller-supplied inputs: MR iid, head SHA
// and pipeline ID, plus the Cloud Run single-task guarantee.
package input

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"regexp"
)

const (
	envTaskCount = "CLOUD_RUN_TASK_COUNT"
	envTaskIndex = "CLOUD_RUN_TASK_INDEX"
)

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
	mrIID := fs.Int("mr-iid", 0, "")
	sha := fs.String("sha", "", "")
	pipelineID := fs.Int("pipeline-id", 0, "")
	if err := fs.Parse(args); err != nil {
		return Input{}, fmt.Errorf("input: parse flags: %w", err)
	}
	if fs.NArg() > 0 {
		return Input{}, errors.New("input: positional arguments are not allowed")
	}
	if *mrIID <= 0 {
		return Input{}, errors.New("input: --mr-iid must be a positive integer")
	}
	if !shaPattern.MatchString(*sha) {
		return Input{}, errors.New("input: --sha must be 40 lowercase hex characters")
	}
	if *pipelineID <= 0 {
		return Input{}, errors.New("input: --pipeline-id must be a positive integer")
	}
	return Input{MRIID: *mrIID, SHA: *sha, PipelineID: *pipelineID}, nil
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
