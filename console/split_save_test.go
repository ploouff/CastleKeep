package main

// Saving and restarting: the exchange and two wallets save to files, stop in
// the middle of trading (with an order resting in the book), start again from
// the files, and carry on: the resting order fills, and the exchange's
// watchtower still catches an old state using a secret it had saved.

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestSaveBitcoin(t *testing.T)  { runSave(t, "btc") }
func TestSaveLitecoin(t *testing.T) { runSave(t, "ltc") }
func TestSaveEthereum(t *testing.T) { runSave(t, "eth") }
func TestSaveBase(t *testing.T)     { runSave(t, "base") }

// restart builds a new exchange and new wallets from the saved files, the way
// the programs do when they start again, and points the test at them.
func (e *bookEnv) restart(dir string, owner *testing.T) {
	t := e.t
	t.Helper()
	cfgs, rpc := map[string]UTXOConfig{}, map[string]string{}
	var ee *EthEngine
	if isEVM(e.chain) {
		ee = engine(t, e.chain).(*EthEngine)
	} else {
		ue := engine(t, e.chain).(*UTXOEngine)
		cfgs[e.chain], rpc[e.chain] = ue.cfg, ue.RPCAddr()
	}
	x, err := NewExchange(cfgs, rpc)
	e.ok(err)
	if ee != nil {
		x.AttachEVM(e.chain, ee.Endpoint(), ee.cfg.DefaultPrice)
	}
	loaded, err := x.Load(filepath.Join(dir, "exchange.json"))
	e.ok(err)
	if !loaded {
		t.Fatal("the exchange's file wasn't there")
	}
	sameRecord(t, "exchange", e.x.record(), x.record())
	srv := httptest.NewServer(x.Handler())
	owner.Cleanup(srv.Close) // lives as long as the whole test, not just a sub-test
	for _, p := range []struct {
		name string
		w    **Wallet
	}{{"alice", &e.alice}, {"bob", &e.bob}} {
		nw, err := NewWallet(srv.URL, cfgs, rpc)
		e.ok(err)
		if ee != nil {
			nw.AttachEVM(e.chain, ee.Endpoint())
		}
		loaded, err := nw.Load(filepath.Join(dir, p.name+".json"))
		e.ok(err)
		if !loaded {
			t.Fatalf("%s's file wasn't there", p.name)
		}
		sameRecord(t, p.name, (*p.w).record(), nw.record())
		nw.SaveTo(filepath.Join(dir, p.name+".json"))
		*p.w = nw
	}
	x.SaveTo(filepath.Join(dir, "exchange.json"))
	e.x = x
}

// sameRecord checks that what was loaded saves exactly as before.
func sameRecord(t *testing.T, who string, before, after any) {
	t.Helper()
	a, _ := json.Marshal(before)
	b, _ := json.Marshal(after)
	if string(a) != string(b) {
		t.Fatalf("%s after a restart differs from before:\nbefore %s\nafter  %s", who, a, b)
	}
}

func runSave(t *testing.T, chain string) {
	root := t
	e := newBookEnv(t, chain)
	dir := t.TempDir()
	e.x.SaveTo(filepath.Join(dir, "exchange.json"))
	e.alice.SaveTo(filepath.Join(dir, "alice.json"))
	e.bob.SaveTo(filepath.Join(dir, "bob.json"))
	e.open(e.alice)
	e.open(e.bob)
	e.mine()
	e.capacity(e.bob)
	e.mine()
	e.mine()
	p := e.price - e.price/20

	// Before the restart: Alice sells some straight to the exchange, then
	// puts a sell order in the book that nobody has matched yet.
	if isEVM(chain) {
		e.ok(e.alice.EthOrder(e.chain, "sell", 10_000_000))
	} else {
		e.ok(e.alice.Order(chain, "sell", 10_000_000))
	}
	_, err := e.alice.PlaceOrder(chain, "sell", p, 30_000_000)
	e.ok(err)
	e.cash(e.bob, orderCost(20_000_000, p))
	e.mine()
	aliceShare := e.share(e.alice)

	e.restart(dir, t)

	t.Run("BookCarriesOn", func(t *testing.T) {
		e.t = t
		book, err := e.bob.Book(chain)
		e.ok(err)
		if len(book.Asks) != 1 || book.Asks[0].Sats != 30_000_000 {
			t.Fatalf("Alice's order didn't survive the restart: %+v", book.Asks)
		}
		before := e.share(e.bob)
		_, err = e.bob.PlaceOrder(chain, "buy", p, 20_000_000)
		e.ok(err)
		e.expectShare("Bob", e.bob, before+20_000_000)
		e.alice.Tick() // Alice's restarted wallet signs her side against her saved order
		e.expectShare("Alice", e.alice, aliceShare-20_000_000)
	})

	t.Run("RestartAgainMidway", func(t *testing.T) {
		e.t = t
		e.mine()
		e.restart(dir, root) // a second restart, after the fill, must load cleanly too
	})

	if isEVM(chain) {
		t.Run("SettleAfterRestart", func(t *testing.T) {
			e.t = t
			e.ok(e.alice.EthSettle(e.chain))
			e.mine()
			if _, _, _, closed := e.alice.EthVault(e.chain); closed != "settled" {
				t.Fatalf("Alice's vault: %q", closed)
			}
		})
		return
	}

	t.Run("WatchtowerUsesSavedSecrets", func(t *testing.T) {
		e.t = t
		// Alice publishes an old state. The exchange revoked it before the
		// restarts, so it only has her secret from its saved file.
		e.ok(e.alice.Exit(chain))
		e.ok(e.alice.Cheat(chain))
		e.mine()
		e.mine()
		var kind string
		for _, v := range e.x.vaults {
			if v.account == e.alice.Account {
				kind = v.sweepKind
			}
		}
		if kind != "exchange-penalty" {
			t.Fatalf("the restarted exchange's sweep: %q, want exchange-penalty", kind)
		}
	})
}

// A kept test chain starts again where it was: same blocks, same faucet key,
// and the faucet doesn't try to spend a coinbase it already spent.
func TestKeptChainRestarts(t *testing.T) {
	cfg := btcConfig(goBin("btcd"))
	keepChain(&cfg, filepath.Join(t.TempDir(), "btc"), 0)
	start := func() *UTXOEngine {
		e := NewUTXOEngine(cfg)
		if !e.available {
			t.Skip("btcd isn't installed")
		}
		e.Start()
		t.Cleanup(e.Stop)
		for i := 0; ; i++ {
			s := e.Snapshot()
			if s.Ready {
				return e
			}
			if s.InitErr != "" || i > 600 {
				t.Fatalf("the chain didn't start: %s", s.InitErr)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	e := start()
	c, err := dialChain(e.RPCAddr())
	e.ok(t, err)
	tag := c.tag()
	if _, err := e.Faucet(make([]byte, 34), unit); err != nil { // any script will do
		t.Fatal(err)
	}
	e.Mine(1)
	h := e.Snapshot().Height
	e.Stop()

	e = start()
	c, err = dialChain(e.RPCAddr())
	e.ok(t, err)
	if got := c.tag(); got != tag {
		t.Fatalf("a different chain after the restart: %s, want %s", got, tag)
	}
	if got := e.Snapshot().Height; got != h {
		t.Fatalf("height %d after the restart, want %d", got, h)
	}
	if _, err := e.Faucet(make([]byte, 34), unit); err != nil {
		t.Fatalf("the faucet after a restart: %v", err)
	}
}

func (e *UTXOEngine) ok(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
