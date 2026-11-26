package main

// Limits on a shared server (-shared; see SERVER.md and split_shared.go).
//
// A shared server answers anyone who has its address, so one wallet (or a
// script) shouldn't be able to slow it down for everyone or fill its disk:
//
//   - requests a second from each address (all three programs), and from each
//     exchange account once its signature checks out;
//   - how many accounts the exchange and how many wallets the watchtower take,
//     in all and new ones from one address in a day;
//   - per exchange account: Bitcoin/Litecoin vaults that aren't closed, open
//     orders, and how much test cash "Add test cash" goes up to;
//   - per watchtower wallet: vaults and stored transactions;
//   - the Bitcoin gateway: calls in flight to the node at once;
//   - the size of a request, and how long a slow connection may take.
//
// Programs on the server itself that come straight to the port (not through
// the web server in front, which adds X-Forwarded-For) aren't counted against
// the per-address limits. Without -shared there are no limits.
// Test networks only. Not audited.

import (
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Limits for a shared server. Zero turns a limit off.
type Limits struct {
	PerAddress, PerAddressBurst float64 // requests a second from one address, and how many it can save up
	PerAccount, PerAccountBurst float64 // the same for one exchange account
	Accounts                    int     // exchange accounts, or watchtower wallets, in all
	NewPerAddressPerDay         int     // new accounts (or watchtower wallets) from one address in a day
	Vaults                      int     // Bitcoin/Litecoin vaults per account that aren't closed
	Orders                      int     // open orders per account
	CashCents                   int64   // "Add test cash" stops at this balance
	TowerVaults                 int     // vaults one watchtower wallet may have it watch
	TowerBytes                  int     // signed transactions it keeps for one wallet
	NodeCalls                   int     // gateway: calls to the node at once
	Body                        int64   // the largest request the exchange reads
}

// DefaultLimits suit a group of friends. A wallet sends the exchange about 7
// requests a second however many tabs of its page are open (Wallet.look), so
// the rates leave room for several wallets on one connection.
func DefaultLimits() Limits {
	return Limits{
		PerAddress: 60, PerAddressBurst: 600,
		PerAccount: 30, PerAccountBurst: 300,
		Accounts: 100, NewPerAddressPerDay: 5,
		Vaults: 10, Orders: 50, CashCents: 10_000_000, // $100,000
		TowerVaults: 200, TowerBytes: 16 << 20,
		NodeCalls: 16, Body: 256 << 10,
	}
}

// guard holds one program's limits and what it has counted so far.
type guard struct {
	Limits
	addr, acct, fresh *limiter
	slots             chan struct{} // gateway calls in flight
}

func newGuard(l Limits) *guard {
	g := &guard{Limits: l,
		addr:  newLimiter(l.PerAddress, l.PerAddressBurst),
		acct:  newLimiter(l.PerAccount, l.PerAccountBurst),
		fresh: newLimiter(float64(l.NewPerAddressPerDay)/86400, float64(l.NewPerAddressPerDay)),
	}
	if l.NodeCalls > 0 {
		g.slots = make(chan struct{}, l.NodeCalls)
	}
	return g
}

const tooMany = "Too many requests from your address. Slow down and try again in a moment."

const tooManyWallet = "Too many requests from your wallet. Slow down and try again in a moment."

// limitErr is a refusal because of a limit on how often (shown with
// 429 Too Many Requests, so a program knows to wait and try again).
type limitErr string

func (e limitErr) Error() string { return string(e) }

// perAddress refuses an address's requests beyond its allowance. g gives the
// program's guard (nil: not shared, no limits).
func perAddress(g func() *guard, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gd := g(); gd != nil {
			if a := addrOf(r); a != "" && !gd.addr.allow(a) {
				writeErr(w, http.StatusTooManyRequests, tooMany)
				return
			}
		}
		h.ServeHTTP(w, r)
	})
}

// newAddress counts a new account or watchtower wallet from a request's
// address against the day's allowance. have is how many there are already.
func (g *guard) newAddress(addr string, have int, what string) error {
	if g.Accounts > 0 && have >= g.Accounts {
		return userErr("This server already has the most %ss it takes (%d). Ask its operator.", what, g.Accounts)
	}
	if addr != "" && !g.fresh.allow(addr) {
		return limitErr(fmt.Sprintf("Too many new %ss from your address today (at most %d a day). Use one you already made, or try again tomorrow.", what, g.NewPerAddressPerDay))
	}
	return nil
}

// nodeWait is how long a gateway call waits for the node to be free.
var nodeWait = 10 * time.Second

// enterNode waits for a free slot to call the node (gateway). The returned
// func gives it back; ok is false if the node stayed busy.
func (g *guard) enterNode() (release func(), ok bool) {
	if g == nil || g.slots == nil {
		return func() {}, true
	}
	select {
	case g.slots <- struct{}{}:
		return func() { <-g.slots }, true
	case <-time.After(nodeWait):
		return nil, false
	}
}

// addrOf is who sent a request, for the limits: the address the web server
// in front (Caddy) says the request came from, or the peer's own address.
// "" is a program on this computer that came straight here (not counted).
// An IPv6 address counts per /64, the block one connection usually has.
func addrOf(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip != nil && ip.IsLoopback() {
		fwd := r.Header.Values("X-Forwarded-For")
		if len(fwd) == 0 {
			return ""
		}
		// The last address is the one the web server in front added.
		parts := strings.Split(fwd[len(fwd)-1], ",")
		host = strings.TrimSpace(parts[len(parts)-1])
		if ip = net.ParseIP(host); ip == nil {
			return "unreadable"
		}
	}
	if ip == nil {
		return host
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

// limiter allows each key rate requests a second on average, and up to
// burst at once (a token bucket).
type limiter struct {
	mu          sync.Mutex
	rate, burst float64
	buckets     map[string]*bucket
}

type bucket struct {
	tokens float64
	at     time.Time
}

// maxTracked is how many keys a limiter remembers before it forgets those
// that are back to a full allowance (and, failing that, everyone).
const maxTracked = 50000

func newLimiter(rate, burst float64) *limiter {
	return &limiter{rate: rate, burst: math.Max(burst, 1), buckets: map[string]*bucket{}}
}

// allow takes one request from key's allowance. A nil limiter or a zero rate
// allows everything.
func (l *limiter) allow(key string) bool {
	if l == nil || l.rate <= 0 {
		return true
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[key]
	if b == nil {
		if len(l.buckets) >= maxTracked {
			l.prune(now)
		}
		b = &bucket{tokens: l.burst, at: now}
		l.buckets[key] = b
	} else {
		b.tokens = math.Min(l.burst, b.tokens+now.Sub(b.at).Seconds()*l.rate)
		b.at = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (l *limiter) prune(now time.Time) {
	for k, b := range l.buckets {
		if b.tokens+now.Sub(b.at).Seconds()*l.rate >= l.burst {
			delete(l.buckets, k)
		}
	}
	if len(l.buckets) >= maxTracked {
		l.buckets = map[string]*bucket{}
	}
}

// serveHTTP serves a program's pages and API with time limits, so slow or
// stalled connections can't pile up.
func serveHTTP(ln net.Listener, h http.Handler) error {
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: time.Minute,
		WriteTimeout: 2 * time.Minute, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 64 << 10}
	return srv.Serve(ln)
}

// ------------------------------------------------------------------ the wallet

// pageReads are what a wallet's page shows from the exchange: prices, cash
// and order books. Every open tab asks them for every chain every couple of
// seconds, so the page's reads (Wallet.look) reuse an answer for
// readCacheFor. The wallet's own checks (a swap's prices, say) always ask
// afresh (Wallet.call).
var pageReads = map[string]bool{"/v1/info": true, "/v1/cashbalance": true, "/v1/book": true, "/v1/eth/info": true}

// Any other call (an order, a settlement, test cash...) forgets the reused
// answers, so the page shows the wallet's own changes at once. Except this
// one, which the wallet sends every couple of seconds while it has an open
// order: it hands out a fill to sign, and nothing changes until it's signed.
const fillsPath = "/v1/fills/next"

// readCacheFor is how long the page's reads reuse an answer.
var readCacheFor = 2 * time.Second

type readCache struct {
	mu      sync.Mutex
	answers map[string]readAnswer
	gen     int // goes up with every call that may change what the page shows
}

type readAnswer struct {
	body []byte
	at   time.Time
}

// sent is told about every call the wallet makes.
func (c *readCache) sent(path string) {
	if pageReads[path] || path == fillsPath {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	c.answers = nil
}

// look answers one of the page's reads from a recent answer, or runs send.
func (c *readCache) look(path string, req, resp any, send func() error) error {
	rb, err := json.Marshal(req)
	if !pageReads[path] || readCacheFor <= 0 || resp == nil || err != nil {
		return send()
	}
	key := path + " " + string(rb)
	c.mu.Lock()
	a, ok := c.answers[key]
	gen := c.gen
	c.mu.Unlock()
	if ok && time.Since(a.at) < readCacheFor && json.Unmarshal(a.body, resp) == nil {
		return nil
	}
	if err := send(); err != nil {
		return err
	}
	body, err := json.Marshal(resp)
	if err != nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gen == gen { // nothing was changed while it was asked
		if c.answers == nil || len(c.answers) > 1000 {
			c.answers = map[string]readAnswer{}
		}
		c.answers[key] = readAnswer{body, time.Now()}
	}
	return nil
}

// ------------------------------------------------------------------ the exchange

// SetLimits changes a shared exchange's limits (Shared sets DefaultLimits).
func (x *Exchange) SetLimits(l Limits) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.guard = newGuard(l)
}

func (x *Exchange) limits() *guard {
	x.mu.Lock()
	defer x.mu.Unlock()
	if !x.shared {
		return nil
	}
	return x.guard
}

// vaultRoom checks an account may open another Bitcoin/Litecoin vault.
// Vaults it asked keys for but never signed hold no coins: those are
// dropped first. (x.mu held.)
func (x *Exchange) vaultRoom(acct string) error {
	g := x.guard
	if !x.shared || g == nil || g.Vaults <= 0 {
		return nil
	}
	count := func() int {
		n := 0
		for _, v := range x.vaults {
			if v.account == acct && v.closed == "" {
				n++
			}
		}
		return n
	}
	if count() < g.Vaults {
		return nil
	}
	for id, v := range x.vaults {
		if v.account == acct && !v.opened && len(v.states) == 0 {
			delete(x.vaults, id)
		}
	}
	if count() < g.Vaults {
		return nil
	}
	return userErr("An account can have at most %d vaults with this exchange on a shared server, and you have that many. Settle one first.", g.Vaults)
}

// orderRoom checks an account may place another order. (x.mu held.)
func (x *Exchange) orderRoom(acct string) error {
	g := x.guard
	if !x.shared || g == nil || g.Orders <= 0 {
		return nil
	}
	n := 0
	for _, b := range x.books {
		for _, o := range b.orders {
			if o.account == acct && o.status == "open" {
				n++
			}
		}
	}
	if n >= g.Orders {
		return userErr("An account can have at most %d open orders on a shared server, and you have that many. Cancel one first.", g.Orders)
	}
	return nil
}

// cashToAdd is how much "Add test cash" adds for an account. (x.mu held.)
func (x *Exchange) cashToAdd(acct string) (int64, error) {
	g := x.guard
	if !x.shared || g == nil || g.CashCents <= 0 {
		return testCashCents, nil
	}
	room := g.CashCents - x.cash[acct]
	if room <= 0 {
		return 0, userErr("You have %s of test cash already; on a shared server \"Add test cash\" stops at %s.", fmtUSD(x.cash[acct]), fmtUSD(g.CashCents))
	}
	return min(room, testCashCents), nil
}

// ------------------------------------------------------------------ the watchtower

// SetLimits changes a shared watchtower's limits (Shared sets DefaultLimits).
func (t *Tower) SetLimits(l Limits) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.guard = newGuard(l)
}

func (t *Tower) limits() *guard {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.shared {
		return nil
	}
	return t.guard
}

// towerRoom checks an update fits in what the watchtower keeps for one
// wallet. tw is nil for a wallet signing up. (t.mu held.)
func (t *Tower) towerRoom(tw *towerWallet, u TowerUpdate) error {
	g := t.guard
	if !t.shared || g == nil {
		return nil
	}
	if tw == nil {
		tw = &towerWallet{}
	}
	vaults, bytes := towerGrowth(tw, u)
	if g.TowerVaults > 0 && len(tw.UTXO)+len(tw.EVM)+vaults > g.TowerVaults {
		return userErr("This watchtower watches at most %d vaults for one wallet. Ask its operator, or use another watchtower.", g.TowerVaults)
	}
	if g.TowerBytes > 0 && towerSize(tw)+bytes > g.TowerBytes {
		return userErr("This watchtower keeps at most %s of signed transactions for one wallet. Ask its operator, or use another watchtower.", sizeText(g.TowerBytes))
	}
	return nil
}

// sizeText writes a size for people: 16 MB, 300 KB, 200 bytes.
func sizeText(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%d MB", n>>20)
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d bytes", n)
}

// towerEVMSize is roughly what one EVM vault's record takes.
const towerEVMSize = 512

// towerSize is roughly how much a wallet's record takes: its signed
// transactions, in hex.
func towerSize(tw *towerWallet) int {
	n := len(tw.EVM) * towerEVMSize
	for _, v := range tw.UTXO {
		for _, g := range v.Gens {
			n += len(g.ExitTxid) + len(g.Latest)
		}
		for _, b := range v.Breaches {
			n += len(b.StateTxid) + len(b.Sweep)
		}
	}
	return n
}

// towerGrowth is how many vaults and how much data an update would add to
// a wallet's record (mergeTowerUTXO and mergeTowerEVM keep only what's new).
func towerGrowth(tw *towerWallet, u TowerUpdate) (vaults, bytes int) {
	type seen struct {
		gens   map[string]int // exit txid -> size of its newest state
		states map[string]bool
	}
	utxo := map[string]*seen{}
	for _, in := range u.UTXO {
		key := in.Chain + "/" + in.VaultID
		s := utxo[key]
		if s == nil {
			s = &seen{gens: map[string]int{}, states: map[string]bool{}}
			utxo[key] = s
			if have := tw.UTXO[key]; have != nil {
				for _, g := range have.Gens {
					s.gens[g.ExitTxid] = len(g.Latest)
				}
				for _, b := range have.Breaches {
					s.states[b.StateTxid] = true
				}
			} else {
				vaults++
			}
		}
		for _, g := range in.Gens {
			if old, ok := s.gens[g.ExitTxid]; !ok {
				bytes += len(g.ExitTxid) + len(g.Latest)
				s.gens[g.ExitTxid] = len(g.Latest)
			} else if len(g.Latest) > old {
				bytes += len(g.Latest) - old
				s.gens[g.ExitTxid] = len(g.Latest)
			}
		}
		for _, b := range in.Breaches {
			if !s.states[b.StateTxid] {
				bytes += len(b.StateTxid) + len(b.Sweep)
				s.states[b.StateTxid] = true
			}
		}
	}
	evm := map[string]bool{}
	for _, in := range u.EVM {
		key := in.Chain + "/" + in.VaultID
		if tw.EVM[key] == nil && !evm[key] {
			vaults++
			bytes += towerEVMSize
		}
		evm[key] = true
	}
	return vaults, bytes
}
