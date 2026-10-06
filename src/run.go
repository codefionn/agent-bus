package bus

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// runLimits are enforced by the process backend, separately from admission.
type runLimits struct {
	CPUs        string
	MaxRAM      uint64
	CPUQuota    float64
	MaxCPUTime  time.Duration
	MaxTasks    int
	TrackMemory bool
	TreeScope   bool
}

type runOptions struct {
	request     resourceRequest
	limits      runLimits
	waitTimeout time.Duration
	argv        []string
}

const runUsage = `agent-bus run [OPTIONS] -- COMMAND [ARG...]

Wait for capacity, then run a command and release its reservations on exit.
Wrap a whole agent session or an individual build/test command.

  --mutex NAME           named concurrency group (default capacity 1)
  --max-processes N      maximum concurrent commands in the group;
                         without --mutex, uses the global group
  --cpus LIST            CPU IDs/ranges, such as 0,2-4 (Linux)
  --max-ram SIZE         hard memory limit for the command tree (Linux)
  --reserve-ram SIZE     memory admission request; defaults to --max-ram
  --keep-free-ram SIZE   leave this much RAM available (default 1GiB)
  --cpu-quota PERCENT    sustained CPU limit; 100% is one CPU (Linux)
  --max-cpu-time TIME    total CPU time across the command tree (Linux)
  --max-tasks N          limit OS tasks, including threads (Linux)
  --wait-timeout TIME    maximum queue wait; default waits indefinitely

Sizes accept bytes or K/M/G/T, KiB/MiB/GiB/TiB, or KB/MB/GB/TB.
K/M/G/T and *iB are binary; *B are decimal. Times accept Go durations
such as 30s or 20m, or a number of seconds. Waiting consumes no CPU-time
budget. Limits affect wrapped commands, not already-running sessions.
`

func parseByteSize(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	i := 0
	for i < len(s) && ((s[i] >= '0' && s[i] <= '9') || s[i] == '.') {
		i++
	}
	if i == 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	n, err := strconv.ParseFloat(s[:i], 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	multipliers := map[string]float64{
		"": 1, "B": 1,
		"K": 1 << 10, "KIB": 1 << 10, "KB": 1e3,
		"M": 1 << 20, "MIB": 1 << 20, "MB": 1e6,
		"G": 1 << 30, "GIB": 1 << 30, "GB": 1e9,
		"T": 1 << 40, "TIB": 1 << 40, "TB": 1e12,
	}
	m, ok := multipliers[strings.ToUpper(strings.TrimSpace(s[i:]))]
	v := n * m
	if !ok || v >= math.Exp2(64) || v != math.Trunc(v) {
		return 0, fmt.Errorf("invalid or overflowing size %q", s)
	}
	return uint64(v), nil
}

func parseRunDuration(s string) (time.Duration, error) {
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		v := n * float64(time.Second)
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v >= math.Exp2(63) {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		return time.Duration(v), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	return d, nil
}

func parseRunOptions(args []string, out io.Writer) (runOptions, error) {
	var opts runOptions
	var maxRAM, reserveRAM, keepFree, quota, cpuTime, waitTime string
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.Usage = func() { fmt.Fprint(out, runUsage) }
	fs.StringVar(&opts.request.Mutex, "mutex", "", "named concurrency group")
	fs.IntVar(&opts.request.MaxProcesses, "max-processes", 0, "concurrent commands per group")
	fs.StringVar(&opts.limits.CPUs, "cpus", "", "CPU IDs and ranges")
	fs.StringVar(&maxRAM, "max-ram", "", "hard command-tree memory limit")
	fs.StringVar(&reserveRAM, "reserve-ram", "", "memory admission request")
	fs.StringVar(&reserveRAM, "reserved-ram", "", "alias for --reserve-ram")
	fs.StringVar(&keepFree, "keep-free-ram", "1GiB", "system RAM headroom")
	fs.StringVar(&quota, "cpu-quota", "", "CPU quota percentage")
	fs.StringVar(&cpuTime, "max-cpu-time", "", "aggregate CPU time")
	fs.IntVar(&opts.limits.MaxTasks, "max-tasks", 0, "OS task limit")
	fs.StringVar(&waitTime, "wait-timeout", "0", "admission wait timeout")
	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	opts.argv = fs.Args()
	if len(opts.argv) == 0 {
		return opts, errors.New("run requires a command after --")
	}
	if opts.request.MaxProcesses < 0 || opts.limits.MaxTasks < 0 {
		return opts, errors.New("process and task limits must be positive")
	}
	provided := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { provided[f.Name] = true })
	if provided["max-processes"] && opts.request.MaxProcesses == 0 || provided["max-tasks"] && opts.limits.MaxTasks == 0 {
		return opts, errors.New("process and task limits must be positive")
	}
	if opts.request.Mutex != "" && opts.request.MaxProcesses == 0 {
		opts.request.MaxProcesses = 1
	}
	opts.limits.TreeScope = opts.request.MaxProcesses > 0
	for _, size := range []struct {
		name string
		text string
		dest *uint64
	}{
		{"max-ram", maxRAM, &opts.limits.MaxRAM},
		{"reserve-ram", reserveRAM, &opts.request.ReserveRAM},
		{"keep-free-ram", keepFree, &opts.request.KeepFreeRAM},
	} {
		if size.text == "" {
			continue
		}
		v, err := parseByteSize(size.text)
		if err != nil {
			return opts, fmt.Errorf("--%s: %w", size.name, err)
		}
		if size.name == "max-ram" && v == 0 {
			return opts, errors.New("--max-ram must be greater than zero")
		}
		*size.dest = v
	}
	if reserveRAM == "" {
		opts.request.ReserveRAM = opts.limits.MaxRAM
	}
	if opts.request.ReserveRAM == 0 && !provided["keep-free-ram"] {
		opts.request.KeepFreeRAM = 0
	}
	opts.limits.TrackMemory = opts.request.ReserveRAM > 0
	if quota != "" {
		v, err := strconv.ParseFloat(strings.TrimSuffix(quota, "%"), 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
			return opts, errors.New("--cpu-quota must be a positive percentage")
		}
		opts.limits.CPUQuota = v
	}
	var err error
	if opts.waitTimeout, err = parseRunDuration(waitTime); err != nil {
		return opts, fmt.Errorf("--wait-timeout: %w", err)
	}
	if cpuTime != "" {
		if opts.limits.MaxCPUTime, err = parseRunDuration(cpuTime); err != nil || opts.limits.MaxCPUTime == 0 {
			return opts, errors.New("--max-cpu-time must be a positive duration")
		}
	}
	return opts, nil
}

func runManagedCommand(ctx context.Context, opts runOptions) (code int, runErr error) {
	record := newRunRecord(opts)
	_ = saveRun(record)
	defer func() {
		record.Finished = now()
		record.ExitCode = &code
		record.Status = "completed"
		if code != 0 || runErr != nil {
			record.Status = "failed"
		}
		if ctx.Err() != nil || errors.Is(runErr, context.Canceled) {
			record.Status = "canceled"
		}
		_ = saveRun(record)
	}()
	if err := prepareRunPlatform(&opts.limits); err != nil {
		return 1, err
	}
	var token [8]byte
	if _, err := rand.Read(token[:]); err != nil {
		return 1, err
	}
	if opts.limits.TreeScope || opts.limits.TrackMemory || opts.limits.MaxRAM > 0 || opts.limits.CPUs != "" || opts.limits.CPUQuota > 0 || opts.limits.MaxCPUTime > 0 || opts.limits.MaxTasks > 0 {
		opts.request.Unit = fmt.Sprintf("agent-bus-run-%d-%s.scope", os.Getpid(), hex.EncodeToString(token[:]))
	}
	record.Request = opts.request
	_ = saveRun(record)
	waitCtx := ctx
	cancel := func() {}
	if opts.waitTimeout > 0 {
		waitCtx, cancel = context.WithTimeout(ctx, opts.waitTimeout)
	}
	defer cancel()
	// Most acquisitions are immediate. Explain a queue wait once it is visible.
	waiting := time.AfterFunc(time.Second, func() {
		fmt.Fprintln(os.Stderr, "agent-bus: waiting for resource capacity; other jobs must finish or release memory")
	})
	lease, err := acquireResources(waitCtx, opts.request)
	waiting.Stop()
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return 124, errors.New("resource wait timed out")
		}
		if errors.Is(err, context.Canceled) {
			return 130, errors.New("resource wait canceled")
		}
		return 1, err
	}
	cancel()
	record.Status = "running"
	record.Started = now()
	record.ResourceID = lease.id
	_ = saveRun(record)
	code, runErr = executeResourceCommand(ctx, opts.argv, opts.limits, opts.request.Unit, lease.SetCgroup)
	if runErr != nil && code == 0 {
		code = 1
		if errors.Is(runErr, context.Canceled) {
			code = 130
		}
	}
	if releaseErr := lease.Release(); releaseErr != nil {
		if runErr == nil {
			runErr = fmt.Errorf("release reservation: %w", releaseErr)
			if code == 0 {
				code = 1
			}
		} else {
			runErr = errors.Join(runErr, releaseErr)
		}
	}
	return code, runErr
}

func cmdRun(args []string) {
	opts, err := parseRunOptions(args, os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		die(2, "%v", err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	code, err := runManagedCommand(ctx, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-bus: %v\n", err)
	}
	if code != 0 {
		os.Exit(code)
	}
}
