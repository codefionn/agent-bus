package bus

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// runRecord contains only redacted command arguments. Execution always uses the
// original arguments from runOptions.
type runRecord struct {
	ID         string          `json:"id"`
	Status     string          `json:"status"`
	OwnerID    string          `json:"owner_id,omitempty"`
	OwnerName  string          `json:"owner_name,omitempty"`
	Command    []string        `json:"command"`
	Cwd        string          `json:"cwd"`
	PID        int             `json:"pid"`
	Start      uint64          `json:"start"`
	Created    float64         `json:"created"`
	Started    float64         `json:"started,omitempty"`
	Finished   float64         `json:"finished,omitempty"`
	ExitCode   *int            `json:"exit_code,omitempty"`
	Request    resourceRequest `json:"request"`
	Limits     runRecordLimits `json:"limits"`
	ResourceID string          `json:"resource_id,omitempty"`
	Error      string          `json:"error,omitempty"`
}

type runRecordLimits struct {
	CPUs       string  `json:"cpus,omitempty"`
	MaxRAM     uint64  `json:"max_ram,omitempty"`
	CPUQuota   float64 `json:"cpu_quota,omitempty"`
	MaxCPUTime float64 `json:"max_cpu_time,omitempty"`
	MaxTasks   int     `json:"max_tasks,omitempty"`
}

var runHistoryMu sync.Mutex

func withRunHistory(fn func(*[]runRecord) bool) error {
	runHistoryMu.Lock()
	defer runHistoryMu.Unlock()
	dir := filepath.Join(root, "runs")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := lockFile(lock); err != nil {
		return err
	}
	path := filepath.Join(dir, "state.json")
	records := []runRecord{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &records); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if !fn(&records) {
		return nil
	}
	sort.SliceStable(records, func(i, j int) bool { return records[i].Created > records[j].Created })
	kept := records[:0]
	terminal := 0
	for _, r := range records {
		if r.Status != "queued" && r.Status != "running" {
			terminal++
			if terminal > 100 {
				continue
			}
		}
		kept = append(kept, r)
	}
	data, err := json.Marshal(kept)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func saveRun(r runRecord) error {
	r.Command = redactRunArgs(r.Command)
	return withRunHistory(func(records *[]runRecord) bool {
		for i := range *records {
			if (*records)[i].ID == r.ID {
				(*records)[i] = r
				return true
			}
		}
		*records = append(*records, r)
		return true
	})
}

func newRunRecord(opts runOptions) runRecord {
	id, _ := resourceID()
	cwd, _ := os.Getwd()
	owner := cleanID(os.Getenv("AGENT_BUS_ID"))
	if owner == "" {
		owner, _, _, _ = identity()
	}
	r := runRecord{ID: id, Status: "queued", OwnerID: owner, Command: redactRunArgs(opts.argv), Cwd: cwd, PID: os.Getpid(), Start: procStart(os.Getpid()), Created: now(), Request: opts.request,
		Limits: runRecordLimits{CPUs: opts.limits.CPUs, MaxRAM: opts.limits.MaxRAM, CPUQuota: opts.limits.CPUQuota, MaxCPUTime: opts.limits.MaxCPUTime.Seconds(), MaxTasks: opts.limits.MaxTasks}}
	if owner != "" {
		if e, ok := load[entry](filepath.Join(sessDir, owner+".json")); ok {
			r.OwnerName = e.Name
		}
	}
	return r
}

// Snapshot resources before taking the history lock. No history operation takes
// the bus lock, and resource callbacks never take the history lock.
func listRuns() ([]runRecord, error) {
	active := make(map[string]bool)
	if err := withResourceState(func(s *resourceState) (bool, error) {
		changed := pruneResources(s)
		for _, l := range s.Leases {
			active[l.ID] = true
			if l.Req.Unit != "" {
				active[l.Req.Unit] = true
			}
		}
		return changed, nil
	}); err != nil {
		return nil, err
	}
	var result []runRecord
	err := withRunHistory(func(records *[]runRecord) bool {
		changed := false
		for i := range *records {
			r := &(*records)[i]
			if (r.Status == "queued" || r.Status == "running") && (r.PID <= 0 || r.Start == 0 || procStart(r.PID) != r.Start) && !active[r.ResourceID] && (r.Request.Unit == "" || !active[r.Request.Unit]) {
				r.Status = "canceled"
				r.Finished = now()
				r.Error = "wrapper interrupted"
				changed = true
			}
		}
		result = append([]runRecord{}, (*records)...)
		return changed
	})
	sort.SliceStable(result, func(i, j int) bool { return result[i].Created > result[j].Created })
	// Pruning interrupted wrappers can create additional terminal records. Match
	// the bounded persisted view in this first response too.
	kept := result[:0]
	terminal := 0
	for _, r := range result {
		if r.Status != "queued" && r.Status != "running" {
			terminal++
			if terminal > 100 {
				continue
			}
		}
		kept = append(kept, r)
	}
	return kept, err
}
