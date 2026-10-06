package bus

import (
	"io"
	"testing"
	"time"
)

func TestRunOptionsReservationsAndBudgets(t *testing.T) {
	opts, err := parseRunOptions([]string{
		"--mutex", "builds", "--max-ram", "2GiB", "--cpus", "0,2-3",
		"--cpu-quota", "150%", "--max-cpu-time", "2m", "--wait-timeout", "30",
		"--", "sh", "-c", "exit 7",
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if opts.request.Mutex != "builds" || opts.request.MaxProcesses != 1 {
		t.Fatalf("mutex defaults: %+v", opts.request)
	}
	if opts.request.ReserveRAM != 2<<30 || opts.limits.MaxRAM != 2<<30 || !opts.limits.TrackMemory {
		t.Fatalf("memory defaults: %+v %+v", opts.request, opts.limits)
	}
	if opts.request.KeepFreeRAM != 1<<30 || opts.limits.CPUQuota != 150 || opts.limits.MaxCPUTime != 2*time.Minute || opts.waitTimeout != 30*time.Second {
		t.Fatalf("budgets: %+v", opts)
	}
	if len(opts.argv) != 3 || opts.argv[2] != "exit 7" {
		t.Fatalf("command args were altered: %q", opts.argv)
	}
	// A smaller reservation is an explicit admission estimate, separate from the cap.
	opts, err = parseRunOptions([]string{"--max-ram", "4G", "--reserve-ram", "1G", "--keep-free-ram", "0", "--", "true"}, io.Discard)
	if err != nil || opts.request.ReserveRAM != 1<<30 || opts.request.KeepFreeRAM != 0 {
		t.Fatalf("explicit reservation: %+v, %v", opts, err)
	}
}

func TestRunOptionsRejectInvalidBudgets(t *testing.T) {
	for _, flags := range [][]string{
		{"--max-ram", "0"}, {"--max-ram", "-1G"}, {"--reserve-ram", "999999999T"},
		{"--reserve-ram", "NaN"}, {"--max-processes", "0"}, {"--max-processes", "-1"},
		{"--max-tasks", "0"}, {"--cpu-quota", "NaN"}, {"--cpu-quota", "Inf"},
		{"--cpu-quota", "0"}, {"--max-cpu-time", "0"}, {"--max-cpu-time", "-1s"},
		{"--wait-timeout", "NaN"}, {"--wait-timeout", "1e100"},
	} {
		t.Run(flags[0]+"="+flags[1], func(t *testing.T) {
			args := append(append([]string{}, flags...), "--", "true")
			if _, err := parseRunOptions(args, io.Discard); err == nil {
				t.Fatalf("accepted %q", flags)
			}
		})
	}
	if _, err := parseRunOptions(nil, io.Discard); err == nil {
		t.Fatal("accepted missing command")
	}
}

func TestByteSizeUnits(t *testing.T) {
	for s, want := range map[string]uint64{"1G": 1 << 30, "1GiB": 1 << 30, "1GB": 1e9, "1.5MiB": 1572864, "1024": 1024, "0": 0} {
		if got, err := parseByteSize(s); err != nil || got != want {
			t.Fatalf("%q: got %d, %v; want %d", s, got, err, want)
		}
	}
}
