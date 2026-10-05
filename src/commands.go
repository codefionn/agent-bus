package bus

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

func cmdRegister(args []string) {
	o := joinOpts{meta: map[string]string{}}
	if i := slices.Index(args, "--note"); i >= 0 {
		o.note = strings.Join(args[i+1:], " ")
		args = args[:i]
	}
	for i := 0; i < len(args); {
		switch args[i] {
		case "--id", "--meta", "--deliver":
		default:
			i++
			continue
		}
		if i+1 >= len(args) {
			die(2, "%s needs a value", args[i])
		}
		switch args[i] {
		case "--id":
			o.sessionID = args[i+1]
		case "--meta":
			parseMeta(o.meta, args[i+1])
		case "--deliver":
			o.delivery = args[i+1]
			if o.delivery != deliverHooks && o.delivery != deliverManual {
				die(2, "--deliver takes hooks or manual")
			}
		}
		args = slices.Delete(args, i, i+2)
	}
	var err error
	o.id, o.harness, o.owner, err = identity()
	if err != nil {
		die(1, "%v", err)
	}
	// The harness variable names this session only when AGENT_BUS_ID doesn't override it.
	if os.Getenv("AGENT_BUS_ID") == "" {
		o.defaultSessionID = os.Getenv(sessionVars[o.harness])
	}
	if len(args) > 0 {
		o.name = args[0]
		if !metaKey.MatchString(o.name) {
			die(1, "names may use letters, digits, '_', '.' and '-'")
		}
	}
	o.defaultDelivery = deliverManual
	o.cwd, _ = os.Getwd()
	var e *entry
	locked(func() {
		if e, _, err = join(o); err != nil {
			die(1, "%v", err)
		}
		os.Remove(leftPath(o.id))
	})
	fmt.Printf("registered as %s (%s %s, delivery %s)\n", e.Name, e.Harness, e.ID, e.Delivery)
}

// joinOpts describes a registration. Empty fields keep what an existing
// registration has, and fall back to the defaults for a new one.
type joinOpts struct {
	id, harness                 string
	owner                       *proc
	name                        string
	sessionID, defaultSessionID string
	note                        string
	meta                        map[string]string
	delivery, defaultDelivery   string
	cwd                         string
}

// join registers a session or updates its registration. Call with the lock held.
func join(o joinOpts) (e *entry, isNew bool, err error) {
	active := sessions()
	old := active[o.id]
	name := o.name
	switch {
	case name == "" && old != nil:
		name = old.Name
	case name == "":
		name = autoName(active, o.cwd, o.harness, o.id)
	}
	for _, other := range active {
		if other.Name == name && other.ID != o.id {
			return nil, false, fmt.Errorf("name %q is taken by %s session %s", name, other.Harness, other.ID)
		}
	}
	e = &entry{ID: o.id, Name: name, Harness: o.harness, SessionID: o.sessionID, Delivery: o.delivery,
		PID: o.owner.pid, Start: o.owner.start, Cwd: o.cwd, Note: o.note, Registered: now(), Seen: now()}
	if old != nil {
		e.Registered, e.Meta = old.Registered, old.Meta
		e.Note = cmp.Or(e.Note, old.Note)
		e.SessionID = cmp.Or(e.SessionID, old.SessionID)
		e.Delivery = cmp.Or(e.Delivery, old.Delivery)
	}
	e.SessionID = cmp.Or(e.SessionID, o.defaultSessionID)
	e.Delivery = cmp.Or(e.Delivery, o.defaultDelivery)
	for _, other := range active {
		if e.SessionID != "" && other.SessionID == e.SessionID && other.ID != o.id {
			return nil, false, fmt.Errorf("session id %q is taken by %s", e.SessionID, other.Name)
		}
	}
	e.Meta = applyMeta(e.Meta, o.meta)
	writeJSON(sessionPath(o.id), e)
	os.Mkdir(filepath.Join(inboxDir, o.id), 0o700)
	if old == nil {
		logEvent(newEvent("register", e))
	} else {
		logEvent(newEvent("update", e))
	}
	return e, old == nil, nil
}

// autoName picks a free name from the working directory: "agent-bus", then
// "agent-bus-7d54" when another session already has that.
func autoName(active map[string]*entry, cwd, harness, id string) string {
	base := strings.Trim(unsafeID.ReplaceAllString(filepath.Base(cwd), "-"), "-.")
	if base == "" || base == "." {
		base = harness
	}
	taken := func(n string) bool {
		for _, e := range active {
			if e.Name == n && e.ID != id {
				return true
			}
		}
		return false
	}
	name := base
	if taken(name) {
		name = fmt.Sprintf("%s-%.4s", base, cleanID(id))
	}
	for i := 2; taken(name); i++ {
		name = fmt.Sprintf("%s-%.4s-%d", base, cleanID(id), i)
	}
	return name
}

func cmdUnregister(args []string) {
	id, _, _, err := identity()
	if err != nil {
		die(1, "%v", err)
	}
	locked(func() {
		if e, ok := load[entry](sessionPath(id)); ok {
			logEvent(newEvent("unregister", e))
		}
		drop(id)
		markLeft(id)
	})
	poke(id) // a waiting `agent-bus wait` of this session returns
	fmt.Println("unregistered")
}

func cmdNote(args []string) {
	locked(func() {
		e := me(sessions(), true)
		e.Note = strings.Join(args, " ")
		writeJSON(sessionPath(e.ID), e)
		logEvent(newEvent("note", e))
	})
}

func checkSessionID(active map[string]*entry, id, sessionID string) {
	for _, other := range active {
		if sessionID != "" && other.SessionID == sessionID && other.ID != id {
			die(1, "session id %q is taken by %s", sessionID, other.Name)
		}
	}
}

var metaKey = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// parseMeta reads KEY=VALUE into m. An empty VALUE marks KEY for removal.
func parseMeta(m map[string]string, kv string) {
	k, v, ok := strings.Cut(kv, "=")
	if !ok || !metaKey.MatchString(k) {
		die(2, "metadata takes KEY=VALUE, with KEY made of letters, digits, '_', '.' and '-'")
	}
	m[k] = v
}

func applyMeta(cur, changes map[string]string) map[string]string {
	for k, v := range changes {
		if cur == nil {
			cur = map[string]string{}
		}
		if v == "" {
			delete(cur, k)
		} else {
			cur[k] = v
		}
	}
	if len(cur) == 0 {
		return nil
	}
	return cur
}

func nonNil(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func formatMeta(m map[string]string) string {
	keys := slices.Sorted(maps.Keys(m))
	for i, k := range keys {
		keys[i] = k + "=" + m[k]
	}
	return strings.Join(keys, " ")
}

// cmdID prints the session id the harness knows this session by, or records a new one.
func cmdID(args []string) {
	if len(args) > 1 {
		die(2, "usage: agent-bus id [SESSION]")
	}
	locked(func() {
		active := sessions()
		e := me(active, true)
		if len(args) == 0 {
			if e.SessionID == "" {
				die(1, "no session id set; run: agent-bus id SESSION")
			}
			fmt.Println(e.SessionID)
			return
		}
		checkSessionID(active, e.ID, args[0])
		e.SessionID = args[0]
		writeJSON(sessionPath(e.ID), e)
		logEvent(newEvent("id", e))
	})
}

// cmdMeta prints this session's metadata, or sets KEY=VALUE pairs (KEY= removes KEY).
func cmdMeta(args []string) {
	changes := map[string]string{}
	for _, a := range args {
		parseMeta(changes, a)
	}
	locked(func() {
		e := me(sessions(), true)
		if len(args) == 0 {
			for _, k := range slices.Sorted(maps.Keys(e.Meta)) {
				fmt.Printf("%s=%s\n", k, e.Meta[k])
			}
			return
		}
		e.Meta = applyMeta(e.Meta, changes)
		writeJSON(sessionPath(e.ID), e)
		logEvent(newEvent("meta", e))
	})
}

func cmdWhoami(args []string) {
	var e *entry
	locked(func() { e = me(sessions(), false) })
	if e != nil {
		fmt.Printf("%s (%s %s) in %s\n", e.Name, e.Harness, e.ID, e.Cwd)
		if e.SessionID != "" {
			fmt.Printf("    session %s\n", e.SessionID)
		}
		if len(e.Meta) > 0 {
			fmt.Printf("    meta %s\n", formatMeta(e.Meta))
		}
		return
	}
	id, harness, owner, err := identity()
	if err != nil {
		die(1, "%v", err)
	}
	fmt.Printf("not registered (%s %s, pid %d)\n", harness, id, owner.pid)
	os.Exit(1)
}

func age(seconds float64) string {
	s := int(seconds)
	switch {
	case s < 120:
		return fmt.Sprintf("%ds", s)
	case s < 7200:
		return fmt.Sprintf("%dm", s/60)
	}
	return fmt.Sprintf("%dh", s/3600)
}

func orDash(s *string) string {
	if s == nil {
		return "-"
	}
	return *s
}

func cmdList(args []string) {
	asJSON := slices.Contains(args, "--json")
	args = slices.DeleteFunc(slices.Clone(args), func(a string) bool { return a == "--json" })
	var active map[string]*entry
	locked(func() { active = sessions() })
	mine, _, _, _ := identity()
	found, _, rest, picked, err := scope(active, args)
	if err != nil {
		die(2, "%v", err)
	}
	if len(rest) > 0 {
		die(2, "usage: agent-bus list [--all | --dir DIR | --under DIR | --repo [DIR]] [--json]")
	}
	if !picked {
		found = byRegistration(active)
	}
	if asJSON {
		out := listing(found, mine)
		data, _ := json.MarshalIndent(out, "", " ")
		fmt.Println(string(data))
		return
	}
	t := now()
	for _, e := range found {
		marker := " "
		if e.ID == mine {
			marker = "*"
		}
		fmt.Printf("%s %-20s %-9s %-6s %-24s seen %4s ago  %s\n", marker, e.Name, e.Harness, cmp.Or(e.Delivery, deliverHooks), orDash(gitWhere(e.Cwd).Branch), age(t-e.Seen), e.Cwd)
		if e.SessionID != "" {
			fmt.Printf("    session %s\n", e.SessionID)
		}
		if len(e.Meta) > 0 {
			fmt.Printf("    meta %s\n", formatMeta(e.Meta))
		}
		if e.Note != "" {
			fmt.Printf("    %s\n", e.Note)
		}
	}
	if len(found) == 0 {
		fmt.Println("no registered sessions")
	}
}

func cmdSend(args []string) {
	var recipients []*entry
	locked(func() {
		active := sessions()
		sender := me(active, true)
		var label *string
		hits, l, rest, picked, err := scope(active, args)
		if err != nil {
			die(2, "%v", err)
		}
		switch {
		case picked:
			label, args = &l, rest
			for _, e := range hits {
				if e.ID != sender.ID {
					recipients = append(recipients, e)
				}
			}
		case len(args) > 0:
			r, err := resolve(active, args[0])
			if err != nil {
				die(1, "%v", err)
			}
			if r.ID == sender.ID {
				die(1, "that is you")
			}
			recipients, args = []*entry{r}, args[1:]
		}
		if len(args) == 0 {
			die(2, "usage: agent-bus send (NAME | --all | --dir DIR | --under DIR | --repo [DIR]) MESSAGE...")
		}
		deliver(sender, recipients, label, strings.Join(args, " "))
	})
	names := make([]string, len(recipients))
	for i, r := range recipients {
		names[i] = r.Name
		poke(r.ID)
	}
	if len(names) == 0 {
		fmt.Println("sent to nobody")
		os.Exit(1)
	}
	fmt.Println("sent to " + strings.Join(names, ", "))
}

// cmdInbox prints the messages this session has not received yet and marks them received.
func cmdInbox(args []string) {
	var msgs []*message
	locked(func() { msgs = take(me(sessions(), true), slices.Contains(args, "--peek")) })
	switch {
	case slices.Contains(args, "--json"):
		printJSON(nonNilMsgs(msgs))
	case len(msgs) == 0:
		fmt.Println("no new messages")
	default:
		fmt.Println(render(msgs))
	}
}

func printJSON(v any) {
	data, _ := json.MarshalIndent(v, "", " ")
	fmt.Println(string(data))
}

func nonNilMsgs(msgs []*message) []*message {
	if msgs == nil {
		return []*message{}
	}
	return msgs
}

// Exit codes of wait besides 0 (messages arrived).
const (
	exitTimeout      = 1
	exitUnsubscribed = 3
)

// cmdWait blocks until the session gets messages, which it prints and marks
// received, or until the session leaves the bus. It is meant to run in the
// background: the agent hears about it when it returns.
//
// It sleeps on the session's wake endpoint (a Unix socket, or a named pipe on
// Windows). send pokes it after delivering, and unregister after leaving. It
// rechecks every few seconds anyway, which also notices a harness that exited.
func cmdWait(args []string) {
	asJSON := slices.Contains(args, "--json")
	var deadline time.Time
	if i := slices.Index(args, "--timeout"); i >= 0 && i+1 < len(args) {
		secs, err := strconv.ParseFloat(args[i+1], 64)
		if err != nil {
			die(2, "--timeout needs a number of seconds")
		}
		deadline = time.Now().Add(time.Duration(secs * float64(time.Second)))
	}
	var e *entry
	locked(func() { e = me(sessions(), false) })
	if e == nil {
		finishWait(asJSON, "unsubscribed", nil, "not registered; run: agent-bus register NAME")
	}
	recheck := 10 * time.Second
	wake, stop, err := listenWake(e.ID)
	if err != nil {
		recheck = time.Second
	}
	defer stop()
	for {
		var msgs []*message
		gone := false
		locked(func() {
			cur := sessions()[e.ID]
			if cur == nil {
				gone = true
				return
			}
			cur.Seen = now() // a session that waits is active
			writeJSON(sessionPath(cur.ID), cur)
			msgs = take(cur, false)
		})
		switch {
		case gone:
			stop()
			finishWait(asJSON, "unsubscribed", nil, e.Name+" left the bus")
		case len(msgs) > 0:
			stop()
			finishWait(asJSON, "message", msgs, render(msgs))
		}
		pause := recheck
		if !deadline.IsZero() {
			left := time.Until(deadline)
			if left <= 0 {
				stop()
				finishWait(asJSON, "timeout", nil, "no new messages")
			}
			pause = min(pause, left)
		}
		select {
		case <-wake:
		case <-time.After(pause):
		}
	}
}

func finishWait(asJSON bool, status string, msgs []*message, text string) {
	if asJSON {
		printJSON(map[string]any{"status": status, "messages": nonNilMsgs(msgs)})
	} else {
		fmt.Println(text)
	}
	switch status {
	case "timeout":
		os.Exit(exitTimeout)
	case "unsubscribed":
		os.Exit(exitUnsubscribed)
	}
	os.Exit(0)
}

// cmdHook is the entry point for harness hooks and plugins. It reads the hook
// payload on stdin and answers in Claude Code's hook output format.
//
//   - SessionStart registers the session when auto-register is on (hooks mode)
//     and tells the agent its bus name.
//   - SessionEnd takes the session off the bus only when another session
//     replaces it in the same harness (Claude Code's /clear and /resume).
//     Otherwise the session stays until its harness process exits, so a
//     harness that fires SessionEnd while it keeps running still gets messages.
//   - Any other event follows the session into its current directory and hands
//     unread messages to the model, if the session uses hooks delivery. It
//     registers a session the SessionStart hook missed, for example because the
//     hook was added after the session started, unless the session left the
//     bus with agent-bus unregister.
//
// It stays silent when there is nothing to say.
func cmdHook(args []string) {
	var payload struct {
		Event     string `json:"hook_event_name"`
		SessionID string `json:"session_id"`
		Cwd       string `json:"cwd"`
		Reason    string `json:"reason"`
	}
	data, _ := io.ReadAll(os.Stdin)
	json.Unmarshal(data, &payload)
	if payload.Event == "" {
		payload.Event = "PostToolUse"
	}
	// Hook processes may not inherit the session variables, so try the session
	// the payload names first, then the one the environment and process tree name.
	var candidates []string
	if payload.SessionID != "" && os.Getenv("AGENT_BUS_ID") == "" {
		candidates = append(candidates, cleanID(payload.SessionID))
	}
	if id, _, _, err := identity(); err == nil {
		candidates = append(candidates, id)
	}
	id := ""
	for _, c := range candidates {
		if _, err := os.Stat(sessionPath(c)); err == nil {
			id = c
			break
		}
	}

	switch payload.Event {
	case "SessionStart":
		hookSessionStart(id, candidates, payload.SessionID, payload.Cwd)
		return
	case "SessionEnd":
		if id != "" && (payload.Reason == "clear" || payload.Reason == "resume") {
			locked(func() {
				if e, ok := load[entry](sessionPath(id)); ok {
					logEvent(newEvent("unregister", e))
				}
				drop(id)
			})
		}
		return
	}
	if id == "" {
		if len(candidates) > 0 && !fileExists(leftPath(candidates[0])) && autoRegister() {
			hookJoin(payload.Event, "", candidates, payload.SessionID, payload.Cwd)
		}
		return
	}
	os.Setenv("AGENT_BUS_ID", id)
	e, ok := load[entry](sessionPath(id))
	if !ok {
		return
	}
	moved := payload.Cwd != "" && payload.Cwd != e.Cwd
	deliver := e.Delivery != deliverManual && len(inboxFiles(id)) > 0
	if !moved && !deliver {
		return
	}
	var msgs []*message
	locked(func() {
		e := me(sessions(), false)
		if e == nil {
			return
		}
		// Follow the session into the directory it works in now.
		if moved {
			e.Cwd = payload.Cwd
			writeJSON(sessionPath(id), e)
			logEvent(newEvent("move", e))
		}
		if e.Delivery != deliverManual {
			msgs = take(e, false)
		}
	})
	if len(msgs) > 0 {
		hookOutput(payload.Event, "Messages from other agent sessions (reply with agent-bus send NAME ...):\n"+render(msgs))
	}
}

func hookSessionStart(id string, candidates []string, sessionID, cwd string) {
	if id == "" && !autoRegister() {
		return
	}
	hookJoin("SessionStart", id, candidates, sessionID, cwd)
}

// hookJoin registers the session of a hook, or refreshes its registration,
// and tells the agent its bus name in the output of event.
func hookJoin(event, id string, candidates []string, sessionID, cwd string) {
	harness, owner := findHarness()
	if owner == nil {
		return
	}
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	o := joinOpts{harness: harness, owner: owner, cwd: cwd, defaultDelivery: deliverHooks, defaultSessionID: sessionID}
	switch {
	case id != "":
		o.id = id
	case len(candidates) > 0:
		o.id = candidates[0]
	default:
		return
	}
	var e *entry
	var isNew bool
	var msgs []*message
	locked(func() {
		var err error
		if e, isNew, err = join(o); err != nil {
			e = nil
			return
		}
		os.Remove(leftPath(o.id))
		if e.Delivery != deliverManual {
			msgs = take(e, false)
		}
	})
	if e == nil {
		return
	}
	verb := "You are"
	if isNew {
		verb = "This session joined the agent bus automatically. You are"
	}
	var text string
	if e.Delivery == deliverManual {
		text = fmt.Sprintf("%s on the agent bus as %s, with manual delivery: fetch messages with `agent-bus inbox`, "+
			"or run `agent-bus wait` in the background to hear about the next one.", verb, e.Name)
	} else {
		text = fmt.Sprintf("%s on the agent bus as %s. Messages from other agent sessions on this machine arrive here on their own. "+
			"`agent-bus list` shows who is active, `agent-bus send NAME MESSAGE` writes to one, "+
			"and `agent-bus note TEXT` tells the others what you work on. "+
			"When you expect a reply or handoff, run `agent-bus wait --timeout 7200` in the background and keep the turn active. "+
			"Give peers at least two hours unless the user sets a shorter deadline. Monitor the wait with short tool calls "+
			"and restart it on timeout while the reply is still needed and the peer is active.", verb, e.Name)
	}
	if len(msgs) > 0 {
		text += "\n\nMessages from other agent sessions (reply with agent-bus send NAME ...):\n" + render(msgs)
	}
	hookOutput(event, text)
}

func hookOutput(event, text string) {
	data, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]string{
		"hookEventName":     event,
		"additionalContext": text,
	}})
	fmt.Println(string(data))
}
