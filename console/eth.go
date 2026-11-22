package main

// EthEngine runs Castlekeep on a local Ethereum test chain (Anvil or a
// Hardhat node) using the CastleVault smart contract in contract/.

import (
	"bytes"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	secpecdsa "github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"golang.org/x/crypto/sha3"
)

//go:embed contract/CastleVault.bin
var castleVaultBin string

const weiPerUnit = 10_000_000_000 // 1e10 wei = 0.00000001 ETH

type EthConfig struct {
	Info         ChainInfo
	AnvilPath    string // start this Anvil if it exists
	RPCURL       string // or connect to an already-running node
	InstallHint  string
	ExitDelay    int64 // seconds
	DefaultPrice int64
}

type ethState struct {
	Index    int
	Label    string
	UserSats int64
	ExchSats int64
	Height   int64
	userSig  []byte // 65 bytes r||s||v
	exchSig  []byte
}

type ethVault struct {
	id          *big.Int
	amount      int64 // units, total locked
	userDeposit int64
	exchDeposit int64
	fundTx      string
	fundAmt     int64
	fundDone    bool
	states      []*ethState
	openTx      string
	exitTx      string
	closeTx     string
	closeKind   string
	exchPend    string // latest state submission by the exchange
	userPend    string // latest state submission by your watchtower
	exchLast    int    // state index the exchange last submitted
	userLast    int
}

func (v *ethVault) latest() *ethState { return v.states[len(v.states)-1] }

type EthEngine struct {
	cfg EthConfig
	mu  sync.Mutex

	available   bool
	unavailable string
	ready       bool
	progress    string
	initErr     string

	node    *exec.Cmd
	url     string
	client  string // "anvil" or "hardhat"
	chainID *big.Int

	userKey, exchKey   *btcec.PrivateKey
	userAddr, exchAddr string
	faucetAddr         string
	contract           string
	exchStart          *big.Int
	exchReceived       int64

	tradeBook
	userWT bool
	vault  *ethVault
	eventLog
	txs      map[string]*TxInfo
	txSeq    int
	rejected int
}

func NewEthEngine(cfg EthConfig) *EthEngine {
	uk, _ := btcec.NewPrivateKey()
	ek, _ := btcec.NewPrivateKey()
	e := &EthEngine{
		cfg: cfg, progress: "Starting the Ethereum node…",
		userKey: uk, exchKey: ek, userAddr: ethAddress(uk), exchAddr: ethAddress(ek),
		tradeBook: tradeBook{price: cfg.DefaultPrice, mode: "honest"},
		userWT:    true,
		txs:       map[string]*TxInfo{},
	}
	switch {
	case cfg.RPCURL != "":
		e.available, e.url = true, cfg.RPCURL
	case cfg.AnvilPath != "":
		if _, err := os.Stat(cfg.AnvilPath); err == nil {
			e.available = true
		}
	}
	if !e.available {
		e.unavailable = cfg.InstallHint
	}
	return e
}

func (e *EthEngine) Info() ChainInfo    { return e.cfg.Info }
func (e *EthEngine) fmt(s int64) string { return fmtCoin(s, "ETH") }

// ------------------------------------------------------------------ crypto + ABI

func keccak(parts ...[]byte) []byte {
	h := sha3.NewLegacyKeccak256()
	for _, p := range parts {
		h.Write(p)
	}
	return h.Sum(nil)
}

func ethAddress(k *btcec.PrivateKey) string {
	pub := k.PubKey().SerializeUncompressed()
	return "0x" + hex.EncodeToString(keccak(pub[1:])[12:])
}

func selector(sig string) []byte { return keccak([]byte(sig))[:4] }

func word(b []byte) []byte {
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return out
}
func wordInt(n *big.Int) []byte { return word(n.Bytes()) }
func wordU(n int64) []byte      { return wordInt(big.NewInt(n)) }
func wordAddr(a string) []byte {
	b, _ := hex.DecodeString(strings.TrimPrefix(a, "0x"))
	return word(b)
}
func toWei(units int64) *big.Int { return new(big.Int).Mul(big.NewInt(units), big.NewInt(weiPerUnit)) }
func fromWei(w *big.Int) int64   { return new(big.Int).Quo(w, big.NewInt(weiPerUnit)).Int64() }

func calldata(sig string, words ...[]byte) string {
	b := append([]byte{}, selector(sig)...)
	for _, w := range words {
		b = append(b, w...)
	}
	return "0x" + hex.EncodeToString(b)
}

// stateDigest matches CastleVault.stateHash plus the "Ethereum Signed Message" prefix.
func (e *EthEngine) stateDigest(id *big.Int, nonce int64, userUnits int64) []byte {
	nb := make([]byte, 8)
	for i := 0; i < 8; i++ {
		nb[7-i] = byte(nonce >> (8 * i))
	}
	ca, _ := hex.DecodeString(strings.TrimPrefix(e.contract, "0x"))
	h := keccak([]byte("castlekeep-state"), wordInt(e.chainID), ca, wordInt(id), nb, wordInt(toWei(userUnits)))
	return keccak([]byte("\x19Ethereum Signed Message:\n32"), h)
}

// signState returns r||s||v (65 bytes, v = 27/28).
func signDigest(k *btcec.PrivateKey, digest []byte) []byte {
	c := secpecdsa.SignCompact(k, digest, false)
	out := make([]byte, 65)
	copy(out, c[1:65])
	out[64] = c[0]
	return out
}

func sigWords(sig []byte) []byte {
	b := append([]byte{}, wordU(int64(sig[64]))...)
	b = append(b, word(sig[0:32])...)
	return append(b, word(sig[32:64])...)
}

// ------------------------------------------------------------------ JSON-RPC

type rpcErr struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func (e *EthEngine) call(method string, params ...any) (json.RawMessage, error) {
	if params == nil {
		params = []any{}
	}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	resp, err := http.Post(e.url, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Result json.RawMessage `json:"result"`
		Error  *rpcErr         `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.Error != nil {
		msg := out.Error.Message
		if len(out.Error.Data) > 0 {
			msg += " " + string(out.Error.Data)
		}
		return nil, errors.New(msg)
	}
	return out.Result, nil
}

func (e *EthEngine) callStr(method string, params ...any) (string, error) {
	r, err := e.call(method, params...)
	if err != nil {
		return "", err
	}
	var s string
	err = json.Unmarshal(r, &s)
	return s, err
}

func hexBig(s string) *big.Int {
	n, _ := new(big.Int).SetString(strings.TrimPrefix(s, "0x"), 16)
	if n == nil {
		return big.NewInt(0)
	}
	return n
}

func (e *EthEngine) height() int64 {
	s, err := e.callStr("eth_blockNumber")
	if err != nil {
		return 0
	}
	return hexBig(s).Int64()
}

func (e *EthEngine) chainTime() int64 {
	r, err := e.call("eth_getBlockByNumber", "latest", false)
	if err != nil {
		return 0
	}
	var b struct {
		Timestamp string `json:"timestamp"`
	}
	json.Unmarshal(r, &b)
	return hexBig(b.Timestamp).Int64()
}

func (e *EthEngine) balance(addr string) *big.Int {
	s, err := e.callStr("eth_getBalance", addr, "latest")
	if err != nil {
		return big.NewInt(0)
	}
	return hexBig(s)
}

func (e *EthEngine) ethCall(data string) ([]byte, error) {
	s, err := e.callStr("eth_call", map[string]string{"to": e.contract, "data": data}, "latest")
	if err != nil {
		return nil, err
	}
	return hex.DecodeString(strings.TrimPrefix(s, "0x"))
}

var revertRe = []*regexp.Regexp{
	regexp.MustCompile(`reverted with reason string '([^']*)'`),
	regexp.MustCompile(`execution reverted: ([^"\\]*)`),
}

func revertReason(err error) string {
	s := err.Error()
	for _, re := range revertRe {
		if m := re.FindStringSubmatch(s); m != nil {
			r := strings.TrimSpace(m[1])
			return strings.ToUpper(r[:1]) + r[1:] + "."
		}
	}
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

// send submits a transaction from an account the node has unlocked or
// impersonates. Reverts are caught when the node estimates gas.
func (e *EthEngine) send(from string, value *big.Int, data string, info *TxInfo, okTitle, badTitle string) (string, error) {
	tx := map[string]string{"from": from, "to": e.contract, "data": data}
	if value != nil && value.Sign() > 0 {
		tx["value"] = "0x" + value.Text(16)
	}
	return e.sendRaw(tx, info, okTitle, badTitle)
}

func (e *EthEngine) sendRaw(tx map[string]string, info *TxInfo, okTitle, badTitle string) (string, error) {
	info.Seq = e.txSeq
	e.txSeq++
	// Simulate against the latest block first so the contract's refusal shows
	// up straight away, the same way a Bitcoin node rejects a bad spend.
	gas, err := e.callStr("eth_estimateGas", tx, "latest")
	var hash string
	if err == nil {
		tx["gas"] = "0x" + new(big.Int).Div(new(big.Int).Mul(hexBig(gas), big.NewInt(13)), big.NewInt(10)).Text(16)
		hash, err = e.callStr("eth_sendTransaction", tx)
	}
	if err != nil {
		e.rejected++
		info.Txid = fmt.Sprintf("not-sent-%d", e.rejected)
		info.Status = "rejected"
		info.Error = revertReason(err)
		e.txs[info.Txid] = info
		e.log("bad", badTitle, "Refused by the contract: "+info.Error, info.Txid)
		return "", err
	}
	info.Txid = hash
	info.Status = "mempool"
	e.txs[hash] = info
	e.log("ok", okTitle, "Sent. Mine a block to confirm it.", hash)
	return hash, nil
}

// receipt: 0 = pending, 1 = success, -1 = reverted when mined
func (e *EthEngine) receipt(hash string) int {
	if hash == "" || strings.HasPrefix(hash, "not-sent") {
		return 0
	}
	if t := e.txs[hash]; t != nil && t.Status == "confirmed" {
		return 1
	}
	r, err := e.call("eth_getTransactionReceipt", hash)
	if err != nil || string(r) == "null" {
		return 0
	}
	var rc struct {
		Status      string `json:"status"`
		BlockNumber string `json:"blockNumber"`
	}
	json.Unmarshal(r, &rc)
	t := e.txs[hash]
	if rc.Status == "0x1" {
		if t != nil {
			t.Status, t.Height = "confirmed", hexBig(rc.BlockNumber).Int64()
		}
		return 1
	}
	if t != nil && t.Status != "rejected" {
		t.Status, t.Height = "rejected", hexBig(rc.BlockNumber).Int64()
		t.Error = "Reverted when it was mined: another transaction changed the vault first."
		e.log("bad", t.Label+" failed when mined", t.Error, hash)
	}
	return -1
}

func (e *EthEngine) log(kind, title, detail, txid string) {
	e.eventLog.add(e.height(), kind, title, detail, txid)
}

// ------------------------------------------------------------------ lifecycle

func (e *EthEngine) Start() {
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

func (e *EthEngine) start() error {
	if e.url == "" {
		port := freePort()
		e.url = fmt.Sprintf("http://127.0.0.1:%d", port)
		e.node = exec.Command(e.cfg.AnvilPath, "--port", fmt.Sprint(port), "--silent")
		if err := e.node.Start(); err != nil {
			return fmt.Errorf("couldn't start anvil: %v", err)
		}
	}
	for i := 0; ; i++ {
		if _, err := e.callStr("eth_chainId"); err == nil {
			break
		}
		if i > 120 {
			return errors.New("the Ethereum node didn't respond")
		}
		time.Sleep(500 * time.Millisecond)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.progress = "Deploying the CastleVault contract…"

	ver, _ := e.callStr("web3_clientVersion")
	e.client = "hardhat"
	if strings.Contains(strings.ToLower(ver), "anvil") {
		e.client = "anvil"
	}
	cid, err := e.callStr("eth_chainId")
	if err != nil {
		return err
	}
	e.chainID = hexBig(cid)
	if _, err := e.call("evm_setAutomine", false); err != nil {
		return fmt.Errorf("couldn't switch off automining: %v", err)
	}
	var accts []string
	r, err := e.call("eth_accounts")
	if err != nil {
		return err
	}
	json.Unmarshal(r, &accts)
	if len(accts) == 0 {
		return errors.New("the node has no unlocked dev accounts")
	}
	e.faucetAddr = accts[0]

	// Deploy the contract.
	hash, err := e.callStr("eth_sendTransaction", map[string]string{
		"from": e.faucetAddr, "data": "0x" + strings.TrimSpace(castleVaultBin), "gas": "0x2dc6c0"})
	if err != nil {
		return fmt.Errorf("couldn't deploy the contract: %v", err)
	}
	if _, err := e.call("evm_mine"); err != nil {
		return err
	}
	rr, err := e.call("eth_getTransactionReceipt", hash)
	if err != nil {
		return err
	}
	var rc struct {
		ContractAddress string `json:"contractAddress"`
		Status          string `json:"status"`
	}
	json.Unmarshal(rr, &rc)
	if rc.Status != "0x1" || rc.ContractAddress == "" {
		return errors.New("contract deployment failed")
	}
	e.contract = rc.ContractAddress

	// Fund and unlock the two parties.
	for _, a := range []struct {
		addr string
		wei  *big.Int
	}{{e.userAddr, toWei(2 * unit)}, {e.exchAddr, toWei(10 * unit)}} {
		if _, err := e.call(e.client+"_setBalance", a.addr, "0x"+a.wei.Text(16)); err != nil {
			return fmt.Errorf("couldn't fund test account: %v", err)
		}
		if _, err := e.call(e.client+"_impersonateAccount", a.addr); err != nil {
			return fmt.Errorf("couldn't unlock test account: %v", err)
		}
	}
	e.call("evm_mine")
	e.exchStart = e.balance(e.exchAddr)
	e.log("info", "Test chain ready", fmt.Sprintf("CastleVault contract deployed at %s on a local %s node.", e.contract, map[string]string{"anvil": "Anvil", "hardhat": "Hardhat"}[e.client]), "")
	e.log("info", "Your wallet has 2 test ETH", "Lock some into a vault to start trading. Gas fees come out of your wallet, as on the real network.", "")
	e.progress = "Ready"
	e.ready = true
	return nil
}

func (e *EthEngine) Stop() {
	if e.node != nil && e.node.Process != nil {
		e.node.Process.Kill()
		e.node.Wait()
	}
}

// ------------------------------------------------------------------ on-chain vault

type chainVault struct {
	exitStart, bestNonce int64
	bestUser             int64
	hasBest, closed      bool
}

func (e *EthEngine) readVault() (chainVault, error) {
	// vaults(id): user, exchange, amount, userDeposit, exchangeDeposit,
	// exitDelay, exitStart, bestNonce, bestUser, hasBest, closed
	b, err := e.ethCall(calldata("vaults(uint256)", wordInt(e.vault.id)))
	if err != nil || len(b) < 11*32 {
		return chainVault{}, errors.New("couldn't read the vault")
	}
	w := func(i int) *big.Int { return new(big.Int).SetBytes(b[i*32 : (i+1)*32]) }
	return chainVault{
		exitStart: w(6).Int64(), bestNonce: w(7).Int64(), bestUser: fromWei(w(8)),
		hasBest: w(9).Sign() != 0, closed: w(10).Sign() != 0,
	}, nil
}

func (e *EthEngine) vaultStatus() string {
	v := e.vault
	if v == nil {
		return "none"
	}
	if v.closeTx != "" {
		switch e.receipt(v.closeTx) {
		case 1:
			return "closed"
		case -1:
			v.closeTx, v.closeKind = "", ""
		default:
			return "closing"
		}
	}
	if v.exitTx != "" && e.receipt(v.exitTx) == -1 {
		v.exitTx = ""
	}
	if e.receipt(v.openTx) != 1 {
		return "funding"
	}
	if v.exitTx != "" {
		if e.receipt(v.exitTx) != 1 {
			return "exit_pending"
		}
		cv, _ := e.readVault()
		if e.chainTime() >= cv.exitStart+e.cfg.ExitDelay {
			return "ready"
		}
		return "exit_window"
	}
	return "open"
}

func (e *EthEngine) newState(label string, user, exch int64) *ethState {
	v := e.vault
	s := &ethState{Index: len(v.states), Label: label, UserSats: user, ExchSats: exch, Height: e.height()}
	d := e.stateDigest(v.id, int64(s.Index), user)
	s.userSig = signDigest(e.userKey, d)
	s.exchSig = signDigest(e.exchKey, d)
	v.states = append(v.states, s)
	return s
}

func (e *EthEngine) stateCall(fn string, s *ethState, userSig []byte) string {
	return calldata(fn+"(uint256,uint64,uint256,(uint8,bytes32,bytes32),(uint8,bytes32,bytes32))",
		wordInt(e.vault.id), wordU(int64(s.Index)), wordInt(toWei(s.UserSats)), sigWords(userSig), sigWords(s.exchSig))
}

const ruleSig = `ecrecover(state, userSig)     == user      // else "missing a valid user signature"
ecrecover(state, exchangeSig) == exchange  // else "missing a valid exchange signature"`

// ------------------------------------------------------------------ actions

func (e *EthEngine) Do(r ActionReq) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.ready {
		return userErr("The Ethereum test chain is still starting up.")
	}
	switch r.Action {
	case "faucet":
		return e.faucet()
	case "mine":
		return e.mine(r.N, r.Secs)
	case "lock":
		return e.lock(r.Sats)
	case "order":
		return e.order(r.Side, r.Sats)
	case "settle":
		return e.settle()
	case "exit":
		return e.requestExit(r.By)
	case "claim":
		return e.finalize()
	case "capacity":
		return e.addCapacity(r.Sats)
	case "cash":
		e.cashCents += 1_000_000
		e.log("info", "Added $10,000 of test cash on the exchange", "Use it to buy ETH. Like any exchange balance, the exchange holds this cash.", "")
		return nil
	case "steal":
		return e.steal()
	case "mode":
		return e.setMode(r.Mode)
	case "price":
		if r.USD < 1 || r.USD > 10_000_000 {
			return userErr("Pick a price between $1 and $10,000,000.")
		}
		e.price = r.USD
		return nil
	case "watchtower":
		e.userWT = r.On
		if r.On {
			e.log("info", "Your watchtower is on", "It submits your newest signed state if an older one shows up during an exit.", "")
		} else {
			e.log("warn", "Your watchtower is off", "Nobody will defend you if the exchange submits an old state during an exit.", "")
		}
		e.watchtower()
		return nil
	}
	return userErr("Unknown action.")
}

func (e *EthEngine) faucet() error {
	info := &TxInfo{Label: "Test faucet: 1 ETH to your wallet", Path: "Ordinary ETH transfer from a test account",
		Inputs: []IO{{Label: "Test faucet account", Sats: unit, Ref: e.faucetAddr}}, Outputs: []IO{{Label: "Your wallet", Sats: unit, Ref: e.userAddr}}}
	e.sendRaw(map[string]string{"from": e.faucetAddr, "to": e.userAddr, "value": "0x" + toWei(unit).Text(16)},
		info, "Received 1 test ETH", "Faucet payment refused")
	return nil
}

func (e *EthEngine) mine(n int, secs int64) error {
	if secs > 0 {
		if _, err := e.call("evm_increaseTime", secs); err != nil {
			return err
		}
		if _, err := e.call("evm_mine"); err != nil {
			return err
		}
		e.log("info", "Skipped ahead "+describeHours(float64(secs)/60), "One block mined with the clock moved forward.", "")
	} else {
		if n < 1 || n > 500 {
			return userErr("Mine between 1 and 500 blocks at a time.")
		}
		for i := 0; i < n; i++ {
			if _, err := e.call("evm_mine"); err != nil {
				return err
			}
		}
		word := "blocks"
		if n == 1 {
			word = "block"
		}
		e.log("info", fmt.Sprintf("Mined %d %s", n, word), "Each block is about 12 seconds on the real network.", "")
	}
	e.applyFunding()
	e.watchtower()
	return nil
}

const capacityFeeBps = 20 // 0.2% of the locked amount

func capacityFee(sats int64) int64 { return sats * capacityFeeBps / 10_000 }

// addCapacity asks the exchange to lock its own ETH in the vault.
func (e *EthEngine) addCapacity(sats int64) error {
	v := e.vault
	switch st := e.vaultStatus(); st {
	case "open":
	case "funding":
		return userErr("Your deposit hasn't confirmed yet. Mine a block first.")
	default:
		return userErr("Open a vault first, then ask for buying capacity.")
	}
	if e.mode == "offline" {
		return userErr("The exchange is offline and can't lock anything.")
	}
	if v.exchDeposit > 0 || (v.fundTx != "" && e.receipt(v.fundTx) == 0) {
		return userErr("The exchange has already locked buying capacity in this vault.")
	}
	if sats < 100_000 || sats > 5*unit {
		return userErr("Ask for between 0.001 and 5 ETH.")
	}
	fee := capacityFee(sats)
	if fee > v.latest().UserSats {
		return userErr("The fee (%s) is more than your share of the vault.", e.fmt(fee))
	}
	info := &TxInfo{Label: "Exchange locks " + e.fmt(sats) + " of buying capacity", Path: "fund(id): payable, exchange only, once per vault",
		Script:  "fund(id)\nrequire(msg.sender == exchange)\nrequire(no exit in progress)\nrequire(no capacity added yet)\namount += msg.value",
		Inputs:  []IO{{Label: "Exchange wallet", Sats: sats, Ref: e.exchAddr}},
		Outputs: []IO{{Label: "Vault #" + v.id.String() + " (exchange's side)", Sats: sats, Ref: e.contract}}}
	hash, err := e.send(e.exchAddr, toWei(sats), calldata("fund(uint256)", wordInt(v.id)), info,
		"The exchange is locking "+e.fmt(sats)+" in your vault", "The exchange's deposit was refused")
	if err != nil {
		return nil
	}
	v.fundTx, v.fundAmt, v.fundDone = hash, sats, false
	e.log("info", "Mine a block to confirm the exchange's deposit",
		fmt.Sprintf("Once it confirms, you'll both sign a new state that adds the capacity and charges the %s fee (0.2%%) from your share.", e.fmt(fee)), "")
	return nil
}

// applyFunding signs the new state once the exchange's deposit confirms.
func (e *EthEngine) applyFunding() {
	v := e.vault
	if v == nil || v.fundTx == "" || v.fundDone {
		return
	}
	switch e.receipt(v.fundTx) {
	case 1:
		v.fundDone = true
		v.exchDeposit = v.fundAmt
		v.amount += v.fundAmt
		fee := capacityFee(v.fundAmt)
		l := v.latest()
		s := e.newState(fmt.Sprintf("Exchange locked %s (fee %s)", e.fmt(v.fundAmt), e.fmt(fee)), l.UserSats-fee, l.ExchSats+v.fundAmt+fee)
		e.log("sign", fmt.Sprintf("You both signed state #%d: the exchange's %s is in the vault", s.Index, e.fmt(v.fundAmt)),
			fmt.Sprintf("You can now buy up to %s from the exchange. The %s fee came out of your share.", e.fmt(s.ExchSats), e.fmt(fee)), "")
	case -1:
		v.fundTx = ""
	}
}

func (e *EthEngine) lock(sats int64) error {
	if e.vault != nil && e.vault.closeTx == "" {
		return userErr("You already have an open vault. Settle or exit it first.")
	}
	if sats < 100_000 {
		return userErr("Lock at least 0.001 ETH.")
	}
	bal := fromWei(e.balance(e.userAddr))
	if sats+100_000 > bal {
		return userErr("Your wallet holds %s, and you need a little extra for gas.", e.fmt(bal))
	}
	b, err := e.ethCall(calldata("nextId()"))
	if err != nil || len(b) < 32 {
		return errors.New("couldn't read the next vault number")
	}
	id := new(big.Int).SetBytes(b[:32])
	info := &TxInfo{Label: "Lock " + e.fmt(sats) + " into vault #" + id.String(),
		Path:   "open(exchange, exitDelay): payable",
		Script: fmt.Sprintf("open(exchange = %s, exitDelay = %d seconds)\nrequire(msg.value > 0)", e.exchAddr, e.cfg.ExitDelay),
		Inputs: []IO{{Label: "Your wallet", Sats: sats, Ref: e.userAddr}}, Outputs: []IO{{Label: "Vault #" + id.String() + " in the CastleVault contract", Sats: sats, Ref: e.contract}}}
	hash, err := e.send(e.userAddr, toWei(sats), calldata("open(address,uint64)", wordAddr(e.exchAddr), wordU(e.cfg.ExitDelay)),
		info, "Locked "+e.fmt(sats)+" into vault #"+id.String(), "Vault deposit refused")
	if err != nil {
		return nil
	}
	e.vault = &ethVault{id: id, amount: sats, userDeposit: sats, openTx: hash, exchLast: -1, userLast: -1}
	e.newState("Opening balance", sats, 0)
	e.log("sign", "You and the exchange signed state #0: you own the whole vault",
		"On Ethereum both sides sign every state, and the contract always prefers the newest one.", "")
	return nil
}

func (e *EthEngine) order(side string, sats int64) error {
	v := e.vault
	switch st := e.vaultStatus(); st {
	case "open":
	case "funding":
		return userErr("Your deposit hasn't confirmed yet. Mine a block first.")
	case "none", "closed", "closing":
		return userErr("Lock ETH into a vault before trading.")
	default:
		return userErr("Trading is frozen while an exit is in progress.")
	}
	if e.mode == "offline" {
		return userErr("The exchange is offline and isn't accepting orders.")
	}
	if sats <= 0 {
		return userErr("Enter an amount above zero.")
	}
	l := v.latest()
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
			if v.exchDeposit == 0 {
				return userErr("You can only buy back ETH sold from this vault (%s). Ask the exchange to lock buying capacity to buy more.", e.fmt(l.ExchSats))
			}
			return userErr("The exchange's side of the vault holds %s, so that's the most you can buy.", e.fmt(l.ExchSats))
		}
		if cost > e.cashCents {
			return userErr("That costs %s and you have %s in cash.", fmtUSD(cost), fmtUSD(e.cashCents))
		}
		user, exch = l.UserSats+sats, l.ExchSats-sats
		label = fmt.Sprintf("Bought %s at %s", e.fmt(sats), fmtUSD(e.price*100))
	default:
		return userErr("Unknown order side.")
	}
	s := e.newState(label, user, exch)
	e.log("sign", fmt.Sprintf("You signed state #%d with the order; the exchange signed it on the fill", s.Index),
		fmt.Sprintf("Yours: %s, owed to buyers: %s. State #%d now replaces every earlier state.", e.fmt(user), e.fmt(exch), s.Index), "")
	if side == "sell" {
		e.cashCents += cost
	} else {
		e.cashCents -= cost
	}
	e.log("ok", label, fmt.Sprintf("Filled instantly on the order book. Cash balance: %s.", fmtUSD(e.cashCents)), "")
	return nil
}

func (e *EthEngine) payoutIO(user, exch int64) []IO {
	var out []IO
	if user > 0 {
		out = append(out, IO{Label: "Your wallet", Sats: user, Ref: e.userAddr})
	}
	if exch > 0 {
		out = append(out, IO{Label: "Exchange wallet", Sats: exch, Ref: e.exchAddr})
	}
	return out
}

func (e *EthEngine) settle() error {
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
		e.log("warn", "The exchange refused to settle", "Your route now is Request exit, which starts the 24-hour window.", "")
		return nil
	}
	l := v.latest()
	info := &TxInfo{Label: fmt.Sprintf("Cooperative settlement of state #%d", l.Index), Path: "settle(): both signatures on the latest state",
		Script: "settle(id, nonce, userAmount, userSig, exchangeSig)\nrequire(no exit in progress)\n" + ruleSig,
		Inputs: []IO{{Label: "Vault #" + v.id.String(), Sats: v.amount, Ref: e.contract}}, Outputs: e.payoutIO(l.UserSats, l.ExchSats)}
	if hash, err := e.send(e.userAddr, nil, e.stateCall("settle", l, l.userSig), info,
		"Settled: your share is on its way back", "Settlement refused"); err == nil {
		v.closeTx, v.closeKind = hash, "settled"
	}
	return nil
}

func (e *EthEngine) requestExit(by string) error {
	v := e.vault
	st := e.vaultStatus()
	if st == "funding" {
		return userErr("Your deposit hasn't confirmed yet. Mine a block first.")
	}
	if st != "open" {
		return userErr("There's no open vault to exit.")
	}
	from, who := e.userAddr, "You"
	if by == "exchange" {
		from, who = e.exchAddr, "The exchange"
	}
	info := &TxInfo{Label: "Exit request (" + strings.ToLower(who) + ")", Path: "requestExit(): starts the 24-hour window",
		Script: "requestExit(id)\nrequire(msg.sender == user || msg.sender == exchange)\nexitStart = block.timestamp",
		Inputs: []IO{{Label: "Vault #" + v.id.String(), Sats: v.amount, Ref: e.contract}}}
	if hash, err := e.send(from, nil, calldata("requestExit(uint256)", wordInt(v.id)), info,
		who+" requested an exit", "Exit request refused"); err == nil {
		v.exitTx = hash
		e.log("info", "The 24-hour window starts once this confirms",
			"During it, anyone can submit a state both sides signed, and only a newer state can replace an older one. After it, anyone can pay the vault out.", "")
	}
	return nil
}

func (e *EthEngine) finalize() error {
	v := e.vault
	if v == nil || v.exitTx == "" {
		return userErr("Request an exit first. The payout only works after the exit window.")
	}
	if v.closeTx != "" {
		return userErr("This vault is already closed.")
	}
	cv, _ := e.readVault()
	user := v.userDeposit
	if cv.hasBest {
		user = cv.bestUser
	}
	info := &TxInfo{Label: "Pay out after the exit window", Path: "finalize(): newest submitted state, or everything to you if none",
		Script: "finalize(id)\nrequire(block.timestamp >= exitStart + exitDelay)  // else \"the exit window is still running\"\nuserAmount = hasBest ? bestUser : userDeposit   // no state: each side gets its deposit back",
		Inputs: []IO{{Label: "Vault #" + v.id.String(), Sats: v.amount, Ref: e.contract}}, Outputs: e.payoutIO(user, v.amount-user)}
	if hash, err := e.send(e.userAddr, nil, calldata("finalize(uint256)", wordInt(v.id)), info,
		"The vault paid out after the window", "Payout refused"); err == nil {
		v.closeTx, v.closeKind = hash, "finalized"
		if !cv.hasBest {
			switch l := v.latest(); {
			case l.UserSats < v.userDeposit:
				e.log("warn", "Nobody submitted a state, so you kept ETH you had sold",
					"The exchange missed its window. This is why a real exchange must watch for exits around the clock.", "")
			case l.UserSats > v.userDeposit:
				e.log("warn", "Nobody submitted a state, so the ETH you bought went back to the exchange",
					"With no state submitted, each side gets its own deposit back. Your watchtower would have submitted your latest state.", "")
			}
		}
	}
	return nil
}

func (e *EthEngine) steal() error {
	v := e.vault
	if v == nil || v.closeTx != "" {
		return userErr("Open a vault first so the exchange has something to try to take.")
	}
	fake := &ethState{Index: len(v.states), UserSats: 0, ExchSats: v.amount}
	fake.exchSig = signDigest(e.exchKey, e.stateDigest(v.id, int64(fake.Index), 0))
	fn := "settle"
	if v.exitTx != "" {
		fn = "submitState"
	}
	info := &TxInfo{Label: "Exchange tries to take the vault", Path: fn + "(): a state paying the exchange everything, signed only by the exchange",
		Script: ruleSig, Inputs: []IO{{Label: "Vault #" + v.id.String(), Sats: v.amount, Ref: e.contract}}}
	e.send(e.exchAddr, nil, e.stateCall(fn, fake, fake.exchSig), info, "The exchange took the vault", "The exchange tried to take your coins alone")
	return nil
}

func (e *EthEngine) setMode(m string) error {
	switch m {
	case "honest":
		e.log("info", "Exchange is online and honest", "", "")
	case "offline":
		e.log("warn", "Exchange went offline", "It won't sign settlements or answer exits.", "")
	case "malicious":
		e.log("warn", "Exchange turned malicious", "It refuses to settle, and during an exit it submits whichever signed state pays it most.", "")
	default:
		return userErr("Unknown exchange mode.")
	}
	e.mode = m
	e.watchtower()
	return nil
}

func (e *EthEngine) submit(by string, s *ethState) string {
	v := e.vault
	from, who := e.exchAddr, "Exchange"
	if by == "user" {
		from, who = e.userAddr, "Your watchtower"
	}
	info := &TxInfo{Label: fmt.Sprintf("%s submits state #%d", who, s.Index),
		Path:   "submitState(): only a newer state can replace an older one",
		Script: "submitState(id, nonce, userAmount, userSig, exchangeSig)\nrequire(inside the exit window)\nrequire(nonce > bestNonce)  // else \"a newer state was already submitted\"\n" + ruleSig,
		Inputs: []IO{{Label: "Vault #" + v.id.String(), Sats: v.amount, Ref: e.contract}}}
	title := fmt.Sprintf("%s submitted state #%d", who, s.Index)
	if by == "exchange" && s != v.latest() {
		title = fmt.Sprintf("Malicious exchange submitted an OLD state (#%d, not #%d)", s.Index, v.latest().Index)
	}
	hash, _ := e.send(from, nil, e.stateCall("submitState", s, s.userSig), info, title, who+"'s state was refused")
	return hash
}

// watchtower: both sides react to an exit in progress.
func (e *EthEngine) watchtower() {
	v := e.vault
	if v == nil || v.exitTx == "" || v.closeTx != "" || e.receipt(v.exitTx) != 1 {
		return
	}
	cv, err := e.readVault()
	if err != nil || cv.closed || e.chainTime() >= cv.exitStart+e.cfg.ExitDelay {
		return
	}
	latest := v.latest()
	if e.mode != "offline" {
		pick := latest
		if e.mode == "malicious" {
			for _, st := range v.states {
				if st.ExchSats > pick.ExchSats {
					pick = st
				}
			}
		}
		busy := v.exchPend != "" && e.receipt(v.exchPend) == 0
		if !busy && v.exchLast != pick.Index && (!cv.hasBest || int64(pick.Index) > cv.bestNonce) {
			v.exchLast = pick.Index
			v.exchPend = e.submit("exchange", pick)
		}
	}
	busy := v.userPend != "" && e.receipt(v.userPend) == 0
	exchCovering := v.exchPend != "" && e.receipt(v.exchPend) == 0 && v.exchLast >= latest.Index
	if e.userWT && !busy && !exchCovering && v.userLast != latest.Index {
		switch {
		case cv.hasBest && cv.bestNonce < int64(latest.Index):
			e.log("warn", fmt.Sprintf("Your watchtower spotted an old state (#%d) in the window", cv.bestNonce),
				fmt.Sprintf("It's submitting state #%d, which both sides signed later. The contract will replace the old one.", latest.Index), "")
			v.userLast = latest.Index
			v.userPend = e.submit("user", latest)
		case !cv.hasBest && latest.UserSats > v.userDeposit:
			e.log("info", "Your watchtower is submitting your latest state",
				fmt.Sprintf("Without it, each side would just get its deposit back and you'd lose the %s you bought.", e.fmt(latest.UserSats-v.userDeposit)), "")
			v.userLast = latest.Index
			v.userPend = e.submit("user", latest)
		}
	}
}

// ------------------------------------------------------------------ snapshot

func (e *EthEngine) Snapshot() Snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := Snapshot{Chain: e.cfg.Info, Available: e.available, Unavailable: e.unavailable,
		Ready: e.ready, Progress: e.progress, InitErr: e.initErr, Price: e.price, Mode: e.mode,
		CashCents: e.cashCents, Events: e.events, Watchtower: e.userWT, HasUserWT: true}
	if !e.ready {
		return s
	}
	s.Height = e.height()
	now := e.chainTime()
	s.ChainTime = time.Unix(now, 0).UTC().Format("2 Jan 15:04 UTC")
	if r, err := e.call("eth_getBlockByNumber", "pending", false); err == nil {
		var b struct {
			Transactions []string `json:"transactions"`
		}
		json.Unmarshal(r, &b)
		s.Mempool = len(b.Transactions)
	}
	s.WalletSats = fromWei(e.balance(e.userAddr))
	s.ExchSats = fromWei(new(big.Int).Sub(e.balance(e.exchAddr), e.exchStart))
	for _, t := range e.txs {
		if t.Status == "mempool" {
			e.receipt(t.Txid)
		}
		s.Txs = append(s.Txs, t)
	}
	sort.Slice(s.Txs, func(i, j int) bool { return s.Txs[i].Seq > s.Txs[j].Seq })

	if v := e.vault; v != nil {
		vv := &VaultView{Status: e.vaultStatus(), Locked: v.amount, ExitSent: v.exitTx != "", BestState: -1, PublishedState: -1,
			CloseTxid: v.closeTx, CloseKind: v.closeKind, ClockTotal: e.cfg.ExitDelay, Address: e.contract + " (vault #" + v.id.String() + ")",
			Scripts: []Labelled{
				{"Settle now: both signatures on the same state", "settle(id, nonce, userAmount, userSig, exchangeSig)\n" + ruleSig},
				{"During the 24-hour exit window", "submitState(...)\nrequire(nonce > bestNonce)   // only a newer state wins"},
				{"After the window", "finalize(id)\npays the newest submitted state, or refunds each side its own deposit if nobody submitted one"},
				{"Buying capacity", "fund(id)   // exchange only, once\nlocks the exchange's ETH in the vault so you can buy from it"},
			}}
		l := v.latest()
		vv.UserSats, vv.ExchSats = l.UserSats, l.ExchSats
		vv.UserDeposit, vv.ExchDeposit = v.userDeposit, v.exchDeposit
		vv.FundPending = v.fundTx != "" && !v.fundDone
		vv.Locked = v.amount
		for _, st := range v.states {
			vv.States = append(vv.States, StateView{Index: st.Index, Label: st.Label, UserSats: st.UserSats, ExchSats: st.ExchSats, Height: st.Height})
		}
		if v.exitTx != "" && e.receipt(v.exitTx) == 1 {
			if cv, err := e.readVault(); err == nil && cv.exitStart > 0 {
				vv.ClockStarted = true
				done := now - cv.exitStart
				if done > e.cfg.ExitDelay {
					done = e.cfg.ExitDelay
				}
				vv.ClockDone = done
				vv.ClockReady = done >= e.cfg.ExitDelay
				if cv.hasBest {
					vv.BestState = int(cv.bestNonce)
				}
			}
		}
		s.Vault = vv
	}
	return s
}
