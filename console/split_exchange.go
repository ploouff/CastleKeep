package main

// The exchange as a separate program (Bitcoin and Litecoin). It holds only
// the exchange's keys, its wallet, prices and the cash ledger. It never sees
// the user's private keys: it checks every signature the wallet sends, and
// reads the chain itself to react to exits.
//
// Test networks only. Not audited.

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"sync"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

type xChain struct {
	id    string
	cfg   UTXOConfig
	c     *chainConn
	key   *btcec.PrivateKey // the exchange's wallet key
	pay   []byte
	coins []utxo
	price int64
	mode  string // honest, offline, malicious
}

type xVault struct {
	id, account, chain string
	key                *btcec.PrivateKey // this vault's signing key
	def                *vaultDef
	gens               []*vgen
	states             []*pstate
	opened             bool // the wallet sent its signatures; the deposit may follow
	pend               *xPending
	spliceTx           *wire.MsgTx // the latest splice, if any
	spliceFinished     bool
	settleTx           *wire.MsgTx
	settling           bool // a settlement was signed: no more trades, or the old split could be settled later

	// after an exit
	published   *pstate
	publishedBy string
	closed      string // "settled", "claimed", "published"
	sweepKind   string
	events      []string
}

type xPending struct {
	done      func() // run once the wallet's signature arrives (order book fills)
	kind      string // "order" or "splice"
	side      string
	sats      int64
	cost      int64
	st        *pstate
	gen       *vgen
	splice    *wire.MsgTx
	fetch     txscript.PrevOutputFetcher
	spliceSig []byte
}

// Exchange is the exchange server's state.
type Exchange struct {
	mu        sync.Mutex
	chains    map[string]*xChain
	vaults    map[string]*xVault
	cash      map[string]int64 // account -> cents
	tamper    bool             // tests only: corrupt the next signature sent to the wallet
	eth       *xEth
	ethVaults map[string]*xEthVault
	books     map[string]*book
	orderSeq  int
}

// NewExchange connects to each chain's node. rpc maps chain id to the
// node's RPC address.
func NewExchange(cfgs map[string]UTXOConfig, rpc map[string]string) (*Exchange, error) {
	x := &Exchange{chains: map[string]*xChain{}, vaults: map[string]*xVault{}, cash: map[string]int64{}}
	for id, cfg := range cfgs {
		c, err := dialChain(rpc[id])
		if err != nil {
			return nil, err
		}
		k, _ := btcec.NewPrivateKey()
		x.chains[id] = &xChain{id: id, cfg: cfg, c: c, key: k, pay: walletScript(k), price: cfg.DefaultPrice, mode: "honest"}
	}
	return x, nil
}

// WalletScript is where test coins for the exchange's own wallet go.
func (x *Exchange) WalletScript(chain string) []byte { return x.chains[chain].pay }

// AddCoins records a coin paid to the exchange's wallet.
func (x *Exchange) AddCoins(chain string, op wire.OutPoint, amt int64) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.chains[chain].coins = append(x.chains[chain].coins, utxo{op, amt})
}

func (x *Exchange) Handler() http.Handler {
	m := http.NewServeMux()
	m.Handle("/v1/open/keys", handle(x.openKeys))
	m.Handle("/v1/open/start", handle(x.openStart))
	m.Handle("/v1/open/finish", handle(x.openFinish))
	m.Handle("/v1/order", handle(x.order))
	m.Handle("/v1/order/commit", handle(x.commit))
	m.Handle("/v1/settle", handle(x.settle))
	m.Handle("/v1/capacity", handle(x.capacity))
	m.Handle("/v1/capacity/commit", handle(x.capacityCommit))
	m.Handle("/v1/capacity/finish", handle(x.capacityFinish))
	m.Handle("/v1/cash", handle(x.addCash))
	m.Handle("/v1/cashbalance", handle(func(r CashReq) (CashResp, error) { return CashResp{x.Cash(r.Account)}, nil }))
	m.Handle("/v1/admin", handle(x.admin))
	m.Handle("/v1/eth/info", handle(x.ethInfo))
	m.Handle("/v1/eth/open", handle(x.ethOpen))
	m.Handle("/v1/eth/order", handle(x.ethOrder))
	m.Handle("/v1/eth/commit", handle(x.ethCommit))
	m.Handle("/v1/eth/capacity", handle(x.ethCapacity))
	m.Handle("/v1/eth/capacity/offer", handle(x.ethCapacityOffer))
	m.Handle("/v1/eth/settle", handle(x.ethSettle))
	m.Handle("/v1/swap", handle(x.swap))
	m.Handle("/v1/book/place", handle(x.place))
	m.Handle("/v1/book/cancel", handle(x.cancel))
	m.Handle("/v1/book", handle(x.bookView))
	m.Handle("/v1/fills/next", handle(x.nextFill))
	m.Handle("/v1/info", handle(x.info))
	m.Handle("/v1/state", handle(x.state))
	m.Handle("/v1/tick", handle(func(empty) (empty, error) { x.Tick(); return empty{}, nil }))
	return m
}

func (v *xVault) log(f string, a ...any) { v.events = append(v.events, fmt.Sprintf(f, a...)) }
func (v *xVault) gen() *vgen             { return v.gens[len(v.gens)-1] }
func (v *xVault) latest() *pstate        { return v.states[len(v.states)-1] }

func (x *Exchange) vault(id string) (*xVault, *xChain, error) {
	v, ok := x.vaults[id]
	if !ok {
		return nil, nil, userErr("Unknown vault.")
	}
	return v, x.chains[v.chain], nil
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ------------------------------------------------------------------ opening

func (x *Exchange) openKeys(r OpenKeysReq) (OpenKeysResp, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	xc, ok := x.chains[r.Chain]
	if !ok {
		return OpenKeysResp{}, userErr("Unknown chain.")
	}
	if xc.mode == "offline" {
		return OpenKeysResp{}, userErr("The exchange is offline.")
	}
	k, _ := btcec.NewPrivateKey()
	def, err := newVaultDef(r.UserPub, xonly(k), r.UserPay, xc.pay, xc.cfg.ExitDelay, xc.cfg.PenaltyDelay)
	if err != nil {
		return OpenKeysResp{}, userErr("%v", err)
	}
	v := &xVault{id: newID(), account: r.Account, chain: r.Chain, key: k, def: def}
	x.vaults[v.id] = v
	return OpenKeysResp{VaultID: v.id, ExchPub: xonly(k), ExchPay: xc.pay, Delay: def.delay, D2: def.d2}, nil
}

func (x *Exchange) openStart(r OpenStartReq) (OpenStartResp, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	v, xc, err := x.vault(r.VaultID)
	if err != nil {
		return OpenStartResp{}, err
	}
	if xc.mode == "offline" {
		return OpenStartResp{}, userErr("The exchange is offline.")
	}
	if len(v.gens) > 0 {
		return OpenStartResp{}, userErr("This vault is already set up.")
	}
	if r.Amt < 100_000 {
		return OpenStartResp{}, userErr("Lock at least 0.001.")
	}
	g := v.def.buildGen(r.FundOut, r.Amt, r.Amt, 0)
	rs, rh := newSecret()
	st, err := v.def.buildState(g, 0, 0, "Opening balance", r.Amt, 0, rh, r.UHash)
	if err != nil {
		return OpenStartResp{}, userErr("%v", err)
	}
	st.rSecret = rs
	g.exitSigX = signLeaf(g.exitTx, 0, v.def.exitFetch(g), v.def.ms, v.key)
	g.refundSigX = signLeaf(g.refundTx, 0, v.def.eFetch(g), v.def.dl, v.key)
	st.exchSigU = signLeaf(st.utx, 0, v.def.eFetch(g), v.def.ms, v.key)
	v.gens, v.states = []*vgen{g}, []*pstate{st}
	return OpenStartResp{ExitSig: g.exitSigX, RefundSig: g.refundSigX, RHash: rh, ExchSigU: x.maybeTamper(st.exchSigU)}, nil
}

func (x *Exchange) openFinish(r OpenFinishReq) (empty, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	v, _, err := x.vault(r.VaultID)
	if err != nil || len(v.gens) == 0 {
		return empty{}, userErr("Unknown vault.")
	}
	g, st := v.gen(), v.latest()
	if !checkLeafSig(g.exitTx, 0, v.def.exitFetch(g), v.def.ms, v.def.user, r.ExitSig) ||
		!checkLeafSig(g.refundTx, 0, v.def.eFetch(g), v.def.dl, v.def.user, r.RefundSig) ||
		!checkLeafSig(st.etx, 0, v.def.eFetch(g), v.def.ms, v.def.user, r.UserSigE) {
		return empty{}, userErr("A signature from the wallet doesn't check out.")
	}
	g.exitSigU, g.refundSigU, st.userSigE = r.ExitSig, r.RefundSig, r.UserSigE
	v.opened = true
	v.log("Vault opened with %d, waiting for the deposit", g.vaultAmt)
	return empty{}, nil
}

// ------------------------------------------------------------------ trading

// tradable: the vault's current output is confirmed, no exit has started,
// nothing is pending and every state but the latest is revoked.
func (x *Exchange) tradable(v *xVault, xc *xChain) error {
	if !v.opened || v.closed != "" {
		return userErr("This vault isn't open.")
	}
	if xc.mode == "offline" {
		return userErr("The exchange is offline and isn't accepting orders.")
	}
	g := v.gen()
	if n, _ := xc.c.seen(g.vaultOut.Hash); n == 0 {
		return userErr("The latest deposit into this vault hasn't confirmed yet. Mine a block first.")
	}
	for _, gg := range v.gens {
		if xc.c.isSeen(gg.exitTx) {
			return userErr("Trading is frozen while an exit request is in progress.")
		}
	}
	for _, s := range v.states[:len(v.states)-1] {
		if !s.revoked {
			return userErr("An older state hasn't been revoked yet.")
		}
	}
	return nil
}

func (x *Exchange) order(r OrderReq) (StateOffer, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	v, xc, err := x.vault(r.VaultID)
	if err != nil {
		return StateOffer{}, err
	}
	if v.settling {
		return StateOffer{}, userErr("A settlement was signed for this vault, so it can't trade any more.")
	}
	if err := x.tradable(v, xc); err != nil {
		return StateOffer{}, err
	}
	l := v.latest()
	if r.Sats <= 0 {
		return StateOffer{}, userErr("Enter an amount above zero.")
	}
	cost := orderCost(r.Sats, xc.price)
	user, exch := l.UserSats-r.Sats, l.ExchSats+r.Sats
	label := fmt.Sprintf("Sold %s at %s", fmtCoin(r.Sats, xc.cfg.Info.Ticker), fmtUSD(xc.price*100))
	switch r.Side {
	case "sell":
		if r.Sats > l.UserSats {
			return StateOffer{}, userErr("You only have %s in the vault to sell.", fmtCoin(l.UserSats, xc.cfg.Info.Ticker))
		}
	case "buy":
		if r.Sats > l.ExchSats {
			return StateOffer{}, userErr("The exchange's side of the vault holds %s, so that's the most you can buy.", fmtCoin(l.ExchSats, xc.cfg.Info.Ticker))
		}
		if cost > x.cash[v.account] {
			return StateOffer{}, userErr("That costs %s and you have %s in cash.", fmtUSD(cost), fmtUSD(x.cash[v.account]))
		}
		user, exch = l.UserSats+r.Sats, l.ExchSats-r.Sats
		label = fmt.Sprintf("Bought %s at %s", fmtCoin(r.Sats, xc.cfg.Info.Ticker), fmtUSD(xc.price*100))
	default:
		return StateOffer{}, userErr("Unknown order side.")
	}
	g := v.gen()
	rs, rh := newSecret()
	st, err := v.def.buildState(g, len(v.states), len(v.gens)-1, label, user, exch, rh, r.UHash)
	if err != nil {
		return StateOffer{}, userErr("%v", err)
	}
	st.rSecret = rs
	st.exchSigU = signLeaf(st.utx, 0, v.def.eFetch(g), v.def.ms, v.key)
	v.pend = &xPending{kind: "order", side: r.Side, sats: r.Sats, cost: cost, st: st}
	return StateOffer{Index: st.Index, Label: label, UserSats: user, ExchSats: exch, RHash: rh, ExchSigU: x.maybeTamper(st.exchSigU)}, nil
}

func (x *Exchange) commit(r CommitReq) (CommitResp, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	v, _, err := x.vault(r.VaultID)
	if err != nil {
		return CommitResp{}, err
	}
	p := v.pend
	if p == nil || p.kind != "order" || p.st.Index != r.Index {
		return CommitResp{}, userErr("There's no matching order waiting for your signature.")
	}
	g, prev := v.gen(), v.latest()
	if !checkLeafSig(p.st.etx, 0, v.def.eFetch(g), v.def.ms, v.def.user, r.UserSigE) {
		return CommitResp{}, userErr("Your signature on the new state doesn't check out.")
	}
	if !secretMatches(r.USecretPrev, prev.uHash) {
		return CommitResp{}, userErr("That isn't your secret for state #%d.", prev.Index)
	}
	if p.side == "buy" || p.side == "fill-buy" {
		if p.cost > x.cash[v.account] {
			return CommitResp{}, userErr("Not enough cash any more.")
		}
		x.cash[v.account] -= p.cost
	} else {
		x.cash[v.account] += p.cost
	}
	p.st.userSigE = r.UserSigE
	prev.uSecret, prev.revoked = r.USecretPrev, true
	v.states = append(v.states, p.st)
	v.pend = nil
	if p.done != nil {
		p.done()
	}
	v.log("State #%d signed: %s", p.st.Index, p.st.Label)
	return CommitResp{RSecretPrev: prev.rSecret, CashCents: x.cash[v.account]}, nil
}

func (x *Exchange) settle(r SettleReq) (SettleResp, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	v, xc, err := x.vault(r.VaultID)
	if err != nil {
		return SettleResp{}, err
	}
	switch xc.mode {
	case "offline":
		return SettleResp{}, userErr("The exchange is offline. Request an exit instead.")
	case "malicious":
		return SettleResp{}, userErr("The exchange won't co-sign a settlement. Request an exit instead.")
	}
	if err := x.tradable(v, xc); err != nil {
		return SettleResp{}, err
	}
	g := v.gen()
	tx := v.def.settleTx(g, v.latest())
	v.settleTx, v.settling = tx, true
	v.settling = true
	return SettleResp{ExchSig: signLeaf(tx, 0, one(v.def.V, g.vaultAmt), v.def.ms, v.key)}, nil
}

func (x *Exchange) addCash(r CashReq) (CashResp, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.cash[r.Account] += testCashCents
	return CashResp{x.cash[r.Account]}, nil
}

func (x *Exchange) admin(r AdminReq) (empty, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if r.Chain == "eth" && x.eth != nil {
		switch r.Mode {
		case "":
		case "honest", "offline", "malicious":
			x.eth.mode = r.Mode
		default:
			return empty{}, userErr("Unknown mode.")
		}
		if r.USD > 0 {
			x.eth.price = r.USD
		}
		return empty{}, nil
	}
	xc, ok := x.chains[r.Chain]
	if !ok {
		return empty{}, userErr("Unknown chain.")
	}
	switch r.Mode {
	case "":
	case "honest", "offline", "malicious":
		xc.mode = r.Mode
	default:
		return empty{}, userErr("Unknown mode.")
	}
	if r.USD > 0 {
		xc.price = r.USD
	}
	return empty{}, nil
}

func (x *Exchange) maybeTamper(sig []byte) []byte {
	if !x.tamper {
		return sig
	}
	x.tamper = false
	bad := append([]byte(nil), sig...)
	bad[10] ^= 1
	return bad
}

// ------------------------------------------------------------------ capacity (splice)

func (x *Exchange) capacity(r CapacityReq) (CapacityOffer, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	v, xc, err := x.vault(r.VaultID)
	if err != nil {
		return CapacityOffer{}, err
	}
	if v.settling {
		return CapacityOffer{}, userErr("A settlement was signed for this vault, so it can't trade any more.")
	}
	if err := x.tradable(v, xc); err != nil {
		return CapacityOffer{}, err
	}
	if r.Sats < 100_000 || r.Sats > 5*unit {
		return CapacityOffer{}, userErr("Ask for between 0.001 and 5.")
	}
	fee := capacityFee(r.Sats)
	l, g := v.latest(), v.gen()
	if fee > l.UserSats {
		return CapacityOffer{}, userErr("The fee is more than your share of the vault.")
	}
	var ins []spliceIn
	var in int64
	for _, u := range xc.coins {
		if in >= r.Sats+txFee {
			break
		}
		if _, ok := xc.c.unspent(u.op); ok {
			ins = append(ins, spliceIn{u.op, u.amt, xc.pay})
			in += u.amt
		}
	}
	tx, fetch, err := v.def.buildSplice(g, ins, xc.pay, r.Sats)
	if err != nil {
		return CapacityOffer{}, userErr("The exchange's wallet doesn't hold enough right now.")
	}
	hashes := txscript.NewTxSigHashes(tx, fetch)
	for i := 1; i < len(tx.TxIn); i++ {
		w, err := txscript.TaprootWitnessSignature(tx, hashes, i, ins[i-1].Amt, xc.pay, txscript.SigHashDefault, xc.key)
		if err != nil {
			return CapacityOffer{}, err
		}
		tx.TxIn[i].Witness = w
	}
	ng := v.def.buildGen(wire.OutPoint{Hash: tx.TxHash(), Index: 0}, g.vaultAmt+r.Sats, g.userDep, g.exchDep+r.Sats)
	rs, rh := newSecret()
	label := fmt.Sprintf("Exchange locked %s (fee %s)", fmtCoin(r.Sats, xc.cfg.Info.Ticker), fmtCoin(fee, xc.cfg.Info.Ticker))
	st, err := v.def.buildState(ng, len(v.states), len(v.gens), label, l.UserSats-fee, l.ExchSats+r.Sats+fee, rh, r.UHash)
	if err != nil {
		return CapacityOffer{}, userErr("%v", err)
	}
	st.rSecret = rs
	ng.exitSigX = signLeaf(ng.exitTx, 0, v.def.exitFetch(ng), v.def.ms, v.key)
	ng.refundSigX = signLeaf(ng.refundTx, 0, v.def.eFetch(ng), v.def.dl, v.key)
	st.exchSigU = signLeaf(st.utx, 0, v.def.eFetch(ng), v.def.ms, v.key)
	spliceSig := signLeaf(tx, 0, fetch, v.def.ms, v.key)
	v.pend = &xPending{kind: "splice", sats: r.Sats, st: st, gen: ng, splice: tx, fetch: fetch, spliceSig: spliceSig}
	prevs := append([]spliceIn{{g.vaultOut, g.vaultAmt, v.def.V.pkScript}}, ins...)
	return CapacityOffer{SpliceTx: txHex(tx), Prevouts: prevs, SpliceSig: spliceSig, ExitSig: ng.exitSigX,
		RefundSig: ng.refundSigX, RHash: rh, ExchSigU: x.maybeTamper(st.exchSigU),
		State: StateOffer{Index: st.Index, Label: label, UserSats: st.UserSats, ExchSats: st.ExchSats, RHash: rh, ExchSigU: st.exchSigU}}, nil
}

func (x *Exchange) capacityCommit(r CapacityCommitReq) (empty, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	v, xc, err := x.vault(r.VaultID)
	if err != nil {
		return empty{}, err
	}
	p := v.pend
	if p == nil || p.kind != "splice" {
		return empty{}, userErr("There's no splice waiting for your signature.")
	}
	ng, st := p.gen, p.st
	if !checkLeafSig(p.splice, 0, p.fetch, v.def.ms, v.def.user, r.SpliceSig) ||
		!checkLeafSig(ng.exitTx, 0, v.def.exitFetch(ng), v.def.ms, v.def.user, r.ExitSig) ||
		!checkLeafSig(ng.refundTx, 0, v.def.eFetch(ng), v.def.dl, v.def.user, r.RefundSig) ||
		!checkLeafSig(st.etx, 0, v.def.eFetch(ng), v.def.ms, v.def.user, r.UserSigE) {
		return empty{}, userErr("A signature from the wallet doesn't check out.")
	}
	tx := p.splice.Copy()
	tx.TxIn[0].Witness = wire.TxWitness{p.spliceSig, r.SpliceSig, v.def.ms.Script, v.def.V.controlBlock(v.def.ms)}
	if err := xc.c.send(tx); err != nil {
		return empty{}, err
	}
	ng.exitSigU, ng.refundSigU, st.userSigE = r.ExitSig, r.RefundSig, r.UserSigE
	v.gens = append(v.gens, ng)
	v.states = append(v.states, st)
	v.spliceTx, v.spliceFinished, v.pend = tx, false, nil
	if len(tx.TxOut) > 1 {
		xc.coins = append(xc.coins, utxo{wire.OutPoint{Hash: tx.TxHash(), Index: 1}, tx.TxOut[1].Value})
	}
	v.log("Splice sent: the exchange added %d", p.sats)
	return empty{}, nil
}

// capacityFinish swaps the secrets for the last state before the splice,
// once the splice has confirmed.
func (x *Exchange) capacityFinish(r CapacityFinishReq) (CommitResp, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	v, xc, err := x.vault(r.VaultID)
	if err != nil {
		return CommitResp{}, err
	}
	if v.spliceTx == nil || v.spliceFinished {
		return CommitResp{}, userErr("There's no splice to finish.")
	}
	if xc.c.confs(v.spliceTx) == 0 {
		return CommitResp{}, userErr("The splice hasn't confirmed yet.")
	}
	prev := v.states[len(v.states)-2]
	if !secretMatches(r.USecretPrev, prev.uHash) {
		return CommitResp{}, userErr("That isn't your secret for state #%d.", prev.Index)
	}
	prev.uSecret, prev.revoked = r.USecretPrev, true
	v.spliceFinished = true
	return CommitResp{RSecretPrev: prev.rSecret, CashCents: x.cash[v.account]}, nil
}

// ------------------------------------------------------------------ watchtower

// Tick is the exchange's watchtower. It runs after every block.
func (x *Exchange) Tick() {
	x.mu.Lock()
	defer x.mu.Unlock()
	for _, v := range x.vaults {
		if v.opened && len(v.gens) > 0 {
			x.watch(v, x.chains[v.chain])
		}
	}
	x.ethTick()
}

func (x *Exchange) watch(v *xVault, xc *xChain) {
	c := xc.c
	if v.closed == "" {
		if c.isSeen(v.settleTx) {
			v.closed = "settled"
			return
		}
		for gi, g := range v.gens {
			if !c.isSeen(g.exitTx) {
				continue
			}
			if c.isSeen(g.refundTx) {
				v.closed = "claimed"
				return
			}
			for _, s := range v.states {
				if s.Gen != gi {
					continue
				}
				if c.isSeen(s.utx) {
					v.published, v.publishedBy, v.closed = s, "user", "published"
					v.log("The user published state #%d", s.Index)
				}
				if c.isSeen(s.etx) {
					v.published, v.publishedBy, v.closed = s, "exchange", "published"
				}
			}
			if v.closed != "" || xc.mode == "offline" || c.confs(g.exitTx) == 0 {
				break
			}
			// Answer the exit with this generation's latest state (or, when
			// malicious, whichever of its states pays the exchange most).
			var pick *pstate
			for _, s := range v.states {
				if s.Gen != gi || s.userSigE == nil {
					continue
				}
				if pick == nil || xc.mode != "malicious" || s.ExchSats > pick.ExchSats {
					pick = s
				}
			}
			if pick == nil {
				break
			}
			tx := withBoth(pick.etx, v.def.E, v.def.ms, pick.userSigE, signLeaf(pick.etx, 0, v.def.eFetch(g), v.def.ms, v.key))
			if c.send(tx) == nil {
				v.published, v.publishedBy, v.closed = pick, "exchange", "published"
				v.log("Published state #%d", pick.Index)
			}
			break
		}
		return
	}
	if v.closed != "published" || v.sweepKind != "" || xc.mode == "offline" {
		return
	}
	s := v.published
	if v.publishedBy == "user" && s.revoked && s.uIdx >= 0 && c.confs(s.utx) > 0 {
		if _, ok := c.unspent(s.uOut()); ok {
			tx := spendLeaf(s.uOut(), s.uAmt, s.U.out, s.U.rev, 0, v.key, v.def.exchPay, s.uSecret)
			if c.send(tx) == nil {
				v.sweepKind = "exchange-penalty"
				v.log("Penalty sweep of the user's share of old state #%d", s.Index)
			}
		}
	}
	if v.publishedBy == "exchange" && s.rIdx >= 0 && c.confs(s.etx) >= v.def.d2 {
		if _, ok := c.unspent(s.rOut()); ok {
			tx := spendLeaf(s.rOut(), s.rAmt, s.R.out, s.R.own, v.def.d2, v.key, v.def.exchPay)
			if c.send(tx) == nil {
				v.sweepKind = "exchange"
				v.log("Collected the exchange's share of state #%d", s.Index)
			}
		}
	}
}

// Cash reports an account's cash balance.
func (x *Exchange) Cash(account string) int64 {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.cash[account]
}

// SetTamper makes the next signature sent to a wallet invalid (tests only).
func (x *Exchange) SetTamper() { x.mu.Lock(); x.tamper = true; x.mu.Unlock() }
