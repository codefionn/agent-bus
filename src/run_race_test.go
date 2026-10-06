//go:build !windows

package bus

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// These tests drive the built agent-bus binary, so every `run` is a separate
// process and the admission lock is exercised across processes.

var (
	runBinOnce sync.Once
	runBin     string
	runBinErr  error
)

func runBinary(t *testing.T) string {
	t.Helper()
	runBinOnce.Do(func() {
		// /tmp may be RAM-backed; keep the binary on disk and remove it in TestMain.
		parent := ""
		if cache, err := os.UserCacheDir(); err == nil && os.MkdirAll(cache, 0o700) == nil {
			parent = cache
		}
		dir, err := os.MkdirTemp(parent, "agent-bus-run-test-")
		if err != nil {
			runBinErr = err
			return
		}
		runBin = filepath.Join(dir, "agent-bus")
		out, err := exec.Command("go", "build", "-o", runBin, "../cmd/agent-bus").CombinedOutput()
		if err != nil {
			runBinErr = fmt.Errorf("build: %v\n%s", err, out)
		}
	})
	if runBinErr != nil {
		t.Fatal(runBinErr)
	}
	return runBin
}

func TestMain(m *testing.M) {
	code := m.Run()
	if runBin != "" {
		os.RemoveAll(filepath.Dir(runBin))
	}
	os.Exit(code)
}

// runEnv is one private bus state directory shared by the runs of a test.
type runEnv struct {
	t   *testing.T
	bin string
	dir string // state directory
	tmp string // scratch for the commands under test
}

func newRunEnv(t *testing.T) *runEnv {
	t.Helper()
	base := t.TempDir()
	state := filepath.Join(base, "state")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { killInDir(base) })
	return &runEnv{t: t, bin: runBinary(t), dir: state, tmp: base}
}

// killInDir kills leftover commands of a test: the runner puts its command in
// a process group of its own, so killing the runner's group misses it.
func killInDir(dir string) {
	procs, _ := filepath.Glob("/proc/[0-9]*/cwd")
	for _, link := range procs {
		if cwd, err := os.Readlink(link); err == nil && (cwd == dir || strings.HasPrefix(cwd, dir+"/")) {
			if pid, err := strconv.Atoi(filepath.Base(filepath.Dir(link))); err == nil {
				syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	}
}

// cmd builds `agent-bus run ARGS... -- sh -c SCRIPT`.
func (r *runEnv) cmd(args []string, script string) *exec.Cmd {
	full := append([]string{"run"}, args...)
	full = append(full, "--", "/bin/sh", "-c", script)
	c := exec.Command(r.bin, full...)
	c.Dir = r.tmp
	c.Env = append(os.Environ(), "AGENT_BUS_DIR="+r.dir, "T="+r.tmp)
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return c
}

type runResult struct {
	code int
	out  string
	took time.Duration
}

func (r *runEnv) run(timeout time.Duration, args []string, script string) runResult {
	r.t.Helper()
	c := r.cmd(args, script)
	var buf bytes.Buffer
	c.Stdout, c.Stderr = &buf, &buf
	start := time.Now()
	if err := c.Start(); err != nil {
		r.t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	var err error
	select {
	case err = <-done:
	case <-time.After(timeout):
		syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
		<-done
		r.t.Fatalf("run %v did not finish within %v\n%s", args, timeout, buf.String())
	}
	return runResult{code: exitCode(err), out: buf.String(), took: time.Since(start)}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if exit, ok := err.(*exec.ExitError); ok {
		if ws, ok := exit.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return exit.ExitCode()
	}
	return -1
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return strings.Fields(string(data))
}

// maxInt parses every line as an int and returns the largest.
func maxInt(t *testing.T, lines []string) int {
	t.Helper()
	best := 0
	for _, l := range lines {
		n, err := strconv.Atoi(l)
		if err != nil {
			t.Fatalf("bad count %q", l)
		}
		best = max(best, n)
	}
	return best
}

// killPIDFile kills the process whose pid a command wrote and waits until it is gone.
func killPIDFile(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		t.Fatalf("bad pid %q", data)
	}
	syscall.Kill(pid, syscall.SIGKILL)
	for deadline := time.Now().Add(5 * time.Second); syscall.Kill(pid, 0) == nil; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("pid %d survived SIGKILL", pid)
		}
	}
}

func waitFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !fileExists(path) {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not appear within %v", path, timeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRunExitCodePropagates(t *testing.T) {
	r := newRunEnv(t)
	if res := r.run(10*time.Second, nil, "exit 7"); res.code != 7 {
		t.Fatalf("exit = %d, want 7\n%s", res.code, res.out)
	}
}

// Each holder records how many holders are inside the critical section when it
// enters. With a cap of N no holder may ever see more than N.
func concurrentHolders(t *testing.T, r *runEnv, n int, args []string) int {
	t.Helper()
	inside := filepath.Join(r.tmp, "inside")
	os.Mkdir(inside, 0o700)
	script := `touch "$T/inside/$$"; ls "$T/inside" | wc -l >> "$T/counts"; sleep 0.15; rm "$T/inside/$$"`
	var wg sync.WaitGroup
	results := make([]runResult, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = r.run(60*time.Second, args, script)
		}()
	}
	wg.Wait()
	for i, res := range results {
		if res.code != 0 {
			t.Fatalf("run %d exit = %d\n%s", i, res.code, res.out)
		}
	}
	counts := readLines(t, filepath.Join(r.tmp, "counts"))
	if len(counts) != n {
		t.Fatalf("%d commands ran, want %d", len(counts), n)
	}
	return maxInt(t, counts)
}

func TestRunMutexExcludes(t *testing.T) {
	r := newRunEnv(t)
	if got := concurrentHolders(t, r, 12, []string{"--mutex", "m"}); got != 1 {
		t.Fatalf("%d holders inside mutex at once, want 1", got)
	}
}

func TestRunMutexMaxProcesses(t *testing.T) {
	r := newRunEnv(t)
	got := concurrentHolders(t, r, 16, []string{"--mutex", "m", "--max-processes", "3"})
	if got > 3 {
		t.Fatalf("%d holders inside at once, cap 3", got)
	}
	if got < 2 {
		t.Errorf("never more than %d holder at once; cap 3 is not used", got)
	}
}

func TestRunDistinctMutexesDoNotBlock(t *testing.T) {
	r := newRunEnv(t)
	hold := r.cmd([]string{"--mutex", "a"}, `touch "$T/a-in"; sleep 30`)
	if err := hold.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { syscall.Kill(-hold.Process.Pid, syscall.SIGKILL); hold.Wait() }()
	waitFile(t, filepath.Join(r.tmp, "a-in"), 10*time.Second)
	res := r.run(10*time.Second, []string{"--mutex", "b", "--wait-timeout", "2s"}, "true")
	if res.code != 0 {
		t.Fatalf("mutex b blocked behind a: exit %d\n%s", res.code, res.out)
	}
}

// A waiter queued behind mutex a must not hold up a later run for mutex b.
func TestRunQueuedWaiterDoesNotBlockOtherMutex(t *testing.T) {
	r := newRunEnv(t)
	hold := r.cmd([]string{"--mutex", "a"}, `touch "$T/a-in"; sleep 30`)
	if err := hold.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { syscall.Kill(-hold.Process.Pid, syscall.SIGKILL); hold.Wait() }()
	waitFile(t, filepath.Join(r.tmp, "a-in"), 10*time.Second)
	queued := r.cmd([]string{"--mutex", "a"}, "true")
	if err := queued.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { syscall.Kill(-queued.Process.Pid, syscall.SIGKILL); queued.Wait() }()
	time.Sleep(300 * time.Millisecond)
	res := r.run(10*time.Second, []string{"--mutex", "b", "--wait-timeout", "2s"}, "true")
	if res.code != 0 {
		t.Fatalf("mutex b blocked behind a queued waiter for a: exit %d\n%s", res.code, res.out)
	}
}

func TestRunWaitTimeout(t *testing.T) {
	r := newRunEnv(t)
	hold := r.cmd([]string{"--mutex", "m"}, `touch "$T/held"; sleep 30`)
	if err := hold.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { syscall.Kill(-hold.Process.Pid, syscall.SIGKILL); hold.Wait() }()
	waitFile(t, filepath.Join(r.tmp, "held"), 10*time.Second)
	res := r.run(10*time.Second, []string{"--mutex", "m", "--wait-timeout", "300ms"}, `touch "$T/ran"`)
	if res.code == 0 {
		t.Fatal("run succeeded while the mutex was held")
	}
	if fileExists(filepath.Join(r.tmp, "ran")) {
		t.Fatal("command ran without the mutex")
	}
	if res.took > 5*time.Second {
		t.Errorf("wait timeout of 300ms took %v", res.took)
	}
}

// Waiters queue behind a holder one at a time; they must be admitted in
// arrival order.
func TestRunFIFO(t *testing.T) {
	r := newRunEnv(t)
	gate := filepath.Join(r.tmp, "go")
	hold := r.cmd([]string{"--mutex", "m"}, `touch "$T/held"; while [ ! -e "$T/go" ]; do sleep 0.02; done`)
	if err := hold.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { syscall.Kill(-hold.Process.Pid, syscall.SIGKILL); hold.Wait() }()
	waitFile(t, filepath.Join(r.tmp, "held"), 10*time.Second)

	const n = 6
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := r.run(60*time.Second, []string{"--mutex", "m"}, fmt.Sprintf(`echo %d >> "$T/order"`, i))
			if res.code != 0 {
				t.Errorf("waiter %d exit %d\n%s", i, res.code, res.out)
			}
		}()
		time.Sleep(250 * time.Millisecond) // let waiter i take its ticket first
	}
	os.WriteFile(gate, nil, 0o600)
	wg.Wait()
	order := readLines(t, filepath.Join(r.tmp, "order"))
	want := []string{"0", "1", "2", "3", "4", "5"}
	if strings.Join(order, " ") != strings.Join(want, " ") {
		t.Fatalf("admission order %v, want %v", order, want)
	}
}

// A run killed with SIGKILL cannot release its lease itself. Once its whole
// tree is gone the next run must notice and take the mutex.
func TestRunKilledHolderReleases(t *testing.T) {
	r := newRunEnv(t)
	hold := r.cmd([]string{"--mutex", "m"}, `echo $$ > "$T/pid"; touch "$T/held"; exec sleep 30`)
	if err := hold.Start(); err != nil {
		t.Fatal(err)
	}
	waitFile(t, filepath.Join(r.tmp, "held"), 10*time.Second)
	hold.Process.Signal(syscall.SIGKILL) // the runner first, so it cannot clean up
	hold.Wait()
	killPIDFile(t, filepath.Join(r.tmp, "pid")) // then the orphaned command
	res := r.run(20*time.Second, []string{"--mutex", "m", "--wait-timeout", "10s"}, "true")
	if res.code != 0 {
		t.Fatalf("mutex stayed held after its runner died: exit %d\n%s", res.code, res.out)
	}
}

// Waiters that give up (timeout or kill) must not leave tickets that block the
// queue for everyone behind them.
func TestRunAbandonedWaitersDoNotBlock(t *testing.T) {
	r := newRunEnv(t)
	hold := r.cmd([]string{"--mutex", "m"}, `touch "$T/held"; while [ ! -e "$T/go" ]; do sleep 0.02; done`)
	if err := hold.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { syscall.Kill(-hold.Process.Pid, syscall.SIGKILL); hold.Wait() }()
	waitFile(t, filepath.Join(r.tmp, "held"), 10*time.Second)

	r.run(10*time.Second, []string{"--mutex", "m", "--wait-timeout", "200ms"}, "true")
	killed := r.cmd([]string{"--mutex", "m"}, "true")
	if err := killed.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	syscall.Kill(-killed.Process.Pid, syscall.SIGKILL)
	killed.Wait()

	os.WriteFile(filepath.Join(r.tmp, "go"), nil, 0o600)
	hold.Wait()
	res := r.run(15*time.Second, []string{"--mutex", "m", "--wait-timeout", "5s"}, "true")
	if res.code != 0 {
		t.Fatalf("queue blocked by abandoned tickets: exit %d\n%s", res.code, res.out)
	}
}

// SIGTERM to the runner reaches the command, and the runner reports it.
func TestRunForwardsSIGTERM(t *testing.T) {
	r := newRunEnv(t)
	c := r.cmd(nil, `trap 'touch "$T/term"; exit 0' TERM; touch "$T/up"; while :; do sleep 0.02; done`)
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	defer syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
	waitFile(t, filepath.Join(r.tmp, "up"), 10*time.Second)
	c.Process.Signal(syscall.SIGTERM)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { c.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("runner did not exit after SIGTERM")
	}
	if !fileExists(filepath.Join(r.tmp, "term")) {
		t.Fatal("command never saw SIGTERM")
	}
}
