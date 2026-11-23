package main

// Password-protected save files (seal.go).

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSealRoundTrip(t *testing.T) {
	k, err := newSealKey([]byte("correct horse"))
	if err != nil {
		t.Fatal(err)
	}
	plain := []byte(`{"secret":"abc"}`)
	sealed, err := k.seal(plain)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("abc")) {
		t.Fatal("the sealed file shows the plain text")
	}
	sf, ok := asSealed(sealed)
	if !ok {
		t.Fatal("not recognised as sealed")
	}
	got, k2, err := unseal(sf, []byte("correct horse"))
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("right password: %v %q", err, got)
	}
	if !bytes.Equal(k2.key, k.key) {
		t.Fatal("the key for later saves differs")
	}
	if _, _, err := unseal(sf, []byte("wrong horse")); !errors.Is(err, errWrongPassword) {
		t.Fatalf("wrong password: %v", err)
	}
	sf.Data[0] ^= 1
	if _, _, err := unseal(sf, []byte("correct horse")); !errors.Is(err, errWrongPassword) {
		t.Fatalf("damaged file: %v", err)
	}
	if _, ok := asSealed(plain); ok {
		t.Fatal("a plain file was taken for a sealed one")
	}
}

// TestSaveWithPassword: a wallet and an exchange with passwords trade, save,
// and load again; the files don't show their contents; a wrong or missing
// password loads nothing.
func TestSaveWithPassword(t *testing.T) {
	s := newSplit(t, "btc")
	dir := t.TempDir()
	xk, _ := newSealKey([]byte("exchange-pass"))
	wk, _ := newSealKey([]byte("wallet-pass"))
	s.x.seal, s.w.seal = xk, wk
	xf, wf := filepath.Join(dir, "exchange.json"), filepath.Join(dir, "wallet.json")
	s.x.SaveTo(xf)
	s.w.SaveTo(wf)
	s.open()
	s.ok(s.w.Order("btc", "sell", 30_000_000))
	s.w.Save()
	s.x.Tick()

	for _, f := range []string{xf, wf} {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := asSealed(data); !ok {
			t.Fatalf("%s isn't sealed", f)
		}
		if bytes.Contains(data, []byte(s.w.Account)) {
			t.Fatalf("%s shows the account in plain text", f)
		}
	}

	cfgs := map[string]UTXOConfig{"btc": s.eng.cfg}
	rpc := map[string]string{"btc": s.eng.RPCAddr()}
	load := func(k *sealKey) (*Wallet, error) {
		nw, err := NewWallet("http://127.0.0.1:1", cfgs, rpc)
		if err != nil {
			t.Fatal(err)
		}
		nw.seal = k
		_, err = nw.Load(wf)
		return nw, err
	}
	if _, err := load(nil); err == nil || !strings.Contains(err.Error(), "password") {
		t.Fatalf("no password: %v", err)
	}
	bad, _ := newSealKey([]byte("not-the-one"))
	if _, err := load(bad); !errors.Is(err, errWrongPassword) {
		t.Fatalf("wrong password: %v", err)
	}
	nw, err := load(&sealKey{pass: []byte("wallet-pass")})
	if err != nil {
		t.Fatal(err)
	}
	sameRecord(t, "wallet", s.w.record(), nw.record())

	nx, err := NewExchange(cfgs, rpc)
	if err != nil {
		t.Fatal(err)
	}
	nx.seal = &sealKey{pass: []byte("exchange-pass")}
	if _, err := nx.Load(xf); err != nil {
		t.Fatal(err)
	}
	sameRecord(t, "exchange", s.x.record(), nx.record())
}

// TestPasswordForAtStart: the start-up rules, with CASTLEKEEP_PASSWORD
// standing in for the keyboard.
func TestPasswordForAtStart(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "wallet-bob.json")
	plain := []byte(`{"Version":1}`)
	if err := os.WriteFile(f, plain, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CASTLEKEEP_PASSWORD", "bobs-password")

	// No flags, plain file: stays plain.
	if rk, sk, err := passwordFor(f, "bob", false, false); err != nil || rk != nil || sk != nil {
		t.Fatalf("plain, no flags: %v %v %v", rk, sk, err)
	}
	// -password on a plain file: a key to save with, and a copy of the old file.
	_, sk, err := passwordFor(f, "bob", true, false)
	if err != nil || sk == nil {
		t.Fatalf("protect: %v", err)
	}
	copies, _ := filepath.Glob(f + ".unprotected-*")
	if len(copies) != 1 {
		t.Fatalf("copies of the old file: %v", copies)
	}
	sealed, _ := sk.seal(plain)
	os.WriteFile(f, sealed, 0o600)
	// A sealed file opens with its password, with or without -password.
	rk, sk2, err := passwordFor(f, "bob", false, false)
	if err != nil || rk == nil || sk2 == nil {
		t.Fatalf("sealed: %v", err)
	}
	if got, err := readSaved(f, rk); err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("read back: %v %q", err, got)
	}
	// -remove-password: read with the key, save plain.
	if rk, sk, err := passwordFor(f, "bob", false, true); err != nil || rk == nil || sk != nil {
		t.Fatalf("remove: %v %v", sk, err)
	}
	// The wrong password is refused.
	t.Setenv("CASTLEKEEP_PASSWORD", "someone-else")
	if _, _, err := passwordFor(f, "bob", false, false); !errors.Is(err, errWrongPassword) {
		t.Fatalf("wrong password: %v", err)
	}
}
