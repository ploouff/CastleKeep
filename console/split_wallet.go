package main

// The user's wallet as a separate program (Bitcoin and Litecoin). It holds
// only the user's keys and revocation secrets, reads the chain itself, and
// checks everything the exchange sends before signing or broadcasting.
//
// Test networks only. Not audited.

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

type wChain struct {
	id        string
	cfg       UTXOConfig
	c         *chainConn
	key       *btcec.PrivateKey // wallet key
	pay       []byte
	coins     []utxo
	vault     *wVault
	lastScan  time.Time                // signet: last look for coins sent to the wallet
	firstSeen map[chainhash.Hash]int64 // when each waiting transaction was first seen (fees.go)
}

type wVault struct {
	id     string
	key    *btcec.PrivateKey
	def    *vaultDef
	gens   []*vgen
	states []*pstate
	fund   *wire.MsgTx // the deposit, broadcast only after the exchange signed everything

	spliceTx       *wire.MsgTx
	spliceFinished bool
	exchFault      string // set when the exchange broke the protocol

	exitSent    bool
	settleTx    *wire.MsgTx
	closeTx     *wire.MsgTx // whatever closed the vault (settlement, pay-back or a published state)
	published   *pstate
	publishedBy string
	closed      string
	sweepKind   string
	Events      []string
}

// Wallet is one user's wallet program.
type Wallet struct {
	mu      sync.Mutex
	base    string // the exchange's address
	Account string
	Name    string // shown on the page; the account id starts with it
	WT      bool   // the user's watchtower
	chains  map[string]*wChain
	evm     map[string]*wEth // EVM chains by ID
	orders  map[int]*wOrder

	// saving (split_save.go)
	file, saveErr string
	lastSaved     []byte
	seal          *sealKey // password for the file (seal.go); nil: plain text
}

func NewWallet(exchangeURL string, cfgs map[string]UTXOConfig, rpc map[string]string) (*Wallet, error) {
	k, _ := btcec.NewPrivateKey()
	w := &Wallet{base: exchangeURL, Account: hex.EncodeToString(xonly(k))[:16], WT: true, chains: map[string]*wChain{}}
	for id, cfg := range cfgs {
		c, err := dialChain(rpc[id])
		if err != nil {
			return nil, err
		}
		wk, _ := btcec.NewPrivateKey()
		w.chains[id] = &wChain{id: id, cfg: cfg, c: c, key: wk, pay: walletScript(wk)}
	}
	return w, nil
}

func (v *wVault) log(f string, a ...any) { v.Events = append(v.Events, fmt.Sprintf(f, a...)) }
func (v *wVault) gen() *vgen             { return v.gens[len(v.gens)-1] }
func (v *wVault) latest() *pstate        { return v.states[len(v.states)-1] }

func (w *Wallet) WalletScript(chain string) []byte { return w.chains[chain].pay }

func (w *Wallet) AddCoins(chain string, op wire.OutPoint, amt int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.chains[chain].coins = append(w.chains[chain].coins, utxo{op, amt})
}

// Balance is the wallet's unspent coins on a chain (mempool included).
func (w *Wallet) Balance(chain string) int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	wc := w.chains[chain]
	var sum int64
	for _, u := range wc.coins {
		if _, ok := wc.c.unspent(u.op); ok {
			sum += u.amt
		}
	}
	return sum
}

// learn adds any output of tx that pays the wallet.
func (wc *wChain) learn(tx *wire.MsgTx) {
	for i, o := range tx.TxOut {
		if bytes.Equal(o.PkScript, wc.pay) {
			op := wire.OutPoint{Hash: tx.TxHash(), Index: uint32(i)}
			known := false
			for _, u := range wc.coins {
				known = known || u.op == op
			}
			if !known {
				wc.coins = append(wc.coins, utxo{op, o.Value})
			}
		}
	}
}

func (w *Wallet) chain(id string) (*wChain, error) {
	wc, ok := w.chains[id]
	if !ok {
		return nil, userErr("Unknown chain.")
	}
	return wc, nil
}

func (w *Wallet) openVault(id string) (*wChain, *wVault, error) {
	wc, err := w.chain(id)
	if err != nil {
		return nil, nil, err
	}
	if wc.vault == nil || wc.vault.closed != "" {
		return nil, nil, userErr("Open a vault first.")
	}
	return wc, wc.vault, nil
}

// ------------------------------------------------------------------ opening

// Open locks sats into a new vault. The deposit is built and signed first,
// but only broadcast once the exchange's signatures on the exit request, the
// pay-back and state #0 have all been checked.
func (w *Wallet) Open(chain string, sats int64) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	wc, err := w.chain(chain)
	if err != nil {
		return err
	}
	if wc.vault != nil && wc.vault.closed == "" {
		return userErr("You already have an open vault.")
	}
	vk, _ := btcec.NewPrivateKey()
	var keys OpenKeysResp
	if err := call(w.base, "/v1/open/keys", OpenKeysReq{w.Account, chain, xonly(vk), wc.pay}, &keys); err != nil {
		return err
	}
	// Our own rules, not the exchange's: the delays must be the ones we expect.
	if keys.Delay != wc.cfg.ExitDelay || keys.D2 != wc.cfg.PenaltyDelay {
		return userErr("The exchange asked for different exit delays (%d/%d). Refusing.", keys.Delay, keys.D2)
	}
	def, err := newVaultDef(xonly(vk), keys.ExchPub, wc.pay, keys.ExchPay, wc.cfg.ExitDelay, wc.cfg.PenaltyDelay)
	if err != nil {
		return userErr("The exchange sent a bad key: %v", err)
	}
	def.anchors = keys.Anchors
	if err := def.setMuSig2(keys.MuSig2); err != nil {
		return userErr("The exchange sent keys that can't be combined: %v", err)
	}
	fund, err := wc.buildDeposit(def.V.pkScript, sats)
	if err != nil {
		return err
	}
	out := wire.OutPoint{Hash: fund.TxHash(), Index: 0}
	us, uh := newSecret()
	var st OpenStartResp
	if err := call(w.base, "/v1/open/start", OpenStartReq{keys.VaultID, sats, out, uh}, &st); err != nil {
		return err
	}
	g := def.buildGen(out, sats, sats, 0)
	s0, err := def.buildState(g, 0, 0, "Opening balance", sats, 0, st.RHash, uh)
	if err != nil {
		return err
	}
	if !checkLeafSig(g.exitTx, 0, def.exitFetch(g), def.ms, def.exch, st.ExitSig) ||
		!checkLeafSig(g.refundTx, 0, def.eFetch(g), def.dl, def.exch, st.RefundSig) ||
		!checkLeafSig(s0.utx, 0, def.eFetch(g), def.ms, def.exch, st.ExchSigU) {
		return userErr("A signature from the exchange doesn't check out, so the deposit was not sent.")
	}
	g.exitSigX, g.refundSigX, s0.exchSigU, s0.uSecret = st.ExitSig, st.RefundSig, st.ExchSigU, us
	g.exitSigU = signLeaf(g.exitTx, 0, def.exitFetch(g), def.ms, vk)
	g.refundSigU = signLeaf(g.refundTx, 0, def.eFetch(g), def.dl, vk)
	s0.userSigE = signLeaf(s0.etx, 0, def.eFetch(g), def.ms, vk)
	if err := call(w.base, "/v1/open/finish", OpenFinishReq{keys.VaultID, g.exitSigU, g.refundSigU, s0.userSigE}, nil); err != nil {
		return err
	}
	if err := wc.c.send(fund); err != nil {
		return err
	}
	wc.learn(fund)
	wc.vault = &wVault{id: keys.VaultID, key: vk, def: def, gens: []*vgen{g}, states: []*pstate{s0}, fund: fund}
	wc.vault.log("Locked %d into a new vault; the exchange signed the exit request and pay-back first", sats)
	return nil
}

// buildDeposit signs (but doesn't send) a payment of sats to script.
func (wc *wChain) buildDeposit(script []byte, sats int64) (*wire.MsgTx, error) {
	tx := wire.NewMsgTx(2)
	prevs := map[wire.OutPoint]*wire.TxOut{}
	var in int64
	for _, u := range wc.coins {
		if in >= sats+txFee {
			break
		}
		if _, ok := wc.c.unspent(u.op); !ok {
			continue
		}
		op := u.op
		tx.AddTxIn(wire.NewTxIn(&op, nil, nil))
		prevs[op] = wire.NewTxOut(u.amt, wc.pay)
		in += u.amt
	}
	if in < sats+txFee {
		return nil, userErr("Your wallet holds %s.", fmtCoin(in, wc.cfg.Info.Ticker))
	}
	tx.AddTxOut(wire.NewTxOut(sats, script))
	if ch := in - sats - txFee; ch > 546 {
		tx.AddTxOut(wire.NewTxOut(ch, wc.pay))
	}
	hashes := txscript.NewTxSigHashes(tx, txscript.NewMultiPrevOutFetcher(prevs))
	for i, in := range tx.TxIn {
		p := prevs[in.PreviousOutPoint]
		wit, err := txscript.TaprootWitnessSignature(tx, hashes, i, p.Value, p.PkScript, txscript.SigHashDefault, wc.key)
		if err != nil {
			return nil, err
		}
		tx.TxIn[i].Witness = wit
	}
	return tx, nil
}

// ------------------------------------------------------------------ trading

// Order places a buy or sell. The wallet checks that the exchange's offer is
// exactly the trade asked for, and that its signature on our copy of the new
// state is valid, before signing anything. It then hands over its secret for
// the old state and checks the exchange's secret in return.
func (w *Wallet) Order(chain, side string, sats int64) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, v, err := w.openVault(chain)
	if err != nil {
		return err
	}
	if v.exchFault != "" {
		return userErr("Not trading: %s", v.exchFault)
	}
	us, uh := newSecret()
	var off StateOffer
	if err := call(w.base, "/v1/order", OrderReq{v.id, side, sats, uh}, &off); err != nil {
		return err
	}
	want := v.latest().UserSats - sats
	if side == "buy" {
		want = v.latest().UserSats + sats
	}
	return w.utxoAgree(v, off, want, us, uh)
}

// utxoAgree finishes a Bitcoin/Litecoin state the exchange offered: check
// the split is exactly want for us and the exchange's signature on our copy
// is valid, sign its copy, hand over our old secret, and check its old
// secret in return. (w.mu held.)
func (w *Wallet) utxoAgree(v *wVault, off StateOffer, want int64, us, uh []byte) error {
	l, g := v.latest(), v.gen()
	if off.Index != len(v.states) || off.UserSats != want || off.UserSats+off.ExchSats != g.vaultAmt {
		return userErr("The exchange offered a different split than agreed. Refusing.")
	}
	st, err := v.def.buildState(g, off.Index, len(v.gens)-1, off.Label, off.UserSats, off.ExchSats, off.RHash, uh)
	if err != nil {
		return err
	}
	if !checkLeafSig(st.utx, 0, v.def.eFetch(g), v.def.ms, v.def.exch, off.ExchSigU) {
		return userErr("The exchange's signature on the new state doesn't check out. Nothing was signed.")
	}
	st.exchSigU, st.uSecret = off.ExchSigU, us
	st.userSigE = signLeaf(st.etx, 0, v.def.eFetch(g), v.def.ms, v.key)
	var cr CommitResp
	if err := call(w.base, "/v1/order/commit", CommitReq{v.id, st.Index, st.userSigE, l.uSecret}, &cr); err != nil {
		return err
	}
	v.states = append(v.states, st)
	if !secretMatches(cr.RSecretPrev, l.rHash) {
		// We already revoked our old copy but the exchange didn't revoke its
		// own. Stop trading; the safe move is to exit with the latest state.
		v.exchFault = fmt.Sprintf("the exchange didn't hand over its secret for state #%d", l.Index)
		v.log("Problem: %s. Request an exit.", v.exchFault)
		return userErr("The exchange didn't revoke state #%d. Request an exit.", l.Index)
	}
	l.rSecret, l.revoked = cr.RSecretPrev, true
	v.log("State #%d: %s", st.Index, st.Label)
	return nil
}

// Settle asks the exchange to co-sign the latest split, checks its
// signature on a transaction the wallet built itself, then broadcasts.
func (w *Wallet) Settle(chain string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	wc, v, err := w.openVault(chain)
	if err != nil {
		return err
	}
	g := v.gen()
	tx := v.def.settleTx(g, v.latest())
	fetch := one(v.def.V, g.vaultAmt)
	req := SettleReq{VaultID: v.id}
	var nonce *musigNonce
	if v.def.musig {
		// A fresh nonce for this settlement only; it is never saved.
		if nonce, err = newMusigNonce(v.key); err != nil {
			return err
		}
		req.UserNonce = nonce.pub()
	}
	var sr SettleResp
	if err := call(w.base, "/v1/settle", req, &sr); err != nil {
		return err
	}
	var full *wire.MsgTx
	if v.def.musig {
		// Key path: check the exchange's partial signature on the
		// settlement we built ourselves, add ours, check the result.
		ks, err := v.def.vaultSpend(tx, 0, fetch)
		if err != nil {
			return err
		}
		theirs, ok := ks.verifyPartial(sr.ExchPartial, v.def.exch, sr.ExchNonce, req.UserNonce)
		if !ok {
			return userErr("The exchange's MuSig2 signature for the settlement doesn't check out.")
		}
		mine, err := ks.sign(v.key, nonce, sr.ExchNonce)
		if err != nil {
			return err
		}
		sig, err := ks.combine(mine, theirs)
		if err != nil {
			return userErr("The settlement signature doesn't check out: %v", err)
		}
		full = withKeySig(tx, sig)
	} else {
		if !checkLeafSig(tx, 0, fetch, v.def.ms, v.def.exch, sr.ExchSig) {
			return userErr("The exchange's settlement signature doesn't check out.")
		}
		full = withBoth(tx, v.def.V, v.def.ms, signLeaf(tx, 0, fetch, v.def.ms, v.key), sr.ExchSig)
	}
	if err := wc.c.send(full); err != nil {
		return err
	}
	wc.learn(full)
	v.settleTx, v.closed = full, "settled"
	v.closeTx = full
	return nil
}

// Exit broadcasts the pre-signed exit request.
func (w *Wallet) Exit(chain string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	wc, v, err := w.openVault(chain)
	if err != nil {
		return err
	}
	if err := wc.c.send(v.def.signedExit(v.gen())); err != nil {
		return err
	}
	v.exitSent = true
	return nil
}

// Claim broadcasts the pre-signed pay-back of deposits (after the delay).
func (w *Wallet) Claim(chain string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	wc, v, err := w.openVault(chain)
	if err != nil {
		return err
	}
	for _, g := range v.gens {
		if wc.c.isSeen(g.exitTx) {
			tx := v.def.signedRefund(g)
			if err := wc.c.send(tx); err != nil {
				return err
			}
			wc.learn(tx)
			v.closed = "claimed"
			v.closeTx = tx
			return nil
		}
	}
	return userErr("Request an exit first.")
}

// Cheat publishes the user's copy of whichever state paid the user most
// (a demo of a cheating user; the exchange holds the secret to punish it).
func (w *Wallet) Cheat(chain string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	wc, v, err := w.openVault(chain)
	if err != nil {
		return err
	}
	g := v.gen()
	var pick *pstate
	for _, s := range v.states {
		if s.Gen == len(v.gens)-1 && (pick == nil || s.UserSats > pick.UserSats) {
			pick = s
		}
	}
	tx := withBoth(pick.utx, v.def.E, v.def.ms, signLeaf(pick.utx, 0, v.def.eFetch(g), v.def.ms, v.key), pick.exchSigU)
	if err := wc.c.send(tx); err != nil {
		return err
	}
	v.published, v.publishedBy, v.closed = pick, "user", "published"
	v.closeTx = tx
	return nil
}

// ------------------------------------------------------------------ capacity (splice)

// Capacity asks the exchange to add its own coins. The wallet checks the
// splice (same vault script, exactly sats more, spends our vault once) and
// every exchange signature before signing. Our secret for the last state
// before the splice is only handed over once the splice confirms.
func (w *Wallet) Capacity(chain string, sats int64) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, v, err := w.openVault(chain)
	if err != nil {
		return err
	}
	us, uh := newSecret()
	req := CapacityReq{VaultID: v.id, Sats: sats, UHash: uh}
	var nonce *musigNonce
	if v.def.musig {
		// A fresh nonce for this splice only; it is never saved.
		if nonce, err = newMusigNonce(v.key); err != nil {
			return err
		}
		req.UserNonce = nonce.pub()
	}
	var off CapacityOffer
	if err := call(w.base, "/v1/capacity", req, &off); err != nil {
		return err
	}
	g, l := v.gen(), v.latest()
	tx, err := parseTxHex(off.SpliceTx)
	if err != nil {
		return userErr("The exchange sent a splice that can't be read.")
	}
	if err := v.def.checkSplice(g, tx, sats); err != nil {
		return userErr("Refusing the splice: %v", err)
	}
	if len(off.Prevouts) != len(tx.TxIn) || off.Prevouts[0].Op != g.vaultOut || off.Prevouts[0].Amt != g.vaultAmt {
		return userErr("The splice's inputs don't match what the exchange described.")
	}
	prevs := map[wire.OutPoint]*wire.TxOut{}
	for i, p := range off.Prevouts {
		if tx.TxIn[i].PreviousOutPoint != p.Op {
			return userErr("The splice's inputs don't match what the exchange described.")
		}
		prevs[p.Op] = wire.NewTxOut(p.Amt, p.Script)
	}
	prevs[g.vaultOut] = wire.NewTxOut(g.vaultAmt, v.def.V.pkScript)
	fetch := txscript.NewMultiPrevOutFetcher(prevs)
	fee := capacityFee(sats)
	if off.State.UserSats != l.UserSats-fee || off.State.ExchSats != l.ExchSats+sats+fee || off.State.Index != len(v.states) {
		return userErr("The exchange's new split isn't the one agreed (0.2%% fee from your share). Refusing.")
	}
	ng := v.def.buildGen(wire.OutPoint{Hash: tx.TxHash(), Index: 0}, g.vaultAmt+sats, g.userDep, g.exchDep+sats)
	st, err := v.def.buildState(ng, off.State.Index, len(v.gens), off.State.Label, off.State.UserSats, off.State.ExchSats, off.RHash, uh)
	if err != nil {
		return err
	}
	// The splice spends the vault: through the key path on a MuSig2 vault
	// (the exchange's partial signature), otherwise through the 2-of-2 script.
	var ks *keySpend
	var theirs *musig2.PartialSignature
	spliceOK := false
	if v.def.musig {
		if ks, err = v.def.vaultSpend(tx, 0, fetch); err != nil {
			return err
		}
		theirs, spliceOK = ks.verifyPartial(off.SplicePartial, v.def.exch, off.SpliceNonce, req.UserNonce)
	} else {
		spliceOK = checkLeafSig(tx, 0, fetch, v.def.ms, v.def.exch, off.SpliceSig)
	}
	if !spliceOK ||
		!checkLeafSig(ng.exitTx, 0, v.def.exitFetch(ng), v.def.ms, v.def.exch, off.ExitSig) ||
		!checkLeafSig(ng.refundTx, 0, v.def.eFetch(ng), v.def.dl, v.def.exch, off.RefundSig) ||
		!checkLeafSig(st.utx, 0, v.def.eFetch(ng), v.def.ms, v.def.exch, off.ExchSigU) {
		return userErr("A signature from the exchange doesn't check out. Nothing was signed.")
	}
	ng.exitSigX, ng.refundSigX, st.exchSigU, st.uSecret = off.ExitSig, off.RefundSig, off.ExchSigU, us
	commit := CapacityCommitReq{VaultID: v.id}
	var witness wire.TxWitness
	if v.def.musig {
		mine, err := ks.sign(v.key, nonce, off.SpliceNonce)
		if err != nil {
			return err
		}
		sig, err := ks.combine(mine, theirs)
		if err != nil {
			return userErr("The splice signature doesn't check out: %v", err)
		}
		commit.SplicePartial, witness = encodePartial(mine), wire.TxWitness{sig}
	} else {
		commit.SpliceSig = signLeaf(tx, 0, fetch, v.def.ms, v.key)
		witness = wire.TxWitness{off.SpliceSig, commit.SpliceSig, v.def.ms.Script, v.def.V.controlBlock(v.def.ms)}
	}
	ng.exitSigU = signLeaf(ng.exitTx, 0, v.def.exitFetch(ng), v.def.ms, v.key)
	ng.refundSigU = signLeaf(ng.refundTx, 0, v.def.eFetch(ng), v.def.dl, v.key)
	st.userSigE = signLeaf(st.etx, 0, v.def.eFetch(ng), v.def.ms, v.key)
	commit.ExitSig, commit.RefundSig, commit.UserSigE = ng.exitSigU, ng.refundSigU, st.userSigE
	if err := call(w.base, "/v1/capacity/commit", commit, nil); err != nil {
		return err
	}
	tx.TxIn[0].Witness = witness
	v.gens = append(v.gens, ng)
	v.states = append(v.states, st)
	v.spliceTx, v.spliceFinished = tx, false
	v.log("The exchange is adding %d; secrets for state #%d are swapped once the splice confirms", sats, l.Index)
	return nil
}

// finishSplice swaps the secrets for the last pre-splice state, once the
// splice is confirmed.
func (w *Wallet) finishSplice(wc *wChain, v *wVault) {
	if v.spliceTx == nil || v.spliceFinished || v.exchFault != "" || wc.c.confs(v.spliceTx) == 0 {
		return
	}
	prev := v.states[len(v.states)-2]
	var cr CommitResp
	if err := call(w.base, "/v1/capacity/finish", CapacityFinishReq{v.id, prev.uSecret}, &cr); err != nil {
		return
	}
	v.spliceFinished = true
	if !secretMatches(cr.RSecretPrev, prev.rHash) {
		v.exchFault = fmt.Sprintf("the exchange didn't hand over its secret for state #%d", prev.Index)
		v.log("Problem: %s. Request an exit.", v.exchFault)
		return
	}
	prev.rSecret, prev.revoked = cr.RSecretPrev, true
}

// ------------------------------------------------------------------ watchtower

// Tick is the user's watchtower and wallet upkeep. It runs after every block.
func (w *Wallet) Tick() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, wc := range w.chains {
		w.scan(wc, false)
		if v := wc.vault; v != nil {
			w.finishSplice(wc, v)
			w.watch(wc, v)
		}
		w.autoBump(wc)
	}
	w.ethTick()
	ids := []string{"btc", "ltc"}
	for _, c := range evmChains {
		ids = append(ids, c.ID)
	}
	for _, c := range ids {
		if _, err := w.vaultID(c); err == nil {
			w.signFills(c)
		}
	}
	w.save()
}

func (w *Wallet) watch(wc *wChain, v *wVault) {
	c := wc.c
	if v.closed == "" {
		for gi, g := range v.gens {
			if !c.isSeen(g.exitTx) {
				continue
			}
			for _, s := range v.states {
				if s.Gen != gi {
					continue
				}
				if c.isSeen(s.etx) {
					v.published, v.publishedBy, v.closed = s, "exchange", "published"
					v.closeTx = s.etx
					wc.learn(s.etx)
					v.log("The exchange published state #%d", s.Index)
				}
				if c.isSeen(s.utx) {
					v.published, v.publishedBy, v.closed = s, "user", "published"
					v.closeTx = s.utx
				}
			}
			if v.closed != "" || !w.WT || c.confs(g.exitTx) == 0 {
				break
			}
			// Nobody has published. If our latest state in this generation
			// pays us more than our deposit, publish it: the pay-back after
			// the window would only return deposits.
			var l *pstate
			for _, s := range v.states {
				if s.Gen == gi {
					l = s
				}
			}
			if l != nil && l.UserSats > g.userDep {
				tx := withBoth(l.utx, v.def.E, v.def.ms, signLeaf(l.utx, 0, v.def.eFetch(g), v.def.ms, v.key), l.exchSigU)
				if c.send(tx) == nil {
					v.published, v.publishedBy, v.closed = l, "user", "published"
					v.closeTx = tx
					v.log("Your watchtower published your latest state #%d", l.Index)
				}
			}
			break
		}
		return
	}
	if v.closed != "published" || v.sweepKind != "" {
		return
	}
	s := v.published
	// Notice when the exchange took a delayed share (ours with our secret,
	// or its own after D2), so the page can say what happened.
	if v.publishedBy == "user" && s.uIdx >= 0 && c.confs(s.utx) > 0 {
		if _, ok := c.unspent(s.uOut()); !ok {
			v.sweepKind = "exchange-penalty"
			v.log("The exchange used your revocation secret #%d to take your share of old state #%d", s.Index, s.Index)
			return
		}
	}
	if v.publishedBy == "exchange" && s.rIdx >= 0 && c.confs(s.etx) > 0 && !(w.WT && s.revoked) {
		if _, ok := c.unspent(s.rOut()); !ok {
			v.sweepKind = "exchange"
			v.log("The exchange collected its share of state #%d", s.Index)
			return
		}
	}
	if v.publishedBy == "exchange" && w.WT && s.revoked && s.rIdx >= 0 && c.confs(s.etx) > 0 {
		if _, ok := c.unspent(s.rOut()); ok {
			tx := spendLeaf(s.rOut(), s.rAmt, s.R.out, s.R.rev, 0, v.key, v.def.userPay, s.rSecret)
			if c.send(tx) == nil {
				wc.learn(tx)
				v.sweepKind = "penalty"
				v.log("Penalty sweep: took the exchange's share of old state #%d", s.Index)
			}
		}
	}
	if v.publishedBy == "user" && s.uIdx >= 0 && c.confs(s.utx) >= v.def.d2 {
		if _, ok := c.unspent(s.uOut()); ok {
			tx := spendLeaf(s.uOut(), s.uAmt, s.U.out, s.U.own, v.def.d2, v.key, v.def.userPay)
			if c.send(tx) == nil {
				wc.learn(tx)
				v.sweepKind = "user"
				v.log("Collected your share of state #%d", s.Index)
			}
		}
	}
}

// VaultView is a summary for tests and the page.
func (w *Wallet) Vault(chain string) (userSats, exchSats int64, states int, closed, sweep string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	v := w.chains[chain].vault
	if v == nil {
		return
	}
	l := v.latest()
	return l.UserSats, l.ExchSats, len(v.states), v.closed, v.sweepKind
}

// AddCash asks the exchange for $10,000 of test cash.
func (w *Wallet) AddCash() (int64, error) {
	var r CashResp
	err := call(w.base, "/v1/cash", CashReq{w.Account}, &r)
	return r.CashCents, err
}

// Admin changes the exchange's behaviour or price (test networks only).
func (w *Wallet) Admin(chain, mode string, usd int64) error {
	return call(w.base, "/v1/admin", AdminReq{chain, mode, usd}, nil)
}
