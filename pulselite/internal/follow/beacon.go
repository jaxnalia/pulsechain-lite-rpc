// Package follow keeps the block window in sync with the consensus client:
// it reads head/finalized execution payload pointers from the Beacon API,
// backfills headers by hash linkage, and fills bodies/receipts for the logs
// window from upstreams (verified against transactions/receipts roots).
package follow

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// BeaconClient reads the Beacon REST API.
type BeaconClient struct {
	URL    string
	client *http.Client
}

// NewBeaconClient creates a client for the beacon node at url.
func NewBeaconClient(url string) *BeaconClient {
	return &BeaconClient{URL: url, client: &http.Client{Timeout: 15 * time.Second}}
}

// PayloadRef is the execution payload pointer of a beacon block.
type PayloadRef struct {
	BlockHash   common.Hash
	BlockNumber uint64
	StateRoot   common.Hash
	Slot        uint64
}

// ExecutionPayload returns the execution payload pointer of block id
// ("head", "finalized", a slot or a root).
// Blinded blocks are used because they only need the payload header (the
// full-block endpoint makes the beacon node reconstruct payloads from the
// execution layer).
func (c *BeaconClient) ExecutionPayload(ctx context.Context, id string) (*PayloadRef, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL+"/eth/v1/beacon/blinded_blocks/"+id, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("beacon %s: HTTP %d", id, resp.StatusCode)
	}
	type payloadRef struct {
		BlockHash   common.Hash `json:"block_hash"`
		BlockNumber string      `json:"block_number"`
		StateRoot   common.Hash `json:"state_root"`
	}
	var body struct {
		Data struct {
			Message struct {
				Slot string `json:"slot"`
				Body struct {
					ExecutionPayload       *payloadRef `json:"execution_payload"`
					ExecutionPayloadHeader *payloadRef `json:"execution_payload_header"`
				} `json:"body"`
			} `json:"message"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	ep := body.Data.Message.Body.ExecutionPayloadHeader
	if ep == nil {
		ep = body.Data.Message.Body.ExecutionPayload
	}
	if ep == nil {
		return nil, fmt.Errorf("beacon %s: no execution payload", id)
	}
	n, err := strconv.ParseUint(ep.BlockNumber, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("beacon %s: bad block number %q", id, ep.BlockNumber)
	}
	slot, _ := strconv.ParseUint(body.Data.Message.Slot, 10, 64)
	return &PayloadRef{BlockHash: ep.BlockHash, BlockNumber: n, StateRoot: ep.StateRoot, Slot: slot}, nil
}

// SyncStatus is /eth/v1/node/syncing.
type SyncStatus struct {
	HeadSlot     uint64
	SyncDistance uint64
	IsSyncing    bool
	IsOptimistic bool
}

// Syncing returns the beacon node sync status.
func (c *BeaconClient) Syncing(ctx context.Context) (*SyncStatus, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL+"/eth/v1/node/syncing", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var body struct {
		Data struct {
			HeadSlot     string `json:"head_slot"`
			SyncDistance string `json:"sync_distance"`
			IsSyncing    bool   `json:"is_syncing"`
			IsOptimistic bool   `json:"is_optimistic"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	hs, _ := strconv.ParseUint(body.Data.HeadSlot, 10, 64)
	sd, _ := strconv.ParseUint(body.Data.SyncDistance, 10, 64)
	return &SyncStatus{HeadSlot: hs, SyncDistance: sd, IsSyncing: body.Data.IsSyncing, IsOptimistic: body.Data.IsOptimistic}, nil
}

// PeerCount returns the number of connected consensus-layer peers.
func (c *BeaconClient) PeerCount(ctx context.Context) (uint64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL+"/eth/v1/node/peer_count", nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var body struct {
		Data struct {
			Connected string `json:"connected"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return 0, err
	}
	return strconv.ParseUint(body.Data.Connected, 10, 64)
}
