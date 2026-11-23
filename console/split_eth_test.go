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

func newSplitEth(t *testing.T) *splitEth {
	ee := engine(t, "eth").(*EthEngine)
	x, err := NewExchange(map[string]UTXOConfig{}, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	ep := ee.Endpoint()
	xa := x.AttachEth(ep, ee.cfg.DefaultPrice)
	srv := httptest.NewServer(x.Handler())
	t.Cleanup(srv.Close)
	w, err := NewWallet(srv.URL, map[string]UTXOConfig{}, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	wa := w.AttachEth(ep)
	if err := ee.FundAddress(xa, 10*unit); err != nil {
		t.Fatal(err)
	}
	if err := ee.FundAddress(wa, 2*unit); err != nil {
		t.Fatal(err)
	}
	s := &splitEth{splitEnv: &splitEnv{t: t, chain: "eth", x: x, w: w}, ee: ee}
	s.mine(1)
	s.start = w.EthBalance()
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
	got := s.w.EthBalance() - s.start
	if got > want || got < want-ethGas {
		s.t.Fatalf("%s: wallet change %s, want %s minus gas", what, fmtCoin(got, "ETH"), fmtCoin(want, "ETH"))
	}
	s.t.Logf("%s: wallet change %s", what, fmtCoin(got, "ETH"))
}

func (s *splitEth) open() {
	s.t.Helper()
	s.ok(s.w.EthOpen(oneCoin))
	s.refused(s.w.EthOrder("sell", 1000), "hasn't confirmed")
	s.mine(1) // the deposit confirms; the wallet agrees state #0 with the exchange
	if _, _, n, _ := s.w.EthVault(); n != 1 {
		s.t.Fatalf("state #0 wasn't agreed (%d states)", n)
	}
}

func (s *splitEth) finish() {
	s.t.Helper()
	s.skipDay()
	s.ok(s.w.EthClaim())
	s.mine(1)
}

func TestSplitEthereum(t *testing.T) {
	t.Run("NormalTradeAndSettle", func(t *testing.T) {
		s := newSplitEth(t)
		s.open()
		s.ok(s.w.EthOrder("sell", 60_000_000))
		s.ok(s.w.EthSettle())
		s.mine(1)
		s.within("after settling", -60_000_000)
	})

	t.Run("NoTradingAfterSettleSignature", func(t *testing.T) {
		s := newSplitEth(t)
		s.open()
		// The exchange gave a settle signature for state #0. If it kept
		// trading, the wallet could settle later with that old split.
		we, v := s.w.eth, s.w.eth.vault
		d := we.c.settleDigest(v.id, 0, v.latest().UserSats)
		s.ok(call(s.w.base, "/v1/eth/settle", EthSettleReq{v.id.String(), signDigest(we.key, d)}, &EthSettleResp{}))
		s.refused(s.w.EthOrder("sell", 10_000_000), "settlement was signed")
	})

	t.Run("ExchangeOfflineExitAndPayout", func(t *testing.T) {
		s := newSplitEth(t)
		s.open()
		s.ok(s.w.Admin("eth", "offline", 0))
		s.refused(s.w.EthSettle(), "offline")
		s.ok(s.w.EthExit())
		s.mine(1)
		s.refused(s.w.EthClaim(), "still running")
		s.finish()
		s.within("deposit back", 0)
	})

	t.Run("ReversalAttackExchangeSubmits", func(t *testing.T) {
		s := newSplitEth(t)
		s.open()
		s.ok(s.w.EthOrder("sell", 60_000_000))
		s.ok(s.w.EthExit())
		s.mine(2) // the exit confirms, then the exchange's submission does
		s.finish()
		s.within("the sale stands", -60_000_000)
	})

	t.Run("MaliciousOldStateWatchtowerWins", func(t *testing.T) {
		s := newSplitEth(t)
		s.open()
		s.ok(s.w.EthOrder("sell", 50_000_000))
		for s.x.Cash(s.w.Account) < orderCost(50_000_000, s.x.eth.price) {
			_, err := s.w.AddCash()
			s.ok(err)
		}
		s.ok(s.w.EthOrder("buy", 50_000_000))
		s.ok(s.w.Admin("eth", "malicious", 0))
		s.refused(s.w.EthSettle(), "won't co-sign")
		s.ok(s.w.EthExit())
		s.mine(3) // exit; the exchange submits old #1; the wallet submits #2
		s.finish()
		s.within("whole vault back", 0)
	})

	t.Run("BuyNewETHAndKeepItWhenExchangeVanishes", func(t *testing.T) {
		s := newSplitEth(t)
		s.open()
		s.ok(s.w.EthCapacity(oneCoin))
		s.mine(1) // deposit confirms; the wallet agrees the new split
		if u, e, _, _ := s.w.EthVault(); u != oneCoin-capacityFee(oneCoin) || e != oneCoin+capacityFee(oneCoin) {
			t.Fatalf("split after the exchange's deposit: %d / %d", u, e)
		}
		for s.x.Cash(s.w.Account) < orderCost(80_000_000, s.x.eth.price) {
			_, err := s.w.AddCash()
			s.ok(err)
		}
		s.ok(s.w.EthOrder("buy", 80_000_000))
		s.ok(s.w.Admin("eth", "offline", 0))
		s.ok(s.w.EthExit())
		s.mine(2) // the exit confirms; the wallet submits its latest state
		s.finish()
		s.within("kept the bought ETH", 79_800_000)
	})

	t.Run("BadSignaturesRefused", func(t *testing.T) {
		s := newSplitEth(t)
		s.open()
		// A bad wallet signature: the exchange refuses it.
		v := s.w.eth.vault
		var off EthStateOffer
		s.ok(call(s.w.base, "/v1/eth/order", EthOrderReq{v.id.String(), "sell", 10_000_000}, &off))
		err := call(s.w.base, "/v1/eth/commit", EthCommitReq{v.id.String(), off.Nonce, make([]byte, 65)}, nil)
		s.refused(err, "doesn't check out")
		// A bad exchange signature: the wallet stops trading.
		s.x.SetTamper()
		s.refused(s.w.EthOrder("sell", 10_000_000), "doesn't check out")
		s.refused(s.w.EthOrder("sell", 10_000_000), "Not trading")
		if _, _, n, _ := s.w.EthVault(); n != 1 {
			t.Fatalf("%d states; the tampered one must not count", n)
		}
	})
}
