//go:build linux

package bus

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const cgroupRoot = "/sys/fs/cgroup"

func validateRunPlatform(l runLimits) error {
	if l.MaxTasks < 0 || l.CPUQuota < 0 || math.IsNaN(l.CPUQuota) || math.IsInf(l.CPUQuota, 0) || l.MaxCPUTime < 0 {
		return errors.New("invalid resource limit")
	}
	if l.CPUQuota > 0 && l.CPUQuota < 0.1 {
		return errors.New("CPU quota must be at least 0.1%")
	}
	if l.CPUs != "" {
		wanted, err := parseCPUList(l.CPUs)
		if err != nil {
			return fmt.Errorf("invalid CPU list: %w", err)
		}
		status, err := os.ReadFile("/proc/self/status")
		if err != nil {
			return fmt.Errorf("read allowed CPUs: %w", err)
		}
		var allowed map[int]bool
		for _, line := range strings.Split(string(status), "\n") {
			if strings.HasPrefix(line, "Cpus_allowed_list:") {
				allowed, err = parseCPUList(strings.TrimSpace(strings.TrimPrefix(line, "Cpus_allowed_list:")))
				break
			}
		}
		if err != nil || allowed == nil {
			return fmt.Errorf("read allowed CPUs: %w", err)
		}
		for cpu := range wanted {
			if !allowed[cpu] {
				return fmt.Errorf("CPU %d is not available to this process", cpu)
			}
		}
	}
	if !needsResourceScope(l) {
		return nil
	}
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return errors.New("resource limits require systemd-run --user")
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return errors.New("resource limits require systemctl --user")
	}
	probeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	probe := exec.CommandContext(probeCtx, "systemctl", "--user", "show-environment")
	var probeOutput limitedBuffer
	probe.Stdout, probe.Stderr = &limitedBuffer{}, &probeOutput
	if err := probe.Run(); err != nil {
		return fmt.Errorf("resource limits require a working user systemd manager: %w: %s", err, strings.TrimSpace(probeOutput.String()))
	}
	controllers, err := os.ReadFile(filepath.Join(cgroupRoot, "cgroup.controllers"))
	if err != nil {
		return fmt.Errorf("resource limits require cgroup v2: %w", err)
	}
	available := strings.Fields(string(controllers))
	for _, required := range []struct {
		needed bool
		name   string
	}{
		{l.MaxRAM > 0 || l.TrackMemory, "memory"},
		{l.CPUQuota > 0 || l.MaxCPUTime > 0, "cpu"},
		{l.CPUs != "", "cpuset"},
		{l.MaxTasks > 0, "pids"},
	} {
		if required.needed && !containsString(available, required.name) {
			return fmt.Errorf("resource limit requires unavailable cgroup v2 %s controller", required.name)
		}
	}
	return nil
}

// prepareRunPlatform resolves the optional tree scope before the lease is
// acquired, so its unit records whether a cgroup will really be created.
func prepareRunPlatform(l *runLimits) error {
	if l.TreeScope && !requiresResourceScope(*l) && !optionalUserScopeAvailable() {
		l.TreeScope = false
	}
	return validateRunPlatform(*l)
}

func requiresResourceScope(l runLimits) bool {
	return l.MaxRAM > 0 || l.CPUQuota > 0 || l.CPUs != "" || l.MaxCPUTime > 0 || l.MaxTasks > 0 || l.TrackMemory
}

func needsResourceScope(l runLimits) bool {
	return requiresResourceScope(l) || l.TreeScope
}

func containsString(items []string, item string) bool {
	for _, candidate := range items {
		if candidate == item {
			return true
		}
	}
	return false
}

func parseCPUList(list string) (map[int]bool, error) {
	result := make(map[int]bool)
	for _, part := range strings.Split(list, ",") {
		bounds := strings.Split(part, "-")
		if len(bounds) < 1 || len(bounds) > 2 {
			return nil, fmt.Errorf("bad CPU range %q", part)
		}
		first, err := strconv.Atoi(bounds[0])
		if err != nil || first < 0 {
			return nil, fmt.Errorf("bad CPU number %q", part)
		}
		last := first
		if len(bounds) == 2 {
			last, err = strconv.Atoi(bounds[1])
			if err != nil || last < first {
				return nil, fmt.Errorf("bad CPU range %q", part)
			}
		}
		if last-first > 1_000_000 {
			return nil, fmt.Errorf("CPU range too large %q", part)
		}
		if last > 1_000_000 {
			return nil, fmt.Errorf("CPU number too large %q", part)
		}
		for cpu := first; cpu <= last; cpu++ {
			result[cpu] = true
		}
	}
	return result, nil
}

func executeResourceCommand(ctx context.Context, argv []string, l runLimits, unit string, onCgroup func(string) error) (int, error) {
	if len(argv) == 0 {
		return 0, errors.New("missing command")
	}
	if err := validateRunPlatform(l); err != nil {
		return 0, err
	}
	if !needsResourceScope(l) {
		return executePlainCommand(ctx, argv)
	}
	if unit == "" {
		unit = fmt.Sprintf("agent-bus-run-%d-%d.scope", os.Getpid(), time.Now().UnixNano())
	} else if !strings.HasSuffix(unit, ".scope") {
		unit += ".scope"
	}
	args := []string{"--user", "--scope", "--quiet", "--expand-environment=no", "--unit=" + unit}
	if l.MaxRAM > 0 {
		args = append(args, "-p", fmt.Sprintf("MemoryMax=%d", l.MaxRAM), "-p", "MemorySwapMax=0", "-p", "OOMPolicy=kill")
	}
	if l.CPUQuota > 0 {
		args = append(args, "-p", "CPUQuota="+strconv.FormatFloat(l.CPUQuota, 'f', -1, 64)+"%")
	}
	if l.CPUs != "" {
		args = append(args, "-p", "AllowedCPUs="+l.CPUs)
	}
	if l.MaxTasks > 0 {
		args = append(args, "-p", fmt.Sprintf("TasksMax=%d", l.MaxTasks))
	}
	args = append(args, "--")
	args = append(args, argv...)
	cmd := exec.Command("systemd-run", args...)
	var startupStderr limitedBuffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, io.MultiWriter(os.Stderr, &startupStderr)
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start systemd scope: %w", err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	// systemd-run starts the command before it returns. Find the unit immediately so
	// reservations and CPU accounting attach while the scope is still running.
	var cgroup string
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	discoveryDeadline := time.NewTimer(3 * time.Second)
	defer discoveryDeadline.Stop()
	var discoveryError error
	for {
		path, err := scopeCgroup(unit)
		if err == nil && path != "" {
			cgroup = path
			break
		}
		discoveryError = err
		select {
		case waitErr := <-waitCh:
			cleanupErr := stopScope(unit, cgroup)
			if waitErr != nil && scopeStartupError(startupStderr.String()) {
				return 0, errors.Join(fmt.Errorf("start systemd scope: %s", strings.TrimSpace(startupStderr.String())), cleanupErr)
			}
			code, err := exitResult(waitErr)
			return code, errors.Join(err, cleanupErr)
		case <-ctx.Done():
			return cancelScope(ctx, unit, cgroup, waitCh)
		case <-ticker.C:
		case <-discoveryDeadline.C:
			cleanupErr := stopScope(unit, cgroup)
			<-waitCh
			return 0, errors.Join(fmt.Errorf("could not discover systemd scope %s: %w", unit, discoveryError), cleanupErr)
		}
	}
	if onCgroup != nil {
		if err := onCgroup(cgroup); err != nil {
			cleanupErr := stopScope(unit, cgroup)
			<-waitCh
			return 0, errors.Join(err, cleanupErr)
		}
	}
	var cpuTicker *time.Ticker
	var cpuTicks <-chan time.Time
	if l.MaxCPUTime > 0 {
		cpuTicker = time.NewTicker(100 * time.Millisecond)
		cpuTicks = cpuTicker.C
		defer cpuTicker.Stop()
	}
	for {
		select {
		case waitErr := <-waitCh:
			cleanupErr := stopScope(unit, cgroup)
			code, err := exitResult(waitErr)
			return code, errors.Join(err, cleanupErr)
		case <-ctx.Done():
			return cancelScope(ctx, unit, cgroup, waitCh)
		case <-cpuTicks:
			used, err := cgroupCPUTime(cgroup)
			if err != nil {
				// The unit can vanish between the stat read and the process exit.
				select {
				case waitErr := <-waitCh:
					cleanupErr := stopScope(unit, cgroup)
					code, err := exitResult(waitErr)
					return code, errors.Join(err, cleanupErr)
				default:
				}
				cleanupErr := stopScope(unit, cgroup)
				<-waitCh
				return 0, errors.Join(fmt.Errorf("read scope CPU usage: %w", err), cleanupErr)
			}
			if used >= l.MaxCPUTime {
				cleanupErr := stopScope(unit, cgroup)
				<-waitCh
				return 124, cleanupErr
			}
		}
	}
}

func optionalUserScopeAvailable() bool {
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return false
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(cgroupRoot, "cgroup.controllers")); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, "systemctl", "--user", "show-environment")
	cmd.Stdout, cmd.Stderr = &limitedBuffer{}, &limitedBuffer{}
	return cmd.Run() == nil
}

func scopeStartupError(stderr string) bool {
	return strings.Contains(stderr, "Failed to start transient scope unit") ||
		strings.Contains(stderr, "Unknown assignment:") ||
		strings.Contains(stderr, "Failed to create")
}

func executePlainCommand(ctx context.Context, argv []string) (int, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if _, err := unix.IoctlGetInt(int(os.Stdin.Fd()), unix.TIOCGPGRP); err == nil {
		cmd.SysProcAttr.Foreground = true
		cmd.SysProcAttr.Ctty = 0
	}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	select {
	case err := <-waitCh:
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		return exitResult(err)
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		grace := time.NewTimer(time.Second)
		defer grace.Stop()
		select {
		case <-waitCh:
		case <-grace.C:
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-waitCh
		}
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		return 0, ctx.Err()
	}
}

func exitResult(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal()), nil
		}
		return exit.ExitCode(), nil
	}
	return 0, err
}

func scopeCgroup(unit string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "systemctl", "--user", "show", "--property=ControlGroup", "--value", "--", unit)
	var output limitedBuffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("query scope: %w: %s", err, output.String())
	}
	group := strings.TrimSpace(output.String())
	if group == "" || !strings.HasPrefix(group, "/") {
		return "", errors.New("scope has no control group yet")
	}
	path := filepath.Join(cgroupRoot, group)
	if !strings.HasPrefix(path, cgroupRoot+string(filepath.Separator)) {
		return "", errors.New("invalid scope control group")
	}
	return path, nil
}

func cgroupCPUTime(path string) (time.Duration, error) {
	file, err := os.Open(filepath.Join(path, "cpu.stat"))
	if err != nil {
		return 0, err
	}
	defer file.Close()
	scan := bufio.NewScanner(file)
	for scan.Scan() {
		fields := strings.Fields(scan.Text())
		if len(fields) == 2 && fields[0] == "usage_usec" {
			micros, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				return 0, err
			}
			if micros > math.MaxInt64/uint64(time.Microsecond) {
				return time.Duration(math.MaxInt64), nil
			}
			return time.Duration(micros) * time.Microsecond, nil
		}
	}
	if err := scan.Err(); err != nil {
		return 0, err
	}
	return 0, errors.New("cpu.stat lacks usage_usec")
}

func cancelScope(ctx context.Context, unit, path string, waitCh <-chan error) (int, error) {
	_ = signalScope(unit, "TERM")
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	waited := false
	select {
	case <-waitCh:
		waited = true
	case <-timer.C:
		// A command may ignore TERM. Force the complete unit down below.
	}
	cleanupErr := stopScope(unit, path)
	if !waited {
		<-waitCh
	}
	return 0, errors.Join(ctx.Err(), cleanupErr)
}

func signalScope(unit, signal string) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "systemctl", "--user", "kill", "--kill-whom=all", "--signal="+signal, "--", unit)
	var output limitedBuffer
	cmd.Stdout, cmd.Stderr = &limitedBuffer{}, &output
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("signal scope %s: %w: %s", unit, err, strings.TrimSpace(output.String()))
	}
	return nil
}

func stopScope(unit, path string) error {
	_ = signalScope(unit, "KILL")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "systemctl", "--user", "stop", "--no-block", "--", unit)
	cmd.Stdout, cmd.Stderr = &limitedBuffer{}, &limitedBuffer{}
	_ = cmd.Run()
	deadline := time.Now().Add(3 * time.Second)
	for {
		populated, err := scopePopulated(unit, path)
		if err == nil && !populated {
			return nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("verify stopped scope %s: %w", unit, err)
			}
			return fmt.Errorf("scope %s remains populated after kill", unit)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func scopePopulated(unit, path string) (bool, error) {
	if path == "" {
		return readResourceUnitPopulated(unit)
	}
	populated, err := readResourceCgroupPopulated(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return populated, err
}

type limitedBuffer struct{ data []byte }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := 4096 - len(b.data); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		b.data = append(b.data, p[:room]...)
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string { return string(b.data) }
