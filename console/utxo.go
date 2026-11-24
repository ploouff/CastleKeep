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
	CheckForks   bool   // btcd reports soft-fork status; ltcd can't
	DataDir      string // keep the chain here between runs ("" = a temporary folder, removed on Stop)
	RPCPort      int    // fixed RPC port (0 = any free port)
	Network      string // "" for a private regtest chain, or "signet" (Bitcoin's public test network)
	AddrIndex    bool   // run the node with an address index (always on for signet)
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

	node       *exec.Cmd
	rpcAddr    string        // the node's RPC address, for the separate exchange and wallet programs
	lastFaucet wire.OutPoint // the output the last faucet payment created
	dir        string
	rpc        *rpcclient.Client

	miner   *btcec.PrivateKey
	minerPk []byte
	nextCB  int64
	kept    int64 // kept chains: blocks already in blocks.txt

	userWallet, exchWallet *btcec.PrivateKey
	userPk, exchPk         []byte
	userUtxos, exchUtxos   []utxo

	tradeBook
	userWT   bool  // the user's watchtower: sweeps revoked states with their secret
	exchBase int64 // the exchange's starting stock; the UI shows the change from it
	vault    *Vault
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
		tradeBook:   tradeBook{acct: NewAccount(), price: cfg.DefaultPrice, mode: "honest"},
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
	var err error
	if e.cfg.DataDir != "" {
		e.dir = e.cfg.DataDir
		if err := e.loadMiner(); err != nil {
			return err
		}
	} else if e.dir, err = os.MkdirTemp("", "castlekeep-"+e.cfg.Info.ID+"-"); err != nil {
		return err
	}
	addr, err := btcutil.NewAddressTaproot(e.minerPk[2:], &e.cfg.Params)
	if err != nil {
		return err
	}
	port := e.cfg.RPCPort
	if port == 0 {
		port = freePort()
	}
	rpcAddr := fmt.Sprintf("127.0.0.1:%d", port)
	e.rpcAddr = rpcAddr
	args := []string{"--regtest", "--notls", "--txindex",
		"--datadir=" + filepath.Join(e.dir, "data"), "--logdir=" + filepath.Join(e.dir, "logs"),
		"--rpcuser=castlekeep", "--rpcpass=castlekeep",
		"--rpclisten=" + rpcAddr, fmt.Sprintf("--listen=127.0.0.1:%d", freePort()),
		"--miningaddr=" + addr.EncodeAddress()}
	if e.signet() {
		// The public signet: no mining here; the address index finds coins
		// that people send to the exchange and the wallets.
		args = append([]string{"--signet", "--addrindex"}, args[1:len(args)-1]...)
	} else if e.cfg.AddrIndex {
		args = append(args, "--addrindex")
	}
	e.node = exec.Command(e.cfg.NodePath, args...)
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

	if e.signet() {
		return e.waitSynced()
	}
	if e.cfg.DataDir != "" {
		if err := e.replayBlocks(); err != nil {
			return err
		}
	}
	if h, _ := e.rpc.GetBlockCount(); e.cfg.DataDir != "" && h >= 500 {
		// A kept chain from an earlier run: carry on from where it was.
		e.mu.Lock()
		defer e.mu.Unlock()
		e.log("info", "Test chain resumed", fmt.Sprintf("Kept from the last run, at block %d.", h), "")
		e.progress, e.ready = "Ready", true
		return nil
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
	// The exchange's own stock, which it can lock in vaults as buying capacity.
	if err := e.faucetTo(e.exchPk, exchangeStock, fmt.Sprintf("Test faucet: %d %s to the exchange's wallet", exchangeStock/unit, e.cfg.Info.Ticker),
		"The exchange received its test "+e.cfg.Info.Ticker); err != nil {
		return err
	}
	e.exchBase = exchangeStock
	if _, err := e.rpc.Generate(1); err != nil {
		return err
	}
	if err := e.keepBlocks(); err != nil {
		return err
	}
	e.log("info", "Your wallet has 2 test "+e.cfg.Info.Ticker, "Lock some into a vault to start trading.", "")
	e.progress = "Ready"
	e.ready = true
	return nil
}

func (e *UTXOEngine) Stop() {
	if e.node != nil && e.node.Process != nil {
		done := make(chan struct{})
		go func() { e.node.Wait(); close(done) }()
		if e.cfg.DataDir != "" && e.rpc != nil {
			// A kept chain: ask the node to shut down cleanly first.
			e.rpc.RawRequest("stop", nil)
			select {
			case <-done:
			case <-time.After(20 * time.Second):
			}
		}
		e.node.Process.Kill()
		<-done
		e.node = nil // stopping twice is harmless
	}
	if e.dir != "" && e.cfg.DataDir == "" {
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
			return fmt.Sprintf("Exit output (a state both signed, or the pay-back of deposits after %d blocks)", v.delay)
		}
		for _, s := range v.States {
			if string(pk) == string(s.R.pkScript) {
				return fmt.Sprintf("Exchange's share of state #%d (exchange after %d blocks, or you with revocation secret #%d)", s.Index, v.penaltyDelay, s.Index)
			}
			if s.U != nil && string(pk) == string(s.U.pkScript) {
				return fmt.Sprintf("Your share of state #%d (you after %d blocks, or the exchange with your revocation secret #%d)", s.Index, v.penaltyDelay, s.Index)
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
	if err := e.tradeReady(); err != nil {
		return err
	}
	switch r.Action {
	case "cash":
		e.addTestCash(e.log)
		return nil
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
	case "capacity":
		return e.addCapacity(r.Sats)
	case "cheat":
		return e.userPublishOld()
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
	return e.faucetTo(e.userPk, unit, "Test faucet: 1 "+e.cfg.Info.Ticker+" to your wallet", "Received 1 test "+e.cfg.Info.Ticker)
}

// faucetTo pays amt from the next mature test coinbase to a wallet script.
func (e *UTXOEngine) faucetTo(to []byte, amt int64, label, okTitle string) error {
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
	e.saveMiner()
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
	cbAmt, _ := btcutil.NewAmount(txs[0].Vout[0].Value)
	prevAmt := int64(cbAmt)
	prev := wire.OutPoint{Hash: *cbHash, Index: 0}
	e.outLabels[prev] = IO{Label: "Test faucet (mined coins)", Sats: prevAmt, Ref: prev.String()}
	if prevAmt < amt+2*txFee {
		return userErr("The next test coinbase is too small. Try again.")
	}

	tx := wire.NewMsgTx(2)
	tx.AddTxIn(wire.NewTxIn(&prev, nil, nil))
	tx.AddTxOut(wire.NewTxOut(amt, to))
	tx.AddTxOut(wire.NewTxOut(prevAmt-amt-txFee, e.minerPk))
	hashes := txscript.NewTxSigHashes(tx, txscript.NewCannedPrevOutputFetcher(e.minerPk, prevAmt))
	wit, err := txscript.TaprootWitnessSignature(tx, hashes, 0, prevAmt, e.minerPk, txscript.SigHashDefault, e.miner)
	if err != nil {
		return err
	}
	tx.TxIn[0].Witness = wit
	if err := e.broadcast(tx, label, "Faucet key (ordinary key path)", nil, okTitle, "Faucet payment rejected"); err != nil {
		return err
	}
	e.lastFaucet = wire.OutPoint{Hash: tx.TxHash(), Index: 0}
	return nil
}

func (e *UTXOEngine) mine(n int) error {
	if e.signet() {
		return userErr("On signet, blocks come by themselves about every 10 minutes. Nothing can be mined here.")
	}
	if n < 1 || n > 2000 {
		return userErr("Mine between 1 and 2,000 blocks at a time.")
	}
	if _, err := e.rpc.Generate(uint32(n)); err != nil {
		return err
	}
	if err := e.keepBlocks(); err != nil {
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
	v.userDeposit = sats
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

// exchangeStock is how many test coins the exchange's wallet starts with.
const exchangeStock = 20 * unit

// addCapacity: the exchange adds its own coins to the vault (a splice) so
// the user can buy more than they sold. The fee comes from the user's share.
func (e *UTXOEngine) addCapacity(sats int64) error {
	v := e.vault
	t := e.cfg.Info.Ticker
	switch st := e.vaultStatus(); st {
	case "open":
	case "funding":
		return userErr("The last deposit into this vault hasn't confirmed yet. Mine a block first.")
	default:
		return userErr("Open a vault first, then ask for buying capacity.")
	}
	if e.mode == "offline" {
		return userErr("The exchange is offline and can't lock anything.")
	}
	if sats < 100_000 || sats > 5*unit {
		return userErr("Ask for between 0.001 and 5 %s.", t)
	}
	fee := capacityFee(sats)
	l := v.Latest()
	if fee > l.UserSats {
		return userErr("The fee (%s) is more than your share of the vault.", e.fmt(fee))
	}
	var ins []utxo
	var in int64
	for _, u := range e.exchUtxos {
		if in >= sats+txFee {
			break
		}
		if _, ok := e.unspent(u.op); !ok {
			continue
		}
		ins = append(ins, u)
		in += u.amt
	}
	if in < sats+txFee {
		return userErr("The exchange's wallet only holds %s right now.", e.fmt(in))
	}
	tx, err := v.spliceTx(ins, e.exchPk, e.exchWallet, sats)
	if err != nil {
		return err
	}
	for _, u := range ins {
		e.outLabels[u.op] = IO{Label: "Exchange wallet", Sats: u.amt, Ref: u.op.String()}
	}
	e.outLabels[v.vaultOut] = IO{Label: "Vault (you + exchange)", Sats: v.vaultAmt, Ref: v.vaultOut.String()}
	if err := e.broadcast(tx, fmt.Sprintf("Exchange adds %s to your vault", e.fmt(sats)),
		"Key path: one MuSig2 signature made by both of you together (no script shown on the chain), plus the exchange's wallet key for its coins", nil,
		fmt.Sprintf("The exchange is adding %s to your vault", e.fmt(sats)), "The exchange's deposit was rejected"); err != nil {
		return nil
	}
	v.applySplice(tx, sats)
	v.fundTxid = tx.TxHash().String()
	e.log("sign", "You and the exchange signed a new exit request and pay-back",
		fmt.Sprintf("The old vault output is spent, so the old exit request and every older state stop working. After an exit window, the pay-back now returns %s to you and %s to the exchange.", e.fmt(v.userDeposit), e.fmt(v.exchDeposit)), "")
	s := v.signState(fmt.Sprintf("Exchange locked %s (fee %s)", e.fmt(sats), e.fmt(fee)), l.UserSats-fee, l.ExchSats+sats+fee, e.height())
	e.log("sign", fmt.Sprintf("You both signed state #%d: the exchange's %s is in the vault", s.Index, e.fmt(sats)),
		fmt.Sprintf("Mine a block to confirm it. You can then buy up to %s from the exchange. The %s fee (0.2%%) came out of your share.", e.fmt(s.ExchSats), e.fmt(fee)), "")
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

func (e *UTXOEngine) lockTrade()   { e.mu.Lock() }
func (e *UTXOEngine) unlockTrade() { e.mu.Unlock() }

func (e *UTXOEngine) tradeReady() error {
	if !e.ready {
		return userErr("The %s test chain is still starting up.", e.cfg.Info.Name)
	}
	return nil
}

// canTrade checks that an order of this size can go into the vault now. It
// doesn't look at cash: orders pay with cash, swaps pay with another coin.
func (e *UTXOEngine) canTrade(side string, sats int64) error {
	v := e.vault
	t := e.cfg.Info.Ticker
	switch st := e.vaultStatus(); st {
	case "open":
	case "funding":
		return userErr("Your %s deposit hasn't confirmed yet. Mine a block first.", t)
	case "none", "closed", "closing":
		return userErr("Lock %s into a vault before trading.", t)
	default:
		return userErr("%s trading is frozen while an exit request is in progress.", t)
	}
	if e.mode == "offline" {
		return userErr("The exchange is offline and isn't accepting %s orders.", t)
	}
	if sats <= 0 {
		return userErr("Enter an amount above zero.")
	}
	l := v.Latest()
	switch side {
	case "sell":
		if sats > l.UserSats {
			return userErr("You only have %s in the vault to sell.", e.fmt(l.UserSats))
		}
	case "buy":
		if sats > l.ExchSats {
			if v.exchDeposit == 0 {
				return userErr("You can only buy back %s sold from this vault (%s). Ask the exchange to lock buying capacity to buy more.", t, e.fmt(l.ExchSats))
			}
			return userErr("The exchange's side of the vault holds %s, so that's the most you can buy. Ask for more buying capacity to buy more.", e.fmt(l.ExchSats))
		}
	default:
		return userErr("Unknown order side.")
	}
	return nil
}

// recordTrade signs the new state for an order canTrade accepted.
func (e *UTXOEngine) recordTrade(side string, sats int64, label string) {
	v := e.vault
	l := v.Latest()
	user, exch := l.UserSats-sats, l.ExchSats+sats
	if side == "buy" {
		user, exch = l.UserSats+sats, l.ExchSats-sats
	}
	s := v.signState(label, user, exch, e.height())
	e.log("sign", fmt.Sprintf("You signed state #%d before sending the order", s.Index),
		fmt.Sprintf("Yours: %s, owed to buyers: %s. The exchange now holds your signature, so this trade can't be undone. It sent you the hash of revocation secret #%d.", e.fmt(user), e.fmt(exch), s.Index), "")
	e.log("sign", fmt.Sprintf("The exchange handed you revocation secret #%d", s.Index-1),
		fmt.Sprintf("State #%d is now revoked. If the exchange ever publishes it, your watchtower can use this secret to take the exchange's share of it.", s.Index-1), "")
}

func (e *UTXOEngine) order(side string, sats int64) error {
	if err := e.canTrade(side, sats); err != nil {
		return err
	}
	cost := orderCost(sats, e.price)
	var cash int64
	label := fmt.Sprintf("Sold %s at %s", e.fmt(sats), fmtUSD(e.price*100))
	if side == "buy" {
		var err error
		if cash, err = e.acct.spend(cost); err != nil {
			return err
		}
		label = fmt.Sprintf("Bought %s at %s", e.fmt(sats), fmtUSD(e.price*100))
	} else {
		cash = e.acct.add(cost)
	}
	e.recordTrade(side, sats, label)
	e.log("ok", label, fmt.Sprintf("Filled instantly on the order book. Cash balance: %s.", fmtUSD(cash)), "")
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
	if err := e.broadcast(tx, "Cooperative settlement", "Key path: one MuSig2 signature made by you and the exchange together. On the chain it looks like an ordinary single-key payment: no script, no second key",
		nil, "Settled: coins on their way back to your wallet", "Settlement rejected"); err == nil {
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
	if err := e.broadcast(tx, "Claim alone after the timelock: deposits paid back",
		fmt.Sprintf("Exit script: <%d> CHECKSEQUENCEVERIFY, then both signatures, which the exchange gave when the vault opened", v.delay), v.delayLeaf.Script,
		"You claimed the vault on your own", "Your solo claim was rejected"); err == nil {
		v.closeTxid, v.closeKind = tx.TxHash().String(), "claimed"
		l := v.Latest()
		switch {
		case l.ExchSats > v.exchDeposit:
			e.log("warn", "The exchange missed its window, so you kept "+e.cfg.Info.Ticker+" you had sold",
				"It never published your signed sale in time, so each side got its deposit back. This is why a real exchange must watch for exit requests around the clock.", "")
		case l.UserSats > v.userDeposit:
			e.log("warn", "Nobody published your latest state, so the "+e.cfg.Info.Ticker+" you bought went back to the exchange",
				"After the window only deposits are paid back. Your watchtower publishes your latest state during the window to prevent this.", "")
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

// watchtower runs after every block and mode change. In order: the exchange
// answers an exit, the exchange's watchtower punishes a replaced state the
// user published, the user's watchtower punishes a replaced state the
// exchange published (or publishes the latest state when the pay-back of
// deposits would cost the user coins they bought), and each side collects
// its own delayed share once D2 has passed.
func (e *UTXOEngine) watchtower() {
	e.exchangeWatch()
	e.exchangePenalty()
	e.userWatch()
	e.exchangeCollect()
	e.userCollectOwn()
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
			if v.current(s) && s.ExchSats > pick.ExchSats {
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
		v.published, v.publishedBy = pick, "exchange"
		if pick.revealed {
			e.log("warn", fmt.Sprintf("You hold revocation secret #%d for that state", pick.Index),
				fmt.Sprintf("The node accepts the old state because you signed it, but the exchange's %s share is locked for %d blocks. With the secret, your watchtower can take it first.", e.fmt(pick.rAmt), v.penaltyDelay), "")
		}
	}
}

// userPublishOld is the "Publish an older state yourself" button: the user
// tries the same trick, publishing their copy of whichever state paid them
// most. The exchange's watchtower holds the user's secret for every replaced
// state.
func (e *UTXOEngine) userPublishOld() error {
	v := e.vault
	if v == nil || !v.exitSent {
		return userErr("Request an exit first. A state can only be published from the exit output.")
	}
	if v.closeTxid != "" {
		return userErr("This vault is already closed.")
	}
	pick := v.Latest()
	for _, s := range v.States {
		if v.current(s) && s.UserSats > pick.UserSats {
			pick = s
		}
	}
	if pick == v.Latest() {
		return userErr("None of your older states pays you more than the latest one, so there's nothing to gain.")
	}
	tx := v.userPublish(pick)
	if err := e.broadcast(tx, fmt.Sprintf("You publish old state #%d", pick.Index),
		"Exit script: the exchange's signature from the fill plus yours", v.multisigLeaf.Script,
		fmt.Sprintf("You published an OLD state (#%d, not #%d)", pick.Index, v.Latest().Index),
		"Your state publication was rejected"); err == nil {
		v.closeTxid, v.closeKind = tx.TxHash().String(), "published"
		v.published, v.publishedBy = pick, "user"
		e.log("warn", fmt.Sprintf("The exchange holds your revocation secret #%d", pick.Index),
			fmt.Sprintf("The node accepts the old state because the exchange signed it, but your %s share is locked for %d blocks. With your secret, the exchange's watchtower can take it first.", e.fmt(pick.uAmt), v.penaltyDelay), "")
	}
	return nil
}

// exchangePenalty: once a replaced state the user published is confirmed,
// the exchange's watchtower takes the user's share with the user's secret.
func (e *UTXOEngine) exchangePenalty() {
	v := e.vault
	if v == nil || v.publishedBy != "user" || v.sweepTxid != "" || e.mode == "offline" {
		return
	}
	s := v.published
	if !s.revealed || s.uIndex < 0 || e.confs(v.closeTxid) == 0 {
		return
	}
	if _, ok := e.unspent(s.userOut()); !ok {
		return
	}
	tx := v.exchPenaltySweep(s, s.uSecret)
	if err := e.broadcast(tx, fmt.Sprintf("Exchange's penalty sweep of state #%d", s.Index),
		fmt.Sprintf("Revocation path: SHA256(your secret #%d) matches, plus the exchange's signature", s.Index), s.uRevLeaf.Script,
		fmt.Sprintf("Penalty sweep: the exchange's watchtower took your %s share of old state #%d", e.fmt(s.uAmt), s.Index),
		"Exchange's penalty sweep rejected"); err == nil {
		v.sweepTxid, v.sweepKind = tx.TxHash().String(), "exchange-penalty"
	}
}

// userWatch is the user's watchtower. If the exchange published a replaced
// state, it sweeps the exchange's share with the revocation secret. If the
// exit is confirmed, nobody has published, and the latest state pays the
// user more than their deposit, it publishes the latest state, because the
// pay-back after the window would only return deposits.
func (e *UTXOEngine) userWatch() {
	v := e.vault
	if v == nil || !e.userWT || !v.exitSent {
		return
	}
	if v.closeTxid == "" {
		l := v.Latest()
		if l.UserSats <= v.userDeposit || e.confs(v.exitTx.TxHash().String()) == 0 {
			return
		}
		e.log("info", "Your watchtower is publishing your latest state",
			fmt.Sprintf("Without it, the pay-back after the window would only return deposits and you'd lose the %s you bought.", e.fmt(l.UserSats-v.userDeposit)), "")
		tx := v.userPublish(l)
		if err := e.broadcast(tx, fmt.Sprintf("You publish state #%d", l.Index),
			"Exit script: the exchange's signature from the fill plus yours", v.multisigLeaf.Script,
			fmt.Sprintf("Your watchtower published your latest state #%d", l.Index), "Your state publication was rejected"); err == nil {
			v.closeTxid, v.closeKind = tx.TxHash().String(), "published"
			v.published, v.publishedBy = l, "user"
		}
		return
	}
	if v.publishedBy != "exchange" || v.sweepTxid != "" {
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

// exchangeCollect: the exchange takes its share of a state it published
// after D2 blocks, if nobody swept it with the revocation secret first.
func (e *UTXOEngine) exchangeCollect() {
	v := e.vault
	if v == nil || v.publishedBy != "exchange" || v.sweepTxid != "" || e.mode == "offline" {
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

// userCollectOwn: the user's wallet takes the user's share of a state the
// user published, once D2 blocks have passed and nobody swept it.
func (e *UTXOEngine) userCollectOwn() {
	v := e.vault
	if v == nil || v.publishedBy != "user" || v.sweepTxid != "" {
		return
	}
	s := v.published
	if s.uIndex < 0 || e.confs(v.closeTxid) < v.penaltyDelay {
		return
	}
	if _, ok := e.unspent(s.userOut()); !ok {
		return
	}
	tx := v.userCollect(s)
	if err := e.broadcast(tx, fmt.Sprintf("You collect your share of state #%d", s.Index),
		fmt.Sprintf("Your path: <%d> CHECKSEQUENCEVERIFY, then your signature", v.penaltyDelay), s.userLeaf.Script,
		fmt.Sprintf("You collected your %s share of state #%d after %d blocks", e.fmt(s.uAmt), s.Index, v.penaltyDelay),
		"Your claim was rejected"); err == nil {
		v.sweepTxid, v.sweepKind = tx.TxHash().String(), "user"
		if s.revealed {
			e.log("warn", "The exchange didn't use your revocation secret in time",
				"Its watchtower wasn't running, so the old state stood. The exchange has to watch the chain too.", "")
		}
	}
}

// ------------------------------------------------------------------ snapshot

func (e *UTXOEngine) Snapshot() Snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := Snapshot{Chain: e.cfg.Info, Available: e.available, Unavailable: e.unavailable,
		Ready: e.ready, Progress: e.progress, InitErr: e.initErr,
		Price: e.price, Mode: e.mode, CashCents: e.acct.Cash(), Events: e.events,
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
	s.ExchSats = e.walletBalance(&e.exchUtxos) - e.exchBase

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
			PublishedBy: v.publishedBy, UserDeposit: v.userDeposit, ExchDeposit: v.exchDeposit,
			FundPending: v.gen > 0 && e.confs(v.fundTxid) == 0,
			Scripts: append(keyPathNote(v.V), []Labelled{
				{"Both must sign (vault and exit output)", v.friendlyScript(v.multisigLeaf.Script)},
				{fmt.Sprintf("Pay-back of deposits, %d blocks after the exit request (both signed it when the vault opened, so you can use it alone)", v.delay), v.friendlyScript(v.delayLeaf.Script)},
			}...),
		}
		if v.published != nil {
			vv.PublishedState = v.published.Index
		}
		if l := v.Latest(); l.R != nil {
			vv.Scripts = append(vv.Scripts,
				Labelled{fmt.Sprintf("Exchange's share of a published state: the exchange alone, %d blocks later", v.penaltyDelay), v.friendlyScript(l.exchLeaf.Script)},
				Labelled{fmt.Sprintf("Exchange's share of state #%d: you, with revocation secret #%d (only once that state is replaced)", l.Index, l.Index), v.friendlyScript(l.revLeaf.Script)},
				Labelled{fmt.Sprintf("Your share of a state you publish: you alone, %d blocks later", v.penaltyDelay), v.friendlyScript(l.userLeaf.Script)},
				Labelled{fmt.Sprintf("Your share of state #%d: the exchange, with your secret #%d (only once that state is replaced)", l.Index, l.Index), v.friendlyScript(l.uRevLeaf.Script)})
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

// RPCAddr is the node's RPC address (user and password "castlekeep").
func (e *UTXOEngine) RPCAddr() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.rpcAddr
}

// Faucet pays amt test coins to a wallet script from the next mature
// coinbase and returns the new output. Used by the separate wallet and
// exchange programs, which have no mined coins of their own.
func (e *UTXOEngine) Faucet(script []byte, amt int64) (wire.OutPoint, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.signet() {
		return wire.OutPoint{}, userErr("Signet has no built-in faucet: get free signet coins from a public signet faucet and send them to the address shown.")
	}
	if !e.ready {
		return wire.OutPoint{}, userErr("The %s test chain is still starting up.", e.cfg.Info.Name)
	}
	if err := e.faucetTo(script, amt, fmt.Sprintf("Test faucet: %s to another program", e.fmt(amt)), "Paid test "+e.cfg.Info.Ticker); err != nil {
		return wire.OutPoint{}, err
	}
	return e.lastFaucet, nil
}

// Mine mines n blocks (and runs this engine's own watchtowers).
func (e *UTXOEngine) Mine(n int) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.mine(n)
}
