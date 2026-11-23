package main

// Local signing (evm_sign.go): the separate programs on Polygon with the
// node's impersonation switched off, so every transaction the exchange and
// the wallet send must be signed by them, as on a public network.

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRLP(t *testing.T) {
	// Examples from the Ethereum RLP specification.
	cases := []struct {
		got  []byte
		want string
	}{
		{rlpBytes([]byte("dog")), "83646f67"},
		{rlpList(rlpBytes([]byte("cat")), rlpBytes([]byte("dog"))), "c88363617483646f67"},
		{rlpBytes(nil), "80"},
		{rlpList(), "c0"},
		{rlpInt(nil), "80"},
		{rlpBytes([]byte{0x0f}), "0f"},
		{rlpBytes([]byte{0x04, 0x00}), "820400"},
		{rlpBytes([]byte(strings.Repeat("a", 56)))[:2], "b838"},
	}
	for i, c := range cases {
		if got := hexOf(c.got); got != c.want {
			t.Errorf("case %d: %s, want %s", i, got, c.want)
		}
	}
}

func hexOf(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, 2*len(b))
	for _, x := range b {
		out = append(out, digits[x>>4], digits[x&15])
	}
	return string(out)
}

func TestSplitSignedLocally(t *testing.T) {
	s := newSplitEth(t, "pol")
	xe, we := s.x.evm["pol"], s.w.evm["pol"]
	xe.c.signLocal, we.c.signLocal = true, true
	for _, a := range []string{xe.addr, we.addr} {
		if _, err := s.ee.call(s.ee.client+"_stopImpersonatingAccount", a); err != nil {
			t.Fatal(err)
		}
	}
	s.open()
	s.ok(s.w.EthCapacity("pol", oneCoin)) // the exchange signs its fund()
	s.mine(1)
	s.ok(s.w.EthOrder("pol", "sell", 30_000_000))
	s.ok(s.w.EthSettle("pol"))
	s.mine(1)
	if _, _, _, closed := s.w.EthVault("pol"); closed != "settled" {
		t.Fatalf("vault: %q", closed)
	}
	raw, err := s.ee.call("eth_getTransactionByHash", we.vault.closeTx)
	if err != nil {
		t.Fatal(err)
	}
	var tx struct{ Type, From string }
	json.Unmarshal(raw, &tx)
	if tx.Type != "0x2" || !strings.EqualFold(tx.From, we.addr) {
		t.Fatalf("settlement: type %s from %s, want 0x2 from %s", tx.Type, tx.From, we.addr)
	}
	if s.ee.receipt(we.vault.closeTx) != 1 {
		t.Fatal("the settlement didn't succeed")
	}
	s.within("settled after selling 0.3 (less the capacity fee)", -30_000_000-capacityFee(oneCoin))
}
