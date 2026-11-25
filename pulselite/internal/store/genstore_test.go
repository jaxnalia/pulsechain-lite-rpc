package store

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"testing"
)

func key(i int) []byte {
	k := make([]byte, 32)
	binary.BigEndian.PutUint64(k[24:], uint64(i))
	return k
}

func value(n int) []byte {
	v := make([]byte, n)
	rand.Read(v)
	return v
}

const capBytes = 4 << 20

func TestCapEnforced(t *testing.T) {
	dir := t.TempDir()
	g, err := OpenGenStore(dir, capBytes, 16)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	// Write 6x the cap through both single puts and batches.
	for i := 0; i < 12000; i++ {
		if i%2 == 0 {
			g.Put(key(i), value(1000))
		} else {
			b := g.NewBatch()
			b.Put(key(i), value(1000))
			b.Write()
		}
		if lb := g.LogicalBytes(); lb > capBytes+2000 {
			t.Fatalf("logical bytes %d exceed cap %d at write %d", lb, capBytes, i)
		}
	}
	g.SyncKeyValue()
	if du := dirSize(dir); du > 2*capBytes {
		t.Fatalf("on-disk size %d exceeds 2x cap", du)
	}
	// The oldest keys must have been evicted, the newest retained.
	if _, err := g.Get(key(0)); err == nil {
		t.Fatal("oldest key survived 6x cap of writes")
	}
	if v, err := g.Get(key(11999)); err != nil || len(v) != 1000 {
		t.Fatal("newest key missing")
	}
}

func TestHotKeysSurviveRotation(t *testing.T) {
	g, err := OpenGenStore(t.TempDir(), capBytes, 16)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	hot := value(500)
	g.Put([]byte("hot"), hot)
	for i := 0; i < 20000; i++ {
		g.Put(key(i), value(1000))
		if i%500 == 0 { // regular access keeps it promoted
			if v, err := g.Get([]byte("hot")); err != nil || !bytes.Equal(v, hot) {
				t.Fatalf("hot key evicted at write %d", i)
			}
		}
	}
	if v, err := g.Get([]byte("hot")); err != nil || !bytes.Equal(v, hot) {
		t.Fatal("hot key evicted")
	}
}

func TestRestartDoesNotBloat(t *testing.T) {
	dir := t.TempDir()
	var sizes []uint64
	for cycle := 0; cycle < 8; cycle++ {
		g, err := OpenGenStore(dir, capBytes, 16)
		if err != nil {
			t.Fatal(err)
		}
		if cycle > 0 {
			// Data from the previous run must still be served after restart.
			if _, err := g.Get(key(cycle*3000 - 1)); err != nil {
				t.Fatalf("cycle %d: last key of previous run lost on restart", cycle)
			}
		}
		for i := 0; i < 3000; i++ {
			g.Put(key(cycle*3000+i), value(1000))
		}
		g.Close()
		sizes = append(sizes, dirSize(dir))
		ids, _ := (&GenStore{dir: dir}).listGenerations()
		if len(ids) > 2 {
			t.Fatalf("cycle %d: %d generations on disk", cycle, len(ids))
		}
	}
	for i, s := range sizes {
		if s > 2*capBytes {
			t.Fatalf("cycle %d: on-disk size %d exceeds 2x cap (%v)", i, s, sizes)
		}
	}
	t.Logf("on-disk size per restart cycle: %v", sizes)
}

func TestMissAndDelete(t *testing.T) {
	g, err := OpenGenStore(t.TempDir(), capBytes, 16)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if _, err := g.Get([]byte("nope")); err == nil {
		t.Fatal("missing key returned no error")
	}
	if ok, _ := g.Has([]byte("nope")); ok {
		t.Fatal("Has reported missing key")
	}
	g.Put([]byte("k"), []byte("v"))
	g.Rotate()
	g.Delete([]byte("k"))
	if _, err := g.Get([]byte("k")); err == nil {
		t.Fatal("deleted key still present in previous generation")
	}
	st, _ := g.Stat()
	if st == "" {
		t.Fatal("empty stat")
	}
	_ = fmt.Sprint(st)
}

func TestPurge(t *testing.T) {
	g, err := OpenGenStore(t.TempDir(), capBytes, 16)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	for i := 0; i < 100; i++ {
		g.Put(key(i), value(100))
	}
	g.Purge()
	if g.LogicalBytes() != 0 {
		t.Fatalf("purge left %d bytes", g.LogicalBytes())
	}
	if _, err := g.Get(key(1)); err == nil {
		t.Fatal("purged key still present")
	}
}
