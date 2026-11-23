package main

// One test account across every chain. The coins stay on their own chains
// (a wallet and a vault per chain), but the account has a single cash balance
// on the exchange, so dollars from selling one coin can buy another. Like any
// exchange balance, this cash is held by the exchange (it is not in a vault).

import (
	"fmt"
	"sort"
	"sync"
)

const testCashCents = 1_000_000 // the "Add $10,000 test cash" button

type Account struct {
	mu        sync.Mutex
	cashCents int64
}

func NewAccount() *Account { return &Account{} }

func (a *Account) Cash() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cashCents
}

func (a *Account) add(cents int64) int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cashCents += cents
	return a.cashCents
}

// spend takes cents from the balance, or refuses without changing it.
func (a *Account) spend(cents int64) (int64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if cents > a.cashCents {
		return a.cashCents, userErr("That costs %s and you have %s in cash.", fmtUSD(cents), fmtUSD(a.cashCents))
	}
	a.cashCents -= cents
	return a.cashCents, nil
}

func (a *Account) set(cents int64) {
	a.mu.Lock()
	a.cashCents = cents
	a.mu.Unlock()
}

// ------------------------------------------------------------------ overview

type Holding struct {
	Chain      string `json:"chain"`
	Name       string `json:"name"`
	Ticker     string `json:"ticker"`
	Ready      bool   `json:"ready"`
	Price      int64  `json:"price"`      // whole dollars per coin
	WalletSats int64  `json:"walletSats"` // confirmed plus pending
	VaultSats  int64  `json:"vaultSats"`  // your share of the latest state, while the vault is open
	ValueCents int64  `json:"valueCents"`
}

type AccountView struct {
	CashCents  int64     `json:"cashCents"`
	Holdings   []Holding `json:"holdings"`
	TotalCents int64     `json:"totalCents"`
}

func accountView(a *Account, engines []Engine) AccountView {
	out := AccountView{CashCents: a.Cash()}
	out.TotalCents = out.CashCents
	for _, e := range engines {
		s := e.Snapshot()
		h := Holding{Chain: s.Chain.ID, Name: s.Chain.Name, Ticker: s.Chain.Ticker, Ready: s.Ready, Price: s.Price}
		if s.Ready {
			h.WalletSats = s.WalletSats + s.WalletPend
			if v := s.Vault; v != nil && v.Status != "closed" && v.Status != "closing" {
				h.VaultSats = v.UserSats
			}
			h.ValueCents = orderCost(h.WalletSats+h.VaultSats, s.Price)
		}
		out.TotalCents += h.ValueCents
		out.Holdings = append(out.Holdings, h)
	}
	return out
}

// ------------------------------------------------------------------ swaps

// trader is what a swap needs from each chain's engine. Both engines provide
// it; the methods expect the engine's lock to be held.
type trader interface {
	Engine
	lockTrade()
	unlockTrade()
	tradeReady() error
	tradePrice() int64
	canTrade(side string, sats int64) error
	recordTrade(side string, sats int64, label string)
	log(kind, title, detail, txid string)
}

// swap sells sats of one coin and buys another with the proceeds, as one
// order at the current prices. It is two vault states on two chains, not one
// transaction: the exchange signs the buying side first, and only then does
// the user sign the selling side, so the user never gives up the coin they
// sell without already holding the coin they buy.
func swap(a *Account, from, to Engine, sats int64) error {
	if from == nil || to == nil {
		return userErr("Unknown chain.")
	}
	if from.Info().ID == to.Info().ID {
		return userErr("Pick two different coins to swap.")
	}
	sell, ok1 := from.(trader)
	buy, ok2 := to.(trader)
	if !ok1 || !ok2 {
		return userErr("That chain can't swap.")
	}
	// Lock both engines in a fixed order so two swaps can't wait on each other.
	pair := []trader{sell, buy}
	sort.Slice(pair, func(i, j int) bool { return pair[i].Info().ID < pair[j].Info().ID })
	for _, t := range pair {
		t.lockTrade()
		defer t.unlockTrade()
	}
	for _, t := range pair {
		if err := t.tradeReady(); err != nil {
			return err
		}
	}
	ft, tt := from.Info().Ticker, to.Info().Ticker
	if sats <= 0 {
		return userErr("Enter an amount above zero.")
	}
	if err := sell.canTrade("sell", sats); err != nil {
		return err
	}
	proceeds := orderCost(sats, sell.tradePrice()) // cents
	buyPrice := buy.tradePrice()
	got := proceeds * unit / (buyPrice * 100)
	if got <= 0 {
		return userErr("That's too little %s to buy any %s.", ft, tt)
	}
	if err := buy.canTrade("buy", got); err != nil {
		return userErr("Can't buy %s with that: %s", fmtCoin(got, tt), err.Error())
	}
	leftover := proceeds - orderCost(got, buyPrice) // rounding, a cent or less

	sellLabel := fmt.Sprintf("Swapped %s for %s (sold at %s)", fmtCoin(sats, ft), fmtCoin(got, tt), fmtUSD(sell.tradePrice()*100))
	buyLabel := fmt.Sprintf("Swapped %s for %s (bought at %s)", fmtCoin(sats, ft), fmtCoin(got, tt), fmtUSD(buyPrice*100))

	// Buying side first: the exchange commits to the coin you get.
	buy.recordTrade("buy", got, buyLabel)
	buy.log("ok", buyLabel, fmt.Sprintf("Part of a swap with your %s vault. The exchange signed this side first, so you held it before you signed the %s sale.", ft, ft), "")
	// Then the selling side, which you sign.
	sell.recordTrade("sell", sats, sellLabel)
	cash := a.Cash()
	if leftover > 0 {
		cash = a.add(leftover)
	}
	sell.log("ok", sellLabel, fmt.Sprintf("Part of a swap with your %s vault. No cash changed hands; cash balance: %s.", tt, fmtUSD(cash)), "")
	return nil
}
