package main

// Settings for each chain. main() and the scenario tests both use these, so the
// tests run with exactly the same delays and prices as the console.

import "github.com/btcsuite/btcd/chaincfg"

func btcConfig(btcdPath string) UTXOConfig {
	return UTXOConfig{
		Info: ChainInfo{ID: "btc", Name: "Bitcoin", Ticker: "BTC", ClockUnit: "blocks", BlockMinutes: 10,
			MineButtons: []MineButton{{Label: "Mine 1 block", N: 1}, {Label: "Mine 10", N: 10},
				{Label: "Mine 144 (1 day)", N: 144, Title: "144 blocks is about one day on the real network"}},
			ClaimLabel: "Claim alone", DefaultLock: "1", DefaultOrder: "0.6", TestCash: true, DualFund: true,
			Model: "Taproot vault: a 2-of-2 script, a pre-signed exit request, a 144-block timelock, revocation secrets in both directions, and exchange coins added to the vault so you can buy from it.", Color: "#F7931A", Badge: "₿"},
		NodePath: btcdPath, NodeName: "btcd",
		InstallHint: "Install btcd:  go install github.com/btcsuite/btcd@v0.24.2",
		Params:      chaincfg.RegressionNetParams,
		ExitDelay:   144, PenaltyDelay: 6, BlockMinutes: 10, DefaultPrice: 65000, CheckForks: true,
	}
}

func ltcConfig(ltcdPath string) UTXOConfig {
	ltcParams := chaincfg.RegressionNetParams
	ltcParams.Bech32HRPSegwit = "rltc"
	return UTXOConfig{
		Info: ChainInfo{ID: "ltc", Name: "Litecoin", Ticker: "LTC", ClockUnit: "blocks", BlockMinutes: 2.5,
			MineButtons: []MineButton{{Label: "Mine 1 block", N: 1}, {Label: "Mine 40", N: 40},
				{Label: "Mine 576 (1 day)", N: 576, Title: "Litecoin blocks come every 2.5 minutes, so 576 blocks is about one day"}},
			ClaimLabel: "Claim alone", DefaultLock: "1", DefaultOrder: "0.6", TestCash: true, DualFund: true,
			Model: "Same Taproot vault as Bitcoin. Blocks are 2.5 minutes, so the 24-hour window is 576 blocks.", Color: "#345D9D", Badge: "Ł"},
		NodePath: ltcdPath, NodeName: "ltcd",
		InstallHint: "Install ltcd:  go install github.com/ltcsuite/ltcd@v0.23.6",
		Params:      ltcParams,
		ExitDelay:   576, PenaltyDelay: 24, BlockMinutes: 2.5, DefaultPrice: 100,
	}
}

func ethConfig(anvilPath, rpcURL string) EthConfig {
	return evmConfig(evmChains[0], anvilPath, "", rpcURL)
}
