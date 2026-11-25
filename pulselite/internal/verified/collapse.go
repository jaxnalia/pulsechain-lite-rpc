package verified

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb/memorydb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/pulsechainlite/pulselite/internal/metrics"
	"github.com/pulsechainlite/pulselite/internal/proof"
)

// PostState identifies the claimed post-state of the block being executed.
// It is used only to *locate* collapse siblings; every reconstructed node is
// accepted solely if it hashes to the reference held by the verified
// pre-state trie, so the claimed post-state is never trusted.
type PostState struct {
	Root common.Hash
	At   BlockRef
}

// keybytesToHex converts key bytes to nibbles (without terminator).
func keybytesToHex(b []byte) []byte {
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[2*i], out[2*i+1] = c/16, c%16
	}
	return out
}

// compactToHex decodes a compact (hex-prefix) key; leaf reports a terminator.
func compactToHex(compact []byte) (nibbles []byte, leaf bool) {
	if len(compact) == 0 {
		return nil, false
	}
	h := keybytesToHex(compact)
	flag := h[0]
	leaf = flag >= 2
	if flag&1 == 1 {
		return h[1:], leaf
	}
	return h[2:], leaf
}

// hexToCompact encodes nibbles in hex-prefix form.
func hexToCompact(nibbles []byte, leaf bool) []byte {
	flag := byte(0)
	if leaf {
		flag = 2
	}
	odd := len(nibbles)%2 == 1
	if odd {
		flag++
	}
	var buf []byte
	if odd {
		buf = append(buf, flag<<4|nibbles[0])
		nibbles = nibbles[1:]
	} else {
		buf = append(buf, flag<<4)
	}
	for i := 0; i < len(nibbles); i += 2 {
		buf = append(buf, nibbles[i]<<4|nibbles[i+1])
	}
	return buf
}

type rawNode struct {
	short bool
	key   []byte // nibbles (short nodes)
	leaf  bool
	val   []byte // raw RLP of the value element (short nodes)
	kids  [16][]byte
}

func decodeNode(blob []byte) (*rawNode, error) {
	elems, _, err := rlp.SplitList(blob)
	if err != nil {
		return nil, err
	}
	var items [][]byte
	for len(elems) > 0 {
		_, _, rest, err := rlp.Split(elems)
		if err != nil {
			return nil, err
		}
		items = append(items, elems[:len(elems)-len(rest)])
		elems = rest
	}
	switch len(items) {
	case 2:
		k, _, err := rlp.SplitString(items[0])
		if err != nil {
			return nil, err
		}
		key, leaf := compactToHex(k)
		return &rawNode{short: true, key: key, leaf: leaf, val: items[1]}, nil
	case 17:
		n := &rawNode{}
		for i := 0; i < 16; i++ {
			n.kids[i] = items[i]
		}
		return n, nil
	}
	return nil, fmt.Errorf("invalid node with %d items", len(items))
}

// childRef resolves a raw child reference to a node blob: either a 32-byte
// hash looked up in the proof, or an embedded node.
func childRef(ref []byte, proofNodes map[common.Hash][]byte) ([]byte, bool) {
	kind, content, _, err := rlp.Split(ref)
	if err != nil {
		return nil, false
	}
	if kind == rlp.List {
		return ref, true
	}
	if len(content) == 32 {
		n, ok := proofNodes[common.BytesToHash(content)]
		return n, ok
	}
	return nil, false
}

var errNoSibling = errors.New("sibling not reconstructible from post-state")

// reconstructSibling walks a post-state proof along keyHex and rebuilds the
// pre-state node at path (whose hash is want) from the merged short node
// created by the collapse.
func reconstructSibling(postRoot common.Hash, proofNodes map[common.Hash][]byte, keyHex, path []byte, want common.Hash) ([]byte, error) {
	blob, ok := proofNodes[postRoot]
	if !ok {
		return nil, errNoSibling
	}
	depth := 0
	for {
		n, err := decodeNode(blob)
		if err != nil {
			return nil, err
		}
		if n.short {
			// keyHex and path agree up to the collapsed branch, so the
			// nibbles covered by this node are path[:depth] + n.key.
			var full []byte
			if depth < len(path) {
				full = append(append([]byte{}, path[:depth]...), n.key...)
			}
			if depth < len(path) && len(full) >= len(path) && bytes.Equal(full[:len(path)], path) {
				rest := full[len(path):]
				if len(rest) == 0 && !n.leaf {
					return nil, errNoSibling // sibling was a full node
				}
				enc, err := rlp.EncodeToBytes([]rlp.RawValue{mustEncodeString(hexToCompact(rest, n.leaf)), n.val})
				if err != nil {
					return nil, err
				}
				if crypto.Keccak256Hash(enc) != want {
					return nil, errNoSibling
				}
				return enc, nil
			}
			if n.leaf || len(keyHex) < depth+len(n.key) || !bytes.Equal(keyHex[depth:depth+len(n.key)], n.key) {
				return nil, errNoSibling
			}
			depth += len(n.key)
			next, ok := childRef(n.val, proofNodes)
			if !ok {
				return nil, errNoSibling
			}
			blob = next
			continue
		}
		if depth >= len(keyHex) {
			return nil, errNoSibling
		}
		next, ok := childRef(n.kids[keyHex[depth]], proofNodes)
		if !ok {
			return nil, errNoSibling
		}
		depth++
		blob = next
	}
}

func mustEncodeString(b []byte) rlp.RawValue {
	enc, _ := rlp.EncodeToBytes(b)
	return enc
}

// resolveFromPostState tries to rebuild the missing sibling using post-state
// proofs for keys deleted by the block. Returns true if the node was stored.
func (s *Source) resolveFromPostState(ctx context.Context, post *PostState, r *Reader, sb sibling, want common.Hash) bool {
	if post == nil || len(sb.path) == 0 {
		return false
	}
	prefix := sb.path[:len(sb.path)-1]
	// Candidate keys: accessed keys of this trie that ran through the
	// collapsed branch but not through the surviving sibling.
	type cand struct {
		addr common.Address
		slot *common.Hash
		hex  []byte
	}
	var cands []cand
	r.mu.Lock()
	if sb.owner == (common.Hash{}) {
		for h, a := range r.owners {
			hx := keybytesToHex(h.Bytes())
			if bytes.HasPrefix(hx, prefix) && !bytes.HasPrefix(hx, sb.path) {
				cands = append(cands, cand{addr: a, hex: hx})
			}
		}
	} else if addr, ok := r.owners[sb.owner]; ok {
		for slot := range r.slotsRead[addr] {
			hx := keybytesToHex(crypto.Keccak256(slot.Bytes()))
			if bytes.HasPrefix(hx, prefix) && !bytes.HasPrefix(hx, sb.path) {
				sl := slot
				cands = append(cands, cand{addr: addr, slot: &sl, hex: hx})
			}
		}
	}
	r.mu.Unlock()
	for _, c := range cands {
		req := ProofRequest{Address: c.addr}
		if c.slot != nil {
			req.Slots = []common.Hash{*c.slot}
		}
		// The upstream may still be importing the block: retry briefly.
		var nodes map[common.Hash][]byte
		for attempt := 0; attempt < 4 && nodes == nil; attempt++ {
			out, err := s.backend.Proofs(ctx, post.At, []ProofRequest{req})
			if err == nil {
				nodes = collectNodes(out[0])
				break
			}
			select {
			case <-ctx.Done():
				return false
			case <-time.After(time.Duration(500*(attempt+1)) * time.Millisecond):
			}
		}
		if nodes == nil {
			continue
		}
		root := post.Root
		keyHex := c.hex
		if c.slot != nil {
			// Walk the account proof to find the post-state storage root.
			acc, err := s.postAccount(post.Root, c.addr, nodes)
			if err != nil {
				continue
			}
			root = acc
		}
		blob, err := reconstructSibling(root, nodes, keyHex, sb.path, want)
		if err != nil {
			continue
		}
		rawdb.WriteLegacyTrieNode(s.kv, want, blob)
		metrics.SiblingSearches.Inc()
		log.Debug("Reconstructed collapse sibling from post-state", "owner", sb.owner, "path", fmt.Sprintf("%x", sb.path))
		return true
	}
	return false
}

func collectNodes(res *proof.AccountResult) map[common.Hash][]byte {
	nodes := make(map[common.Hash][]byte)
	for _, n := range res.AccountProof {
		nodes[crypto.Keccak256Hash(n)] = n
	}
	for _, sp := range res.StorageProof {
		for _, n := range sp.Proof {
			nodes[crypto.Keccak256Hash(n)] = n
		}
	}
	return nodes
}

// postAccount extracts an account's storage root from post-state proof nodes.
func (s *Source) postAccount(root common.Hash, addr common.Address, nodes map[common.Hash][]byte) (common.Hash, error) {
	db := memorydb.New()
	for h, n := range nodes {
		db.Put(h.Bytes(), n)
	}
	val, err := trie.VerifyProof(root, crypto.Keccak256(addr.Bytes()), db)
	if err != nil || val == nil {
		return common.Hash{}, errNoSibling
	}
	var acc struct {
		Nonce    uint64
		Balance  []byte
		Root     common.Hash
		CodeHash []byte
	}
	if err := rlp.DecodeBytes(val, &acc); err != nil {
		return common.Hash{}, err
	}
	return acc.Root, nil
}
