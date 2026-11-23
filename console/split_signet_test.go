package main

// The signet programs find coins sent to them with the node's address index.
// Signet itself can't run in a test (it needs the public network), so this
// checks the same lookup on a private chain with the index switched on.

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
)

func TestCoinsToFindsPayments(t *testing.T) {
	cfg := btcConfig(goBin("btcd"))
	cfg.AddrIndex = true
	e := NewUTXOEngine(cfg)
	if !e.available {
		t.Skip("btcd isn't installed")
	}
	e.Start()
	t.Cleanup(e.Stop)
	for i := 0; !e.Snapshot().Ready; i++ {
		if s := e.Snapshot(); s.InitErr != "" || i > 600 {
			t.Fatalf("the chain didn't start: %s", s.InitErr)
		}
		time.Sleep(200 * time.Millisecond)
	}
	c, err := dialChain(e.RPCAddr())
	if err != nil {
		t.Fatal(err)
	}
	k, _ := btcec.NewPrivateKey()
	script := walletScript(k)
	if got := c.coinsTo(script, &cfg.Params); len(got) != 0 {
		t.Fatalf("found %d coins at a new address", len(got))
	}
	op, err := e.Faucet(script, 30_000_000)
	if err != nil {
		t.Fatal(err)
	}
	check := func(when string) {
		t.Helper()
		got := c.coinsTo(script, &cfg.Params)
		if len(got) != 1 || got[0].op != op || got[0].amt != 30_000_000 {
			t.Fatalf("%s: found %+v, want %v for 0.3", when, got, op)
		}
	}
	check("before a block") // the index covers the mempool too
	e.Mine(1)
	check("after a block")
	coins, added := addNew(nil, c.coinsTo(script, &cfg.Params))
	if _, again := addNew(coins, c.coinsTo(script, &cfg.Params)); added != 30_000_000 || again != 0 {
		t.Fatalf("addNew added %d, then %d again", added, again)
	}
}
