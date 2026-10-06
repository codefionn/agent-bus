package bus

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// resourceRequest describes admission for one command. An empty mutex with a
// positive MaxProcesses uses the shared, unnamed process group.
type resourceRequest struct {
	Mutex        string `json:"mutex,omitempty"`
	MaxProcesses int    `json:"max_processes,omitempty"`
	ReserveRAM   uint64 `json:"reserve_ram,omitempty"`
	KeepFreeRAM  uint64 `json:"keep_free_ram,omitempty"`
	Unit         string `json:"unit,omitempty"`
}

type resourceLease struct {
	id string
	mu sync.Mutex
}

type resourceRecord struct {
	ID       string          `json:"id"`
	PID      int             `json:"pid"`
	Start    uint64          `json:"start"`
	Order    uint64          `json:"order"`
	Admitted int64           `json:"admitted,omitempty"`
	Req      resourceRequest `json:"request"`
	Cgroup   string          `json:"cgroup,omitempty"`
}

type resourceState struct {
	Next    uint64           `json:"next"`
	Waiting []resourceRecord `json:"waiting,omitempty"`
	Leases  []resourceRecord `json:"leases,omitempty"`
}

const resourcePoll = 200 * time.Millisecond
const resourceScopeStartGrace = 5 * time.Second

var resourceMu sync.Mutex
var resourceMemorySnapshot = readResourceMemory
var resourceCgroupUsage = readResourceCgroupUsage
var resourceCgroupPopulated = readResourceCgroupPopulated
var resourceUnitPopulated = readResourceUnitPopulated

func resourceGroup(req resourceRequest) string {
	if req.Mutex != "" {
		return "named:" + req.Mutex
	}
	if req.MaxProcesses > 0 {
		return "global"
	}
	return ""
}

func resourceCapacity(req resourceRequest) int {
	if req.MaxProcesses > 0 {
		return req.MaxProcesses
	}
	return 1
}

func resourceID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// withResourceState holds only the resource lock. Callers must not wait for a
// command, a cgroup, or another resource while inside fn.
func withResourceState(fn func(*resourceState) (bool, error)) error {
	resourceMu.Lock()
	defer resourceMu.Unlock()
	dir := filepath.Join(root, "resources")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := lockFile(lock); err != nil {
		return fmt.Errorf("lock resources: %w", err)
	}
	path := filepath.Join(dir, "state.json")
	var s resourceState
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &s); err != nil {
			return fmt.Errorf("read resource state: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	changed, err := fn(&s)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	data, err := json.Marshal(&s)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
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
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	return nil
}

func pruneResources(s *resourceState) bool {
	changed := false
	waiting := s.Waiting[:0]
	for _, w := range s.Waiting {
		if w.PID <= 0 || w.Start == 0 || procStart(w.PID) != w.Start {
			changed = true
			continue
		}
		waiting = append(waiting, w)
	}
	s.Waiting = waiting
	leases := s.Leases[:0]
	for _, l := range s.Leases {
		if l.PID > 0 && l.Start != 0 && procStart(l.PID) == l.Start {
			leases = append(leases, l)
			continue
		}
		if resourceScopeActive(l) {
			leases = append(leases, l)
			continue
		}
		if l.Req.Unit != "" && l.Cgroup == "" {
			// systemd may have accepted a scope whose cgroup is not visible
			// yet when the short-lived wrapper exits.
			if l.Admitted > 0 && time.Since(time.Unix(0, l.Admitted)) < resourceScopeStartGrace {
				leases = append(leases, l)
				continue
			}
		}
		changed = true
	}
	s.Leases = leases
	return changed
}

func resourceScopeActive(l resourceRecord) bool {
	if l.Cgroup != "" {
		populated, err := resourceCgroupPopulated(l.Cgroup)
		if err != nil || populated {
			return true
		}
	}
	if l.Req.Unit != "" {
		populated, err := resourceUnitPopulated(l.Req.Unit)
		return err != nil || populated
	}
	return false
}

func resourceNeedsMemory(req resourceRequest) bool {
	return req.ReserveRAM > 0 || req.KeepFreeRAM > 0
}

func resourceUnusedRAM(l resourceRecord) uint64 {
	if l.Req.ReserveRAM == 0 {
		return 0
	}
	if l.Cgroup != "" {
		used, err := resourceCgroupUsage(l.Cgroup)
		if err == nil {
			if used >= l.Req.ReserveRAM {
				return 0
			}
			return l.Req.ReserveRAM - used
		}
	}
	return l.Req.ReserveRAM
}

func resourceFitsRAM(s *resourceState, req resourceRequest, available uint64) bool {
	// Saturate all subtraction. MemAvailable already reflects consumed memory;
	// only the unconsumed part of each reservation must be counted again.
	headroom := req.KeepFreeRAM
	for _, l := range s.Leases {
		if l.Req.KeepFreeRAM > headroom {
			headroom = l.Req.KeepFreeRAM
		}
		unused := resourceUnusedRAM(l)
		if unused >= available {
			available = 0
		} else {
			available -= unused
		}
	}
	if headroom > available {
		return false
	}
	return req.ReserveRAM <= available-headroom
}

func scheduleResources(s *resourceState) (bool, error) {
	if len(s.Waiting) == 0 {
		return false, nil
	}
	sort.SliceStable(s.Waiting, func(i, j int) bool { return s.Waiting[i].Order < s.Waiting[j].Order })
	var available uint64
	needSnapshot := false
	for _, w := range s.Waiting {
		if resourceNeedsMemory(w.Req) {
			needSnapshot = true
			break
		}
	}
	if needSnapshot {
		var err error
		available, _, err = resourceMemorySnapshot()
		if err != nil {
			return false, err
		}
	}
	counts := make(map[string]int)
	for _, l := range s.Leases {
		if group := resourceGroup(l.Req); group != "" {
			counts[group]++
		}
	}
	blockedGroups := make(map[string]bool)
	memoryBlocked := false
	waiting := s.Waiting[:0]
	changed := false
	for _, w := range s.Waiting {
		group := resourceGroup(w.Req)
		groupBlocked := group != "" && (blockedGroups[group] || counts[group] >= resourceCapacity(w.Req))
		ramBlocked := false
		if resourceNeedsMemory(w.Req) {
			ramBlocked = memoryBlocked || !resourceFitsRAM(s, w.Req, available)
			if ramBlocked {
				memoryBlocked = true
			}
		}
		if groupBlocked || ramBlocked {
			waiting = append(waiting, w)
			if group != "" {
				blockedGroups[group] = true
			}
			continue
		}
		w.Admitted = time.Now().UnixNano()
		s.Leases = append(s.Leases, w)
		if group != "" {
			counts[group]++
		}
		changed = true
	}
	s.Waiting = waiting
	return changed, nil
}

// acquireResources queues durably and waits until all requested resources are
// assigned. The state lock is released between polls and before the caller runs.
func acquireResources(ctx context.Context, req resourceRequest) (*resourceLease, error) {
	if req.MaxProcesses < 0 {
		return nil, fmt.Errorf("max processes must be positive")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if resourceNeedsMemory(req) {
		_, total, err := resourceMemorySnapshot()
		if err != nil {
			return nil, err
		}
		if req.ReserveRAM > total || req.KeepFreeRAM > total-req.ReserveRAM {
			return nil, fmt.Errorf("RAM reservation plus free headroom exceeds total RAM")
		}
	}
	id, err := resourceID()
	if err != nil {
		return nil, err
	}
	owner := resourceRecord{ID: id, PID: os.Getpid(), Start: procStart(os.Getpid()), Req: req}
	if owner.Start == 0 {
		return nil, fmt.Errorf("cannot identify resource owner process")
	}
	if err := withResourceState(func(s *resourceState) (bool, error) {
		pruneResources(s)
		group := resourceGroup(req)
		if group != "" {
			for _, record := range append(append([]resourceRecord{}, s.Waiting...), s.Leases...) {
				if resourceGroup(record.Req) == group && resourceCapacity(record.Req) != resourceCapacity(req) {
					return false, fmt.Errorf("conflicting capacity for resource group %q: requested %d, existing %d", group, resourceCapacity(req), resourceCapacity(record.Req))
				}
			}
		}
		s.Next++
		owner.Order = s.Next
		s.Waiting = append(s.Waiting, owner)
		_, err := scheduleResources(s)
		return true, err
	}); err != nil {
		return nil, err
	}
	lease := &resourceLease{id: id}
	ticker := time.NewTicker(resourcePoll)
	defer ticker.Stop()
	for {
		granted := false
		err := withResourceState(func(s *resourceState) (bool, error) {
			changed := pruneResources(s)
			if ctx.Err() != nil {
				changed = removeResource(s, id) || changed
				return changed, nil
			}
			more, err := scheduleResources(s)
			changed = changed || more
			if err != nil {
				return changed, err
			}
			for _, l := range s.Leases {
				if l.ID == id {
					granted = true
					break
				}
			}
			return changed, nil
		})
		if err != nil {
			_ = lease.Release()
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			_ = lease.Release()
			return nil, err
		}
		if granted {
			return lease, nil
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
}

func removeResource(s *resourceState, id string) bool {
	changed := false
	waiting := s.Waiting[:0]
	for _, w := range s.Waiting {
		if w.ID == id {
			changed = true
		} else {
			waiting = append(waiting, w)
		}
	}
	s.Waiting = waiting
	leases := s.Leases[:0]
	for _, l := range s.Leases {
		if l.ID == id {
			changed = true
		} else {
			leases = append(leases, l)
		}
	}
	s.Leases = leases
	return changed
}

// Release frees the reservation even if the child command failed. It is safe
// to call more than once.
func (l *resourceLease) Release() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return withResourceState(func(s *resourceState) (bool, error) {
		changed := pruneResources(s)
		for _, current := range s.Leases {
			if current.ID == l.id && resourceScopeActive(current) {
				// The wrapper may be exiting while descendants still run in the
				// scope. The next acquisition prunes this lease after it empties.
				return changed, nil
			}
		}
		changed = removeResource(s, l.id) || changed
		return changed, nil
	})
}

// SetCgroup records where a systemd scope accounts actual memory. This lets
// admission subtract only the reservation the scope has not yet consumed.
func (l *resourceLease) SetCgroup(path string) error {
	if l == nil {
		return fmt.Errorf("nil resource lease")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return withResourceState(func(s *resourceState) (bool, error) {
		for i := range s.Leases {
			if s.Leases[i].ID == l.id {
				s.Leases[i].Cgroup = path
				return true, nil
			}
		}
		return false, fmt.Errorf("resource lease has ended")
	})
}
