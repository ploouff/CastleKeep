package main

// Swaps in the separate programs: sell some of one coin and buy another with
// the money, in one order at the exchange's current prices. As in the
// one-program console, it is two vault states on two chains, not one
// transaction and not a hash-locked atomic swap.
//
// Order of signatures:
//  1. The exchange prepares both new states at once and signs the state for
//     the coin you buy first (on Bitcoin/Litecoin its signature on your copy,
//     on Ethereum its state signature). It takes the price of what you buy
//     from your cash balance straight away, so if your wallet never signs
//     the selling side, you have simply bought with cash. (Your balance can
//     go below zero for that moment.)
//  2. Your wallet checks both splits and the prices, and that the exchange's
//     signature on the buying side is valid, before signing anything.
//  3. It completes the buying side, then signs the selling side. The money
//     from the sale goes back to your cash balance when that is signed.
//
// So you never give up the coin you sell without already holding the coin
// you buy. Test networks only. Not audited.

import (
	"fmt"
	"strings"
)

type SwapLeg struct {
	Chain, VaultID string
	UHash          []byte // Bitcoin/Litecoin: hash of the wallet's secret for the new state
}
type SwapReq struct {
	Account  string
	From, To SwapLeg
	Sats     int64
}
type SwapOfferLeg struct {
	UTXO       *StateOffer
	Eth        *EthStateOffer
	EthExchSig []byte // Ethereum buying side: the exchange signs first
}
type SwapResp struct {
	Sell, Buy          SwapOfferLeg
	Got, Proceeds      int64 // coin bought; cents from the sale
	PriceFrom, PriceTo int64
}

// ------------------------------------------------------------------ exchange side

func (x *Exchange) priceOf(chain string) (int64, string, error) {
	if chain == "eth" {
		if x.eth == nil {
			return 0, "", userErr("This exchange has no Ethereum.")
		}
		return x.eth.price, "ETH", nil
	}
	xc, ok := x.chains[chain]
	if !ok {
		return 0, "", userErr("Unknown chain.")
	}
	return xc.price, xc.cfg.Info.Ticker, nil
}

// swapLeg prepares one side of a swap and its pending record (x.mu held).
// The record is only stored once both sides are ready.
func (x *Exchange) swapLeg(l SwapLeg, account, side string, sats, cost int64, label string) (SwapOfferLeg, func(), error) {
	if l.Chain == "eth" {
		v, err := x.ethVault(l.VaultID)
		if err != nil {
			return SwapOfferLeg{}, nil, err
		}
		if v.account != account {
			return SwapOfferLeg{}, nil, userErr("That vault belongs to another account.")
		}
		if err := x.ethTradable(v); err != nil {
			return SwapOfferLeg{}, nil, err
		}
		last := v.latest()
		user, exch := last.UserSats-sats, last.ExchSats+sats
		if side == "buy" {
			if sats > last.ExchSats {
				return SwapOfferLeg{}, nil, userErr("The exchange's side of the ETH vault holds %s, so that's the most you can get.", fmtCoin(last.ExchSats, "ETH"))
			}
			user, exch = last.UserSats+sats, last.ExchSats-sats
		} else if sats > last.UserSats {
			return SwapOfferLeg{}, nil, userErr("You only have %s in the ETH vault.", fmtCoin(last.UserSats, "ETH"))
		}
		st := &ethState{Index: len(v.states), Label: label, UserSats: user, ExchSats: exch}
		off := SwapOfferLeg{Eth: &EthStateOffer{l.VaultID, st.Index, label, user, exch}}
		if side == "buy" {
			off.EthExchSig = x.maybeTamper(signDigest(x.eth.key, x.eth.c.stateDigest(v.id, int64(st.Index), user)))
		}
		return off, func() { v.pend = &xEthPend{kind: "order", side: "swap-" + side, cost: cost, st: st} }, nil
	}
	v, xc, err := x.vault(l.VaultID)
	if err != nil {
		return SwapOfferLeg{}, nil, err
	}
	if v.account != account || v.chain != l.Chain {
		return SwapOfferLeg{}, nil, userErr("That vault belongs to another account.")
	}
	if v.settling {
		return SwapOfferLeg{}, nil, userErr("A settlement was signed for that vault, so it can't trade any more.")
	}
	if err := x.tradable(v, xc); err != nil {
		return SwapOfferLeg{}, nil, err
	}
	last := v.latest()
	t := xc.cfg.Info.Ticker
	user, exch := last.UserSats-sats, last.ExchSats+sats
	if side == "buy" {
		if sats > last.ExchSats {
			return SwapOfferLeg{}, nil, userErr("The exchange's side of the %s vault holds %s, so that's the most you can get.", t, fmtCoin(last.ExchSats, t))
		}
		user, exch = last.UserSats+sats, last.ExchSats-sats
	} else if sats > last.UserSats {
		return SwapOfferLeg{}, nil, userErr("You only have %s in the %s vault.", fmtCoin(last.UserSats, t), t)
	}
	g := v.gen()
	rs, rh := newSecret()
	st, err := v.def.buildState(g, len(v.states), len(v.gens)-1, label, user, exch, rh, l.UHash)
	if err != nil {
		return SwapOfferLeg{}, nil, userErr("%v", err)
	}
	st.rSecret = rs
	st.exchSigU = signLeaf(st.utx, 0, v.def.eFetch(g), v.def.ms, v.key)
	sig := st.exchSigU
	if side == "buy" {
		sig = x.maybeTamper(sig)
	}
	off := SwapOfferLeg{UTXO: &StateOffer{Index: st.Index, Label: label, UserSats: user, ExchSats: exch, RHash: rh, ExchSigU: sig}}
	return off, func() { v.pend = &xPending{kind: "order", side: "swap-" + side, cost: cost, st: st} }, nil
}

func (x *Exchange) swap(r SwapReq) (SwapResp, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if r.From.Chain == r.To.Chain {
		return SwapResp{}, userErr("Pick two different coins to swap.")
	}
	if r.Sats <= 0 {
		return SwapResp{}, userErr("Enter an amount above zero.")
	}
	pf, tf, err := x.priceOf(r.From.Chain)
	if err != nil {
		return SwapResp{}, err
	}
	pt, tt, err := x.priceOf(r.To.Chain)
	if err != nil {
		return SwapResp{}, err
	}
	proceeds := orderCost(r.Sats, pf)
	got := proceeds * unit / (pt * 100)
	if got <= 0 {
		return SwapResp{}, userErr("That's too little %s to buy any %s.", tf, tt)
	}
	cost := orderCost(got, pt)
	sellLabel := fmt.Sprintf("Swapped %s for %s (sold at %s)", fmtCoin(r.Sats, tf), fmtCoin(got, tt), fmtUSD(pf*100))
	buyLabel := fmt.Sprintf("Swapped %s for %s (bought at %s)", fmtCoin(r.Sats, tf), fmtCoin(got, tt), fmtUSD(pt*100))
	buy, setBuy, err := x.swapLeg(r.To, r.Account, "buy", got, 0, buyLabel)
	if err != nil {
		return SwapResp{}, userErr("Can't buy %s: %s", fmtCoin(got, tt), strings.TrimSuffix(err.Error(), "."))
	}
	sell, setSell, err := x.swapLeg(r.From, r.Account, "sell", r.Sats, proceeds, sellLabel)
	if err != nil {
		return SwapResp{}, err
	}
	setBuy()
	setSell()
	// The buying side is signed: take its price from cash now. The sale's
	// money comes back when the wallet signs the selling side.
	x.cash[r.Account] -= cost
	return SwapResp{Sell: sell, Buy: buy, Got: got, Proceeds: proceeds, PriceFrom: pf, PriceTo: pt}, nil
}

// ------------------------------------------------------------------ wallet side

// Swap sells sats of one coin's vault and buys another coin's with the money.
func (w *Wallet) Swap(from, to string, sats int64) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if from == to {
		return userErr("Pick two different coins to swap.")
	}
	var prices [2]int64
	for i, c := range []string{from, to} {
		var info InfoResp
		if err := call(w.base, "/v1/info", InfoReq{c}, &info); err != nil {
			return err
		}
		prices[i] = info.Price
	}
	// Each side: its vault id, and for Bitcoin/Litecoin a fresh secret.
	type side struct {
		leg    SwapLeg
		us     []byte
		before int64 // our share now
	}
	prep := func(chain string) (side, error) {
		if chain == "eth" {
			_, v, err := w.ethVault()
			if err != nil {
				return side{}, err
			}
			if len(v.states) == 0 || v.exchFault != "" || v.capPending {
				return side{}, userErr("Your ETH vault isn't ready to trade.")
			}
			return side{leg: SwapLeg{Chain: "eth", VaultID: v.id.String()}, before: v.latest().UserSats}, nil
		}
		_, v, err := w.openVault(chain)
		if err != nil {
			return side{}, err
		}
		if v.exchFault != "" {
			return side{}, userErr("Not trading: %s", v.exchFault)
		}
		us, uh := newSecret()
		return side{leg: SwapLeg{Chain: chain, VaultID: v.id, UHash: uh}, us: us, before: v.latest().UserSats}, nil
	}
	sf, err := prep(from)
	if err != nil {
		return err
	}
	st, err := prep(to)
	if err != nil {
		return err
	}
	var r SwapResp
	if err := call(w.base, "/v1/swap", SwapReq{w.Account, sf.leg, st.leg, sats}, &r); err != nil {
		return err
	}
	// The prices must be the ones the exchange is showing, and the amounts
	// must follow from them exactly.
	proceeds := orderCost(sats, prices[0])
	if r.PriceFrom != prices[0] || r.PriceTo != prices[1] || r.Proceeds != proceeds || r.Got != proceeds*unit/(prices[1]*100) {
		return userErr("The exchange's swap doesn't match its own prices. Refusing.")
	}
	// Check the exchange's signature on the buying side before signing anything.
	if to == "eth" {
		we, v, _ := w.ethVault()
		if r.Buy.Eth == nil || r.Buy.Eth.UserSats != st.before+r.Got ||
			!strings.EqualFold(recoverAddr(we.c.stateDigest(v.id, int64(r.Buy.Eth.Nonce), r.Buy.Eth.UserSats), r.Buy.EthExchSig), we.exchAddr) {
			return userErr("The exchange's signature on the coin you'd buy doesn't check out. Nothing was signed.")
		}
	} else if r.Buy.UTXO == nil {
		return userErr("The exchange's swap is missing the buying side.")
	}
	if (from == "eth" && r.Sell.Eth == nil) || (from != "eth" && r.Sell.UTXO == nil) {
		return userErr("The exchange's swap is missing the selling side.")
	}
	// Buying side first (utxoAgree checks the exchange's signature before
	// signing), then the selling side.
	if err := w.swapAgree(to, r.Buy, st.before+r.Got, st.us, st.leg.UHash); err != nil {
		return err
	}
	return w.swapAgree(from, r.Sell, sf.before-sats, sf.us, sf.leg.UHash)
}

func (w *Wallet) swapAgree(chain string, off SwapOfferLeg, want int64, us, uh []byte) error {
	if chain == "eth" {
		we, v, _ := w.ethVault()
		return w.ethAgree(we, v, *off.Eth, want, w.ethAmount(we, v))
	}
	_, v, _ := w.openVault(chain)
	return w.utxoAgree(v, *off.UTXO, want, us, uh)
}
