package main

// Bitcoin's public signet for the separate programs:
//
//	go run . chains -network signet
//
// Signet is a public test network: real blocks about every 10 minutes, made
// by the network, and coins with no value that you get from a public signet
// faucet. Nothing can be mined here and there is no built-in faucet, so the
// node runs with an address index, and the exchange and the wallets look up
// coins sent to their addresses themselves. Bitcoin only (Litecoin has no
// signet). Test networks only. Not audited.

import (
	"bytes"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

func signetConfig(btcdPath string) UTXOConfig {
	c := btcConfig(btcdPath)
	c.Network, c.Params, c.CheckForks = "signet", chaincfg.SigNetParams, false
	c.Info.Name, c.Info.Signet = "Bitcoin signet", true
	c.Info.MineButtons = []MineButton{}
	c.Info.DefaultLock, c.Info.DefaultOrder = "0.002", "0.001"
	c.Info.Model = "Bitcoin's public test network (signet): blocks come by themselves about every 10 minutes, and the coins have no value. " + c.Info.Model
	return c
}

func (e *UTXOEngine) signet() bool { return e.cfg.Network == "signet" }

// waitSynced waits until the node has caught up with the signet.
func (e *UTXOEngine) waitSynced() error {
	for i := 0; ; i++ {
		info, err := e.rpc.GetBlockChainInfo()
		if err == nil && info.Headers > 0 && info.Blocks >= info.Headers {
			if h, err := chainhash.NewHashFromStr(info.BestBlockHash); err == nil {
				if hdr, err := e.rpc.GetBlockHeader(h); err == nil && time.Since(hdr.Timestamp) < 3*time.Hour {
					e.mu.Lock()
					e.log("info", "Signet ready", fmt.Sprintf("Caught up with Bitcoin's signet at block %d.", info.Blocks), "")
					e.progress, e.ready = "Ready", true
					e.mu.Unlock()
					return nil
				}
			}
		}
		e.mu.Lock()
		if err != nil {
			e.progress = "Waiting for the signet node…"
		} else if info.Headers == 0 {
			e.progress = "Looking for signet peers…"
		} else {
			day := ""
			if h, err := chainhash.NewHashFromStr(info.BestBlockHash); err == nil {
				if hdr, err := e.rpc.GetBlockHeader(h); err == nil {
					day = ", up to " + hdr.Timestamp.Format("2 Jan 2006")
				}
			}
			e.progress = fmt.Sprintf("Downloading Bitcoin's signet: block %d%s. Only the first start takes long (it can be hours).", info.Blocks, day)
		}
		e.mu.Unlock()
		time.Sleep(5 * time.Second)
	}
}

// coinsTo finds outputs that pay script, using the signet node's address
// index (mempool included).
func (c *chainConn) coinsTo(script []byte, params *chaincfg.Params) []utxo {
	addr, err := btcutil.NewAddressTaproot(script[2:], params)
	if err != nil {
		return nil
	}
	txs, err := c.rpc.SearchRawTransactions(addr, 0, 1000, false, nil)
	if err != nil {
		return nil // also "no transactions yet"
	}
	var out []utxo
	for _, tx := range txs {
		for i, o := range tx.TxOut {
			if bytes.Equal(o.PkScript, script) {
				out = append(out, utxo{wire.OutPoint{Hash: tx.TxHash(), Index: uint32(i)}, o.Value})
			}
		}
	}
	return out
}

// addNew adds coins that aren't in the list yet.
func addNew(have []utxo, found []utxo) ([]utxo, int64) {
	var added int64
	for _, f := range found {
		known := false
		for _, h := range have {
			known = known || h.op == f.op
		}
		if !known {
			have = append(have, f)
			added += f.amt
		}
	}
	return have, added
}

// address is a wallet script as a signet (or regtest) address for display.
func address(script []byte, params *chaincfg.Params) string {
	a, err := btcutil.NewAddressTaproot(script[2:], params)
	if err != nil {
		return ""
	}
	return a.EncodeAddress()
}

// scan picks up coins sent to the exchange's wallet on signet (x.mu held).
func (x *Exchange) scan(xc *xChain) {
	if xc.cfg.Network != "signet" || time.Since(xc.lastScan) < 15*time.Second {
		return
	}
	xc.lastScan = time.Now()
	var added int64
	if xc.coins, added = addNew(xc.coins, xc.c.coinsTo(xc.pay, &xc.cfg.Params)); added > 0 {
		fmt.Printf("The exchange's wallet received %s on signet.\n", fmtCoin(added, "sBTC"))
	}
}

// scan picks up coins sent to the wallet on signet (w.mu held).
func (w *Wallet) scan(wc *wChain, now bool) int64 {
	if wc.cfg.Network != "signet" || (!now && time.Since(wc.lastScan) < 15*time.Second) {
		return 0
	}
	wc.lastScan = time.Now()
	var added int64
	wc.coins, added = addNew(wc.coins, wc.c.coinsTo(wc.pay, &wc.cfg.Params))
	return added
}

// CheckCoins looks for coins sent to the wallet's signet address now.
func (w *Wallet) CheckCoins(chain string) (int64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	wc, err := w.chain(chain)
	if err != nil {
		return 0, err
	}
	return w.scan(wc, true), nil
}

// Address is where to send coins to the wallet (shown on the page).
func (w *Wallet) Address(chain string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if wc, ok := w.chains[chain]; ok {
		return address(wc.pay, &wc.cfg.Params)
	}
	return ""
}
