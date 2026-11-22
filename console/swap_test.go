package main

// Swap and shared-account tests. They need all three chains (Bitcoin,
// Litecoin and the Ethereum node), which share one test account, as in the
// console. Test networks only.

import (
	"strings"
	"testing"
)

// swapTo runs a swap selling sats of h's coin for the other harness's coin.
func (h *harness) swapTo(to *harness, sats int64) error {
	return swap(testAcct, h.e, to.e, sats)
}

func (h *harness) states() int { return len(h.vault().States) }

func (h *harness) userShare(want int64) {
	h.t.Helper()
	if got := h.vault().UserSats; got != want {
		h.t.Fatalf("%s vault: your share is %s, want %s", h.chain, fmtCoin(got, h.e.Info().Ticker), fmtCoin(want, h.e.Info().Ticker))
	}
}

func TestSwap(t *testing.T) {
	btc, ltc, eth := newHarness(t, "btc"), newHarness(t, "ltc"), newHarness(t, "eth")
	// Same chain for both sides is refused.
	if err := btc.swapTo(btc, oneCoin/10); err == nil {
		t.Fatal("swapping BTC for BTC was accepted")
	}
	// No vaults yet: refused, nothing signed.
	if err := btc.swapTo(eth, oneCoin/10); err == nil || !strings.Contains(err.Error(), "Lock BTC into a vault") {
		t.Fatalf("swap without a vault: %v", err)
	}

	utxoLock(btc)
	utxoLock(ltc)
	ethFund(eth) // ETH vault with 1 ETH of the exchange's buying capacity
	ethShare := int64(oneCoin) - capacityFee(oneCoin)
	eth.userShare(ethShare)

	t.Run("BTCForETH", func(t *testing.T) {
		btc.t, eth.t = t, t
		// 0.04 BTC at $65,000 = $2,600 = 0.86666666 ETH at $3,000.
		if err := btc.swapTo(eth, 4_000_000); err != nil {
			t.Fatal(err)
		}
		btc.userShare(96_000_000)
		eth.userShare(ethShare + 86_666_666)
		btc.expectEvent("Swapped 0.04 BTC for 0.86666666 ETH")
		eth.expectEvent("Swapped 0.04 BTC for 0.86666666 ETH")
		if c := testAcct.Cash(); c > 1 {
			t.Fatalf("a swap shouldn't use cash; balance is %s", fmtUSD(c))
		}
	})

	t.Run("RefusedSwapsChangeNothing", func(t *testing.T) {
		btc.t, eth.t = t, t
		nb, ne := btc.states(), eth.states()
		// More ETH than the exchange's side holds (0.4 BTC = 8.67 ETH).
		if err := btc.swapTo(eth, 40_000_000); err == nil || !strings.Contains(err.Error(), "Can't buy") {
			t.Fatalf("too-big swap: %v", err)
		}
		// The exchange is offline on the buying side.
		eth.do(ActionReq{Action: "mode", Mode: "offline"})
		if err := btc.swapTo(eth, 1_000_000); err == nil || !strings.Contains(err.Error(), "offline") {
			t.Fatalf("swap with ETH offline: %v", err)
		}
		eth.do(ActionReq{Action: "mode", Mode: "honest"})
		// More BTC than you hold in the vault.
		if err := btc.swapTo(eth, 2*oneCoin); err == nil || !strings.Contains(err.Error(), "only have") {
			t.Fatalf("selling more than the vault holds: %v", err)
		}
		if btc.states() != nb || eth.states() != ne {
			t.Fatalf("a refused swap signed a state: BTC %d->%d, ETH %d->%d", nb, btc.states(), ne, eth.states())
		}
	})

	t.Run("LTCForBTCBuyBack", func(t *testing.T) {
		btc.t, ltc.t = t, t
		// Bitcoin's vault only holds your coins, so you can buy back the
		// 0.04 BTC sold above, but no more.
		ltc.do(ActionReq{Action: "price", USD: 6500})
		if err := ltc.swapTo(btc, 50_000_000); err == nil || !strings.Contains(err.Error(), "buy back") {
			t.Fatalf("buying more BTC than was sold: %v", err)
		}
		// 0.2 LTC at $6,500 = $1,300 = 0.02 BTC at $65,000.
		if err := ltc.swapTo(btc, 20_000_000); err != nil {
			t.Fatal(err)
		}
		ltc.userShare(80_000_000)
		btc.userShare(98_000_000)
	})

	t.Run("CashFromOneCoinBuysAnother", func(t *testing.T) {
		btc.t, eth.t = t, t
		testAcct.set(0)
		// Sell 0.005 BTC for $325 on Bitcoin, then spend it on ETH.
		btc.order("sell", 500_000)
		if c := testAcct.Cash(); c != 32_500 {
			t.Fatalf("cash after selling 0.005 BTC: %s, want $325", fmtUSD(c))
		}
		before := eth.vault().UserSats
		eth.order("buy", 10_000_000) // 0.1 ETH = $300
		eth.userShare(before + 10_000_000)
		if c := testAcct.Cash(); c != 2_500 {
			t.Fatalf("cash after buying 0.1 ETH: %s, want $25", fmtUSD(c))
		}
		eth.doUserErr(ActionReq{Action: "order", Side: "buy", Sats: 3_000_000}, "you have $25 in cash")
	})

	t.Run("SwapIntoNewLTC", func(t *testing.T) {
		btc.t, ltc.t = t, t
		// The exchange adds 1 LTC to the Litecoin vault, so you can now buy
		// LTC you never sold. 0.05 BTC at $65,000 = $3,250 = 0.5 LTC at $6,500.
		ltc.do(ActionReq{Action: "capacity", Sats: oneCoin})
		ltc.mine(1)
		ltcShare := int64(80_000_000) - capacityFee(oneCoin)
		ltc.userShare(ltcShare)
		bShare := btc.vault().UserSats
		if err := btc.swapTo(ltc, 5_000_000); err != nil {
			t.Fatal(err)
		}
		ltc.userShare(ltcShare + 50_000_000)
		btc.userShare(bShare - 5_000_000)
	})

	t.Run("AccountOverview", func(t *testing.T) {
		btc.t = t
		v := accountView(testAcct, []Engine{btc.e, ltc.e, eth.e})
		if len(v.Holdings) != 3 || v.CashCents != 2_500 {
			t.Fatalf("overview: %d holdings, cash %s", len(v.Holdings), fmtUSD(v.CashCents))
		}
		var sum int64 = v.CashCents
		for _, h := range v.Holdings {
			if h.VaultSats <= 0 {
				t.Fatalf("%s: vault share missing from the overview", h.Ticker)
			}
			sum += h.ValueCents
			t.Logf("%s: wallet %s, vault %s, worth %s", h.Ticker, fmtCoin(h.WalletSats, h.Ticker), fmtCoin(h.VaultSats, h.Ticker), fmtUSD(h.ValueCents))
		}
		if sum != v.TotalCents {
			t.Fatalf("total %s doesn't add up (%s)", fmtUSD(v.TotalCents), fmtUSD(sum))
		}
	})

	t.Run("SettleAll", func(t *testing.T) {
		for _, h := range []*harness{btc, ltc, eth} {
			h.t = t
			h.do(ActionReq{Action: "settle"})
			h.expectAccepted(h.chain + " settlement")
			h.mine(1)
			h.expectStatus("closed")
		}
	})
}

// ethFund opens an ETH vault and has the exchange lock 1 ETH of capacity.
func ethFund(h *harness) {
	h.t.Helper()
	ethLock(h)
	h.do(ActionReq{Action: "capacity", Sats: oneCoin})
	h.expectAccepted("exchange deposit")
	h.mine(1)
	if v := h.vault(); v.ExchDeposit != oneCoin {
		h.t.Fatalf("exchange deposit %d, want 1 ETH", v.ExchDeposit)
	}
}
