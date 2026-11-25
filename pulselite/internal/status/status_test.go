package status

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// readySnapshot is a node that has been up for an hour and is fully ready.
func readySnapshot(now time.Time) *Snapshot {
	s := &Snapshot{Head: &Block{Number: 1000, AgeSec: 5}, NetworkHead: 1000, UptimeSec: 3600}
	s.P2P = &struct {
		Eth  int `json:"eth"`
		Snap int `json:"snap"`
	}{Eth: 10, Snap: 6}
	s.Consensus.Reachable = true
	s.Consensus.IsOptimistic = true // expected in rpc mode
	s.Probe = Probe{Block: 999, OK: true, LatencyMs: 120, At: now.Unix(), LastOK: now.Unix()}
	return s
}

func justStarted(s *Snapshot) { s.UptimeSec = 20 }

func TestEvaluate(t *testing.T) {
	svc := &Service{}
	now := time.Now()
	cases := []struct {
		name   string
		mutate func(*Snapshot)
		ready  bool
		state  string
		check  string // id of the check expected in checkState
		cState string
	}{
		{"all good", func(*Snapshot) {}, true, "ready", "state", CheckOK},
		{"rpc upstream mode", func(s *Snapshot) { s.P2P = nil }, true, "ready", "head", CheckOK},

		// Still working: spinner, not red.
		{"no head yet after start", func(s *Snapshot) { justStarted(s); s.Head = nil }, false, "starting", "head", CheckPending},
		{"beacon starting", func(s *Snapshot) { justStarted(s); s.Consensus.Reachable = false }, false, "starting", "consensus", CheckPending},
		{"beacon syncing", func(s *Snapshot) { s.Consensus.IsSyncing = true; s.Consensus.SyncDistance = 500 }, false, "syncing", "consensus", CheckPending},
		{"beacon restarting long after start", func(s *Snapshot) { s.Consensus.Reachable = false; s.Consensus.DownSec = 25 }, false, "starting", "consensus", CheckPending},
		{"beacon syncing slowly but progressing", func(s *Snapshot) {
			s.Consensus.IsSyncing = true
			s.Consensus.SyncDistance = 5000
			s.Consensus.StalledSec = 20
		}, false, "syncing", "consensus", CheckPending},
		{"head lagging", func(s *Snapshot) { s.NetworkHead = 1010 }, false, "syncing", "head", CheckPending},
		{"peers connecting", func(s *Snapshot) { justStarted(s); s.P2P.Snap = 0 }, false, "syncing", "peers", CheckPending},
		{"first read in progress", func(s *Snapshot) { justStarted(s); s.Probe = Probe{} }, false, "syncing", "state", CheckPending},
		{"read retrying", func(s *Snapshot) { s.Probe.OK = false; s.Probe.Error = "timeout"; s.Probe.LastOK = now.Unix() - 30 }, false, "syncing", "state", CheckPending},
		{"read at older head", func(s *Snapshot) { s.Probe.Block = 900 }, false, "syncing", "state", CheckPending},

		// Stopped working: red.
		{"beacon down for minutes", func(s *Snapshot) { s.Consensus.Reachable = false; s.Consensus.DownSec = 600 }, false, "degraded", "consensus", CheckFail},
		{"beacon sync stalled", func(s *Snapshot) {
			s.Consensus.IsSyncing = true
			s.Consensus.SyncDistance = 500
			s.Consensus.StalledSec = 600
		}, false, "degraded", "consensus", CheckFail},
		{"no head long after start", func(s *Snapshot) { s.Head = nil }, false, "degraded", "head", CheckFail},
		{"head stalled", func(s *Snapshot) { s.Head.AgeSec = 600 }, false, "degraded", "head", CheckFail},
		{"no snap peers", func(s *Snapshot) { s.P2P.Snap = 0 }, false, "degraded", "peers", CheckFail},
		{"reads failing for minutes", func(s *Snapshot) { s.Probe.OK = false; s.Probe.Error = "timeout"; s.Probe.LastOK = now.Unix() - 600 }, false, "degraded", "state", CheckFail},
	}
	for _, c := range cases {
		s := readySnapshot(now)
		c.mutate(s)
		svc.evaluate(s, now)
		if s.Ready != c.ready || s.State != c.state {
			t.Errorf("%s: ready=%v state=%s, want ready=%v state=%s (%s)", c.name, s.Ready, s.State, c.ready, c.state, s.Summary)
		}
		for _, ch := range s.Checks {
			if ch.ID == c.check && ch.State != c.cState {
				t.Errorf("%s: check %s state=%s, want %s (%s)", c.name, ch.ID, ch.State, c.cState, ch.Detail)
			}
		}
	}
}

func TestWrapRouting(t *testing.T) {
	rpc := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("rpc")) })
	h := (&Service{}).Wrap(rpc)
	do := func(method, path, accept string) string {
		req := httptest.NewRequest(method, path, strings.NewReader(`{}`))
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Body.String()
	}
	if got := do("GET", "/", "text/html,application/xhtml+xml"); !strings.Contains(got, "<title>PulseChain Lite RPC</title>") {
		t.Fatal("browser GET / should serve the dashboard")
	}
	if got := do("GET", "/", ""); got != "rpc" {
		t.Fatalf("plain GET / should reach the RPC handler, got %q", got)
	}
	if got := do("POST", "/", "text/html"); got != "rpc" {
		t.Fatalf("POST should reach the RPC handler, got %q", got)
	}
}
