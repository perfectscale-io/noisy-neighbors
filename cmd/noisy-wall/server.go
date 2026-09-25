package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/perfectscale-io/noisy-neighbors/internal/collect"
	"github.com/perfectscale-io/noisy-neighbors/internal/wall"
)

//go:embed assets
var assets embed.FS

// broker fans the latest frame out to every connected browser.
//
// It keeps the last frame so a browser that connects mid-run draws immediately
// rather than waiting for the next poll. On stage that is the difference
// between a blank screen and a picture while you are still talking.
type broker struct {
	mu   sync.RWMutex
	last *wall.Snapshot
	seq  int64
	subs map[chan *wall.Snapshot]struct{}

	// recent is a rolling log of transitions. Events belong to the frame they
	// were detected in, so a browser that opens after a kill would otherwise
	// show an empty rail: the kill already scrolled past. On stage that reads
	// as the demo having not worked.
	recent []wall.Event
}

// maxRecentEvents bounds the replay log. Long enough to cover a session, short
// enough that a reconnect does not dump a wall of history.
const maxRecentEvents = 60

func newBroker() *broker {
	return &broker{subs: map[chan *wall.Snapshot]struct{}{}}
}

func (b *broker) publish(s *wall.Snapshot) {
	b.mu.Lock()
	b.seq++
	s.Seq = b.seq
	// Transitions are computed here, once, against the frame we published
	// last, rather than per subscriber: the event rail must read the same on
	// every screen in the room.
	wall.Diff(b.last, s)
	b.last = s
	b.recent = append(b.recent, s.Events...)
	if n := len(b.recent) - maxRecentEvents; n > 0 {
		b.recent = b.recent[n:]
	}
	subs := make([]chan *wall.Snapshot, 0, len(b.subs))
	for ch := range b.subs {
		subs = append(subs, ch)
	}
	b.mu.Unlock()

	for _, ch := range subs {
		// Never block on a slow browser. A dropped frame is invisible; a
		// stalled publisher stops the wall for everyone.
		select {
		case ch <- s:
		default:
		}
	}
}

func (b *broker) latest() *wall.Snapshot {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.last
}

// catchUp is the latest frame with the rolling event log in place of that one
// frame's transitions, for a browser that has just connected. The client
// de-duplicates events by identity, so replaying them is safe.
func (b *broker) catchUp() *wall.Snapshot {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.last == nil {
		return nil
	}
	copy := *b.last
	copy.Events = append([]wall.Event(nil), b.recent...)
	return &copy
}

func (b *broker) subscribe() (chan *wall.Snapshot, func()) {
	ch := make(chan *wall.Snapshot, 4)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
		close(ch)
	}
}

type server struct {
	broker  *broker
	clients *collect.Clients

	allowButton  bool
	synthetic    bool
	buttonNS     string
	buttonDeploy string
	// showTenants is an allowlist for the simple view. Empty means every
	// non-system namespace.
	showTenants []string

	mu        sync.Mutex
	pressed   bool
	pressedAt time.Time
}

func (s *server) routes(mux *http.ServeMux) {
	// Bubbles are the front door. The bars say the same thing in one
	// dimension, which reads better for some rooms, and the console is a
	// keystroke past both.
	mux.HandleFunc("/", s.page("assets/bubbles.html"))
	mux.HandleFunc("/bars", s.page("assets/bars.html"))
	mux.HandleFunc("/detail", s.page("assets/detail.html"))
	mux.HandleFunc("/tenant", s.page("assets/tenant.html"))
	mux.Handle("/assets/", http.FileServer(http.FS(assets)))
	mux.HandleFunc("/api/stream", s.stream)
	mux.HandleFunc("/api/snapshot", s.snapshot)
	mux.HandleFunc("/api/button", s.button)
	mux.HandleFunc("/api/config", s.config)
}

func (s *server) page(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/", "/bars", "/detail", "/tenant":
		default:
			http.NotFound(w, r)
			return
		}
		body, err := assets.ReadFile(name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(body)
	}
}

func (s *server) snapshot(w http.ResponseWriter, r *http.Request) {
	snap := s.broker.catchUp()
	w.Header().Set("Content-Type", "application/json")
	if snap == nil {
		_, _ = w.Write([]byte(`{"pending":true}`))
		return
	}
	_ = json.NewEncoder(w).Encode(snap)
}

// stream is the server-sent-events feed the wall renders from.
func (s *server) stream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch, unsub := s.broker.subscribe()
	defer unsub()

	send := func(snap *wall.Snapshot) bool {
		payload, err := json.Marshal(snap)
		if err != nil {
			return true
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	if first := s.broker.catchUp(); first != nil {
		if !send(first) {
			return
		}
	}

	// A comment line every 20s keeps intermediaries from closing an idle
	// stream, which on a conference network happens more often than not.
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case snap := <-ch:
			if snap == nil || !send(snap) {
				return
			}
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// config tells the front end what this instance is scoped to. A real cluster
// carries namespaces the demo is not about -- the vendor's own exporter, other
// teams' workloads -- and a wall that lists the tool you are pitching as one of
// the misbehaving tenants is worse than no wall.
func (s *server) config(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"tenants": s.showTenants,
		"button":  s.allowButton,
	})
}

func (s *server) button(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	var err error
	switch r.URL.Query().Get("action") {
	case "reset":
		err = s.resetButton(ctx)
	default:
		err = s.pressButton(ctx)
	}

	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

func (s *server) press() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.pressed {
		s.pressed = true
		s.pressedAt = time.Now()
	}
}

func (s *server) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pressed = false
	s.pressedAt = time.Time{}
}

// sinceButton is seconds since the press, or -1 if it has not happened.
func (s *server) sinceButton() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.pressed {
		return -1
	}
	return time.Since(s.pressedAt).Seconds()
}

// pressButton scales the lab's aggressor up.
//
// Guarded by -allow-button so this binary cannot mutate anything unless that
// was asked for explicitly. The kubectl plugin stays read-only; this is the
// only write in the repository and it is one field on one lab deployment.
func (s *server) pressButton(ctx context.Context) error {
	if !s.allowButton {
		return errors.New("button disabled; restart with -allow-button")
	}
	if s.synthetic {
		s.press()
		return nil
	}
	if s.clients == nil {
		return errors.New("no cluster connection")
	}
	return s.scale(ctx, 1)
}

// resetButton scales the aggressor back to zero so a dry run can be repeated.
func (s *server) resetButton(ctx context.Context) error {
	if !s.allowButton {
		return errors.New("button disabled; restart with -allow-button")
	}
	if s.synthetic {
		s.reset()
		return nil
	}
	if s.clients == nil {
		return errors.New("no cluster connection")
	}
	return s.scale(ctx, 0)
}

func (s *server) scale(ctx context.Context, replicas int32) error {
	api := s.clients.Kube.AppsV1().Deployments(s.buttonNS)
	sc, err := api.GetScale(ctx, s.buttonDeploy, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reading scale of %s/%s: %w", s.buttonNS, s.buttonDeploy, err)
	}
	if sc.Spec.Replicas == replicas {
		return nil // pressing twice is not an error on stage
	}
	sc.Spec.Replicas = replicas
	if _, err := api.UpdateScale(ctx, s.buttonDeploy, sc, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("scaling %s/%s to %d: %w", s.buttonNS, s.buttonDeploy, replicas, err)
	}
	log.Printf("button: scaled %s/%s to %d", s.buttonNS, s.buttonDeploy, replicas)
	return nil
}
