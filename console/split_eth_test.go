package main

// Ethereum in the separate exchange and wallet programs: the exchange runs
// as a real HTTP server, the wallet talks to it over HTTP, and both use the
// CastleVault contract on the Hardhat node with their own keys.
// Skipped if no Ethereum node is reachable. Test networks only.

import (
	"net/http/httptest"
	"testing"
)

type splitEth struct {
	*splitEnv
	ee    *EthEngine
	start int64
}

func newSplitEth(t *testing.T, chain string) *splitEth {
	ee := engine(t, chain).(*EthEngine)
	x, err := NewExchange(map[string]UTXOConfig{}, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	ep := ee.Endpoint()
	xa := x.AttachEVM(chain, ep, ee.cfg.DefaultPrice)
	srv := httptest.NewServer(x.Handler())
	t.Cleanup(srv.Close)
	w, err := NewWallet(srv.URL, map[string]UTXOConfig{}, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	wa := w.AttachEVM(chain, ep)
	if err := ee.FundAddress(xa, 10*unit); err != nil {
		t.Fatal(err)
	}
	if err := ee.FundAddress(wa, 2*unit); err != nil {
		t.Fatal(err)
	}
	s := &splitEth{splitEnv: &splitEnv{t: t, chain: chain, x: x, w: w}, ee: ee}
	s.mine(1)
	s.start = w.EthBalance(chain)
	return s
}

func (s *splitEth) mine(n int) {
	s.t.Helper()
	for i := 0; i < n; i++ {
		if err := s.ee.MineEth(1, 0); err != nil {
			s.t.Fatal(err)
		}
		s.x.Tick()
		s.w.Tick()
	}
}

func (s *splitEth) skipDay() {
	s.t.Helper()
	if err := s.ee.MineEth(0, 86400); err != nil {
		s.t.Fatal(err)
	}
	s.x.Tick()
	s.w.Tick()
}

// within: the wallet changed by want, less at most ethGas of gas.
func (s *splitEth) within(what string, want int64) {
	s.t.Helper()
	got := s.w.EthBalance(s.chain) - s.start
	if got > want || got < want-ethGas {
		s.t.Fatalf("%s: wallet change %s, want %s minus gas", what, fmtCoin(got, s.x.evm[s.chain].ticker), fmtCoin(want, s.x.evm[s.chain].ticker))
	}
	s.t.Logf("%s: wallet change %s", what, fmtCoin(got, s.x.evm[s.chain].ticker))
}

func (s *splitEth) open() {
	s.t.Helper()
	s.ok(s.w.EthOpen(s.chain, oneCoin))
	s.refused(s.w.EthOrder(s.chain, "sell", 1000), "hasn't confirmed")
	s.mine(1) // the deposit confirms; the wallet agrees state #0 with the exchange
	if _, _, n, _ := s.w.EthVault(s.chain); n != 1 {
		s.t.Fatalf("state #0 wasn't agreed (%d states)", n)
	}
}

func (s *splitEth) finish() {
	s.t.Helper()
	s.skipDay()
	s.ok(s.w.EthClaim(s.chain))
	s.mine(1)
}

func TestSplitEthereum(t *testing.T)  { runSplitEVM(t, "eth") }
func TestSplitPolygon(t *testing.T)   { runSplitEVM(t, "pol") }
func TestSplitBase(t *testing.T)      { runSplitEVM(t, "base") }
func TestSplitBNBChain(t *testing.T)  { runSplitEVM(t, "bnb") }
func TestSplitAvalanche(t *testing.T) { runSplitEVM(t, "avax") }

func runSplitEVM(t *testing.T, chain string) {
	t.Run("NormalTradeAndSettle", func(t *testing.T) {
		s := newSplitEth(t, chain)
		s.open()
		s.ok(s.w.EthOrder(s.chain, "sell", 60_000_000))
		s.ok(s.w.EthSettle(s.chain))
		s.mine(1)
		s.within("after settling", -60_000_000)
	})

	t.Run("NoTradingAfterSettleSignature", func(t *testing.T) {
		s := newSplitEth(t, chain)
		s.open()
		// The exchange gave a settle signature for state #0. If it kept
		// trading, the wallet could settle later with that old split.
		we, v := s.w.evm[s.chain], s.w.evm[s.chain].vault
		d := we.c.settleDigest(v.id, 0, v.latest().UserSats)
		s.ok(call(s.w.base, "/v1/eth/settle", EthSettleReq{v.id.String(), signDigest(we.key, d), s.chain}, &EthSettleResp{}))
		s.refused(s.w.EthOrder(s.chain, "sell", 10_000_000), "settlement was signed")
	})

	t.Run("ExchangeOfflineExitAndPayout", func(t *testing.T) {
		s := newSplitEth(t, chain)
		s.open()
		s.ok(s.w.Admin(s.chain, "offline", 0))
		s.refused(s.w.EthSettle(s.chain), "offline")
		s.ok(s.w.EthExit(s.chain))
		s.mine(1)
		s.refused(s.w.EthClaim(s.chain), "still running")
		s.finish()
		s.within("deposit back", 0)
	})

	t.Run("ReversalAttackExchangeSubmits", func(t *testing.T) {
		s := newSplitEth(t, chain)
		s.open()
		s.ok(s.w.EthOrder(s.chain, "sell", 60_000_000))
		s.ok(s.w.EthExit(s.chain))
		s.mine(2) // the exit confirms, then the exchange's submission does
		s.finish()
		s.within("the sale stands", -60_000_000)
	})

	t.Run("MaliciousOldStateWatchtowerWins", func(t *testing.T) {
		s := newSplitEth(t, chain)
		s.open()
		s.ok(s.w.EthOrder(s.chain, "sell", 50_000_000))
		for s.x.Cash(s.w.Account) < orderCost(50_000_000, s.x.evm[s.chain].price) {
			_, err := s.w.AddCash()
			s.ok(err)
		}
		s.ok(s.w.EthOrder(s.chain, "buy", 50_000_000))
		s.ok(s.w.Admin(s.chain, "malicious", 0))
		s.refused(s.w.EthSettle(s.chain), "won't co-sign")
		s.ok(s.w.EthExit(s.chain))
		s.mine(3) // exit; the exchange submits old #1; the wallet submits #2
		s.finish()
		s.within("whole vault back", 0)
	})

	t.Run("BuyNewETHAndKeepItWhenExchangeVanishes", func(t *testing.T) {
		s := newSplitEth(t, chain)
		s.open()
		s.ok(s.w.EthCapacity(s.chain, oneCoin))
		s.mine(1) // deposit confirms; the wallet agrees the new split
		if u, e, _, _ := s.w.EthVault(s.chain); u != oneCoin-capacityFee(oneCoin) || e != oneCoin+capacityFee(oneCoin) {
			t.Fatalf("split after the exchange's deposit: %d / %d", u, e)
		}
		// Ask again for more; a second request waits for the first.
		s.ok(s.w.EthCapacity(s.chain, oneCoin/2))
		s.refused(s.w.EthCapacity(s.chain, oneCoin/2), "still waiting for a block")
		s.mine(1)
		if u, _, _, _ := s.w.EthVault(s.chain); u != oneCoin-capacityFee(oneCoin)-capacityFee(oneCoin/2) {
			t.Fatalf("your share after a second top-up: %d", u)
		}
		for s.x.Cash(s.w.Account) < orderCost(80_000_000, s.x.evm[s.chain].price) {
			_, err := s.w.AddCash()
			s.ok(err)
		}
		s.ok(s.w.EthOrder(s.chain, "buy", 80_000_000))
		s.ok(s.w.Admin(s.chain, "offline", 0))
		s.ok(s.w.EthExit(s.chain))
		s.mine(2) // the exit confirms; the wallet submits its latest state
		s.finish()
		s.within("kept the bought ETH (less two capacity fees)", 79_800_000-capacityFee(oneCoin/2))
	})

	t.Run("BadSignaturesRefused", func(t *testing.T) {
		s := newSplitEth(t, chain)
		s.open()
		// A bad wallet signature: the exchange refuses it.
		v := s.w.evm[s.chain].vault
		var off EthStateOffer
		s.ok(call(s.w.base, "/v1/eth/order", EthOrderReq{v.id.String(), "sell", 10_000_000, s.chain}, &off))
		err := call(s.w.base, "/v1/eth/commit", EthCommitReq{v.id.String(), off.Nonce, make([]byte, 65), s.chain}, nil)
		s.refused(err, "doesn't check out")
		// A bad exchange signature: the wallet stops trading.
		s.x.SetTamper()
		s.refused(s.w.EthOrder(s.chain, "sell", 10_000_000), "doesn't check out")
		s.refused(s.w.EthOrder(s.chain, "sell", 10_000_000), "Not trading")
		if _, _, n, _ := s.w.EthVault(s.chain); n != 1 {
			t.Fatalf("%d states; the tampered one must not count", n)
		}
	})
}
