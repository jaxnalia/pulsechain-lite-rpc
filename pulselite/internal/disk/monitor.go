// Package disk measures on-disk usage of the execution data directory (and
// optionally other watched directories such as the consensus datadir) and
// enforces soft and hard limits.
package disk

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"time"

	"github.com/ethereum/go-ethereum/log"
	"github.com/pulsechainlite/pulselite/internal/metrics"
)

// Usage returns the total size in bytes of all regular files below dir.
func Usage(dir string) uint64 {
	var total uint64
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // files may disappear while walking (compaction, rotation)
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				total += uint64(info.Size())
			}
		}
		return nil
	})
	return total
}

// Monitor periodically measures disk usage.
type Monitor struct {
	ExecDir    string
	Watch      []string
	Interval   time.Duration
	ExecLimit  uint64 // hard limit for ExecDir
	TotalLimit uint64 // hard limit for ExecDir + Watch
	FailHard   bool

	// OnSoftLimit is invoked when execution usage crosses 80% of ExecLimit,
	// giving the cache a chance to evict aggressively.
	OnSoftLimit func()
	// OnHardLimit is invoked when a hard limit is crossed and FailHard is set.
	OnHardLimit func(reason string)
}

// Sample is one measurement.
type Sample struct {
	Execution uint64
	Watched   uint64
}

// Measure takes one sample and updates metrics.
func (m *Monitor) Measure() Sample {
	s := Sample{Execution: Usage(m.ExecDir)}
	for _, d := range m.Watch {
		s.Watched += Usage(d)
	}
	metrics.DiskExecution.Set(float64(s.Execution))
	metrics.DiskWatched.Set(float64(s.Watched))
	metrics.DiskTotal.Set(float64(s.Execution + s.Watched))
	return s
}

// Run measures until ctx is cancelled.
func (m *Monitor) Run(ctx context.Context) {
	t := time.NewTicker(m.Interval)
	defer t.Stop()
	for {
		m.check(m.Measure())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (m *Monitor) check(s Sample) {
	total := s.Execution + s.Watched
	log.Info("Disk usage", "execution", gb(s.Execution), "watched", gb(s.Watched), "total", gb(total),
		"execLimit", gb(m.ExecLimit), "totalLimit", gb(m.TotalLimit))
	if m.ExecLimit > 0 && s.Execution > m.ExecLimit*8/10 && m.OnSoftLimit != nil {
		log.Warn("Execution disk usage above soft limit, evicting cache", "usage", gb(s.Execution))
		m.OnSoftLimit()
	}
	reason := ""
	if m.ExecLimit > 0 && s.Execution > m.ExecLimit {
		reason = "execution data directory exceeds disk.limit"
	}
	if m.TotalLimit > 0 && total > m.TotalLimit {
		reason = "execution + consensus disk usage exceeds disk.total-limit"
	}
	if reason != "" {
		log.Error("Hard disk limit exceeded", "reason", reason, "execution", gb(s.Execution), "total", gb(total))
		if m.FailHard && m.OnHardLimit != nil {
			m.OnHardLimit(reason)
		}
	}
}

func gb(b uint64) string {
	return fmt.Sprintf("%.3fGB", float64(b)/1e9)
}
