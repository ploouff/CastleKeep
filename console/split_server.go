package main

// The wallet's -server option: a shared server (SERVER.md) in one address
// instead of three. On it the chains are at /chains, the exchange at
// /exchange and the watchtower at /tower. The wallet remembers the server, so
// the next start needs only  go run . wallet -name bob.
// Test networks only.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// serverURLs are the three programs' addresses on a server.
func serverURLs(server string) (chains, exch, tower string) {
	return server + "/chains", server + "/exchange", server + "/tower"
}

// cleanServer turns what was typed into a server address: https:// unless it
// says otherwise, without a trailing slash.
func cleanServer(s string) string {
	s = strings.TrimRight(strings.TrimSpace(s), "/")
	if s != "" && !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
		s = "https://" + s
	}
	return s
}

// savedServer is the server a wallet's file remembers, if it does. (A file
// with a password can't be read before the password is asked for, so it
// remembers none: start it with -server each time.)
func savedServer(data, name string) string {
	for _, dir := range []string{filepath.Join(data, publicFolder), data} {
		b, err := os.ReadFile(filepath.Join(dir, walletFile(name)))
		if err != nil {
			continue
		}
		var f struct{ Server string }
		if json.Unmarshal(b, &f) == nil && f.Server != "" {
			return f.Server
		}
	}
	return ""
}

// towerAnswers says whether a watchtower answers at url: it refuses a status
// request for a wallet it doesn't know, in its own words. (A web server's
// catch-all page, which may answer 200 with text, isn't one.)
func towerAnswers(url string) bool {
	body, _ := json.Marshal(TowerAuth{Wallet: "probe", Token: strings.Repeat("0", 32)})
	client := &http.Client{Timeout: 10 * time.Second}
	r, err := client.Post(url+"/v1/tower/status", "application/json", bytes.NewReader(body))
	if err != nil {
		return false
	}
	defer r.Body.Close()
	var e apiErr
	b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	return r.StatusCode == http.StatusBadRequest && json.Unmarshal(b, &e) == nil && e.Error != ""
}

// SetServer records the server this wallet uses ("" for the programs on this
// computer).
func (w *Wallet) SetServer(server string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.server = server
	w.save()
}

// exchDown says, for the page, why the exchange didn't answer: unreachable
// (with its address), or its own refusal.
func exchDown(err error, addr string) string {
	if strings.Contains(err.Error(), "Couldn't reach") {
		return "Can't reach the exchange at " + addr + "."
	}
	return err.Error()
}
