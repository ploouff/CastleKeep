package main

// Limits on a shared server (split_limits.go). Test networks only.

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
)

func TestLimiter(t *testing.T) {
	l := newLimiter(0.0001, 3) // three at once, then about one every three hours
	for i := 0; i < 3; i++ {
		if !l.allow("a") {
			t.Fatalf("request %d of the first three was refused", i+1)
		}
	}
	if l.allow("a") {
		t.Fatal("a fourth request at once was allowed")
	}
	if !l.allow("b") {
		t.Fatal("another key shares the first one's allowance")
	}
	l.buckets["a"].at = time.Now().Add(-time.Hour * 24) // a day later it's full again
	for i := 0; i < 3; i++ {
		if !l.allow("a") {
			t.Fatalf("after waiting, request %d was refused", i+1)
		}
	}
	var none *limiter
	if !none.allow("a") || !newLimiter(0, 0).allow("a") {
		t.Fatal("no limit refused a request")
	}
}

func TestAddrOf(t *testing.T) {
	for _, c := range []struct{ remote, fwd, want string }{
		{"127.0.0.1:5000", "", ""},                                     // a program on the server
		{"[::1]:5000", "", ""},                                         // the same over IPv6
		{"127.0.0.1:5000", "203.0.113.7", "203.0.113.7"},               // through Caddy
		{"127.0.0.1:5000", "198.51.100.1, 203.0.113.7", "203.0.113.7"}, // the last one is Caddy's
		{"203.0.113.9:5000", "198.51.100.1", "203.0.113.9"},            // straight in: its own header isn't believed
		{"127.0.0.1:5000", "2001:db8:1:2:3:4:5:6", "2001:db8:1:2::/64"},
		{"127.0.0.1:5000", "not an address", "unreadable"},
	} {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.RemoteAddr = c.remote
		if c.fwd != "" {
			r.Header.Set("X-Forwarded-For", c.fwd)
		}
		if got := addrOf(r); got != c.want {
			t.Errorf("from %s, forwarded for %q: got %q, want %q", c.remote, c.fwd, got, c.want)
		}
	}
}

// fwd is a signed request's headers plus the address the web server in front
// says it came from.
func fwd(h map[string]string, addr string) map[string]string {
	h["X-Forwarded-For"] = addr
	return h
}

func TestSharedLimits(t *testing.T) {
	s := newSplit(t, "btc")
	s.x.Shared()
	s.open()
	keys := func() OpenKeysReq {
		k, _ := btcec.NewPrivateKey() // any key will do: these vaults are never finished
		return OpenKeysReq{Account: s.w.Account, Chain: "btc", UserPub: xonly(k), UserPay: s.w.WalletScript("btc")}
	}

	t.Run("Vaults", func(t *testing.T) {
		lim := DefaultLimits()
		lim.Vaults = 2
		s.x.SetLimits(lim)
		// Unfinished vaults hold no coins, so a new one replaces them at the limit.
		for i := 0; i < 3; i++ {
			s.ok(s.w.call("/v1/open/keys", keys(), &OpenKeysResp{}))
		}
		mine := 0
		for _, v := range s.x.vaults {
			if v.account == s.w.Account {
				mine++
			}
		}
		if mine != 2 {
			t.Fatalf("%d vaults kept for the account, want 2 (its open one and the newest unfinished one)", mine)
		}
		lim.Vaults = 1
		s.x.SetLimits(lim)
		expectRefused(t, s.w.call("/v1/open/keys", keys(), &OpenKeysResp{}), "at most 1 vaults")
	})

	t.Run("Orders", func(t *testing.T) {
		lim := DefaultLimits()
		lim.Orders = 1
		s.x.SetLimits(lim)
		id, err := s.w.PlaceOrder("btc", "sell", 9_000_000, 10_000) // far above the price: stays open
		s.ok(err)
		_, err = s.w.PlaceOrder("btc", "sell", 9_000_000, 10_000)
		expectRefused(t, err, "at most 1 open orders")
		s.ok(s.w.CancelOrder(id))
		id, err = s.w.PlaceOrder("btc", "sell", 9_000_000, 10_000)
		s.ok(err)
		s.ok(s.w.CancelOrder(id))
	})

	t.Run("TestCash", func(t *testing.T) {
		have := s.x.Cash(s.w.Account)
		lim := DefaultLimits()
		lim.CashCents = have + testCashCents + testCashCents/2
		s.x.SetLimits(lim)
		got, err := s.w.AddCash()
		s.ok(err)
		if got != have+testCashCents {
			t.Fatalf("cash %d after the first top-up, want %d", got, have+testCashCents)
		}
		if got, err = s.w.AddCash(); err != nil || got != lim.CashCents {
			t.Fatalf("the second top-up should stop at the limit: %d, %v", got, err)
		}
		_, err = s.w.AddCash()
		expectRefused(t, err, "stops at")
	})

	t.Run("NewAccounts", func(t *testing.T) {
		lim := DefaultLimits()
		lim.NewPerAddressPerDay = 1
		s.x.SetLimits(lim)
		newcomer := func(name, addr string) (int, string) {
			w, err := NewWallet(s.w.base, nil, nil)
			s.ok(err)
			w.SetName(name)
			body, _ := json.Marshal(CashReq{w.Account})
			return rawPost(t, s.w.base+"/v1/cashbalance", body, fwd(signedHeaders(w.Account, w.acctKey)("/v1/cashbalance", body), addr))
		}
		if code, out := newcomer("bob", "203.0.113.7"); code != http.StatusOK {
			t.Fatalf("a first new account was refused: %d %s", code, out)
		}
		if code, out := newcomer("carol", "203.0.113.7"); code != http.StatusTooManyRequests || !strings.Contains(out, "today") {
			t.Fatalf("a second new account from the same address: %d %s", code, out)
		}
		if code, out := newcomer("carol", "198.51.100.1"); code != http.StatusOK {
			t.Fatalf("a new account from another address was refused: %d %s", code, out)
		}
		lim.Accounts = len(s.x.acctKeys)
		s.x.SetLimits(lim)
		if code, out := newcomer("dave", "192.0.2.1"); code == http.StatusOK || !strings.Contains(out, "most accounts") {
			t.Fatalf("an account past the most the exchange takes: %d %s", code, out)
		}
		s.ok(s.w.call("/v1/cashbalance", CashReq{s.w.Account}, &CashResp{})) // known accounts carry on
	})

	t.Run("RequestsPerAccountAndAddress", func(t *testing.T) {
		lim := DefaultLimits()
		lim.PerAccount, lim.PerAccountBurst = 0.0001, 3
		s.x.SetLimits(lim)
		for i := 0; i < 3; i++ {
			s.ok(s.w.call("/v1/cashbalance", CashReq{s.w.Account}, &CashResp{}))
		}
		expectRefused(t, s.w.call("/v1/cashbalance", CashReq{s.w.Account}, &CashResp{}), "Too many requests")

		lim = DefaultLimits()
		lim.PerAddress, lim.PerAddressBurst = 0.0001, 2
		s.x.SetLimits(lim)
		send := func(addr string) (int, string) {
			body, _ := json.Marshal(CashReq{s.w.Account})
			return rawPost(t, s.w.base+"/v1/cashbalance", body, fwd(signedHeaders(s.w.Account, s.w.acctKey)("/v1/cashbalance", body), addr))
		}
		for i := 0; i < 2; i++ {
			if code, out := send("203.0.113.7"); code != http.StatusOK {
				t.Fatalf("request %d was refused: %d %s", i+1, code, out)
			}
		}
		if code, out := send("203.0.113.7"); code != http.StatusTooManyRequests {
			t.Fatalf("a third request at once from one address: %d %s", code, out)
		}
		if code, out := send("198.51.100.1"); code != http.StatusOK {
			t.Fatalf("another address was refused: %d %s", code, out)
		}
		s.ok(s.w.call("/v1/cashbalance", CashReq{s.w.Account}, &CashResp{})) // programs on the server itself aren't counted
	})

	t.Run("BigRequest", func(t *testing.T) {
		lim := DefaultLimits()
		lim.Body = 1024
		s.x.SetLimits(lim)
		body := []byte(fmt.Sprintf(`{"Account":%q,"Pad":%q}`, s.w.Account, strings.Repeat("x", 2000)))
		if code, out := rawPost(t, s.w.base+"/v1/cashbalance", body, signedHeaders(s.w.Account, s.w.acctKey)("/v1/cashbalance", body)); code != http.StatusRequestEntityTooLarge {
			t.Fatalf("a request over the size limit: %d %s", code, out)
		}
	})

	// Without -shared there are no limits.
	x, err := NewExchange(nil, nil)
	s.ok(err)
	x.SetLimits(Limits{Vaults: 1, Orders: 1, CashCents: 1})
	x.mu.Lock()
	if x.vaultRoom("a") != nil || x.orderRoom("a") != nil {
		t.Fatal("limits apply to an exchange that isn't shared")
	}
	if add, err := x.cashToAdd("a"); err != nil || add != testCashCents {
		t.Fatal("the test cash limit applies to an exchange that isn't shared")
	}
	x.mu.Unlock()

	// Alice carries on as normal.
	s.x.SetLimits(DefaultLimits())
	s.ok(s.w.Settle("btc"))
	s.mine(1)
}

// towerVault is a made-up vault record with gens generations (no signed
// transactions in it, so each generation takes 64 bytes: its exit request id).
func towerVault(id string, gens int) *TowerUTXOVault {
	v := &TowerUTXOVault{Chain: "btc", VaultID: id}
	for i := 0; i < gens; i++ {
		v.Gens = append(v.Gens, &TowerGen{ExitTxid: strings.Repeat("0", 60-2*len(id)) + hex.EncodeToString([]byte(id)) + fmt.Sprintf("%04d", i)})
	}
	return v
}

func TestSharedTowerLimits(t *testing.T) {
	tw, err := NewTower(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	tw.Shared()
	lim := DefaultLimits()
	lim.TowerVaults, lim.TowerBytes, lim.Accounts, lim.NewPerAddressPerDay = 2, 200, 3, 1
	tw.SetLimits(lim)
	alice := TowerAuth{"alice-1", newToken()}
	up := func(a TowerAuth, from string, vs ...*TowerUTXOVault) error {
		_, err := tw.updateFrom(from, TowerUpdate{Wallet: a.Wallet, Token: a.Token, UTXO: vs})
		return err
	}
	ok := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	// Vaults and stored data per wallet.
	ok(up(alice, "203.0.113.7", towerVault("v1", 1), towerVault("v2", 1)))
	expectRefused(t, up(alice, "203.0.113.7", towerVault("v3", 1)), "at most 2 vaults")
	ok(up(alice, "203.0.113.7", towerVault("v1", 2)))                      // one more generation: 192 bytes
	ok(up(alice, "203.0.113.7", towerVault("v1", 2), towerVault("v2", 1))) // sending it all again adds nothing
	expectRefused(t, up(alice, "203.0.113.7", towerVault("v1", 3)), "at most 200 bytes")

	// New wallets: one a day from an address, and at most lim.Accounts in all.
	bob, carol, dave := TowerAuth{"bob-1", newToken()}, TowerAuth{"carol-1", newToken()}, TowerAuth{"dave-1", newToken()}
	err = up(bob, "203.0.113.7")
	expectRefused(t, err, "today")
	if _, isLimit := err.(limitErr); !isLimit {
		t.Fatal("too many new wallets isn't reported as a limit on how often")
	}
	ok(up(bob, "198.51.100.1"))
	ok(up(carol, "")) // a program on the server itself
	expectRefused(t, up(dave, "192.0.2.1"), "most wallets")

	// Over HTTP: too many requests from one address get 429.
	lim.PerAddress, lim.PerAddressBurst = 0.0001, 1
	tw.SetLimits(lim)
	srv := httptest.NewServer(tw.Handler())
	defer srv.Close()
	body, _ := json.Marshal(alice)
	hdr := map[string]string{"X-Forwarded-For": "203.0.113.7"}
	if code, out := rawPost(t, srv.URL+"/v1/tower/status", body, hdr); code != http.StatusOK {
		t.Fatalf("a first request was refused: %d %s", code, out)
	}
	if code, out := rawPost(t, srv.URL+"/v1/tower/status", body, hdr); code != http.StatusTooManyRequests {
		t.Fatalf("a second request at once: %d %s", code, out)
	}
}

func TestGatewayNodeCalls(t *testing.T) {
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"result":42,"error":null,"id":1}`))
	}))
	defer node.Close()
	lim := DefaultLimits()
	lim.NodeCalls = 1
	g := newGuard(lim)
	old := nodeWait
	nodeWait = 50 * time.Millisecond
	defer func() { nodeWait = old }()
	gw := httptest.NewServer(rpcGateway(func() string { return strings.TrimPrefix(node.URL, "http://") }, g))
	defer gw.Close()
	body := []byte(`{"jsonrpc":"1.0","method":"getblockcount","params":[],"id":1}`)
	outside := map[string]string{"X-Forwarded-For": "203.0.113.7"}

	if code, out := rawPost(t, gw.URL, body, outside); code != http.StatusOK || !strings.Contains(out, "42") {
		t.Fatalf("a call through the gateway: %d %s", code, out)
	}
	release, ok := g.enterNode() // someone else's call is at the node
	if !ok {
		t.Fatal("couldn't take the free slot")
	}
	if code, out := rawPost(t, gw.URL, body, outside); code != http.StatusServiceUnavailable {
		t.Fatalf("a call while the node is busy: %d %s", code, out)
	}
	if code, out := rawPost(t, gw.URL, body, nil); code != http.StatusOK {
		t.Fatalf("the server's own programs should go straight through: %d %s", code, out)
	}
	release()
	if code, out := rawPost(t, gw.URL, body, outside); code != http.StatusOK {
		t.Fatalf("a call once the node is free: %d %s", code, out)
	}
}

// The page's reads reuse an answer for a moment; anything the wallet does
// itself (other than asking for fills) makes the next read ask again.
func TestWalletPageReads(t *testing.T) {
	var c readCache
	asked := 0
	price := int64(100)
	look := func(chain string) int64 {
		var r InfoResp
		if err := c.look("/v1/info", InfoReq{chain}, &r, func() error { asked++; r = InfoResp{Price: price}; return nil }); err != nil {
			t.Fatal(err)
		}
		return r.Price
	}
	if look("btc") != 100 || look("btc") != 100 || asked != 1 {
		t.Fatalf("a second read straight after the first asked again (%d asks)", asked)
	}
	look("ltc")
	if asked != 2 {
		t.Fatalf("another chain's read was answered from the first chain's (%d asks)", asked)
	}
	price = 200
	c.sent(fillsPath) // asking for fills changes nothing
	if look("btc") != 100 || asked != 2 {
		t.Fatalf("asking for fills made the page ask again (%d asks)", asked)
	}
	c.sent("/v1/order") // the wallet traded: read again at once
	if look("btc") != 200 || asked != 3 {
		t.Fatalf("after a trade the page showed the old answer (%d asks)", asked)
	}
	// A trade while a read is on its way: that answer isn't kept.
	var r InfoResp
	c.look("/v1/info", InfoReq{"eth"}, &r, func() error { asked++; c.sent("/v1/order"); return nil })
	if look("eth"); asked != 5 {
		t.Fatalf("an answer from before a trade was kept (%d asks)", asked)
	}
	old := readCacheFor
	readCacheFor = 0
	defer func() { readCacheFor = old }()
	look("btc")
	if asked != 6 {
		t.Fatal("with readCacheFor 0 the page's reads were still reused")
	}
}
