package machineagent

import (
	"bytes"
	"fmt"
	"os/exec"
)

type CmdResult struct {
	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
	ExitCode int    `json:"exit_code"`
}

func runCmd(name string, args ...string) (*CmdResult, error) {
	cmd := exec.Command(name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	result := &CmdResult{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		result.ExitCode = exitErr.ExitCode()
	} else if err != nil {
		result.ExitCode = -1
	}
	return result, err
}

func cmdString(name string, args ...string) string {
	s := name
	for _, a := range args {
		s += " " + a
	}
	return s
}

func cmdError(msg, command string, result *CmdResult) error {
	return fmt.Errorf("%s: command=%q exit_code=%d stderr=%q", msg, command, result.ExitCode, result.Stderr)
}
