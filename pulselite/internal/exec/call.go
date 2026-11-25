package exec

import (
	"context"
	"errors"
	"math"

	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/eth/gasestimator"
	"github.com/pulsechainlite/pulselite/internal/verified"
)

func refOf(h *types.Header) verified.BlockRef {
	return verified.BlockRef{Hash: h.Hash(), Number: h.Number.Uint64()}
}

func copyMsg(m *core.Message) *core.Message {
	c := *m
	return &c
}

// ReadState runs fn against the verified post-state of header.
func (e *Executor) ReadState(ctx context.Context, header *types.Header, fn func(sdb *state.StateDB) error) error {
	_, err := e.src.Run(ctx, header.Root, refOf(header), e.MaxRounds, func(r *verified.Reader) error {
		sdb, err := state.NewWithReader(header.Root, e.src.StateDatabase(), r)
		if err != nil {
			return err
		}
		ferr := fn(sdb)
		if !r.Misses().Empty() {
			return nil
		}
		if serr := sdb.Error(); serr != nil {
			if r.RecordStateError(serr) {
				return nil
			}
			return serr
		}
		return ferr
	})
	return err
}

// Call executes msg on the verified post-state of header (eth_call).
func (e *Executor) Call(ctx context.Context, header *types.Header, msg *core.Message) (*core.ExecutionResult, error) {
	var result *core.ExecutionResult
	_, err := e.src.Run(ctx, header.Root, refOf(header), e.MaxRounds, func(r *verified.Reader) error {
		result = nil
		sdb, err := state.NewWithReader(header.Root, e.src.StateDatabase(), r)
		if err != nil {
			return err
		}
		blockCtx := core.NewEVMBlockContext(header, e.NewChainContext(r), nil)
		evm := vm.NewEVM(blockCtx, sdb, e.cfg, vm.Config{NoBaseFee: true})
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			select {
			case <-ctx.Done():
				evm.Cancel()
			case <-stop:
			}
		}()
		res, aerr := core.ApplyMessage(evm, copyMsg(msg), new(core.GasPool).AddGas(math.MaxUint64))
		if !r.Misses().Empty() {
			return nil
		}
		if serr := sdb.Error(); serr != nil {
			if r.RecordStateError(serr) {
				return nil
			}
			return serr
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if aerr != nil {
			return aerr
		}
		result = res
		return nil
	})
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, errors.New("call produced no result")
	}
	return result, nil
}

// EstimateGas runs go-ethereum's gas estimator on verified state.
func (e *Executor) EstimateGas(ctx context.Context, header *types.Header, msg *core.Message, gasCap uint64) (uint64, []byte, error) {
	var (
		gas    uint64
		revert []byte
	)
	_, err := e.src.Run(ctx, header.Root, refOf(header), e.MaxRounds, func(r *verified.Reader) error {
		sdb, err := state.NewWithReader(header.Root, e.src.StateDatabase(), r)
		if err != nil {
			return err
		}
		opts := &gasestimator.Options{
			Config:     e.cfg,
			Chain:      e.NewChainContext(r),
			Header:     header,
			State:      sdb,
			ErrorRatio: 0.015,
		}
		g, rev, eerr := gasestimator.Estimate(ctx, copyMsg(msg), opts, gasCap)
		if !r.Misses().Empty() {
			return nil
		}
		if serr := sdb.Error(); serr != nil {
			if r.RecordStateError(serr) {
				return nil
			}
			return serr
		}
		gas, revert = g, rev
		return eerr
	})
	return gas, revert, err
}
