package main

// Saving the exchange and the wallets to disk, so they carry on after a
// restart. Only the inputs are saved: keys, amounts, signatures, secrets and
// a few transactions. The scripts and transactions of every vault state are
// rebuilt from them on load, exactly as they were built the first time.
//
// The files hold private keys and revocation secrets in plain JSON. That is
// fine for test coins only. Anything half-done when a program stopped (an
// order waiting for the second signature, a splice not yet sent) is dropped;
// the other side simply asks again. Test networks only. Not audited.

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
)

const saveVersion = 1

// ------------------------------------------------------------------ shared records

type keyHex string

func keyRec(k *btcec.PrivateKey) keyHex {
	if k == nil {
		return ""
	}
	return keyHex(hex.EncodeToString(k.Serialize()))
}

func (h keyHex) key() (*btcec.PrivateKey, error) {
	b, err := hex.DecodeString(string(h))
	if err != nil || len(b) != 32 {
		return nil, errors.New("a saved key is damaged")
	}
	k, _ := btcec.PrivKeyFromBytes(b)
	return k, nil
}

func txRec(tx *wire.MsgTx) string {
	if tx == nil {
		return ""
	}
	return txHex(tx)
}

func txLoad(s string) (*wire.MsgTx, error) {
	if s == "" {
		return nil, nil
	}
	return parseTxHex(s)
}

type utxoRec struct {
	Op  wire.OutPoint
	Amt int64
}

func utxoRecs(us []utxo) []utxoRec {
	out := []utxoRec{}
	for _, u := range us {
		out = append(out, utxoRec{u.op, u.amt})
	}
	return out
}

func utxoLoad(rs []utxoRec) []utxo {
	var out []utxo
	for _, r := range rs {
		out = append(out, utxo{r.Op, r.Amt})
	}
	return out
}

type defRec struct {
	User, Exch, UserPay, ExchPay []byte
	Delay, D2                    int64
	Anchors                      bool `json:",omitempty"`
	MuSig2                       bool `json:",omitempty"`
}

type genRec struct {
	Out                                        wire.OutPoint
	Amt, UserDep, ExchDep                      int64
	ExitSigU, ExitSigX, RefundSigU, RefundSigX []byte
}

type stateRec struct {
	Index, Gen                                         int
	Label                                              string
	UserSats, ExchSats                                 int64
	RHash, UHash, UserSigE, ExchSigU, RSecret, USecret []byte
	Revoked                                            bool
}

// vaultRec is the part of a Bitcoin/Litecoin vault both sides keep.
type vaultRec struct {
	Def    defRec
	Gens   []genRec
	States []stateRec
}

func saveVault(d *vaultDef, gens []*vgen, states []*pstate) vaultRec {
	r := vaultRec{Def: defRec{d.user, d.exch, d.userPay, d.exchPay, d.delay, d.d2, d.anchors, d.musig}}
	for _, g := range gens {
		r.Gens = append(r.Gens, genRec{g.vaultOut, g.vaultAmt, g.userDep, g.exchDep, g.exitSigU, g.exitSigX, g.refundSigU, g.refundSigX})
	}
	for _, s := range states {
		r.States = append(r.States, stateRec{s.Index, s.Gen, s.Label, s.UserSats, s.ExchSats,
			s.rHash, s.uHash, s.userSigE, s.exchSigU, s.rSecret, s.uSecret, s.revoked})
	}
	return r
}

// load rebuilds the vault definition, its generations and states.
func (r vaultRec) load() (*vaultDef, []*vgen, []*pstate, error) {
	d, err := newVaultDef(r.Def.User, r.Def.Exch, r.Def.UserPay, r.Def.ExchPay, r.Def.Delay, r.Def.D2)
	if err != nil {
		return nil, nil, nil, err
	}
	d.anchors = r.Def.Anchors
	if err := d.setMuSig2(r.Def.MuSig2); err != nil {
		return nil, nil, nil, err
	}
	var gens []*vgen
	for _, gr := range r.Gens {
		g := d.buildGen(gr.Out, gr.Amt, gr.UserDep, gr.ExchDep)
		g.exitSigU, g.exitSigX, g.refundSigU, g.refundSigX = gr.ExitSigU, gr.ExitSigX, gr.RefundSigU, gr.RefundSigX
		gens = append(gens, g)
	}
	var states []*pstate
	for _, sr := range r.States {
		if sr.Gen < 0 || sr.Gen >= len(gens) {
			return nil, nil, nil, errors.New("a saved state points at a missing generation")
		}
		s, err := d.buildState(gens[sr.Gen], sr.Index, sr.Gen, sr.Label, sr.UserSats, sr.ExchSats, sr.RHash, sr.UHash)
		if err != nil {
			return nil, nil, nil, err
		}
		s.userSigE, s.exchSigU, s.rSecret, s.uSecret, s.revoked = sr.UserSigE, sr.ExchSigU, sr.RSecret, sr.USecret, sr.Revoked
		states = append(states, s)
	}
	return d, gens, states, nil
}

func stateIndex(states []*pstate, s *pstate) int {
	for i, x := range states {
		if x == s {
			return i
		}
	}
	return -1
}

func stateAt(states []*pstate, i int) *pstate {
	if i < 0 || i >= len(states) {
		return nil
	}
	return states[i]
}

type ethStateRec struct {
	Index              int
	Label              string
	UserSats, ExchSats int64
	Height             int64
	UserSig, ExchSig   []byte
}

func ethStateRecs(ss []*ethState) []ethStateRec {
	out := []ethStateRec{}
	for _, s := range ss {
		out = append(out, ethStateRec{s.Index, s.Label, s.UserSats, s.ExchSats, s.Height, s.userSig, s.exchSig})
	}
	return out
}

func ethStatesLoad(rs []ethStateRec) []*ethState {
	var out []*ethState
	for _, r := range rs {
		out = append(out, &ethState{Index: r.Index, Label: r.Label, UserSats: r.UserSats, ExchSats: r.ExchSats, Height: r.Height, userSig: r.UserSig, exchSig: r.ExchSig})
	}
	return out
}

// writeFile writes data next to path and then renames it into place, so a
// crash never leaves a half-written file.
func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// setAside renames an old save file (or folder) out of the way instead of
// deleting it, for -fresh.
func setAside(path string) (string, error) {
	if _, err := os.Stat(path); err != nil {
		return "", nil
	}
	to := path + ".old-" + time.Now().Format("20060102-150405")
	return to, os.Rename(path, to)
}

// DataDir is where the separate programs keep their files by default.
func DataDir() string {
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, "castlekeep-data")
	}
	return "castlekeep-data"
}

// ------------------------------------------------------------------ exchange

type xChainRec struct {
	Tag   string // which test chain (see chainConn.tag)
	Key   keyHex
	Coins []utxoRec
	Price int64
	Mode  string
}

type xVaultRec struct {
	ID, Account, Chain string
	Key                keyHex
	Vault              vaultRec
	Opened             bool
	SpliceTx           string
	SpliceFinished     bool
	SettleTx           string
	Settling           bool
	Published          int // index into States, or -1
	PublishedBy        string
	Closed, SweepKind  string
	Events             []string
}

type xEthRec struct {
	Key      keyHex
	Contract string // the vaults below only make sense on this contract
	Price    int64
	Mode     string
	Vaults   []xEthVaultRec
}

type xEthVaultRec struct {
	ID, Account, User string
	States            []ethStateRec
	FundTx            string
	FundAmt           int64
	Settling          bool
	ExchLast          int
	ExchPend          string
	Closed            bool
	Events            []string
}

type orderRec struct {
	ID                    int
	Account, Chain, Vault string
	Side                  string
	Price, Sats, Left     int64
	Seq                   int
	Status                string
}

type legRec struct {
	Order       int
	Side        string
	Price, Sats int64
	Done        bool
}

type bookRec struct {
	Orders []orderRec
	Legs   []legRec
	Trades []BookTrade
	Seq    int
}

type xSave struct {
	Version    int
	Chains     map[string]xChainRec
	Vaults     []xVaultRec
	Cash       map[string]int64
	EVM        map[string]*xEthRec // eth, pol, base, bnb, avax
	Eth        *xEthRec            // files saved before the other EVM chains: Ethereum only
	EthVaults  []xEthVaultRec
	Books      map[string]bookRec
	OrderSeq   int
	Accounts   map[string]string `json:",omitempty"` // shared mode: each account's key (split_shared.go)
	AdminToken string            `json:",omitempty"` // shared mode: the operator's token
}

// SaveTo makes the exchange save itself to path after every change.
func (x *Exchange) SaveTo(path string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.file = path
	x.save()
}

// save writes the file if anything changed (x.mu held).
func (x *Exchange) save() {
	if x.file == "" {
		return
	}
	data, err := json.MarshalIndent(x.record(), "", " ")
	if err == nil && bytes.Equal(data, x.lastSaved) {
		return
	}
	if err == nil {
		var out []byte
		if out, err = sealIfSet(x.seal, data); err == nil {
			err = writeFile(x.file, out)
		}
	}
	if err != nil {
		if msg := err.Error(); msg != x.saveErr {
			x.saveErr = msg
			fmt.Println("Couldn't save the exchange's data:", msg)
		}
		return
	}
	x.lastSaved, x.saveErr = data, ""
}

func (x *Exchange) record() xSave {
	s := xSave{Version: saveVersion, Chains: map[string]xChainRec{}, Cash: x.cash, Books: map[string]bookRec{}, OrderSeq: x.orderSeq,
		Vaults: []xVaultRec{}, EthVaults: []xEthVaultRec{}}
	if len(x.acctKeys) > 0 {
		s.Accounts = x.acctKeys
	}
	s.AdminToken = x.adminToken
	for id, xc := range x.chains {
		s.Chains[id] = xChainRec{xc.c.tag(), keyRec(xc.key), utxoRecs(xc.coins), xc.price, xc.mode}
	}
	for _, v := range x.vaults {
		s.Vaults = append(s.Vaults, xVaultRec{ID: v.id, Account: v.account, Chain: v.chain, Key: keyRec(v.key),
			Vault: saveVault(v.def, v.gens, v.states), Opened: v.opened, SpliceTx: txRec(v.spliceTx), SpliceFinished: v.spliceFinished,
			SettleTx: txRec(v.settleTx), Settling: v.settling, Published: stateIndex(v.states, v.published), PublishedBy: v.publishedBy,
			Closed: v.closed, SweepKind: v.sweepKind, Events: v.events})
	}
	// Chains that aren't running this time go back into the file as they were
	// loaded, so their keys, coins and vaults are there when they run again.
	for id, cr := range x.kept.Chains {
		if _, ok := s.Chains[id]; !ok {
			s.Chains[id] = cr
		}
	}
	s.Vaults = append(s.Vaults, x.kept.Vaults...)
	sortBy(s.Vaults, func(v xVaultRec) string { return v.ID })
	for id, xe := range x.evm {
		er := &xEthRec{Key: keyRec(xe.key), Contract: xe.c.contract, Price: xe.price, Mode: xe.mode}
		for _, v := range xe.vaults {
			er.Vaults = append(er.Vaults, xEthVaultRec{v.id.String(), v.account, v.user, ethStateRecs(v.states), v.fundTx, v.fundAmt,
				v.settling, v.exchLast, v.exchPend, v.closed, v.events})
		}
		sortBy(er.Vaults, func(v xEthVaultRec) string { return fmt.Sprintf("%020s", v.ID) })
		if s.EVM == nil {
			s.EVM = map[string]*xEthRec{}
		}
		s.EVM[id] = er
	}
	for id, er := range x.kept.EVM {
		if _, ok := s.EVM[id]; !ok {
			if s.EVM == nil {
				s.EVM = map[string]*xEthRec{}
			}
			s.EVM[id] = er
		}
	}
	for chain, b := range x.books {
		br := bookRec{Trades: b.trades, Seq: b.seq, Orders: []orderRec{}, Legs: []legRec{}}
		for _, o := range b.orders {
			br.Orders = append(br.Orders, orderRec{o.id, o.account, o.chain, o.vault, o.side, o.price, o.sats, o.left, o.seq, o.status})
		}
		for _, l := range b.legs {
			br.Legs = append(br.Legs, legRec{l.order.id, l.side, l.price, l.sats, l.done})
		}
		s.Books[chain] = br
	}
	return s
}

// Load reads a saved exchange. Call it after NewExchange and AttachEth and
// before serving anything. It reports whether there was a file to load.
func (x *Exchange) Load(path string) (bool, error) {
	data, err := readSaved(path, x.seal)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var s xSave
	if err := json.Unmarshal(data, &s); err != nil {
		return false, fmt.Errorf("%s is damaged: %v", path, err)
	}
	if s.Version != saveVersion {
		return false, fmt.Errorf("%s was saved by a different version", path)
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if s.EVM == nil && s.Eth != nil {
		s.Eth.Vaults = s.EthVaults
		s.EVM = map[string]*xEthRec{"eth": s.Eth}
	}
	x.kept = xSave{Chains: map[string]xChainRec{}, EVM: map[string]*xEthRec{}}
	for id, cr := range s.Chains {
		if _, ok := x.chains[id]; !ok {
			x.kept.Chains[id] = cr
		}
	}
	for _, vr := range s.Vaults {
		if _, ok := x.chains[vr.Chain]; !ok {
			x.kept.Vaults = append(x.kept.Vaults, vr)
		}
	}
	for id, er := range s.EVM {
		if x.evm[id] == nil {
			x.kept.EVM[id] = er
		}
	}
	notRunning(path, x.kept.Chains, x.kept.EVM)
	for id, cr := range s.Chains {
		xc, ok := x.chains[id]
		if !ok {
			continue
		}
		if err := sameChain(path, id, cr.Tag, xc.c); err != nil {
			return false, err
		}
		k, err := cr.Key.key()
		if err != nil {
			return false, err
		}
		xc.key, xc.pay, xc.coins, xc.price, xc.mode = k, walletScript(k), utxoLoad(cr.Coins), cr.Price, cr.Mode
	}
	for _, vr := range s.Vaults {
		if _, ok := x.chains[vr.Chain]; !ok {
			continue
		}
		k, err := vr.Key.key()
		if err != nil {
			return false, err
		}
		def, gens, states, err := vr.Vault.load()
		if err != nil {
			return false, fmt.Errorf("vault %s: %v", vr.ID, err)
		}
		v := &xVault{id: vr.ID, account: vr.Account, chain: vr.Chain, key: k, def: def, gens: gens, states: states,
			opened: vr.Opened, spliceFinished: vr.SpliceFinished, settling: vr.Settling,
			published: stateAt(states, vr.Published), publishedBy: vr.PublishedBy, closed: vr.Closed, sweepKind: vr.SweepKind, events: vr.Events}
		if v.spliceTx, err = txLoad(vr.SpliceTx); err != nil {
			return false, err
		}
		if v.settleTx, err = txLoad(vr.SettleTx); err != nil {
			return false, err
		}
		x.vaults[v.id] = v
	}
	if s.Cash != nil {
		x.cash = s.Cash
	}
	for chain, er := range s.EVM {
		xe := x.evm[chain]
		if xe == nil {
			continue
		}
		k, err := er.Key.key()
		if err != nil {
			return false, err
		}
		xe.key, xe.addr, xe.price, xe.mode = k, ethAddress(k), er.Price, er.Mode
		if xe.c.public && xe.c.contract == "" && er.Contract != "" && xe.c.codeOK(er.Contract) {
			xe.c.contract = er.Contract // the chains program forgot it; the exchange tells it again
		}
		if strings.EqualFold(er.Contract, xe.c.contract) {
			for _, vr := range er.Vaults {
				id, ok := new(big.Int).SetString(vr.ID, 10)
				if !ok {
					continue
				}
				xe.vaults[vr.ID] = &xEthVault{id: id, account: vr.Account, user: vr.User, states: ethStatesLoad(vr.States),
					fundTx: vr.FundTx, fundAmt: vr.FundAmt, settling: vr.Settling, exchLast: vr.ExchLast, exchPend: vr.ExchPend,
					closed: vr.Closed, events: vr.Events}
			}
		} else if len(er.Vaults) > 0 {
			fmt.Printf("The %s contract changed (a restarted test node, or a new version of the contract), so the vaults saved on the old one are left out.\n", evmName(chain))
		}
	}
	x.books = map[string]*book{}
	for chain, br := range s.Books {
		b := &book{trades: br.Trades, seq: br.Seq}
		byID := map[int]*bookOrder{}
		for _, o := range br.Orders {
			bo := &bookOrder{id: o.ID, account: o.Account, chain: o.Chain, vault: o.Vault, side: o.Side,
				price: o.Price, sats: o.Sats, left: o.Left, seq: o.Seq, status: o.Status}
			byID[o.ID] = bo
			b.orders = append(b.orders, bo)
		}
		for _, l := range br.Legs {
			if o := byID[l.Order]; o != nil {
				b.legs = append(b.legs, &fillLeg{order: o, side: l.Side, price: l.Price, sats: l.Sats, done: l.Done})
			}
		}
		x.books[chain] = b
	}
	x.orderSeq = s.OrderSeq
	for a, k := range s.Accounts {
		x.acctKeys[a] = k
	}
	if s.AdminToken != "" {
		x.adminToken = s.AdminToken
	}
	return true, nil
}

// ------------------------------------------------------------------ wallet

type wVaultRec struct {
	ID             string
	Key            keyHex
	Vault          vaultRec
	Fund           string
	SpliceTx       string
	SpliceFinished bool
	ExchFault      string
	ExitSent       bool
	SettleTx       string
	CloseTx        string
	Published      int
	PublishedBy    string
	Closed         string
	SweepKind      string
	Events         []string
}

type wChainRec struct {
	Tag   string
	Key   keyHex
	Coins []utxoRec
	Vault *wVaultRec
}

type wEthVaultRec struct {
	ID              string
	States          []ethStateRec
	OpenTx, ExitTx  string
	CloseTx, Closed string
	CapPending      bool
	CapSats         int64
	UserLast        int
	UserPend        string
	ExchFault       string
	Events          []string
}

type wEthRec struct {
	Key      keyHex
	Contract string
	ExchAddr string
	Vault    *wEthVaultRec
}

type wSave struct {
	Version       int
	Account, Name string
	WT            bool
	Chains        map[string]wChainRec
	EVM           map[string]*wEthRec // eth, pol, base, bnb, avax
	Eth           *wEthRec            // files saved before the other EVM chains: Ethereum only
	Orders        []wOrder
	Tower         *wTowerRec `json:",omitempty"` // a separate watchtower in use (split_tower.go)
	AcctKey       keyHex     `json:",omitempty"` // signs requests to the exchange (split_shared.go)
}

// SaveTo makes the wallet save itself to path after every change.
func (w *Wallet) SaveTo(path string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.file = path
	w.save()
}

// Save writes the wallet's file if anything changed.
func (w *Wallet) Save() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.save()
}

func (w *Wallet) save() {
	if w.file == "" {
		return
	}
	data, err := json.MarshalIndent(w.record(), "", " ")
	if err == nil && bytes.Equal(data, w.lastSaved) {
		return
	}
	if err == nil {
		var out []byte
		if out, err = sealIfSet(w.seal, data); err == nil {
			err = writeFile(w.file, out)
		}
	}
	if err != nil {
		if msg := err.Error(); msg != w.saveErr {
			w.saveErr = msg
			fmt.Println("Couldn't save the wallet's data:", msg)
		}
		return
	}
	w.lastSaved, w.saveErr = data, ""
}

func (w *Wallet) record() wSave {
	s := wSave{Version: saveVersion, Account: w.Account, Name: w.Name, WT: w.WT, Chains: map[string]wChainRec{}, Orders: []wOrder{}}
	if w.tower != "" {
		s.Tower = &wTowerRec{w.tower, w.towerToken}
	}
	s.AcctKey = keyRec(w.acctKey)
	for id, wc := range w.chains {
		cr := wChainRec{Tag: wc.c.tag(), Key: keyRec(wc.key), Coins: utxoRecs(wc.coins)}
		if v := wc.vault; v != nil {
			cr.Vault = &wVaultRec{ID: v.id, Key: keyRec(v.key), Vault: saveVault(v.def, v.gens, v.states), Fund: txRec(v.fund),
				SpliceTx: txRec(v.spliceTx), SpliceFinished: v.spliceFinished, ExchFault: v.exchFault, ExitSent: v.exitSent,
				SettleTx: txRec(v.settleTx), CloseTx: txRec(v.closeTx), Published: stateIndex(v.states, v.published),
				PublishedBy: v.publishedBy, Closed: v.closed, SweepKind: v.sweepKind, Events: v.Events}
		}
		s.Chains[id] = cr
	}
	for id, we := range w.evm {
		er := &wEthRec{Key: keyRec(we.key), Contract: we.c.contract, ExchAddr: we.exchAddr}
		if v := we.vault; v != nil {
			er.Vault = &wEthVaultRec{v.id.String(), ethStateRecs(v.states), v.openTx, v.exitTx, v.closeTx, v.closed,
				v.capPending, v.capSats, v.userLast, v.userPend, v.exchFault, v.Events}
		}
		if s.EVM == nil {
			s.EVM = map[string]*wEthRec{}
		}
		s.EVM[id] = er
	}
	// Chains that aren't running this time go back into the file as they were.
	for id, cr := range w.kept.Chains {
		if _, ok := s.Chains[id]; !ok {
			s.Chains[id] = cr
		}
	}
	for id, er := range w.kept.EVM {
		if _, ok := s.EVM[id]; !ok {
			if s.EVM == nil {
				s.EVM = map[string]*wEthRec{}
			}
			s.EVM[id] = er
		}
	}
	for _, o := range w.orders {
		s.Orders = append(s.Orders, *o)
	}
	sortBy(s.Orders, func(o wOrder) string { return fmt.Sprintf("%09d", o.ID) })
	return s
}

// Load reads a saved wallet. Call it after NewWallet and AttachEth, before
// using the wallet. It reports whether there was a file to load.
func (w *Wallet) Load(path string) (bool, error) {
	data, err := readSaved(path, w.seal)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var s wSave
	if err := json.Unmarshal(data, &s); err != nil {
		return false, fmt.Errorf("%s is damaged: %v", path, err)
	}
	if s.Version != saveVersion {
		return false, fmt.Errorf("%s was saved by a different version", path)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.Account, w.Name, w.WT = s.Account, s.Name, s.WT
	if s.Tower != nil {
		w.tower, w.towerToken = s.Tower.URL, s.Tower.Token
	}
	if s.AcctKey != "" {
		k, err := s.AcctKey.key()
		if err != nil {
			return false, err
		}
		w.acctKey = k
	}
	if s.EVM == nil && s.Eth != nil {
		s.EVM = map[string]*wEthRec{"eth": s.Eth}
	}
	w.kept = wSave{Chains: map[string]wChainRec{}, EVM: map[string]*wEthRec{}}
	for id, cr := range s.Chains {
		if _, ok := w.chains[id]; !ok {
			w.kept.Chains[id] = cr
		}
	}
	for id, er := range s.EVM {
		if w.evm[id] == nil {
			w.kept.EVM[id] = er
		}
	}
	notRunning(path, w.kept.Chains, w.kept.EVM)
	for id, cr := range s.Chains {
		wc, ok := w.chains[id]
		if !ok {
			continue
		}
		if err := sameChain(path, id, cr.Tag, wc.c); err != nil {
			return false, err
		}
		k, err := cr.Key.key()
		if err != nil {
			return false, err
		}
		wc.key, wc.pay, wc.coins, wc.vault = k, walletScript(k), utxoLoad(cr.Coins), nil
		vr := cr.Vault
		if vr == nil {
			continue
		}
		vk, err := vr.Key.key()
		if err != nil {
			return false, err
		}
		def, gens, states, err := vr.Vault.load()
		if err != nil {
			return false, fmt.Errorf("your %s vault: %v", id, err)
		}
		v := &wVault{id: vr.ID, key: vk, def: def, gens: gens, states: states, spliceFinished: vr.SpliceFinished,
			exchFault: vr.ExchFault, exitSent: vr.ExitSent, published: stateAt(states, vr.Published),
			publishedBy: vr.PublishedBy, closed: vr.Closed, sweepKind: vr.SweepKind, Events: vr.Events}
		for _, p := range []struct {
			s  string
			tx **wire.MsgTx
		}{{vr.Fund, &v.fund}, {vr.SpliceTx, &v.spliceTx}, {vr.SettleTx, &v.settleTx}, {vr.CloseTx, &v.closeTx}} {
			if *p.tx, err = txLoad(p.s); err != nil {
				return false, err
			}
		}
		wc.vault = v
	}
	for chain, er := range s.EVM {
		we := w.evm[chain]
		if we == nil {
			continue
		}
		k, err := er.Key.key()
		if err != nil {
			return false, err
		}
		we.key, we.addr, we.exchAddr, we.vault = k, ethAddress(k), er.ExchAddr, nil
		if we.c.public && we.c.contract == "" && er.Contract != "" && we.c.codeOK(er.Contract) {
			we.c.contract = er.Contract // the chains program forgot it
		}
		if vr := er.Vault; vr != nil {
			id, ok := new(big.Int).SetString(vr.ID, 10)
			if ok && strings.EqualFold(er.Contract, we.c.contract) {
				we.vault = &wEthVault{id: id, states: ethStatesLoad(vr.States), openTx: vr.OpenTx, exitTx: vr.ExitTx,
					closeTx: vr.CloseTx, closed: vr.Closed, capPending: vr.CapPending, capSats: vr.CapSats,
					userLast: vr.UserLast, userPend: vr.UserPend, exchFault: vr.ExchFault, Events: vr.Events}
			} else if ok {
				fmt.Printf("The %s contract changed (a restarted test node, or a new version of the contract), so your vault saved on the old one is left out.\n", evmName(chain))
			}
		}
	}
	w.orders = map[int]*wOrder{}
	for _, o := range s.Orders {
		o := o
		w.orders[o.ID] = &o
	}
	return true, nil
}

// notRunning says which saved chains aren't running this time. Their saved
// data stays in the file unchanged (see kept), ready for when they run again.
func notRunning[U, E any](path string, utxo map[string]U, evm map[string]E) {
	var names []string
	for id := range utxo {
		names = append(names, strings.ToUpper(id))
	}
	for id := range evm {
		names = append(names, evmName(id))
	}
	if len(names) == 0 {
		return
	}
	sort.Strings(names)
	fmt.Printf("Not running this time: %s. Their saved keys, coins and vaults stay in %s for when they run again.\n",
		strings.Join(names, ", "), filepath.Base(path))
}

// sortBy sorts a slice by a string key, so saved files keep a stable order.
func sortBy[T any](s []T, key func(T) string) {
	sort.Slice(s, func(i, j int) bool { return key(s[i]) < key(s[j]) })
}

// ------------------------------------------------------------------ chains

// minerRec is what a kept test chain needs besides the node's own data: the
// key its blocks are mined to, and the next coinbase the faucet may spend.
type minerRec struct {
	Key    keyHex
	NextCB int64
}

// loadMiner reads the kept chain's mining key, or saves a new one (e.dir set).
func (e *UTXOEngine) loadMiner() error {
	path := filepath.Join(e.dir, "miner.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return writeFile(path, e.minerJSON())
	}
	if err != nil {
		return err
	}
	var r minerRec
	if err := json.Unmarshal(data, &r); err != nil {
		return fmt.Errorf("%s is damaged: %v", path, err)
	}
	k, err := r.Key.key()
	if err != nil {
		return err
	}
	e.miner, e.minerPk, e.nextCB = k, walletScript(k), r.NextCB
	return nil
}

func (e *UTXOEngine) minerJSON() []byte {
	b, _ := json.MarshalIndent(minerRec{keyRec(e.miner), e.nextCB}, "", " ")
	return b
}

// saveMiner records which coinbase the faucet spends next (kept chains only).
func (e *UTXOEngine) saveMiner() {
	if e.cfg.DataDir != "" {
		if err := writeFile(filepath.Join(e.dir, "miner.json"), e.minerJSON()); err != nil {
			fmt.Println("Couldn't save the test chain's faucet position:", err)
		}
	}
}

// sameChain refuses a saved file made on a different test chain (for example
// after the chains program was started with -fresh).
func sameChain(path, chain, saved string, c *chainConn) error {
	if now := c.tag(); saved != "" && now != "" && saved != now {
		return fmt.Errorf("%s was saved on a different %s test chain (the chains were started fresh). "+
			"Start this program with -fresh as well, or start the chains without -fresh", path, strings.ToUpper(chain))
	}
	return nil
}

// The chains program remembers the CastleVault contract it deployed, with a
// hash of the compiled code, and only reuses it for the same code.
type contractRec struct {
	Contract, Code string
}

func codeHash() string {
	return hex.EncodeToString(hash32([]byte(strings.TrimSpace(castleVaultBin))))[:16]
}

func keptContract(path string) string {
	var r contractRec
	if data, err := os.ReadFile(path); err == nil && json.Unmarshal(data, &r) == nil && r.Code == codeHash() {
		return r.Contract
	}
	return ""
}

func saveContract(path, addr string) error {
	b, _ := json.MarshalIndent(contractRec{addr, codeHash()}, "", " ")
	return writeFile(path, b)
}

// ------------------------------------------------------------------ program start

// startFile loads a program's saved file (after moving it aside for -fresh).
// It stops the program with a plain message if the file can't be used.
func startFile(path string, fresh bool, load func(string) (bool, error)) bool {
	if fresh {
		if old, err := setAside(path); err != nil {
			fmt.Println("Couldn't move the old file aside:", err)
			os.Exit(1)
		} else if old != "" {
			fmt.Println("Started fresh. The old file is now", old)
		}
	}
	loaded, err := load(path)
	if err != nil {
		fmt.Println("Couldn't load the saved data:", err)
		os.Exit(1)
	}
	if loaded {
		fmt.Println("Carrying on from", path)
	}
	return loaded
}

func validName(s string) bool {
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return len(s) <= 32
}

func walletFile(name string) string {
	if name == "" {
		return "wallet.json"
	}
	return "wallet-" + strings.ToLower(name) + ".json"
}

// keepChain makes a test chain last between runs: it lives in dir, on a fixed
// RPC port. (btcd and ltcd wipe a regtest chain every time they start, so
// the engine saves each block it mines and feeds them back in on the next
// start; see replayBlocks.)
func keepChain(cfg *UTXOConfig, dir string, port int) {
	cfg.DataDir, cfg.RPCPort = dir, port
}

// keepBlocks appends every block mined since the last call to blocks.txt
// (kept chains only), one block per line as hex.
func (e *UTXOEngine) keepBlocks() error {
	if e.cfg.DataDir == "" || e.signet() {
		return nil
	}
	tip, err := e.rpc.GetBlockCount()
	if err != nil || tip <= e.kept {
		return err
	}
	f, err := os.OpenFile(filepath.Join(e.dir, "blocks.txt"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	for h := e.kept + 1; h <= tip; h++ {
		hash, err := e.rpc.GetBlockHash(h)
		if err != nil {
			return err
		}
		// Raw block as hex: btcd wants verbosity 0, older nodes the word false.
		raw, err := e.rpc.RawRequest("getblock", []json.RawMessage{jsonStr(hash.String()), json.RawMessage("0")})
		if err != nil {
			raw, err = e.rpc.RawRequest("getblock", []json.RawMessage{jsonStr(hash.String()), json.RawMessage("false")})
		}
		if err != nil {
			return err
		}
		var hexBlock string
		if err := json.Unmarshal(raw, &hexBlock); err != nil {
			return err
		}
		if _, err := f.WriteString(hexBlock + "\n"); err != nil {
			return err
		}
		e.kept = h
	}
	return nil
}

// replayBlocks feeds the kept blocks back into the node, which starts every
// run with an empty regtest chain. The node checks each block again.
func (e *UTXOEngine) replayBlocks() error {
	data, err := os.ReadFile(filepath.Join(e.dir, "blocks.txt"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	lines := strings.Fields(string(data))
	for i, b := range lines {
		if i%50 == 0 {
			e.mu.Lock()
			e.progress = fmt.Sprintf("Loading the kept test chain… block %d of %d", i, len(lines))
			e.mu.Unlock()
		}
		raw, err := e.rpc.RawRequest("submitblock", []json.RawMessage{jsonStr(b)})
		if err != nil {
			return fmt.Errorf("the kept %s chain didn't load at block %d: %v", e.cfg.Info.Name, i+1, err)
		}
		if s := strings.Trim(string(raw), `"`); s != "null" && s != "" {
			return fmt.Errorf("the kept %s chain didn't load at block %d: %s", e.cfg.Info.Name, i+1, s)
		}
	}
	e.kept = int64(len(lines))
	return nil
}

func jsonStr(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}
