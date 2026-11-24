package main

// Public EVM test networks (evm_public.go), tried on a Hardhat node that acts
// like one: it mines each transaction by itself, has the Sepolia chain ID,
// and can't send for the exchange or the wallet (their keys are their own,
// so every transaction is signed by them). The exchange deploys the
// contract, the chains program checks and keeps its address, and the wallet
// checks the code before locking anything.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// publicNode starts a Hardhat node with chainID that mines by itself.
func publicNode(t *testing.T, chainID int64) string {
	t.Helper()
	if !hardhatInstalled(defaultHardhatDir()) {
		t.Skipf("Hardhat isn't installed in %s", defaultHardhatDir())
	}
	port := freePort()
	cmd, err := startHardhat(defaultHardhatDir(), chainID, port)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	for i := 0; !ethNodeUp(url); i++ {
		if i > 120 {
			t.Fatal("the Hardhat node didn't start")
		}
		time.Sleep(500 * time.Millisecond)
	}
	return url
}

func nodeCall(t *testing.T, url, method string, params ...any) json.RawMessage {
	t.Helper()
	r, err := (&EthEngine{url: url}).call(method, params...)
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	return r
}

func TestPublicChainCheck(t *testing.T) {
	url := publicNode(t, 1) // a node that says it is Ethereum's MAIN network
	eth, pol := evmChains[0], evmChains[1]
	if err := checkTestChain(url, eth); !errors.Is(err, errMainnet) {
		t.Fatalf("a main network wasn't refused: %v", err)
	}
	if err := checkTestChain(url, pol); !errors.Is(err, errMainnet) {
		t.Fatalf("a main network wasn't refused for another chain either: %v", err)
	}
	p := startPublicEVM(eth, url, 3600, filepath.Join(t.TempDir(), "c.json"))
	for i := 0; ; i++ {
		if st := p.status(); strings.HasPrefix(st.Progress, "Failed") {
			if st.Ready || st.Eth != nil {
				t.Fatal("a refused network was reported ready")
			}
			t.Logf("refused: %s", st.Progress)
			break
		}
		if i > 50 {
			t.Fatal("the main network wasn't refused")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := checkTestChain("http://127.0.0.1:1", eth); err == nil {
		t.Fatal("no node, but no error")
	}
}

func TestExitWindowFlag(t *testing.T) {
	for in, want := range map[string]int64{"1h": 3600, "24h": 86400, "30m": 1800, "1m": 60} {
		if got, err := parseExitWindow(in); err != nil || got != want {
			t.Errorf("%s: %d %v", in, got, err)
		}
	}
	for _, bad := range []string{"30s", "25h", "soon", ""} {
		if _, err := parseExitWindow(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
	if durText(3600) != "1 hour" || durText(86400) != "24 hours" || durText(1800) != "30 minutes" {
		t.Error("durText")
	}
}

func TestRPCCache(t *testing.T) {
	var c rpcCache
	n := 0
	fetch := func() (json.RawMessage, error) { n++; return json.RawMessage(fmt.Sprint(n)), nil }
	a, _ := c.get("eth_blockNumber", []any{}, fetch)
	b, _ := c.get("eth_blockNumber", []any{}, fetch)
	if string(a) != "1" || string(b) != "1" {
		t.Fatalf("a read wasn't kept: %s %s", a, b)
	}
	c.get("eth_sendRawTransaction", []any{"0x"}, fetch)
	c.get("eth_sendRawTransaction", []any{"0x"}, fetch)
	if n != 3 {
		t.Fatalf("a transaction was cached (%d fetches)", n)
	}
	c.forget()
	if x, _ := c.get("eth_blockNumber", []any{}, fetch); string(x) != "4" {
		t.Fatalf("forget didn't drop the reads: %s", x)
	}
	c.setFinal(true, "0xabc", 1)
	if r, ok := c.final("0xabc"); !ok || r != 1 {
		t.Fatal("a finished receipt wasn't kept")
	}
}

// publicEnv is a chains service, an exchange server and a wallet on a node
// that behaves like a public test network.
type publicEnv struct {
	*splitEnv
	url string
	p   *publicEVM
	xe  *xEth
	we  *wEth
}

func newPublicEnv(t *testing.T, exitSecs int64) *publicEnv {
	t.Helper()
	oldCache, oldSetup, oldLook := cacheFor, setupEvery, lookEvery
	cacheFor, setupEvery, lookEvery = 0, 0, 0
	t.Cleanup(func() { cacheFor, setupEvery, lookEvery = oldCache, oldSetup, oldLook })

	c := evmChains[0] // Ethereum: Sepolia's chain ID
	url := publicNode(t, c.ChainID)
	p := startPublicEVM(c, url, exitSecs, filepath.Join(t.TempDir(), "evm-public-eth.json"))
	for i := 0; !p.status().Ready; i++ {
		if i > 100 {
			t.Fatalf("not ready: %s", p.status().Progress)
		}
		time.Sleep(100 * time.Millisecond)
	}
	cm := http.NewServeMux()
	cm.Handle("/chains", handle(func(empty) ([]ChainStatus, error) { return []ChainStatus{p.status()}, nil }))
	cm.Handle("/contract", handle(func(r ContractReq) (empty, error) { return empty{}, p.register(r.Address) }))
	chains := httptest.NewServer(cm)
	t.Cleanup(chains.Close)

	ep := *p.status().Eth
	if !ep.Public || ep.Contract != "" {
		t.Fatalf("endpoint: %+v", ep)
	}
	x, err := NewExchange(map[string]UTXOConfig{}, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	x.AttachEVM(c.ID, ep, c.Price)
	x.evm[c.ID].announce = announceContract(chains.URL, c.ID)
	srv := httptest.NewServer(x.Handler())
	t.Cleanup(srv.Close)
	w, err := NewWallet(srv.URL, map[string]UTXOConfig{}, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	w.AttachEVM(c.ID, ep)
	w.evm[c.ID].lookup = lookupContract(chains.URL, c.ID)
	return &publicEnv{&splitEnv{t: t, chain: c.ID, x: x, w: w}, url, p, x.evm[c.ID], w.evm[c.ID]}
}

// faucet stands in for a public faucet: it gives addr coins.
func (s *publicEnv) faucet(addr string, units int64) {
	s.t.Helper()
	nodeCall(s.t, s.url, "hardhat_setBalance", addr, "0x"+toWei(units).Text(16))
}

func (s *publicEnv) tick() {
	s.x.Tick()
	s.w.Tick()
}

func TestPublicEVMNetwork(t *testing.T) {
	s := newPublicEnv(t, 120)
	// The node can't send for these addresses: only signed transactions work.
	if _, err := s.we.c.callStr("eth_sendTransaction", map[string]string{"from": s.we.addr, "to": s.xe.addr}); err == nil {
		t.Fatal("the node sent a transaction for the wallet without its signature")
	}

	// No coins yet: no contract, so nothing can be locked.
	s.tick()
	s.refused(s.w.EthOpen("eth", 1_000_000), "Not ready to lock yet")
	if !strings.Contains(s.xe.setupNote, "Waiting for test coins") {
		t.Fatalf("exchange note: %q", s.xe.setupNote)
	}

	// The exchange gets faucet coins and deploys the contract itself.
	s.faucet(s.xe.addr, oneCoin)
	s.faucet(s.we.addr, oneCoin)
	for i := 0; s.we.c.contract == "" || !s.we.checked; i++ {
		if i > 10 {
			t.Fatalf("no contract: exchange %q, wallet %q", s.xe.setupNote, s.we.setupNote)
		}
		s.tick()
	}
	if s.p.status().Eth.Contract != s.xe.c.contract || !strings.EqualFold(s.we.c.contract, s.xe.c.contract) {
		t.Fatalf("contract: chains %s, exchange %s, wallet %s", s.p.status().Eth.Contract, s.xe.c.contract, s.we.c.contract)
	}
	if keptContract(s.p.file) != s.xe.c.contract {
		t.Fatal("the chains program didn't keep the contract address")
	}
	// The chains program refuses an address without the CastleVault code.
	if err := s.p.register(s.we.addr); err == nil {
		t.Fatal("an address with no contract was accepted")
	}

	// Lock, trade, top up twice, settle: every transaction signed locally.
	start := s.w.EthBalance("eth")
	s.ok(s.w.EthOpen("eth", 10_000_000)) // 0.1
	s.tick()
	if _, _, n, _ := s.w.EthVault("eth"); n != 1 {
		t.Fatalf("state #0 not agreed (%d states)", n)
	}
	s.ok(s.w.EthOrder("eth", "sell", 4_000_000))
	s.ok(s.w.EthCapacity("eth", 5_000_000))
	s.tick()
	s.ok(s.w.EthCapacity("eth", 1_000_000))
	s.tick()
	u, e, _, _ := s.w.EthVault("eth")
	if want := int64(6_000_000) - capacityFee(5_000_000) - capacityFee(1_000_000); u != want || e != 16_000_000-u {
		t.Fatalf("split after two top-ups: %d / %d (want yours %d)", u, e, want)
	}
	s.ok(s.w.EthSettle("eth"))
	s.tick()
	if _, _, _, closed := s.w.EthVault("eth"); closed != "settled" {
		t.Fatalf("not settled: %q", closed)
	}
	var tx struct{ Type, From string }
	json.Unmarshal(nodeCall(t, s.url, "eth_getTransactionByHash", s.we.vault.closeTx), &tx)
	if tx.Type != "0x2" || !strings.EqualFold(tx.From, s.we.addr) {
		t.Fatalf("settlement: type %s from %s", tx.Type, tx.From)
	}
	got := s.w.EthBalance("eth") - start
	if want := u - 10_000_000; got > want || got < want-ethGas {
		t.Fatalf("wallet change %d, want %d less gas", got, want)
	}

	// The exit window is the chains program's (here 2 minutes), on the
	// chain's own clock.
	s.ok(s.w.EthOpen("eth", 10_000_000))
	s.tick()
	s.ok(s.w.Admin("eth", "offline", 0))
	s.ok(s.w.EthExit("eth"))
	s.tick()
	s.refused(s.w.EthClaim("eth"), "window is still running")
	nodeCall(t, s.url, "evm_increaseTime", 121)
	nodeCall(t, s.url, "evm_mine")
	s.ok(s.w.EthClaim("eth"))
	s.tick()
	if _, _, _, closed := s.w.EthVault("eth"); closed != "finalized" {
		t.Fatalf("not paid out: %q", closed)
	}
}

// TestPublicEVMRestart: after a restart the chains program, the exchange and
// the wallet all come back to the same contract and vault.
func TestPublicEVMRestart(t *testing.T) {
	s := newPublicEnv(t, 3600)
	s.faucet(s.xe.addr, oneCoin)
	s.faucet(s.we.addr, oneCoin)
	for i := 0; !s.we.checked; i++ {
		if i > 10 {
			t.Fatal("no contract")
		}
		s.tick()
	}
	s.ok(s.w.EthOpen("eth", 10_000_000))
	s.tick()
	dir := t.TempDir()
	xf, wf := filepath.Join(dir, "exchange.json"), filepath.Join(dir, "wallet.json")
	s.x.SaveTo(xf)
	s.w.SaveTo(wf)

	// The chains program starts again and finds its saved contract.
	p2 := startPublicEVM(evmChains[0], s.url, 3600, s.p.file)
	for i := 0; !p2.status().Ready; i++ {
		if i > 100 {
			t.Fatal("not ready")
		}
		time.Sleep(100 * time.Millisecond)
	}
	ep := *p2.status().Eth
	if ep.Contract != s.xe.c.contract {
		t.Fatalf("contract after restart: %q, want %s", ep.Contract, s.xe.c.contract)
	}
	nx, _ := NewExchange(map[string]UTXOConfig{}, map[string]string{})
	nx.AttachEVM("eth", ep, 3000)
	if _, err := nx.Load(xf); err != nil {
		t.Fatal(err)
	}
	sameRecord(t, "exchange", s.x.record(), nx.record())
	nw, _ := NewWallet("http://127.0.0.1:1", map[string]UTXOConfig{}, map[string]string{})
	nw.AttachEVM("eth", ep)
	if _, err := nw.Load(wf); err != nil {
		t.Fatal(err)
	}
	sameRecord(t, "wallet", s.w.record(), nw.record())

	// A chains program that lost its file: the saved address comes back from
	// the exchange's and the wallet's own files, and the wallet checks it.
	ep.Contract = ""
	nw2, _ := NewWallet("http://127.0.0.1:1", map[string]UTXOConfig{}, map[string]string{})
	nw2.AttachEVM("eth", ep)
	if _, err := nw2.Load(wf); err != nil {
		t.Fatal(err)
	}
	nw2.Tick()
	if !nw2.evm["eth"].checked || nw2.evm["eth"].vault == nil {
		t.Fatal("the wallet didn't come back to its contract and vault")
	}
}
