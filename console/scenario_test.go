package main

// Scenario tests. Each test starts a real local test chain (btcd, ltcd, or
// the Ethereum node at CASTLEKEEP_ETH_RPC, default http://127.0.0.1:8545)
// and drives the engine through Do(ActionReq), the same call the console's
// /api/action endpoint makes. Accept/reject decisions come from the node.
//
//	go test -v -timeout 30m
//
// A chain whose node isn't installed or reachable is skipped with a message.
// Test networks only.

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/wire"
)

// ------------------------------------------------------------------ shared engines

var (
	engMu    sync.Mutex
	engines_ = map[string]Engine{}
)

func TestMain(m *testing.M) {
	code := m.Run()
	for _, e := range engines_ {
		e.Stop()
	}
	os.Exit(code)
}

func ethRPCURL() string {
	if u := os.Getenv("CASTLEKEEP_ETH_RPC"); u != "" {
		return u
	}
	return "http://127.0.0.1:8545"
}

func ethReachable(url string) bool {
	c := http.Client{Timeout: 3 * time.Second}
	r, err := c.Post(url, "application/json", bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`)))
	if err != nil {
		return false
	}
	r.Body.Close()
	return r.StatusCode == 200
}

// engine returns a started, ready engine for the chain, shared by all tests.
func engine(t *testing.T, chain string) Engine {
	t.Helper()
	engMu.Lock()
	defer engMu.Unlock()
	e, ok := engines_[chain]
	if !ok {
		switch chain {
		case "btc":
			e = NewUTXOEngine(btcConfig(goBin("btcd")))
		case "ltc":
			e = NewUTXOEngine(ltcConfig(goBin("ltcd")))
		case "eth":
			url := ethRPCURL()
			if !ethReachable(url) {
				t.Skipf("no Ethereum node at %s; start Hardhat (npx hardhat node) to run the Ethereum tests", url)
			}
			e = NewEthEngine(ethConfig("", url))
		}
		if s := e.Snapshot(); !s.Available {
			t.Skipf("%s not available: %s", chain, s.Unavailable)
		}
		e.Start()
		engines_[chain] = e
	}
	deadline := time.Now().Add(5 * time.Minute)
	for {
		s := e.Snapshot()
		if s.InitErr != "" {
			t.Fatalf("%s failed to start: %s", chain, s.InitErr)
		}
		if s.Ready {
			return e
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s didn't become ready: %s", chain, s.Progress)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// ------------------------------------------------------------------ harness

type harness struct {
	t     *testing.T
	e     Engine
	chain string
}

func newHarness(t *testing.T, chain string) *harness {
	h := &harness{t: t, e: engine(t, chain), chain: chain}
	h.fresh()
	return h
}

// fresh forgets any vault left by an earlier (possibly failed) scenario,
// resets the exchange, and makes sure the wallet holds at least 1.5 coins.
func (h *harness) fresh() {
	switch e := h.e.(type) {
	case *UTXOEngine:
		e.mu.Lock()
		e.vault, e.mode, e.price, e.cashCents, e.userWT = nil, "honest", e.cfg.DefaultPrice, 0, true
		e.events = nil
		e.mu.Unlock()
	case *EthEngine:
		e.mu.Lock()
		e.vault, e.mode, e.price, e.cashCents, e.userWT = nil, "honest", e.cfg.DefaultPrice, 0, true
		e.events = nil
		e.mu.Unlock()
	}
	for i := 0; i < 3 && h.wallet() < 3*unit/2; i++ {
		h.do(ActionReq{Action: "faucet"})
		h.mine(1)
	}
	if h.wallet() < 3*unit/2 {
		h.t.Fatalf("couldn't fund the test wallet (has %d)", h.wallet())
	}
}

func (h *harness) do(r ActionReq) Snapshot {
	h.t.Helper()
	r.Chain = h.chain
	if err := h.e.Do(r); err != nil {
		h.t.Fatalf("%s %+v: unexpected error: %v", h.chain, r, err)
	}
	return h.e.Snapshot()
}

// doUserErr expects the engine itself to refuse the action with a message.
func (h *harness) doUserErr(r ActionReq, contains string) {
	h.t.Helper()
	r.Chain = h.chain
	err := h.e.Do(r)
	if err == nil {
		h.t.Fatalf("%+v: expected a refusal containing %q, got none", r, contains)
	}
	if !strings.Contains(err.Error(), contains) {
		h.t.Fatalf("%+v: expected refusal containing %q, got %q", r, contains, err.Error())
	}
}

func (h *harness) mine(n int)      { h.t.Helper(); h.do(ActionReq{Action: "mine", N: n}) }
func (h *harness) skip(secs int64) { h.t.Helper(); h.do(ActionReq{Action: "mine", Secs: secs}) }
func (h *harness) snap() Snapshot  { return h.e.Snapshot() }
func (h *harness) wallet() int64   { s := h.snap(); return s.WalletSats + s.WalletPend }
func (h *harness) order(side string, sats int64) {
	h.t.Helper()
	h.do(ActionReq{Action: "order", Side: side, Sats: sats})
}

// lastTx is the most recent transaction the engine tried to send.
func (h *harness) lastTx() *TxInfo {
	h.t.Helper()
	s := h.snap()
	if len(s.Txs) == 0 {
		h.t.Fatal("no transactions yet")
	}
	return s.Txs[0]
}

func (h *harness) expectRejected(what, errContains string) {
	h.t.Helper()
	tx := h.lastTx()
	if tx.Status != "rejected" {
		h.t.Fatalf("%s: expected the chain to refuse it, but status is %q (%s)", what, tx.Status, tx.Label)
	}
	if errContains != "" && !strings.Contains(strings.ToLower(tx.Error), strings.ToLower(errContains)) {
		h.t.Fatalf("%s: refused for the wrong reason: %q (want %q)", what, tx.Error, errContains)
	}
	h.t.Logf("refused as expected: %s: %s", tx.Label, tx.Error)
}

func (h *harness) expectAccepted(what string) {
	h.t.Helper()
	tx := h.lastTx()
	if tx.Status == "rejected" {
		h.t.Fatalf("%s: refused: %s", what, tx.Error)
	}
}

func (h *harness) vault() *VaultView {
	h.t.Helper()
	v := h.snap().Vault
	if v == nil {
		h.t.Fatal("no vault")
	}
	return v
}

func (h *harness) expectStatus(want string) {
	h.t.Helper()
	if got := h.vault().Status; got != want {
		h.t.Fatalf("vault status %q, want %q", got, want)
	}
}

func (h *harness) hasEvent(sub string) bool {
	for _, e := range h.snap().Events {
		if strings.Contains(e.Title, sub) {
			return true
		}
	}
	return false
}

func (h *harness) expectEvent(sub string) {
	h.t.Helper()
	if !h.hasEvent(sub) {
		h.t.Fatalf("expected an activity log entry containing %q", sub)
	}
}

// approx checks got is within tol of want (all in base units).
func (h *harness) approx(what string, got, want, tol int64) {
	h.t.Helper()
	d := got - want
	if d < 0 {
		d = -d
	}
	if d > tol {
		h.t.Fatalf("%s: got %s, want %s (±%s)", what, fmtCoin(got, ""), fmtCoin(want, ""), fmtCoin(tol, ""))
	}
	h.t.Logf("%s: %s (expected about %s)", what, fmtCoin(got, h.e.Info().Ticker), fmtCoin(want, h.e.Info().Ticker))
}

const (
	oneCoin = unit
	ethGas  = 200_000 // 0.002 ETH: generous allowance for gas across a scenario
)

// ------------------------------------------------------------------ Bitcoin / Litecoin

func TestBitcoin(t *testing.T)  { runUTXOScenarios(t, "btc") }
func TestLitecoin(t *testing.T) { runUTXOScenarios(t, "ltc") }

func runUTXOScenarios(t *testing.T, chain string) {
	t.Run("NormalTradeAndSettle", func(t *testing.T) { utxoNormal(newHarness(t, chain)) })
	t.Run("ExchangeOfflineExitAndClaim", func(t *testing.T) { utxoOffline(newHarness(t, chain)) })
	t.Run("ReversalAttack", func(t *testing.T) { utxoReversal(newHarness(t, chain)) })
	t.Run("MaliciousOldStatePenaltySweep", func(t *testing.T) { utxoMaliciousOldState(newHarness(t, chain)) })
	t.Run("MaliciousOldStateWatchtowerOff", func(t *testing.T) { utxoMaliciousWatchtowerOff(newHarness(t, chain)) })
	t.Run("TheftRefused", func(t *testing.T) { utxoTheft(newHarness(t, chain)) })
}

func utxoLock(h *harness) {
	h.do(ActionReq{Action: "lock", Sats: oneCoin})
	h.expectStatus("funding")
	h.mine(1)
	h.expectStatus("open")
}

func utxoNormal(h *harness) {
	w0 := h.wallet()
	utxoLock(h)
	h.order("sell", 60_000_000)
	h.do(ActionReq{Action: "settle"})
	h.expectAccepted("cooperative settlement")
	h.mine(1)
	h.expectStatus("closed")
	if k := h.vault().CloseKind; k != "settled" {
		h.t.Fatalf("close kind %q, want settled", k)
	}
	// Lock fee + settlement fee; 0.4 comes back.
	h.approx("wallet change", h.wallet()-w0, -60_000_000-2*txFee, 1500)
}

func utxoOffline(h *harness) {
	w0 := h.wallet()
	utxoLock(h)
	h.do(ActionReq{Action: "mode", Mode: "offline"})
	h.do(ActionReq{Action: "settle"})
	h.expectEvent("Settlement request got no answer")
	h.do(ActionReq{Action: "exit"})
	h.expectAccepted("exit request")
	h.mine(1)
	h.expectStatus("exit_window")
	delay := int(h.vault().ClockTotal)

	h.do(ActionReq{Action: "claim"})
	h.expectRejected("claim right after the exit", "timelock")
	h.mine(delay - 2) // one block short of the window
	h.do(ActionReq{Action: "claim"})
	h.expectRejected("claim one block early", "timelock")
	h.mine(1)
	h.expectStatus("ready")
	h.do(ActionReq{Action: "claim"})
	h.expectAccepted("claim after the window")
	h.mine(1)
	h.expectStatus("closed")
	if k := h.vault().CloseKind; k != "claimed" {
		h.t.Fatalf("close kind %q, want claimed", k)
	}
	// Lock fee, exit fee, claim fee: everything else comes back.
	h.approx("wallet change", h.wallet()-w0, -3*txFee, 1500)
}

func utxoReversal(h *harness) {
	w0 := h.wallet()
	utxoLock(h)
	h.order("sell", 60_000_000)
	price := h.snap().Price
	h.do(ActionReq{Action: "price", USD: price * 2}) // the price doubles
	h.do(ActionReq{Action: "exit"})                  // the user tries to leave with the coins
	h.mine(1)                                        // exit confirms; the exchange answers
	h.expectEvent("Exchange watchtower published state #1")
	h.doUserErr(ActionReq{Action: "claim"}, "already closed")
	h.mine(1)
	h.expectStatus("closed")
	if k := h.vault().CloseKind; k != "published" {
		h.t.Fatalf("close kind %q, want published", k)
	}
	h.finishPenaltyWindow()
	// The sale stands: only 0.4 comes back (less lock, exit and payout fees).
	h.approx("wallet change", h.wallet()-w0, -60_000_000-3*txFee, 1500)
}

func utxoTheft(h *harness) {
	w0 := h.wallet()
	utxoLock(h)
	h.do(ActionReq{Action: "steal"})
	h.expectRejected("exchange spending the vault alone", "signature")
	h.do(ActionReq{Action: "mode", Mode: "offline"})
	h.do(ActionReq{Action: "exit"})
	h.mine(1)
	h.do(ActionReq{Action: "steal"})
	h.expectRejected("exchange spending the exit output alone", "signature")
	h.mine(int(h.vault().ClockTotal))
	h.do(ActionReq{Action: "claim"})
	h.expectAccepted("claim after the window")
	h.mine(1)
	h.approx("wallet change", h.wallet()-w0, -3*txFee, 1500)
}

// ------------------------------------------------------------------ Ethereum

func TestEthereum(t *testing.T) {
	t.Run("NormalTradeAndSettle", func(t *testing.T) { ethNormal(newHarness(t, "eth")) })
	t.Run("ExchangeOfflineExitAndClaim", func(t *testing.T) { ethOffline(newHarness(t, "eth")) })
	t.Run("ReversalAttack", func(t *testing.T) { ethReversal(newHarness(t, "eth")) })
	t.Run("MaliciousOldStateWatchtowerOn", func(t *testing.T) { ethMalicious(newHarness(t, "eth"), true) })
	t.Run("MaliciousOldStateWatchtowerOff", func(t *testing.T) { ethMalicious(newHarness(t, "eth"), false) })
	t.Run("TheftRefused", func(t *testing.T) { ethTheft(newHarness(t, "eth")) })
	t.Run("BuyingCapacity", func(t *testing.T) { ethCapacity(newHarness(t, "eth")) })
	t.Run("WatchtowerKeepsBoughtETH", func(t *testing.T) { ethCapacityExit(newHarness(t, "eth")) })
}

func ethLock(h *harness) {
	h.do(ActionReq{Action: "lock", Sats: oneCoin})
	h.expectAccepted("lock")
	h.expectStatus("funding")
	h.mine(1)
	h.expectStatus("open")
}

// ethWithin checks a wallet change is `want` less at most ethGas of gas.
func (h *harness) ethWithin(what string, got, want int64) {
	h.t.Helper()
	if got > want || got < want-ethGas {
		h.t.Fatalf("%s: got %s, want %s minus gas (at most %s)", what, fmtCoin(got, "ETH"), fmtCoin(want, "ETH"), fmtCoin(ethGas, "ETH"))
	}
	h.t.Logf("%s: %s (expected %s minus gas)", what, fmtCoin(got, "ETH"), fmtCoin(want, "ETH"))
}

func ethNormal(h *harness) {
	w0 := h.wallet()
	ethLock(h)
	h.order("sell", 60_000_000)
	h.do(ActionReq{Action: "settle"})
	h.expectAccepted("settle")
	h.mine(1)
	h.expectStatus("closed")
	h.ethWithin("wallet change", h.wallet()-w0, -60_000_000)
}

func ethOffline(h *harness) {
	w0 := h.wallet()
	ethLock(h)
	h.do(ActionReq{Action: "mode", Mode: "offline"})
	h.do(ActionReq{Action: "exit"})
	h.mine(1)
	h.expectStatus("exit_window")
	h.do(ActionReq{Action: "claim"})
	h.expectRejected("payout during the window", "window")
	h.skip(3600)
	h.do(ActionReq{Action: "claim"})
	h.expectRejected("payout after 1 hour", "window")
	h.skip(86400)
	h.expectStatus("ready")
	h.do(ActionReq{Action: "claim"})
	h.expectAccepted("payout after the window")
	h.mine(1)
	h.expectStatus("closed")
	h.ethWithin("wallet change", h.wallet()-w0, 0)
}

func ethReversal(h *harness) {
	w0 := h.wallet()
	ethLock(h)
	h.order("sell", 60_000_000)
	h.do(ActionReq{Action: "price", USD: h.snap().Price * 2})
	h.do(ActionReq{Action: "exit"})
	h.mine(1) // exit confirms; the honest exchange submits state #1
	h.expectEvent("Exchange submitted state #1")
	h.mine(1)
	if b := h.vault().BestState; b != 1 {
		h.t.Fatalf("best submitted state %d, want 1", b)
	}
	h.skip(86400)
	h.do(ActionReq{Action: "claim"})
	h.mine(1)
	h.expectStatus("closed")
	h.ethWithin("wallet change", h.wallet()-w0, -60_000_000)
}

func ethMalicious(h *harness, watchtower bool) {
	w0 := h.wallet()
	ethLock(h)
	h.order("sell", 50_000_000)
	h.order("buy", 50_000_000)
	if !watchtower {
		h.do(ActionReq{Action: "watchtower", On: false})
	}
	h.do(ActionReq{Action: "mode", Mode: "malicious"})
	h.do(ActionReq{Action: "settle"})
	h.expectEvent("refused to settle")
	h.do(ActionReq{Action: "exit"})
	h.mine(1) // exit confirms; the exchange submits old state #1
	h.expectEvent("Malicious exchange submitted an OLD state (#1, not #2)")
	h.mine(1) // old state confirms; the watchtower (if on) answers with #2
	h.mine(1)
	want, best := int64(0), 2
	if !watchtower {
		want, best = -50_000_000, 1
	} else {
		h.expectEvent("Your watchtower spotted an old state")
	}
	if b := h.vault().BestState; b != best {
		h.t.Fatalf("best submitted state %d, want %d", b, best)
	}
	h.skip(86400)
	h.do(ActionReq{Action: "claim"})
	h.mine(1)
	h.expectStatus("closed")
	h.ethWithin("wallet change", h.wallet()-w0, want)
}

func ethTheft(h *harness) {
	w0 := h.wallet()
	ethLock(h)
	h.do(ActionReq{Action: "steal"})
	h.expectRejected("exchange settling alone", "user signature")
	h.do(ActionReq{Action: "mode", Mode: "offline"})
	h.do(ActionReq{Action: "exit"})
	h.mine(1)
	h.do(ActionReq{Action: "steal"})
	h.expectRejected("exchange submitting a state alone", "user signature")
	h.skip(86400)
	h.do(ActionReq{Action: "claim"})
	h.mine(1)
	h.expectStatus("closed")
	h.ethWithin("wallet change", h.wallet()-w0, 0)
}

func ethFundCapacityAndBuy(h *harness) {
	ethLock(h)
	h.do(ActionReq{Action: "capacity", Sats: oneCoin})
	h.expectAccepted("exchange deposit")
	h.mine(1)
	v := h.vault()
	if v.ExchDeposit != oneCoin || v.Locked != 2*oneCoin {
		h.t.Fatalf("after funding: exchange deposit %d, locked %d", v.ExchDeposit, v.Locked)
	}
	h.do(ActionReq{Action: "cash"})
	h.order("buy", 80_000_000)
	if u := h.vault().UserSats; u != 179_800_000 {
		h.t.Fatalf("your share after buying: %s, want 1.798", fmtCoin(u, "ETH"))
	}
}

func ethCapacity(h *harness) {
	ethFundCapacityAndBuy(h)
	w1 := h.wallet()
	h.do(ActionReq{Action: "settle"})
	h.mine(1)
	h.expectStatus("closed")
	h.ethWithin("paid back on settlement", h.wallet()-w1, 179_800_000)
}

func ethCapacityExit(h *harness) {
	ethFundCapacityAndBuy(h)
	w1 := h.wallet()
	h.do(ActionReq{Action: "mode", Mode: "offline"})
	h.do(ActionReq{Action: "exit"})
	h.mine(1) // exit confirms; nobody else submits, so your watchtower does
	h.expectEvent("Your watchtower is submitting your latest state")
	h.mine(1)
	if b := h.vault().BestState; b != 2 {
		h.t.Fatalf("best submitted state %d, want 2", b)
	}
	h.skip(86400)
	h.do(ActionReq{Action: "claim"})
	h.mine(1)
	h.expectStatus("closed")
	h.ethWithin("paid back after the window", h.wallet()-w1, 179_800_000)
}

// ------------------------------------------------------------------ revocation (Bitcoin / Litecoin)

// utxoEngine gives tests direct access to the UTXO engine.
func (h *harness) utxoEngine() *UTXOEngine { return h.e.(*UTXOEngine) }

// tryUTXO builds a transaction from the current vault and sends it to the
// node, for checking what the scripts themselves allow.
func (h *harness) tryUTXO(label string, build func(v *Vault) *wire.MsgTx) {
	e := h.utxoEngine()
	e.mu.Lock()
	defer e.mu.Unlock()
	e.broadcast(build(e.vault), label, "Built by the test", nil, label+" accepted", label+" rejected")
}

func (h *harness) published() *State {
	h.t.Helper()
	e := h.utxoEngine()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.vault == nil || e.vault.published == nil {
		h.t.Fatal("no state has been published")
	}
	return e.vault.published
}

// finishPenaltyWindow runs after the exchange published the LATEST state. It
// checks the rules on the exchange's share R_n: the user can't sweep it
// without the real secret, the exchange can't take it before D2, and the
// exchange does take it once D2 has passed.
func (h *harness) finishPenaltyWindow() {
	h.t.Helper()
	v := h.vault()
	s := h.published()
	if v.PublishedState != s.Index || s.revealed {
		h.t.Fatalf("published state #%d should be the latest, unrevoked state", v.PublishedState)
	}
	if v.States[s.Index].Secret != "" {
		h.t.Fatal("the user should not hold the secret for the latest state")
	}
	h.tryUTXO("Sweep with a made-up secret", func(vt *Vault) *wire.MsgTx {
		return vt.penaltySweep(s, bytes.Repeat([]byte{7}, 32))
	})
	h.expectRejected("penalty sweep without the real secret", "")
	h.tryUTXO("Exchange collects too early", func(vt *Vault) *wire.MsgTx { return vt.exchangeClaim(s) })
	h.expectRejected("exchange collecting before D2", "timelock")

	exch0 := h.snap().ExchSats
	h.mine(int(v.PenaltyDelay)) // exchangeCollect fires once D2 has passed
	h.expectEvent(fmt.Sprintf("The exchange claimed its %s share", fmtCoin(s.rAmt, h.e.Info().Ticker)))
	h.mine(1)
	if k := h.vault().SweepKind; k != "exchange" {
		h.t.Fatalf("sweep kind %q, want exchange", k)
	}
	if h.hasEvent("Penalty sweep") {
		h.t.Fatal("no penalty sweep should happen for the latest state")
	}
	h.approx("exchange collected", h.snap().ExchSats-exch0, s.rAmt-txFee, 0)
}

// maliciousSetup: sell 0.5, buy it back, the exchange turns malicious, the
// user requests an exit, and the exchange publishes the older "sold 0.5"
// state (#1) instead of the latest (#2).
func maliciousSetup(h *harness) {
	utxoLock(h)
	h.order("sell", 50_000_000)
	h.order("buy", 50_000_000)
	h.expectEvent("The exchange handed you revocation secret #1")
	v := h.vault()
	if v.States[0].Secret == "" || v.States[1].Secret == "" || v.States[2].Secret != "" {
		h.t.Fatal("the user should hold secrets #0 and #1 but not #2")
	}
	h.do(ActionReq{Action: "mode", Mode: "malicious"})
	h.do(ActionReq{Action: "settle"})
	h.expectEvent("refused to co-sign your settlement")
	h.do(ActionReq{Action: "exit"})
	h.mine(1) // exit confirms; the exchange publishes old state #1
	h.expectEvent("Malicious exchange published an OLD state (#1, not #2)")
	h.expectEvent("You hold revocation secret #1 for that state")
}

// utxoMaliciousOldState: with revocation, publishing the old state costs the
// exchange its whole share, so the user ends up with the entire vault.
func utxoMaliciousOldState(h *harness) {
	w0, exch0 := h.wallet(), h.snap().ExchSats
	maliciousSetup(h)
	h.mine(1) // old state confirms; the watchtower sweeps with secret #1
	h.expectEvent("Your watchtower spotted revoked state #1")
	h.expectEvent("Penalty sweep: your watchtower took the exchange's")
	h.expectAccepted("penalty sweep")
	h.mine(1)
	v := h.vault()
	if v.Status != "closed" || v.SweepKind != "penalty" {
		h.t.Fatalf("status %q, sweep %q; want closed and penalty", v.Status, v.SweepKind)
	}
	// The exchange can't collect afterwards: mine past D2 and check.
	h.mine(int(v.PenaltyDelay) + 1)
	if h.hasEvent("The exchange claimed its") {
		h.t.Fatal("the exchange collected its share after a penalty sweep")
	}
	// The whole vault, less four flat fees (lock, exit, state payout, sweep).
	h.approx("wallet change", h.wallet()-w0, -4*txFee, 1500)
	h.approx("exchange wallet change", h.snap().ExchSats-exch0, 0, 0)
}

// utxoMaliciousWatchtowerOff: nobody uses the secret, so after D2 the
// exchange collects its share and the old state stands.
func utxoMaliciousWatchtowerOff(h *harness) {
	w0 := h.wallet()
	h.do(ActionReq{Action: "watchtower", On: false})
	maliciousSetup(h)
	h.mine(1)
	if h.hasEvent("Penalty sweep") {
		h.t.Fatal("penalty sweep happened with the watchtower off")
	}
	h.mine(int(h.vault().PenaltyDelay))
	h.expectEvent("The exchange claimed its")
	h.expectEvent("Nobody used your revocation secret in time")
	h.mine(1)
	if k := h.vault().SweepKind; k != "exchange" {
		h.t.Fatalf("sweep kind %q, want exchange", k)
	}
	// Turning the watchtower on now is too late: the share is gone.
	h.do(ActionReq{Action: "watchtower", On: true})
	h.mine(1)
	if h.hasEvent("Penalty sweep") {
		h.t.Fatal("penalty sweep after the exchange already collected")
	}
	h.approx("wallet change", h.wallet()-w0, -50_000_000-3*txFee, 1500)
}
