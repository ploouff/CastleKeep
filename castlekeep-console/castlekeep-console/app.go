package main

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// ------------------------------------------------------------------ types

type Event struct {
	ID     int    `json:"id"`
	Height int64  `json:"height"`
	Time   string `json:"time"`
	Kind   string `json:"kind"` // info, ok, bad, warn, sign
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
	Txid   string `json:"txid,omitempty"`
}

type IO struct {
	Label string `json:"label"`
	Sats  int64  `json:"sats"`
	Ref   string `json:"ref"`
}

type TxInfo struct {
	Txid    string `json:"txid"`
	Label   string `json:"label"`
	Path    string `json:"path"`
	Script  string `json:"script,omitempty"`
	Inputs  []IO   `json:"inputs"`
	Outputs []IO   `json:"outputs"`
	Status  string `json:"status"` // mempool, confirmed, rejected
	Height  int64  `json:"height,omitempty"`
	Error   string `json:"error,omitempty"`
	Seq     int    `json:"seq"`
}

type utxo struct {
	op  wire.OutPoint
	amt int64
}

type App struct {
	mu sync.Mutex

	ready    bool
	progress string
	initErr  string

	rpc     *rpcclient.Client
	miner   *btcec.PrivateKey
	minerPk []byte
	nextCB  int64

	userWallet, exchWallet *btcec.PrivateKey
	userPk, exchPk         []byte
	userUtxos, exchUtxos   []utxo

	cashCents int64
	price     int64
	mode      string // honest, offline, malicious

	vault       *Vault
	vaultsDone  int
	events      []Event
	txs         map[string]*TxInfo
	txSeq       int
	outLabels   map[wire.OutPoint]IO
	confirmedAt map[string]int64
}

func newApp() *App {
	uw, _ := btcec.NewPrivateKey()
	ew, _ := btcec.NewPrivateKey()
	m, _ := btcec.NewPrivateKey()
	return &App{
		progress:    "Starting the Bitcoin node…",
		miner:       m,
		minerPk:     walletScript(m),
		nextCB:      1,
		userWallet:  uw,
		exchWallet:  ew,
		userPk:      walletScript(uw),
		exchPk:      walletScript(ew),
		price:       65000,
		mode:        "honest",
		txs:         map[string]*TxInfo{},
		outLabels:   map[wire.OutPoint]IO{},
		confirmedAt: map[string]int64{},
	}
}

// ------------------------------------------------------------------ helpers

var errUser = errors.New("")

func userErr(f string, a ...any) error { return fmt.Errorf("%w%s", errUser, fmt.Sprintf(f, a...)) }

func (a *App) height() int64 {
	h, _ := a.rpc.GetBlockCount()
	return h
}

func (a *App) log(kind, title, detail, txid string) {
	a.events = append(a.events, Event{
		ID: len(a.events) + 1, Height: a.height(), Time: time.Now().Format("15:04:05"),
		Kind: kind, Title: title, Detail: detail, Txid: txid,
	})
	if len(a.events) > 300 {
		a.events = a.events[len(a.events)-300:]
	}
}

func fmtBTC(s int64) string {
	str := fmt.Sprintf("%.8f", float64(s)/sat)
	str = strings.TrimRight(strings.TrimRight(str, "0"), ".")
	return str + " BTC"
}

func fmtUSD(cents int64) string {
	d := cents / 100
	s := fmt.Sprintf("%d", d)
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return "$" + string(out)
}

func (a *App) labelScript(pk []byte) string {
	switch {
	case string(pk) == string(a.userPk):
		return "Your wallet"
	case string(pk) == string(a.exchPk):
		return "Exchange wallet (buyers' BTC)"
	case string(pk) == string(a.minerPk):
		return "Test faucet (change)"
	}
	if v := a.vault; v != nil {
		if string(pk) == string(v.V.pkScript) {
			return "Vault (you + exchange)"
		}
		if string(pk) == string(v.E.pkScript) {
			return "Exit output (you + exchange, or you alone after 144 blocks)"
		}
	}
	return "Unknown"
}

func cleanNodeErr(err error) string {
	s := err.Error()
	switch {
	case strings.Contains(s, "sequence locks"):
		return "The 144-block timelock has not passed yet."
	case strings.Contains(s, "orphan") || strings.Contains(s, "already spent") || strings.Contains(s, "spent"):
		return "The coins this spends are already gone; another transaction spent them first."
	case strings.Contains(s, "failed to validate input"):
		return "Script check failed: a valid signature from you is missing."
	}
	if i := strings.Index(s, "TX rejected:"); i >= 0 {
		s = strings.TrimSpace(s[i+len("TX rejected:"):])
	}
	return s
}

// broadcast registers, sends and logs a transaction. It returns nil on
// acceptance, or the node's rejection.
func (a *App) broadcast(tx *wire.MsgTx, label, path string, script []byte, okTitle, badTitle string) error {
	txid := tx.TxHash().String()
	info := &TxInfo{Txid: txid, Label: label, Path: path, Seq: a.txSeq}
	a.txSeq++
	if script != nil {
		info.Script = disasm(script)
		if a.vault != nil {
			info.Script = a.vault.friendlyScript(script)
		}
	}
	for _, in := range tx.TxIn {
		io, ok := a.outLabels[in.PreviousOutPoint]
		if !ok {
			io = IO{Label: "Unknown", Ref: in.PreviousOutPoint.String()}
		}
		info.Inputs = append(info.Inputs, io)
	}
	for i, out := range tx.TxOut {
		info.Outputs = append(info.Outputs, IO{Label: a.labelScript(out.PkScript), Sats: out.Value, Ref: fmt.Sprintf("%s:%d", txid, i)})
	}
	a.txs[txid] = info

	if _, err := a.rpc.SendRawTransaction(tx, true); err != nil {
		info.Status = "rejected"
		info.Error = cleanNodeErr(err)
		a.log("bad", badTitle, "Rejected by the node: "+info.Error, txid)
		return err
	}
	info.Status = "mempool"
	for i, out := range tx.TxOut {
		op := wire.OutPoint{Hash: tx.TxHash(), Index: uint32(i)}
		a.outLabels[op] = IO{Label: a.labelScript(out.PkScript), Sats: out.Value, Ref: op.String()}
		if string(out.PkScript) == string(a.userPk) {
			a.userUtxos = append(a.userUtxos, utxo{op, out.Value})
		}
		if string(out.PkScript) == string(a.exchPk) {
			a.exchUtxos = append(a.exchUtxos, utxo{op, out.Value})
		}
	}
	a.log("ok", okTitle, "Accepted into the mempool. Mine a block to confirm it.", txid)
	return nil
}

func (a *App) confs(txid string) int64 {
	if h, ok := a.confirmedAt[txid]; ok {
		return a.height() - h + 1
	}
	hash, err := chainhash.NewHashFromStr(txid)
	if err != nil {
		return 0
	}
	r, err := a.rpc.GetRawTransactionVerbose(hash)
	if err != nil || r.Confirmations == 0 {
		return 0
	}
	a.confirmedAt[txid] = a.height() - int64(r.Confirmations) + 1
	if t := a.txs[txid]; t != nil {
		t.Status, t.Height = "confirmed", a.confirmedAt[txid]
	}
	return int64(r.Confirmations)
}

func (a *App) unspent(op wire.OutPoint) (int64, bool) {
	r, err := a.rpc.GetTxOut(&op.Hash, op.Index, true)
	if err != nil || r == nil {
		return 0, false
	}
	amt, _ := btcutil.NewAmount(r.Value)
	return int64(amt), true
}

func (a *App) walletBalance(list *[]utxo) (total int64) {
	keep := (*list)[:0]
	for _, u := range *list {
		if _, ok := a.unspent(u.op); ok {
			keep = append(keep, u)
			total += u.amt
		}
	}
	*list = keep
	return
}

// ------------------------------------------------------------------ init

func (a *App) initChain() error {
	const total = 500
	for done := 0; done < total; done += 50 {
		a.mu.Lock()
		a.progress = fmt.Sprintf("Mining test blocks to activate Taproot… %d of %d", done, total)
		a.mu.Unlock()
		if _, err := a.rpc.Generate(50); err != nil {
			return err
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	info, err := a.rpc.GetBlockChainInfo()
	if err != nil {
		return err
	}
	if f, ok := info.Bip9SoftForks["taproot"]; !ok || f.Status != "active" {
		return errors.New("taproot did not activate on the test chain")
	}
	a.log("info", "Test chain ready", "500 blocks mined. CSV, SegWit and Taproot are active.", "")
	for i := 0; i < 2; i++ {
		if err := a.faucet(); err != nil {
			return err
		}
	}
	if _, err := a.rpc.Generate(1); err != nil {
		return err
	}
	a.log("info", "Your wallet has 2 test BTC", "Lock some into a vault to start trading.", "")
	a.progress = "Ready"
	a.ready = true
	return nil
}

// ------------------------------------------------------------------ actions

func (a *App) faucet() error {
	if a.height()-a.nextCB < 100 {
		return userErr("No mature test coins left. Mine 100 blocks and try again.")
	}
	hash, err := a.rpc.GetBlockHash(a.nextCB)
	if err != nil {
		return err
	}
	blk, err := a.rpc.GetBlock(hash)
	if err != nil {
		return err
	}
	a.nextCB++
	cb := blk.Transactions[0]
	prev := wire.OutPoint{Hash: cb.TxHash(), Index: 0}
	prevAmt := cb.TxOut[0].Value
	a.outLabels[prev] = IO{Label: "Test faucet (mined coins)", Sats: prevAmt, Ref: prev.String()}

	tx := wire.NewMsgTx(2)
	tx.AddTxIn(wire.NewTxIn(&prev, nil, nil))
	tx.AddTxOut(wire.NewTxOut(1*sat, a.userPk))
	tx.AddTxOut(wire.NewTxOut(prevAmt-1*sat-txFee, a.minerPk))
	hashes := txscript.NewTxSigHashes(tx, txscript.NewCannedPrevOutputFetcher(a.minerPk, prevAmt))
	wit, err := txscript.TaprootWitnessSignature(tx, hashes, 0, prevAmt, a.minerPk, txscript.SigHashDefault, a.miner)
	if err != nil {
		return err
	}
	tx.TxIn[0].Witness = wit
	return a.broadcast(tx, "Test faucet: 1 BTC to your wallet", "Faucet key (ordinary key path)", nil,
		"Received 1 test BTC", "Faucet payment rejected")
}

func (a *App) mine(n int) error {
	if n < 1 || n > 1000 {
		return userErr("Mine between 1 and 1,000 blocks at a time.")
	}
	if _, err := a.rpc.Generate(uint32(n)); err != nil {
		return err
	}
	hours := float64(n) * 10 / 60
	when := fmt.Sprintf("about %.0f minutes on the real network", float64(n)*10)
	if hours >= 1 {
		when = fmt.Sprintf("about %.0f hours on the real network", hours)
	}
	word := "blocks"
	if n == 1 {
		word = "block"
	}
	a.log("info", fmt.Sprintf("Mined %d %s", n, word), "That's "+when+".", "")
	a.watchtower()
	return nil
}

func (a *App) lock(sats int64) error {
	if a.vault != nil && a.vault.closeTxid == "" {
		return userErr("You already have an open vault. Settle or exit it first.")
	}
	if sats < 100_000 {
		return userErr("Lock at least 0.001 BTC.")
	}
	bal := a.walletBalance(&a.userUtxos)
	if sats+txFee > bal {
		return userErr("Your wallet holds %s. Lock less, or get more test BTC.", fmtBTC(bal))
	}

	v := newVault(a.userPk, a.exchPk)
	tx := wire.NewMsgTx(2)
	prevs := map[wire.OutPoint]*wire.TxOut{}
	var in int64
	for _, u := range a.userUtxos {
		if in >= sats+txFee {
			break
		}
		op := u.op
		tx.AddTxIn(wire.NewTxIn(&op, nil, nil))
		prevs[op] = wire.NewTxOut(u.amt, a.userPk)
		in += u.amt
	}
	tx.AddTxOut(wire.NewTxOut(sats, v.V.pkScript))
	if change := in - sats - txFee; change > 546 {
		tx.AddTxOut(wire.NewTxOut(change, a.userPk))
	}
	hashes := txscript.NewTxSigHashes(tx, txscript.NewMultiPrevOutFetcher(prevs))
	for i, txin := range tx.TxIn {
		p := prevs[txin.PreviousOutPoint]
		wit, err := txscript.TaprootWitnessSignature(tx, hashes, i, p.Value, p.PkScript, txscript.SigHashDefault, a.userWallet)
		if err != nil {
			return err
		}
		tx.TxIn[i].Witness = wit
	}

	prevVault := a.vault
	a.vault = v // so outputs get vault labels
	v.vaultOut = wire.OutPoint{Hash: tx.TxHash(), Index: 0}
	v.vaultAmt = sats
	if err := a.broadcast(tx, "Lock "+fmtBTC(sats)+" into the vault", "Your wallet key (ordinary key path)", nil,
		"Locked "+fmtBTC(sats)+" into a new vault", "Vault deposit rejected"); err != nil {
		a.vault = prevVault
		return nil
	}
	if prevVault != nil {
		a.vaultsDone++
	}
	v.fundTxid = tx.TxHash().String()
	v.presignExit()
	a.log("sign", "You and the exchange pre-signed the exit request",
		"Either side can broadcast it later to start the 144-block clock.", "")
	v.signState("Opening balance", sats, 0, a.height())
	a.log("sign", "You signed state #0: you own the whole vault", "", "")
	return nil
}

func (a *App) vaultStatus() string {
	v := a.vault
	if v == nil {
		return "none"
	}
	if v.closeTxid != "" {
		if a.confs(v.closeTxid) > 0 {
			return "closed"
		}
		return "closing"
	}
	if a.confs(v.fundTxid) == 0 {
		return "funding"
	}
	if v.exitSent {
		if a.confs(v.exitTx.TxHash().String()) == 0 {
			return "exit_pending"
		}
		return "exit_window"
	}
	return "open"
}

func (a *App) order(side string, sats int64) error {
	v := a.vault
	switch st := a.vaultStatus(); st {
	case "open":
	case "funding":
		return userErr("Your deposit hasn't confirmed yet. Mine a block first.")
	case "none", "closed", "closing":
		return userErr("Lock BTC into a vault before trading.")
	default:
		return userErr("Trading is frozen while an exit request is in progress.")
	}
	if a.mode == "offline" {
		return userErr("The exchange is offline and isn't accepting orders.")
	}
	if sats <= 0 {
		return userErr("Enter an amount above zero.")
	}
	l := v.Latest()
	cost := sats * a.price * 100 / sat // cents
	var user, exch int64
	var label string
	switch side {
	case "sell":
		if sats > l.UserSats {
			return userErr("You only have %s in the vault to sell.", fmtBTC(l.UserSats))
		}
		user, exch = l.UserSats-sats, l.ExchSats+sats
		label = fmt.Sprintf("Sold %s at %s", fmtBTC(sats), fmtUSD(a.price*100))
	case "buy":
		if sats > l.ExchSats {
			return userErr("You can only buy back BTC sold from this vault (%s). The vault can't hold more than you locked.", fmtBTC(l.ExchSats))
		}
		if cost > a.cashCents {
			return userErr("That costs %s and you have %s in cash.", fmtUSD(cost), fmtUSD(a.cashCents))
		}
		user, exch = l.UserSats+sats, l.ExchSats-sats
		label = fmt.Sprintf("Bought %s at %s", fmtBTC(sats), fmtUSD(a.price*100))
	default:
		return userErr("Unknown order side.")
	}
	s := v.signState(label, user, exch, a.height())
	a.log("sign", fmt.Sprintf("You signed state #%d before sending the order", s.Index),
		fmt.Sprintf("Yours: %s, owed to buyers: %s. The exchange now holds your signature, so this trade can't be undone.", fmtBTC(user), fmtBTC(exch)), "")
	if side == "sell" {
		a.cashCents += cost
	} else {
		a.cashCents -= cost
	}
	a.log("ok", label, fmt.Sprintf("Filled instantly on the order book. Cash balance: %s.", fmtUSD(a.cashCents)), "")
	return nil
}

func (a *App) settle() error {
	v := a.vault
	if st := a.vaultStatus(); st != "open" {
		if st == "funding" {
			return userErr("Your deposit hasn't confirmed yet. Mine a block first.")
		}
		return userErr("There's no open vault to settle.")
	}
	switch a.mode {
	case "offline":
		a.log("warn", "Settlement request got no answer", "The exchange is offline. Use Request exit to get your coins back without it.", "")
		return nil
	case "malicious":
		a.log("warn", "The exchange refused to co-sign your settlement", "Your only route now is Request exit, which starts the 144-block clock.", "")
		return nil
	}
	tx := v.cooperativeSettle()
	if err := a.broadcast(tx, "Cooperative settlement", "Vault script: you and the exchange both sign now",
		v.multisigLeaf.Script, "Settled: coins on their way back to your wallet", "Settlement rejected"); err == nil {
		v.closeTxid, v.closeKind = tx.TxHash().String(), "settled"
	}
	return nil
}

func (a *App) requestExit(by string) error {
	v := a.vault
	st := a.vaultStatus()
	if st != "open" && st != "funding" {
		return userErr("There's no open vault to exit.")
	}
	if st == "funding" {
		return userErr("Your deposit hasn't confirmed yet. Mine a block first.")
	}
	who := "You"
	if by == "exchange" {
		who = "The exchange"
	}
	if err := a.broadcast(v.exitTx, "Exit request ("+strings.ToLower(who)+")",
		"Vault script: both signatures, pre-signed when the vault opened", v.multisigLeaf.Script,
		who+" broadcast the exit request", "Exit request rejected"); err == nil {
		v.exitSent = true
		a.log("info", "The 144-block clock starts once this confirms",
			"During the window the exchange can publish the latest state you signed. After it, you can claim alone.", "")
	}
	return nil
}

func (a *App) claim() error {
	v := a.vault
	if v == nil || !v.exitSent {
		return userErr("Request an exit first. Claiming alone only works on the exit output.")
	}
	if v.closeTxid != "" {
		return userErr("This vault is already closed.")
	}
	tx := v.userClaim()
	if err := a.broadcast(tx, "Claim alone after the timelock",
		"Exit script: <144> CHECKSEQUENCEVERIFY, then your signature only", v.delayLeaf.Script,
		"You claimed the vault on your own", "Your solo claim was rejected"); err == nil {
		v.closeTxid, v.closeKind = tx.TxHash().String(), "claimed"
		if v.Latest().ExchSats > 0 {
			a.log("warn", "The exchange missed its window, so you kept BTC you had sold",
				"It never published your signed sale in time. This is why a real exchange must watch for exit requests around the clock.", "")
		}
	}
	return nil
}

func (a *App) steal() error {
	v := a.vault
	if v == nil {
		return userErr("Open a vault first so the exchange has something to try to take.")
	}
	var prev wire.OutPoint
	var amt int64
	var out *tapOutput
	if _, ok := a.unspent(v.vaultOut); ok {
		prev, amt, out = v.vaultOut, v.vaultAmt, v.V
	} else if v.exitSent {
		if _, ok := a.unspent(v.exitOut()); ok {
			prev, amt, out = v.exitOut(), v.exitAmt, v.E
		}
	}
	if out == nil {
		return userErr("There are no vault coins left to take.")
	}
	tx := v.exchangeOnlySpend(prev, amt, out)
	a.broadcast(tx, "Exchange tries to take the vault", "Vault script with the exchange's signature only",
		v.multisigLeaf.Script, "The exchange took the vault", "The exchange tried to take your coins alone")
	return nil
}

func (a *App) setMode(m string) error {
	switch m {
	case "honest":
		a.log("info", "Exchange is online and honest", "", "")
	case "offline":
		a.log("warn", "Exchange went offline", "It won't sign settlements or answer exit requests.", "")
	case "malicious":
		a.log("warn", "Exchange turned malicious", "It refuses to settle, and if an exit happens it publishes whichever signed state pays it most.", "")
	default:
		return userErr("Unknown exchange mode.")
	}
	a.mode = m
	a.watchtower()
	return nil
}

func (a *App) setPrice(usd int64) error {
	if usd < 1000 || usd > 1_000_000 {
		return userErr("Pick a price between $1,000 and $1,000,000.")
	}
	a.price = usd
	return nil
}

// watchtower: the exchange reacts to a confirmed exit request.
func (a *App) watchtower() {
	v := a.vault
	if v == nil || !v.exitSent || v.closeTxid != "" || a.mode == "offline" {
		return
	}
	if a.confs(v.exitTx.TxHash().String()) == 0 {
		return
	}
	pick := v.Latest()
	if a.mode == "malicious" {
		for _, s := range v.States {
			if s.ExchSats > pick.ExchSats {
				pick = s
			}
		}
	}
	tx := v.publishState(pick)
	title := fmt.Sprintf("Exchange watchtower published state #%d", pick.Index)
	if pick != v.Latest() {
		title = fmt.Sprintf("Malicious exchange published an OLD state (#%d, not #%d)", pick.Index, v.Latest().Index)
	}
	if err := a.broadcast(tx, fmt.Sprintf("Exchange publishes state #%d", pick.Index),
		"Exit script: your signature from order placement plus the exchange's", v.multisigLeaf.Script,
		title, "Exchange's state publication rejected"); err == nil {
		v.closeTxid, v.closeKind = tx.TxHash().String(), "published"
		if pick != v.Latest() {
			a.log("warn", "This is the known gap in version 1",
				"You signed that old state, so the node accepts it. Version 2 adds revocation keys: publishing a replaced state would let you take the whole vault.", "")
		}
	}
}
