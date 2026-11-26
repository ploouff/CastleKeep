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
	"path/filepath"
	"sort"
	"strings"
	"syscall"
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
	Network           string       // "" (private regtest) or "signet"
	Eth               *EthEndpoint // Ethereum only: node, contract and exit window
	Gateway           string       `json:",omitempty"` // Bitcoin/Litecoin: the node through the chains program (split_shared.go), e.g. /rpc/btc
	Shared            bool         `json:",omitempty"` // the chains program is on a shared server: use the gateway
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
	ethRPC := fs.String("eth-rpc", "", "use this Ethereum node (e.g. Hardhat at http://127.0.0.1:8545) instead of starting one")
	hardhat := fs.String("hardhat", defaultHardhatDir(), "folder with Hardhat installed, used to start the EVM test chains")
	data := fs.String("data", "", "folder that keeps the test chains between runs (default: castlekeep-data/chains, or chains-signet)")
	network := fs.String("network", "regtest", "regtest (private test chains), signet (Bitcoin's public test network, Bitcoin only) or testnets (signet plus the public EVM test networks)")
	fresh := fs.Bool("fresh", false, "start new test chains (the old folder is renamed, not deleted)")
	exitWindow := fs.String("exit-window", "24h", "EVM chains: how long the exit window lasts for new vaults, between 1m and 24h (e.g. 1h to try an exit in an afternoon)")
	coins := fs.String("coins", "btc,eth,pol,base,bnb,avax", "testnets: which networks to use (btc = signet, eth = Sepolia, pol = Amoy, base = Base Sepolia, bnb = BNB testnet, avax = Fuji)")
	rpcs := fs.String("rpc", "", "testnets: other public nodes to use, e.g. eth=https://...,pol=https://...")
	shared := fs.Bool("shared", false, "on a server other people's wallets reach (SERVER.md): wallets use the node through a gateway, and the faucet, mining and contract endpoints only answer programs on this server. Needs -network testnets or signet")
	fs.Parse(args)

	testnets := *network == "testnets"
	signet := *network == "signet" || testnets
	if !signet && *network != "regtest" {
		fmt.Println("-network must be regtest, signet or testnets.")
		os.Exit(1)
	}
	if *shared && !signet {
		// Private chains need someone to mine and a faucet, and their EVM nodes
		// would let anyone send from any address: not for sharing.
		fmt.Println("-shared is for the public test networks: add -network testnets (or -network signet).")
		os.Exit(1)
	}
	delay, err := parseExitWindow(*exitWindow)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	use := map[string]bool{}
	for _, c := range strings.Split(*coins, ",") {
		use[strings.TrimSpace(c)] = true
	}
	nodeURL := map[string]string{}
	for _, kv := range strings.Split(*rpcs, ",") {
		if id, u, ok := strings.Cut(strings.TrimSpace(kv), "="); ok {
			nodeURL[id] = u
		}
	}
	if *data == "" {
		*data = filepath.Join(DataDir(), map[bool]string{false: "chains", true: "chains-signet"}[signet])
	}
	if *fresh && signet {
		fmt.Println("Signet is a public network, so there's nothing to start fresh. (Use -fresh on the exchange and wallets instead.)")
		os.Exit(1)
	}
	if *fresh {
		if old, err := setAside(*data); err != nil {
			fmt.Println("Couldn't move the old chains folder aside:", err)
			os.Exit(1)
		} else if old != "" {
			fmt.Println("Started fresh. The old chains are in", old)
		}
	}
	bc, lc := btcConfig(*btcd), ltcConfig(*ltcd)
	// Kept between runs, on fixed ports, so the exchange and wallets can
	// reconnect after the chains restart.
	keepChain(&bc, filepath.Join(*data, "btc"), *port+1)
	keepChain(&lc, filepath.Join(*data, "ltc"), *port+2)
	engines := map[string]*UTXOEngine{"btc": NewUTXOEngine(bc), "ltc": NewUTXOEngine(lc)}
	if signet {
		// Bitcoin on signet. Litecoin has no signet. With -network testnets
		// the EVM chains use their public test networks (evm_public.go).
		sc := signetConfig(*btcd)
		sc.DataDir, sc.RPCPort = filepath.Join(*data, "btc"), *port+1
		engines = map[string]*UTXOEngine{"btc": NewUTXOEngine(sc)}
		if testnets && !use["btc"] {
			engines = map[string]*UTXOEngine{}
		}
		*ethRPC = ""
	}
	publics := map[string]*publicEVM{}
	if testnets {
		for _, c := range evmChains {
			if !use[c.ID] {
				continue
			}
			url := c.RPC
			if u := nodeURL[c.ID]; u != "" {
				url = u
			}
			publics[c.ID] = startPublicEVM(c, url, delay, filepath.Join(*data, "evm-public-"+c.ID+".json"))
		}
	}
	for _, e := range engines {
		e.Start()
	}
	// EVM chains: each its own Hardhat node behind a recorder, kept between
	// runs (evm_keep.go). With -eth-rpc, Ethereum uses that node instead,
	// keeping only the contract address as before.
	evms := map[string]*EthEngine{}
	var kept []*keptEVM
	if !signet {
		for i, c := range evmChains {
			if c.ID == "eth" && *ethRPC != "" {
				if !ethNodeUp(*ethRPC) {
					fmt.Println("No Ethereum node at", *ethRPC, "- leaving Ethereum out.")
					continue
				}
				ec := evmConfig(c, "", "", *ethRPC)
				ec.ExitDelay = delay
				ethFile := filepath.Join(*data, "eth.json")
				ec.Contract = keptContract(ethFile)
				ee := NewEthEngine(ec)
				ee.Start()
				go rememberContract(ee, ethFile)
				evms[c.ID] = ee
				continue
			}
			if !hardhatInstalled(*hardhat) {
				fmt.Println("Hardhat isn't installed in", *hardhat, "- running without the EVM chains (see the README).")
				break
			}
			k, err := startKeptEVM(c, *hardhat, filepath.Join(*data, "evm-"+c.ID+".jsonl"), *port+10+i)
			if err != nil {
				fmt.Printf("Couldn't start %s: %v\n", c.Name, err)
				continue
			}
			if k.Note != "" {
				fmt.Printf("%s: %s\n", c.Name, k.Note)
			}
			ec := evmConfig(c, "", "", k.URL)
			ec.ExitDelay = delay
			cf := filepath.Join(*data, "evm-"+c.ID+".json")
			ec.Contract = keptContract(cf)
			k.Eng = NewEthEngine(ec)
			k.Eng.Start()
			go rememberContract(k.Eng, cf)
			evms[c.ID] = k.Eng
			kept = append(kept, k)
		}
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM) // Ctrl+C, or a service manager stopping it on a server
	go func() {
		<-sig
		fmt.Println("\nStopping the test nodes…")
		for _, e := range engines {
			e.Stop()
		}
		for _, k := range kept {
			k.Stop()
		}
		os.Exit(0)
	}()

	var gd *guard // limits on a shared server (split_limits.go)
	if *shared {
		gd = newGuard(DefaultLimits())
	}
	m := http.NewServeMux()
	m.Handle("/chains", handle(func(empty) ([]ChainStatus, error) {
		var out []ChainStatus
		for id, e := range engines {
			s := e.Snapshot()
			out = append(out, ChainStatus{ID: id, RPC: e.RPCAddr(), Progress: s.Progress, Ready: s.Ready, Network: e.cfg.Network,
				Gateway: "/rpc/" + id, Shared: *shared})
		}
		for id, ee := range evms {
			s := ee.Snapshot()
			cs := ChainStatus{ID: id, RPC: ee.cfg.RPCURL, Progress: s.Progress, Ready: s.Ready}
			if s.InitErr != "" {
				cs.Progress = "Failed: " + s.InitErr
			}
			if s.Ready {
				ep := ee.Endpoint()
				cs.Eth = &ep
			}
			out = append(out, cs)
		}
		for _, p := range publics {
			out = append(out, p.status())
		}
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
		return out, nil
	}))
	for id, e := range engines {
		m.Handle("/rpc/"+id, rpcGateway(e.RPCAddr, gd))
	}
	m.Handle("/contract", localOnly(*shared, handle(func(r ContractReq) (empty, error) {
		p, ok := publics[r.Chain]
		if !ok {
			return empty{}, userErr("%s isn't a public test network here.", r.Chain)
		}
		return empty{}, p.register(r.Address)
	})))
	m.Handle("/faucet", localOnly(*shared, handle(func(r FaucetReq) (FaucetResp, error) {
		if p, ok := publics[r.Chain]; ok {
			return FaucetResp{}, userErr("%s is a public test network: get coins from a %s faucet instead.", p.c.Network, p.c.Network)
		}
		if ee, ok := evms[r.Chain]; ok {
			if r.Sats <= 0 || r.Sats > 25*unit {
				return FaucetResp{}, userErr("Ask for up to 25 %s.", ee.Info().Ticker)
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
	})))
	m.Handle("/mine", localOnly(*shared, handle(func(r MineReq) (empty, error) {
		if p, ok := publics[r.Chain]; ok {
			return empty{}, userErr("Blocks come by themselves on %s; there's nothing to mine.", p.c.Network)
		}
		if ee, ok := evms[r.Chain]; ok {
			return empty{}, ee.MineEth(r.N, r.Secs)
		}
		e, ok := engines[r.Chain]
		if !ok {
			return empty{}, userErr("Unknown chain.")
		}
		return empty{}, e.Mine(r.N)
	})))
	ln, url := listen(*port)
	fmt.Println("Castlekeep test chains (test networks only)")
	fmt.Println("Chains service:", url)
	fmt.Println("Kept in:", *data, "(start with -fresh for new chains)")
	if *shared {
		fmt.Println("Shared: other computers reach the Bitcoin node only through the gateway (a few reading calls and sending transactions); the faucet, mining and contract endpoints only answer programs on this server.")
	}
	if testnets {
		fmt.Println("Public test networks (coins with no value). Exit window for new EVM vaults:", durText(delay))
	}
	if signet && len(engines) > 0 {
		fmt.Println("Bitcoin signet: the first start downloads the signet chain (a few GB; it can take hours).")
		if !underStart() {
			fmt.Println("When it says Ready, in another window:  go run . exchange")
		}
	} else if testnets {
		if !underStart() {
			fmt.Println("Next, in another window:  go run . exchange")
		}
	} else if underStart() {
		fmt.Println("The first start mines test blocks (about a minute).")
	} else {
		fmt.Println("The first start mines test blocks (about a minute). Next, in another window:  go run . exchange")
	}
	if signet && engines["btc"] != nil {
		go func() {
			last := ""
			for range time.Tick(30 * time.Second) {
				if p := engines["btc"].Snapshot().Progress; p != last {
					fmt.Println(p)
					last = p
				}
			}
		}()
	}
	if !underStart() {
		fmt.Println("Press Ctrl+C here to stop the nodes.")
	}
	serveHTTP(ln, perAddress(func() *guard { return gd }, m))
}

// waitChains waits until the chains service reports every chain ready. It
// returns the Bitcoin and Litecoin settings and RPC addresses, and each EVM
// chain's endpoint (eth, pol, base, bnb, avax) that the chains service runs.
func waitChains(url string) (map[string]UTXOConfig, map[string]string, map[string]*EthEndpoint) {
	fmt.Println("Waiting for the test chains at", url, "…")
	started, hinted := time.Now(), false
	for {
		var st []ChainStatus
		err := call(url, "/chains", empty{}, &st)
		if err != nil && !hinted && time.Since(started) > 10*time.Second {
			hinted = true
			fmt.Println(waitHint(url))
		}
		if err == nil && len(st) > 0 {
			rpc, ready, cfgs := map[string]string{}, true, map[string]UTXOConfig{}
			eps := map[string]*EthEndpoint{}
			for _, c := range st {
				if strings.HasPrefix(c.Progress, "Failed") {
					continue // a chain that couldn't start is left out
				}
				if isEVM(c.ID) {
					eps[c.ID] = c.Eth
				} else {
					rpc[c.ID] = gatewayURL(url, c)
					cfgs[c.ID] = splitConfigs()[c.ID]
					if c.Network == "signet" {
						cfgs[c.ID] = signetConfig(goBin("btcd"))
					}
				}
				ready = ready && c.Ready
			}
			if ready {
				return cfgs, rpc, eps
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
	Address          string // signet: where to send coins to the exchange's wallet
	Name, Color      string // for the admin page's coin icons
	Badge            string
	Note             string // public test networks: setting up the contract
}
type XStateResp struct {
	Chains []XChainView
	Vaults []XVaultView
	Books  []XBookView
}

func (x *Exchange) info(r InfoReq) (InfoResp, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if xe := x.evm[r.Chain]; xe != nil {
		return InfoResp{xe.price, xe.mode}, nil
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
		out.Chains = append(out.Chains, XChainView{id, xc.cfg.Info.Ticker, xc.mode, xc.price, bal, "", xc.cfg.Info.Name, xc.cfg.Info.Color, xc.cfg.Info.Badge, ""})
		if xc.cfg.Network == "signet" {
			out.Chains[len(out.Chains)-1].Address = address(xc.pay, &xc.cfg.Params)
		}
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
	sort.SliceStable(out.Chains, func(i, j int) bool { return chainOrder(out.Chains[i].ID) < chainOrder(out.Chains[j].ID) })
	x.bookAdminView(&out)
	sort.Slice(out.Vaults, func(i, j int) bool { return out.Vaults[i].ID < out.Vaults[j].ID })
	return out, nil
}

func runExchange(args []string) {
	fs := flag.NewFlagSet("exchange", flag.ExitOnError)
	port := fs.Int("port", 9200, "port for the exchange server and its admin page")
	chains := fs.String("chains", "http://127.0.0.1:9100", "the chains service")
	stock := fs.Int64("stock", 20, "test coins the exchange starts with on each chain")
	noBrowser := fs.Bool("no-browser", false, "don't open the admin page")
	data := fs.String("data", DataDir(), "folder for the exchange's saved data (exchange.json)")
	fresh := fs.Bool("fresh", false, "start a new exchange (the old file is renamed, not deleted)")
	protect := fs.Bool("password", false, "protect the saved file with a password (asked for at start)")
	unprotect := fs.Bool("remove-password", false, "save the file in plain text again")
	shared := fs.Bool("shared", false, "on a server other people's wallets reach (SERVER.md): every request must be signed by its wallet and may only touch that wallet's vaults; modes, prices and the admin page need the admin token")
	maxAccounts := fs.Int("max-accounts", DefaultLimits().Accounts, "with -shared: the most accounts (wallets) the exchange takes")
	fs.Parse(args)

	cfgs, rpc, eps := waitChains(*chains)
	x, err := NewExchange(cfgs, rpc)
	if err != nil {
		fmt.Println("Couldn't connect to the test nodes:", err)
		os.Exit(1)
	}
	for _, c := range evmChains {
		if ep := eps[c.ID]; ep != nil {
			x.AttachEVM(c.ID, *ep, c.Price)
			if ep.Public {
				x.evm[c.ID].announce = announceContract(*chains, c.ID)
			}
		}
	}
	file := netFile(*data, "exchange.json", onPublic(cfgs, eps))
	loaded := startSealed(file, "the exchange's file", *fresh, *protect, *unprotect, func(k *sealKey) { x.seal = k }, x.Load)
	for id, xc := range x.chains {
		if xc.cfg.Network == "signet" {
			fmt.Printf("Signet: send coins to the exchange's wallet at %s (from a public signet faucet). It finds them by itself.\n", address(xc.pay, &xc.cfg.Params))
			continue
		}
		if loaded {
			continue // its coins are in the saved file
		}
		var f FaucetResp
		if err := call(*chains, "/faucet", FaucetReq{Chain: id, Script: x.WalletScript(id), Sats: *stock * unit}, &f); err != nil {
			fmt.Println("Couldn't get test coins:", err)
			os.Exit(1)
		}
		x.AddCoins(id, f.Op, f.Amt)
		call(*chains, "/mine", MineReq{Chain: id, N: 1}, nil)
	}
	for id, xe := range x.evm {
		if xe.c.public {
			c, _ := evmByID(id)
			fmt.Printf("%s: send test %s to the exchange's address %s (from a faucet for %s). It puts the contract on %s by itself once they arrive.\n",
				c.netName(), c.Ticker, xe.addr, c.Network, c.Network)
			continue
		}
		if loaded && fromWei(xe.c.balance(xe.addr)) >= unit {
			continue // funded on an earlier run
		}
		if err := call(*chains, "/faucet", FaucetReq{Chain: id, Addr: xe.addr, Sats: *stock * unit}, nil); err != nil {
			fmt.Printf("Couldn't get test %s: %v\n", xe.ticker, err)
			os.Exit(1)
		}
	}
	x.SaveTo(file)
	adminQuery := ""
	if *shared {
		adminQuery = "?token=" + x.Shared()
		l := DefaultLimits()
		l.Accounts = *maxAccounts
		x.SetLimits(l)
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
	fmt.Println("It holds only the exchange's keys. Saved in:", file, savedHow(x.seal))
	fmt.Println("Admin page:", url+"/"+adminQuery)
	if *shared {
		fmt.Println("Shared: every request must be signed by its wallet and may only touch that wallet's own vaults.")
		fmt.Println("The admin page and changing modes or prices need the token in the address above. Keep it to yourself;")
		fmt.Println("on the server's web address it's  https://<your server>/exchange/" + adminQuery)
	} else if !underStart() {
		fmt.Println("Next, in another window:  go run . wallet")
	}
	if !*noBrowser {
		openBrowser(url + "/" + adminQuery)
	}
	serveHTTP(ln, m)
}

// ------------------------------------------------------------------ watchtower

//go:embed web/tower.html
var towerFS embed.FS

func runWatchtower(args []string) {
	fs := flag.NewFlagSet("watchtower", flag.ExitOnError)
	port := fs.Int("port", 9400, "port for the watchtower and its page")
	chains := fs.String("chains", "http://127.0.0.1:9100", "the chains service")
	noBrowser := fs.Bool("no-browser", false, "don't open its page")
	data := fs.String("data", DataDir(), "folder for the watchtower's saved data (watchtower.json)")
	fresh := fs.Bool("fresh", false, "start a new watchtower (the old file is renamed, not deleted)")
	shared := fs.Bool("shared", false, "on a server other people's wallets reach (SERVER.md): its page, which lists every wallet it watches, needs the admin token")
	maxWallets := fs.Int("max-wallets", DefaultLimits().Accounts, "with -shared: the most wallets the watchtower watches for")
	fs.Parse(args)

	cfgs, rpc, eps := waitChains(*chains)
	t, err := NewTower(cfgs, rpc)
	if err != nil {
		fmt.Println("Couldn't connect to the test nodes:", err)
		os.Exit(1)
	}
	for _, c := range evmChains {
		if ep := eps[c.ID]; ep != nil {
			t.AttachEVM(c.ID, *ep)
		}
	}
	file := netFile(*data, "watchtower.json", onPublic(cfgs, eps))
	startFile(file, *fresh, t.Load)
	t.SaveTo(file)
	pageQuery := ""
	if *shared {
		pageQuery = "?token=" + t.Shared()
		l := DefaultLimits()
		l.Accounts = *maxWallets
		t.SetLimits(l)
	}
	// Its own coins, only to pay gas when it submits a state on an EVM chain.
	for _, c := range evmChains {
		te := t.evm[c.ID]
		if te == nil {
			continue
		}
		if te.public {
			fmt.Printf("%s: send a little test %s (0.01 is plenty) to the watchtower's fee address %s, from a faucet for %s. It pays the gas when it submits a state for you.\n",
				c.netName(), c.Ticker, t.FeeAddress(), c.Network)
			continue
		}
		if fromWei(te.c.balance(t.FeeAddress())) >= towerFeeMin {
			continue // funded on an earlier run
		}
		if err := call(*chains, "/faucet", FaucetReq{Chain: c.ID, Addr: t.FeeAddress(), Sats: unit}, nil); err != nil {
			fmt.Printf("Couldn't get test %s for its fees: %v\n", c.Ticker, err)
		}
	}
	go func() {
		for range time.Tick(3 * time.Second) {
			t.Tick()
		}
	}()
	m := http.NewServeMux()
	m.Handle("/v1/", t.Handler())
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		b, _ := towerFS.ReadFile("web/tower.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(b)
	})
	ln, url := listen(*port)
	fmt.Println("Castlekeep watchtower (test networks only)")
	fmt.Println("It holds no keys to your coins: only transactions your wallet signed in advance, which pay you.")
	fmt.Println("Saved in:", file)
	fmt.Println("Its page:", url+"/"+pageQuery)
	if *shared {
		fmt.Println("Shared: its page needs the token in that address (on the server's web address: https://<your server>/tower/" + pageQuery + ").")
		fmt.Println("Each wallet signs up with its own token, so wallets can't see or change each other's data.")
		fmt.Println("Wallets use it with:  go run . wallet -name <name> -watchtower https://<your server>/tower")
	} else if !underStart() {
		fmt.Println("Next, start your wallet with it (once is enough; the wallet remembers):")
		fmt.Println("  go run . wallet -name alice -watchtower", url)
	}
	if !*noBrowser {
		openBrowser(url + "/" + pageQuery)
	}
	serveHTTP(ln, m)
}

// ------------------------------------------------------------------ wallet

func runWallet(args []string) {
	fs := flag.NewFlagSet("wallet", flag.ExitOnError)
	port := fs.Int("port", 9300, "port for your wallet's page")
	chains := fs.String("chains", "http://127.0.0.1:9100", "the chains service")
	exch := fs.String("exchange", "http://127.0.0.1:9200", "the exchange server")
	noBrowser := fs.Bool("no-browser", false, "don't open the browser automatically")
	name := fs.String("name", "", "a name for this wallet, e.g. alice (run a second wallet with -name bob -port 9301)")
	data := fs.String("data", DataDir(), "folder for the wallet's saved data")
	fresh := fs.Bool("fresh", false, "start a new wallet with new keys (the old file is renamed, not deleted)")
	protect := fs.Bool("password", false, "protect the saved file with a password (asked for at start)")
	unprotect := fs.Bool("remove-password", false, "save the file in plain text again")
	towerURL := fs.String("watchtower", "", "a separate watchtower to keep up to date, e.g. http://127.0.0.1:9400 (remembered; \"off\" to stop using it)")
	serverFlag := fs.String("server", "", "a shared server (SERVER.md), e.g. https://castlekeep.example.com: its chains, exchange and watchtower in one address (remembered; \"local\" to use the programs on this computer again)")
	fs.Parse(args)
	if !validName(*name) {
		fmt.Println("Use only letters, digits, - and _ in -name.")
		os.Exit(1)
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	// A shared server: typed now, or remembered from last time (unless the
	// chains or exchange were given instead).
	server, setServer := cleanServer(*serverFlag), given["server"]
	if server == "https://local" {
		server = ""
	} else if !setServer && !given["chains"] && !given["exchange"] {
		if s := savedServer(*data, *name); s != "" {
			server, setServer = s, true
			fmt.Println("Using the server from last time:", s, "(start with -server local to use the programs on this computer)")
		}
	}
	if server != "" {
		c, e, _ := serverURLs(server)
		if !given["chains"] {
			*chains = c
		}
		if !given["exchange"] {
			*exch = e
		}
	}

	cfgs, rpc, eps := waitChains(*chains)
	w, err := NewWallet(*exch, cfgs, rpc)
	if err != nil {
		fmt.Println("Couldn't connect to the test nodes:", err)
		os.Exit(1)
	}
	for _, c := range evmChains {
		if ep := eps[c.ID]; ep != nil {
			w.AttachEVM(c.ID, *ep)
			if ep.Public {
				w.evm[c.ID].lookup = lookupContract(*chains, c.ID)
			}
		}
	}
	file := netFile(*data, walletFile(*name), onPublic(cfgs, eps))
	loaded := startSealed(file, "your wallet", *fresh, *protect, *unprotect, func(k *sealKey) { w.seal = k }, w.Load)
	if !loaded {
		w.SetName(*name)
	}
	w.SaveTo(file)
	if setServer {
		w.SetServer(server)
	}
	if server != "" {
		fmt.Println("Server:", server, "(its chains, exchange and watchtower)")
		// Its watchtower, unless this wallet already uses one.
		if _, _, t := serverURLs(server); *towerURL == "" && w.TowerURL() == "" && towerAnswers(t) {
			*towerURL = t
		}
	}
	switch *towerURL {
	case "":
		// keep whatever the saved file says
	case "off":
		if err := w.StopTower(); err != nil {
			fmt.Println("Couldn't tell the watchtower to forget this wallet, so it's still in use:", err)
			fmt.Println("Start the watchtower, then try again. (Otherwise it would keep a state your next trades replace.)")
			os.Exit(1)
		}
		fmt.Println("Stopped using the separate watchtower; it no longer holds anything for this wallet.")
	default:
		w.UseTower(*towerURL)
	}
	if u := w.TowerURL(); u != "" {
		fmt.Println("Separate watchtower:", u, "(it gets each new state before the old one is given up; -watchtower off to stop)")
	}
	var engines []Engine
	for _, id := range []string{"btc", "ltc"} {
		if _, ok := cfgs[id]; !ok {
			continue
		}
		we := &walletEngine{w: w, id: id, info: cfgs[id].Info, chains: *chains, exch: *exch}
		we.info.Model = "Separate programs: your wallet holds only your keys and checks every signature from the exchange server. " + we.info.Model
		we.info.DualFund, we.info.TestCash = true, true
		if cfgs[id].Network == "signet" {
			fmt.Println("Signet: send coins to your wallet at", w.Address(id), "(from a public signet faucet).")
		} else if loaded {
			// its coins are in the saved file
		} else if err := we.Do(ActionReq{Action: "faucet"}); err == nil {
			we.Do(ActionReq{Action: "faucet"})
			we.Do(ActionReq{Action: "mine", N: 1})
		}
		engines = append(engines, we)
	}
	for _, c := range evmChains {
		if eps[c.ID] == nil {
			continue
		}
		ee := &walletEthEngine{w: w, id: c.ID, info: evmConfig(c, "", "", "").Info, chains: *chains, exch: *exch}
		if eps[c.ID].Public {
			ee.info = publicInfo(c, eps[c.ID].ExitDelay)
		}
		ee.info.Model = "Separate programs: your wallet holds only your " + c.Name + " key and checks the exchange's signature on every state. " + ee.info.Model
		if ee.info.Public {
			fmt.Printf("%s: send test %s to your wallet at %s (from a faucet for %s).\n", ee.info.Name, c.Ticker, w.EthAddress(c.ID), c.Network)
		} else if loaded && w.EthBalance(c.ID) >= unit {
			// funded on an earlier run
		} else if ee.Do(ActionReq{Action: "faucet"}) == nil {
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
		if w.look("/v1/cashbalance", CashReq{w.Account}, &cr) == nil {
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
	if *name != "" {
		fmt.Println("Wallet name:", *name)
	}
	fmt.Println("It holds only your keys. Exchange server:", *exch)
	fmt.Println("Saved in:", file, savedHow(w.seal))
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
	defer e.w.Save()
	w := e.w
	var err error
	ok := ""
	switch r.Action {
	case "speedup":
		ok, err = w.SpeedUp(e.id)
	case "faucet":
		if e.info.Signet {
			var got int64
			if got, err = w.CheckCoins(e.id); err == nil && got == 0 {
				err = userErr("Nothing new has arrived at %s yet. Coins from a faucet show up within a few seconds; they confirm in the next block (about 10 minutes).", w.Address(e.id))
			} else if err == nil {
				ok = "Found " + fmtCoin(got, e.info.Ticker) + " sent to your address"
			}
			break
		}
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
		var handled bool
		if ok, err, handled = w.bookAction(e.id, e.info.Ticker, r); !handled {
			err = userErr("Unknown action.")
		}
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
	if err := e.w.look("/v1/info", InfoReq{e.id}, &info); err == nil {
		s.Price, s.Mode = info.Price, info.Mode
	} else {
		s.ExchDown = exchDown(err, e.exch)
	}
	var cr CashResp
	if e.w.look("/v1/cashbalance", CashReq{e.w.Account}, &cr) == nil {
		s.CashCents = cr.CashCents
	}
	s.Who, s.Book = e.w.Name, e.w.bookSnapshot(e.id)
	if e.info.Signet {
		s.ReceiveAddr = e.w.Address(e.id)
	}
	e.w.mu.Lock()
	defer e.w.mu.Unlock()
	s.Tower = e.w.towerNote(e.id)
	wc := e.w.chains[e.id]
	s.Height = wc.c.height()
	s.Bumpable, s.FeeNote = len(wc.bumpable()), wc.c.feeNote()
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
		Scripts: append(keyPathNote(v.def.V), []Labelled{{"Both must sign (vault and exit output)", disasm(v.def.ms.Script)},
			{fmt.Sprintf("Pay-back of deposits, %d blocks after the exit request", v.def.delay), disasm(v.def.dl.Script)}}...)}
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

// rememberContract saves the contract address once the engine is ready, so
// the next run uses the same contract.
func rememberContract(ee *EthEngine, file string) {
	for !ee.Snapshot().Ready {
		if ee.Snapshot().InitErr != "" {
			return
		}
		time.Sleep(time.Second)
	}
	if err := saveContract(file, ee.Endpoint().Contract); err != nil {
		fmt.Println("Couldn't save the contract address:", err)
	}
}
