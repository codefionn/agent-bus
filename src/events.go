package bus

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
)

// The event log records what happens on the bus, one JSON object per line in
// events.jsonl, so watchers such as agent-bus-web can follow along. Commands
// append under the bus lock and then poke every watcher's wake endpoint.

const maxEventLog = 8 << 20 // rotate to events.1.jsonl past this size

var (
	eventLog    = filepath.Join(root, "events.jsonl")
	eventLogOld = filepath.Join(root, "events.1.jsonl")
	watchDir    = filepath.Join(root, "watchers")

	eventsPending bool // set by logEvent, cleared by locked once it pokes the watchers
)

// event is one line of the event log.
type event struct {
	Time      float64           `json:"time"`
	Type      string            `json:"type"` // register, update, unregister, drop, note, id, meta, move, send, read
	ID        string            `json:"id"`
	Name      string            `json:"name,omitempty"`
	Harness   string            `json:"harness,omitempty"`
	Cwd       string            `json:"cwd,omitempty"`
	SessionID string            `json:"session_id,omitempty"`
	Delivery  string            `json:"delivery,omitempty"`
	Note      string            `json:"note,omitempty"`
	Meta      map[string]string `json:"meta,omitempty"`
	To        []string          `json:"to,omitempty"`        // send: recipient names
	ToIDs     []string          `json:"to_ids,omitempty"`    // send: recipient ids
	Scope     *string           `json:"scope,omitempty"`     // send: the SCOPE it went to
	Text      string            `json:"text,omitempty"`      // send: the message
	Untrusted string            `json:"untrusted,omitempty"` // send: relayed content the recipient must not trust
	Count     int               `json:"count,omitempty"`     // read: messages taken from the inbox
	Reason    string            `json:"reason,omitempty"`    // drop: exited, idle or unreadable
}

func newEvent(typ string, e *entry) event {
	return event{
		Time: now(), Type: typ, ID: e.ID, Name: e.Name, Harness: e.Harness, Cwd: e.Cwd,
		SessionID: e.SessionID, Delivery: e.Delivery, Note: e.Note, Meta: e.Meta,
	}
}

// logEvent appends ev to the event log. Call with the lock held.
func logEvent(ev event) {
	if st, err := os.Stat(eventLog); err == nil && st.Size() > maxEventLog {
		os.Rename(eventLog, eventLogOld) // a reader holding the file on Windows makes this fail; next time then
	}
	f, err := os.OpenFile(eventLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	line, _ := json.Marshal(ev)
	f.Write(append(line, '\n'))
	eventsPending = true
}

// notifyWatchers pokes every session that asked to hear about events.
func notifyWatchers() {
	ids, _ := os.ReadDir(watchDir)
	for _, id := range ids {
		poke(id.Name())
	}
}

// watch makes locked poke session id after every event.
func watch(id string) {
	os.Mkdir(watchDir, 0o700)
	os.WriteFile(filepath.Join(watchDir, id), nil, 0o600)
}

func unwatch(id string) { os.Remove(filepath.Join(watchDir, id)) }

// eventTail reads the event log incrementally. It opens the file only while
// reading, so Windows can still rotate it.
type eventTail struct {
	offset int64
}

// next returns the events appended since the last call.
func (t *eventTail) next() []event {
	st, err := os.Stat(eventLog)
	if err != nil {
		return nil
	}
	var out []event
	if st.Size() < t.offset {
		// Rotated: finish the old file, then start the new one from the top.
		out = readEvents(eventLogOld, t.offset)
		t.offset = 0
	}
	f, err := os.Open(eventLog)
	if err != nil {
		return out
	}
	defer f.Close()
	f.Seek(t.offset, io.SeekStart)
	data, _ := io.ReadAll(f)
	// Leave a line that is still being written for the next call.
	if i := bytes.LastIndexByte(data, '\n'); i >= 0 {
		data = data[:i+1]
	} else {
		data = nil
	}
	t.offset += int64(len(data))
	return append(out, parseEvents(data)...)
}

func readEvents(path string, from int64) []event {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	f.Seek(from, io.SeekStart)
	data, _ := io.ReadAll(f)
	return parseEvents(data)
}

func parseEvents(data []byte) []event {
	var out []event
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var ev event
		if json.Unmarshal(sc.Bytes(), &ev) == nil {
			out = append(out, ev)
		}
	}
	return out
}

// recentEvents returns the last n events and a tail positioned after them.
func recentEvents(n int) ([]event, *eventTail) {
	t := &eventTail{}
	all := append(readEvents(eventLogOld, 0), t.next()...)
	if len(all) > n {
		all = all[len(all)-n:]
	}
	return all, t
}
