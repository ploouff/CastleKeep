package main

// Order book tests: two users (Alice and Bob), each with their own wallet
// and vaults, trade with each other through one exchange server over HTTP.

import (
	"net/http/httptest"
	"testing"
)

type bookEnv struct {
	t          *testing.T
	chain      string
	x          *Exchange
	alice, bob *Wallet
	mineFn     func()
	price      int64
}

func newBookEnv(t *testing.T, chain string) *bookEnv {
	e := &bookEnv{t: t, chain: chain}
	cfgs, rpc := map[string]UTXOConfig{}, map[string]string{}
	var ue *UTXOEngine
	var ee *EthEngine
	if chain == "eth" {
		ee = engine(t, "eth").(*EthEngine)
	} else {
		ue = engine(t, chain).(*UTXOEngine)
		cfgs[chain], rpc[chain] = ue.cfg, ue.RPCAddr()
	}
	x, err := NewExchange(cfgs, rpc)
	if err != nil {
		t.Fatal(err)
	}
	var xa string
	if ee != nil {
		xa = x.AttachEth(ee.Endpoint(), ee.cfg.DefaultPrice)
	}
	srv := httptest.NewServer(x.Handler())
	t.Cleanup(srv.Close)
	e.x = x
	for _, w := range []**Wallet{&e.alice, &e.bob} {
		nw, err := NewWallet(srv.URL, cfgs, rpc)
		if err != nil {
			t.Fatal(err)
		}
		*w = nw
	}
	if ee != nil {
		for _, a := range []string{xa, e.alice.AttachEth(ee.Endpoint()), e.bob.AttachEth(ee.Endpoint())} {
			if err := ee.FundAddress(a, 5*unit); err != nil {
				t.Fatal(err)
			}
		}
		e.mineFn = func() { ee.MineEth(1, 0) }
		e.price = ee.cfg.DefaultPrice
	} else {
		op, err := ue.Faucet(x.WalletScript(chain), 10*unit)
		if err != nil {
			t.Fatal(err)
		}
		x.AddCoins(chain, op, 10*unit)
		for _, w := range []*Wallet{e.alice, e.bob} {
			op, err := ue.Faucet(w.WalletScript(chain), 2*unit)
			if err != nil {
				t.Fatal(err)
			}
			w.AddCoins(chain, op, 2*unit)
		}
		e.mineFn = func() { ue.Mine(1) }
		e.price = ue.cfg.DefaultPrice
	}
	e.mine()
	return e
}

func (e *bookEnv) mine() {
	e.mineFn()
	e.x.Tick()
	e.alice.Tick()
	e.bob.Tick()
}

func (e *bookEnv) ok(err error) {
	e.t.Helper()
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *bookEnv) refused(err error, contains string) {
	e.t.Helper()
	(&splitEnv{t: e.t}).refused(err, contains)
}

func (e *bookEnv) open(w *Wallet) {
	e.t.Helper()
	if e.chain == "eth" {
		e.ok(w.EthOpen(oneCoin))
	} else {
		e.ok(w.Open(e.chain, oneCoin))
	}
}

func (e *bookEnv) capacity(w *Wallet) {
	e.t.Helper()
	if e.chain == "eth" {
		e.ok(w.EthCapacity(oneCoin))
	} else {
		e.ok(w.Capacity(e.chain, oneCoin))
	}
}

func (e *bookEnv) share(w *Wallet) int64 {
	if e.chain == "eth" {
		u, _, _, _ := w.EthVault()
		return u
	}
	u, _, _, _, _ := w.Vault(e.chain)
	return u
}

func (e *bookEnv) expectShare(who string, w *Wallet, want int64) {
	e.t.Helper()
	if got := e.share(w); got != want {
		e.t.Fatalf("%s's share %s, want %s", who, fmtCoin(got, ""), fmtCoin(want, ""))
	}
}

func (e *bookEnv) cash(w *Wallet, cents int64) {
	e.t.Helper()
	for e.x.Cash(w.Account) < cents {
		_, err := w.AddCash()
		e.ok(err)
	}
}

func TestBookBitcoin(t *testing.T)  { runBook(t, "btc") }
func TestBookLitecoin(t *testing.T) { runBook(t, "ltc") }
func TestBookEthereum(t *testing.T) { runBook(t, "eth") }

func runBook(t *testing.T, chain string) {
	e := newBookEnv(t, chain)
	e.open(e.alice)
	e.open(e.bob)
	e.mine()
	e.capacity(e.bob) // Bob needs the exchange's coins in his vault to buy
	e.mine()
	e.mine()
	bobShare := oneCoin - capacityFee(oneCoin)
	e.expectShare("Bob", e.bob, bobShare)
	p := e.price - e.price/20 // a little under the starting price

	t.Run("RefusedOrders", func(t *testing.T) {
		e.t = t
		e.refused(func() error { _, err := e.alice.PlaceOrder(chain, "sell", p, 2*oneCoin); return err }(), "free to sell")
		e.refused(func() error { _, err := e.alice.PlaceOrder(chain, "buy", p, 10_000_000); return err }(), "buying capacity")
		e.refused(func() error { _, err := e.bob.PlaceOrder(chain, "buy", p, 10_000_000); return err }(), "cash free")
	})

	t.Run("AliceSellsToBob", func(t *testing.T) {
		e.t = t
		aCash := e.x.Cash(e.alice.Account)
		e.cash(e.bob, orderCost(20_000_000, p+e.price/50))
		bCash := e.x.Cash(e.bob.Account)
		ask, err := e.alice.PlaceOrder(chain, "sell", p, 30_000_000)
		e.ok(err)
		book, err := e.bob.Book(chain)
		e.ok(err)
		if len(book.Asks) != 1 || book.Asks[0].Price != p || book.Asks[0].Sats != 30_000_000 {
			t.Fatalf("book after Alice's order: %+v", book.Asks)
		}
		// Bob bids above Alice's price: he gets her price (the resting order's).
		_, err = e.bob.PlaceOrder(chain, "buy", p+e.price/50, 20_000_000)
		e.ok(err)
		e.expectShare("Bob", e.bob, bobShare+20_000_000) // Bob signed his side straight away
		e.alice.Tick()                                   // Alice's wallet signs her side
		e.expectShare("Alice", e.alice, oneCoin-20_000_000)
		cost := orderCost(20_000_000, p)
		if got := e.x.Cash(e.alice.Account) - aCash; got != cost {
			t.Fatalf("Alice's cash went up by %s, want %s", fmtUSD(got), fmtUSD(cost))
		}
		if got := bCash - e.x.Cash(e.bob.Account); got != cost {
			t.Fatalf("Bob's cash went down by %s, want %s", fmtUSD(got), fmtUSD(cost))
		}
		book, _ = e.alice.Book(chain)
		if book.Price != p || len(book.Asks) != 1 || book.Asks[0].Sats != 10_000_000 || len(book.Trades) != 1 {
			t.Fatalf("after the trade: price %d, asks %+v, trades %d", book.Price, book.Asks, len(book.Trades))
		}
		e.ok(e.alice.CancelOrder(ask))
		book, _ = e.alice.Book(chain)
		if len(book.Asks) != 0 {
			t.Fatal("Alice's cancelled order is still in the book")
		}
	})

	t.Run("WalletRefusesAFillWorseThanItsOrder", func(t *testing.T) {
		e.t = t
		_, err := e.alice.PlaceOrder(chain, "sell", p, 5_000_000)
		e.ok(err)
		before := e.share(e.alice)
		// Pretend Alice had asked for more: her wallet must not sign a fill below it.
		for _, o := range e.alice.orders {
			if o.Left == 5_000_000 {
				o.Price = p + 1
			}
		}
		e.cash(e.bob, e.x.Cash(e.bob.Account)+orderCost(5_000_000, p))
		_, err = e.bob.PlaceOrder(chain, "buy", p, 5_000_000)
		e.ok(err)
		e.refused(func() error { e.alice.mu.Lock(); defer e.alice.mu.Unlock(); return e.alice.signFills(chain) }(), "doesn't match any order")
		if e.share(e.alice) != before {
			t.Fatal("Alice's wallet signed a fill worse than her order")
		}
	})

	t.Run("PagePlaceAndCancel", func(t *testing.T) {
		e.t = t
		high := e.price * 10 // far above everything, so it rests in the book
		_, err, handled := e.bob.bookAction(chain, "X", ActionReq{Action: "place", Side: "sell", USD: high, Sats: 1_000_000})
		if !handled {
			t.Fatal("place wasn't handled")
		}
		e.ok(err)
		b := e.bob.bookSnapshot(chain)
		var id int
		for _, o := range b.Mine {
			if o.Price == high && o.Status == "open" {
				id = o.ID
			}
		}
		if id == 0 || len(b.Asks) == 0 || b.Asks[len(b.Asks)-1].Price != high {
			t.Fatalf("Bob's order isn't in the book: %+v", b)
		}
		_, err, _ = e.bob.bookAction(chain, "X", ActionReq{Action: "cancel", ID: id})
		e.ok(err)
		for _, o := range e.bob.bookSnapshot(chain).Mine {
			if o.ID == id && o.Status != "cancelled" {
				t.Fatalf("order #%d is %s after cancel", id, o.Status)
			}
		}
		if _, _, handled := e.bob.bookAction(chain, "X", ActionReq{Action: "nope"}); handled {
			t.Fatal("an unknown action was handled")
		}
	})
}
