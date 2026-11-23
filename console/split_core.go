package main

// Two-party version of the Bitcoin/Litecoin vault, for the separate exchange
// and wallet programs. Each side holds only its own private key; everything
// here is built from public keys, so both sides can build the same
// transactions independently and check the other side's signatures.
//
// The scripts and transactions are the same as in utxo_vault.go:
//
//	V:   <user> CHECKSIGVERIFY <exch> CHECKSIG
//	E:   same 2-of-2, or <delay> CSV DROP <user> CHECKSIGVERIFY <exch> CHECKSIG (pay-back of deposits)
//	R_n: <D2> CSV DROP <exch> CHECKSIG, or SHA256 <H(r_n)> EQUALVERIFY <user> CHECKSIG
//	U_n: <D2> CSV DROP <user> CHECKSIG, or SHA256 <H(u_n)> EQUALVERIFY <exch> CHECKSIG
//
// A "generation" is one vault output with its exit request and pay-back.
// Adding exchange coins (a splice) starts a new generation.

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// vaultDef is everything both sides agree on when a vault opens.
type vaultDef struct {
	user, exch       []byte // x-only public keys
	userPay, exchPay []byte // where each side's coins are paid
	delay, d2        int64
	anchors          bool // exit requests carry an anchor output for each side (vaults opened since fees.go)
	ms, dl           txscript.TapLeaf
	V, E             *tapOutput
}

func newVaultDef(user, exch, userPay, exchPay []byte, delay, d2 int64) (*vaultDef, error) {
	for _, k := range [][]byte{user, exch} {
		if _, err := schnorr.ParsePubKey(k); err != nil {
			return nil, fmt.Errorf("bad public key: %v", err)
		}
	}
	ms, err := txscript.NewScriptBuilder().
		AddData(user).AddOp(txscript.OP_CHECKSIGVERIFY).
		AddData(exch).AddOp(txscript.OP_CHECKSIG).Script()
	if err != nil {
		return nil, err
	}
	dl, err := txscript.NewScriptBuilder().
		AddInt64(delay).AddOp(txscript.OP_CHECKSEQUENCEVERIFY).AddOp(txscript.OP_DROP).
		AddData(user).AddOp(txscript.OP_CHECKSIGVERIFY).
		AddData(exch).AddOp(txscript.OP_CHECKSIG).Script()
	if err != nil {
		return nil, err
	}
	d := &vaultDef{user: user, exch: exch, userPay: userPay, exchPay: exchPay, delay: delay, d2: d2,
		ms: txscript.NewBaseTapLeaf(ms), dl: txscript.NewBaseTapLeaf(dl)}
	d.V = newTapOutput(d.ms)
	d.E = newTapOutput(d.ms, d.dl)
	return d, nil
}

// vgen is one generation: the vault output, its exit request and pay-back.
type vgen struct {
	vaultOut         wire.OutPoint
	vaultAmt         int64
	userDep, exchDep int64
	exitTx           *wire.MsgTx
	exitAmt          int64
	refundTx         *wire.MsgTx
	// signatures, filled in as they are exchanged
	exitSigU, exitSigX     []byte
	refundSigU, refundSigX []byte
}

func (d *vaultDef) buildGen(out wire.OutPoint, amt, userDep, exchDep int64) *vgen {
	g := &vgen{vaultOut: out, vaultAmt: amt, userDep: userDep, exchDep: exchDep}
	g.exitTx = wire.NewMsgTx(2)
	g.exitTx.AddTxIn(wire.NewTxIn(&out, nil, nil))
	g.exitAmt = amt - txFee
	if d.anchors {
		g.exitAmt -= 2 * anchorAmt
	}
	g.exitTx.AddTxOut(wire.NewTxOut(g.exitAmt, d.E.pkScript))
	if d.anchors {
		// One small output for each side, so either can speed the exit
		// request up with a child transaction (child pays for parent).
		g.exitTx.AddTxOut(wire.NewTxOut(anchorAmt, d.userPay))
		g.exitTx.AddTxOut(wire.NewTxOut(anchorAmt, d.exchPay))
	}
	g.refundTx, _, _, _, _ = splitPayout(g.exitOut(), g.exitAmt, userDep, exchDep, d.userPay, d.exchPay)
	g.refundTx.TxIn[0].Sequence = uint32(d.delay)
	return g
}

func (g *vgen) exitOut() wire.OutPoint { return wire.OutPoint{Hash: g.exitTx.TxHash(), Index: 0} }

// revOut is a revocable output: its owner after D2 blocks, or the other side
// with the owner's secret for that state.
type revOut struct {
	own, rev txscript.TapLeaf
	out      *tapOutput
}

func (d *vaultDef) revOutput(owner, other, hash []byte) (revOut, error) {
	ol, err := txscript.NewScriptBuilder().
		AddInt64(d.d2).AddOp(txscript.OP_CHECKSEQUENCEVERIFY).AddOp(txscript.OP_DROP).
		AddData(owner).AddOp(txscript.OP_CHECKSIG).Script()
	if err != nil {
		return revOut{}, err
	}
	rl, err := txscript.NewScriptBuilder().
		AddOp(txscript.OP_SHA256).AddData(hash).AddOp(txscript.OP_EQUALVERIFY).
		AddData(other).AddOp(txscript.OP_CHECKSIG).Script()
	if err != nil {
		return revOut{}, err
	}
	r := revOut{own: txscript.NewBaseTapLeaf(ol), rev: txscript.NewBaseTapLeaf(rl)}
	r.out = newTapOutput(r.own, r.rev)
	return r, nil
}

// pstate is one signed split. Both copies pay out from the generation's
// exit output: the exchange's copy (etx: user share to the user's wallet,
// exchange share to R) and the user's copy (utx: exchange share to the
// exchange, user share to U).
type pstate struct {
	Index, Gen         int
	Label              string
	UserSats, ExchSats int64
	rHash, uHash       []byte
	R, U               revOut
	etx, utx           *wire.MsgTx
	rIdx, uIdx         int
	rAmt, uAmt         int64
	userSigE           []byte // the user's signature on the exchange's copy
	exchSigU           []byte // the exchange's signature on the user's copy
	rSecret, uSecret   []byte // each side knows its own; the other's once revoked
	revoked            bool
}

func (d *vaultDef) buildState(g *vgen, idx, gen int, label string, user, exch int64, rHash, uHash []byte) (*pstate, error) {
	if len(rHash) != 32 || len(uHash) != 32 {
		return nil, errors.New("revocation hashes must be 32 bytes")
	}
	if user < 0 || exch < 0 || user+exch != g.vaultAmt {
		return nil, fmt.Errorf("split %d + %d doesn't add up to the vault's %d", user, exch, g.vaultAmt)
	}
	s := &pstate{Index: idx, Gen: gen, Label: label, UserSats: user, ExchSats: exch, rHash: rHash, uHash: uHash}
	var err error
	if s.R, err = d.revOutput(d.exch, d.user, rHash); err != nil {
		return nil, err
	}
	if s.U, err = d.revOutput(d.user, d.exch, uHash); err != nil {
		return nil, err
	}
	s.etx, _, _, s.rIdx, s.rAmt = splitPayout(g.exitOut(), g.exitAmt, user, exch, d.userPay, s.R.out.pkScript)
	s.utx, s.uIdx, s.uAmt, _, _ = splitPayout(g.exitOut(), g.exitAmt, user, exch, s.U.out.pkScript, d.exchPay)
	return s, nil
}

func (s *pstate) rOut() wire.OutPoint {
	return wire.OutPoint{Hash: s.etx.TxHash(), Index: uint32(s.rIdx)}
}
func (s *pstate) uOut() wire.OutPoint {
	return wire.OutPoint{Hash: s.utx.TxHash(), Index: uint32(s.uIdx)}
}

// ------------------------------------------------------------------ signatures

func tapSigHash(tx *wire.MsgTx, idx int, fetch txscript.PrevOutputFetcher, leaf txscript.TapLeaf) ([]byte, error) {
	return txscript.CalcTapscriptSignaturehash(txscript.NewTxSigHashes(tx, fetch), txscript.SigHashDefault, tx, idx, fetch, leaf)
}

// signLeaf signs input idx of tx through leaf.
func signLeaf(tx *wire.MsgTx, idx int, fetch txscript.PrevOutputFetcher, leaf txscript.TapLeaf, key *btcec.PrivateKey) []byte {
	h, err := tapSigHash(tx, idx, fetch, leaf)
	if err != nil {
		panic(err)
	}
	sig, err := schnorr.Sign(key, h)
	if err != nil {
		panic(err)
	}
	return sig.Serialize()
}

// checkLeafSig reports whether sig is pub's valid signature for input idx.
func checkLeafSig(tx *wire.MsgTx, idx int, fetch txscript.PrevOutputFetcher, leaf txscript.TapLeaf, pub, sig []byte) bool {
	if len(sig) != 64 {
		return false
	}
	h, err := tapSigHash(tx, idx, fetch, leaf)
	if err != nil {
		return false
	}
	pk, err := schnorr.ParsePubKey(pub)
	if err != nil {
		return false
	}
	s, err := schnorr.ParseSignature(sig)
	return err == nil && s.Verify(h, pk)
}

func one(out *tapOutput, amt int64) txscript.PrevOutputFetcher {
	return txscript.NewCannedPrevOutputFetcher(out.pkScript, amt)
}

// what each signature covers
func (d *vaultDef) exitFetch(g *vgen) txscript.PrevOutputFetcher { return one(d.V, g.vaultAmt) }
func (d *vaultDef) eFetch(g *vgen) txscript.PrevOutputFetcher    { return one(d.E, g.exitAmt) }

// withBoth returns a copy of tx with the 2-of-2 witness for input 0.
func withBoth(tx *wire.MsgTx, out *tapOutput, leaf txscript.TapLeaf, userSig, exchSig []byte) *wire.MsgTx {
	c := tx.Copy()
	c.TxIn[0].Witness = wire.TxWitness{exchSig, userSig, leaf.Script, out.controlBlock(leaf)}
	return c
}

func (d *vaultDef) signedExit(g *vgen) *wire.MsgTx {
	return withBoth(g.exitTx, d.V, d.ms, g.exitSigU, g.exitSigX)
}
func (d *vaultDef) signedRefund(g *vgen) *wire.MsgTx {
	return withBoth(g.refundTx, d.E, d.dl, g.refundSigU, g.refundSigX)
}

// settleTx is the cooperative close: the vault output pays the latest split directly.
func (d *vaultDef) settleTx(g *vgen, s *pstate) *wire.MsgTx {
	tx, _, _, _, _ := splitPayout(g.vaultOut, g.vaultAmt, s.UserSats, s.ExchSats, d.userPay, d.exchPay)
	return tx
}

// ------------------------------------------------------------------ splice

// spliceIn is one of the exchange's wallet coins going into a splice.
type spliceIn struct {
	Op     wire.OutPoint
	Amt    int64
	Script []byte
}

// buildSplice spends the vault output and the exchange's coins into a bigger
// vault output with the same scripts. The exchange pays the fee.
func (d *vaultDef) buildSplice(g *vgen, ins []spliceIn, change []byte, sats int64) (*wire.MsgTx, txscript.PrevOutputFetcher, error) {
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(wire.NewTxIn(&g.vaultOut, nil, nil))
	prevs := map[wire.OutPoint]*wire.TxOut{g.vaultOut: wire.NewTxOut(g.vaultAmt, d.V.pkScript)}
	var in int64
	for _, u := range ins {
		op := u.Op
		tx.AddTxIn(wire.NewTxIn(&op, nil, nil))
		prevs[op] = wire.NewTxOut(u.Amt, u.Script)
		in += u.Amt
	}
	if in < sats+txFee {
		return nil, nil, fmt.Errorf("the exchange's coins (%d) don't cover %d plus the fee", in, sats)
	}
	tx.AddTxOut(wire.NewTxOut(g.vaultAmt+sats, d.V.pkScript))
	if ch := in - sats - txFee; ch > 546 {
		tx.AddTxOut(wire.NewTxOut(ch, change))
	}
	return tx, txscript.NewMultiPrevOutFetcher(prevs), nil
}

// checkSplice is the user's check that a splice the exchange built does
// what was agreed: input 0 is the vault, output 0 is the same vault script
// holding exactly sats more, and nothing else of the vault's goes anywhere.
func (d *vaultDef) checkSplice(g *vgen, tx *wire.MsgTx, sats int64) error {
	if len(tx.TxIn) < 2 || tx.TxIn[0].PreviousOutPoint != g.vaultOut {
		return errors.New("the splice doesn't spend the vault as its first input")
	}
	for _, in := range tx.TxIn[1:] {
		if in.PreviousOutPoint == g.vaultOut {
			return errors.New("the splice spends the vault twice")
		}
	}
	if len(tx.TxOut) < 1 || !bytes.Equal(tx.TxOut[0].PkScript, d.V.pkScript) || tx.TxOut[0].Value != g.vaultAmt+sats {
		return fmt.Errorf("the splice's first output isn't the vault with %d more", sats)
	}
	for _, o := range tx.TxOut[1:] {
		if bytes.Equal(o.PkScript, d.V.pkScript) {
			return errors.New("the splice has a second vault output")
		}
	}
	return nil
}

func hash32(b []byte) []byte { h := sha256.Sum256(b); return h[:] }

// secretMatches checks a revealed secret against the hash agreed earlier.
func secretMatches(secret, hash []byte) bool {
	return len(secret) == 32 && bytes.Equal(hash32(secret), hash)
}
