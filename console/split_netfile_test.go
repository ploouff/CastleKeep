package main

// The public test networks keep their files apart from the private test
// chains' (netFile, split_save.go). Test networks only.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFilesPerNetwork(t *testing.T) {
	data := t.TempDir()
	write := func(name, tag string) {
		t.Helper()
		if err := writeFile(filepath.Join(data, name), []byte(`{"Version":1,"Chains":{"btc":{"Tag":"`+tag+`"}}}`)); err != nil {
			t.Fatal(err)
		}
	}
	exists := func(path string) bool { _, err := os.Stat(path); return err == nil }
	root := func(name string) string { return filepath.Join(data, name) }
	pub := func(name string) string { return filepath.Join(data, publicFolder, name) }

	// Alice's wallet was made on signet back when everything shared one folder.
	write("wallet-alice.json", signetBlock1)
	// Bob's on the private test chains.
	write("wallet-bob.json", strings.Repeat("ab", 32))

	// On the private test chains, Alice's file moves to the testnets folder
	// and a new one starts; Bob's stays where it is.
	if got := netFile(data, "wallet-alice.json", false); got != root("wallet-alice.json") {
		t.Fatalf("private chains: Alice's file is %s", got)
	}
	if exists(root("wallet-alice.json")) || !exists(pub("wallet-alice.json")) {
		t.Fatal("Alice's signet file wasn't moved to the testnets folder")
	}
	if got := netFile(data, "wallet-bob.json", false); got != root("wallet-bob.json") || !exists(got) {
		t.Fatal("Bob's file for the private chains was moved")
	}

	// On the public test networks Alice's file is found in its new place, and
	// Bob gets a new one there (his private-chains file is left alone).
	if got := netFile(data, "wallet-alice.json", true); got != pub("wallet-alice.json") || !exists(got) {
		t.Fatalf("public networks: Alice's file is %s", got)
	}
	if got := netFile(data, "wallet-bob.json", true); got != pub("wallet-bob.json") || !exists(root("wallet-bob.json")) {
		t.Fatalf("public networks: Bob's file is %s, or his private one moved", got)
	}

	// If both places have a file, nothing is moved: the program says why it
	// can't use the one it found.
	write("exchange.json", signetBlock1)
	if err := writeFile(pub("exchange.json"), []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	netFile(data, "exchange.json", false)
	if !exists(root("exchange.json")) {
		t.Fatal("a signet file was moved over one already in the testnets folder")
	}

	// A file with a password can't be read, so it stays put.
	if err := writeFile(root("wallet-carol.json"), []byte(`{"castlekeep":"sealed"}`)); err != nil {
		t.Fatal(err)
	}
	netFile(data, "wallet-carol.json", false)
	if !exists(root("wallet-carol.json")) {
		t.Fatal("a file with a password was moved")
	}
}

func TestOtherNetworkMessage(t *testing.T) {
	err := sameChain("wallet-alice.json", "btc", signetBlock1, &chainConn{tagged: strings.Repeat("ab", 32)})
	if err == nil || !strings.Contains(err.Error(), "-network testnets") {
		t.Fatalf("a signet file on the private chains: %v", err)
	}
	err = sameChain("wallet-alice.json", "btc", strings.Repeat("ab", 32), &chainConn{tagged: signetBlock1})
	if err == nil || !strings.Contains(err.Error(), "without -network") {
		t.Fatalf("a private-chains file on signet: %v", err)
	}
	err = sameChain("wallet-alice.json", "btc", strings.Repeat("ab", 32), &chainConn{tagged: strings.Repeat("cd", 32)})
	if err == nil || !strings.Contains(err.Error(), "started fresh") {
		t.Fatalf("a file from before the chains were started fresh: %v", err)
	}
}
