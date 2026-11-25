package verified

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	gomath "math"
	"regexp"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/trie"
)

// StateDB wraps some trie errors with %v, losing the type, so fall back to
// parsing trie.MissingNodeError's canonical string form.
var missingNodeRe = regexp.MustCompile(`missing trie node ([0-9a-f]{64})(?: \(owner ([0-9a-f]{64})\))? \(path ([0-9a-f]*)\)`)

// ParseMissingNode extracts a MissingNodeError from err (typed or textual).
func ParseMissingNode(err error) (*trie.MissingNodeError, bool) {
	if err == nil {
		return nil, false
	}
	var mn *trie.MissingNodeError
	if errors.As(err, &mn) {
		return mn, true
	}
	m := missingNodeRe.FindStringSubmatch(err.Error())
	if m == nil {
		return nil, false
	}
	path, perr := hex.DecodeString(m[3])
	if perr != nil {
		return nil, false
	}
	out := &trie.MissingNodeError{NodeHash: common.HexToHash(m[1]), Path: path}
	if m[2] != "" {
		out.Owner = common.HexToHash(m[2])
	}
	return out, true
}

// hasNibblePrefix reports whether the nibbles of h start with path.
func hasNibblePrefix(h []byte, path []byte) bool {
	for i, nib := range path {
		b := h[i/2]
		var got byte
		if i%2 == 0 {
			got = b >> 4
		} else {
			got = b & 0x0f
		}
		if got != nib {
			return false
		}
	}
	return true
}

// findPreimage searches for a key of the given width (20 for addresses, 32
// for storage slots) whose keccak hash starts with the nibble path. Expected
// work is 16^len(path) hashes, so the depth is bounded by SiblingDepth.
func (s *Source) findPreimage(owner common.Hash, path []byte, width int) ([]byte, bool) {
	cacheKey := string(owner.Bytes()) + string(path)
	if v, ok := s.preimages.Load(cacheKey); ok {
		return v.([]byte), true
	}
	if len(path) > s.SiblingDepth {
		return nil, false
	}
	workers := runtime.NumCPU()
	limit := uint64(64) << (4 * uint(len(path))) // 64x the expected attempts
	// The smallest matching counter is chosen so the search is deterministic
	// (same key, hence same upstream request, on every run).
	var (
		best atomic.Uint64
		wg   sync.WaitGroup
	)
	best.Store(gomath.MaxUint64)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(start uint64) {
			defer wg.Done()
			key := siblingKey(width, 0)
			hasher := crypto.NewKeccakState()
			var out [32]byte
			for i := start; i < limit && i < best.Load(); i += uint64(workers) {
				binary.BigEndian.PutUint64(key[width-8:], i)
				hasher.Reset()
				hasher.Write(key)
				hasher.Read(out[:])
				if hasNibblePrefix(out[:], path) {
					for {
						cur := best.Load()
						if i >= cur || best.CompareAndSwap(cur, i) {
							break
						}
					}
					return
				}
			}
		}(uint64(w))
	}
	wg.Wait()
	if best.Load() == gomath.MaxUint64 {
		return nil, false
	}
	result := siblingKey(width, best.Load())
	s.preimages.Store(cacheKey, result)
	return result, true
}

// siblingKey builds the candidate key for counter i. A fixed marker keeps
// generated keys away from small "real" slots.
func siblingKey(width int, i uint64) []byte {
	key := make([]byte, width)
	copy(key, []byte("pulselite-sibling"))
	binary.BigEndian.PutUint64(key[width-8:], i)
	return key
}
