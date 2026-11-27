package main

// go run . start: the separate programs (chains, exchange, watchtower and a
// wallet) in one window instead of four. Each runs as its own process, as
// before, so nothing about how they talk to each other changes: this only
// starts them in order, labels their output and stops them together.
// Test networks only.

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// child is one of the programs start runs.
type child struct {
	name string
	cmd  *exec.Cmd
	done chan struct{} // closed when it has stopped
	err  error
}

type starter struct {
	exe      string
	mu       sync.Mutex // one line of output at a time
	children []*child
	stopping bool
}

func runStart(args []string) {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	network := fs.String("network", "regtest", "regtest (private test chains on this computer), signet (Bitcoin's public test network) or testnets (signet plus the public EVM test networks)")
	coins := fs.String("coins", "", "with -network testnets: which networks to use, e.g. btc,eth (see go run . chains -h)")
	exitWindow := fs.String("exit-window", "", "exit window for new EVM vaults, e.g. 1h (see go run . chains -h)")
	name := fs.String("name", "", "your wallet's name, e.g. alice (its file is wallet-<name>.json)")
	data := fs.String("data", DataDir(), "folder for the saved data")
	fresh := fs.Bool("fresh", false, "start everything new (old files are renamed, not deleted)")
	noTower := fs.Bool("no-watchtower", false, "don't run the separate watchtower")
	noBrowser := fs.Bool("no-browser", false, "don't open your wallet's page")
	base := fs.Int("ports", 9100, "the chains' port; the exchange uses this +100, your wallet +200 and the watchtower +300")
	fs.Parse(args)

	exe, err := os.Executable()
	if err != nil {
		fmt.Println("Couldn't find this program to start the others:", err)
		os.Exit(1)
	}
	chainsPort, exchPort, walletPort, towerPort := *base, *base+100, *base+200, *base+300
	ports := []int{chainsPort, exchPort, walletPort}
	if !*noTower {
		ports = append(ports, towerPort)
	}
	for _, p := range ports {
		if !portFree(p) {
			fmt.Printf("Port %d is already in use: is Castlekeep already running in another window? Stop it there first (Ctrl+C),\n", p)
			fmt.Println("or start this set on other ports, e.g.  go run . start -ports 9600")
			os.Exit(1)
		}
	}
	// A file with a password needs the program to ask for it, which it can't
	// do here (the output is labelled line by line).
	if os.Getenv("CASTLEKEEP_PASSWORD") == "" {
		for _, f := range []string{"exchange.json", walletFile(*name)} {
			for _, dir := range []string{*data, filepath.Join(*data, publicFolder)} {
				if b, err := os.ReadFile(filepath.Join(dir, f)); err == nil {
					if _, sealed := asSealed(b); sealed {
						fmt.Printf("%s has a password, and go run . start can't ask for it.\n", filepath.Join(dir, f))
						fmt.Println("Start the programs one by one instead (go run . chains, then exchange, watchtower, wallet),")
						fmt.Println("each in its own window, so they can ask.")
						os.Exit(1)
					}
				}
			}
		}
	}
	chainsURL := fmt.Sprintf("http://127.0.0.1:%d", chainsPort)
	exchURL := fmt.Sprintf("http://127.0.0.1:%d", exchPort)
	towerURL := fmt.Sprintf("http://127.0.0.1:%d", towerPort)
	walletURL := fmt.Sprintf("http://127.0.0.1:%d", walletPort)
	common := []string{"-data", *data}
	if *fresh {
		common = append(common, "-fresh")
	}

	s := &starter{exe: exe}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		s.stopAll("")
	}()

	fmt.Println("Castlekeep: starting the test chains, the exchange, the watchtower and your wallet in this window.")
	fmt.Println("Test networks only. Press Ctrl+C here to stop everything.")
	fmt.Println()

	// The chains keep their own folder, as when run alone (chains, or chains-signet).
	chainsDir := filepath.Join(*data, map[bool]string{false: "chains", true: "chains-signet"}[*network != "regtest"])
	chainArgs := []string{"chains", "-port", fmt.Sprint(chainsPort), "-network", *network, "-data", chainsDir}
	if *coins != "" {
		chainArgs = append(chainArgs, "-coins", *coins)
	}
	if *exitWindow != "" {
		chainArgs = append(chainArgs, "-exit-window", *exitWindow)
	}
	if *fresh && *network == "regtest" {
		chainArgs = append(chainArgs, "-fresh")
	}
	chains := s.start("chains", chainArgs...)
	if !s.wait(chains, func() bool { return chainsReady(chainsURL) }) {
		return
	}

	exch := s.start("exchange", append([]string{"exchange", "-port", fmt.Sprint(exchPort), "-chains", chainsURL, "-no-browser"}, common...)...)
	var tower *child
	if !*noTower {
		tower = s.start("watchtower", append([]string{"watchtower", "-port", fmt.Sprint(towerPort), "-chains", chainsURL, "-no-browser"}, common...)...)
	}
	if !s.wait(exch, func() bool { return !portFree(exchPort) }) {
		return
	}
	if tower != nil && !s.wait(tower, func() bool { return !portFree(towerPort) }) {
		return
	}

	walletArgs := append([]string{"wallet", "-port", fmt.Sprint(walletPort), "-chains", chainsURL, "-exchange", exchURL}, common...)
	if *name == "" && !*fresh {
		// No name given: the wallet used last on these chains, if there is one.
		dir := *data
		if *network != "regtest" {
			dir = filepath.Join(*data, publicFolder)
		}
		if n := lastWallet(dir); n != "" {
			*name = n
			s.say("Using your wallet %q (the one used last; start with -name to pick another).", n)
		}
	}
	if *name != "" {
		walletArgs = append(walletArgs, "-name", *name)
	}
	if tower != nil {
		walletArgs = append(walletArgs, "-watchtower", towerURL)
	}
	if *noBrowser {
		walletArgs = append(walletArgs, "-no-browser")
	}
	wallet := s.start("wallet", walletArgs...)
	if !s.wait(wallet, func() bool { return !portFree(walletPort) }) {
		return
	}

	s.mu.Lock()
	fmt.Println()
	fmt.Println("Everything is running (test networks only):")
	fmt.Println("  Your wallet       ", walletURL)
	fmt.Println("  Exchange (admin)  ", exchURL)
	if tower != nil {
		fmt.Println("  Watchtower        ", towerURL)
	}
	second := fmt.Sprintf("go run . wallet -name bob -port %d", walletPort+1)
	if *base != 9100 {
		second += " -chains " + chainsURL + " -exchange " + exchURL
	}
	fmt.Println("A second wallet, in another window: ", second)
	fmt.Println("Press Ctrl+C here to stop everything.")
	fmt.Println()
	s.mu.Unlock()

	// Run until one stops by itself (then stop the rest) or Ctrl+C.
	any := make(chan *child, len(s.children))
	for _, c := range s.children {
		go func(c *child) { <-c.done; any <- c }(c)
	}
	c := <-any
	s.stopAll(c.name)
}

// start runs one program (this same executable with a subcommand), with its
// output labelled.
func (s *starter) start(name string, args ...string) *child {
	c := &child{name: name, done: make(chan struct{})}
	c.cmd = exec.Command(s.exe, args...)
	c.cmd.SysProcAttr = childAttr()
	c.cmd.Env = append(os.Environ(), "CASTLEKEEP_START=1") // underStart: no "in another window" hints
	c.cmd.Stdin = nil
	pr, pw := io.Pipe()
	c.cmd.Stdout, c.cmd.Stderr = pw, pw
	go func() {
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			s.mu.Lock()
			fmt.Printf("%-10s| %s\n", name, sc.Text())
			s.mu.Unlock()
		}
	}()
	if err := c.cmd.Start(); err != nil {
		c.err = err
		pw.Close()
		close(c.done)
		s.say("Couldn't start the %s: %v", name, err)
		return c
	}
	s.mu.Lock()
	s.children = append(s.children, c)
	s.mu.Unlock()
	go func() {
		c.err = c.cmd.Wait()
		pw.Close()
		close(c.done)
	}()
	return c
}

// wait waits until ready says yes, or the program stops (then everything
// is stopped and it says false).
func (s *starter) wait(c *child, ready func() bool) bool {
	for {
		select {
		case <-c.done:
			s.stopAll(c.name)
			return false
		default:
		}
		if ready() {
			return true
		}
		time.Sleep(time.Second)
	}
}

func (s *starter) say(f string, a ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Printf("%-10s| %s\n", "start", fmt.Sprintf(f, a...))
}

// stopAll asks every program to stop as Ctrl+C would (so the chains stop
// their test nodes), waits for them, and exits. stopped names a program that
// stopped by itself, if one did.
func (s *starter) stopAll(stopped string) {
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		select {} // the first call exits
	}
	s.stopping = true
	children := append([]*child(nil), s.children...)
	s.mu.Unlock()
	if stopped != "" {
		s.say("The %s stopped (its last lines are above), so everything else is stopping too.", stopped)
	} else {
		s.say("Stopping everything…")
	}
	// The wallet first and the chains last, so nothing loses its node mid-step.
	for i := len(children) - 1; i >= 0; i-- {
		c := children[i]
		select {
		case <-c.done:
			continue
		default:
		}
		interrupt(c.cmd.Process)
		select {
		case <-c.done:
		case <-time.After(60 * time.Second):
			s.say("The %s didn't stop within a minute, so it was ended.", c.name)
			c.cmd.Process.Kill()
			<-c.done
		}
	}
	time.Sleep(200 * time.Millisecond) // let the last lines print
	s.say("Everything has stopped.")
	if stopped != "" {
		os.Exit(1)
	}
	os.Exit(0)
}

// underStart: this program was started by go run . start, so the hints about
// starting the next program in another window don't apply.
func underStart() bool { return os.Getenv("CASTLEKEEP_START") == "1" }

func portFree(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

// chainsReady: every chain the chains program runs is ready (or failed).
func chainsReady(url string) bool {
	var st []ChainStatus
	if err := callAs("the chains", &http.Client{Timeout: 5 * time.Second}, url, "/chains", empty{}, &st); err != nil || len(st) == 0 {
		return false
	}
	for _, c := range st {
		if !c.Ready && !strings.HasPrefix(c.Progress, "Failed") {
			return false
		}
	}
	return true
}

// lastWallet is the name of the wallet saved most recently in dir ("" for
// the unnamed wallet.json, or if there is none).
func lastWallet(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	best, when := "", time.Time{}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasPrefix(n, "wallet") || !strings.HasSuffix(n, ".json") {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.ModTime().After(when) {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(n, "wallet"), "-"), ".json")
		if n != "wallet.json" && (name == "" || !validName(name)) {
			continue
		}
		best, when = name, info.ModTime()
	}
	return best
}
