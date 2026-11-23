package main

// Fee estimates and speeding up waiting transactions (Bitcoin and Litecoin,
// separate programs).
//
// Most vault transactions are signed by both sides long before they are
// sent (the exit request, the pay-back, each state), so their fee is fixed
// when they are signed (txFee). If fees rise, one of them can sit unconfirmed.
// The way out is "child pays for parent": spend one of your own outputs of
// the waiting transaction in a new transaction with a higher fee. Miners take
// both together, for the combined fee.
//
// For that, every transaction the user may need to hurry must have an output
// only the user can spend at once:
//   - the exit request now carries a small anchor output for each side
//     (anchorAmt, vaults opened since this change; split_core.go);
//   - the exchange's copy of a state, the pay-back and a settlement pay the
//     user's share straight to the user's wallet;
//   - the deposit pays change back to the wallet.
//
// Test networks only. Not audited.

import (
	"fmt"
	"math"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

const (
	anchorAmt      = 330 // an anchor output: the smallest a Taproot output may be
	minFeeRate     = 2   // sat/vB used when the node has no estimate
	feeRateTarget  = 6   // blocks: estimate for confirmation within about an hour (BTC)
	bumpAfter      = 2   // blocks a transaction may wait before the watchtower speeds it up
	childInputVB   = 58  // a Taproot key-path input, in virtual bytes (rounded up)
	childOutputVB  = 43  // a Taproot output
	childOverheadB = 11  // version, counts, locktime, segwit marker
)

// feeRate is the node's fee estimate in sat/vB, or minFeeRate when the node
// has none (new private chains have no fee history). estimated says which.
func (c *chainConn) feeRate() (rate int64, estimated bool) {
	if r, err := c.rpc.EstimateFee(feeRateTarget); err == nil && r > 0 {
		rate = int64(math.Ceil(r * 1e8 / 1000)) // coins per kB -> sat per vB
		if rate < 1 {
			rate = 1
		}
		return rate, true
	}
	return minFeeRate, false
}

// feeNote describes the current fee estimate for the page.
func (c *chainConn) feeNote() string {
	r, ok := c.feeRate()
	if ok {
		return fmt.Sprintf("Network fee estimate: %d sat/vB", r)
	}
	return fmt.Sprintf("The node has no fee estimate yet; speeding up uses %d sat/vB", r)
}

// waiting reports whether tx is in the mempool (sent, not yet in a block),
// the fee it pays and its size as sent (with signatures), in vbytes.
func (c *chainConn) waiting(tx *wire.MsgTx) (fee, vb int64, ok bool) {
	if tx == nil {
		return 0, 0, false
	}
	m, err := c.rpc.GetRawMempoolVerbose()
	if err != nil {
		return 0, 0, false
	}
	e, ok := m[tx.TxHash().String()]
	if !ok {
		return 0, 0, false
	}
	a, _ := btcutil.NewAmount(e.Fee)
	vb = int64(e.Vsize)
	if vb == 0 {
		vb = (int64(e.Weight) + 3) / 4
	}
	if vb == 0 {
		vb = int64(e.Size)
	}
	return int64(a), vb, true
}

// bumpable lists the user's transactions on this chain that are waiting for
// a block and have an output the wallet can spend at once (w.mu held).
func (wc *wChain) bumpable() []*wire.MsgTx {
	v := wc.vault
	var cands []*wire.MsgTx
	if v != nil {
		cands = append(cands, v.fund, v.settleTx, v.closeTx, v.spliceTx)
		for _, g := range v.gens {
			cands = append(cands, g.exitTx, g.refundTx)
		}
		for _, s := range v.states {
			cands = append(cands, s.etx)
		}
	}
	seen := map[chainhash.Hash]bool{}
	var out []*wire.MsgTx
	for _, tx := range cands {
		if tx == nil || seen[tx.TxHash()] {
			continue
		}
		seen[tx.TxHash()] = true
		pi := payIndex(tx, wc.pay)
		if pi < 0 {
			continue
		}
		// Once a child spends it (ours or the exchange's anchor), the pair
		// has been sped up.
		if wc.c.hasChild(tx) {
			continue
		}
		if _, _, ok := wc.c.waiting(tx); ok {
			out = append(out, tx)
		}
	}
	return out
}

// hasChild reports whether a transaction in the mempool spends an output of tx.
func (c *chainConn) hasChild(tx *wire.MsgTx) bool {
	m, err := c.rpc.GetRawMempoolVerbose()
	if err != nil {
		return false
	}
	id := tx.TxHash().String()
	for _, e := range m {
		for _, d := range e.Depends {
			if d == id {
				return true
			}
		}
	}
	return false
}

func payIndex(tx *wire.MsgTx, script []byte) int {
	for i, o := range tx.TxOut {
		if string(o.PkScript) == string(script) {
			return i
		}
	}
	return -1
}

// bump sends a child of parent that pays enough fee for both to reach rate
// (sat/vB). It spends the wallet's output of parent, plus wallet coins if
// that output is too small to pay the fee (w.mu held). It returns the fee the
// child pays.
func (wc *wChain) bump(parent *wire.MsgTx, rate int64) (int64, error) {
	pfee, pv, ok := wc.c.waiting(parent)
	if !ok {
		return 0, userErr("That transaction isn't waiting for a block any more.")
	}
	pi := payIndex(parent, wc.pay)
	if pi < 0 {
		return 0, userErr("That transaction has no output your wallet can spend.")
	}
	child := wire.NewMsgTx(2)
	prevs := map[wire.OutPoint]*wire.TxOut{}
	add := func(op wire.OutPoint, amt int64) {
		child.AddTxIn(wire.NewTxIn(&op, nil, nil))
		prevs[op] = wire.NewTxOut(amt, wc.pay)
	}
	pop := wire.OutPoint{Hash: parent.TxHash(), Index: uint32(pi)}
	in := parent.TxOut[pi].Value
	add(pop, in)
	need := func() int64 {
		cv := int64(childOverheadB + childOutputVB + childInputVB*len(child.TxIn))
		fee := rate*(pv+cv) - pfee
		if fee < rate*cv {
			fee = rate * cv // the child must pay its own way too
		}
		return fee
	}
	// Add confirmed wallet coins until the output stays above the dust limit.
	for _, u := range wc.coins {
		if in-need() > anchorAmt {
			break
		}
		if u.op == pop {
			continue
		}
		if confs, ok := wc.c.seen(u.op.Hash); !ok || confs < 1 {
			continue
		}
		if _, ok := wc.c.unspent(u.op); !ok {
			continue
		}
		add(u.op, u.amt)
		in += u.amt
	}
	fee := need()
	if in-fee <= anchorAmt {
		return 0, userErr("Your wallet needs about %s more to pay for speeding this up.", fmtCoin(fee+anchorAmt-in+1, wc.cfg.Info.Ticker))
	}
	child.AddTxOut(wire.NewTxOut(in-fee, wc.pay))
	hashes := txscript.NewTxSigHashes(child, txscript.NewMultiPrevOutFetcher(prevs))
	for i, txin := range child.TxIn {
		p := prevs[txin.PreviousOutPoint]
		wit, err := txscript.TaprootWitnessSignature(child, hashes, i, p.Value, p.PkScript, txscript.SigHashDefault, wc.key)
		if err != nil {
			return 0, err
		}
		child.TxIn[i].Witness = wit
	}
	if err := wc.c.send(child); err != nil {
		return 0, err
	}
	wc.learn(parent)
	wc.learn(child)
	return fee, nil
}

// SpeedUp speeds up every one of the user's waiting transactions on chain,
// at the node's fee estimate (or minFeeRate), at least twice what each
// already pays. It returns a summary for the page.
func (w *Wallet) SpeedUp(chain string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	wc, err := w.chain(chain)
	if err != nil {
		return "", err
	}
	txs := wc.bumpable()
	if len(txs) == 0 {
		return "", userErr("None of your transactions is waiting for a block.")
	}
	n, total := 0, int64(0)
	var last error
	for _, tx := range txs {
		fee, err := wc.bump(tx, wc.targetRate(tx))
		if err != nil {
			last = err
			continue
		}
		n++
		total += fee
		if v := wc.vault; v != nil {
			v.log("Sped up %s with a child transaction paying %s", tx.TxHash().String()[:12], fmtCoin(fee, wc.cfg.Info.Ticker))
		}
	}
	if n == 0 {
		return "", last
	}
	return fmt.Sprintf("Sped up %d waiting transaction(s); the extra fee is %s", n, fmtCoin(total, wc.cfg.Info.Ticker)), nil
}

// targetRate is the rate to bump tx to: the estimate, and at least twice the
// rate tx pays now.
func (wc *wChain) targetRate(tx *wire.MsgTx) int64 {
	r, _ := wc.c.feeRate()
	if fee, vb, ok := wc.c.waiting(tx); ok && vb > 0 {
		if now := (fee + vb - 1) / vb; 2*now > r {
			r = 2 * now
		}
	}
	return r
}

// autoBump is the watchtower's part: during an exit, a transaction of ours
// that has waited bumpAfter blocks is sped up once (w.mu held).
func (w *Wallet) autoBump(wc *wChain) {
	v := wc.vault
	if !w.WT || v == nil || v.closed != "" || !v.exitSent {
		return
	}
	if wc.firstSeen == nil {
		wc.firstSeen = map[chainhash.Hash]int64{}
	}
	h := wc.c.height()
	for _, tx := range wc.bumpable() {
		id := tx.TxHash()
		first, ok := wc.firstSeen[id]
		if !ok {
			wc.firstSeen[id] = h
			continue
		}
		if first < 0 || h-first < bumpAfter {
			continue
		}
		wc.firstSeen[id] = -1 // once
		if fee, err := wc.bump(tx, wc.targetRate(tx)); err == nil {
			v.log("Your watchtower sped up %s, which had waited %d blocks (extra fee %s)", id.String()[:12], h-first, fmtCoin(fee, wc.cfg.Info.Ticker))
		}
	}
}
