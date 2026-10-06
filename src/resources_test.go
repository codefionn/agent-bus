package bus

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func resourceTestRoot(t *testing.T) {
	t.Helper()
	old := root
	root = t.TempDir()
	t.Cleanup(func() { root = old })
}

func resourceStateForTest(t *testing.T) resourceState {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "resources", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s resourceState
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func waitForResourceTicket(t *testing.T, count int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var got int
		if err := withResourceState(func(s *resourceState) (bool, error) {
			got = len(s.Waiting)
			return false, nil
		}); err != nil {
			t.Fatal(err)
		}
		if got == count {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("wanted %d waiting resource tickets", count)
}

func TestResourcesMutexFIFOAndIndependentGroup(t *testing.T) {
	resourceTestRoot(t)
	ctx := context.Background()
	first, err := acquireResources(ctx, resourceRequest{Mutex: "a"})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	type answer struct {
		name  string
		lease *resourceLease
		err   error
	}
	answers := make(chan answer, 2)
	go func() {
		lease, err := acquireResources(ctx, resourceRequest{Mutex: "a"})
		answers <- answer{"second", lease, err}
	}()
	waitForResourceTicket(t, 1)
	go func() {
		lease, err := acquireResources(ctx, resourceRequest{Mutex: "a"})
		answers <- answer{"third", lease, err}
	}()
	waitForResourceTicket(t, 2)
	other, err := acquireResources(ctx, resourceRequest{Mutex: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Release(); err != nil {
		t.Fatal(err)
	}
	select {
	case a := <-answers:
		if a.lease != nil {
			a.lease.Release()
		}
		t.Fatalf("same mutex admitted before release: %s, %v", a.name, a.err)
	default:
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	a := <-answers
	if a.err != nil || a.name != "second" {
		t.Fatalf("first waiter = %s, %v", a.name, a.err)
	}
	select {
	case b := <-answers:
		if b.lease != nil {
			b.lease.Release()
		}
		t.Fatal("third waiter passed the second")
	default:
	}
	if err := a.lease.Release(); err != nil {
		t.Fatal(err)
	}
	b := <-answers
	if b.err != nil || b.name != "third" {
		t.Fatalf("second waiter = %s, %v", b.name, b.err)
	}
	defer b.lease.Release()
}

func TestResourcesCancellationAndCapacityConflict(t *testing.T) {
	resourceTestRoot(t)
	first, err := acquireResources(context.Background(), resourceRequest{Mutex: "pool", MaxProcesses: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	if _, err := acquireResources(context.Background(), resourceRequest{Mutex: "pool", MaxProcesses: 3}); err == nil || !strings.Contains(err.Error(), "conflicting capacity") {
		t.Fatalf("capacity conflict = %v", err)
	}
	second, err := acquireResources(context.Background(), resourceRequest{Mutex: "pool", MaxProcesses: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Release()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := acquireResources(ctx, resourceRequest{Mutex: "pool", MaxProcesses: 2})
		result <- err
	}()
	waitForResourceTicket(t, 1)
	cancel()
	if err := <-result; err != context.Canceled {
		t.Fatalf("cancel = %v", err)
	}
	if s := resourceStateForTest(t); len(s.Waiting) != 0 || len(s.Leases) != 2 {
		t.Fatalf("canceled ticket leaked: %+v", s)
	}
}

func TestResourcesUnnamedGlobalGroup(t *testing.T) {
	resourceTestRoot(t)
	first, err := acquireResources(context.Background(), resourceRequest{MaxProcesses: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := acquireResources(ctx, resourceRequest{MaxProcesses: 1}); err != context.DeadlineExceeded {
		t.Fatalf("global group wait = %v", err)
	}
	if s := resourceStateForTest(t); len(s.Waiting) != 0 || len(s.Leases) != 1 {
		t.Fatalf("global group state: %+v", s)
	}
}

func TestResourcesStaleOwnerAndCgroup(t *testing.T) {
	resourceTestRoot(t)
	owner := resourceRecord{ID: "stale", PID: os.Getpid(), Start: procStart(os.Getpid()) + 1, Req: resourceRequest{Mutex: "m", ReserveRAM: 20, Unit: "unit.scope"}}
	oldPopulated := resourceCgroupPopulated
	oldUnit := resourceUnitPopulated
	resourceCgroupPopulated = func(string) (bool, error) { return false, nil }
	resourceUnitPopulated = func(string) (bool, error) { return true, nil }
	t.Cleanup(func() { resourceCgroupPopulated, resourceUnitPopulated = oldPopulated, oldUnit })
	if err := withResourceState(func(s *resourceState) (bool, error) {
		s.Leases = append(s.Leases, owner)
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := withResourceState(func(s *resourceState) (bool, error) {
		if pruneResources(s) {
			t.Error("populated orphan scope was pruned")
		}
		return false, nil
	}); err != nil {
		t.Fatal(err)
	}
	resourceUnitPopulated = func(string) (bool, error) { return false, nil }
	lease, err := acquireResources(context.Background(), resourceRequest{Mutex: "m"})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if s := resourceStateForTest(t); len(s.Leases) != 1 || s.Leases[0].ID != lease.id {
		t.Fatalf("stale lease not pruned: %+v", s)
	}
}

func TestResourcesReleaseWaitsForPopulatedScope(t *testing.T) {
	resourceTestRoot(t)
	oldPopulated := resourceCgroupPopulated
	oldUnit := resourceUnitPopulated
	active := true
	resourceCgroupPopulated = func(string) (bool, error) { return active, nil }
	resourceUnitPopulated = func(string) (bool, error) { return active, nil }
	t.Cleanup(func() { resourceCgroupPopulated, resourceUnitPopulated = oldPopulated, oldUnit })
	lease, err := acquireResources(context.Background(), resourceRequest{Mutex: "scope", Unit: "unit.scope"})
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.SetCgroup("/mock/cgroup"); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if s := resourceStateForTest(t); len(s.Leases) != 1 {
		t.Fatalf("populated scope lost lease: %+v", s)
	}
	active = false
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if s := resourceStateForTest(t); len(s.Leases) != 0 {
		t.Fatalf("empty scope retained lease: %+v", s)
	}
}

func TestResourcesMissingCgroupIsEmpty(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("cgroup v2 is Linux-only")
	}
	populated, err := readResourceCgroupPopulated(filepath.Join(t.TempDir(), "gone"))
	if err != nil || populated {
		t.Fatalf("removed cgroup = populated %v, error %v", populated, err)
	}
}

func TestResourcesRAMUnusedReservation(t *testing.T) {
	resourceTestRoot(t)
	oldMemory, oldUsage := resourceMemorySnapshot, resourceCgroupUsage
	resourceMemorySnapshot = func() (uint64, uint64, error) { return 100, 200, nil }
	var used atomic.Uint64
	resourceCgroupUsage = func(string) (uint64, error) { return used.Load(), nil }
	t.Cleanup(func() { resourceMemorySnapshot, resourceCgroupUsage = oldMemory, oldUsage })
	first, err := acquireResources(context.Background(), resourceRequest{ReserveRAM: 60, KeepFreeRAM: 10})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	if err := first.SetCgroup("/mock/cgroup"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result := make(chan struct {
		lease *resourceLease
		err   error
	}, 1)
	go func() {
		lease, err := acquireResources(ctx, resourceRequest{ReserveRAM: 50, KeepFreeRAM: 10})
		result <- struct {
			lease *resourceLease
			err   error
		}{lease, err}
	}()
	waitForResourceTicket(t, 1)
	used.Store(30)
	r := <-result
	if r.err != nil {
		t.Fatal(r.err)
	}
	defer r.lease.Release()
	if _, err := acquireResources(context.Background(), resourceRequest{ReserveRAM: 191, KeepFreeRAM: 10}); err == nil || !strings.Contains(err.Error(), "exceeds total") {
		t.Fatalf("impossible RAM request = %v", err)
	}
}

func TestResourcesMutexWaiterDoesNotBlockOtherMemoryGroup(t *testing.T) {
	resourceTestRoot(t)
	oldMemory := resourceMemorySnapshot
	resourceMemorySnapshot = func() (uint64, uint64, error) { return 100, 200, nil }
	t.Cleanup(func() { resourceMemorySnapshot = oldMemory })
	holder, err := acquireResources(context.Background(), resourceRequest{Mutex: "a"})
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Release()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result := make(chan struct {
		lease *resourceLease
		err   error
	}, 1)
	go func() {
		lease, err := acquireResources(ctx, resourceRequest{Mutex: "a", ReserveRAM: 40})
		result <- struct {
			lease *resourceLease
			err   error
		}{lease, err}
	}()
	waitForResourceTicket(t, 1)
	bCtx, bCancel := context.WithTimeout(context.Background(), time.Second)
	defer bCancel()
	b, err := acquireResources(bCtx, resourceRequest{Mutex: "b", ReserveRAM: 30})
	if err != nil {
		t.Fatalf("unrelated RAM request waited behind mutex a: %v", err)
	}
	defer b.Release()
	if err := holder.Release(); err != nil {
		t.Fatal(err)
	}
	a := <-result
	if a.err != nil {
		t.Fatal(a.err)
	}
	defer a.lease.Release()
}

func TestResourcesPreserveExistingHeadroom(t *testing.T) {
	s := &resourceState{Leases: []resourceRecord{{Req: resourceRequest{ReserveRAM: 20, KeepFreeRAM: 60}}}}
	oldUsage := resourceCgroupUsage
	resourceCgroupUsage = func(string) (uint64, error) { return 0, nil }
	defer func() { resourceCgroupUsage = oldUsage }()
	if resourceFitsRAM(s, resourceRequest{ReserveRAM: 30, KeepFreeRAM: 10}, 100) {
		t.Fatal("new reservation spent earlier lease's free headroom")
	}
	if !resourceFitsRAM(s, resourceRequest{ReserveRAM: 20, KeepFreeRAM: 10}, 100) {
		t.Fatal("exact available budget should fit")
	}
}
