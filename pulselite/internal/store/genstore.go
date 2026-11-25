// Package store implements the bounded verified-state cache.
//
// GenStore is an ethdb.KeyValueStore made of two pebble "generations". Writes
// go to the current generation; reads check current then previous, and hits
// in the previous generation are promoted (copied) into the current one. When
// the current generation reaches half of the byte cap, the previous
// generation directory is deleted outright and the current one becomes the
// previous. This is a segmented LRU approximation with O(1) memory overhead
// and immediate disk reclamation (no compaction debt), bounding the logical
// cache size to the configured cap.
//
// Everything stored here is content addressed (trie nodes by hash, code by
// code hash), so evicting any entry is always safe: a later miss simply
// triggers a verified re-fetch.
package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/ethdb/pebble"
	"github.com/ethereum/go-ethereum/log"
	"github.com/pulsechainlite/pulselite/internal/metrics"
)

var errNotFound = errors.New("not found")

const genPrefix = "gen-"

// GenStore is a size-capped two-generation key-value store.
type GenStore struct {
	dir      string
	capBytes uint64
	cacheMB  int

	mu       sync.RWMutex
	cur      *pebble.Database
	prev     *pebble.Database
	curID    uint64
	prevID   uint64
	curBytes atomic.Uint64
	prvBytes atomic.Uint64
	closed   bool
}

// OpenGenStore opens (or creates) the cache in dir with the given byte cap.
func OpenGenStore(dir string, capBytes uint64, cacheMB int) (*GenStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	g := &GenStore{dir: dir, capBytes: capBytes, cacheMB: cacheMB}
	ids, err := g.listGenerations()
	if err != nil {
		return nil, err
	}
	// Keep at most the two newest generations; anything else is stale.
	for len(ids) > 2 {
		os.RemoveAll(g.genDir(ids[0]))
		ids = ids[1:]
	}
	switch len(ids) {
	case 0:
		g.curID = 1
	case 1:
		g.curID = ids[0]
	case 2:
		g.prevID, g.curID = ids[0], ids[1]
	}
	if g.cur, err = g.openGen(g.curID); err != nil {
		return nil, err
	}
	if g.prevID != 0 {
		if g.prev, err = g.openGen(g.prevID); err != nil {
			g.cur.Close()
			return nil, err
		}
		g.prvBytes.Store(dirSize(g.genDir(g.prevID)))
	}
	g.curBytes.Store(dirSize(g.genDir(g.curID)))
	g.updateGauge()
	g.maybeRotate()
	return g, nil
}

func (g *GenStore) genDir(id uint64) string {
	return filepath.Join(g.dir, fmt.Sprintf("%s%08d", genPrefix, id))
}

func (g *GenStore) listGenerations() ([]uint64, error) {
	entries, err := os.ReadDir(g.dir)
	if err != nil {
		return nil, err
	}
	var ids []uint64
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), genPrefix) {
			continue
		}
		id, err := strconv.ParseUint(strings.TrimPrefix(e.Name(), genPrefix), 10, 64)
		if err == nil {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
}

func (g *GenStore) openGen(id uint64) (*pebble.Database, error) {
	return pebble.New(g.genDir(id), g.cacheMB, 64, "", false)
}

func dirSize(dir string) uint64 {
	var total uint64
	filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			total += uint64(info.Size())
		}
		return nil
	})
	return total
}

func (g *GenStore) updateGauge() {
	metrics.CacheBytes.Set(float64(g.curBytes.Load() + g.prvBytes.Load()))
}

// LogicalBytes is the number of bytes accounted to both generations.
func (g *GenStore) LogicalBytes() uint64 { return g.curBytes.Load() + g.prvBytes.Load() }

// Cap returns the configured byte cap.
func (g *GenStore) Cap() uint64 { return g.capBytes }

// maybeRotate rotates generations if the current one is over half the cap.
func (g *GenStore) maybeRotate() {
	if g.curBytes.Load() < g.capBytes/2 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.curBytes.Load() >= g.capBytes/2 {
		g.rotateLocked()
	}
}

// Rotate drops the previous generation and starts a fresh current one.
func (g *GenStore) Rotate() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.rotateLocked()
}

func (g *GenStore) rotateLocked() {
	if g.closed {
		return
	}
	if g.prev != nil {
		g.prev.Close()
		// Deleting a large generation can be slow; never do it under the lock.
		go os.RemoveAll(g.genDir(g.prevID))
	}
	g.prev, g.prevID = g.cur, g.curID
	g.prvBytes.Store(g.curBytes.Load())
	g.curID++
	cur, err := g.openGen(g.curID)
	if err != nil {
		// Fall back to keeping the old generation as current.
		log.Error("Cache rotation failed", "err", err)
		g.cur, g.curID = g.prev, g.prevID
		g.prev, g.prevID = nil, 0
		g.prvBytes.Store(0)
		return
	}
	g.cur = cur
	g.curBytes.Store(0)
	metrics.CacheRotations.Inc()
	g.updateGauge()
	log.Info("Cache generation rotated", "generation", g.curID, "capBytes", g.capBytes)
}

// Purge drops both generations (used when disk pressure is critical).
func (g *GenStore) Purge() {
	g.Rotate()
	g.Rotate()
}

// Has implements ethdb.KeyValueReader.
func (g *GenStore) Has(key []byte) (bool, error) {
	v, err := g.Get(key)
	return err == nil && v != nil, nil
}

// Get implements ethdb.KeyValueReader, promoting previous-generation hits.
func (g *GenStore) Get(key []byte) ([]byte, error) {
	g.mu.RLock()
	if g.closed {
		g.mu.RUnlock()
		return nil, errors.New("store closed")
	}
	if v, err := g.cur.Get(key); err == nil {
		g.mu.RUnlock()
		metrics.CacheHitCur.Inc()
		return v, nil
	}
	if g.prev != nil {
		if v, err := g.prev.Get(key); err == nil {
			g.mu.RUnlock()
			metrics.CacheHitPrev.Inc()
			g.Put(key, v)
			return v, nil
		}
	}
	g.mu.RUnlock()
	metrics.CacheMiss.Inc()
	return nil, errNotFound
}

// Put implements ethdb.KeyValueWriter.
func (g *GenStore) Put(key, value []byte) error {
	g.mu.RLock()
	if g.closed {
		g.mu.RUnlock()
		return errors.New("store closed")
	}
	err := g.cur.Put(key, value)
	g.mu.RUnlock()
	if err == nil {
		g.curBytes.Add(uint64(len(key) + len(value)))
		g.updateGauge()
		g.maybeRotate()
	}
	return err
}

// Delete implements ethdb.KeyValueWriter.
func (g *GenStore) Delete(key []byte) error {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.closed {
		return errors.New("store closed")
	}
	if g.prev != nil {
		g.prev.Delete(key)
	}
	return g.cur.Delete(key)
}

// DeleteRange implements ethdb.KeyValueRangeDeleter.
func (g *GenStore) DeleteRange(start, end []byte) error {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.prev != nil {
		g.prev.DeleteRange(start, end)
	}
	return g.cur.DeleteRange(start, end)
}

// Stat implements ethdb.KeyValueStater.
func (g *GenStore) Stat() (string, error) {
	return fmt.Sprintf("generations cur=%d prev=%d bytes=%d cap=%d", g.curID, g.prevID, g.LogicalBytes(), g.capBytes), nil
}

// SyncKeyValue implements ethdb.KeyValueSyncer.
func (g *GenStore) SyncKeyValue() error {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.cur.SyncKeyValue()
}

// Compact implements ethdb.Compacter.
func (g *GenStore) Compact(start, limit []byte) error {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.cur.Compact(start, limit)
}

// NewIterator implements ethdb.Iteratee over the current generation only.
func (g *GenStore) NewIterator(prefix, start []byte) ethdb.Iterator {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.cur.NewIterator(prefix, start)
}

// NewBatch implements ethdb.Batcher.
func (g *GenStore) NewBatch() ethdb.Batch { return &genBatch{g: g} }

// NewBatchWithSize implements ethdb.Batcher.
func (g *GenStore) NewBatchWithSize(int) ethdb.Batch { return &genBatch{g: g} }

// Close implements io.Closer.
func (g *GenStore) Close() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return nil
	}
	g.closed = true
	if g.prev != nil {
		g.prev.Close()
	}
	return g.cur.Close()
}

type kv struct {
	key, value []byte
	del        bool
}

type genBatch struct {
	g    *GenStore
	ops  []kv
	size int
}

func (b *genBatch) Put(key, value []byte) error {
	b.ops = append(b.ops, kv{key: clone(key), value: clone(value)})
	b.size += len(key) + len(value)
	return nil
}

func (b *genBatch) Delete(key []byte) error {
	b.ops = append(b.ops, kv{key: clone(key), del: true})
	b.size += len(key)
	return nil
}

func (b *genBatch) DeleteRange(start, end []byte) error {
	return b.g.DeleteRange(start, end)
}

func (b *genBatch) ValueSize() int { return b.size }

func (b *genBatch) Write() error {
	b.g.mu.RLock()
	if b.g.closed {
		b.g.mu.RUnlock()
		return errors.New("store closed")
	}
	batch := b.g.cur.NewBatch()
	var added uint64
	for _, op := range b.ops {
		if op.del {
			batch.Delete(op.key)
		} else {
			batch.Put(op.key, op.value)
			added += uint64(len(op.key) + len(op.value))
		}
	}
	err := batch.Write()
	b.g.mu.RUnlock()
	if err == nil {
		b.g.curBytes.Add(added)
		b.g.updateGauge()
		b.g.maybeRotate()
	}
	return err
}

func (b *genBatch) Reset() {
	b.ops = b.ops[:0]
	b.size = 0
}

func (b *genBatch) Replay(w ethdb.KeyValueWriter) error {
	for _, op := range b.ops {
		var err error
		if op.del {
			err = w.Delete(op.key)
		} else {
			err = w.Put(op.key, op.value)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func clone(b []byte) []byte {
	if b == nil {
		return nil
	}
	c := make([]byte, len(b))
	copy(c, b)
	return c
}
