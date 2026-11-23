package main

// Ethereum in the separate exchange and wallet programs. Each side has its
// own key and checks the other's signatures with ecrecover, as the contract
// does. Message order for a new state (a trade, the opening state, or the
// state after the exchange adds buying capacity):
//
//  1. the exchange proposes the split (nonce, user's share);
//  2. the wallet checks it is exactly what was asked for, signs it, sends it;
//  3. the exchange checks that signature, then sends its own.
//
// No revocation is needed: during an exit the contract only accepts a newer
// state. Settling needs a separate settle signature from both, which the
// exchange only gives for the latest state, and after which it stops trading
// on that vault.
//
// Test networks only. Not audited.

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/btcsuite/btcd/btcec/v2"
)

// ------------------------------------------------------------------ messages

type EthInfoResp struct {
	ExchAddr  string
	ExitDelay int64
}
type EthOpenReq struct {
	Account, VaultID string
}
type EthStateOffer struct {
	VaultID            string
	Nonce              int
	Label              string
	UserSats, ExchSats int64
}
type EthCommitReq struct {
	VaultID string
	Nonce   int
	UserSig []byte
}
type EthCommitResp struct {
	ExchSig   []byte
	CashCents int64
}
type EthOrderReq struct {
	VaultID, Side string
	Sats          int64
}
type EthCapacityReq struct {
	VaultID string
	Sats    int64
}
type EthSettleReq struct {
	VaultID string
	UserSig []byte // on the settle message for the latest state
}
type EthSettleResp struct{ ExchSig []byte }

// ------------------------------------------------------------------ exchange side

type xEth struct {
	c     *EthEngine // RPC and contract helpers only
	key   *btcec.PrivateKey
	addr  string
	delay int64
	price int64
	mode  string
}

type xEthVault struct {
	id       *big.Int
	account  string
	user     string
	states   []*ethState
	pend     *xEthPend
	fundTx   string
	fundAmt  int64
	settling bool
	exchLast int
	exchPend string
	closed   bool
	events   []string
}

type xEthPend struct {
	done func() // run once the wallet's signature arrives (order book fills)
	kind string // "open", "order", "fund"
	side string
	cost int64
	st   *ethState
}

func (v *xEthVault) latest() *ethState { return v.states[len(v.states)-1] }
func (v *xEthVault) log(f string, a ...any) {
	v.events = append(v.events, fmt.Sprintf(f, a...))
}

// AttachEth connects the exchange to the Ethereum node and contract.
func (x *Exchange) AttachEth(ep EthEndpoint, price int64) string {
	x.mu.Lock()
	defer x.mu.Unlock()
	k, _ := btcec.NewPrivateKey()
	x.eth = &xEth{c: newEthLight(ep), key: k, addr: ethAddress(k), delay: ep.ExitDelay, price: price, mode: "honest"}
	x.ethVaults = map[string]*xEthVault{}
	return x.eth.addr
}

func (x *Exchange) ethVault(id string) (*xEthVault, error) {
	if x.eth == nil {
		return nil, userErr("This exchange has no Ethereum.")
	}
	v, ok := x.ethVaults[id]
	if !ok {
		return nil, userErr("Unknown vault.")
	}
	return v, nil
}

func (x *Exchange) ethInfo(empty) (EthInfoResp, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.eth == nil {
		return EthInfoResp{}, userErr("This exchange has no Ethereum.")
	}
	return EthInfoResp{x.eth.addr, x.eth.delay}, nil
}

// ethOpen: the user has already locked ETH with open(); the deposit is safe
// without the exchange (finalize refunds it). The exchange checks the vault
// on the chain and proposes state #0.
func (x *Exchange) ethOpen(r EthOpenReq) (EthStateOffer, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.eth == nil {
		return EthStateOffer{}, userErr("This exchange has no Ethereum.")
	}
	if x.eth.mode == "offline" {
		return EthStateOffer{}, userErr("The exchange is offline.")
	}
	if _, ok := x.ethVaults[r.VaultID]; ok {
		return EthStateOffer{}, userErr("This vault is already registered.")
	}
	id, ok := new(big.Int).SetString(r.VaultID, 10)
	if !ok {
		return EthStateOffer{}, userErr("Bad vault number.")
	}
	cv, err := x.eth.c.readVaultID(id)
	if err != nil || cv.amount == 0 {
		return EthStateOffer{}, userErr("That vault isn't on the chain yet. Mine a block first.")
	}
	if !strings.EqualFold(cv.exchange, x.eth.addr) || cv.exitDelay != x.eth.delay || cv.closed || cv.exitStart != 0 {
		return EthStateOffer{}, userErr("That vault isn't set up with this exchange's address and exit delay.")
	}
	st := &ethState{Index: 0, Label: "Opening balance", UserSats: cv.amount}
	v := &xEthVault{id: id, account: r.Account, user: cv.user, exchLast: -1}
	v.pend = &xEthPend{kind: "open", st: st}
	x.ethVaults[r.VaultID] = v
	return EthStateOffer{r.VaultID, 0, st.Label, st.UserSats, 0}, nil
}

func (x *Exchange) ethTradable(v *xEthVault) error {
	if len(v.states) == 0 || v.closed {
		return userErr("This vault isn't open.")
	}
	if x.eth.mode == "offline" {
		return userErr("The exchange is offline and isn't accepting orders.")
	}
	if v.settling {
		return userErr("A settlement was signed for this vault, so it can't trade any more.")
	}
	if v.pend != nil && v.pend.kind == "fund" {
		return userErr("The exchange's deposit is waiting for a block.")
	}
	cv, err := x.eth.c.readVaultID(v.id)
	if err != nil {
		return err
	}
	if cv.exitStart != 0 || cv.closed {
		return userErr("Trading is frozen while an exit is in progress.")
	}
	return nil
}

func (x *Exchange) ethOrder(r EthOrderReq) (EthStateOffer, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	v, err := x.ethVault(r.VaultID)
	if err != nil {
		return EthStateOffer{}, err
	}
	if err := x.ethTradable(v); err != nil {
		return EthStateOffer{}, err
	}
	if r.Sats <= 0 {
		return EthStateOffer{}, userErr("Enter an amount above zero.")
	}
	l := v.latest()
	cost := orderCost(r.Sats, x.eth.price)
	user, exch := l.UserSats-r.Sats, l.ExchSats+r.Sats
	label := fmt.Sprintf("Sold %s at %s", fmtCoin(r.Sats, "ETH"), fmtUSD(x.eth.price*100))
	switch r.Side {
	case "sell":
		if r.Sats > l.UserSats {
			return EthStateOffer{}, userErr("You only have %s in the vault to sell.", fmtCoin(l.UserSats, "ETH"))
		}
	case "buy":
		if r.Sats > l.ExchSats {
			return EthStateOffer{}, userErr("The exchange's side of the vault holds %s, so that's the most you can buy.", fmtCoin(l.ExchSats, "ETH"))
		}
		if cost > x.cash[v.account] {
			return EthStateOffer{}, userErr("That costs %s and you have %s in cash.", fmtUSD(cost), fmtUSD(x.cash[v.account]))
		}
		user, exch = l.UserSats+r.Sats, l.ExchSats-r.Sats
		label = fmt.Sprintf("Bought %s at %s", fmtCoin(r.Sats, "ETH"), fmtUSD(x.eth.price*100))
	default:
		return EthStateOffer{}, userErr("Unknown order side.")
	}
	st := &ethState{Index: len(v.states), Label: label, UserSats: user, ExchSats: exch}
	v.pend = &xEthPend{kind: "order", side: r.Side, cost: cost, st: st}
	return EthStateOffer{r.VaultID, st.Index, label, user, exch}, nil
}

// ethCommit: the wallet's signature on the proposed state arrives; the
// exchange checks it, applies the trade, and answers with its own.
func (x *Exchange) ethCommit(r EthCommitReq) (EthCommitResp, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	v, err := x.ethVault(r.VaultID)
	if err != nil {
		return EthCommitResp{}, err
	}
	p := v.pend
	if p == nil || p.kind == "fund" || p.st.Index != r.Nonce {
		return EthCommitResp{}, userErr("There's no matching state waiting for your signature.")
	}
	d := x.eth.c.stateDigest(v.id, int64(p.st.Index), p.st.UserSats)
	if !strings.EqualFold(recoverAddr(d, r.UserSig), v.user) {
		return EthCommitResp{}, userErr("Your signature on the new state doesn't check out.")
	}
	if p.kind == "order" {
		if p.side == "buy" || p.side == "fill-buy" {
			if p.cost > x.cash[v.account] {
				return EthCommitResp{}, userErr("Not enough cash any more.")
			}
			x.cash[v.account] -= p.cost
		} else {
			x.cash[v.account] += p.cost
		}
	}
	p.st.userSig = r.UserSig
	p.st.exchSig = signDigest(x.eth.key, d)
	v.states = append(v.states, p.st)
	v.pend = nil
	if p.done != nil {
		p.done()
	}
	v.log("State #%d signed: %s", p.st.Index, p.st.Label)
	return EthCommitResp{x.maybeTamper(p.st.exchSig), x.cash[v.account]}, nil
}

// ethCapacity: the exchange locks its own ETH with fund(). The new state is
// proposed once that deposit has confirmed (ethCapacityOffer). Until then,
// if an exit happened, the exchange would just get its deposit back.
func (x *Exchange) ethCapacity(r EthCapacityReq) (empty, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	v, err := x.ethVault(r.VaultID)
	if err != nil {
		return empty{}, err
	}
	if err := x.ethTradable(v); err != nil {
		return empty{}, err
	}
	if r.Sats < 100_000 || r.Sats > 5*unit {
		return empty{}, userErr("Ask for between 0.001 and 5 ETH.")
	}
	if capacityFee(r.Sats) > v.latest().UserSats {
		return empty{}, userErr("The fee is more than your share of the vault.")
	}
	info := &TxInfo{Label: "Exchange locks buying capacity"}
	hash, err := x.eth.c.send(x.eth.addr, toWei(r.Sats), calldata("fund(uint256)", wordInt(v.id)), info, "fund sent", "fund refused")
	if err != nil {
		return empty{}, userErr("The contract refused the exchange's deposit: %s", revertReason(err))
	}
	v.fundTx, v.fundAmt = hash, r.Sats
	v.pend = &xEthPend{kind: "fund"}
	v.log("Sent %s of buying capacity", fmtCoin(r.Sats, "ETH"))
	return empty{}, nil
}

func (x *Exchange) ethCapacityOffer(r EthOpenReq) (EthStateOffer, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	v, err := x.ethVault(r.VaultID)
	if err != nil {
		return EthStateOffer{}, err
	}
	if v.pend == nil || v.pend.kind != "fund" {
		return EthStateOffer{}, userErr("No buying capacity is being added.")
	}
	switch x.eth.c.receipt(v.fundTx) {
	case 0:
		return EthStateOffer{}, userErr("The exchange's deposit hasn't confirmed yet.")
	case -1:
		v.pend = nil
		return EthStateOffer{}, userErr("The exchange's deposit failed.")
	}
	l := v.latest()
	fee := capacityFee(v.fundAmt)
	label := fmt.Sprintf("Exchange locked %s (fee %s)", fmtCoin(v.fundAmt, "ETH"), fmtCoin(fee, "ETH"))
	st := &ethState{Index: len(v.states), Label: label, UserSats: l.UserSats - fee, ExchSats: l.ExchSats + v.fundAmt + fee}
	v.pend = &xEthPend{kind: "capacity", st: st}
	return EthStateOffer{r.VaultID, st.Index, label, st.UserSats, st.ExchSats}, nil
}

func (x *Exchange) ethSettle(r EthSettleReq) (EthSettleResp, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	v, err := x.ethVault(r.VaultID)
	if err != nil {
		return EthSettleResp{}, err
	}
	switch x.eth.mode {
	case "offline":
		return EthSettleResp{}, userErr("The exchange is offline. Request an exit instead.")
	case "malicious":
		return EthSettleResp{}, userErr("The exchange won't co-sign a settlement. Request an exit instead.")
	}
	if !v.settling {
		if err := x.ethTradable(v); err != nil {
			return EthSettleResp{}, err
		}
	}
	l := v.latest()
	d := x.eth.c.settleDigest(v.id, int64(l.Index), l.UserSats)
	if !strings.EqualFold(recoverAddr(d, r.UserSig), v.user) {
		return EthSettleResp{}, userErr("Your settle signature doesn't check out (it must be for the latest state).")
	}
	v.settling = true
	return EthSettleResp{signDigest(x.eth.key, d)}, nil
}

// ethTick is the exchange's Ethereum watchtower: during an exit it submits
// its latest state (or, when malicious, the one that pays it most).
func (x *Exchange) ethTick() {
	if x.eth == nil {
		return
	}
	now := x.eth.c.chainTime()
	for _, v := range x.ethVaults {
		if v.closed || len(v.states) == 0 {
			continue
		}
		cv, err := x.eth.c.readVaultID(v.id)
		if err != nil {
			continue
		}
		if cv.closed {
			v.closed = true
			v.log("Vault closed on the chain")
			continue
		}
		if cv.exitStart == 0 || now >= cv.exitStart+cv.exitDelay || x.eth.mode == "offline" {
			continue
		}
		pick := v.latest()
		if x.eth.mode == "malicious" {
			for _, s := range v.states {
				if s.ExchSats > pick.ExchSats {
					pick = s
				}
			}
		}
		busy := v.exchPend != "" && x.eth.c.receipt(v.exchPend) == 0
		if busy || v.exchLast == pick.Index || (cv.hasBest && int64(pick.Index) <= cv.bestNonce) {
			continue
		}
		hash, err := x.eth.c.send(x.eth.addr, nil, x.eth.c.submitCall(v.id, int64(pick.Index), pick.UserSats, pick.userSig, pick.exchSig),
			&TxInfo{Label: "Exchange submits a state"}, "submitted", "refused")
		if err == nil {
			v.exchLast, v.exchPend = pick.Index, hash
			v.log("Submitted state #%d during the exit window", pick.Index)
		}
	}
}

// ------------------------------------------------------------------ wallet side

type wEth struct {
	c        *EthEngine
	key      *btcec.PrivateKey
	addr     string
	delay    int64
	exchAddr string
	vault    *wEthVault
}

type wEthVault struct {
	id              *big.Int
	states          []*ethState
	openTx, exitTx  string
	closeTx, closed string
	capPending      bool
	capSats         int64
	userLast        int
	userPend        string
	exchFault       string
	Events          []string
}

func (v *wEthVault) latest() *ethState { return v.states[len(v.states)-1] }
func (v *wEthVault) log(f string, a ...any) {
	v.Events = append(v.Events, fmt.Sprintf(f, a...))
}

// AttachEth gives the wallet its own Ethereum key on the node.
func (w *Wallet) AttachEth(ep EthEndpoint) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	k, _ := btcec.NewPrivateKey()
	w.eth = &wEth{c: newEthLight(ep), key: k, addr: ethAddress(k), delay: ep.ExitDelay}
	return w.eth.addr
}

func (w *Wallet) ethVault() (*wEth, *wEthVault, error) {
	if w.eth == nil {
		return nil, nil, userErr("This wallet has no Ethereum.")
	}
	if w.eth.vault == nil || w.eth.vault.closed != "" {
		return nil, nil, userErr("Open a vault first.")
	}
	return w.eth, w.eth.vault, nil
}

// EthOpen locks ETH with the contract's open(). This is safe to do before
// the exchange signs anything: if nobody ever submits a state, finalize pays
// the deposit back after the exit window.
func (w *Wallet) EthOpen(sats int64) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	we := w.eth
	if we == nil {
		return userErr("This wallet has no Ethereum.")
	}
	if we.vault != nil && we.vault.closed == "" {
		return userErr("You already have an open vault.")
	}
	var info EthInfoResp
	if err := call(w.base, "/v1/eth/info", empty{}, &info); err != nil {
		return err
	}
	if info.ExitDelay != we.delay {
		return userErr("The exchange asked for a different exit window (%d seconds). Refusing.", info.ExitDelay)
	}
	we.exchAddr = info.ExchAddr
	b, err := we.c.ethCall(calldata("nextId()"))
	if err != nil || len(b) < 32 {
		return userErr("Couldn't read the contract.")
	}
	id := new(big.Int).SetBytes(b[:32])
	hash, err := we.c.send(we.addr, toWei(sats), calldata("open(address,uint64)", wordAddr(info.ExchAddr), wordU(we.delay)),
		&TxInfo{Label: "Lock ETH"}, "sent", "refused")
	if err != nil {
		return userErr("The contract refused the deposit: %s", revertReason(err))
	}
	we.vault = &wEthVault{id: id, openTx: hash, userLast: -1}
	we.vault.log("Locked %s into vault #%s. Mine a block to confirm it", fmtCoin(sats, "ETH"), id)
	return nil
}

// ethAgree runs steps 2 and 3 for a state the exchange proposed: check the
// split is what we expect, sign, send, and check the exchange's signature.
func (w *Wallet) ethAgree(we *wEth, v *wEthVault, off EthStateOffer, wantUser, total int64) error {
	if off.Nonce != len(v.states) || off.UserSats != wantUser || off.UserSats+off.ExchSats != total {
		return userErr("The exchange proposed a different split than agreed. Refusing.")
	}
	d := we.c.stateDigest(v.id, int64(off.Nonce), off.UserSats)
	sig := signDigest(we.key, d)
	var cr EthCommitResp
	if err := call(w.base, "/v1/eth/commit", EthCommitReq{off.VaultID, off.Nonce, sig}, &cr); err != nil {
		return err
	}
	if !strings.EqualFold(recoverAddr(d, cr.ExchSig), we.exchAddr) {
		// The exchange holds our signature but we don't hold its: our newest
		// fully signed state is the previous one. Stop and exit.
		v.exchFault = fmt.Sprintf("the exchange's signature on state #%d doesn't check out", off.Nonce)
		v.log("Problem: %s. Request an exit.", v.exchFault)
		return userErr("The exchange's signature doesn't check out. Request an exit.")
	}
	v.states = append(v.states, &ethState{Index: off.Nonce, Label: off.Label, UserSats: off.UserSats, ExchSats: off.ExchSats, userSig: sig, exchSig: cr.ExchSig})
	v.log("State #%d: %s", off.Nonce, off.Label)
	return nil
}

// ethRegister: once the deposit confirms, agree state #0 with the exchange.
func (w *Wallet) ethRegister(we *wEth, v *wEthVault) {
	if len(v.states) > 0 || we.c.receipt(v.openTx) != 1 {
		return
	}
	// The vault number guessed before sending can be taken by someone else's
	// deposit in the same block; the Opened event says which one is ours.
	if id := we.c.openedID(v.openTx); id != nil && id.Cmp(v.id) != 0 {
		v.log("Your vault is #%s", id)
		v.id = id
	}
	cv, err := we.c.readVaultID(v.id)
	if err != nil || !strings.EqualFold(cv.user, we.addr) {
		v.exchFault = "the vault on the chain isn't ours"
		return
	}
	var off EthStateOffer
	if call(w.base, "/v1/eth/open", EthOpenReq{w.Account, v.id.String()}, &off) != nil {
		return
	}
	w.ethAgree(we, v, off, cv.amount, cv.amount)
}

func (w *Wallet) ethAmount(we *wEth, v *wEthVault) int64 {
	cv, _ := we.c.readVaultID(v.id)
	return cv.amount
}

func (w *Wallet) EthOrder(side string, sats int64) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	we, v, err := w.ethVault()
	if err != nil {
		return err
	}
	if len(v.states) == 0 {
		return userErr("Your deposit hasn't confirmed yet. Mine a block first.")
	}
	if v.exchFault != "" {
		return userErr("Not trading: %s", v.exchFault)
	}
	var off EthStateOffer
	if err := call(w.base, "/v1/eth/order", EthOrderReq{v.id.String(), side, sats}, &off); err != nil {
		return err
	}
	want := v.latest().UserSats - sats
	if side == "buy" {
		want = v.latest().UserSats + sats
	}
	return w.ethAgree(we, v, off, want, w.ethAmount(we, v))
}

func (w *Wallet) EthCapacity(sats int64) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, v, err := w.ethVault()
	if err != nil {
		return err
	}
	if err := call(w.base, "/v1/eth/capacity", EthCapacityReq{v.id.String(), sats}, nil); err != nil {
		return err
	}
	v.capPending, v.capSats = true, sats
	v.log("The exchange is locking %s. Mine a block to confirm it", fmtCoin(sats, "ETH"))
	return nil
}

// ethCapacityAgree: after the exchange's deposit confirms, agree the new
// split (its ETH added, 0.2% fee from our share), checked against the chain.
func (w *Wallet) ethCapacityAgree(we *wEth, v *wEthVault) {
	if !v.capPending || len(v.states) == 0 {
		return
	}
	var off EthStateOffer
	if call(w.base, "/v1/eth/capacity/offer", EthOpenReq{w.Account, v.id.String()}, &off) != nil {
		return
	}
	cv, err := we.c.readVaultID(v.id)
	l := v.latest()
	if err != nil || cv.amount != l.UserSats+l.ExchSats+v.capSats {
		return
	}
	if w.ethAgree(we, v, off, l.UserSats-capacityFee(v.capSats), cv.amount) == nil {
		v.capPending = false
	}
}

// EthSettle signs the settle message for the latest state, gets the
// exchange's, checks it, and sends settle().
func (w *Wallet) EthSettle() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	we, v, err := w.ethVault()
	if err != nil {
		return err
	}
	if len(v.states) == 0 {
		return userErr("Your deposit hasn't confirmed yet.")
	}
	l := v.latest()
	d := we.c.settleDigest(v.id, int64(l.Index), l.UserSats)
	us := signDigest(we.key, d)
	var sr EthSettleResp
	if err := call(w.base, "/v1/eth/settle", EthSettleReq{v.id.String(), us}, &sr); err != nil {
		return err
	}
	if !strings.EqualFold(recoverAddr(d, sr.ExchSig), we.exchAddr) {
		return userErr("The exchange's settle signature doesn't check out.")
	}
	hash, err := we.c.send(we.addr, nil, we.c.settleCall(v.id, int64(l.Index), l.UserSats, us, sr.ExchSig), &TxInfo{Label: "Settle"}, "sent", "refused")
	if err != nil {
		return userErr("The contract refused the settlement: %s", revertReason(err))
	}
	v.closeTx, v.closed = hash, "settled"
	return nil
}

func (w *Wallet) EthExit() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	we, v, err := w.ethVault()
	if err != nil {
		return err
	}
	hash, err := we.c.send(we.addr, nil, calldata("requestExit(uint256)", wordInt(v.id)), &TxInfo{Label: "Exit request"}, "sent", "refused")
	if err != nil {
		return userErr("The contract refused the exit request: %s", revertReason(err))
	}
	v.exitTx = hash
	return nil
}

func (w *Wallet) EthClaim() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	we, v, err := w.ethVault()
	if err != nil {
		return err
	}
	hash, err := we.c.send(we.addr, nil, calldata("finalize(uint256)", wordInt(v.id)), &TxInfo{Label: "Pay out"}, "sent", "refused")
	if err != nil {
		return userErr("The contract refused the payout: %s", revertReason(err))
	}
	v.closeTx, v.closed = hash, "finalized"
	return nil
}

// ethTick is the wallet's Ethereum upkeep and watchtower.
func (w *Wallet) ethTick() {
	we := w.eth
	if we == nil || we.vault == nil {
		return
	}
	v := we.vault
	w.ethRegister(we, v)
	w.ethCapacityAgree(we, v)
	if len(v.states) == 0 || v.closed != "" {
		return
	}
	cv, err := we.c.readVaultID(v.id)
	if err != nil {
		return
	}
	if cv.closed {
		v.closed = "finalized"
		return
	}
	if !w.WT || cv.exitStart == 0 || we.c.chainTime() >= cv.exitStart+cv.exitDelay {
		return
	}
	l := v.latest()
	busy := v.userPend != "" && we.c.receipt(v.userPend) == 0
	need := (cv.hasBest && cv.bestNonce < int64(l.Index)) || (!cv.hasBest && l.UserSats > cv.userDep)
	if busy || !need || v.userLast == l.Index {
		return
	}
	hash, err := we.c.send(we.addr, nil, we.c.submitCall(v.id, int64(l.Index), l.UserSats, l.userSig, l.exchSig),
		&TxInfo{Label: "Your watchtower submits"}, "submitted", "refused")
	if err == nil {
		v.userLast, v.userPend = l.Index, hash
		v.log("Your watchtower submitted your latest state #%d", l.Index)
	}
}

// EthVault is a summary for tests.
func (w *Wallet) EthVault() (userSats, exchSats int64, states int, closed string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.eth == nil || w.eth.vault == nil || len(w.eth.vault.states) == 0 {
		return
	}
	v := w.eth.vault
	l := v.latest()
	return l.UserSats, l.ExchSats, len(v.states), v.closed
}

// EthBalance is the wallet's ETH balance on the node.
func (w *Wallet) EthBalance() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return fromWei(w.eth.c.balance(w.eth.addr))
}
