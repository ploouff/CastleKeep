package main

// Tests for the separate exchange and wallet programs (Bitcoin and
// Litecoin). The exchange runs as a real HTTP server; the wallet talks to it
// over HTTP and both read the same local test node. Neither side has the
// other's private keys. Test networks only.

import (
	"net/http/httptest"
	"strings"
	"testing"
)

type splitEnv struct {
	t     *testing.T
	chain string
	eng   *UTXOEngine
	x     *Exchange
	w     *Wallet
	start int64
}

func newSplit(t *testing.T, chain string) *splitEnv {
	e := engine(t, chain).(*UTXOEngine)
	cfgs := map[string]UTXOConfig{chain: e.cfg}
	rpc := map[string]string{chain: e.RPCAddr()}
	x, err := NewExchange(cfgs, rpc)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(x.Handler())
	t.Cleanup(srv.Close)
	w, err := NewWallet(srv.URL, cfgs, rpc)
	if err != nil {
		t.Fatal(err)
	}
	s := &splitEnv{t: t, chain: chain, eng: e, x: x, w: w}
	op, err := e.Faucet(x.WalletScript(chain), 10*unit)
	if err != nil {
		t.Fatal(err)
	}
	x.AddCoins(chain, op, 10*unit)
	op, err = e.Faucet(w.WalletScript(chain), 2*unit)
	if err != nil {
		t.Fatal(err)
	}
	w.AddCoins(chain, op, 2*unit)
	s.mine(1)
	s.start = w.Balance(chain)
	return s
}

// mine mines n blocks one at a time, letting both watchtowers look after each.
func (s *splitEnv) mine(n int) {
	s.t.Helper()
	for i := 0; i < n; i++ {
		if err := s.eng.Mine(1); err != nil {
			s.t.Fatal(err)
		}
		s.x.Tick()
		s.w.Tick()
	}
}

func (s *splitEnv) ok(err error) {
	s.t.Helper()
	if err != nil {
		s.t.Fatal(err)
	}
}

func (s *splitEnv) refused(err error, contains string) {
	s.t.Helper()
	if err == nil {
		s.t.Fatalf("expected a refusal containing %q, got none", contains)
	}
	if !strings.Contains(err.Error(), contains) {
		s.t.Fatalf("expected a refusal containing %q, got %q", contains, err.Error())
	}
	s.t.Logf("refused as expected: %v", err)
}

func (s *splitEnv) balance(what string, want, tol int64) {
	s.t.Helper()
	got := s.w.Balance(s.chain) - s.start
	if d := got - want; d > tol || d < -tol {
		s.t.Fatalf("%s: wallet change %s, want %s", what, fmtCoin(got, ""), fmtCoin(want, ""))
	}
	s.t.Logf("%s: wallet change %s", what, fmtCoin(got, s.eng.cfg.Info.Ticker))
}

func (s *splitEnv) open() {
	s.t.Helper()
	s.ok(s.w.Open(s.chain, oneCoin))
	s.refused(s.w.Order(s.chain, "sell", 1000), "hasn't confirmed")
	s.mine(1)
}

func (s *splitEnv) vault() *wVault { return s.w.chains[s.chain].vault }

func (s *splitEnv) cashFor(sats int64) {
	s.t.Helper()
	price := s.x.chains[s.chain].price
	for s.x.Cash(s.w.Account) < orderCost(sats, price) {
		_, err := s.w.AddCash()
		s.ok(err)
	}
}

func TestSplitBitcoin(t *testing.T)  { runSplit(t, "btc") }
func TestSplitLitecoin(t *testing.T) { runSplit(t, "ltc") }

func runSplit(t *testing.T, chain string) {
	t.Run("SpeedUpExitRequest", func(t *testing.T) {
		s := newSplit(t, chain)
		s.open()
		g := s.vault().gen()
		if n := len(g.exitTx.TxOut); n != 3 {
			t.Fatalf("the exit request has %d outputs, want 3 (with an anchor for each side)", n)
		}
		_, err := s.w.SpeedUp(chain)
		s.refused(err, "waiting")
		s.ok(s.w.Exit(chain))
		msg, err := s.w.SpeedUp(chain)
		s.ok(err)
		t.Log(msg)
		wc := s.w.chains[chain]
		if len(wc.bumpable()) != 0 {
			t.Fatal("still listed as waiting after speeding up (the child should not be sped up again)")
		}
		s.mine(1)
		if wc.c.confs(g.exitTx) < 1 {
			t.Fatal("the exit request didn't confirm")
		}
	})

	t.Run("WatchtowerSpeedsUpWaitingExit", func(t *testing.T) {
		s := newSplit(t, chain)
		s.open()
		s.ok(s.w.Exit(chain))
		wc := s.w.chains[chain]
		s.w.Tick() // first sight of the waiting exit request
		id := s.vault().gen().exitTx.TxHash()
		if _, ok := wc.firstSeen[id]; !ok {
			t.Fatal("the watchtower didn't note the waiting exit request")
		}
		wc.firstSeen[id] -= bumpAfter // as if it had waited that many blocks
		s.w.Tick()
		found := false
		for _, e := range s.vault().Events {
			found = found || strings.Contains(e, "watchtower sped up")
		}
		if !found {
			t.Fatalf("no speed-up: %v", s.vault().Events)
		}
		s.mine(1)
		if wc.c.confs(s.vault().gen().exitTx) < 1 {
			t.Fatal("the exit request didn't confirm")
		}
	})

	t.Run("NormalTradeAndSettle", func(t *testing.T) {
		s := newSplit(t, chain)
		s.open()
		s.ok(s.w.Order(chain, "sell", 60_000_000))
		if c := s.x.Cash(s.w.Account); c != orderCost(60_000_000, s.eng.cfg.DefaultPrice) {
			t.Fatalf("cash %d after selling 0.6", c)
		}
		s.ok(s.w.Settle(chain))
		s.mine(1)
		s.balance("after settling", -60_000_000-2*txFee, 1500)
	})

	t.Run("ExchangeOfflineExitAndClaim", func(t *testing.T) {
		s := newSplit(t, chain)
		s.open()
		s.ok(s.w.Admin(chain, "offline", 0))
		s.refused(s.w.Settle(chain), "offline")
		s.ok(s.w.Exit(chain))
		s.mine(1)
		s.refused(s.w.Claim(chain), "timelock")
		s.mine(int(s.vault().def.delay) - 2)
		s.refused(s.w.Claim(chain), "timelock")
		s.mine(1)
		s.ok(s.w.Claim(chain))
		s.mine(1)
		s.balance("after claiming alone", -3*txFee, 1500)
	})

	t.Run("ReversalAttackExchangePublishes", func(t *testing.T) {
		s := newSplit(t, chain)
		s.open()
		s.ok(s.w.Order(chain, "sell", 60_000_000))
		s.ok(s.w.Exit(chain))
		s.mine(1) // the exchange (a separate program) publishes the sale
		if v := s.vault(); v.publishedBy != "exchange" || v.published.Index != 1 {
			t.Fatalf("published by %q", v.publishedBy)
		}
		s.mine(int(s.vault().def.d2) + 1)
		if v := s.x.vaults[s.vault().id]; v.sweepKind != "exchange" {
			t.Fatalf("the exchange didn't collect its share: %q", v.sweepKind)
		}
		if k := s.vault().sweepKind; k != "exchange" {
			t.Fatalf("the wallet didn't notice the exchange collecting: %q", k)
		}
		s.balance("the sale stands", -60_000_000-3*txFee, 1500)
	})

	t.Run("MaliciousOldStatePenaltySweep", func(t *testing.T) {
		s := newSplit(t, chain)
		s.open()
		s.ok(s.w.Order(chain, "sell", 50_000_000))
		s.ok(s.w.Order(chain, "buy", 50_000_000))
		s.ok(s.w.Admin(chain, "malicious", 0))
		s.refused(s.w.Settle(chain), "won't co-sign")
		s.ok(s.w.Exit(chain))
		s.mine(1) // the exchange publishes old state #1
		if v := s.vault(); v.published == nil || v.published.Index != 1 {
			t.Fatal("expected the exchange to publish old state #1")
		}
		s.mine(1) // the wallet's watchtower sweeps with secret #1
		s.mine(1)
		if v := s.vault(); v.sweepKind != "penalty" {
			t.Fatalf("sweep %q, want penalty", v.sweepKind)
		}
		s.balance("whole vault back", -4*txFee, 1500)
	})

	t.Run("UserPublishesOldStatePenalized", func(t *testing.T) {
		s := newSplit(t, chain)
		s.open()
		s.ok(s.w.Order(chain, "sell", 60_000_000))
		s.ok(s.w.Exit(chain))
		s.ok(s.w.Cheat(chain))
		s.mine(1) // the exchange's watchtower sweeps with the wallet's secret #0
		s.mine(1)
		if v := s.x.vaults[s.vault().id]; v.sweepKind != "exchange-penalty" {
			t.Fatalf("sweep %q, want exchange-penalty", v.sweepKind)
		}
		s.mine(1) // the sweep confirms; the wallet notices its share is gone
		if k := s.vault().sweepKind; k != "exchange-penalty" {
			t.Fatalf("the wallet didn't notice the penalty: %q", k)
		}
		s.balance("lost the whole vault", -oneCoin-txFee, 1500)
	})

	t.Run("BuyNewCoinsAndKeepThemWhenExchangeVanishes", func(t *testing.T) {
		s := newSplit(t, chain)
		s.open()
		s.ok(s.w.Capacity(chain, oneCoin))
		// The splice isn't confirmed, so state #0 must not be revoked yet:
		// the old vault output could still be the one that confirms.
		if v := s.vault(); v.states[0].revoked || v.states[0].rSecret != nil {
			t.Fatal("state #0 was revoked before the splice confirmed")
		}
		s.refused(s.w.Order(chain, "buy", 1000), "hasn't confirmed")
		s.mine(1) // splice confirms; the wallet swaps the secrets for #0
		if v := s.vault(); !v.states[0].revoked || !v.spliceFinished {
			t.Fatal("state #0 should be revoked once the splice confirmed")
		}
		s.cashFor(80_000_000)
		s.ok(s.w.Order(chain, "buy", 80_000_000))
		if u, _, _, _, _ := s.w.Vault(chain); u != 179_800_000 {
			t.Fatalf("your share %d, want 1.798", u)
		}
		s.ok(s.w.Admin(chain, "offline", 0))
		s.ok(s.w.Exit(chain))
		s.mine(1) // the wallet publishes its latest state
		if v := s.vault(); v.publishedBy != "user" || v.published.Index != 2 {
			t.Fatalf("published by %q", v.publishedBy)
		}
		s.mine(int(s.vault().def.d2))
		if v := s.vault(); v.sweepKind != "user" {
			t.Fatalf("sweep %q, want user", v.sweepKind)
		}
		s.mine(1)
		s.balance("kept the bought coins", 79_800_000-4*txFee, 1500)
	})

	t.Run("BadSignaturesRefused", func(t *testing.T) {
		s := newSplit(t, chain)
		// A bad exchange signature at opening: the deposit is never sent.
		s.x.SetTamper()
		s.refused(s.w.Open(chain, oneCoin), "deposit was not sent")
		s.balance("nothing sent", 0, 0)
		s.open()
		// A bad exchange signature on an order: nothing is signed.
		s.x.SetTamper()
		s.refused(s.w.Order(chain, "sell", 10_000_000), "doesn't check out")
		if n := len(s.vault().states); n != 1 {
			t.Fatalf("%d states after a refused order", n)
		}
		// A bad wallet signature: the exchange refuses the commit.
		v := s.vault()
		_, uh := newSecret()
		var off StateOffer
		s.ok(call(s.w.base, "/v1/order", OrderReq{v.id, "sell", 10_000_000, uh}, &off))
		err := call(s.w.base, "/v1/order/commit", CommitReq{v.id, off.Index, make([]byte, 64), v.latest().uSecret}, nil)
		s.refused(err, "doesn't check out")
		// A wrong secret: refused too.
		sig := signLeaf(v.latest().etx, 0, v.def.eFetch(v.gen()), v.def.ms, v.key) // a valid signature, wrong state
		err = call(s.w.base, "/v1/order/commit", CommitReq{v.id, off.Index, sig, make([]byte, 32)}, nil)
		s.refused(err, "doesn't check out")
		// The wallet still trades normally afterwards.
		s.ok(s.w.Order(chain, "sell", 10_000_000))
	})

	t.Run("NoTradingAfterSettleSignature", func(t *testing.T) {
		s := newSplit(t, chain)
		s.open()
		// The exchange signed a settlement of state #0. If it kept trading, the
		// wallet could broadcast that old settlement later.
		s.ok(call(s.w.base, "/v1/settle", SettleReq{s.vault().id}, &SettleResp{}))
		s.refused(s.w.Order(chain, "sell", 10_000_000), "settlement was signed")
		s.refused(s.w.Capacity(chain, oneCoin), "settlement was signed")
	})

	t.Run("ExchangeOfflineAtOpening", func(t *testing.T) {
		s := newSplit(t, chain)
		s.ok(s.w.Admin(chain, "offline", 0))
		s.refused(s.w.Open(chain, oneCoin), "offline")
		s.balance("nothing sent", 0, 0)
	})
}
