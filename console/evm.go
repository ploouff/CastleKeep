package main

// EVM chains. Ethereum, Polygon, Base, BNB Chain and Avalanche all run the
// same CastleVault contract with the same code (EthEngine in the console,
// xEth/wEth in the separate programs). Each runs as its own local test node,
// with the chain ID of that network's public TEST network (Sepolia, Amoy,
// Base Sepolia, BNB testnet, Fuji), never a main network's. Prices are made-up
// test prices in whole dollars.
//
// Test networks only. Not audited.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

type evmChain struct {
	ID, Name, Ticker string
	ChainID          int64
	BlockSecs        float64
	Price            int64  // test price, whole dollars
	Color, Badge     string // for the coin icon on the pages
}

// evmChains lists every EVM chain. Ethereum comes first; its node is the one
// started with -eth-rpc (or Anvil), as before.
var evmChains = []evmChain{
	{"eth", "Ethereum", "ETH", 11155111, 12, 3000, "#627EEA", "Ξ"},
	{"pol", "Polygon", "POL", 80002, 2, 1, "#8247E5", "P"},
	{"base", "Base", "ETH", 84532, 2, 3000, "#0052FF", "B"},
	{"bnb", "BNB Chain", "BNB", 97, 3, 600, "#F0B90B", "BNB"},
	{"avax", "Avalanche", "AVAX", 43113, 2, 30, "#E84142", "A"},
}

func evmByID(id string) (evmChain, bool) {
	for _, c := range evmChains {
		if c.ID == id {
			return c, true
		}
	}
	return evmChain{}, false
}

// isEVM reports whether a chain ID is one of the EVM chains.
func isEVM(id string) bool { _, ok := evmByID(id); return ok }

// evmConfig is the engine config for an EVM chain. rpcURL, when set, is an
// already-running node; otherwise Anvil (if installed) or Hardhat (from
// hardhatDir) is started on a free port with the chain's test chain ID.
func evmConfig(c evmChain, anvilPath, hardhatDir, rpcURL string) EthConfig {
	t := c.Ticker
	model := "CastleVault smart contract: both sides sign every state, during an exit only a newer state can replace an older one, and the exchange can lock " + t + " so you can buy from it."
	if c.ID != "eth" {
		model = "The same CastleVault contract as on Ethereum, on a local " + c.Name + " test chain (chain ID " + fmt.Sprint(c.ChainID) + ", as on its public test network)."
	}
	return EthConfig{
		Info: ChainInfo{ID: c.ID, Name: c.Name, Ticker: t, ClockUnit: "seconds", BlockMinutes: c.BlockSecs / 60,
			MineButtons: []MineButton{{Label: "Mine 1 block", N: 1}, {Label: "Skip 1 hour", Secs: 3600},
				{Label: "Skip 1 day", Secs: 86400, Title: "Moves the chain's clock forward 24 hours"}},
			ClaimLabel: "Pay out after the window", DefaultLock: "1", DefaultOrder: "0.6",
			Model: model, DualFund: true, TestCash: true, EVM: true, Color: c.Color, Badge: c.Badge},
		AnvilPath: anvilPath, HardhatDir: hardhatDir, RPCURL: rpcURL, ChainID: c.ChainID,
		InstallHint: "Install Anvil (part of Foundry), or Hardhat in " + defaultHardhatDir() + " (npm.cmd install hardhat), or start with -eth-rpc. See the README.",
		ExitDelay:   86400, DefaultPrice: c.Price,
	}
}

// defaultHardhatDir is where the README has Hardhat installed.
func defaultHardhatDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "castlekeep-eth")
}

func hardhatInstalled(dir string) bool {
	if dir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, "node_modules", "hardhat", "internal", "cli", "bootstrap.js"))
	return err == nil
}

// startHardhat starts "hardhat node" from dir on port with chainID. The config
// file is written to a temporary folder, so dir is never changed.
func startHardhat(dir string, chainID int64, port int) (*exec.Cmd, error) {
	return startHardhatAt(dir, chainID, port, 0)
}

// startHardhatAt is startHardhat with the first block at genesis (unix
// seconds; 0: now), so a kept chain can be replayed with the same hashes.
func startHardhatAt(dir string, chainID int64, port int, genesis int64) (*exec.Cmd, error) {
	if !hardhatInstalled(dir) {
		return nil, fmt.Errorf("Hardhat isn't installed in %s", dir)
	}
	cfgDir := filepath.Join(os.TempDir(), "castlekeep-hardhat", fmt.Sprint(port))
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		return nil, err
	}
	cfg := filepath.Join(cfgDir, "hardhat.config.js")
	body := "module.exports = { networks: { hardhat: { allowBlocksWithSameTimestamp: true,"
	if chainID != 0 {
		body += fmt.Sprintf(" chainId: %d,", chainID)
	}
	if genesis != 0 {
		body += fmt.Sprintf(" initialDate: %q,", time.Unix(genesis, 0).UTC().Format(time.RFC3339))
	}
	body += " } } };\n"
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		return nil, err
	}
	node, err := exec.LookPath(exe("node"))
	if err != nil {
		return nil, fmt.Errorf("Node.js isn't installed (needed for Hardhat)")
	}
	cmd := exec.Command(node, filepath.Join(dir, "node_modules", "hardhat", "internal", "cli", "bootstrap.js"),
		"node", "--port", fmt.Sprint(port), "--config", cfg)
	cmd.Dir = cfgDir
	cmd.Env = append(os.Environ(), "NODE_PATH="+filepath.Join(dir, "node_modules"), "HARDHAT_DISABLE_TELEMETRY_PROMPT=true")
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("couldn't start Hardhat: %v", err)
	}
	return cmd, nil
}

func evmName(id string) string  { c, _ := evmByID(id); return c.Name }
func evmColor(id string) string { c, _ := evmByID(id); return c.Color }
func evmBadge(id string) string { c, _ := evmByID(id); return c.Badge }

// chainOrder puts chains in the order the pages show them: Bitcoin,
// Litecoin, then the EVM chains as listed in evmChains.
func chainOrder(id string) int {
	switch id {
	case "btc":
		return 0
	case "ltc":
		return 1
	}
	for i, c := range evmChains {
		if c.ID == id {
			return 2 + i
		}
	}
	return 100
}
