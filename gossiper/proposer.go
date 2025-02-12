// Copyright (C) 2023, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package gossiper

import (
	"context"
	"errors"
	"time"

	"github.com/ava-labs/avalanchego/cache"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow/engine/common"
	"github.com/ava-labs/avalanchego/utils/set"
	"github.com/ava-labs/avalanchego/vms/proposervm/proposer"
	"github.com/ava-labs/hypersdk/chain"
	"go.uber.org/zap"
)

var _ Gossiper = (*Proposer)(nil)

var proposerWindow = int64(proposer.MaxDelay.Seconds())

type Proposer struct {
	vm        VM
	cfg       *ProposerConfig
	appSender common.AppSender

	doneGossip chan struct{}

	// bounded by validator count (may be slightly out of date as composition changes)
	gossipedTxs map[ids.NodeID]*cache.LRU[ids.ID, struct{}]
	receivedTxs *cache.LRU[ids.ID, struct{}]
}

type ProposerConfig struct {
	GossipProposerDiff      int
	GossipProposerDepth     int
	GossipInterval          time.Duration
	GossipPeerCacheSize     int
	GossipReceivedCacheSize int
	GossipMinLife           int64 // seconds
	BuildProposerDiff       int
}

func DefaultProposerConfig() *ProposerConfig {
	return &ProposerConfig{
		GossipProposerDiff:      2,
		GossipProposerDepth:     3,
		GossipInterval:          1 * time.Second,
		GossipPeerCacheSize:     10_240,
		GossipReceivedCacheSize: 65_536,
		GossipMinLife:           5,
		BuildProposerDiff:       2,
	}
}

func NewProposer(vm VM, cfg *ProposerConfig) *Proposer {
	return &Proposer{
		vm:  vm,
		cfg: cfg,

		doneGossip: make(chan struct{}),

		gossipedTxs: map[ids.NodeID]*cache.LRU[ids.ID, struct{}]{},
		receivedTxs: &cache.LRU[ids.ID, struct{}]{Size: cfg.GossipReceivedCacheSize},
	}
}

func (g *Proposer) sendTxs(ctx context.Context, txs []*chain.Transaction) error {
	ctx, span := g.vm.Tracer().Start(ctx, "Gossiper.sendTxs")
	defer span.End()

	proposers, err := g.vm.Proposers(
		ctx,
		g.cfg.GossipProposerDiff,
		g.cfg.GossipProposerDepth,
	)
	if err != nil || proposers.Len() == 0 {
		g.vm.Logger().Warn(
			"unable to find any proposers, falling back to all-to-all gossip",
			zap.Error(err),
		)

		actionRegistry, authRegistry := g.vm.Registry()
		b, err := chain.MarshalTxs(txs, actionRegistry, authRegistry)
		if err != nil {
			return err
		}

		if err := g.appSender.SendAppGossip(ctx, b); err != nil {
			g.vm.Logger().Warn(
				"GossipTxs failed",
				zap.Error(err),
			)
			return err
		}
		return nil
	}

	for proposer := range proposers {
		// Don't gossip to self
		if proposer == g.vm.NodeID() {
			continue
		}

		c, ok := g.gossipedTxs[proposer]
		if !ok {
			g.gossipedTxs[proposer] = &cache.LRU[ids.ID, struct{}]{Size: g.cfg.GossipPeerCacheSize}
			c = g.gossipedTxs[proposer]
		}

		toGossip := make([]*chain.Transaction, 0, len(txs))
		for _, tx := range txs {
			if _, ok := c.Get(tx.ID()); ok {
				continue
			}
			c.Put(tx.ID(), struct{}{})
			toGossip = append(toGossip, tx)
		}

		if len(toGossip) == 0 {
			g.vm.Logger().Debug("nothing to gossip", zap.Stringer("node", proposer))
			continue
		}

		// TODO: cache marshalization
		actionRegistry, authRegistry := g.vm.Registry()
		b, err := chain.MarshalTxs(toGossip, actionRegistry, authRegistry)
		if err != nil {
			return err
		}

		if err := g.appSender.SendAppGossipSpecific(ctx, set.Set[ids.NodeID]{proposer: {}}, b); err != nil {
			g.vm.Logger().Warn(
				"GossipTxs failed",
				zap.Stringer("node", proposer),
				zap.Error(err),
			)
			return err
		}
	}
	return nil
}

// Triggers "AppGossip" on the pending transactions in the mempool.
// "force" is true to re-gossip whether recently gossiped or not
func (g *Proposer) TriggerGossip(ctx context.Context) error {
	ctx, span := g.vm.Tracer().Start(ctx, "Gossiper.GossipTxs")
	defer span.End()

	// Gossip highest paying txs
	txs := []*chain.Transaction{}
	totalUnits := uint64(0)
	start := time.Now()
	now := start.Unix()
	r := g.vm.Rules(now)

	// Create temporary execution context
	blk, err := g.vm.PreferredBlock(ctx)
	if err != nil {
		return err
	}
	state, err := blk.State()
	if err != nil {
		return err
	}
	ectx, err := chain.GenerateExecutionContext(
		ctx,
		g.vm.ChainID(),
		now,
		blk,
		g.vm.Tracer(),
		g.vm.Rules(now),
	)
	if err != nil {
		return err
	}
	mempoolErr := g.vm.Mempool().Build(
		ctx,
		func(ictx context.Context, next *chain.Transaction) (cont bool, restore bool, removeAcct bool, err error) {
			// Remove txs that are expired
			if next.Base.Timestamp < now {
				return true, false, false, nil
			}

			// Don't gossip txs that are about to expire
			life := next.Base.Timestamp - now
			if life < g.cfg.GossipMinLife {
				return true, true, false, nil
			}

			// Don't gossip txs we received from other nodes (original gossiper will
			// gossip again if the transaction is still important to them, so our
			// gossip will just be useless bytes).
			//
			// We still keep these transactions in our mempool as they may still be
			// the highest-paying transaction to execute at a given time.
			if _, has := g.receivedTxs.Get(next.ID()); has {
				return true, true, false, nil
			}

			// PreExecute does not make any changes to state
			if err := next.PreExecute(ctx, ectx, r, state, now); err != nil {
				// Do not gossip invalid txs (may become invalid during normal block
				// processing)
				cont, restore, removeAcct := chain.HandlePreExecute(err)
				return cont, restore, removeAcct, nil
			}

			// Gossip up to a block of content
			units, err := next.MaxUnits(r)
			if err != nil {
				// Should never happen
				return true, false, false, nil
			}
			if units+totalUnits > r.GetMaxBlockUnits() {
				// Attempt to mirror the function of building a block without execution
				return false, true, false, nil
			}
			txs = append(txs, next)
			totalUnits += units
			return len(txs) < r.GetMaxBlockTxs(), true, false, nil
		},
	)
	if mempoolErr != nil {
		return mempoolErr
	}
	if len(txs) == 0 {
		return nil
	}
	g.vm.Logger().Info(
		"gossiping transactions", zap.Int("txs", len(txs)),
		zap.Uint64("preferred height", blk.Hght), zap.Duration("t", time.Since(start)),
	)
	return g.sendTxs(ctx, txs)
}

func (g *Proposer) HandleAppGossip(ctx context.Context, nodeID ids.NodeID, msg []byte) error {
	r := g.vm.Rules(time.Now().Unix())
	actionRegistry, authRegistry := g.vm.Registry()
	txs, err := chain.UnmarshalTxs(msg, r.GetMaxBlockTxs(), actionRegistry, authRegistry)
	if err != nil {
		g.vm.Logger().Warn(
			"received invalid txs",
			zap.Stringer("peerID", nodeID),
			zap.Error(err),
		)
		return nil
	}

	// Mark incoming gossip as held by [nodeID], if it is a validator
	isValidator, err := g.vm.IsValidator(ctx, nodeID)
	if err != nil {
		g.vm.Logger().Warn(
			"unable to determine if nodeID is validator",
			zap.Stringer("peerID", nodeID),
			zap.Error(err),
		)
	}
	var c *cache.LRU[ids.ID, struct{}]
	if isValidator {
		var ok bool
		c, ok = g.gossipedTxs[nodeID]
		if !ok {
			g.gossipedTxs[nodeID] = &cache.LRU[ids.ID, struct{}]{Size: g.cfg.GossipPeerCacheSize}
			c = g.gossipedTxs[nodeID]
		}
	}

	// Add incoming transactions to our caches to prevent useless gossip
	for _, tx := range txs {
		if c != nil {
			c.Put(tx.ID(), struct{}{})
		}
		g.receivedTxs.Put(tx.ID(), struct{}{})
	}

	// Submit incoming gossip to mempool
	start := time.Now()
	for _, err := range g.vm.Submit(ctx, true, txs) {
		if err == nil || errors.Is(err, chain.ErrDuplicateTx) {
			continue
		}
		g.vm.Logger().Debug(
			"failed to submit gossiped txs",
			zap.Stringer("nodeID", nodeID), zap.Error(err),
		)
	}
	g.vm.Logger().Info(
		"submitted gossipped transactions",
		zap.Int("txs", len(txs)),
		zap.Stringer("nodeID", nodeID), zap.Duration("t", time.Since(start)),
	)

	// only trace error to prevent VM's being shutdown
	// from "AppGossip" returning an error
	return nil
}

// periodically but less aggressively force-regossip the pending
func (g *Proposer) Run(appSender common.AppSender) {
	g.appSender = appSender

	g.vm.Logger().Info("starting gossiper", zap.Duration("interval", g.cfg.GossipInterval))
	defer close(g.doneGossip)

	t := time.NewTicker(g.cfg.GossipInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			tctx := context.Background()

			// If soon to be proposer, don't gossip
			proposers, err := g.vm.Proposers(
				tctx,
				g.cfg.BuildProposerDiff,
				1,
			)
			if err == nil && proposers.Contains(g.vm.NodeID()) {
				g.vm.Logger().Debug("not gossiping because soon to propose")
				continue
			} else if err != nil {
				g.vm.Logger().Warn("unable to determine if will propose soon, gossiping anyways", zap.Error(err))
			}

			// If last block seen was greater than window, don't gossip (will just
			// cause contention)
			preferredBlk, err := g.vm.PreferredBlock(tctx)
			if err != nil {
				g.vm.Logger().Warn(
					"unable to get preferred block, gossiping anyways",
					zap.Error(err),
				)
			}
			if preferredBlk.Tmstmp+proposerWindow < time.Now().Unix() {
				g.vm.Logger().Debug(
					"not gossiping because no recent block, will probably produce ourself",
				)
				continue
			}

			// Gossip to proposers who will produce next
			if err := g.TriggerGossip(tctx); err != nil {
				g.vm.Logger().Warn("gossip txs failed", zap.Error(err))
			}
		case <-g.vm.StopChan():
			g.vm.Logger().Info("stopping gossip loop")
			return
		}
	}
}

func (g *Proposer) Done() {
	<-g.doneGossip
}
