package main

// The three separate programs (Bitcoin and Litecoin for now):
//
//	go run . chains     starts the local test nodes, with a faucet and a mine button
//	go run . exchange   the exchange server: only the exchange's keys
//	go run . wallet     your wallet with the usual page: only your keys
//
// Start them in that order, each in its own Command Prompt window.
// Test networks only. Not audited.

import (
	"embed"
	"encoding/hex"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
)

//go:embed web/exchange.html
var exchangeFS embed.FS

// splitConfigs are the chains the separate programs support so far.
func splitConfigs() map[string]UTXOConfig {
	return map[string]UTXOConfig{"btc": btcConfig(goBin("btcd")), "ltc": ltcConfig(goBin("ltcd"))}
}

func listen(port int) (net.Listener, string) {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		fmt.Printf("Port %d is taken; pick another with -port.\n", port)
		os.Exit(1)
	}
	return ln, "http://" + ln.Addr().String()
}

// ------------------------------------------------------------------ chains

type ChainStatus struct {
	ID, RPC, Progress string
	Ready             bool
	Eth               *EthEndpoint // Ethereum only: node, contract and exit window
}
type FaucetReq struct {
	Chain  string
	Script []byte
	Addr   string // Ethereum address
	Sats   int64
}
type FaucetResp struct {
	Op  wire.OutPoint
	Amt int64
}
type MineReq struct {
	Chain string
	N     int
	Secs  int64 // Ethereum: skip the clock ahead instead
}

func runChains(args []string) {
	fs := flag.NewFlagSet("chains", flag.ExitOnError)
	port := fs.Int("port", 9100, "port for the chains service")
	btcd := fs.String("btcd", goBin("btcd"), "path to btcd")
	ltcd := fs.String("ltcd", goBin("ltcd"), "path to ltcd")
	ethRPC := fs.String("eth-rpc", "http://127.0.0.1:8545", "the Ethereum test node (Hardhat), or \"\" to leave Ethereum out")
	fs.Parse(args)

	engines := map[string]*UTXOEngine{"btc": NewUTXOEngine(btcConfig(*btcd)), "ltc": NewUTXOEngine(ltcConfig(*ltcd))}
	for _, e := range engines {
		e.Start()
	}
	var ee *EthEngine
	if *ethRPC != "" && ethNodeUp(*ethRPC) {
		ee = NewEthEngine(ethConfig("", *ethRPC))
		ee.Start()
	} else if *ethRPC != "" {
		fmt.Println("No Ethereum node at", *ethRPC, "- running Bitcoin and Litecoin only. Start Hardhat first to include Ethereum.")
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() {
		<-sig
		fmt.Println("\nStopping the test nodes…")
		for _, e := range engines {
			e.Stop()
		}
		if ee != nil {
			ee.Stop()
		}
		os.Exit(0)
	}()

	m := http.NewServeMux()
	m.Handle("/chains", handle(func(empty) ([]ChainStatus, error) {
		var out []ChainStatus
		for id, e := range engines {
			s := e.Snapshot()
			out = append(out, ChainStatus{ID: id, RPC: e.RPCAddr(), Progress: s.Progress, Ready: s.Ready})
		}
		if ee != nil {
			s := ee.Snapshot()
			cs := ChainStatus{ID: "eth", RPC: *ethRPC, Progress: s.Progress, Ready: s.Ready}
			if s.Ready {
				ep := ee.Endpoint()
				cs.Eth = &ep
			}
			out = append(out, cs)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
		return out, nil
	}))
	m.Handle("/faucet", handle(func(r FaucetReq) (FaucetResp, error) {
		if r.Chain == "eth" && ee != nil {
			if r.Sats <= 0 || r.Sats > 25*unit {
				return FaucetResp{}, userErr("Ask for up to 25 ETH.")
			}
			return FaucetResp{Amt: r.Sats}, ee.FundAddress(r.Addr, r.Sats)
		}
		e, ok := engines[r.Chain]
		if !ok {
			return FaucetResp{}, userErr("Unknown chain.")
		}
		if r.Sats <= 0 || r.Sats > 25*unit {
			return FaucetResp{}, userErr("Ask for up to 25 coins.")
		}
		op, err := e.Faucet(r.Script, r.Sats)
		return FaucetResp{op, r.Sats}, err
	}))
	m.Handle("/mine", handle(func(r MineReq) (empty, error) {
		if r.Chain == "eth" && ee != nil {
			return empty{}, ee.MineEth(r.N, r.Secs)
		}
		e, ok := engines[r.Chain]
		if !ok {
			return empty{}, userErr("Unknown chain.")
		}
		return empty{}, e.Mine(r.N)
	}))
	ln, url := listen(*port)
	fmt.Println("Castlekeep test chains (test networks only)")
	fmt.Println("Chains service:", url)
	fmt.Println("Mining the first blocks takes about a minute. Next, in another window:  go run . exchange")
	fmt.Println("Press Ctrl+C here to stop the nodes.")
	http.Serve(ln, m)
}

// waitChains waits until the chains service reports every chain ready. It
// returns the Bitcoin and Litecoin RPC addresses, and Ethereum's endpoint
// when the chains service runs it.
func waitChains(url string) (map[string]string, *EthEndpoint) {
	fmt.Println("Waiting for the test chains at", url, "…")
	for {
		var st []ChainStatus
		if err := call(url, "/chains", empty{}, &st); err == nil && len(st) > 0 {
			rpc, ready := map[string]string{}, true
			var ep *EthEndpoint
			for _, c := range st {
				if c.ID == "eth" {
					ep = c.Eth
				} else {
					rpc[c.ID] = c.RPC
				}
				ready = ready && c.Ready
			}
			if ready {
				return rpc, ep
			}
		}
		time.Sleep(2 * time.Second)
	}
}

// ------------------------------------------------------------------ exchange

type InfoReq struct{ Chain string }
type InfoResp struct {
	Price int64
	Mode  string
}

type XVaultView struct {
	ID, Account, Chain, Status string
	UserSats, ExchSats         int64
	States                     int
	Events                     []string
}
type XChainView struct {
	ID, Ticker, Mode string
	Price, Wallet    int64
}
type XStateResp struct {
	Chains []XChainView
	Vaults []XVaultView
}

func (x *Exchange) info(r InfoReq) (InfoResp, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if r.Chain == "eth" && x.eth != nil {
		return InfoResp{x.eth.price, x.eth.mode}, nil
	}
	xc, ok := x.chains[r.Chain]
	if !ok {
		return InfoResp{}, userErr("Unknown chain.")
	}
	return InfoResp{xc.price, xc.mode}, nil
}

func (x *Exchange) state(empty) (XStateResp, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	var out XStateResp
	for id, xc := range x.chains {
		var bal int64
		for _, u := range xc.coins {
			if a, ok := xc.c.unspent(u.op); ok {
				bal += a
			}
		}
		out.Chains = append(out.Chains, XChainView{id, xc.cfg.Info.Ticker, xc.mode, xc.price, bal})
	}
	sort.Slice(out.Chains, func(i, j int) bool { return out.Chains[i].ID < out.Chains[j].ID })
	for _, v := range x.vaults {
		if len(v.states) == 0 {
			continue
		}
		st := "open"
		if v.closed != "" {
			st = v.closed
		}
		l := v.latest()
		out.Vaults = append(out.Vaults, XVaultView{v.id, v.account, v.chain, st, l.UserSats, l.ExchSats, len(v.states), v.events})
	}
	x.ethAdminView(&out)
	sort.Slice(out.Vaults, func(i, j int) bool { return out.Vaults[i].ID < out.Vaults[j].ID })
	return out, nil
}

func runExchange(args []string) {
	fs := flag.NewFlagSet("exchange", flag.ExitOnError)
	port := fs.Int("port", 9200, "port for the exchange server and its admin page")
	chains := fs.String("chains", "http://127.0.0.1:9100", "the chains service")
	stock := fs.Int64("stock", 20, "test coins the exchange starts with on each chain")
	noBrowser := fs.Bool("no-browser", false, "don't open the admin page")
	fs.Parse(args)

	rpc, ep := waitChains(*chains)
	x, err := NewExchange(splitConfigs(), rpc)
	if err != nil {
		fmt.Println("Couldn't connect to the test nodes:", err)
		os.Exit(1)
	}
	for id := range x.chains {
		var f FaucetResp
		if err := call(*chains, "/faucet", FaucetReq{Chain: id, Script: x.WalletScript(id), Sats: *stock * unit}, &f); err != nil {
			fmt.Println("Couldn't get test coins:", err)
			os.Exit(1)
		}
		x.AddCoins(id, f.Op, f.Amt)
		call(*chains, "/mine", MineReq{Chain: id, N: 1}, nil)
	}
	if ep != nil {
		addr := x.AttachEth(*ep, ethConfig("", "").DefaultPrice)
		if err := call(*chains, "/faucet", FaucetReq{Chain: "eth", Addr: addr, Sats: *stock * unit}, nil); err != nil {
			fmt.Println("Couldn't get test ETH:", err)
			os.Exit(1)
		}
	}
	go func() {
		for range time.Tick(time.Second) {
			x.Tick()
		}
	}()
	m := http.NewServeMux()
	h := x.Handler()
	m.Handle("/v1/", h)
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		b, _ := exchangeFS.ReadFile("web/exchange.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(b)
	})
	ln, url := listen(*port)
	fmt.Println("Castlekeep exchange server (test networks only)")
	fmt.Printf("It holds only the exchange's keys. Its wallet has %d test coins on each chain.\n", *stock)
	fmt.Println("Admin page:", url)
	fmt.Println("Next, in another window:  go run . wallet")
	if !*noBrowser {
		openBrowser(url)
	}
	http.Serve(ln, m)
}

// ------------------------------------------------------------------ wallet

func runWallet(args []string) {
	fs := flag.NewFlagSet("wallet", flag.ExitOnError)
	port := fs.Int("port", 9300, "port for your wallet's page")
	chains := fs.String("chains", "http://127.0.0.1:9100", "the chains service")
	exch := fs.String("exchange", "http://127.0.0.1:9200", "the exchange server")
	noBrowser := fs.Bool("no-browser", false, "don't open the browser automatically")
	fs.Parse(args)

	rpc, ep := waitChains(*chains)
	cfgs := splitConfigs()
	w, err := NewWallet(*exch, cfgs, rpc)
	if err != nil {
		fmt.Println("Couldn't connect to the test nodes:", err)
		os.Exit(1)
	}
	var engines []Engine
	for _, id := range []string{"btc", "ltc"} {
		we := &walletEngine{w: w, id: id, info: cfgs[id].Info, chains: *chains, exch: *exch}
		we.info.Model = "Separate programs: your wallet holds only your keys and checks every signature from the exchange server. " + we.info.Model
		we.info.DualFund, we.info.TestCash = true, true
		if err := we.Do(ActionReq{Action: "faucet"}); err == nil {
			we.Do(ActionReq{Action: "faucet"})
			we.Do(ActionReq{Action: "mine", N: 1})
		}
		engines = append(engines, we)
	}
	if ep != nil {
		w.AttachEth(*ep)
		ee := &walletEthEngine{w: w, info: ethConfig("", "").Info, chains: *chains, exch: *exch}
		ee.info.Model = "Separate programs: your wallet holds only your Ethereum key and checks the exchange's signature on every state. " + ee.info.Model
		if ee.Do(ActionReq{Action: "faucet"}) == nil {
			ee.Do(ActionReq{Action: "faucet"})
		}
		engines = append(engines, ee)
	}
	go func() {
		for range time.Tick(2 * time.Second) {
			w.Tick()
		}
	}()
	account := func() AccountView {
		cash := int64(0)
		var cr CashResp
		if call(*exch, "/v1/cashbalance", CashReq{w.Account}, &cr) == nil {
			cash = cr.CashCents
		}
		a := NewAccount()
		a.set(cash)
		return accountView(a, engines)
	}
	swapFn := func(from, to Engine, sats int64) error {
		if to == nil {
			return userErr("Unknown chain.")
		}
		return w.Swap(from.Info().ID, to.Info().ID, sats)
	}
	ln, url := listen(*port)
	fmt.Println("Castlekeep wallet (test networks only)")
	fmt.Println("It holds only your keys. Exchange server:", *exch)
	fmt.Println("Open:", url)
	if !*noBrowser {
		openBrowser(url)
	}
	http.Serve(ln, consoleMux(engines, account, swapFn))
}

// walletEngine lets the usual page drive the separate wallet program.
type walletEngine struct {
	w            *Wallet
	id           string
	info         ChainInfo
	chains, exch string
	eventLog
	copied int // vault events already copied into the log
}

func (e *walletEngine) Info() ChainInfo     { return e.info }
func (e *walletEngine) Start()              {}
func (e *walletEngine) Stop()               {}
func (e *walletEngine) UseAccount(*Account) {}

func (e *walletEngine) Do(r ActionReq) error {
	w := e.w
	var err error
	ok := ""
	switch r.Action {
	case "faucet":
		var f FaucetResp
		if err = call(e.chains, "/faucet", FaucetReq{Chain: e.id, Script: w.WalletScript(e.id), Sats: unit}, &f); err == nil {
			w.AddCoins(e.id, f.Op, f.Amt)
			ok = "Received 1 test " + e.info.Ticker
		}
	case "mine":
		if err = call(e.chains, "/mine", MineReq{Chain: e.id, N: r.N}, nil); err == nil {
			call(e.exch, "/v1/tick", empty{}, nil) // let the exchange's watchtower look now
			w.Tick()
			ok = fmt.Sprintf("Mined %d block(s)", r.N)
		}
	case "lock":
		if err = w.Open(e.id, r.Sats); err == nil {
			ok = "Locked " + fmtCoin(r.Sats, e.info.Ticker) + ". The exchange signed the exit request and pay-back before the deposit was sent"
		}
	case "order":
		if err = w.Order(e.id, r.Side, r.Sats); err == nil {
			ok = "Order filled: " + r.Side + " " + fmtCoin(r.Sats, e.info.Ticker) + ". Signatures and secrets checked"
		}
	case "settle":
		if err = w.Settle(e.id); err == nil {
			ok = "Settled: your share is on its way to your wallet"
		}
	case "exit":
		if err = w.Exit(e.id); err == nil {
			ok = "You broadcast the exit request"
		}
	case "claim":
		if err = w.Claim(e.id); err == nil {
			ok = "You claimed alone: deposits paid back"
		}
	case "capacity":
		if err = w.Capacity(e.id, r.Sats); err == nil {
			ok = "The exchange is adding " + fmtCoin(r.Sats, e.info.Ticker) + ". Mine a block to confirm it"
		}
	case "cheat":
		if err = w.Cheat(e.id); err == nil {
			ok = "You published an older state"
		}
	case "cash":
		if _, err = w.AddCash(); err == nil {
			ok = "Added $10,000 of test cash on the exchange"
		}
	case "mode":
		if err = w.Admin(e.id, r.Mode, 0); err == nil {
			ok = "Exchange is now " + r.Mode
		}
	case "price":
		err = w.Admin(e.id, "", r.USD)
	case "watchtower":
		w.mu.Lock()
		w.WT = r.On
		w.mu.Unlock()
		ok = map[bool]string{true: "Your watchtower is on", false: "Your watchtower is off"}[r.On]
	case "steal":
		err = userErr("In the separate programs the exchange's keys live in the exchange server, so the wallet can't make it try. The one-program console has this demo.")
	default:
		err = userErr("Unknown action.")
	}
	h := e.height()
	if err != nil {
		e.add(h, "bad", "Refused", err.Error(), "")
		return err
	}
	if ok != "" {
		e.add(h, "ok", ok, "", "")
	}
	return nil
}

func (e *walletEngine) height() int64 {
	e.w.mu.Lock()
	defer e.w.mu.Unlock()
	return e.w.chains[e.id].c.height()
}

func (e *walletEngine) Snapshot() Snapshot {
	s := Snapshot{Chain: e.info, Available: true, Ready: true, Progress: "Ready", Watchtower: e.w.WT, HasUserWT: true, Txs: []*TxInfo{}}
	var info InfoResp
	if err := call(e.exch, "/v1/info", InfoReq{e.id}, &info); err == nil {
		s.Price, s.Mode = info.Price, info.Mode
	} else {
		s.Mode = "offline"
	}
	var cr CashResp
	if call(e.exch, "/v1/cashbalance", CashReq{e.w.Account}, &cr) == nil {
		s.CashCents = cr.CashCents
	}
	e.w.mu.Lock()
	defer e.w.mu.Unlock()
	wc := e.w.chains[e.id]
	s.Height = wc.c.height()
	if mp, err := wc.c.rpc.GetRawMempool(); err == nil {
		s.Mempool = len(mp)
	}
	for _, u := range wc.coins {
		r, err := wc.c.rpc.GetTxOut(&u.op.Hash, u.op.Index, true)
		if err != nil || r == nil {
			continue
		}
		if r.Confirmations > 0 {
			s.WalletSats += u.amt
		} else {
			s.WalletPend += u.amt
		}
	}
	if v := wc.vault; v != nil {
		for ; e.copied < len(v.Events); e.copied++ {
			e.add(s.Height, "sign", v.Events[e.copied], "", "")
		}
		s.Vault = e.vaultView(wc, v, s.Height)
	}
	s.Events = e.events
	return s
}

func (e *walletEngine) vaultView(wc *wChain, v *wVault, height int64) *VaultView {
	c, g, l := wc.c, v.gen(), v.latest()
	vv := &VaultView{Locked: g.vaultAmt, UserSats: l.UserSats, ExchSats: l.ExchSats, BestState: -1,
		ClockTotal: v.def.delay, Revocation: true, PenaltyDelay: v.def.d2, PublishedState: -1,
		PublishedBy: v.publishedBy, SweepKind: v.sweepKind, CloseKind: v.closed,
		UserDeposit: g.userDep, ExchDeposit: g.exchDep, ExitSent: v.exitSent,
		FundPending: v.spliceTx != nil && c.confs(v.spliceTx) == 0,
		Scripts: []Labelled{{"Both must sign (vault and exit output)", disasm(v.def.ms.Script)},
			{fmt.Sprintf("Pay-back of deposits, %d blocks after the exit request", v.def.delay), disasm(v.def.dl.Script)}}}
	if v.published != nil {
		vv.PublishedState = v.published.Index
	}
	p := wc.cfg.Params
	if a, err := btcutil.NewAddressTaproot(v.def.V.pkScript[2:], &p); err == nil {
		vv.Address = a.EncodeAddress()
	}
	for _, st := range v.states {
		sv := StateView{Index: st.Index, Label: st.Label, UserSats: st.UserSats, ExchSats: st.ExchSats, RevHash: hex.EncodeToString(st.rHash)}
		if st.revoked && st.rSecret != nil {
			sv.Secret = hex.EncodeToString(st.rSecret)
		}
		vv.States = append(vv.States, sv)
	}
	// Status, from the chain as this wallet sees it.
	var exitG *vgen
	for _, gg := range v.gens {
		if c.isSeen(gg.exitTx) {
			exitG = gg
		}
	}
	exitConfs := int64(0)
	if exitG != nil {
		vv.ExitSent = true
		if exitConfs = c.confs(exitG.exitTx); exitConfs > 0 {
			left := v.def.delay - exitConfs
			if left < 0 {
				left = 0
			}
			vv.ClockStarted, vv.ClockDone, vv.ClockReady = true, v.def.delay-left, left == 0
		}
	}
	if v.closeTx != nil {
		vv.CloseTxid = v.closeTx.TxHash().String()
	}
	fundConfs, _ := c.seen(g.vaultOut.Hash)
	switch {
	case v.closed != "" && v.closeTx != nil && c.confs(v.closeTx) > 0:
		vv.Status = "closed"
	case v.closed != "":
		vv.Status = "closing"
	case fundConfs == 0 || vv.FundPending:
		vv.Status = "funding"
	case exitG != nil && exitConfs == 0:
		vv.Status = "exit_pending"
	case exitG != nil && vv.ClockReady:
		vv.Status = "ready"
	case exitG != nil:
		vv.Status = "exit_window"
	default:
		vv.Status = "open"
	}
	return vv
}
