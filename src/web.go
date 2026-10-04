package bus

import (
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed web/index.html
var indexHTML []byte

const webUsage = `agent-bus-web [--addr HOST:PORT] [--name NAME] [--open]

Serves a live view of the agent bus: who is registered, every registration,
note, metadata change and message, and a box to message sessions. It prints a
URL with a one-time auth token; anyone with the URL can read and send.

The server joins the bus as a session of harness "web", so agents can answer
with: agent-bus send NAME MESSAGE.

    --addr HOST:PORT   where to listen (default 127.0.0.1:0, a free port)
    --name NAME        bus name of the server (default web)
    --open             open the URL in the default browser`

const backlogSize = 1000

// hub fans events out to the connected browsers and keeps the most recent ones for new arrivals.
type hub struct {
	mu      sync.Mutex
	recent  []event
	clients map[chan event]struct{}
}

func (h *hub) publish(evs []event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recent = append(h.recent, evs...)
	if len(h.recent) > backlogSize {
		h.recent = slices.Clone(h.recent[len(h.recent)-backlogSize:])
	}
	for c := range h.clients {
		for _, ev := range evs {
			select {
			case c <- ev:
			default: // a browser that stopped reading misses events rather than stalling everyone
			}
		}
	}
}

// subscribe returns the backlog and a channel for what follows, with no gap between them.
func (h *hub) subscribe() ([]event, chan event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := make(chan event, 256)
	h.clients[c] = struct{}{}
	return slices.Clone(h.recent), c
}

func (h *hub) unsubscribe(c chan event) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
}

type webServer struct {
	hub      hub
	token    string
	name     string
	id       string
	loopback bool
	tail     *eventTail // positioned right after the backlog the hub starts with
}

// Web runs the agent-bus-web command line in args, without the program name.
func Web(args []string) {
	fs := flag.NewFlagSet("agent-bus-web", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprintln(os.Stderr, webUsage) }
	addr := fs.String("addr", "127.0.0.1:0", "")
	name := fs.String("name", "web", "")
	open := fs.Bool("open", false, "")
	fs.Parse(args)
	if !metaKey.MatchString(*name) {
		die(2, "names may use letters, digits, '_', '.' and '-'")
	}
	setup()

	l, err := net.Listen("tcp", *addr)
	if err != nil {
		die(1, "%v", err)
	}
	tok := make([]byte, 24)
	rand.Read(tok)
	s := &webServer{
		hub:   hub{clients: map[chan event]struct{}{}},
		token: hex.EncodeToString(tok),
		id:    cleanID(fmt.Sprintf("web-%d", os.Getpid())),
	}
	tcp := l.Addr().(*net.TCPAddr)
	s.loopback = tcp.IP.IsLoopback()
	s.register(*name)
	s.hub.recent, s.tail = recentEvents(backlogSize)

	host := tcp.IP.String()
	if tcp.IP.IsUnspecified() {
		host = "127.0.0.1"
	}
	url := fmt.Sprintf("http://%s/?token=%s", net.JoinHostPort(host, strconv.Itoa(tcp.Port)), s.token)
	fmt.Printf("agent-bus web on the bus as %s\n%s\n", s.name, url)
	if *open {
		openBrowser(url)
	}

	go s.follow()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		s.leave()
		os.Exit(0)
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /api/sessions", s.sessions)
	mux.HandleFunc("GET /api/events", s.events)
	mux.HandleFunc("POST /api/send", s.send)
	srv := &http.Server{Handler: s.guard(mux), ReadHeaderTimeout: 10 * time.Second}
	err = srv.Serve(l)
	s.leave()
	die(1, "%v", err)
}

// register puts the server on the bus, or back on it after it was pruned. Call without the lock.
func (s *webServer) register(name string) {
	cwd, _ := os.Getwd()
	locked(func() {
		active := sessions()
		for _, other := range active {
			if other.Name == name && other.ID != s.id {
				name = fmt.Sprintf("%s-%d", name, os.Getpid())
			}
		}
		e := &entry{ID: s.id, Name: name, Harness: "web", PID: os.Getpid(), Start: procStart(os.Getpid()), Cwd: cwd, Registered: now(), Seen: now()}
		writeJSON(sessionPath(s.id), e)
		os.Mkdir(filepath.Join(inboxDir, s.id), 0o700)
		watch(s.id)
		logEvent(newEvent("register", e))
	})
	s.name = name
}

func (s *webServer) leave() {
	locked(func() {
		if e, ok := load[entry](sessionPath(s.id)); ok {
			logEvent(newEvent("unregister", e))
		}
		drop(s.id)
	})
}

// follow tails the event log. Every bus command pokes the server's wake
// endpoint after it logs, so this sleeps until something happens. The ticker
// keeps the server's entry fresh and prunes sessions whose harness exited, so
// drops show up even when no agent runs a command.
func (s *webServer) follow() {
	wake, _, err := listenWake(s.id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-bus-web: no wake endpoint (%v); polling\n", err)
	}
	tick := time.NewTicker(10 * time.Second)
	poll := time.NewTicker(time.Second)
	if err == nil {
		poll.Stop()
	}
	for {
		select {
		case <-wake:
		case <-poll.C:
		case <-tick.C:
			s.housekeep()
		}
		if evs := s.tail.next(); len(evs) > 0 {
			s.hub.publish(evs)
		}
		if len(inboxFiles(s.id)) > 0 {
			// Messages to the server already show as send events; reading them keeps the inbox empty.
			locked(func() {
				if e := sessions()[s.id]; e != nil {
					take(e, false)
				}
			})
		}
	}
}

func (s *webServer) housekeep() {
	gone := false
	locked(func() {
		e := sessions()[s.id]
		if e == nil {
			gone = true
			return
		}
		e.Seen = now()
		writeJSON(sessionPath(s.id), e)
	})
	if gone {
		s.register(s.name)
	}
}

// guard checks the Host header against DNS rebinding and the token on every request.
func (s *webServer) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.loopback {
			host, _, err := net.SplitHostPort(r.Host)
			if err != nil {
				host = r.Host
			}
			if host != "localhost" && net.ParseIP(host) == nil {
				http.Error(w, "bad host", http.StatusForbidden)
				return
			}
		}
		if q := r.URL.Query().Get("token"); q != "" && s.valid(q) && r.Method == http.MethodGet && r.URL.Path == "/" {
			// Trade the token in the URL for a cookie, and drop it from the address bar.
			http.SetCookie(w, &http.Cookie{Name: "agent_bus_token", Value: s.token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if c, err := r.Cookie("agent_bus_token"); err == nil && tok == "" {
			tok = c.Value
		}
		if tok == "" {
			tok = r.URL.Query().Get("token")
		}
		if !s.valid(tok) {
			http.Error(w, "missing or wrong token; open the URL agent-bus-web printed", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *webServer) valid(tok string) bool {
	return subtle.ConstantTimeCompare([]byte(tok), []byte(s.token)) == 1
}

func (s *webServer) index(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src data:; style-src 'unsafe-inline'; script-src 'unsafe-inline'")
	w.Write(indexHTML)
}

func writeJSONResponse(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func (s *webServer) sessions(w http.ResponseWriter, r *http.Request) {
	var active map[string]*entry
	locked(func() { active = sessions() })
	writeJSONResponse(w, http.StatusOK, map[string]any{
		"self":     s.id,
		"sessions": listing(byRegistration(active), s.id),
	})
}

// events streams bus events as server-sent events. ?session=ID,NAME,... keeps
// only events from or to those sessions; ?backlog=N sends the last N first.
func (s *webServer) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	var only map[string]bool
	if q := r.URL.Query().Get("session"); q != "" {
		only = map[string]bool{}
		for _, v := range strings.Split(q, ",") {
			only[v] = true
		}
	}
	keep := func(ev event) bool {
		if only == nil || only[ev.ID] || only[ev.Name] || (ev.SessionID != "" && only[ev.SessionID]) {
			return true
		}
		return slices.ContainsFunc(ev.To, func(n string) bool { return only[n] }) ||
			slices.ContainsFunc(ev.ToIDs, func(n string) bool { return only[n] })
	}
	backlog := 200
	if n, err := strconv.Atoi(r.URL.Query().Get("backlog")); err == nil {
		backlog = max(0, n)
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	recent, c := s.hub.subscribe()
	defer s.hub.unsubscribe(c)
	recent = slices.DeleteFunc(recent, func(ev event) bool { return !keep(ev) })
	if len(recent) > backlog {
		recent = recent[len(recent)-backlog:]
	}
	write := func(ev event) {
		data, _ := json.Marshal(ev)
		fmt.Fprintf(w, "data: %s\n\n", data)
	}
	for _, ev := range recent {
		write(ev)
	}
	fmt.Fprint(w, "event: live\ndata: {}\n\n")
	flusher.Flush()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-c:
			if keep(ev) {
				write(ev)
				flusher.Flush()
			}
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// send delivers a message from the server's session. The body is JSON:
// {"to": NAME} or {"scope": "all"|"dir"|"under"|"repo", "dir": DIR}, plus "text".
func (s *webServer) send(w http.ResponseWriter, r *http.Request) {
	var req struct {
		To    string `json:"to"`
		Scope string `json:"scope"`
		Dir   string `json:"dir"`
		Text  string `json:"text"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil || strings.TrimSpace(req.Text) == "" {
		writeJSONResponse(w, http.StatusBadRequest, map[string]string{"error": "want JSON with text and to or scope"})
		return
	}
	var recipients []*entry
	var sendErr error
	locked(func() {
		active := sessions()
		sender := active[s.id]
		if sender == nil {
			sendErr = fmt.Errorf("the server dropped off the bus; retry in a few seconds")
			return
		}
		if req.Scope != "" {
			args := []string{"--" + req.Scope}
			if req.Dir != "" {
				args = append(args, req.Dir)
			}
			hits, label, _, ok, err := scope(active, args)
			if err != nil || !ok {
				sendErr = cmpErr(err, fmt.Errorf("unknown scope %q", req.Scope))
				return
			}
			for _, e := range hits {
				if e.ID != s.id {
					recipients = append(recipients, e)
				}
			}
			deliver(sender, recipients, &label, req.Text)
			return
		}
		e, err := resolve(active, req.To)
		if err != nil {
			sendErr = err
			return
		}
		recipients = []*entry{e}
		deliver(sender, recipients, nil, req.Text)
	})
	if sendErr != nil {
		writeJSONResponse(w, http.StatusBadRequest, map[string]string{"error": sendErr.Error()})
		return
	}
	names := []string{}
	for _, e := range recipients {
		names = append(names, e.Name)
		poke(e.ID)
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"sent_to": names})
}

func cmpErr(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "agent-bus-web: could not open a browser: %v\n", err)
	}
}
