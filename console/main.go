// Castlekeep console: trade from a vault the exchange can't empty, on local
// test chains for Bitcoin, Litecoin and Ethereum.
//
//	go run .
//
// TEST NETWORKS ONLY. Do not use with real coins.
package main

import (
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/build"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
)

//go:embed web/index.html
var webFS embed.FS

func freePort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func exe(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

func goBin(name string) string { return filepath.Join(build.Default.GOPATH, "bin", exe(name)) }

func findAnvil() string {
	if p, err := exec.LookPath(exe("anvil")); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".foundry", "bin", exe("anvil"))
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

func main() {
	// The separate programs: go run . chains | exchange | wallet | watchtower,
	// or go run . start for all of them in one window (split_start.go).
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "chains":
			runChains(os.Args[2:])
			return
		case "exchange":
			runExchange(os.Args[2:])
			return
		case "wallet":
			runWallet(os.Args[2:])
			return
		case "watchtower":
			runWatchtower(os.Args[2:])
			return
		case "start":
			runStart(os.Args[2:])
			return
		}
	}
	btcdPath := flag.String("btcd", goBin("btcd"), "path to btcd (Bitcoin)")
	ltcdPath := flag.String("ltcd", goBin("ltcd"), "path to ltcd (Litecoin)")
	anvilPath := flag.String("anvil", findAnvil(), "path to anvil (Ethereum)")
	ethRPC := flag.String("eth-rpc", "", "use an already-running Ethereum dev node instead of anvil, e.g. http://127.0.0.1:8545")
	hardhat := flag.String("hardhat", defaultHardhatDir(), "folder with Hardhat installed, used to start the other EVM chains' test nodes")
	only := flag.String("chains", "btc,ltc,eth,pol,base,bnb,avax", "chains to run, comma-separated")
	port := flag.Int("port", 8080, "port for the web interface")
	noBrowser := flag.Bool("no-browser", false, "don't open the browser automatically")
	flag.Parse()

	want := map[string]bool{}
	for _, id := range strings.Split(*only, ",") {
		want[strings.TrimSpace(id)] = true
	}
	var engines []Engine
	if want["btc"] {
		engines = append(engines, NewUTXOEngine(btcConfig(*btcdPath)))
	}
	if want["ltc"] {
		engines = append(engines, NewUTXOEngine(ltcConfig(*ltcdPath)))
	}
	for _, c := range evmChains {
		if !want[c.ID] {
			continue
		}
		rpc := ""
		if c.ID == "eth" {
			rpc = *ethRPC
		}
		engines = append(engines, NewEthEngine(evmConfig(c, *anvilPath, *hardhat, rpc)))
	}
	if len(engines) == 0 {
		fmt.Println("No chains picked. Use -chains btc,ltc,eth,pol,base,bnb,avax (or some of them).")
		os.Exit(1)
	}
	// One test account: every chain shares its cash balance.
	acct := NewAccount()
	byID := map[string]Engine{}
	for _, e := range engines {
		byID[e.Info().ID] = e
		e.UseAccount(acct)
		e.Start()
	}
	stopAll := func() {
		for _, e := range engines {
			e.Stop()
		}
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() {
		<-sig
		fmt.Println("\nShutting down the test nodes…")
		stopAll()
		os.Exit(0)
	}()

	mux := consoleMux(engines, func() AccountView { return accountView(acct, engines) },
		func(from, to Engine, sats int64) error { return swap(acct, from, to, sats) })

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			panic(err)
		}
	}
	url := fmt.Sprintf("http://%s", ln.Addr().String())
	fmt.Println("Castlekeep console (test networks only)")
	for _, e := range engines {
		s := e.Snapshot()
		status := "starting"
		if !s.Available {
			status = "not installed: " + s.Unavailable
		}
		fmt.Printf("  %-9s %s\n", e.Info().Name, status)
	}
	fmt.Println("Open:", url)
	fmt.Println("Press Ctrl+C here to stop.")
	if !*noBrowser {
		openBrowser(url)
	}
	if err := http.Serve(ln, mux); err != nil {
		stopAll()
		panic(err)
	}
}

// consoleMux serves the page and its API for a set of chain engines. The
// one-program console and the separate wallet program both use it.
func consoleMux(engines []Engine, account func() AccountView, swapFn func(from, to Engine, sats int64) error) *http.ServeMux {
	byID := map[string]Engine{}
	for _, e := range engines {
		byID[e.Info().ID] = e
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		b, _ := webFS.ReadFile("web/index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(b)
	})
	mux.HandleFunc("/api/chains", func(w http.ResponseWriter, r *http.Request) {
		type entry struct {
			ChainInfo
			Available   bool   `json:"available"`
			Unavailable string `json:"unavailable,omitempty"`
			Ready       bool   `json:"ready"`
		}
		var out []entry
		for _, e := range engines {
			s := e.Snapshot()
			out = append(out, entry{e.Info(), s.Available, s.Unavailable, s.Ready})
		}
		writeJSON(w, out)
	})
	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		e, ok := byID[r.URL.Query().Get("chain")]
		if !ok {
			e = engines[0]
		}
		writeJSON(w, e.Snapshot())
	})
	mux.HandleFunc("/api/account", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, account())
	})
	mux.HandleFunc("/api/action", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var req ActionReq
		json.NewDecoder(r.Body).Decode(&req)
		e, ok := byID[req.Chain]
		if !ok {
			writeJSON(w, map[string]any{"error": "Unknown chain."})
			return
		}
		resp := map[string]any{}
		var err error
		if req.Action == "swap" {
			err = swapFn(e, byID[req.To], req.Sats)
		} else {
			err = e.Do(req)
		}
		if err != nil {
			if errors.Is(err, errUser) {
				resp["error"] = err.Error()
			} else {
				resp["error"] = "Something went wrong: " + err.Error()
			}
		}
		resp["state"] = e.Snapshot()
		writeJSON(w, resp)
	})
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}
