package bus

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunHistoryLifecycle(t *testing.T) {
	resourceTestRoot(t)
	for _, code := range []int{0, 7} {
		got, err := runManagedCommand(context.Background(), runOptions{argv: []string{"sh", "-c", fmt.Sprintf("exit %d", code)}})
		if err != nil || got != code {
			t.Fatalf("run = %d, %v", got, err)
		}
	}
	records, err := listRuns()
	if err != nil || len(records) != 2 {
		t.Fatalf("history = %+v, %v", records, err)
	}
	for _, r := range records {
		if r.Started == 0 || r.Finished < r.Started || r.ExitCode == nil {
			t.Fatalf("missing lifecycle: %+v", r)
		}
		want := "completed"
		if *r.ExitCode != 0 {
			want = "failed"
		}
		if r.Status != want {
			t.Fatalf("status: %+v", r)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = runManagedCommand(ctx, runOptions{argv: []string{"true"}})
	records, err = listRuns()
	if err != nil || records[0].Status != "canceled" {
		t.Fatalf("cancellation: %+v, %v", records, err)
	}
}

func TestRunHistoryRetentionAndInterruptedScope(t *testing.T) {
	resourceTestRoot(t)
	for i := 0; i < 105; i++ {
		if err := saveRun(runRecord{ID: fmt.Sprint(i), Status: "completed", Created: float64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	live := runRecord{ID: "live", Status: "queued", PID: os.Getpid(), Start: procStart(os.Getpid()), Created: 200}
	if err := saveRun(live); err != nil {
		t.Fatal(err)
	}
	records, err := listRuns()
	if err != nil || len(records) != 101 {
		t.Fatalf("retention %d %v", len(records), err)
	}
	dead := runRecord{ID: "dead", Status: "running", PID: 99999999, Start: 1, ResourceID: "scope", Created: 300}
	if err := saveRun(dead); err != nil {
		t.Fatal(err)
	}
	old := resourceCgroupPopulated
	resourceCgroupPopulated = func(string) (bool, error) { return true, nil }
	t.Cleanup(func() { resourceCgroupPopulated = old })
	if err := withResourceState(func(s *resourceState) (bool, error) {
		s.Leases = append(s.Leases, resourceRecord{ID: "scope", PID: dead.PID, Start: 1, Cgroup: "test"})
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	records, err = listRuns()
	if err != nil || records[0].Status != "running" {
		t.Fatalf("active scope: %+v %v", records[0], err)
	}
	resourceCgroupPopulated = func(string) (bool, error) { return false, nil }
	records, err = listRuns()
	if err != nil || records[0].Status != "canceled" || records[0].Finished == 0 {
		t.Fatalf("interrupted: %+v %v", records[0], err)
	}
}

func TestRunHistoryRedactsBeforePersistence(t *testing.T) {
	resourceTestRoot(t)
	r := newRunRecord(runOptions{argv: []string{"tool", "--token", "private-value"}})
	if err := saveRun(r); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "runs", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private-value") {
		t.Fatal("secret persisted")
	}
}

func TestRunHistoryQueuedCancellation(t *testing.T) {
	resourceTestRoot(t)
	req := resourceRequest{Mutex: "history-test", MaxProcesses: 1}
	lease, err := acquireResources(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() {
		code, _ := runManagedCommand(ctx, runOptions{request: req, argv: []string{"true"}})
		done <- code
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		records, err := listRuns()
		if err != nil {
			t.Fatal(err)
		}
		if len(records) == 1 {
			if records[0].Status != "queued" || records[0].Started != 0 {
				t.Fatalf("queued: %+v", records[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("queued run was not recorded")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if code := <-done; code != 130 {
		t.Fatalf("cancel code %d", code)
	}
	records, err := listRuns()
	if err != nil || records[0].Status != "canceled" || records[0].Started != 0 {
		t.Fatalf("queued cancellation: %+v %v", records, err)
	}
}
