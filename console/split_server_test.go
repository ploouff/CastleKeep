package main

// The wallet's -server option (split_server.go) and go run . start
// (split_start.go). Test networks only.

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestServerAddress(t *testing.T) {
	for in, want := range map[string]string{
		"castlekeep.example.com":          "https://castlekeep.example.com",
		"https://castlekeep.example.com/": "https://castlekeep.example.com",
		" http://127.0.0.1:9950 ":         "http://127.0.0.1:9950",
		"":                                "",
	} {
		if got := cleanServer(in); got != want {
			t.Errorf("cleanServer(%q) = %q, want %q", in, got, want)
		}
	}
	c, e, tw := serverURLs("https://s.example")
	if c != "https://s.example/chains" || e != "https://s.example/exchange" || tw != "https://s.example/tower" {
		t.Fatalf("server addresses: %s %s %s", c, e, tw)
	}
}

func TestSavedServer(t *testing.T) {
	data := t.TempDir()
	if s := savedServer(data, "bob"); s != "" {
		t.Fatalf("no file, but a server: %q", s)
	}
	// A server runs the public test networks, so its wallet file is in testnets.
	if err := writeFile(filepath.Join(data, publicFolder, "wallet-bob.json"), []byte(`{"Version":1,"Server":"https://s.example"}`)); err != nil {
		t.Fatal(err)
	}
	if s := savedServer(data, "bob"); s != "https://s.example" {
		t.Fatalf("saved server: %q", s)
	}
	// A file with a password can't be read before it's asked for.
	if err := writeFile(filepath.Join(data, "wallet-carol.json"), []byte(`{"castlekeep":"sealed"}`)); err != nil {
		t.Fatal(err)
	}
	if s := savedServer(data, "carol"); s != "" {
		t.Fatalf("a sealed file gave a server: %q", s)
	}
	// Saved and loaded with the wallet.
	w, err := NewWallet("http://127.0.0.1:1", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "wallet.json")
	w.SaveTo(file)
	w.SetServer("https://s.example")
	w2, _ := NewWallet("http://127.0.0.1:1", nil, nil)
	if _, err := w2.Load(file); err != nil || w2.server != "https://s.example" {
		t.Fatalf("server after loading: %q, %v", w2.server, err)
	}
}

func TestTowerAnswers(t *testing.T) {
	tw, err := NewTower(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// As on a server: the watchtower under /tower.
	m := http.NewServeMux()
	m.Handle("/tower/", http.StripPrefix("/tower", tw.Handler()))
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("Castlekeep test server: test networks only."))
	})
	srv := httptest.NewServer(m)
	defer srv.Close()
	if !towerAnswers(srv.URL + "/tower") {
		t.Fatal("the watchtower wasn't found")
	}
	// A server without one: its catch-all page answers 200, which isn't a watchtower.
	if towerAnswers(srv.URL + "/notower") {
		t.Fatal("a web server's catch-all page was taken for a watchtower")
	}
	if towerAnswers("http://127.0.0.1:1") {
		t.Fatal("nothing listening was taken for a watchtower")
	}
}

func TestStartHelpers(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if portFree(port) {
		t.Fatal("a port in use was free")
	}
	ln.Close()
	if !portFree(port) {
		t.Fatal("a closed port wasn't free")
	}
	os.Setenv("CASTLEKEEP_START", "1")
	if !underStart() {
		t.Fatal("CASTLEKEEP_START=1 not seen")
	}
	os.Unsetenv("CASTLEKEEP_START")
	if underStart() {
		t.Fatal("underStart without CASTLEKEEP_START")
	}
}
