// Castlekeep console: a local web interface for the self-custodial exchange
// vault, running on a private Bitcoin regtest chain.
//
//	go run .
//
// TEST NETWORK ONLY. Do not use with real bitcoin.
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
	"sort"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/rpcclient"
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

func defaultBtcd() string {
	name := "btcd"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(build.Default.GOPATH, "bin", name)
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
	btcdPath := flag.String("btcd", defaultBtcd(), "path to the btcd binary")
	port := flag.Int("port", 8080, "port for the web interface")
	noBrowser := flag.Bool("no-browser", false, "don't open the browser automatically")
	flag.Parse()

	if _, err := os.Stat(*btcdPath); err != nil {
		fmt.Printf("Can't find btcd at %s\nInstall it with:  go install github.com/btcsuite/btcd@v0.24.2\n", *btcdPath)
		os.Exit(1)
	}

	app := newApp()
	minerAddr, _ := btcutil.NewAddressTaproot(app.minerPk[2:], &chaincfg.RegressionNetParams)

	dir, err := os.MkdirTemp("", "castlekeep-console-")
	if err != nil {
		panic(err)
	}
	rpcAddr := fmt.Sprintf("127.0.0.1:%d", freePort())
	node := exec.Command(*btcdPath, "--regtest", "--notls", "--txindex",
		"--datadir="+filepath.Join(dir, "data"), "--logdir="+filepath.Join(dir, "logs"),
		"--rpcuser=castlekeep", "--rpcpass=castlekeep",
		"--rpclisten="+rpcAddr, fmt.Sprintf("--listen=127.0.0.1:%d", freePort()),
		"--miningaddr="+minerAddr.EncodeAddress())
	if err := node.Start(); err != nil {
		fmt.Println("Couldn't start btcd:", err)
		os.Exit(1)
	}
	cleanup := func() {
		if node.Process != nil {
			node.Process.Kill()
			node.Wait()
		}
		os.RemoveAll(dir)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() {
		<-sig
		fmt.Println("\nShutting down the test node…")
		cleanup()
		os.Exit(0)
	}()

	app.rpc, err = rpcclient.New(&rpcclient.ConnConfig{
		Host: rpcAddr, User: "castlekeep", Pass: "castlekeep", HTTPPostMode: true, DisableTLS: true,
	}, nil)
	if err != nil {
		panic(err)
	}

	go func() {
		for i := 0; ; i++ {
			if _, err := app.rpc.GetBlockCount(); err == nil {
				break
			}
			if i > 120 {
				app.mu.Lock()
				app.initErr = "The test node didn't start. Close this window and run the program again."
				app.mu.Unlock()
				return
			}
			time.Sleep(500 * time.Millisecond)
		}
		if err := app.initChain(); err != nil {
			app.mu.Lock()
			app.initErr = err.Error()
			app.mu.Unlock()
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		b, _ := webFS.ReadFile("web/index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(b)
	})
	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		app.mu.Lock()
		defer app.mu.Unlock()
		writeJSON(w, app.snapshot())
	})
	mux.HandleFunc("/api/action", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Action string `json:"action"`
			Sats   int64  `json:"sats"`
			N      int    `json:"n"`
			USD    int64  `json:"usd"`
			Mode   string `json:"mode"`
			Side   string `json:"side"`
			By     string `json:"by"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		app.mu.Lock()
		defer app.mu.Unlock()
		resp := map[string]any{}
		if !app.ready {
			resp["error"] = "The test chain is still starting up."
		} else if err := app.do(req.Action, req.Sats, req.N, req.USD, req.Mode, req.Side, req.By); err != nil {
			if errors.Is(err, errUser) {
				resp["error"] = err.Error()
			} else {
				resp["error"] = "Something went wrong: " + err.Error()
			}
		}
		resp["state"] = app.snapshot()
		writeJSON(w, resp)
	})

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			panic(err)
		}
	}
	url := fmt.Sprintf("http://%s", ln.Addr().String())
	fmt.Println("Castlekeep console (test network only)")
	fmt.Println("Open:", url)
	fmt.Println("Press Ctrl+C here to stop.")
	if !*noBrowser {
		openBrowser(url)
	}
	if err := http.Serve(ln, mux); err != nil {
		cleanup()
		panic(err)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

func (a *App) do(action string, sats int64, n int, usd int64, mode, side, by string) error {
	switch action {
	case "faucet":
		return a.faucet()
	case "mine":
		return a.mine(n)
	case "lock":
		return a.lock(sats)
	case "order":
		return a.order(side, sats)
	case "settle":
		return a.settle()
	case "exit":
		return a.requestExit(by)
	case "claim":
		return a.claim()
	case "steal":
		return a.steal()
	case "mode":
		return a.setMode(mode)
	case "price":
		return a.setPrice(usd)
	}
	return userErr("Unknown action.")
}

// ------------------------------------------------------------------ snapshot

type StateView struct {
	Index    int    `json:"index"`
	Label    string `json:"label"`
	UserSats int64  `json:"userSats"`
	ExchSats int64  `json:"exchSats"`
	Height   int64  `json:"height"`
}

type VaultView struct {
	Status      string      `json:"status"`
	Address     string      `json:"address"`
	Locked      int64       `json:"locked"`
	UserSats    int64       `json:"userSats"`
	ExchSats    int64       `json:"exchSats"`
	States      []StateView `json:"states"`
	FundTxid    string      `json:"fundTxid"`
	ExitTxid    string      `json:"exitTxid,omitempty"`
	ExitSent    bool        `json:"exitSent"`
	ExitHeight  int64       `json:"exitHeight,omitempty"`
	BlocksLeft  int64       `json:"blocksLeft"`
	ExitDelay   int64       `json:"exitDelay"`
	CloseTxid   string      `json:"closeTxid,omitempty"`
	CloseKind   string      `json:"closeKind,omitempty"`
	ScriptVault string      `json:"scriptVault"`
	ScriptExit  string      `json:"scriptExit"`
}

type Snapshot struct {
	Ready      bool       `json:"ready"`
	Progress   string     `json:"progress"`
	InitErr    string     `json:"initErr,omitempty"`
	Height     int64      `json:"height"`
	Mempool    int        `json:"mempool"`
	Price      int64      `json:"price"`
	Mode       string     `json:"mode"`
	WalletSats int64      `json:"walletSats"`
	WalletPend int64      `json:"walletPending"`
	CashCents  int64      `json:"cashCents"`
	ExchSats   int64      `json:"exchWalletSats"`
	Vault      *VaultView `json:"vault"`
	VaultsDone int        `json:"vaultsDone"`
	Events     []Event    `json:"events"`
	Txs        []*TxInfo  `json:"txs"`
}

func (a *App) snapshot() Snapshot {
	s := Snapshot{Ready: a.ready, Progress: a.progress, InitErr: a.initErr, Price: a.price, Mode: a.mode,
		CashCents: a.cashCents, VaultsDone: a.vaultsDone, Events: a.events}
	if !a.ready {
		return s
	}
	s.Height = a.height()
	if mp, err := a.rpc.GetRawMempool(); err == nil {
		s.Mempool = len(mp)
	}

	keep := a.userUtxos[:0]
	for _, u := range a.userUtxos {
		r, err := a.rpc.GetTxOut(&u.op.Hash, u.op.Index, true)
		if err != nil || r == nil {
			continue
		}
		keep = append(keep, u)
		if r.Confirmations > 0 {
			s.WalletSats += u.amt
		} else {
			s.WalletPend += u.amt
		}
	}
	a.userUtxos = keep
	s.ExchSats = a.walletBalance(&a.exchUtxos)

	for _, t := range a.txs {
		if t.Status == "mempool" {
			a.confs(t.Txid)
		}
		s.Txs = append(s.Txs, t)
	}
	sort.Slice(s.Txs, func(i, j int) bool { return s.Txs[i].Seq > s.Txs[j].Seq })

	if v := a.vault; v != nil {
		vv := &VaultView{
			Status: a.vaultStatus(), Locked: v.vaultAmt, FundTxid: v.fundTxid, ExitSent: v.exitSent,
			CloseTxid: v.closeTxid, CloseKind: v.closeKind, ExitDelay: exitDelay, BlocksLeft: exitDelay,
			ScriptVault: v.friendlyScript(v.multisigLeaf.Script), ScriptExit: v.friendlyScript(v.delayLeaf.Script),
		}
		if addr, err := btcutil.NewAddressTaproot(v.V.pkScript[2:], &chaincfg.RegressionNetParams); err == nil {
			vv.Address = addr.EncodeAddress()
		}
		l := v.Latest()
		vv.UserSats, vv.ExchSats = l.UserSats, l.ExchSats
		for _, st := range v.States {
			vv.States = append(vv.States, StateView{st.Index, st.Label, st.UserSats, st.ExchSats, st.Height})
		}
		if v.exitTx != nil {
			vv.ExitTxid = v.exitTx.TxHash().String()
		}
		if v.exitSent && a.confs(vv.ExitTxid) > 0 {
			vv.ExitHeight = a.confirmedAt[vv.ExitTxid]
			left := vv.ExitHeight + exitDelay - (s.Height + 1)
			if left < 0 {
				left = 0
			}
			vv.BlocksLeft = left
		}
		s.Vault = vv
	}
	return s
}
