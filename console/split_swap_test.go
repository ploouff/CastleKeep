package main

// Swaps in the separate programs, across Bitcoin, Litecoin and Ethereum:
// one exchange server over HTTP, one wallet, three test chains.

import (
	"net/http/httptest"
	"testing"
)

type splitAll struct {
	*splitEnv
	btc, ltc *UTXOEngine
	ee       *EthEngine
}

func newSplitAll(t *testing.T) *splitAll {
	btc := engine(t, "btc").(*UTXOEngine)
	ltc := engine(t, "ltc").(*UTXOEngine)
	ee := engine(t, "eth").(*EthEngine)
	cfgs := map[string]UTXOConfig{"btc": btc.cfg, "ltc": ltc.cfg}
	rpc := map[string]string{"btc": btc.RPCAddr(), "ltc": ltc.RPCAddr()}
	x, err := NewExchange(cfgs, rpc)
	if err != nil {
		t.Fatal(err)
	}
	xa := x.AttachEth(ee.Endpoint(), ee.cfg.DefaultPrice)
	srv := httptest.NewServer(x.Handler())
	t.Cleanup(srv.Close)
	w, err := NewWallet(srv.URL, cfgs, rpc)
	if err != nil {
		t.Fatal(err)
	}
	wa := w.AttachEth(ee.Endpoint())
	for id, e := range map[string]*UTXOEngine{"btc": btc, "ltc": ltc} {
		op, err := e.Faucet(x.WalletScript(id), 10*unit)
		if err != nil {
			t.Fatal(err)
		}
		x.AddCoins(id, op, 10*unit)
		if op, err = e.Faucet(w.WalletScript(id), 2*unit); err != nil {
			t.Fatal(err)
		}
		w.AddCoins(id, op, 2*unit)
	}
	if err := ee.FundAddress(xa, 10*unit); err != nil {
		t.Fatal(err)
	}
	if err := ee.FundAddress(wa, 2*unit); err != nil {
		t.Fatal(err)
	}
	s := &splitAll{splitEnv: &splitEnv{t: t, x: x, w: w}, btc: btc, ltc: ltc, ee: ee}
	s.mineAll()
	return s
}

func (s *splitAll) mineAll() {
	s.t.Helper()
	for _, e := range []*UTXOEngine{s.btc, s.ltc} {
		if err := e.Mine(1); err != nil {
			s.t.Fatal(err)
		}
	}
	if err := s.ee.MineEth(1, 0); err != nil {
		s.t.Fatal(err)
	}
	s.x.Tick()
	s.w.Tick()
}

func (s *splitAll) share(chain string) int64 {
	if chain == "eth" {
		u, _, _, _ := s.w.EthVault()
		return u
	}
	u, _, _, _, _ := s.w.Vault(chain)
	return u
}

func (s *splitAll) states(chain string) int {
	if chain == "eth" {
		_, _, n, _ := s.w.EthVault()
		return n
	}
	_, _, n, _, _ := s.w.Vault(chain)
	return n
}

func (s *splitAll) expectShare(chain string, want int64) {
	s.t.Helper()
	if got := s.share(chain); got != want {
		s.t.Fatalf("%s share %s, want %s", chain, fmtCoin(got, ""), fmtCoin(want, ""))
	}
}

func TestSplitSwap(t *testing.T) {
	s := newSplitAll(t)
	s.ok(s.w.Open("btc", oneCoin))
	s.ok(s.w.Open("ltc", oneCoin))
	s.ok(s.w.EthOpen(oneCoin))
	s.mineAll()
	s.ok(s.w.EthCapacity(oneCoin))
	s.ok(s.w.Capacity("ltc", oneCoin))
	s.mineAll()
	ethShare := oneCoin - capacityFee(oneCoin)
	ltcShare := oneCoin - capacityFee(oneCoin)
	s.expectShare("eth", ethShare)
	s.expectShare("ltc", ltcShare)
	cash0 := s.x.Cash(s.w.Account)

	t.Run("BTCForETH", func(t *testing.T) {
		s.t = t
		// 0.04 BTC at $65,000 = $2,600 = 0.86666666 ETH at $3,000.
		s.ok(s.w.Swap("btc", "eth", 4_000_000))
		s.expectShare("btc", 96_000_000)
		s.expectShare("eth", ethShare+86_666_666)
		if c := s.x.Cash(s.w.Account) - cash0; c < 0 || c > 1 {
			t.Fatalf("a completed swap should leave cash unchanged (got %d cents)", c)
		}
	})

	t.Run("ETHForNewLTC", func(t *testing.T) {
		s.t = t
		// 0.01 ETH at $3,000 = $30 = 0.3 LTC at $100, from the exchange's LTC.
		s.ok(s.w.Swap("eth", "ltc", 1_000_000))
		s.expectShare("ltc", ltcShare+30_000_000)
		s.expectShare("eth", ethShare+86_666_666-1_000_000)
	})

	t.Run("LTCForBTCBuyBack", func(t *testing.T) {
		s.t = t
		// 0.65 LTC at $100 = $65 = 0.001 BTC at $65,000 (you sold 0.04 BTC above).
		before := s.share("btc")
		s.ok(s.w.Swap("ltc", "btc", 65_000_000))
		s.expectShare("btc", before+100_000)
	})

	t.Run("RefusedSwapChangesNothing", func(t *testing.T) {
		s.t = t
		nb, ne, cash := s.states("btc"), s.states("eth"), s.x.Cash(s.w.Account)
		// 0.5 BTC = $32,500 = 10.8 ETH, far more than the exchange's ETH side.
		s.refused(s.w.Swap("btc", "eth", 50_000_000), "Can't buy")
		s.refused(s.w.Swap("btc", "btc", 1000), "two different")
		if s.states("btc") != nb || s.states("eth") != ne || s.x.Cash(s.w.Account) != cash {
			t.Fatal("a refused swap changed something")
		}
	})

	t.Run("BadSignatureOnBuyingSideNothingSigned", func(t *testing.T) {
		s.t = t
		nb, ne := s.states("btc"), s.states("eth")
		s.x.SetTamper()
		s.refused(s.w.Swap("btc", "eth", 500_000), "doesn't check out")
		if s.states("btc") != nb || s.states("eth") != ne {
			t.Fatal("the wallet signed something after a bad signature")
		}
	})

	t.Run("TradingContinuesAndSettles", func(t *testing.T) {
		s.t = t
		s.ok(s.w.Order("btc", "sell", 1_000_000))
		s.ok(s.w.Order("ltc", "sell", 1_000_000))
		s.ok(s.w.Settle("btc"))
		s.ok(s.w.Settle("ltc"))
		s.ok(s.w.EthSettle())
		s.mineAll()
		for _, c := range []string{"btc", "ltc"} {
			if _, _, _, closed, _ := s.w.Vault(c); closed != "settled" {
				t.Fatalf("%s vault: %q", c, closed)
			}
		}
		if _, _, _, closed := s.w.EthVault(); closed != "settled" {
			t.Fatalf("eth vault: %q", closed)
		}
	})
}
