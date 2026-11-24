package main

// Signing EVM transactions locally (EIP-1559, type 2), the way a public
// network needs them. On the local test nodes the programs normally let the
// node send "as" an account (impersonation); public test networks such as
// Sepolia or Amoy don't allow that, so each program must sign its own
// transactions with its own key and send them with eth_sendRawTransaction.
//
// This is switched on per engine (signLocal). The tests run the separate
// programs this way on Hardhat with impersonation switched off, so every
// transaction really is signed here.
//
// Test networks only. Not audited.

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/btcsuite/btcd/btcec/v2"
)

// ------------------------------------------------------------------ RLP

func rlpBytes(b []byte) []byte {
	if len(b) == 1 && b[0] < 0x80 {
		return b
	}
	return append(rlpLen(len(b), 0x80), b...)
}

func rlpList(items ...[]byte) []byte {
	var body []byte
	for _, it := range items {
		body = append(body, it...)
	}
	return append(rlpLen(len(body), 0xc0), body...)
}

func rlpLen(n int, base byte) []byte {
	if n < 56 {
		return []byte{base + byte(n)}
	}
	var lb []byte
	for v := n; v > 0; v >>= 8 {
		lb = append([]byte{byte(v)}, lb...)
	}
	return append([]byte{base + 55 + byte(len(lb))}, lb...)
}

func rlpInt(v *big.Int) []byte {
	if v == nil || v.Sign() == 0 {
		return rlpBytes(nil)
	}
	return rlpBytes(v.Bytes())
}

func hexBytes(s string) []byte {
	b, _ := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	return b
}

// ------------------------------------------------------------------ signing

// signer returns the key for an address this engine may sign for (nil: none).
type signerFunc func(addr string) *btcec.PrivateKey

// sendSigned signs tx (from, to, value, data, gas as hex strings) with key
// and sends it. It returns the transaction hash.
func (e *EthEngine) sendSigned(tx map[string]string, key *btcec.PrivateKey) (string, error) {
	from := tx["from"]
	nonce, err := e.callStr("eth_getTransactionCount", from, "pending")
	if err != nil {
		return "", err
	}
	raw, err := e.call("eth_getBlockByNumber", "latest", false)
	if err != nil {
		return "", err
	}
	var blk struct{ BaseFeePerGas string }
	if err := json.Unmarshal(raw, &blk); err != nil {
		return "", err
	}
	base := hexBig(blk.BaseFeePerGas)
	// The tip: what the node suggests, but at least what its plain gas price
	// leaves above the base fee (some networks, such as BNB Chain, have no
	// base fee and a minimum gas price), and never zero.
	tip := big.NewInt(0)
	if t, err := e.callStr("eth_maxPriorityFeePerGas"); err == nil {
		tip = hexBig(t)
	}
	if g, err := e.callStr("eth_gasPrice"); err == nil {
		if left := new(big.Int).Sub(hexBig(g), base); left.Cmp(tip) > 0 {
			tip = left
		}
	}
	if tip.Sign() == 0 {
		tip = big.NewInt(1_000_000_000) // 1 gwei
	}
	maxFee := new(big.Int).Add(new(big.Int).Mul(base, big.NewInt(2)), tip)
	to := hexBytes(tx["to"]) // empty: a new contract
	if len(to) != 20 && len(to) != 0 {
		return "", errors.New("sending to an address that isn't 20 bytes")
	}
	value := big.NewInt(0)
	if v := tx["value"]; v != "" {
		value = hexBig(v)
	}
	fields := [][]byte{
		rlpInt(e.chainID), rlpInt(hexBig(nonce)), rlpInt(tip), rlpInt(maxFee), rlpInt(hexBig(tx["gas"])),
		rlpBytes(to), rlpInt(value), rlpBytes(hexBytes(tx["data"])), rlpList(),
	}
	digest := keccak([]byte{0x02}, rlpList(fields...))
	sig := signDigest(key, digest) // r || s || v (27 or 28)
	yParity := new(big.Int).SetInt64(int64(sig[64]) - 27)
	signed := append(fields,
		rlpInt(yParity),
		rlpInt(new(big.Int).SetBytes(sig[0:32])),
		rlpInt(new(big.Int).SetBytes(sig[32:64])))
	rawTx := append([]byte{0x02}, rlpList(signed...)...)
	hash, err := e.callStr("eth_sendRawTransaction", "0x"+hex.EncodeToString(rawTx))
	if err != nil {
		return "", err
	}
	if want := "0x" + hex.EncodeToString(keccak(rawTx)); !strings.EqualFold(hash, want) {
		return "", fmt.Errorf("the node returned hash %s for transaction %s", hash, want)
	}
	e.cache.forget() // what this program reads next should include it
	return hash, nil
}

// signerFor is the key to sign a transaction from addr with, when this
// engine signs locally (nil: let the node send it).
func (e *EthEngine) signerFor(addr string) *btcec.PrivateKey {
	if !e.signLocal || e.signer == nil {
		return nil
	}
	return e.signer(addr)
}
