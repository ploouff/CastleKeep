package main

// The watchtower as its own program ("go run . watchtower"). It keeps
// watching the chains for a user while their wallet is closed.
//
// It holds no key that can move the user's coins. After every trade the
// wallet hands it transactions signed in advance that can only pay the user:
//
//   - Bitcoin/Litecoin, for each revoked state: the penalty sweep that takes
//     the exchange's share of that state with the exchange's revocation
//     secret, already signed by the user. Sent if the exchange publishes the
//     old state. (The exchange can take that share itself after D2 blocks,
//     6 on Bitcoin, about an hour on signet, so this can't wait for the
//     user to come back.)
//   - Bitcoin/Litecoin, for each generation: the user's copy of the newest
//     state, signed by both, when it pays the user more than their deposit.
//     Sent if an exit request confirms and nobody has published a state.
//     The wallet gives the watchtower each new state BEFORE it hands the
//     exchange its secret for the old one (utxoAgree), so the watchtower never
//     holds a state the exchange could punish.
//   - EVM chains: the newest state with both signatures. Anyone may submit
//     it to the contract during an exit window; the watchtower pays the gas
//     from its own small fee wallet (its only key).
//
// Limits: it sees the user's vault transactions and amounts (a real one would
// get them encrypted until needed), and the transactions signed in advance
// have the flat test fee, so it can't speed them up.
//
// Test networks only. Not audited.

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

// ------------------------------------------------------------------ messages

// TowerGen is one generation of a Bitcoin/Litecoin vault (one exit request).
type TowerGen struct {
	ExitTxid    string // the generation's exit request
	LatestIndex int    // the newest state of this generation
	Latest      string // the user's copy of that state signed by both (hex); "" if it doesn't pay the user more than their deposit
	Sent        string `json:",omitempty"` // the watchtower's own note: what it published
}

// TowerBreach is one revoked state and the penalty sweep for it.
type TowerBreach struct {
	Index     int
	StateTxid string // the exchange's copy of the revoked state
	Sweep     string // takes the exchange's share with its revocation secret and pays the user (hex, signed)
	Sent      string `json:",omitempty"` // the watchtower's own note: the sweep it sent
}

type TowerUTXOVault struct {
	Chain, VaultID string
	Gens           []*TowerGen
	Breaches       []*TowerBreach
	Done           bool // settled, claimed or swept: nothing left to watch
}

type TowerEVMVault struct {
	Chain, VaultID, Contract string
	Index                    int
	UserSats                 int64
	UserSig, ExchSig         []byte
	Done                     bool
	Pend                     string `json:",omitempty"` // the watchtower's submission waiting for a block
	Note                     string `json:",omitempty"` // its last problem, so it's only reported once
}

// TowerUpdate is everything a wallet's watchtower needs. The first update
// registers the wallet with its token; later ones must bring the same token.
type TowerUpdate struct {
	Wallet, Token, Name string
	UTXO                []*TowerUTXOVault
	EVM                 []*TowerEVMVault
}

type TowerAuth struct{ Wallet, Token string }

type TowerStatus struct {
	Vaults    int      // vaults it is watching for this wallet
	Ready     int      // transactions and states it holds ready to send
	LastCheck int64    // unix time of its last look at the chains (0: not yet)
	Chains    []string // the chains it watches
	FeeAddr   string   // its fee address on the EVM chains
	FeeLow    []string // EVM chains where it has too little for fees
	Events    []string // what it did for this wallet, newest last
}

// ------------------------------------------------------------------ the watchtower

type towerChain struct {
	id      string
	cfg     UTXOConfig
	c       *chainConn
	checked int64 // the height it last looked at
}

type towerEVM struct {
	id, ticker string
	c          *EthEngine
	public     bool
}

type towerWallet struct {
	Name      string
	TokenHash string
	UTXO      map[string]*TowerUTXOVault // by chain/vault
	EVM       map[string]*TowerEVMVault
	Events    []string
}

func (tw *towerWallet) log(f string, a ...any) {
	tw.Events = append(tw.Events, time.Now().Format("2006-01-02 15:04")+"  "+fmt.Sprintf(f, a...))
	if n := len(tw.Events); n > 200 {
		tw.Events = tw.Events[n-200:]
	}
}

// Tower is the watchtower program.
type Tower struct {
	mu        sync.Mutex
	chains    map[string]*towerChain
	evm       map[string]*towerEVM
	key       *btcec.PrivateKey // its fee key on the EVM chains: it never holds user coins
	addr      string
	wallets   map[string]*towerWallet // by wallet account
	lastCheck time.Time
	failed    map[string]string // txid -> last refusal, so each is reported once

	file, saveErr string
	lastSaved     []byte

	// shared mode (split_shared.go): its page lists every wallet, so it needs the admin token
	shared     bool
	adminToken string // saved
}

// towerFeeMin is what the watchtower wants on an EVM chain to pay for a
// submission (about 100,000 gas; plenty on the test networks).
const towerFeeMin = unit / 100

func NewTower(cfgs map[string]UTXOConfig, rpc map[string]string) (*Tower, error) {
	k, _ := btcec.NewPrivateKey()
	t := &Tower{chains: map[string]*towerChain{}, evm: map[string]*towerEVM{}, key: k, addr: ethAddress(k),
		wallets: map[string]*towerWallet{}, failed: map[string]string{}}
	for id, cfg := range cfgs {
		c, err := dialChain(rpc[id])
		if err != nil {
			return nil, err
		}
		t.chains[id] = &towerChain{id: id, cfg: cfg, c: c, checked: -1}
	}
	return t, nil
}

// AttachEVM lets the watchtower submit states on an EVM chain. It returns
// its fee address there (the same on every EVM chain).
func (t *Tower) AttachEVM(chain string, ep EthEndpoint) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	ticker := "ETH"
	if c, ok := evmByID(chain); ok {
		ticker = c.Ticker
	}
	te := &towerEVM{id: chain, ticker: ticker, c: newEthLight(ep), public: ep.Public}
	te.c.signer = func(a string) *btcec.PrivateKey {
		if strings.EqualFold(a, t.addr) {
			return t.key
		}
		return nil
	}
	t.evm[chain] = te
	return t.addr
}

// FeeAddress is where the watchtower's fees on the EVM chains come from.
func (t *Tower) FeeAddress() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.addr
}

func (t *Tower) Handler() http.Handler {
	m := http.NewServeMux()
	m.Handle("/v1/tower/update", handle(t.update))
	m.Handle("/v1/tower/status", handle(t.status))
	m.Handle("/v1/tower/forget", handle(t.forget))
	m.Handle("/v1/tower/view", adminOnly(t.adminOK, handle(func(empty) (TowerView, error) { return t.View(), nil })))
	return m
}

func tokenHash(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}

// wallet finds a wallet's record and checks its token. A wallet the
// watchtower hasn't seen is registered when register is set. (t.mu held.)
func (t *Tower) wallet(a TowerAuth, register bool) (*towerWallet, error) {
	if a.Wallet == "" || len(a.Wallet) > 64 || len(a.Token) < 32 || len(a.Token) > 128 {
		return nil, userErr("Bad wallet id or token.")
	}
	tw := t.wallets[a.Wallet]
	if tw == nil {
		if !register {
			return nil, nil
		}
		tw = &towerWallet{TokenHash: tokenHash(a.Token), UTXO: map[string]*TowerUTXOVault{}, EVM: map[string]*TowerEVMVault{}}
		t.wallets[a.Wallet] = tw
		tw.log("Started watching for this wallet")
		return tw, nil
	}
	if subtle.ConstantTimeCompare([]byte(tw.TokenHash), []byte(tokenHash(a.Token))) != 1 {
		return nil, userErr("Wrong token for this wallet: the watchtower only takes updates from the wallet that signed up with it.")
	}
	return tw, nil
}

func (t *Tower) update(u TowerUpdate) (TowerStatus, error) {
	// Check everything before changing anything.
	for _, v := range u.UTXO {
		if err := checkTowerUTXO(v); err != nil {
			return TowerStatus{}, err
		}
	}
	for _, v := range u.EVM {
		if err := checkTowerEVM(v); err != nil {
			return TowerStatus{}, err
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	tw, err := t.wallet(TowerAuth{u.Wallet, u.Token}, true)
	if err != nil {
		return TowerStatus{}, err
	}
	if validName(u.Name) {
		tw.Name = u.Name
	}
	for _, v := range u.UTXO {
		mergeTowerUTXO(tw, v)
	}
	for _, v := range u.EVM {
		mergeTowerEVM(tw, v)
	}
	t.save()
	return t.statusFor(tw), nil
}

func (t *Tower) status(a TowerAuth) (TowerStatus, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	tw, err := t.wallet(a, false)
	if err != nil {
		return TowerStatus{}, err
	}
	if tw == nil {
		return TowerStatus{}, userErr("This watchtower doesn't know that wallet yet.")
	}
	return t.statusFor(tw), nil
}

// forget drops everything the watchtower holds for a wallet (the wallet
// stops using it). Unknown wallets are fine: there is nothing to forget.
func (t *Tower) forget(a TowerAuth) (empty, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	tw, err := t.wallet(a, false)
	if err != nil || tw == nil {
		return empty{}, err
	}
	delete(t.wallets, a.Wallet)
	t.save()
	return empty{}, nil
}

func checkTowerUTXO(v *TowerUTXOVault) error {
	if v == nil || v.Chain == "" || v.VaultID == "" || len(v.VaultID) > 64 || len(v.Gens) > 1000 || len(v.Breaches) > 100000 {
		return userErr("A vault in the update is malformed.")
	}
	for _, g := range v.Gens {
		exit, err := chainhash.NewHashFromStr(g.ExitTxid)
		if err != nil {
			return userErr("A vault generation has a bad exit request id.")
		}
		if g.Latest == "" {
			continue
		}
		tx, err := parseTxHex(g.Latest)
		if err != nil || len(tx.TxIn) != 1 || tx.TxIn[0].PreviousOutPoint != (wire.OutPoint{Hash: *exit, Index: 0}) {
			return userErr("State #%d doesn't spend its exit request.", g.LatestIndex)
		}
	}
	for _, b := range v.Breaches {
		st, err := chainhash.NewHashFromStr(b.StateTxid)
		if err != nil {
			return userErr("Revoked state #%d has a bad id.", b.Index)
		}
		tx, err := parseTxHex(b.Sweep)
		if err != nil || len(tx.TxIn) != 1 || tx.TxIn[0].PreviousOutPoint.Hash != *st {
			return userErr("The penalty sweep for state #%d doesn't spend that state.", b.Index)
		}
	}
	return nil
}

func checkTowerEVM(v *TowerEVMVault) error {
	if v == nil || !isEVM(v.Chain) || len(v.UserSig) != 65 || len(v.ExchSig) != 65 || v.Index < 0 || v.UserSats < 0 {
		return userErr("An EVM vault in the update is malformed.")
	}
	if _, ok := new(big.Int).SetString(v.VaultID, 10); !ok {
		return userErr("An EVM vault in the update has a bad number.")
	}
	return nil
}

// mergeTowerUTXO adds what's new. A generation's newest state only ever moves
// forward (an update can never bring back an older state), and penalty sweeps
// are only ever added.
func mergeTowerUTXO(tw *towerWallet, in *TowerUTXOVault) {
	key := in.Chain + "/" + in.VaultID
	have := tw.UTXO[key]
	if have == nil {
		have = &TowerUTXOVault{Chain: in.Chain, VaultID: in.VaultID}
		tw.UTXO[key] = have
	}
	for _, g := range in.Gens {
		var old *TowerGen
		for _, h := range have.Gens {
			if h.ExitTxid == g.ExitTxid {
				old = h
			}
		}
		if old == nil {
			have.Gens = append(have.Gens, &TowerGen{ExitTxid: g.ExitTxid, LatestIndex: g.LatestIndex, Latest: g.Latest})
		} else if old.Sent == "" && g.LatestIndex >= old.LatestIndex {
			old.LatestIndex, old.Latest = g.LatestIndex, g.Latest
		}
	}
	known := map[string]bool{}
	for _, b := range have.Breaches {
		known[b.StateTxid] = true
	}
	for _, b := range in.Breaches {
		if !known[b.StateTxid] {
			have.Breaches = append(have.Breaches, &TowerBreach{Index: b.Index, StateTxid: b.StateTxid, Sweep: b.Sweep})
			known[b.StateTxid] = true
		}
	}
	have.Done = have.Done || in.Done
}

func mergeTowerEVM(tw *towerWallet, in *TowerEVMVault) {
	key := in.Chain + "/" + in.VaultID
	have := tw.EVM[key]
	if have == nil {
		have = &TowerEVMVault{Chain: in.Chain, VaultID: in.VaultID, Index: -1}
		tw.EVM[key] = have
	}
	if have.Contract == "" {
		have.Contract = in.Contract
	}
	if in.Index >= have.Index {
		have.Index, have.UserSats, have.UserSig, have.ExchSig = in.Index, in.UserSats, in.UserSig, in.ExchSig
	}
	have.Done = have.Done || in.Done
}

// Tick is the watchtower's look at the chains. Bitcoin and Litecoin vaults
// are checked once per new block; EVM vaults on every tick.
func (t *Tower) Tick() {
	t.mu.Lock()
	defer t.mu.Unlock()
	ids := make([]string, 0, len(t.wallets))
	for id := range t.wallets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, tc := range t.chains {
		h := tc.c.height()
		if h == tc.checked {
			continue
		}
		tc.checked = h
		for _, id := range ids {
			tw := t.wallets[id]
			for _, v := range tw.UTXO {
				if v.Chain == tc.id && !v.Done {
					t.watchUTXO(tw, tc, v)
				}
			}
		}
	}
	for _, te := range t.evm {
		now := int64(-1)
		for _, id := range ids {
			tw := t.wallets[id]
			for _, v := range tw.EVM {
				if v.Chain != te.id || v.Done || v.Index < 0 {
					continue
				}
				if now < 0 {
					now = te.c.chainTime()
				}
				t.watchEVM(tw, te, v, now)
			}
		}
	}
	t.lastCheck = time.Now()
	t.save()
}

// sendOnce broadcasts tx and reports a refusal only the first time. (t.mu held.)
func (t *Tower) sendOnce(tw *towerWallet, c *chainConn, tx *wire.MsgTx, what string) bool {
	id := tx.TxHash().String()
	err := c.send(tx)
	if err == nil {
		delete(t.failed, id)
		return true
	}
	if t.failed[id] != err.Error() {
		t.failed[id] = err.Error()
		tw.log("Couldn't send %s: %v", what, err)
	}
	return false
}

func (t *Tower) watchUTXO(tw *towerWallet, tc *towerChain, v *TowerUTXOVault) {
	c, name := tc.c, tc.cfg.Info.Name
	if name == "" {
		name = strings.ToUpper(tc.id)
	}
	// An old state published by the exchange: take its share with the
	// revocation secret before it can claim it after D2 blocks.
	for _, b := range v.Breaches {
		if b.Sent != "" {
			continue
		}
		h, _ := chainhash.NewHashFromStr(b.StateTxid)
		if n, ok := c.seen(*h); !ok || n == 0 {
			continue
		}
		tx, err := parseTxHex(b.Sweep)
		if err != nil {
			continue
		}
		if _, ok := c.unspent(tx.TxIn[0].PreviousOutPoint); !ok {
			continue // already swept (by the wallet itself, perhaps)
		}
		if t.sendOnce(tw, c, tx, fmt.Sprintf("the penalty sweep for old state #%d on %s", b.Index, name)) {
			b.Sent = tx.TxHash().String()
			tw.log("%s: the exchange published old state #%d of vault %s. Sent the penalty sweep that takes the exchange's share for you (%s).",
				name, b.Index, shortID(v.VaultID), shortID(b.Sent))
		}
	}
	// An exit request that confirmed with nobody publishing a state: publish
	// the user's newest one (the pay-back after the window would return only
	// the deposits).
	for _, g := range v.Gens {
		if g.Sent != "" || g.Latest == "" {
			continue
		}
		h, _ := chainhash.NewHashFromStr(g.ExitTxid)
		if n, ok := c.seen(*h); !ok || n == 0 {
			continue
		}
		if _, ok := c.unspent(wire.OutPoint{Hash: *h, Index: 0}); !ok {
			continue // a state was already published
		}
		tx, err := parseTxHex(g.Latest)
		if err != nil {
			continue
		}
		if t.sendOnce(tw, c, tx, fmt.Sprintf("your state #%d on %s", g.LatestIndex, name)) {
			g.Sent = tx.TxHash().String()
			tw.log("%s: an exit request for vault %s confirmed and nobody had published a state, so it published your newest state #%d (%s).",
				name, shortID(v.VaultID), g.LatestIndex, shortID(g.Sent))
		}
	}
}

func (t *Tower) watchEVM(tw *towerWallet, te *towerEVM, v *TowerEVMVault, now int64) {
	if v.Contract != "" {
		te.c.contract = v.Contract
	}
	id, ok := new(big.Int).SetString(v.VaultID, 10)
	if te.c.contract == "" || !ok {
		return
	}
	cv, err := te.c.readVaultID(id)
	if err != nil {
		return
	}
	name := evmName(te.id)
	if cv.closed {
		v.Done = true
		return
	}
	if cv.exitStart == 0 || now >= cv.exitStart+cv.exitDelay {
		return
	}
	if v.Pend != "" && te.c.receipt(v.Pend) == 0 {
		return // its submission is waiting for a block
	}
	need := (cv.hasBest && cv.bestNonce < int64(v.Index)) || (!cv.hasBest && v.UserSats > cv.userDep)
	if !need {
		return
	}
	hash, err := te.c.send(t.addr, nil, te.c.submitCall(id, int64(v.Index), v.UserSats, v.UserSig, v.ExchSig),
		&TxInfo{Label: "Watchtower submits"}, "submitted", "refused")
	if err != nil {
		note := revertReason(err)
		if fromWei(te.c.balance(t.addr)) < towerFeeMin {
			note = fmt.Sprintf("it has too little test %s for fees at %s", te.ticker, t.addr)
		}
		if note != v.Note {
			v.Note = note
			tw.log("%s: couldn't submit your newest state #%d of vault #%s: %s", name, v.Index, v.VaultID, note)
		}
		return
	}
	v.Pend, v.Note = hash, ""
	what := "no state"
	if cv.hasBest {
		what = fmt.Sprintf("older state #%d", cv.bestNonce)
	}
	tw.log("%s: an exit of vault #%s is under way and the contract held %s, so it submitted your newest state #%d.", name, v.VaultID, what, v.Index)
}

func shortID(s string) string {
	if len(s) > 16 {
		return s[:8] + "…" + s[len(s)-6:]
	}
	return s
}

// statusFor is what a wallet sees about its watchtower. (t.mu held.)
func (t *Tower) statusFor(tw *towerWallet) TowerStatus {
	s := TowerStatus{}
	if !t.lastCheck.IsZero() {
		s.LastCheck = t.lastCheck.Unix()
	}
	for id := range t.chains {
		s.Chains = append(s.Chains, id)
	}
	for id := range t.evm {
		s.Chains = append(s.Chains, id)
	}
	sort.Slice(s.Chains, func(i, j int) bool { return chainOrder(s.Chains[i]) < chainOrder(s.Chains[j]) })
	for _, v := range tw.UTXO {
		if v.Done {
			continue
		}
		s.Vaults++
		for _, g := range v.Gens {
			if g.Latest != "" && g.Sent == "" {
				s.Ready++
			}
		}
		for _, b := range v.Breaches {
			if b.Sent == "" {
				s.Ready++
			}
		}
	}
	for _, v := range tw.EVM {
		if v.Done || v.Index < 0 {
			continue
		}
		s.Vaults++
		s.Ready++
		if te := t.evm[v.Chain]; te != nil && fromWei(te.c.balance(t.addr)) < towerFeeMin {
			s.FeeLow = append(s.FeeLow, v.Chain)
		}
	}
	if len(t.evm) > 0 {
		s.FeeAddr = t.addr
	}
	s.Events = tw.Events
	if n := len(s.Events); n > 20 {
		s.Events = s.Events[n-20:]
	}
	return s
}

// ------------------------------------------------------------------ its page

type TowerView struct {
	FeeAddr   string
	Chains    []string
	LastCheck int64
	Wallets   []TowerWalletView
}

type TowerWalletView struct {
	Name, Account string
	Vaults        []TowerVaultView
	Events        []string
}

type TowerVaultView struct {
	Chain, ID, Status string
}

func (t *Tower) View() TowerView {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := TowerView{}
	if len(t.evm) > 0 {
		out.FeeAddr = t.addr
	}
	if !t.lastCheck.IsZero() {
		out.LastCheck = t.lastCheck.Unix()
	}
	for id := range t.chains {
		out.Chains = append(out.Chains, id)
	}
	for id := range t.evm {
		out.Chains = append(out.Chains, id)
	}
	sort.Slice(out.Chains, func(i, j int) bool { return chainOrder(out.Chains[i]) < chainOrder(out.Chains[j]) })
	for acct, tw := range t.wallets {
		wv := TowerWalletView{Name: tw.Name, Account: acct, Events: tw.Events}
		for _, v := range tw.UTXO {
			ready, sent := 0, 0
			for _, g := range v.Gens {
				if g.Sent != "" {
					sent++
				} else if g.Latest != "" {
					ready++
				}
			}
			for _, b := range v.Breaches {
				if b.Sent != "" {
					sent++
				} else {
					ready++
				}
			}
			st := fmt.Sprintf("Watching, with %s ready to send if needed", plural(ready, "transaction"))
			if sent > 0 {
				st += fmt.Sprintf("; sent %d", sent)
			}
			if v.Done {
				st = "Finished"
			}
			if _, ok := t.chains[v.Chain]; !ok {
				st += " (this watchtower isn't running that chain)"
			}
			wv.Vaults = append(wv.Vaults, TowerVaultView{v.Chain, v.VaultID, st})
		}
		for _, v := range tw.EVM {
			st := fmt.Sprintf("Watching, with state #%d signed by both ready to submit if needed", v.Index)
			if v.Done {
				st = "Finished"
			}
			if _, ok := t.evm[v.Chain]; !ok {
				st += " (this watchtower isn't running that chain)"
			}
			wv.Vaults = append(wv.Vaults, TowerVaultView{v.Chain, "#" + v.VaultID, st})
		}
		sort.Slice(wv.Vaults, func(i, j int) bool {
			a, b := wv.Vaults[i], wv.Vaults[j]
			if a.Chain != b.Chain {
				return chainOrder(a.Chain) < chainOrder(b.Chain)
			}
			return a.ID < b.ID
		})
		out.Wallets = append(out.Wallets, wv)
	}
	sort.Slice(out.Wallets, func(i, j int) bool {
		return out.Wallets[i].Name+out.Wallets[i].Account < out.Wallets[j].Name+out.Wallets[j].Account
	})
	return out
}

// ------------------------------------------------------------------ saving

type towerSave struct {
	Version    int
	Key        keyHex // the fee key on the EVM chains
	Wallets    map[string]*towerWallet
	AdminToken string `json:",omitempty"` // shared mode: for its page
}

func (t *Tower) SaveTo(path string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.file = path
	t.save()
}

func (t *Tower) save() {
	if t.file == "" {
		return
	}
	data, err := json.MarshalIndent(towerSave{saveVersion, keyRec(t.key), t.wallets, t.adminToken}, "", " ")
	if err == nil && string(data) == string(t.lastSaved) {
		return
	}
	if err == nil {
		err = writeFile(t.file, data)
	}
	if err != nil {
		if msg := err.Error(); msg != t.saveErr {
			t.saveErr = msg
			fmt.Println("Couldn't save the watchtower's data:", msg)
		}
		return
	}
	t.lastSaved, t.saveErr = data, ""
}

// Load reads a saved watchtower. Call it after NewTower and AttachEVM.
func (t *Tower) Load(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var s towerSave
	if err := json.Unmarshal(data, &s); err != nil {
		return false, fmt.Errorf("%s is damaged: %v", path, err)
	}
	if s.Version != saveVersion {
		return false, fmt.Errorf("%s was saved by a different version", path)
	}
	k, err := s.Key.key()
	if err != nil {
		return false, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.key, t.addr = k, ethAddress(k)
	t.adminToken = s.AdminToken
	t.wallets = map[string]*towerWallet{}
	for id, tw := range s.Wallets {
		if tw.UTXO == nil {
			tw.UTXO = map[string]*TowerUTXOVault{}
		}
		if tw.EVM == nil {
			tw.EVM = map[string]*TowerEVMVault{}
		}
		t.wallets[id] = tw
	}
	return true, nil
}

// ------------------------------------------------------------------ the wallet's side

// towerClient has a shorter timeout than the exchange's: the wallet waits for
// the watchtower while it holds its lock.
var towerClient = &http.Client{Timeout: 10 * time.Second}

type wTowerRec struct{ URL, Token string }

// UseTower makes the wallet keep a separate watchtower up to date. It is
// saved, so later runs carry on with it until StopTower.
func (w *Wallet) UseTower(url string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.tower = strings.TrimRight(url, "/")
	if w.towerToken == "" {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			panic(err)
		}
		w.towerToken = hex.EncodeToString(b)
	}
	w.towerSent, w.towerErr = "", ""
	w.save()
}

// StopTower tells the watchtower to forget this wallet, then stops using it.
// It refuses if the watchtower can't be told: it would keep a state that
// later trades replace.
func (w *Wallet) StopTower() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.tower == "" {
		return nil
	}
	if err := callAs("the watchtower", towerClient, w.tower, "/v1/tower/forget", TowerAuth{w.Account, w.towerToken}, nil); err != nil {
		return err
	}
	w.tower, w.towerToken, w.towerSent, w.towerErr, w.towerStat = "", "", "", "", nil
	w.save()
	return nil
}

// TowerURL is the separate watchtower in use ("" for none).
func (w *Wallet) TowerURL() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.tower
}

// userCopy is the user's copy of state s, signed by both.
func (v *wVault) userCopy(g *vgen, s *pstate) *wire.MsgTx {
	return withBoth(s.utx, v.def.E, v.def.ms, signLeaf(s.utx, 0, v.def.eFetch(g), v.def.ms, v.key), s.exchSigU)
}

// penaltySweep takes the exchange's share of revoked state s with its secret.
// Its txid doesn't depend on the signature, so the wallet can recognise the
// watchtower's copy on the chain.
func (v *wVault) penaltySweep(s *pstate) *wire.MsgTx {
	return spendLeaf(s.rOut(), s.rAmt, s.R.out, s.R.rev, 0, v.key, v.def.userPay, s.rSecret)
}

// towerUpdate is everything the watchtower needs. (w.mu held.)
func (w *Wallet) towerUpdate() TowerUpdate {
	u := TowerUpdate{Wallet: w.Account, Token: w.towerToken, Name: w.Name}
	for _, id := range sortedKeys(w.chains) {
		v := w.chains[id].vault
		if v == nil || len(v.states) == 0 {
			continue
		}
		tv := &TowerUTXOVault{Chain: id, VaultID: v.id, Done: v.closed == "settled" || v.closed == "claimed" || v.sweepKind != ""}
		for gi, g := range v.gens {
			var l *pstate
			for _, s := range v.states {
				if s.Gen == gi {
					l = s
				}
			}
			if l == nil {
				continue
			}
			tg := &TowerGen{ExitTxid: g.exitTx.TxHash().String(), LatestIndex: l.Index}
			if l.UserSats > g.userDep && !l.revoked && l.exchSigU != nil {
				tg.Latest = txHex(v.userCopy(g, l))
			}
			tv.Gens = append(tv.Gens, tg)
		}
		for _, s := range v.states {
			if s.revoked && s.rSecret != nil && s.rIdx >= 0 {
				tv.Breaches = append(tv.Breaches, &TowerBreach{Index: s.Index, StateTxid: s.etx.TxHash().String(), Sweep: txHex(v.penaltySweep(s))})
			}
		}
		u.UTXO = append(u.UTXO, tv)
	}
	for _, id := range sortedKeys(w.evm) {
		we := w.evm[id]
		v := we.vault
		if v == nil || len(v.states) == 0 {
			continue
		}
		l := v.latest()
		u.EVM = append(u.EVM, &TowerEVMVault{Chain: id, VaultID: v.id.String(), Contract: we.c.contract, Index: l.Index,
			UserSats: l.UserSats, UserSig: l.userSig, ExchSig: l.exchSig, Done: v.closed != ""})
	}
	return u
}

// towerPrint changes whenever the watchtower needs a new update. (w.mu held.)
func (w *Wallet) towerPrint() string {
	var b strings.Builder
	for _, id := range sortedKeys(w.chains) {
		if v := w.chains[id].vault; v != nil {
			revoked := 0
			for _, s := range v.states {
				if s.revoked {
					revoked++
				}
			}
			fmt.Fprintf(&b, "%s/%s/%d/%d/%d/%s/%s;", id, v.id, len(v.gens), len(v.states), revoked, v.closed, v.sweepKind)
		}
	}
	for _, id := range sortedKeys(w.evm) {
		if v := w.evm[id].vault; v != nil {
			fmt.Fprintf(&b, "%s/%s/%d/%s;", id, v.id, len(v.states), v.closed)
		}
	}
	return b.String()
}

// towerSend gives the watchtower anything new, and refreshes its status every
// 10 seconds. After a failure it waits 15 seconds before trying again, unless
// now is set. (w.mu held.)
func (w *Wallet) towerSend(now bool) error {
	if w.tower == "" {
		return nil
	}
	fp := w.towerPrint()
	wait := 10 * time.Second
	if w.towerErr != "" {
		wait = 15 * time.Second
	} else if fp != w.towerSent {
		wait = 0
	}
	if !now && time.Since(w.towerAt) < wait {
		return nil
	}
	var st TowerStatus
	var err error
	// A full update (which also signs the wallet up) unless the watchtower already
	// has everything: then just its status. After any failure, a full update again.
	if fp == w.towerSent && w.towerStat != nil && w.towerErr == "" && !now {
		err = callAs("the watchtower", towerClient, w.tower, "/v1/tower/status", TowerAuth{w.Account, w.towerToken}, &st)
	} else {
		err = callAs("the watchtower", towerClient, w.tower, "/v1/tower/update", w.towerUpdate(), &st)
	}
	w.towerAt = time.Now()
	if err != nil {
		w.towerErr = err.Error()
		return err
	}
	w.towerSent, w.towerStat, w.towerErr = fp, &st, ""
	return nil
}

// towerBefore hands the watchtower state st (not yet in v.states) before the
// wallet gives up the previous state's secret. (w.mu held.)
func (w *Wallet) towerBefore(v *wVault, st *pstate) error {
	if w.tower == "" {
		return nil
	}
	v.states = append(v.states, st)
	defer func() { v.states = v.states[:len(v.states)-1] }()
	return w.towerSend(true)
}

// towerNote is a line for the page about the separate watchtower. (w.mu held.)
func (w *Wallet) towerNote(chain string) string {
	if w.tower == "" {
		return ""
	}
	at := "Separate watchtower at " + w.tower + ": "
	if w.towerErr != "" {
		if !strings.Contains(w.towerErr, "Couldn't reach") {
			return at + "it refused the last update (" + w.towerErr + "). Trades on Bitcoin and Litecoin wait until it takes one."
		}
		return at + "can't be reached right now. Trades on Bitcoin and Litecoin wait until it's back, so it never holds a state you've given up."
	}
	st := w.towerStat
	if st == nil {
		return at + "connecting…"
	}
	runs := false
	for _, c := range st.Chains {
		runs = runs || c == chain
	}
	name := strings.ToUpper(chain)
	if isEVM(chain) {
		name = evmName(chain)
	}
	if !runs {
		return at + "it isn't watching " + name + " (it was started without it)."
	}
	s := at + "nothing to watch for you right now"
	if st.Vaults > 0 {
		s = at + fmt.Sprintf("watching %s for you, with %s ready to send if needed", plural(st.Vaults, "vault"), plural(st.Ready, "transaction"))
	}
	if st.LastCheck == 0 {
		s += "; it hasn't looked at the chains yet."
	} else {
		ago := time.Since(time.Unix(st.LastCheck, 0)).Round(time.Second)
		if ago < 0 {
			ago = 0
		}
		s += fmt.Sprintf("; it last looked %s ago.", ago)
	}
	for _, c := range st.FeeLow {
		if c == chain {
			s += fmt.Sprintf(" It needs a little test %s for fees at %s.", evmTicker(chain), st.FeeAddr)
		}
	}
	if n := len(st.Events); n > 0 {
		e := st.Events[n-1]
		if i := strings.Index(e, "  "); i >= 0 {
			e = e[i+2:]
		}
		s += " Latest: " + e
	}
	return s
}

// plural is "1 vault" or "2 vaults".
func plural(n int, thing string) string {
	if n == 1 {
		return "1 " + thing
	}
	return fmt.Sprintf("%d %ss", n, thing)
}

func evmTicker(chain string) string {
	if c, ok := evmByID(chain); ok {
		return c.Ticker
	}
	return "ETH"
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
