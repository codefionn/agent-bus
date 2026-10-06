//go:build !linux

package bus

import (
	"context"
	"errors"
	"os"
	"os/exec"
)

func validateRunPlatform(l runLimits) error {
	if l.CPUs != "" || l.MaxRAM > 0 || l.CPUQuota > 0 || l.MaxCPUTime > 0 || l.MaxTasks > 0 || l.TrackMemory {
		return errors.New("resource limits require Linux with user systemd and cgroup v2")
	}
	return nil
}

func prepareRunPlatform(l *runLimits) error {
	l.TreeScope = false
	return validateRunPlatform(*l)
}

func needsResourceScope(l runLimits) bool { return false }

func executeResourceCommand(ctx context.Context, argv []string, l runLimits, _ string, _ func(string) error) (int, error) {
	if err := validateRunPlatform(l); err != nil {
		return 0, err
	}
	if len(argv) == 0 {
		return 0, errors.New("missing command")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode(), nil
		}
		return 0, err
	}
	return 0, nil
}
