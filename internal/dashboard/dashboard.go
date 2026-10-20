// Package dashboard serves a read-only live web view of lookahead-managed pools.
package dashboard

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/avanishkasar/lookahead/internal/inspect"
)

//go:embed index.html
var indexHTML []byte

// Config configures the dashboard.
type Config struct {
	Namespace string
	// Pool limits the view to one pool; empty shows every pool lookahead manages.
	Pool     string
	Interval time.Duration
	Window   time.Duration
	// Now is the clock; time.Now if nil.
	Now func() time.Time
}

// PoolState is one pool in the /api/state response.
type PoolState struct {
	Brief    string            `json:"brief"`
	Snapshot *inspect.Snapshot `json:"snapshot"`
	Samples  []inspect.Sample  `json:"samples"`
	Claims   []inspect.Claim   `json:"claims"`
	Hints    []inspect.Hint    `json:"hints"`
}

// State is the /api/state response.
type State struct {
	Namespace string      `json:"namespace"`
	Now       time.Time   `json:"now"`
	WindowSec int         `json:"windowSeconds"`
	Error     string      `json:"error,omitempty"`
	Pools     []PoolState `json:"pools"`
}

// Server samples the cluster on an interval and serves the page and its data.
type Server struct {
	c   client.Reader
	cfg Config

	mu      sync.Mutex
	history map[string]*inspect.History
	latest  map[string]*inspect.Snapshot
	lastErr string
}

// New returns a dashboard backed by c. Call Run to start sampling.
func New(c client.Reader, cfg Config) *Server {
	if cfg.Interval <= 0 {
		cfg.Interval = time.Second
	}
	if cfg.Window <= 0 {
		cfg.Window = inspect.ClaimWindow
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Server{c: c, cfg: cfg, history: map[string]*inspect.History{}, latest: map[string]*inspect.Snapshot{}}
}

// Run samples until ctx is done.
func (s *Server) Run(ctx context.Context) {
	t := time.NewTicker(s.cfg.Interval)
	defer t.Stop()
	for {
		s.Sample(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Sample takes one observation of every pool in view.
func (s *Server) Sample(ctx context.Context) {
	pools := []string{s.cfg.Pool}
	if s.cfg.Pool == "" {
		var err error
		if pools, err = inspect.Pools(ctx, s.c, s.cfg.Namespace); err != nil {
			s.fail(ctx, err)
			return
		}
	}
	now := s.cfg.Now()
	snaps := make(map[string]*inspect.Snapshot, len(pools))
	for _, p := range pools {
		snap, err := inspect.Collect(ctx, s.c, s.cfg.Namespace, p, now)
		if err != nil {
			s.fail(ctx, err)
			return
		}
		snaps[p] = snap
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastErr = ""
	s.latest = snaps
	for p, snap := range snaps {
		h, ok := s.history[p]
		if !ok {
			h = inspect.NewHistory(s.cfg.Window)
			s.history[p] = h
		}
		h.Observe(snap)
	}
}

func (s *Server) fail(ctx context.Context, err error) {
	log.FromContext(ctx).Error(err, "Dashboard sample failed")
	s.mu.Lock()
	s.lastErr = err.Error()
	s.mu.Unlock()
}

// State returns the current view.
func (s *Server) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := State{Namespace: s.cfg.Namespace, Now: s.cfg.Now(), WindowSec: int(s.cfg.Window.Seconds()), Error: s.lastErr, Pools: []PoolState{}}
	names := make([]string, 0, len(s.latest))
	for n := range s.latest {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		snap, h := s.latest[n], s.history[n]
		st.Pools = append(st.Pools, PoolState{Brief: inspect.Brief(snap), Snapshot: snap,
			Samples: h.Samples(), Claims: h.Claims(), Hints: h.Hints()})
	}
	return st
}

// Handler serves the page at / and its data at /api/state.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'")
		_, _ = w.Write(indexHTML)
	})
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(s.State())
	})
	return mux
}
