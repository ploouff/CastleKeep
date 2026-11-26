package main

// The separate watchtower: the wallet keeps it up to date while trading, then
// the wallet "closes" (it stops looking at the chain) and the watchtower alone
// has to protect it. Test networks only.

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

// withTower starts a watchtower for the split test's chain and points the
// wallet at it.
func (s *splitEnv) withTower() (*Tower, *httptest.Server) {
	s.t.Helper()
	tw, err := NewTower(map[string]UTXOConfig{s.chain: s.eng.cfg}, map[string]string{s.chain: s.eng.RPCAddr()})
	s.ok(err)
	srv := httptest.NewServer(tw.Handler())
	s.t.Cleanup(srv.Close)
	s.w.UseTower(srv.URL)
	return tw, srv
}

// mineAway mines n blocks while the wallet is closed: only the exchange and
// the watchtower look.
func (s *splitEnv) mineAway(tw *Tower, n int) {
	s.t.Helper()
	for i := 0; i < n; i++ {
		if err := s.eng.Mine(1); err != nil {
			s.t.Fatal(err)
		}
		s.x.Tick()
		tw.Tick()
	}
}

func towerSaid(tw *Tower, wallet, contains string) bool {
	tw.mu.Lock()
	defer tw.mu.Unlock()
	if w := tw.wallets[wallet]; w != nil {
		for _, e := range w.Events {
			if strings.Contains(e, contains) {
				return true
			}
		}
	}
	return false
}

func TestTowerBitcoin(t *testing.T)  { runTower(t, "btc") }
func TestTowerLitecoin(t *testing.T) { runTower(t, "ltc") }

func runTower(t *testing.T, chain string) {
	t.Run("PenaltySweepWhileWalletClosed", func(t *testing.T) {
		s := newSplit(t, chain)
		tw, _ := s.withTower()
		file := filepath.Join(t.TempDir(), "watchtower.json")
		tw.SaveTo(file)
		s.open()
		s.ok(s.w.Order(chain, "sell", 50_000_000))
		s.ok(s.w.Order(chain, "buy", 50_000_000))
		// State #1 (after the sale) is revoked: the watchtower holds its sweep.
		if st := s.w.towerStat; st == nil || st.Vaults != 1 || st.Ready != 1 {
			t.Fatalf("the watchtower's status: %+v", st)
		}
		if note := s.w.towerNote(chain); !strings.Contains(note, "watching 1 vault") {
			t.Fatalf("page note: %q", note)
		}
		s.ok(s.w.Admin(chain, "malicious", 0))
		s.ok(s.w.Exit(chain))
		// The wallet closes here. The watchtower restarts from its file too.
		tw2, err := NewTower(map[string]UTXOConfig{chain: s.eng.cfg}, map[string]string{chain: s.eng.RPCAddr()})
		s.ok(err)
		loaded, err := tw2.Load(file)
		s.ok(err)
		if !loaded {
			t.Fatal("the watchtower's file wasn't there")
		}
		s.mineAway(tw2, 1) // the exit confirms; the exchange publishes old state #1
		if v := s.x.vaults[s.vault().id]; v.published == nil || v.published.Index != 1 {
			t.Fatal("expected the exchange to publish old state #1")
		}
		s.mineAway(tw2, 1) // old state #1 confirms; the watchtower sends the penalty sweep
		if !towerSaid(tw2, s.w.Account, "penalty sweep") {
			t.Fatalf("the watchtower didn't sweep: %v", tw2.wallets[s.w.Account].Events)
		}
		s.mineAway(tw2, 1)
		// The wallet comes back and sees what happened.
		s.w.Tick()
		if v := s.vault(); v.sweepKind != "penalty" {
			t.Fatalf("sweep %q, want penalty", v.sweepKind)
		}
		s.mine(int(s.vault().def.d2)) // the exchange can't take its share any more
		s.balance("whole vault back", -4*txFee, 1500)
	})

	t.Run("PublishesNewestStateWhileWalletClosed", func(t *testing.T) {
		s := newSplit(t, chain)
		tw, _ := s.withTower()
		s.open()
		s.ok(s.w.Capacity(chain, oneCoin))
		s.mine(1) // the splice confirms
		s.cashFor(80_000_000)
		s.ok(s.w.Order(chain, "buy", 80_000_000)) // 1.798 now, more than the 1 deposited
		s.ok(s.w.Admin(chain, "offline", 0))
		s.ok(s.w.Exit(chain))
		s.mineAway(tw, 1) // the exit confirms; nobody publishes, so the watchtower does
		if !towerSaid(tw, s.w.Account, "published your newest state #2") {
			t.Fatalf("the watchtower didn't publish: %v", tw.wallets[s.w.Account].Events)
		}
		s.mineAway(tw, 1)
		s.w.Tick() // back
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

	t.Run("NoTradeWhileWatchtowerDown", func(t *testing.T) {
		s := newSplit(t, chain)
		_, srv := s.withTower()
		s.open()
		srv.Close()
		s.refused(s.w.Order(chain, "sell", 10_000_000), "watchtower couldn't be reached")
		if n := len(s.vault().states); n != 1 {
			t.Fatalf("%d states after a stopped trade", n)
		}
		if x := s.x.vaults[s.vault().id]; x.states[0].uSecret != nil {
			t.Fatal("the wallet handed over its secret for state #0 although the watchtower didn't have the new state")
		}
		s.refused(s.w.Swap(chain, "eth", 1000), "swap wasn't started") // checked before anything else is asked
		// A watchtower again: trading carries on.
		tw2, err := NewTower(map[string]UTXOConfig{chain: s.eng.cfg}, map[string]string{chain: s.eng.RPCAddr()})
		s.ok(err)
		srv2 := httptest.NewServer(tw2.Handler())
		t.Cleanup(srv2.Close)
		s.w.UseTower(srv2.URL)
		s.ok(s.w.Order(chain, "sell", 10_000_000))
		// The wallet remembers its watchtower and token.
		file := filepath.Join(t.TempDir(), "wallet.json")
		s.w.SaveTo(file)
		w2, err := NewWallet(s.w.base, map[string]UTXOConfig{chain: s.eng.cfg}, map[string]string{chain: s.eng.RPCAddr()})
		s.ok(err)
		_, err = w2.Load(file)
		s.ok(err)
		if w2.TowerURL() != srv2.URL || w2.towerToken != s.w.towerToken {
			t.Fatal("the saved wallet forgot its watchtower")
		}
		// Stopping: the watchtower forgets the wallet, and trading no longer waits for it.
		s.ok(s.w.StopTower())
		if len(tw2.View().Wallets) != 0 {
			t.Fatal("the watchtower still holds the wallet's data")
		}
		srv2.Close()
		s.ok(s.w.Order(chain, "sell", 10_000_000))
	})
}

// A wallet with no vault yet signs up on its first tick, and the page says so
// (it used to only ask for its status, which a new watchtower refused).
func TestTowerSignsUpNewWallet(t *testing.T) {
	s := newSplit(t, "btc")
	tw, srv := s.withTower()
	s.w.Tick()
	if s.w.towerErr != "" || s.w.towerStat == nil {
		t.Fatalf("not signed up: %q", s.w.towerErr)
	}
	if len(tw.View().Wallets) != 1 {
		t.Fatal("the watchtower doesn't list the wallet")
	}
	if note := s.w.towerNote("btc"); !strings.Contains(note, "nothing to watch") {
		t.Fatalf("page note: %q", note)
	}
	// Down: the page says it can't be reached. Refused: it says so instead.
	srv.Close()
	s.w.towerSend(true)
	if note := s.w.towerNote("btc"); !strings.Contains(note, "can't be reached") {
		t.Fatalf("page note while down: %q", note)
	}
	s.w.towerErr = "The watchtower said: Wrong token for this wallet"
	if note := s.w.towerNote("btc"); !strings.Contains(note, "refused the last update") {
		t.Fatalf("page note after a refusal: %q", note)
	}
}

// The watchtower never goes back to an older state, and only takes updates
// from the wallet that signed up with it.
func TestTowerRules(t *testing.T) {
	tw, err := NewTower(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(tw.Handler())
	t.Cleanup(srv.Close)
	exit := chainhash.Hash{1}
	spend := func(prev wire.OutPoint) string {
		tx := wire.NewMsgTx(2)
		tx.AddTxIn(wire.NewTxIn(&prev, nil, nil))
		tx.AddTxOut(wire.NewTxOut(1000, []byte{0x51}))
		return txHex(tx)
	}
	tok := strings.Repeat("ab", 16)
	put := func(token string, g TowerGen, b ...*TowerBreach) error {
		return callAs("the watchtower", towerClient, srv.URL, "/v1/tower/update", TowerUpdate{Wallet: "alice-1", Token: token,
			UTXO: []*TowerUTXOVault{{Chain: "btc", VaultID: "v1", Gens: []*TowerGen{&g}, Breaches: b}}}, &TowerStatus{})
	}
	latest := func() *TowerGen { return tw.wallets["alice-1"].UTXO["btc/v1"].Gens[0] }
	if err := put(tok, TowerGen{ExitTxid: exit.String(), LatestIndex: 3}); err != nil {
		t.Fatal(err)
	}
	// An older state with a payout must not replace the newer one.
	if err := put(tok, TowerGen{ExitTxid: exit.String(), LatestIndex: 2, Latest: spend(wire.OutPoint{Hash: exit})}); err != nil {
		t.Fatal(err)
	}
	if g := latest(); g.LatestIndex != 3 || g.Latest != "" {
		t.Fatalf("went back to an older state: %+v", g)
	}
	if err := put(tok, TowerGen{ExitTxid: exit.String(), LatestIndex: 4, Latest: spend(wire.OutPoint{Hash: exit})}); err != nil {
		t.Fatal(err)
	}
	if g := latest(); g.LatestIndex != 4 || g.Latest == "" {
		t.Fatalf("didn't take the newer state: %+v", g)
	}
	err = put(strings.Repeat("cd", 16), TowerGen{ExitTxid: exit.String(), LatestIndex: 9})
	if err == nil || !strings.Contains(err.Error(), "Wrong token") {
		t.Fatalf("another token was accepted: %v", err)
	}
	err = put(tok, TowerGen{ExitTxid: exit.String(), LatestIndex: 5, Latest: spend(wire.OutPoint{Hash: chainhash.Hash{2}})})
	if err == nil || !strings.Contains(err.Error(), "doesn't spend its exit request") {
		t.Fatalf("a state that doesn't spend the exit was accepted: %v", err)
	}
	bad := &TowerBreach{Index: 1, StateTxid: chainhash.Hash{3}.String(), Sweep: spend(wire.OutPoint{Hash: chainhash.Hash{4}})}
	err = put(tok, TowerGen{ExitTxid: exit.String(), LatestIndex: 4}, bad)
	if err == nil || !strings.Contains(err.Error(), "doesn't spend that state") {
		t.Fatalf("a sweep of another transaction was accepted: %v", err)
	}
	if g := latest(); g.LatestIndex != 4 || g.Latest == "" {
		t.Fatalf("a refused update changed something: %+v", g)
	}
}

func TestTowerEthereum(t *testing.T) { runTowerEVM(t, "eth") }
func TestTowerBase(t *testing.T)     { runTowerEVM(t, "base") }

func runTowerEVM(t *testing.T, chain string) {
	s := newSplitEth(t, chain)
	tw, err := NewTower(nil, nil)
	s.ok(err)
	s.ok(s.ee.FundAddress(tw.AttachEVM(chain, s.ee.Endpoint()), unit))
	srv := httptest.NewServer(tw.Handler())
	t.Cleanup(srv.Close)
	s.w.UseTower(srv.URL)
	s.open()
	s.ok(s.w.EthOrder(chain, "sell", 50_000_000))
	s.ok(s.w.EthOrder(chain, "buy", 50_000_000))
	s.ok(s.w.Admin(chain, "malicious", 0))
	s.ok(s.w.EthExit(chain))
	// The wallet closes here.
	away := func() {
		s.ok(s.ee.MineEth(1, 0))
		s.x.Tick()
		tw.Tick()
	}
	away() // the exit request is in; the exchange submits old state #1, which pays it most
	away() // that's in; the watchtower submits state #2
	if !towerSaid(tw, s.w.Account, "submitted your newest state #2") {
		t.Fatalf("the watchtower didn't submit: %v", tw.wallets[s.w.Account].Events)
	}
	away()
	cv, err := s.ee.readVaultID(s.w.evm[chain].vault.id)
	s.ok(err)
	if !cv.hasBest || cv.bestNonce != 2 {
		t.Fatalf("the contract holds state #%d, want #2", cv.bestNonce)
	}
	// The wallet comes back after the window and takes its money.
	s.finish()
	s.within("kept the newest state", 0)
}
