package bus

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// A rewake hook observes unread messages without taking them. Claude wakes the
// idle model when this asyncRewake hook exits 2; its next normal hook or inbox
// command delivers the actual messages.
type rewakeClaim struct {
	PID        int     `json:"pid"`
	Start      uint64  `json:"start"`
	Registered float64 `json:"registered"`
	Endpoint   string  `json:"endpoint"`
}

func rewakePath(id string) string { return filepath.Join(root, "rewake", id+".json") }

func sameRewake(a, b *rewakeClaim) bool {
	return a.PID == b.PID && a.Start == b.Start && a.Registered == b.Registered && a.Endpoint == b.Endpoint
}

// stopRewake runs with the bus lock held. A resumed session can immediately
// claim a fresh watcher while the previous watcher finishes shutting down.
func stopRewake(id string) {
	if claim, ok := load[rewakeClaim](rewakePath(id)); ok {
		os.Remove(rewakePath(id))
		unwatch(claim.Endpoint)
		poke(claim.Endpoint)
	}
}

func hookRewake(id string) int {
	if id == "" {
		return 0
	}
	claim := &rewakeClaim{PID: os.Getpid(), Start: procStart(os.Getpid())}
	claim.Endpoint = fmt.Sprintf("%s-rewake-%d-%d", id, claim.PID, claim.Start)
	var wake <-chan struct{}
	stop := func() {}
	claimed := false
	locked(func() {
		e, ok := load[entry](sessionPath(id))
		if !ok || e.Harness != "claude" || e.Delivery == deliverManual || !alive(e) {
			return
		}
		claim.Registered = e.Registered
		if old, ok := load[rewakeClaim](rewakePath(id)); ok {
			if old.Registered == e.Registered && old.PID > 0 && old.Start != 0 && procStart(old.PID) == old.Start {
				return
			}
			stopRewake(id)
		}
		var err error
		wake, stop, err = listenWake(claim.Endpoint)
		if err != nil {
			return
		}
		if err := os.MkdirAll(filepath.Dir(rewakePath(id)), 0o700); err != nil {
			stop()
			return
		}
		writeJSON(rewakePath(id), claim)
		watch(claim.Endpoint)
		claimed = true
	})
	if !claimed {
		return 0
	}
	defer stop()
	defer func() {
		locked(func() {
			if current, ok := load[rewakeClaim](rewakePath(id)); ok && sameRewake(current, claim) {
				os.Remove(rewakePath(id))
			}
			unwatch(claim.Endpoint)
		})
	}()
	for {
		gone, unread := false, false
		locked(func() {
			current, ok := load[rewakeClaim](rewakePath(id))
			e, registered := load[entry](sessionPath(id))
			if !ok || !sameRewake(current, claim) || !registered || e.Registered != claim.Registered || e.Delivery == deliverManual || procStart(e.PID) != e.Start {
				gone = true
				return
			}
			if now()-e.Seen >= 10 {
				e.Seen = now()
				writeJSON(sessionPath(id), e)
			}
			unread = len(inboxFiles(id)) > 0
		})
		if gone {
			return 0
		}
		if unread {
			fmt.Fprintln(os.Stderr, "Peer messages are waiting. Run `agent-bus inbox` to receive them.")
			return 2
		}
		select {
		case <-wake:
		case <-time.After(time.Second):
		}
	}
}
