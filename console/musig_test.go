package main

// MuSig2 key-path spends of the vault (musig.go): settling and splices look
// like an ordinary single-key Taproot payment on the chain, still need both
// sides, refuse a bad partial signature, never sign twice with one nonce,
// and leave vaults opened without MuSig2 working as before.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

func TestMuSig2Keys(t *testing.T) {
	for i := 0; i < 64; i++ {
		k, _ := btcec.NewPrivateKey()
		e := evenKey(k)
		want, _ := schnorr.ParsePubKey(xonly(k))
		if !e.PubKey().IsEqual(want) {
			t.Fatal("evenKey doesn't match the x-only key")
		}
		if !bytes.Equal(xonly(e), xonly(k)) {
			t.Fatal("evenKey changed the x-only key")
		}
	}
	u, _ := btcec.NewPrivateKey()
	x, _ := btcec.NewPrivateKey()
	a, err := musigInternal(xonly(u), xonly(x))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := musigInternal(xonly(x), xonly(u))
	if !a.IsEqual(b) {
		t.Fatal("the combined key depends on the order of the keys")
	}
	// The output key both sides build is the one MuSig2 signs for.
	d, err := newVaultDef(xonly(u), xonly(x), walletScript(u), walletScript(x), 144, 6)
	if err != nil {
		t.Fatal(err)
	}
	if d.V.keyPath() {
		t.Fatal("a vault without MuSig2 has a key path")
	}
	if err := d.setMuSig2(true); err != nil {
		t.Fatal(err)
	}
	keys, _ := musigKeys(d.user, d.exch)
	agg, _, _, _ := musig2.AggregateKeys(keys, true, musig2.WithTaprootKeyTweak(d.V.root()))
	if !bytes.Equal(schnorr.SerializePubKey(agg.FinalKey), d.V.pkScript[2:]) {
		t.Fatal("the vault's output key isn't the MuSig2 key with the Taproot tweak")
	}
}

// TestMuSig2NonceOnce: a nonce signs once; its secret half is wiped.
func TestMuSig2NonceOnce(t *testing.T) {
	u, _ := btcec.NewPrivateKey()
	x, _ := btcec.NewPrivateKey()
	d, _ := newVaultDef(xonly(u), xonly(x), walletScript(u), walletScript(x), 144, 6)
	d.setMuSig2(true)
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(wire.NewTxIn(&wire.OutPoint{Index: 0}, nil, nil))
	tx.AddTxOut(wire.NewTxOut(1000, walletScript(u)))
	ks, err := d.vaultSpend(tx, 0, one(d.V, 5000))
	if err != nil {
		t.Fatal(err)
	}
	un, _ := newMusigNonce(u)
	xn, _ := newMusigNonce(x)
	us, err := ks.sign(u, un, xn.pub())
	if err != nil {
		t.Fatal(err)
	}
	if un.n.SecNonce != ([musig2.SecNonceSize]byte{}) {
		t.Fatal("the secret nonce wasn't wiped after signing")
	}
	if _, err := ks.sign(u, un, xn.pub()); err == nil {
		t.Fatal("a nonce signed twice")
	}
	xs, err := ks.sign(x, xn, un.pub())
	if err != nil {
		t.Fatal(err)
	}
	// Each side checks the other's partial signature; a wrong one fails.
	theirs, ok := ks.verifyPartial(encodePartial(xs), d.exch, xn.pub(), un.pub())
	if !ok {
		t.Fatal("a good partial signature was refused")
	}
	if _, ok := ks.verifyPartial(encodePartial(xs), d.user, xn.pub(), un.pub()); ok {
		t.Fatal("a partial signature was accepted for the wrong key")
	}
	bad := encodePartial(xs)
	bad[5] ^= 1
	if _, ok := ks.verifyPartial(bad, d.exch, xn.pub(), un.pub()); ok {
		t.Fatal("a damaged partial signature was accepted")
	}
	if _, err := ks.combine(us, theirs); err != nil {
		t.Fatal(err)
	}
	// One side alone can't make a valid key-path signature.
	alone, _ := schnorr.Sign(x, ks.msg[:])
	if pk, _ := schnorr.ParsePubKey(ks.out); alone.Verify(ks.msg[:], pk) {
		t.Fatal("one key alone signed for the vault")
	}
}

// onChain fetches a transaction from the node.
func onChain(t *testing.T, c *chainConn, txid string) *wire.MsgTx {
	t.Helper()
	h, err := chainhash.NewHashFromStr(txid)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := c.rpc.GetRawTransaction(h)
	if err != nil {
		t.Fatalf("transaction %s not found: %v", txid, err)
	}
	return tx.MsgTx()
}

// keyPathWitness: one 64-byte signature, as for any single-key Taproot payment.
func keyPathWitness(t *testing.T, what string, tx *wire.MsgTx) {
	t.Helper()
	if w := tx.TxIn[0].Witness; len(w) != 1 || len(w[0]) != 64 {
		t.Fatalf("%s: witness has %d items, want one 64-byte signature", what, len(w))
	}
}

func TestMuSig2SplitBitcoin(t *testing.T)  { runMuSig2Split(t, "btc") }
func TestMuSig2SplitLitecoin(t *testing.T) { runMuSig2Split(t, "ltc") }

func runMuSig2Split(t *testing.T, chain string) {
	t.Run("SettleUsesKeyPath", func(t *testing.T) {
		s := newSplit(t, chain)
		s.open()
		if !s.vault().def.musig || !s.vault().def.V.keyPath() {
			t.Fatal("a new vault has no MuSig2 key path")
		}
		s.ok(s.w.Order(chain, "sell", 60_000_000))
		s.ok(s.w.Settle(chain))
		s.mine(1)
		tx := onChain(t, s.w.chains[chain].c, s.vault().closeTx.TxHash().String())
		keyPathWitness(t, "settlement", tx)
		if s.w.chains[chain].c.confs(tx) < 1 {
			t.Fatal("the settlement didn't confirm")
		}
		s.balance("after settling", -60_000_000-2*txFee, 1500)
	})

	t.Run("SpliceUsesKeyPath", func(t *testing.T) {
		s := newSplit(t, chain)
		s.open()
		s.ok(s.w.Capacity(chain, oneCoin))
		s.mine(1)
		keyPathWitness(t, "splice", onChain(t, s.w.chains[chain].c, s.vault().spliceTx.TxHash().String()))
		if !s.vault().spliceFinished {
			t.Fatal("the splice didn't finish")
		}
		s.cashFor(80_000_000)
		s.ok(s.w.Order(chain, "buy", 80_000_000))
		// A second splice, then settle: all through the key path.
		s.ok(s.w.Capacity(chain, oneCoin/2))
		s.mine(1)
		keyPathWitness(t, "second splice", onChain(t, s.w.chains[chain].c, s.vault().spliceTx.TxHash().String()))
		s.ok(s.w.Settle(chain))
		s.mine(1)
		keyPathWitness(t, "settlement after splices", onChain(t, s.w.chains[chain].c, s.vault().closeTx.TxHash().String()))
		s.balance("bought 0.8 and settled", 79_800_000-capacityFee(oneCoin/2)-2*txFee, 1500)
	})

	t.Run("BadPartialSignatureRefused", func(t *testing.T) {
		s := newSplit(t, chain)
		s.open()
		s.x.tamper = true // the exchange's next partial signature is damaged
		s.refused(s.w.Settle(chain), "MuSig2 signature")
		if s.vault().closed != "" {
			t.Fatal("the vault was closed with a bad signature")
		}
		// Asking again uses new nonces on both sides and works.
		s.ok(s.w.Settle(chain))
		s.mine(1)
		keyPathWitness(t, "settlement", onChain(t, s.w.chains[chain].c, s.vault().closeTx.TxHash().String()))
		s.balance("settled", -2*txFee, 1500)
	})

	t.Run("NoNoncesSaved", func(t *testing.T) {
		s := newSplit(t, chain)
		s.open()
		// A splice the wallet never finishes: the exchange has signed its
		// part. Its saved file holds none of it, and after a restart the
		// half-done splice is gone.
		n, err := newMusigNonce(s.vault().key)
		s.ok(err)
		_, uh := newSecret()
		var off CapacityOffer
		s.ok(call(s.w.base, "/v1/capacity", CapacityReq{VaultID: s.vault().id, Sats: oneCoin, UHash: uh, UserNonce: n.pub()}, &off))
		if len(off.SpliceNonce) != musig2.PubNonceSize || len(off.SplicePartial) != 32 {
			t.Fatalf("offer: nonce %d bytes, partial %d bytes", len(off.SpliceNonce), len(off.SplicePartial))
		}
		data, _ := json.Marshal(s.x.record())
		for _, b := range [][]byte{off.SpliceNonce, off.SplicePartial, n.pub()} {
			if bytes.Contains(data, []byte(base64.StdEncoding.EncodeToString(b))) {
				t.Fatal("the exchange's saved data holds a nonce or partial signature")
			}
		}
		file := t.TempDir() + "/exchange.json"
		s.x.SaveTo(file)
		nx, _ := NewExchange(map[string]UTXOConfig{chain: s.eng.cfg}, map[string]string{chain: s.eng.RPCAddr()})
		if _, err := nx.Load(file); err != nil {
			t.Fatal(err)
		}
		if v := nx.vaults[s.vault().id]; v == nil || v.pend != nil || !v.def.musig {
			t.Fatal("after loading: the vault is missing, still has a half-done splice, or lost MuSig2")
		}
	})

	t.Run("OldVaultsUseTheScript", func(t *testing.T) {
		newVaultsMuSig2 = false // an exchange from before MuSig2
		defer func() { newVaultsMuSig2 = true }()
		s := newSplit(t, chain)
		s.open()
		if s.vault().def.musig || s.vault().def.V.keyPath() {
			t.Fatal("an old-style vault has a key path")
		}
		s.ok(s.w.Capacity(chain, oneCoin/2))
		s.mine(1)
		if w := onChain(t, s.w.chains[chain].c, s.vault().spliceTx.TxHash().String()).TxIn[0].Witness; len(w) != 4 {
			t.Fatalf("old-style splice: %d witness items, want 4 (two signatures, script, control block)", len(w))
		}
		// Saved and loaded, it stays an old-style vault.
		file := t.TempDir() + "/wallet.json"
		s.w.SaveTo(file)
		nw, _ := NewWallet(s.w.base, map[string]UTXOConfig{chain: s.eng.cfg}, map[string]string{chain: s.eng.RPCAddr()})
		if _, err := nw.Load(file); err != nil {
			t.Fatal(err)
		}
		if v := nw.chains[chain].vault; v == nil || v.def.musig || !bytes.Equal(v.def.V.pkScript, s.vault().def.V.pkScript) {
			t.Fatal("the loaded old-style vault changed")
		}
		s.ok(s.w.Settle(chain))
		s.mine(1)
		if w := onChain(t, s.w.chains[chain].c, s.vault().closeTx.TxHash().String()).TxIn[0].Witness; len(w) != 4 {
			t.Fatalf("old-style settlement: %d witness items, want 4", len(w))
		}
		s.balance("settled", -capacityFee(oneCoin/2)-2*txFee, 1500)
	})
}

// TestMuSig2Console: the one-program console settles and splices through
// the key path too.
func TestMuSig2Console(t *testing.T) {
	for _, chain := range []string{"btc", "ltc"} {
		t.Run(chain, func(t *testing.T) {
			h := newHarness(t, chain)
			e := h.e.(*UTXOEngine)
			utxoLock(h)
			h.do(ActionReq{Action: "capacity", Sats: oneCoin / 2})
			h.mine(1)
			splice := e.vault.fundTxid
			h.order("sell", 30_000_000)
			h.do(ActionReq{Action: "settle"})
			h.expectAccepted("cooperative settlement")
			h.mine(1)
			h.expectStatus("closed")
			c := &chainConn{rpc: e.rpc}
			keyPathWitness(t, "console splice", onChain(t, c, splice))
			keyPathWitness(t, "console settlement", onChain(t, c, e.vault.closeTxid))
			if !e.vault.V.keyPath() {
				t.Fatal("the console's vault has no key path")
			}
			// The exit request still uses the 2-of-2 script (pre-signed).
			if w := e.vault.exitTx.TxIn[0].Witness; len(w) != 4 {
				t.Fatalf("exit request: %d witness items, want the script path", len(w))
			}
		})
	}
}
