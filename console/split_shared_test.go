package main

// Shared mode (split_shared.go): the exchange checks every wallet's signature
// and keeps wallets out of each other's vaults; the chains program's gateway
// passes on only the node calls the programs need. Test networks only.

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
)

// rawPost sends body to the exchange with exactly these headers.
func rawPost(t *testing.T, url string, body []byte, hdr map[string]string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func expectRefused(t *testing.T, err error, contains string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), contains) {
		t.Fatalf("expected a refusal containing %q, got %v", contains, err)
	}
	t.Logf("refused as expected: %v", err)
}

func TestSharedExchange(t *testing.T) {
	s := newSplit(t, "btc")
	tok := s.x.Shared()
	// The wallet signs everything, so normal use carries on.
	s.open()
	s.ok(s.w.Order("btc", "sell", 10_000_000))
	v := s.vault()

	t.Run("UnsignedRefused", func(t *testing.T) {
		expectRefused(t, call(s.w.base, "/v1/cashbalance", CashReq{s.w.Account}, &CashResp{}), "must sign every request")
	})

	// Mallory has her own wallet on the same exchange.
	mallory, err := NewWallet(s.w.base, nil, nil)
	s.ok(err)
	mallory.SetName("mallory")
	s.ok(mallory.call("/v1/cashbalance", CashReq{mallory.Account}, &CashResp{}))

	t.Run("OthersVaultsAndCashRefused", func(t *testing.T) {
		_, uh := newSecret()
		expectRefused(t, mallory.call("/v1/order", OrderReq{v.id, "buy", 1000, uh}, &StateOffer{}), "isn't yours")
		expectRefused(t, mallory.call("/v1/settle", SettleReq{VaultID: v.id}, &SettleResp{}), "isn't yours")
		expectRefused(t, mallory.call("/v1/cashbalance", CashReq{s.w.Account}, &CashResp{}), "isn't yours")
		expectRefused(t, mallory.call("/v1/cash", CashReq{s.w.Account}, &CashResp{}), "isn't yours")
		expectRefused(t, mallory.call("/v1/fills/next", FillReq{Account: s.w.Account, Chain: "btc"}, &FillOffer{}), "isn't yours")
		expectRefused(t, mallory.call("/v1/book/place", PlaceReq{mallory.Account, "btc", v.id, "sell", 1, 1000}, &PlaceResp{}), "isn't yours")
	})

	t.Run("BorrowedAccountRefused", func(t *testing.T) {
		// Alice's account name, Mallory's key.
		err := callHeaders("the exchange", httpClient, s.w.base, "/v1/cashbalance", CashReq{s.w.Account}, &CashResp{},
			signedHeaders(s.w.Account, mallory.acctKey))
		expectRefused(t, err, "belongs to another wallet")
	})

	t.Run("ReplayOldAndAlteredRefused", func(t *testing.T) {
		body, _ := json.Marshal(CashReq{s.w.Account})
		h := signedHeaders(s.w.Account, s.w.acctKey)("/v1/cashbalance", body)
		if code, out := rawPost(t, s.w.base+"/v1/cashbalance", body, h); code != http.StatusOK {
			t.Fatalf("a good request was refused: %d %s", code, out)
		}
		if code, out := rawPost(t, s.w.base+"/v1/cashbalance", body, h); code == http.StatusOK || !strings.Contains(out, "already used") {
			t.Fatalf("a replayed request: %d %s", code, out)
		}
		// Signed for one body, sent with another.
		other, _ := json.Marshal(CashReq{s.w.Account + " "})
		h = signedHeaders(s.w.Account, s.w.acctKey)("/v1/cashbalance", body)
		if code, out := rawPost(t, s.w.base+"/v1/cashbalance", other, h); code == http.StatusOK || !strings.Contains(out, "doesn't check out") {
			t.Fatalf("an altered request: %d %s", code, out)
		}
		// A correct signature from an hour ago.
		ts := strconv.FormatInt(time.Now().Unix()-3600, 10)
		sig, _ := schnorr.Sign(s.w.acctKey, requestDigest("/v1/cashbalance", ts, "n1", body))
		h = map[string]string{hdrAccount: s.w.Account, hdrKey: hex.EncodeToString(xonly(s.w.acctKey)), hdrTime: ts, hdrNonce: "n1", hdrSig: hex.EncodeToString(sig.Serialize())}
		if code, out := rawPost(t, s.w.base+"/v1/cashbalance", body, h); code == http.StatusOK || !strings.Contains(out, "5 minutes") {
			t.Fatalf("an old request: %d %s", code, out)
		}
	})

	t.Run("OperatorOnly", func(t *testing.T) {
		expectRefused(t, s.w.Admin("btc", "offline", 0), "Only the operator")
		expectRefused(t, call(s.w.base, "/v1/state", empty{}, &XStateResp{}), "Only the operator")
		admin := func(string, []byte) map[string]string { return map[string]string{hdrAdmin: tok} }
		s.ok(callHeaders("the exchange", httpClient, s.w.base, "/v1/admin", AdminReq{Chain: "btc", Mode: "honest"}, nil, admin))
		s.ok(callHeaders("the exchange", httpClient, s.w.base, "/v1/state", empty{}, &XStateResp{}, admin))
		wrong := func(string, []byte) map[string]string { return map[string]string{hdrAdmin: tok + "x"} }
		expectRefused(t, callHeaders("the exchange", httpClient, s.w.base, "/v1/state", empty{}, &XStateResp{}, wrong), "Only the operator")
	})

	t.Run("KeptAcrossRestarts", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "exchange.json")
		s.x.SaveTo(file)
		x2, err := NewExchange(nil, nil)
		s.ok(err)
		_, err = x2.Load(file)
		s.ok(err)
		if x2.Shared() != tok || x2.acctKeys[s.w.Account] != hex.EncodeToString(xonly(s.w.acctKey)) {
			t.Fatal("the admin token or the account keys weren't kept")
		}
		wfile := filepath.Join(t.TempDir(), "wallet.json")
		s.w.SaveTo(wfile)
		w2, err := NewWallet(s.w.base, nil, nil)
		s.ok(err)
		_, err = w2.Load(wfile)
		s.ok(err)
		s.ok(w2.call("/v1/cashbalance", CashReq{s.w.Account}, &CashResp{})) // same key, so the exchange still knows it
	})

	// Alice carries on as normal.
	s.ok(s.w.Settle("btc"))
	s.mine(1)
	s.balance("settled on a shared exchange", -10_000_000-2*txFee, 1500)
}

// On a shared exchange an EVM vault is only registered for the account its
// owner signed for, so nobody can take its number first.
func TestSharedEVMClaim(t *testing.T) {
	s := newSplitEth(t, "eth")
	s.x.Shared()
	s.ok(s.w.EthOpen("eth", oneCoin))
	s.ok(s.ee.MineEth(1, 0)) // the deposit is on the chain; Alice's wallet hasn't registered it yet
	id := s.w.evm["eth"].vault.id.String()
	mallory, err := NewWallet(s.w.base, nil, nil)
	s.ok(err)
	mallory.SetName("mallory")
	mk := mallory.AttachEVM("eth", s.ee.Endpoint())
	_ = mk
	mc := mallory.evm["eth"]
	off := &EthStateOffer{}
	expectRefused(t, mallory.call("/v1/eth/open", EthOpenReq{Account: mallory.Account, VaultID: id, Chain: "eth"}, off), "isn't yours")
	claim := signDigest(mc.key, claimDigest(mc.c.chainID, mc.c.contract, id, mallory.Account))
	expectRefused(t, mallory.call("/v1/eth/open", EthOpenReq{mallory.Account, id, "eth", claim}, off), "isn't yours")
	// Alice's own wallet registers it and trades.
	s.mine(1)
	if _, _, n, _ := s.w.EthVault("eth"); n != 1 {
		t.Fatalf("Alice's vault wasn't registered (%d states)", n)
	}
	s.ok(s.w.EthOrder("eth", "sell", 10_000_000))
}

// The gateway passes on only the node calls the programs use, and a wallet
// can do everything through it, as a friend's wallet would over the internet.
func TestSharedGateway(t *testing.T) {
	e := engine(t, "btc").(*UTXOEngine)
	m := http.NewServeMux()
	m.Handle("/rpc/btc", rpcGateway(e.RPCAddr))
	m.Handle("/faucet", localOnly(true, handle(func(empty) (empty, error) { return empty{}, nil })))
	gw := httptest.NewServer(m)
	t.Cleanup(gw.Close)
	url := gatewayURL(gw.URL, ChainStatus{ID: "btc", RPC: e.RPCAddr(), Gateway: "/rpc/btc", Shared: true})
	if url != gw.URL+"/rpc/btc" {
		t.Fatalf("gateway address %q", url)
	}
	if gatewayURL(gw.URL, ChainStatus{ID: "btc", RPC: "127.0.0.1:1", Gateway: "/rpc/btc"}) != "127.0.0.1:1" {
		t.Fatal("a chains program that isn't shared should be reached directly")
	}

	t.Run("OnlyAllowedCalls", func(t *testing.T) {
		for _, method := range []string{"stop", "generatetoaddress", "getnewaddress", "addnode", "setgenerate"} {
			body := []byte(`{"jsonrpc":"1.0","id":1,"method":"` + method + `","params":[]}`)
			if code, out := rawPost(t, url, body, nil); code != http.StatusForbidden {
				t.Fatalf("%s went through the gateway: %d %s", method, code, out)
			}
		}
		if code, _ := rawPost(t, url, []byte(`[{"method":"getblockcount"}]`), nil); code != http.StatusBadRequest {
			t.Fatalf("a batch went through: %d", code)
		}
		if code, out := rawPost(t, gw.URL+"/faucet", []byte(`{}`), map[string]string{"X-Forwarded-For": "203.0.113.9"}); code != http.StatusForbidden {
			t.Fatalf("the faucet answered a request from outside: %d %s", code, out)
		}
		if code, out := rawPost(t, gw.URL+"/faucet", []byte(`{}`), nil); code != http.StatusOK {
			t.Fatalf("the faucet refused a program on the same computer: %d %s", code, out)
		}
	})

	t.Run("RemoteWalletTradesThroughIt", func(t *testing.T) {
		cfgs := map[string]UTXOConfig{"btc": e.cfg}
		x, err := NewExchange(cfgs, map[string]string{"btc": e.RPCAddr()})
		s := &splitEnv{t: t, chain: "btc", eng: e, x: x}
		s.ok(err)
		x.Shared()
		srv := httptest.NewServer(x.Handler())
		t.Cleanup(srv.Close)
		// The friend's wallet reaches the chain only through the gateway.
		w, err := NewWallet(srv.URL, cfgs, map[string]string{"btc": url})
		s.ok(err)
		s.w = w
		if h := w.chains["btc"].c.height(); h != e.height() {
			t.Fatalf("height through the gateway %d, node %d", h, e.height())
		}
		op, err := e.Faucet(x.WalletScript("btc"), 10*unit)
		s.ok(err)
		x.AddCoins("btc", op, 10*unit)
		op, err = e.Faucet(w.WalletScript("btc"), 2*unit)
		s.ok(err)
		w.AddCoins("btc", op, 2*unit)
		s.mine(1)
		s.start = w.Balance("btc")
		s.open()
		s.ok(w.Order("btc", "sell", 30_000_000))
		s.ok(w.Settle("btc"))
		s.mine(1)
		s.balance("traded and settled through the gateway", -30_000_000-2*txFee, 1500)
	})
}

// A shared watchtower's page lists every wallet it watches, so it needs the
// admin token; wallets still sign up and get their own status with theirs.
func TestSharedTowerPage(t *testing.T) {
	tw, err := NewTower(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	tok := tw.Shared()
	srv := httptest.NewServer(tw.Handler())
	t.Cleanup(srv.Close)
	expectRefused(t, callAs("the watchtower", towerClient, srv.URL, "/v1/tower/view", empty{}, &TowerView{}), "Only the operator")
	admin := func(string, []byte) map[string]string { return map[string]string{hdrAdmin: tok} }
	if err := callHeaders("the watchtower", towerClient, srv.URL, "/v1/tower/view", empty{}, &TowerView{}, admin); err != nil {
		t.Fatal(err)
	}
	wtok := strings.Repeat("ab", 16)
	if err := callAs("the watchtower", towerClient, srv.URL, "/v1/tower/update", TowerUpdate{Wallet: "alice-1", Token: wtok}, &TowerStatus{}); err != nil {
		t.Fatal(err)
	}
	if err := callAs("the watchtower", towerClient, srv.URL, "/v1/tower/status", TowerAuth{"alice-1", wtok}, &TowerStatus{}); err != nil {
		t.Fatal(err)
	}
}
