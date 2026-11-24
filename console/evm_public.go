package main

// Public EVM TEST networks for the separate programs:
//
//	go run . chains -network testnets
//
// Bitcoin runs on signet (split_signet.go) and the EVM chains on their public
// test networks: Sepolia, Polygon Amoy, Base Sepolia, BNB Chain testnet and
// Avalanche Fuji. Their coins have no value and come from public faucets.
//
//   - Nothing is mined here and there is no faucet button: blocks come by
//     themselves, and you send faucet coins to the addresses the programs show.
//   - Before anything else, the chains program asks each node which chain it
//     is on. A main network (real coins) is always refused, and so is any
//     chain that isn't the expected test network.
//   - Each program signs its own transactions (evm_sign.go).
//   - The exchange puts the CastleVault contract on each network itself, once
//     its address has test coins, and tells the chains program where it is.
//     The wallet only uses a contract whose code it has checked is exactly
//     the CastleVault code built into this program.
//   - Reads from the public nodes are cached for a few seconds, so the pages
//     and watchtowers don't flood them.
//
// Test networks only. Not audited.

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"
)

// ------------------------------------------------------------------ the safety check

// mainNets are chain IDs of main networks (real coins). They are refused
// whatever chain was asked for.
var mainNets = map[int64]string{
	1: "Ethereum", 10: "Optimism", 56: "BNB Chain", 100: "Gnosis", 137: "Polygon", 250: "Fantom",
	324: "zkSync", 8453: "Base", 42161: "Arbitrum", 43114: "Avalanche", 59144: "Linea", 534352: "Scroll",
}

var errMainnet = errors.New("main network")

// checkTestChain asks the node at url which chain it is on, and refuses
// anything but c's public test network.
func checkTestChain(url string, c evmChain) error {
	e := &EthEngine{url: url}
	s, err := e.callStr("eth_chainId")
	if err != nil {
		return fmt.Errorf("no answer from %s: %v", url, err)
	}
	id := hexBig(s).Int64()
	if name, ok := mainNets[id]; ok || id == c.MainID {
		if name == "" {
			name = c.Name
		}
		return fmt.Errorf("%w: the node at %s is on %s's MAIN network (chain ID %d), where coins are real. Castlekeep only runs on test networks", errMainnet, url, name, id)
	}
	if id != c.ChainID {
		return fmt.Errorf("the node at %s is on chain ID %d, not %s (%d)", url, id, c.netName(), c.ChainID)
	}
	return nil
}

// ------------------------------------------------------------------ settings

// publicInfo is how the pages show a public EVM test network.
func publicInfo(c evmChain, exitDelay int64) ChainInfo {
	info := evmConfig(c, "", "", "").Info
	info.Name = c.netName()
	info.Public, info.Explorer = true, c.Explorer
	info.MineButtons = []MineButton{}
	info.DefaultLock, info.DefaultOrder = "0.01", "0.005"
	info.TestCash = true
	info.Model = fmt.Sprintf("%s's public test network (%s, chain ID %d): blocks come by themselves about every %s, and the coins have no value. "+
		"The CastleVault contract: both sides sign every state, during an exit only a newer state can replace an older one, and the exchange can lock %s so you can buy from it. "+
		"The exit window is %s.", c.Name, c.Network, c.ChainID, secsText(c.BlockSecs), c.Ticker, durText(exitDelay))
	return info
}

func secsText(s float64) string {
	if s == float64(int64(s)) {
		return fmt.Sprintf("%d seconds", int64(s))
	}
	return fmt.Sprintf("%.1f seconds", s)
}

// durText is a length of time in words, e.g. "1 hour" or "24 hours".
func durText(secs int64) string {
	switch {
	case secs%3600 == 0:
		if secs == 3600 {
			return "1 hour"
		}
		return fmt.Sprintf("%d hours", secs/3600)
	case secs%60 == 0:
		if secs == 60 {
			return "1 minute"
		}
		return fmt.Sprintf("%d minutes", secs/60)
	}
	return fmt.Sprintf("%d seconds", secs)
}

// parseExitWindow reads -exit-window: between 1 minute (the contract's
// minimum) and 24 hours.
func parseExitWindow(s string) (int64, error) {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("-exit-window: write it like 1h, 30m or 24h")
	}
	if d < time.Minute || d > 24*time.Hour {
		return 0, fmt.Errorf("-exit-window must be between 1m and 24h")
	}
	return int64(d / time.Second), nil
}

// ------------------------------------------------------------------ cached reads

// rpcCache keeps the answers to read-only calls for a few seconds, and the
// final result of finished transactions for good.
type rpcCache struct {
	mu   sync.Mutex
	vals map[string]cached
	done map[string]int
}

type cached struct {
	at  time.Time
	val json.RawMessage
}

// How long reads are kept, and how often the setup steps run (tests shorten them).
var (
	cacheFor   = 4 * time.Second
	setupEvery = 5 * time.Second
	lookEvery  = 10 * time.Second
)

var cachedMethods = map[string]bool{
	"eth_blockNumber": true, "eth_getBlockByNumber": true, "eth_getBalance": true, "eth_call": true,
	"eth_getCode": true, "eth_chainId": true, "eth_gasPrice": true, "eth_maxPriorityFeePerGas": true,
}

func (c *rpcCache) get(method string, params []any, fetch func() (json.RawMessage, error)) (json.RawMessage, error) {
	if !cachedMethods[method] {
		return fetch()
	}
	b, _ := json.Marshal(params)
	key := method + string(b)
	c.mu.Lock()
	if v, ok := c.vals[key]; ok && time.Since(v.at) < cacheFor {
		c.mu.Unlock()
		return v.val, nil
	}
	c.mu.Unlock()
	val, err := fetch()
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.vals == nil || len(c.vals) > 500 {
		c.vals = map[string]cached{}
	}
	c.vals[key] = cached{time.Now(), val}
	c.mu.Unlock()
	return val, nil
}

// forget drops the cached reads, after this program sent a transaction.
func (c *rpcCache) forget() {
	c.mu.Lock()
	c.vals = nil
	c.mu.Unlock()
}

func (c *rpcCache) final(hash string) (int, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.done[hash]
	return r, ok
}

func (c *rpcCache) setFinal(on bool, hash string, r int) {
	if !on {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done == nil {
		c.done = map[string]int{}
	}
	c.done[hash] = r
}

// ------------------------------------------------------------------ the contract

// codeOK reports whether addr holds exactly the CastleVault code built into
// this program (the code a deployment leaves on the chain is part of the
// compiled .bin).
func (e *EthEngine) codeOK(addr string) bool {
	code, err := e.callStr("eth_getCode", addr, "latest")
	code = strings.ToLower(strings.TrimPrefix(code, "0x"))
	if err != nil || len(code) < 200 {
		return false
	}
	return strings.Contains(strings.ToLower(strings.TrimSpace(castleVaultBin)), code)
}

// deploy sends the CastleVault contract from addr (signed here). It returns
// the transaction hash.
func (e *EthEngine) deploy(from string) (string, error) {
	tx := map[string]string{"from": from, "data": "0x" + strings.TrimSpace(castleVaultBin)}
	gas, err := e.callStr("eth_estimateGas", tx, "latest")
	if err != nil {
		return "", err
	}
	tx["gas"] = "0x" + mulDiv(hexBig(gas), 13, 10).Text(16)
	k := e.signerFor(from)
	if k == nil {
		return "", errors.New("no key to sign the deployment with")
	}
	return e.sendSigned(tx, k)
}

// createdAt is the address of the contract a deployment made ("" if none).
func (e *EthEngine) createdAt(hash string) string {
	r, err := e.call("eth_getTransactionReceipt", hash)
	if err != nil {
		return ""
	}
	var rc struct {
		ContractAddress string `json:"contractAddress"`
	}
	json.Unmarshal(r, &rc)
	return rc.ContractAddress
}

// ------------------------------------------------------------------ the chains program

// publicEVM is one public EVM test network as the chains program reports it.
// It runs no node: it checks the public node, and keeps the contract address
// the exchange reports once it has checked the code there.
type publicEVM struct {
	c     evmChain
	url   string
	delay int64
	file  string

	mu       sync.Mutex
	ready    bool
	progress string
	contract string
}

func startPublicEVM(c evmChain, url string, delay int64, file string) *publicEVM {
	p := &publicEVM{c: c, url: url, delay: delay, file: file, progress: "Connecting to " + c.Network + "…"}
	go p.check()
	return p
}

func (p *publicEVM) check() {
	for try := 1; ; try++ {
		err := checkTestChain(p.url, p.c)
		p.mu.Lock()
		if err == nil {
			p.ready, p.progress = true, "Ready"
			if saved := keptContract(p.file); saved != "" && p.light().codeOK(saved) {
				p.contract = saved
			}
			p.mu.Unlock()
			fmt.Printf("%s: connected (%s).\n", p.c.netName(), p.url)
			return
		}
		if errors.Is(err, errMainnet) || try >= 4 {
			p.progress = "Failed: " + err.Error()
			p.mu.Unlock()
			fmt.Printf("%s left out: %v\n", p.c.netName(), err)
			return
		}
		p.progress = fmt.Sprintf("Can't reach %s yet (%v). Trying again…", p.c.Network, err)
		p.mu.Unlock()
		time.Sleep(10 * time.Second)
	}
}

func (p *publicEVM) light() *EthEngine {
	return newEthLight(EthEndpoint{URL: p.url, Public: true})
}

func (p *publicEVM) status() ChainStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	cs := ChainStatus{ID: p.c.ID, RPC: p.url, Progress: p.progress, Ready: p.ready, Network: "public"}
	if p.ready {
		cs.Eth = &EthEndpoint{URL: p.url, Contract: p.contract, ChainID: fmt.Sprint(p.c.ChainID), ExitDelay: p.delay,
			SignLocally: true, Public: true}
	}
	return cs
}

// register keeps the contract address the exchange reports, after checking
// the code there.
func (p *publicEVM) register(addr string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.ready {
		return userErr("%s isn't connected yet.", p.c.Network)
	}
	if strings.EqualFold(addr, p.contract) {
		return nil
	}
	if !p.light().codeOK(addr) {
		return userErr("There's no CastleVault contract (this version's code) at %s on %s.", addr, p.c.Network)
	}
	if p.contract != "" {
		fmt.Printf("%s: the exchange moved to a new contract at %s (was %s).\n", p.c.netName(), addr, p.contract)
	}
	p.contract = addr
	if err := saveContract(p.file, addr); err != nil {
		fmt.Println("Couldn't save the contract address:", err)
	}
	fmt.Printf("%s: CastleVault contract at %s\n", p.c.netName(), addr)
	return nil
}

// ContractReq tells the chains program where the exchange put the contract.
type ContractReq struct{ Chain, Address string }

// ------------------------------------------------------------------ the exchange

// evmSetup puts the contract on a public network once the exchange's
// address has test coins, and tells the chains program where it is (x.mu held).
func (x *Exchange) evmSetup(xe *xEth) {
	if !xe.c.public {
		return
	}
	if xe.c.contract != "" {
		if !xe.announced && xe.announce != nil && time.Since(xe.lastTry) >= 2*setupEvery {
			xe.lastTry = time.Now()
			if err := xe.announce(xe.c.contract); err == nil {
				xe.announced = true
			}
		}
		return
	}
	if time.Since(xe.lastTry) < setupEvery {
		return
	}
	xe.lastTry = time.Now()
	if xe.deployTx != "" {
		switch xe.c.receipt(xe.deployTx) {
		case 0:
			return
		case 1:
			if addr := xe.c.createdAt(xe.deployTx); addr != "" && xe.c.codeOK(addr) {
				xe.c.contract, xe.setupNote = addr, ""
				fmt.Printf("%s: the exchange put the CastleVault contract at %s\n", evmName(xe.id), addr)
				xe.lastTry = time.Time{}
				return
			}
		}
		xe.deployTx = "" // failed: try again
		return
	}
	if fromWei(xe.c.balance(xe.addr)) == 0 {
		xe.setupNote = "Waiting for test coins at the address above, to put the contract on this network."
		return
	}
	hash, err := xe.c.deploy(xe.addr)
	if err != nil {
		note := "Couldn't put the contract on the network yet: " + revertReason(err)
		if note != xe.setupNote {
			fmt.Printf("%s: %s\n", evmName(xe.id), note)
		}
		xe.setupNote, xe.lastTry = note, time.Now().Add(5*setupEvery) // try again in about 30 s
		return
	}
	xe.deployTx = hash
	xe.setupNote = "Putting the contract on the network (transaction " + hash + ")."
	fmt.Printf("%s: putting the CastleVault contract on the network (transaction %s)\n", evmName(xe.id), hash)
}

// ------------------------------------------------------------------ the wallet

// evmSetup finds and checks the contract on a public network (w.mu held).
func (w *Wallet) evmSetup(we *wEth) {
	if !we.c.public || we.checked {
		return
	}
	if time.Since(we.lastLook) < lookEvery {
		return
	}
	we.lastLook = time.Now()
	addr := we.c.contract
	if addr == "" && we.lookup != nil {
		addr = we.lookup()
	}
	if addr == "" {
		we.setupNote = "The exchange hasn't put the contract on this network yet. It does that by itself once its address has test coins."
		return
	}
	if !we.c.codeOK(addr) {
		we.c.contract = ""
		we.setupNote = "The contract at " + addr + " isn't this version's CastleVault code, so your wallet won't use it."
		return
	}
	we.c.contract, we.checked, we.setupNote = addr, true, ""
}

// lookupContract asks the chains program for a network's contract address.
func lookupContract(chains, id string) func() string {
	return func() string {
		var st []ChainStatus
		if call(chains, "/chains", empty{}, &st) != nil {
			return ""
		}
		for _, c := range st {
			if c.ID == id && c.Eth != nil {
				return c.Eth.Contract
			}
		}
		return ""
	}
}

// announceContract tells the chains program where the exchange put a contract.
func announceContract(chains, id string) func(string) error {
	return func(addr string) error {
		return call(chains, "/contract", ContractReq{id, addr}, nil)
	}
}

func mulDiv(x *big.Int, m, d int64) *big.Int {
	return new(big.Int).Div(new(big.Int).Mul(x, big.NewInt(m)), big.NewInt(d))
}
