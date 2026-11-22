package main

// UTXOEngine runs Castlekeep on a Bitcoin-style chain (Bitcoin, Litecoin)
// using a local regtest node (btcd or ltcd) and Taproot vault scripts.

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

type UTXOConfig struct {
	Info         ChainInfo
	NodePath     string
	NodeName     string // "btcd" or "ltcd"
	InstallHint  string
	Params       chaincfg.Params // used for address encoding only
	ExitDelay    int64           // blocks
	PenaltyDelay int64           // D2, blocks: how long the exchange waits for its share of a published state
	BlockMinutes float64
	DefaultPrice int64
	CheckForks   bool // btcd reports soft-fork status; ltcd can't
}

type utxo struct {
	op  wire.OutPoint
	amt int64
}

type UTXOEngine struct {
	cfg UTXOConfig
	mu  sync.Mutex

	available   bool
	unavailable string
	ready       bool
	progress    string
	initErr     string

	node *exec.Cmd
	dir  string
	rpc  *rpcclient.Client

	miner   *btcec.PrivateKey
	minerPk []byte
	nextCB  int64

	userWallet, exchWallet *btcec.PrivateKey
	userPk, exchPk         []byte
	userUtxos, exchUtxos   []utxo

	tradeBook
	userWT bool // the user's watchtower: sweeps revoked states with their secret
	vault  *Vault
	eventLog
	txs         map[string]*TxInfo
	txSeq       int
	outLabels   map[wire.OutPoint]IO
	confirmedAt map[string]int64
}

func NewUTXOEngine(cfg UTXOConfig) *UTXOEngine {
	uw, _ := btcec.NewPrivateKey()
	ew, _ := btcec.NewPrivateKey()
	m, _ := btcec.NewPrivateKey()
	e := &UTXOEngine{
		cfg:         cfg,
		progress:    "Starting the " + cfg.Info.Name + " node…",
		miner:       m,
		minerPk:     walletScript(m),
		nextCB:      1,
		userWallet:  uw,
		exchWallet:  ew,
		userPk:      walletScript(uw),
		exchPk:      walletScript(ew),
		tradeBook:   tradeBook{price: cfg.DefaultPrice, mode: "honest"},
		userWT:      true,
		txs:         map[string]*TxInfo{},
		outLabels:   map[wire.OutPoint]IO{},
		confirmedAt: map[string]int64{},
	}
	if _, err := os.Stat(cfg.NodePath); err != nil {
		e.unavailable = cfg.InstallHint
	} else {
		e.available = true
	}
	return e
}

func (e *UTXOEngine) Info() ChainInfo { return e.cfg.Info }

func (e *UTXOEngine) fmt(s int64) string { return fmtCoin(s, e.cfg.Info.Ticker) }

// ------------------------------------------------------------------ lifecycle

func (e *UTXOEngine) Start() {
	if !e.available {
		return
	}
	go func() {
		if err := e.start(); err != nil {
			e.mu.Lock()
			e.initErr = err.Error()
			e.mu.Unlock()
		}
	}()
}

func (e *UTXOEngine) start() error {
	addr, err := btcutil.NewAddressTaproot(e.minerPk[2:], &e.cfg.Params)
	if err != nil {
		return err
	}
	e.dir, err = os.MkdirTemp("", "castlekeep-"+e.cfg.Info.ID+"-")
	if err != nil {
		return err
	}
	rpcAddr := fmt.Sprintf("127.0.0.1:%d", freePort())
	e.node = exec.Command(e.cfg.NodePath, "--regtest", "--notls", "--txindex",
		"--datadir="+filepath.Join(e.dir, "data"), "--logdir="+filepath.Join(e.dir, "logs"),
		"--rpcuser=castlekeep", "--rpcpass=castlekeep",
		"--rpclisten="+rpcAddr, fmt.Sprintf("--listen=127.0.0.1:%d", freePort()),
		"--miningaddr="+addr.EncodeAddress())
	if err := e.node.Start(); err != nil {
		return fmt.Errorf("couldn't start %s: %v", e.cfg.NodeName, err)
	}
	e.rpc, err = rpcclient.New(&rpcclient.ConnConfig{
		Host: rpcAddr, User: "castlekeep", Pass: "castlekeep", HTTPPostMode: true, DisableTLS: true,
	}, nil)
	if err != nil {
		return err
	}
	for i := 0; ; i++ {
		if _, err := e.rpc.GetBlockCount(); err == nil {
			break
		}
		if i > 120 {
			return fmt.Errorf("the %s node didn't start", e.cfg.NodeName)
		}
		time.Sleep(500 * time.Millisecond)
	}

	const total = 500
	for done := 0; done < total; done += 50 {
		e.mu.Lock()
		e.progress = fmt.Sprintf("Mining test blocks to activate Taproot… %d of %d", done, total)
		e.mu.Unlock()
		if _, err := e.rpc.Generate(50); err != nil {
			return err
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cfg.CheckForks {
		info, err := e.rpc.GetBlockChainInfo()
		if err != nil {
			return err
		}
		if f, ok := info.Bip9SoftForks["taproot"]; !ok || f.Status != "active" {
			return errors.New("taproot did not activate on the test chain")
		}
	}
	e.log("info", "Test chain ready", "500 blocks mined. CSV, SegWit and Taproot are active.", "")
	for i := 0; i < 2; i++ {
		if err := e.faucet(); err != nil {
			return err
		}
	}
	if _, err := e.rpc.Generate(1); err != nil {
		return err
	}
	e.log("info", "Your wallet has 2 test "+e.cfg.Info.Ticker, "Lock some into a vault to start trading.", "")
	e.progress = "Ready"
	e.ready = true
	return nil
}

func (e *UTXOEngine) Stop() {
	if e.node != nil && e.node.Process != nil {
		e.node.Process.Kill()
		e.node.Wait()
	}
	if e.dir != "" {
		os.RemoveAll(e.dir)
	}
}

// ------------------------------------------------------------------ helpers

func (e *UTXOEngine) height() int64 {
	h, _ := e.rpc.GetBlockCount()
	return h
}

func (e *UTXOEngine) log(kind, title, detail, txid string) {
	e.eventLog.add(e.height(), kind, title, detail, txid)
}

func (e *UTXOEngine) labelScript(pk []byte) string {
	t := e.cfg.Info.Ticker
	switch {
	case string(pk) == string(e.userPk):
		return "Your wallet"
	case string(pk) == string(e.exchPk):
		return "Exchange wallet (buyers' " + t + ")"
	case string(pk) == string(e.minerPk):
		return "Test faucet (change)"
	}
	if v := e.vault; v != nil {
		if string(pk) == string(v.V.pkScript) {
			return "Vault (you + exchange)"
		}
		if string(pk) == string(v.E.pkScript) {
			return fmt.Sprintf("Exit output (you + exchange, or you alone after %d blocks)", v.delay)
		}
		for _, s := range v.States {
			if string(pk) == string(s.R.pkScript) {
				return fmt.Sprintf("Exchange's share of state #%d (exchange after %d blocks, or you with revocation secret #%d)", s.Index, v.penaltyDelay, s.Index)
			}
		}
	}
	return "Unknown"
}

func cleanNodeErr(err error) string {
	s := err.Error()
	switch {
	case strings.Contains(s, "sequence locks"):
		return "The exit timelock has not passed yet."
	case strings.Contains(s, "orphan") || strings.Contains(s, "already spent") || strings.Contains(s, "spent"):
		return "The coins this spends are already gone; another transaction spent them first."
	case strings.Contains(s, "failed to validate input"):
		return "Script check failed: a required signature or secret is missing or wrong."
	}
	if i := strings.Index(s, "TX rejected:"); i >= 0 {
		s = strings.TrimSpace(s[i+len("TX rejected:"):])
	}
	return s
}

func (e *UTXOEngine) broadcast(tx *wire.MsgTx, label, path string, script []byte, okTitle, badTitle string) error {
	txid := tx.TxHash().String()
	info := &TxInfo{Txid: txid, Label: label, Path: path, Seq: e.txSeq}
	e.txSeq++
	if script != nil {
		info.Script = disasm(script)
		if e.vault != nil {
			info.Script = e.vault.friendlyScript(script)
		}
	}
	for _, in := range tx.TxIn {
		io, ok := e.outLabels[in.PreviousOutPoint]
		if !ok {
			io = IO{Label: "Unknown", Ref: in.PreviousOutPoint.String()}
		}
		info.Inputs = append(info.Inputs, io)
	}
	for i, out := range tx.TxOut {
		info.Outputs = append(info.Outputs, IO{Label: e.labelScript(out.PkScript), Sats: out.Value, Ref: fmt.Sprintf("%s:%d", txid, i)})
	}
	e.txs[txid] = info

	if _, err := e.rpc.SendRawTransaction(tx, true); err != nil {
		info.Status = "rejected"
		info.Error = cleanNodeErr(err)
		e.log("bad", badTitle, "Rejected by the node: "+info.Error, txid)
		return err
	}
	info.Status = "mempool"
	for i, out := range tx.TxOut {
		op := wire.OutPoint{Hash: tx.TxHash(), Index: uint32(i)}
		e.outLabels[op] = IO{Label: e.labelScript(out.PkScript), Sats: out.Value, Ref: op.String()}
		if string(out.PkScript) == string(e.userPk) {
			e.userUtxos = append(e.userUtxos, utxo{op, out.Value})
		}
		if string(out.PkScript) == string(e.exchPk) {
			e.exchUtxos = append(e.exchUtxos, utxo{op, out.Value})
		}
	}
	e.log("ok", okTitle, "Accepted into the mempool. Mine a block to confirm it.", txid)
	return nil
}

func (e *UTXOEngine) confs(txid string) int64 {
	if h, ok := e.confirmedAt[txid]; ok {
		return e.height() - h + 1
	}
	hash, err := chainhash.NewHashFromStr(txid)
	if err != nil {
		return 0
	}
	r, err := e.rpc.GetRawTransactionVerbose(hash)
	if err != nil || r.Confirmations == 0 {
		return 0
	}
	e.confirmedAt[txid] = e.height() - int64(r.Confirmations) + 1
	if t := e.txs[txid]; t != nil {
		t.Status, t.Height = "confirmed", e.confirmedAt[txid]
	}
	return int64(r.Confirmations)
}

func (e *UTXOEngine) unspent(op wire.OutPoint) (int64, bool) {
	r, err := e.rpc.GetTxOut(&op.Hash, op.Index, true)
	if err != nil || r == nil {
		return 0, false
	}
	amt, _ := btcutil.NewAmount(r.Value)
	return int64(amt), true
}

func (e *UTXOEngine) walletBalance(list *[]utxo) (total int64) {
	keep := (*list)[:0]
	for _, u := range *list {
		if _, ok := e.unspent(u.op); ok {
			keep = append(keep, u)
			total += u.amt
		}
	}
	*list = keep
	return
}

// ------------------------------------------------------------------ actions

func (e *UTXOEngine) Do(r ActionReq) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.ready {
		return userErr("The %s test chain is still starting up.", e.cfg.Info.Name)
	}
	switch r.Action {
	case "faucet":
		return e.faucet()
	case "mine":
		return e.mine(r.N)
	case "lock":
		return e.lock(r.Sats)
	case "order":
		return e.order(r.Side, r.Sats)
	case "settle":
		return e.settle()
	case "exit":
		return e.requestExit(r.By)
	case "claim":
		return e.claim()
	case "steal":
		return e.steal()
	case "mode":
		return e.setMode(r.Mode)
	case "price":
		return e.setPrice(r.USD)
	case "watchtower":
		e.userWT = r.On
		if r.On {
			e.log("info", "Your watchtower is on", "If the exchange publishes a state you hold the revocation secret for, it takes the exchange's share.", "")
		} else {
			e.log("warn", "Your watchtower is off", "Nobody will use your revocation secrets if the exchange publishes an old state.", "")
		}
		e.watchtower()
		return nil
	}
	return userErr("Unknown action.")
}

func (e *UTXOEngine) faucet() error {
	if e.height()-e.nextCB < 100 {
		return userErr("No mature test coins left. Mine 100 blocks and try again.")
	}
	hash, err := e.rpc.GetBlockHash(e.nextCB)
	if err != nil {
		return err
	}
	blk, err := e.rpc.GetBlockVerboseTx(hash)
	if err != nil {
		return err
	}
	e.nextCB++
	txs := blk.Tx
	if len(txs) == 0 {
		txs = blk.RawTx
	}
	if len(txs) == 0 || len(txs[0].Vout) == 0 {
		return errors.New("couldn't read the coinbase transaction")
	}
	cbHash, err := chainhash.NewHashFromStr(txs[0].Txid)
	if err != nil {
		return err
	}
	amt, _ := btcutil.NewAmount(txs[0].Vout[0].Value)
	prevAmt := int64(amt)
	prev := wire.OutPoint{Hash: *cbHash, Index: 0}
	e.outLabels[prev] = IO{Label: "Test faucet (mined coins)", Sats: prevAmt, Ref: prev.String()}
	if prevAmt < unit+2*txFee {
		return userErr("The next test coinbase is too small. Try again.")
	}

	tx := wire.NewMsgTx(2)
	tx.AddTxIn(wire.NewTxIn(&prev, nil, nil))
	tx.AddTxOut(wire.NewTxOut(unit, e.userPk))
	tx.AddTxOut(wire.NewTxOut(prevAmt-unit-txFee, e.minerPk))
	hashes := txscript.NewTxSigHashes(tx, txscript.NewCannedPrevOutputFetcher(e.minerPk, prevAmt))
	wit, err := txscript.TaprootWitnessSignature(tx, hashes, 0, prevAmt, e.minerPk, txscript.SigHashDefault, e.miner)
	if err != nil {
		return err
	}
	tx.TxIn[0].Witness = wit
	return e.broadcast(tx, "Test faucet: 1 "+e.cfg.Info.Ticker+" to your wallet", "Faucet key (ordinary key path)", nil,
		"Received 1 test "+e.cfg.Info.Ticker, "Faucet payment rejected")
}

func (e *UTXOEngine) mine(n int) error {
	if n < 1 || n > 2000 {
		return userErr("Mine between 1 and 2,000 blocks at a time.")
	}
	if _, err := e.rpc.Generate(uint32(n)); err != nil {
		return err
	}
	word := "blocks"
	if n == 1 {
		word = "block"
	}
	e.log("info", fmt.Sprintf("Mined %d %s", n, word),
		"That's "+describeHours(float64(n)*e.cfg.BlockMinutes)+" on the real network.", "")
	e.watchtower()
	return nil
}

func (e *UTXOEngine) lock(sats int64) error {
	t := e.cfg.Info.Ticker
	if e.vault != nil && e.vault.closeTxid == "" {
		return userErr("You already have an open vault. Settle or exit it first.")
	}
	if sats < 100_000 {
		return userErr("Lock at least 0.001 %s.", t)
	}
	bal := e.walletBalance(&e.userUtxos)
	if sats+txFee > bal {
		return userErr("Your wallet holds %s. Lock less, or get more test %s.", e.fmt(bal), t)
	}

	v := newVault(e.userPk, e.exchPk, e.cfg.ExitDelay, e.cfg.PenaltyDelay)
	tx := wire.NewMsgTx(2)
	prevs := map[wire.OutPoint]*wire.TxOut{}
	var in int64
	for _, u := range e.userUtxos {
		if in >= sats+txFee {
			break
		}
		op := u.op
		tx.AddTxIn(wire.NewTxIn(&op, nil, nil))
		prevs[op] = wire.NewTxOut(u.amt, e.userPk)
		in += u.amt
	}
	tx.AddTxOut(wire.NewTxOut(sats, v.V.pkScript))
	if change := in - sats - txFee; change > 546 {
		tx.AddTxOut(wire.NewTxOut(change, e.userPk))
	}
	hashes := txscript.NewTxSigHashes(tx, txscript.NewMultiPrevOutFetcher(prevs))
	for i, txin := range tx.TxIn {
		p := prevs[txin.PreviousOutPoint]
		wit, err := txscript.TaprootWitnessSignature(tx, hashes, i, p.Value, p.PkScript, txscript.SigHashDefault, e.userWallet)
		if err != nil {
			return err
		}
		tx.TxIn[i].Witness = wit
	}

	prevVault := e.vault
	e.vault = v
	v.vaultOut = wire.OutPoint{Hash: tx.TxHash(), Index: 0}
	v.vaultAmt = sats
	if err := e.broadcast(tx, "Lock "+e.fmt(sats)+" into the vault", "Your wallet key (ordinary key path)", nil,
		"Locked "+e.fmt(sats)+" into a new vault", "Vault deposit rejected"); err != nil {
		e.vault = prevVault
		return nil
	}
	v.fundTxid = tx.TxHash().String()
	v.presignExit()
	e.log("sign", "You and the exchange pre-signed the exit request",
		fmt.Sprintf("Either side can broadcast it later to start the %d-block clock.", v.delay), "")
	v.signState("Opening balance", sats, 0, e.height())
	e.log("sign", "You signed state #0: you own the whole vault", "", "")
	return nil
}

func (e *UTXOEngine) vaultStatus() string {
	v := e.vault
	if v == nil {
		return "none"
	}
	if v.closeTxid != "" {
		if e.confs(v.closeTxid) > 0 {
			return "closed"
		}
		return "closing"
	}
	if e.confs(v.fundTxid) == 0 {
		return "funding"
	}
	if v.exitSent {
		exitID := v.exitTx.TxHash().String()
		if e.confs(exitID) == 0 {
			return "exit_pending"
		}
		if e.blocksLeft() == 0 {
			return "ready"
		}
		return "exit_window"
	}
	return "open"
}

func (e *UTXOEngine) blocksLeft() int64 {
	v := e.vault
	exitID := v.exitTx.TxHash().String()
	h, ok := e.confirmedAt[exitID]
	if !ok {
		return v.delay
	}
	left := h + v.delay - (e.height() + 1)
	if left < 0 {
		left = 0
	}
	return left
}

func (e *UTXOEngine) order(side string, sats int64) error {
	v := e.vault
	t := e.cfg.Info.Ticker
	switch st := e.vaultStatus(); st {
	case "open":
	case "funding":
		return userErr("Your deposit hasn't confirmed yet. Mine a block first.")
	case "none", "closed", "closing":
		return userErr("Lock %s into a vault before trading.", t)
	default:
		return userErr("Trading is frozen while an exit request is in progress.")
	}
	if e.mode == "offline" {
		return userErr("The exchange is offline and isn't accepting orders.")
	}
	if sats <= 0 {
		return userErr("Enter an amount above zero.")
	}
	l := v.Latest()
	cost := orderCost(sats, e.price)
	var user, exch int64
	var label string
	switch side {
	case "sell":
		if sats > l.UserSats {
			return userErr("You only have %s in the vault to sell.", e.fmt(l.UserSats))
		}
		user, exch = l.UserSats-sats, l.ExchSats+sats
		label = fmt.Sprintf("Sold %s at %s", e.fmt(sats), fmtUSD(e.price*100))
	case "buy":
		if sats > l.ExchSats {
			return userErr("You can only buy back %s sold from this vault (%s). The vault can't hold more than you locked.", t, e.fmt(l.ExchSats))
		}
		if cost > e.cashCents {
			return userErr("That costs %s and you have %s in cash.", fmtUSD(cost), fmtUSD(e.cashCents))
		}
		user, exch = l.UserSats+sats, l.ExchSats-sats
		label = fmt.Sprintf("Bought %s at %s", e.fmt(sats), fmtUSD(e.price*100))
	default:
		return userErr("Unknown order side.")
	}
	s := v.signState(label, user, exch, e.height())
	e.log("sign", fmt.Sprintf("You signed state #%d before sending the order", s.Index),
		fmt.Sprintf("Yours: %s, owed to buyers: %s. The exchange now holds your signature, so this trade can't be undone. It sent you the hash of revocation secret #%d.", e.fmt(user), e.fmt(exch), s.Index), "")
	e.log("sign", fmt.Sprintf("The exchange handed you revocation secret #%d", s.Index-1),
		fmt.Sprintf("State #%d is now revoked. If the exchange ever publishes it, your watchtower can use this secret to take the exchange's share of it.", s.Index-1), "")
	if side == "sell" {
		e.cashCents += cost
	} else {
		e.cashCents -= cost
	}
	e.log("ok", label, fmt.Sprintf("Filled instantly on the order book. Cash balance: %s.", fmtUSD(e.cashCents)), "")
	return nil
}

func (e *UTXOEngine) settle() error {
	v := e.vault
	if st := e.vaultStatus(); st != "open" {
		if st == "funding" {
			return userErr("Your deposit hasn't confirmed yet. Mine a block first.")
		}
		return userErr("There's no open vault to settle.")
	}
	switch e.mode {
	case "offline":
		e.log("warn", "Settlement request got no answer", "The exchange is offline. Use Request exit to get your coins back without it.", "")
		return nil
	case "malicious":
		e.log("warn", "The exchange refused to co-sign your settlement", "Your only route now is Request exit, which starts the exit clock.", "")
		return nil
	}
	tx := v.cooperativeSettle()
	if err := e.broadcast(tx, "Cooperative settlement", "Vault script: you and the exchange both sign now",
		v.multisigLeaf.Script, "Settled: coins on their way back to your wallet", "Settlement rejected"); err == nil {
		v.closeTxid, v.closeKind = tx.TxHash().String(), "settled"
	}
	return nil
}

func (e *UTXOEngine) requestExit(by string) error {
	v := e.vault
	st := e.vaultStatus()
	if st == "funding" {
		return userErr("Your deposit hasn't confirmed yet. Mine a block first.")
	}
	if st != "open" {
		return userErr("There's no open vault to exit.")
	}
	who := "You"
	if by == "exchange" {
		who = "The exchange"
	}
	if err := e.broadcast(v.exitTx, "Exit request ("+strings.ToLower(who)+")",
		"Vault script: both signatures, pre-signed when the vault opened", v.multisigLeaf.Script,
		who+" broadcast the exit request", "Exit request rejected"); err == nil {
		v.exitSent = true
		e.log("info", fmt.Sprintf("The %d-block clock starts once this confirms", v.delay),
			"During the window the exchange can publish the latest state you signed. After it, you can claim alone.", "")
	}
	return nil
}

func (e *UTXOEngine) claim() error {
	v := e.vault
	if v == nil || !v.exitSent {
		return userErr("Request an exit first. Claiming alone only works on the exit output.")
	}
	if v.closeTxid != "" {
		return userErr("This vault is already closed.")
	}
	tx := v.userClaim()
	if err := e.broadcast(tx, "Claim alone after the timelock",
		fmt.Sprintf("Exit script: <%d> CHECKSEQUENCEVERIFY, then your signature only", v.delay), v.delayLeaf.Script,
		"You claimed the vault on your own", "Your solo claim was rejected"); err == nil {
		v.closeTxid, v.closeKind = tx.TxHash().String(), "claimed"
		if v.Latest().ExchSats > 0 {
			e.log("warn", "The exchange missed its window, so you kept "+e.cfg.Info.Ticker+" you had sold",
				"It never published your signed sale in time. This is why a real exchange must watch for exit requests around the clock.", "")
		}
	}
	return nil
}

func (e *UTXOEngine) steal() error {
	v := e.vault
	if v == nil {
		return userErr("Open a vault first so the exchange has something to try to take.")
	}
	var prev wire.OutPoint
	var amt int64
	var out *tapOutput
	if _, ok := e.unspent(v.vaultOut); ok {
		prev, amt, out = v.vaultOut, v.vaultAmt, v.V
	} else if v.exitSent {
		if _, ok := e.unspent(v.exitOut()); ok {
			prev, amt, out = v.exitOut(), v.exitAmt, v.E
		}
	}
	if out == nil {
		return userErr("There are no vault coins left to take.")
	}
	tx := v.exchangeOnlySpend(prev, amt, out)
	e.broadcast(tx, "Exchange tries to take the vault", "Vault script with the exchange's signature only",
		v.multisigLeaf.Script, "The exchange took the vault", "The exchange tried to take your coins alone")
	return nil
}

func (e *UTXOEngine) setMode(m string) error {
	switch m {
	case "honest":
		e.log("info", "Exchange is online and honest", "", "")
	case "offline":
		e.log("warn", "Exchange went offline", "It won't sign settlements or answer exit requests.", "")
	case "malicious":
		e.log("warn", "Exchange turned malicious", "It refuses to settle, and if an exit happens it publishes whichever signed state pays it most.", "")
	default:
		return userErr("Unknown exchange mode.")
	}
	e.mode = m
	e.watchtower()
	return nil
}

func (e *UTXOEngine) setPrice(usd int64) error {
	if usd < 1 || usd > 10_000_000 {
		return userErr("Pick a price between $1 and $10,000,000.")
	}
	e.price = usd
	return nil
}

// watchtower runs after every block and mode change: the exchange answers an
// exit, then the user's watchtower checks for a revoked state, then the
// exchange collects its share of a published state once D2 has passed.
func (e *UTXOEngine) watchtower() {
	e.exchangeWatch()
	e.userWatch()
	e.exchangeCollect()
}

// exchangeWatch: the exchange reacts to a confirmed exit request.
func (e *UTXOEngine) exchangeWatch() {
	v := e.vault
	if v == nil || !v.exitSent || v.closeTxid != "" || e.mode == "offline" {
		return
	}
	if e.confs(v.exitTx.TxHash().String()) == 0 {
		return
	}
	pick := v.Latest()
	if e.mode == "malicious" {
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
	if err := e.broadcast(tx, fmt.Sprintf("Exchange publishes state #%d", pick.Index),
		"Exit script: your signature from order placement plus the exchange's", v.multisigLeaf.Script,
		title, "Exchange's state publication rejected"); err == nil {
		v.closeTxid, v.closeKind = tx.TxHash().String(), "published"
		v.published = pick
		if pick.revealed {
			e.log("warn", fmt.Sprintf("You hold revocation secret #%d for that state", pick.Index),
				fmt.Sprintf("The node accepts the old state because you signed it, but the exchange's %s share is locked for %d blocks. With the secret, your watchtower can take it first.", e.fmt(pick.rAmt), v.penaltyDelay), "")
		}
	}
}

// userWatch: once a revoked state is confirmed on the chain, the user's
// watchtower sweeps the exchange's share with the revocation secret.
func (e *UTXOEngine) userWatch() {
	v := e.vault
	if v == nil || v.published == nil || v.sweepTxid != "" || !e.userWT {
		return
	}
	s := v.published
	if !s.revealed || s.rIndex < 0 || e.confs(v.closeTxid) == 0 {
		return
	}
	if _, ok := e.unspent(s.stateOut()); !ok {
		return
	}
	e.log("warn", fmt.Sprintf("Your watchtower spotted revoked state #%d on the chain", s.Index),
		fmt.Sprintf("It's using revocation secret #%d to take the exchange's %s share before the exchange's %d-block wait ends.", s.Index, e.fmt(s.rAmt), v.penaltyDelay), "")
	tx := v.penaltySweep(s, s.secret)
	if err := e.broadcast(tx, fmt.Sprintf("Penalty sweep of state #%d", s.Index),
		fmt.Sprintf("Revocation path: SHA256(secret #%d) matches, plus your signature", s.Index), s.revLeaf.Script,
		fmt.Sprintf("Penalty sweep: your watchtower took the exchange's %s share of old state #%d", e.fmt(s.rAmt), s.Index),
		"Penalty sweep rejected"); err == nil {
		v.sweepTxid, v.sweepKind = tx.TxHash().String(), "penalty"
	}
}

// exchangeCollect: the exchange takes its share of a published state after
// D2 blocks, if nobody swept it with the revocation secret first.
func (e *UTXOEngine) exchangeCollect() {
	v := e.vault
	if v == nil || v.published == nil || v.sweepTxid != "" || e.mode == "offline" {
		return
	}
	s := v.published
	if s.rIndex < 0 || e.confs(v.closeTxid) < v.penaltyDelay {
		return
	}
	if _, ok := e.unspent(s.stateOut()); !ok {
		return
	}
	tx := v.exchangeClaim(s)
	if err := e.broadcast(tx, fmt.Sprintf("Exchange collects its share of state #%d", s.Index),
		fmt.Sprintf("Exchange path: <%d> CHECKSEQUENCEVERIFY, then the exchange's signature", v.penaltyDelay), s.exchLeaf.Script,
		fmt.Sprintf("The exchange claimed its %s share of state #%d after %d blocks", e.fmt(s.rAmt), s.Index, v.penaltyDelay),
		"Exchange's claim rejected"); err == nil {
		v.sweepTxid, v.sweepKind = tx.TxHash().String(), "exchange"
		if s.revealed {
			e.log("warn", "Nobody used your revocation secret in time",
				"Your watchtower was off, so the exchange kept its share of the old state. This is why someone must watch the chain for you during an exit.", "")
		}
	}
}

// ------------------------------------------------------------------ snapshot

func (e *UTXOEngine) Snapshot() Snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := Snapshot{Chain: e.cfg.Info, Available: e.available, Unavailable: e.unavailable,
		Ready: e.ready, Progress: e.progress, InitErr: e.initErr,
		Price: e.price, Mode: e.mode, CashCents: e.cashCents, Events: e.events,
		Watchtower: e.userWT, HasUserWT: true}
	if !e.ready {
		return s
	}
	s.Height = e.height()
	if mp, err := e.rpc.GetRawMempool(); err == nil {
		s.Mempool = len(mp)
	}
	keep := e.userUtxos[:0]
	for _, u := range e.userUtxos {
		r, err := e.rpc.GetTxOut(&u.op.Hash, u.op.Index, true)
		if err != nil || r == nil {
			continue
		}
		keep = append(keep, u)
		if r.Confirmations > 0 {
			s.WalletSats += u.amt
		} else {
			s.WalletPend += u.amt
		}
	}
	e.userUtxos = keep
	s.ExchSats = e.walletBalance(&e.exchUtxos)

	for _, t := range e.txs {
		if t.Status == "mempool" {
			e.confs(t.Txid)
		}
		s.Txs = append(s.Txs, t)
	}
	sort.Slice(s.Txs, func(i, j int) bool { return s.Txs[i].Seq > s.Txs[j].Seq })

	if v := e.vault; v != nil {
		vv := &VaultView{
			Status: e.vaultStatus(), Locked: v.vaultAmt, ExitSent: v.exitSent, BestState: -1,
			CloseTxid: v.closeTxid, CloseKind: v.closeKind, ClockTotal: v.delay,
			Revocation: true, PenaltyDelay: v.penaltyDelay, PublishedState: -1,
			SweepKind: v.sweepKind, SweepTxid: v.sweepTxid,
			Scripts: []Labelled{
				{"Both must sign (vault and exit output)", v.friendlyScript(v.multisigLeaf.Script)},
				{fmt.Sprintf("You alone, %d blocks after the exit request", v.delay), v.friendlyScript(v.delayLeaf.Script)},
			},
		}
		if v.published != nil {
			vv.PublishedState = v.published.Index
		}
		if l := v.Latest(); l.R != nil {
			vv.Scripts = append(vv.Scripts,
				Labelled{fmt.Sprintf("Exchange's share of a published state: the exchange alone, %d blocks later", v.penaltyDelay), v.friendlyScript(l.exchLeaf.Script)},
				Labelled{fmt.Sprintf("Exchange's share of state #%d: you, with revocation secret #%d (only once that state is replaced)", l.Index, l.Index), v.friendlyScript(l.revLeaf.Script)})
		}
		p := e.cfg.Params
		if addr, err := btcutil.NewAddressTaproot(v.V.pkScript[2:], &p); err == nil {
			vv.Address = addr.EncodeAddress()
		}
		l := v.Latest()
		vv.UserSats, vv.ExchSats = l.UserSats, l.ExchSats
		for _, st := range v.States {
			sv := StateView{Index: st.Index, Label: st.Label, UserSats: st.UserSats, ExchSats: st.ExchSats, Height: st.Height,
				RevHash: hex.EncodeToString(st.revHash)}
			if st.revealed {
				sv.Secret = hex.EncodeToString(st.secret)
			}
			vv.States = append(vv.States, sv)
		}
		if v.exitSent && e.confs(v.exitTx.TxHash().String()) > 0 {
			vv.ClockStarted = true
			left := e.blocksLeft()
			vv.ClockDone = v.delay - left
			vv.ClockReady = left == 0
		}
		s.Vault = vv
	}
	return s
}

// xonlyFor is used by tests.
var _ = schnorr.SerializePubKey
