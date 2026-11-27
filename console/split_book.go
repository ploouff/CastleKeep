package main

// The order book (separate programs). Several users, each with their own
// wallet and vaults, place limit orders for a coin against cash. The exchange
// matches them (best price first, then oldest first) and sits in the middle of
// every match:
//
//	Alice sells 0.1 BTC to Bob at $64,000:
//	  Alice's vault: a new state moves 0.1 BTC from her side to the exchange's side;
//	                 her cash goes up by $6,400.
//	  Bob's vault:   a new state moves 0.1 BTC from the exchange's side to his side
//	                 (the exchange's coins added as buying capacity); his cash goes down.
//
// Each side of a match ("a fill") is an ordinary state change in that user's
// own vault, signed the usual way. A wallet only signs a fill that matches an
// order it placed: same coin and side, no more than is left, and at its price
// or better. Until then the coins and cash the order needs stay reserved.
// The price of the last trade becomes the coin's price.
//
// Test networks only. Not audited.

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// ------------------------------------------------------------------ messages

type PlaceReq struct {
	Account, Chain, VaultID, Side string
	Price                         int64 // whole dollars per coin
	Sats                          int64
}
type PlaceResp struct {
	OrderID int
	Matched int64 // how much matched straight away
}
type CancelReq struct {
	Account string
	OrderID int
}
type BookReq struct{ Chain, Account string }
type BookLevel struct{ Price, Sats int64 }
type BookTrade struct {
	Price, Sats int64
	Time        string
}
type BookOrderView struct {
	ID                int
	Side, Status      string
	Price, Sats, Left int64
}
type BookResp struct {
	Bids, Asks []BookLevel
	Trades     []BookTrade
	Mine       []BookOrderView
	Price      int64
}
type FillReq struct {
	Account, Chain string
	UHash          []byte // Bitcoin/Litecoin: hash of the wallet's secret for the new state
}
type FillOffer struct {
	OrderID int
	Side    string
	Price   int64
	Sats    int64
	Leg     *SwapOfferLeg // nil when nothing is waiting
}

// ------------------------------------------------------------------ exchange side

type bookOrder struct {
	id                    int
	account, chain, vault string
	side                  string
	price, sats, left     int64
	seq                   int
	status                string // open, filled, cancelled
}

// fillLeg is one user's side of a match, waiting for that user's signature.
type fillLeg struct {
	order       *bookOrder
	side        string
	price, sats int64
	done        bool
}

type book struct {
	orders []*bookOrder
	legs   []*fillLeg
	trades []BookTrade
	seq    int
}

func (x *Exchange) bookFor(chain string) *book {
	if x.books == nil {
		x.books = map[string]*book{}
	}
	b, ok := x.books[chain]
	if !ok {
		b = &book{}
		x.books[chain] = b
	}
	return b
}

// vaultShares returns the user's and the exchange's side of a vault now.
func (x *Exchange) vaultShares(chain, vault, account string) (user, exch int64, err error) {
	if isEVM(chain) {
		xe, v, err := x.ethVault(chain, vault)
		if err != nil {
			return 0, 0, err
		}
		if v.account != account || len(v.states) == 0 {
			return 0, 0, userErr("That vault isn't yours or isn't open.")
		}
		if err := x.ethTradable(xe, v); err != nil {
			return 0, 0, err
		}
		l := v.latest()
		return l.UserSats, l.ExchSats, nil
	}
	v, xc, err := x.vault(vault)
	if err != nil {
		return 0, 0, err
	}
	if v.account != account || v.chain != chain {
		return 0, 0, userErr("That vault isn't yours.")
	}
	if v.settling {
		return 0, 0, userErr("A settlement was signed for this vault, so it can't trade any more.")
	}
	if err := x.tradable(v, xc); err != nil {
		return 0, 0, err
	}
	l := v.latest()
	return l.UserSats, l.ExchSats, nil
}

// reserved: what open orders and unsigned fills already need from a vault
// (coins on a side) and from an account (cash for buys).
func (b *book) reserved(vault, side string) (sats int64) {
	for _, o := range b.orders {
		if o.vault == vault && o.side == side && o.status == "open" {
			sats += o.left
		}
	}
	for _, l := range b.legs {
		if !l.done && l.order.vault == vault && l.side == side {
			sats += l.sats
		}
	}
	return
}

func (x *Exchange) cashReserved(account string) (cents int64) {
	for _, b := range x.books {
		for _, o := range b.orders {
			if o.account == account && o.side == "buy" && o.status == "open" {
				cents += orderCost(o.left, o.price)
			}
		}
		for _, l := range b.legs {
			if !l.done && l.order.account == account && l.side == "buy" {
				cents += orderCost(l.sats, l.price)
			}
		}
	}
	return
}

func (x *Exchange) place(r PlaceReq) (PlaceResp, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if r.Sats < 1000 || r.Price < 1 || r.Price > 10_000_000 {
		return PlaceResp{}, userErr("Enter an amount of at least 0.00001 and a price between $1 and $10,000,000.")
	}
	if r.Side != "buy" && r.Side != "sell" {
		return PlaceResp{}, userErr("Unknown order side.")
	}
	if x.modeOf(r.Chain) == "offline" {
		return PlaceResp{}, userErr("The exchange is offline and isn't accepting orders.")
	}
	if err := x.orderRoom(r.Account); err != nil {
		return PlaceResp{}, err
	}
	user, exch, err := x.vaultShares(r.Chain, r.VaultID, r.Account)
	if err != nil {
		return PlaceResp{}, err
	}
	b := x.bookFor(r.Chain)
	_, ticker, _ := x.priceOf(r.Chain)
	if r.Side == "sell" {
		if free := user - b.reserved(r.VaultID, "sell"); r.Sats > free {
			return PlaceResp{}, userErr("Your vault has %s free to sell (the rest is in other orders).", fmtCoin(free, ticker))
		}
	} else {
		if free := exch - b.reserved(r.VaultID, "buy"); r.Sats > free {
			return PlaceResp{}, userErr("The exchange's side of your vault has %s free, so you can buy at most that. Ask the exchange to lock more buying capacity.", fmtCoin(free, ticker))
		}
		if cost, free := orderCost(r.Sats, r.Price), x.cash[r.Account]-x.cashReserved(r.Account); cost > free {
			return PlaceResp{}, userErr("That order needs %s and you have %s of cash free.", fmtUSD(cost), fmtUSD(free))
		}
	}
	b.seq++
	o := &bookOrder{id: x.nextOrderID(), account: r.Account, chain: r.Chain, vault: r.VaultID, side: r.Side,
		price: r.Price, sats: r.Sats, left: r.Sats, seq: b.seq, status: "open"}
	b.orders = append(b.orders, o)
	matched := x.match(b, o)
	return PlaceResp{OrderID: o.id, Matched: matched}, nil
}

func (x *Exchange) nextOrderID() int {
	x.orderSeq++
	return x.orderSeq
}

func (x *Exchange) modeOf(chain string) string {
	if isEVM(chain) {
		if xe := x.evm[chain]; xe != nil {
			return xe.mode
		}
		return "offline"
	}
	if xc, ok := x.chains[chain]; ok {
		return xc.mode
	}
	return "offline"
}

// match fills o against the other side of the book while prices cross.
// The trade price is the resting order's price.
func (x *Exchange) match(b *book, o *bookOrder) (matched int64) {
	for o.left > 0 {
		var best *bookOrder
		for _, r := range b.orders {
			if r.status != "open" || r.side == o.side || r.account == o.account || r == o {
				continue
			}
			if (o.side == "buy" && r.price > o.price) || (o.side == "sell" && r.price < o.price) {
				continue
			}
			if best == nil || (o.side == "buy" && r.price < best.price) || (o.side == "sell" && r.price > best.price) ||
				(r.price == best.price && r.seq < best.seq) {
				best = r
			}
		}
		if best == nil {
			break
		}
		q := min(o.left, best.left)
		price := best.price
		o.left -= q
		best.left -= q
		for _, oo := range []*bookOrder{o, best} {
			if oo.left == 0 {
				oo.status = "filled"
			}
			b.legs = append(b.legs, &fillLeg{order: oo, side: oo.side, price: price, sats: q})
		}
		b.trades = append(b.trades, BookTrade{price, q, time.Now().Format("15:04:05")})
		if len(b.trades) > 50 {
			b.trades = b.trades[len(b.trades)-50:]
		}
		x.setPrice(o.chain, price)
		matched += q
	}
	return
}

func (x *Exchange) setPrice(chain string, price int64) {
	if xe := x.evm[chain]; xe != nil {
		xe.price = price
	} else if xc, ok := x.chains[chain]; ok {
		xc.price = price
	}
}

func (x *Exchange) cancel(r CancelReq) (empty, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	for _, b := range x.books {
		for _, o := range b.orders {
			if o.id == r.OrderID && o.account == r.Account {
				if o.status != "open" {
					return empty{}, userErr("That order is already %s.", o.status)
				}
				o.status = "cancelled"
				return empty{}, nil
			}
		}
	}
	return empty{}, userErr("Unknown order.")
}

func (x *Exchange) bookView(r BookReq) (BookResp, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	b := x.bookFor(r.Chain)
	price, _, err := x.priceOf(r.Chain)
	if err != nil {
		return BookResp{}, err
	}
	out := BookResp{Price: price, Trades: append([]BookTrade(nil), b.trades...)}
	levels := map[string]map[int64]int64{"buy": {}, "sell": {}}
	for _, o := range b.orders {
		if o.status == "open" {
			levels[o.side][o.price] += o.left
		}
		if o.account == r.Account && r.Account != "" {
			out.Mine = append(out.Mine, BookOrderView{o.id, o.side, o.status, o.price, o.sats, o.left})
		}
	}
	for p, s := range levels["buy"] {
		out.Bids = append(out.Bids, BookLevel{p, s})
	}
	for p, s := range levels["sell"] {
		out.Asks = append(out.Asks, BookLevel{p, s})
	}
	sort.Slice(out.Bids, func(i, j int) bool { return out.Bids[i].Price > out.Bids[j].Price })
	sort.Slice(out.Asks, func(i, j int) bool { return out.Asks[i].Price < out.Asks[j].Price })
	return out, nil
}

// nextFill hands a wallet its oldest unsigned fill on a chain, as a state
// offer for its vault. The wallet signs it through the usual commit.
func (x *Exchange) nextFill(r FillReq) (FillOffer, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	b := x.bookFor(r.Chain)
	for _, l := range b.legs {
		if l.done || l.order.account != r.Account {
			continue
		}
		_, ticker, _ := x.priceOf(r.Chain)
		cost := orderCost(l.sats, l.price)
		verb := map[string]string{"buy": "Bought", "sell": "Sold"}[l.side]
		label := fmt.Sprintf("%s %s at %s (order book)", verb, fmtCoin(l.sats, ticker), fmtUSD(l.price*100))
		leg := l
		off, set, err := x.swapLeg(SwapLeg{Chain: r.Chain, VaultID: l.order.vault, UHash: r.UHash}, r.Account, l.side, l.sats, cost, label,
			"fill-"+l.side, func() { leg.done = true })
		if err != nil {
			return FillOffer{}, err
		}
		set()
		return FillOffer{OrderID: l.order.id, Side: l.side, Price: l.price, Sats: l.sats, Leg: &off}, nil
	}
	return FillOffer{}, nil
}

// ------------------------------------------------------------------ wallet side

type wOrder struct {
	ID                int
	Chain, Side       string
	Price, Sats, Left int64
}

func (w *Wallet) vaultID(chain string) (string, error) {
	if isEVM(chain) {
		_, v, err := w.ethVault(chain)
		if err != nil {
			return "", err
		}
		return v.id.String(), nil
	}
	_, v, err := w.openVault(chain)
	if err != nil {
		return "", err
	}
	return v.id, nil
}

// PlaceOrder puts a limit order in the book. Anything that matches straight
// away is signed at once; the rest is signed as it fills (see Tick).
func (w *Wallet) PlaceOrder(chain, side string, price, sats int64) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	id, err := w.vaultID(chain)
	if err != nil {
		return 0, err
	}
	var r PlaceResp
	if err := w.call("/v1/book/place", PlaceReq{w.Account, chain, id, side, price, sats}, &r); err != nil {
		return 0, err
	}
	if w.orders == nil {
		w.orders = map[int]*wOrder{}
	}
	w.orders[r.OrderID] = &wOrder{r.OrderID, chain, side, price, sats, sats}
	return r.OrderID, w.signFills(chain)
}

func (w *Wallet) CancelOrder(id int) error {
	return w.call("/v1/book/cancel", CancelReq{w.Account, id}, nil)
}

func (w *Wallet) Book(chain string) (BookResp, error) {
	var r BookResp
	err := w.call("/v1/book", BookReq{chain, w.Account}, &r)
	return r, err
}

// signFills signs every fill waiting on a chain, after checking each one
// against the order it belongs to. (w.mu held.)
func (w *Wallet) signFills(chain string) error {
	for i := 0; i < 50; i++ {
		var us, uh []byte
		var before int64
		if isEVM(chain) {
			_, v, err := w.ethVault(chain)
			if err != nil || len(v.states) == 0 || v.exchFault != "" {
				return nil
			}
			before = v.latest().UserSats
		} else {
			_, v, err := w.openVault(chain)
			if err != nil || v.exchFault != "" {
				return nil
			}
			before = v.latest().UserSats
			us, uh = newSecret()
		}
		var f FillOffer
		if err := w.call("/v1/fills/next", FillReq{w.Account, chain, uh}, &f); err != nil {
			return err
		}
		if f.Leg == nil {
			return nil
		}
		o := w.orders[f.OrderID]
		ok := o != nil && o.Chain == chain && o.Side == f.Side && f.Sats > 0 && f.Sats <= o.Left &&
			((f.Side == "sell" && f.Price >= o.Price) || (f.Side == "buy" && f.Price <= o.Price))
		if !ok {
			return userErr("The exchange offered a fill that doesn't match any order of yours. Not signing it.")
		}
		want := before - f.Sats
		if f.Side == "buy" {
			want = before + f.Sats
		}
		if isEVM(chain) && f.Side == "buy" {
			we, v, _ := w.ethVault(chain)
			if f.Leg.Eth == nil || !strings.EqualFold(recoverAddr(we.c.stateDigest(v.id, int64(f.Leg.Eth.Nonce), want), f.Leg.EthExchSig), we.exchAddr) {
				return userErr("The exchange's signature on a fill doesn't check out. Not signing it.")
			}
		}
		if err := w.swapAgree(chain, *f.Leg, want, us, uh); err != nil {
			return err
		}
		o.Left -= f.Sats
	}
	return nil
}

// Orders lists this wallet's orders (for tests and the page).
func (w *Wallet) Orders() []wOrder {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []wOrder
	for _, o := range w.orders {
		out = append(out, *o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// SetName gives the wallet a name for the page. The account id the exchange
// sees starts with it, so the admin page can tell users apart. Call it before
// the wallet talks to the exchange.
func (w *Wallet) SetName(name string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.Name = name
	if name != "" {
		w.Account = name + "-" + w.Account[:8]
	}
}

// ------------------------------------------------------------------ the page

// bookAction runs the order book actions from the page ("place", "cancel").
// handled is false for any other action.
func (w *Wallet) bookAction(chain, ticker string, r ActionReq) (ok string, err error, handled bool) {
	switch r.Action {
	case "place":
		id, err := w.PlaceOrder(chain, r.Side, r.USD, r.Sats)
		if err != nil && id == 0 {
			return "", err, true
		}
		msg := fmt.Sprintf("Order #%d placed: %s %s at %s", id, r.Side, fmtCoin(r.Sats, ticker), fmtUSD(r.USD*100))
		if err != nil {
			// The order is in the book, but a fill couldn't be signed.
			return "", userErr("%s, but a fill wasn't signed: %v", msg, err), true
		}
		return msg + ". Anything that matched is signed; the rest waits in the book", nil, true
	case "cancel":
		if err := w.CancelOrder(r.ID); err != nil {
			return "", err, true
		}
		return fmt.Sprintf("Cancelled what was left of order #%d", r.ID), nil, true
	}
	return "", nil, false
}

// bookSnapshot is the order book for the page, with this wallet's orders.
// (Not with w.mu held: it calls the exchange.)
func (w *Wallet) bookSnapshot(chain string) *BookResp {
	var b BookResp
	if err := w.look("/v1/book", BookReq{chain, w.Account}, &b); err != nil {
		return nil
	}
	return &b
}

// ------------------------------------------------------------------ admin page

type XOrderView struct {
	ID                int
	Account, Side     string
	Status            string
	Price, Sats, Left int64
}
type XBookView struct {
	Chain    string
	Orders   []XOrderView // open orders, best price first
	Trades   []BookTrade
	Unsigned int // fills waiting for a wallet's signature
}

// bookAdminView adds every order book to the admin page's data (x.mu held).
func (x *Exchange) bookAdminView(out *XStateResp) {
	for chain, b := range x.books {
		bv := XBookView{Chain: chain, Trades: append([]BookTrade(nil), b.trades...)}
		for _, o := range b.orders {
			if o.status == "open" {
				bv.Orders = append(bv.Orders, XOrderView{o.id, o.account, o.side, o.status, o.price, o.sats, o.left})
			}
		}
		for _, l := range b.legs {
			if !l.done {
				bv.Unsigned++
			}
		}
		sort.Slice(bv.Orders, func(i, j int) bool {
			a, c := bv.Orders[i], bv.Orders[j]
			if a.Side != c.Side {
				return a.Side == "sell"
			}
			return a.Price > c.Price
		})
		out.Books = append(out.Books, bv)
	}
	sort.Slice(out.Books, func(i, j int) bool { return out.Books[i].Chain < out.Books[j].Chain })
}
