package sandbox

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Interrupted commands still have useful untrusted diagnostics. Read them
// before Run's deferred confirmed removal, without reusing its cancelled ctx.
// Cancellation never becomes a successful result, even if a signal handler
// exits zero. A stop/log failure remains an error rather than an implied ACK.
func (m *Manager) collectInterruptedRun(name string, cause error, timedOut bool) (RunResult, error) {
	result := RunResult{ExitCode: 130, TimedOut: timedOut}
	if timedOut {
		result.ExitCode = 124
		cause = nil
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, stopErr := m.dockerOutput(stopCtx, "stop", "--time", "1", name)
	stopCancel()
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 5*time.Second)
	out, waitErr := m.dockerOutput(waitCtx, "wait", name)
	waitCancel()
	if waitErr == nil {
		code, err := parseExitCode(out)
		waitErr = err
		if !timedOut && err == nil && code != 0 {
			result.ExitCode = code
		}
	}
	logCtx, logCancel := context.WithTimeout(context.Background(), 5*time.Second)
	stdout, logErr, stderr := m.dockerOutputStreams(logCtx, "logs", name)
	logCancel()
	result.Stdout = string(stdout)
	result.Stderr = string(stderr)
	if stopErr != nil {
		stopErr = fmt.Errorf("stop interrupted worker: %w", stopErr)
	}
	if waitErr != nil {
		waitErr = fmt.Errorf("confirm interrupted worker exit: %w", waitErr)
	}
	if logErr != nil {
		logErr = fmt.Errorf("collect interrupted worker output: %w", logErr)
	}
	return result, errors.Join(cause, stopErr, waitErr, logErr)
}
