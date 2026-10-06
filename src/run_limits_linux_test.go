//go:build linux

package bus

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Limits apply through a systemd user scope, so they must hold for every
// process the command starts, not only the direct child.

func needScopes(t *testing.T) {
	t.Helper()
	if err := exec.Command("systemd-run", "--user", "--scope", "-q", "true").Run(); err != nil {
		t.Skipf("systemd user scopes unavailable: %v", err)
	}
}

// grandchild wraps a script so it runs two processes below the runner.
func grandchild(script string) string {
	return `/bin/sh -c '/bin/sh -c "` + script + `"'`
}

func TestRunCommandInOwnScope(t *testing.T) {
	needScopes(t)
	r := newRunEnv(t)
	res := r.run(15*time.Second, []string{"--max-ram", "256M"}, grandchild(`cat /proc/self/cgroup > $T/cg`))
	if res.code != 0 {
		t.Fatalf("exit %d\n%s", res.code, res.out)
	}
	data, _ := os.ReadFile(filepath.Join(r.tmp, "cg"))
	if !strings.Contains(string(data), "agent-bus-run-") {
		t.Fatalf("grandchild not in an agent-bus-run scope: %s", data)
	}
}

func TestRunMaxRAMKillsTree(t *testing.T) {
	needScopes(t)
	r := newRunEnv(t)
	// tail keeps the unterminated line in memory, so it grows to 512M.
	res := r.run(60*time.Second, []string{"--max-ram", "64M"}, grandchild(`head -c 512M /dev/zero | tail -n 1 | wc -c > $T/bytes`))
	n, _ := strconv.Atoi(strings.TrimSpace(string(must(os.ReadFile(filepath.Join(r.tmp, "bytes"))))))
	if n > 64<<20 {
		t.Fatalf("grandchild held %d bytes under --max-ram 64M\n%s", n, res.out)
	}
}

func TestRunCPUsPinsTree(t *testing.T) {
	needScopes(t)
	r := newRunEnv(t)
	res := r.run(15*time.Second, []string{"--cpus", "0"}, grandchild(`grep Cpus_allowed_list /proc/self/status > $T/cpus`))
	if res.code != 0 {
		t.Fatalf("exit %d\n%s", res.code, res.out)
	}
	data, _ := os.ReadFile(filepath.Join(r.tmp, "cpus"))
	if f := strings.Fields(string(data)); len(f) != 2 || f[1] != "0" {
		t.Fatalf("grandchild allowed cpus %q, want 0", data)
	}
}

// A child cannot widen its affinity past the scope's cpuset.
func TestRunCPUsNotEscapable(t *testing.T) {
	needScopes(t)
	r := newRunEnv(t)
	res := r.run(15*time.Second, []string{"--cpus", "0"}, `taskset -cp 0-$(($(nproc --all)-1)) $$ >/dev/null 2>&1; grep Cpus_allowed_list /proc/self/status > $T/cpus`)
	if res.code != 0 {
		t.Fatalf("exit %d\n%s", res.code, res.out)
	}
	data, _ := os.ReadFile(filepath.Join(r.tmp, "cpus"))
	if f := strings.Fields(string(data)); len(f) != 2 || f[1] != "0" {
		t.Fatalf("child widened its cpus to %q", data)
	}
}

func TestRunCPUQuota(t *testing.T) {
	needScopes(t)
	r := newRunEnv(t)
	res := r.run(15*time.Second, []string{"--cpu-quota", "20%"}, `cat "/sys/fs/cgroup$(sed -n 's/^0:://p' /proc/self/cgroup)/cpu.max" > "$T/max"`)
	if res.code != 0 {
		t.Fatalf("exit %d\n%s", res.code, res.out)
	}
	data, _ := os.ReadFile(filepath.Join(r.tmp, "max"))
	f := strings.Fields(string(data))
	if len(f) != 2 {
		t.Fatalf("cpu.max = %q", data)
	}
	quota, _ := strconv.Atoi(f[0])
	period, _ := strconv.Atoi(f[1])
	if period == 0 || quota*100/period != 20 {
		t.Fatalf("cpu.max = %q, want 20%%", data)
	}
}

// --max-cpu-time counts the CPU seconds of the whole tree: two spinning
// grandchildren exhaust 1s in about half a second of wall time.
func TestRunMaxCPUTimeAggregate(t *testing.T) {
	needScopes(t)
	r := newRunEnv(t)
	spin := `/bin/sh -c 'while :; do :; done'`
	res := r.run(30*time.Second, []string{"--max-cpu-time", "1s"}, spin+" & "+spin+" & wait")
	if res.code == 0 {
		t.Fatalf("spinning tree exited 0 under --max-cpu-time\n%s", res.out)
	}
	if res.took > 10*time.Second {
		t.Fatalf("1s CPU budget over two spinners took %v wall", res.took)
	}
	if out, _ := exec.Command("pgrep", "-f", "agent-bus-run-").Output(); len(out) > 0 {
		t.Logf("leftover scope processes: %s", out)
	}
}

// Nothing in the tree may outlive the runner when a CPU budget ends it.
func TestRunMaxCPUTimeLeavesNoOrphans(t *testing.T) {
	needScopes(t)
	r := newRunEnv(t)
	res := r.run(30*time.Second, []string{"--max-cpu-time", "500ms"},
		`(sleep 300; touch $T/orphan) & echo $! > $T/pid; while :; do :; done`)
	if res.code == 0 {
		t.Fatalf("exit 0\n%s", res.out)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(must(os.ReadFile(filepath.Join(r.tmp, "pid"))))))
	time.Sleep(200 * time.Millisecond)
	if pid > 0 && syscall.Kill(pid, 0) == nil {
		syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("background child %d survived the CPU budget", pid)
	}
}

func TestRunMaxTasks(t *testing.T) {
	needScopes(t)
	r := newRunEnv(t)
	r.run(30*time.Second, []string{"--max-tasks", "6"},
		`for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do (echo x >> $T/started; sleep 2) 2>/dev/null & done; wait`)
	started := readLines(t, filepath.Join(r.tmp, "started"))
	if len(started) >= 20 {
		t.Fatalf("%d processes started under --max-tasks 6", len(started))
	}
}

// A reservation larger than the machine can give never runs; it times out.
func TestRunReserveRAMTooLarge(t *testing.T) {
	r := newRunEnv(t)
	res := r.run(10*time.Second, []string{"--reserve-ram", "100T", "--wait-timeout", "300ms"}, `touch "$T/ran"`)
	if res.code == 0 || fileExists(filepath.Join(r.tmp, "ran")) {
		t.Fatalf("100T reservation admitted: exit %d\n%s", res.code, res.out)
	}
}

// Two reservations that each take 60% of available memory must not overlap.
func TestRunReserveRAMSerializes(t *testing.T) {
	r := newRunEnv(t)
	avail := memAvailable(t)
	size := strconv.FormatUint(avail*6/10/1024/1024, 10) + "M"
	args := []string{"--reserve-ram", size, "--keep-free-ram", "0"}
	hold := r.cmd(args, `touch "$T/held"; while [ ! -e "$T/go" ]; do sleep 0.02; done`)
	if err := hold.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { syscall.Kill(-hold.Process.Pid, syscall.SIGKILL); hold.Wait() }()
	waitFile(t, filepath.Join(r.tmp, "held"), 10*time.Second)

	res := r.run(10*time.Second, append(args, "--wait-timeout", "500ms"), `touch "$T/second"`)
	if res.code == 0 || fileExists(filepath.Join(r.tmp, "second")) {
		t.Fatalf("second %s reservation admitted beside the first\n%s", size, res.out)
	}
	os.WriteFile(filepath.Join(r.tmp, "go"), nil, 0o600)
	hold.Wait()
	if res := r.run(15*time.Second, append(args, "--wait-timeout", "5s"), "true"); res.code != 0 {
		t.Fatalf("reservation not released after holder exit: %d\n%s", res.code, res.out)
	}
}

func memAvailable(t *testing.T) uint64 {
	t.Helper()
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "MemAvailable:" {
			kb, _ := strconv.ParseUint(f[1], 10, 64)
			return kb * 1024
		}
	}
	t.Fatal("no MemAvailable")
	return 0
}

func must[T any](v T, _ error) T { return v }

// With a scope the lease follows the cgroup: an orphaned command keeps the
// mutex, and the mutex frees once systemd removes the empty scope.
func TestRunKilledScopedHolderReleases(t *testing.T) {
	needScopes(t)
	r := newRunEnv(t)
	args := []string{"--mutex", "m", "--max-ram", "256M"}
	hold := r.cmd(args, `echo $$ > "$T/pid"; touch "$T/held"; exec sleep 30`)
	if err := hold.Start(); err != nil {
		t.Fatal(err)
	}
	waitFile(t, filepath.Join(r.tmp, "held"), 10*time.Second)
	hold.Process.Signal(syscall.SIGKILL)
	hold.Wait()
	if res := r.run(10*time.Second, append(args, "--wait-timeout", "500ms"), "true"); res.code == 0 {
		t.Error("mutex free while the orphaned command still runs")
	}
	killPIDFile(t, filepath.Join(r.tmp, "pid"))
	if res := r.run(20*time.Second, append(args, "--wait-timeout", "10s"), "true"); res.code != 0 {
		t.Fatalf("mutex stayed held after the scope emptied: exit %d\n%s", res.code, res.out)
	}
}
