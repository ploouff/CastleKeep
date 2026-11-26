package main

// Chain access and the messages the separate exchange and wallet programs
// send each other. Both programs read the chain themselves through a node's
// RPC, so neither has to trust the other about what happened on it.

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/wire"
)

// ------------------------------------------------------------------ chain

type chainConn struct {
	rpc    *rpcclient.Client
	tagged string // cached tag()
}

// dialChain connects to a node's RPC: host:port for a node on this computer,
// or a web address (http:// or https://) for a shared server's gateway
// (split_shared.go), which passes on only the calls the programs need.
func dialChain(addr string) (*chainConn, error) {
	host, plain := addr, true
	if strings.HasPrefix(addr, "http://") || strings.HasPrefix(addr, "https://") {
		u, err := url.Parse(addr)
		if err != nil {
			return nil, err
		}
		host, plain = u.Host+u.Path, u.Scheme == "http"
	}
	c, err := rpcclient.New(&rpcclient.ConnConfig{Host: host, User: "castlekeep", Pass: "castlekeep",
		HTTPPostMode: true, DisableTLS: plain}, nil)
	if err != nil {
		return nil, err
	}
	return &chainConn{rpc: c}, nil
}

func (c *chainConn) height() int64 { h, _ := c.rpc.GetBlockCount(); return h }

// seen reports whether the node knows tx (in a block or the mempool), and
// how many confirmations it has.
func (c *chainConn) seen(txid chainhash.Hash) (confs int64, ok bool) {
	r, err := c.rpc.GetRawTransactionVerbose(&txid)
	if err != nil {
		return 0, false
	}
	return int64(r.Confirmations), true
}

func (c *chainConn) confs(tx *wire.MsgTx) int64 {
	if tx == nil {
		return 0
	}
	n, _ := c.seen(tx.TxHash())
	return n
}

func (c *chainConn) isSeen(tx *wire.MsgTx) bool {
	if tx == nil {
		return false
	}
	_, ok := c.seen(tx.TxHash())
	return ok
}

func (c *chainConn) unspent(op wire.OutPoint) (int64, bool) {
	r, err := c.rpc.GetTxOut(&op.Hash, op.Index, true)
	if err != nil || r == nil {
		return 0, false
	}
	a, _ := btcutil.NewAmount(r.Value)
	return int64(a), true
}

// send broadcasts tx; a refusal comes back as an error in plain words.
func (c *chainConn) send(tx *wire.MsgTx) error {
	if _, err := c.rpc.SendRawTransaction(tx, true); err != nil {
		return userErr("Rejected by the node: %s", cleanNodeErr(err))
	}
	return nil
}

func txHex(tx *wire.MsgTx) string {
	var b bytes.Buffer
	tx.Serialize(&b)
	return hex.EncodeToString(b.Bytes())
}

func parseTxHex(s string) (*wire.MsgTx, error) {
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, err
	}
	tx := wire.NewMsgTx(2)
	return tx, tx.Deserialize(bytes.NewReader(b))
}

// ------------------------------------------------------------------ messages
//
// Byte fields travel as base64 (Go's JSON default). Every signature is
// checked by the side that receives it before it is used or acted on.

// Opening a vault. The user only broadcasts the deposit after holding the
// exchange's signatures on the exit request, the pay-back and state #0.
type OpenKeysReq struct {
	Account, Chain   string
	UserPub, UserPay []byte
}
type OpenKeysResp struct {
	VaultID          string
	ExchPub, ExchPay []byte
	Delay, D2        int64
	Anchors          bool // the exit request has a small output for each side, to speed it up (fees.go)
	MuSig2           bool // the vault output has both keys combined (MuSig2), so settling looks like a normal payment (musig.go)
}
type OpenStartReq struct {
	VaultID string
	Amt     int64
	FundOut wire.OutPoint
	UHash   []byte
}
type OpenStartResp struct {
	ExitSig, RefundSig, RHash, ExchSigU []byte
}
type OpenFinishReq struct {
	VaultID                      string
	ExitSig, RefundSig, UserSigE []byte
}

// An order. The exchange signs the user's copy of the new state first; the
// user then signs the exchange's copy and hands over its secret for the old
// state; the exchange answers with its own old secret.
type OrderReq struct {
	VaultID string
	Side    string
	Sats    int64
	UHash   []byte
}
type StateOffer struct {
	Index              int
	Label              string
	UserSats, ExchSats int64
	RHash, ExchSigU    []byte
}
type CommitReq struct {
	VaultID     string
	Index       int
	UserSigE    []byte
	USecretPrev []byte
}
type CommitResp struct {
	RSecretPrev []byte
	CashCents   int64
}

// Settling. On a MuSig2 vault (musig.go) the wallet sends a fresh public nonce
// and the exchange answers with its own and its partial signature; older
// vaults use the exchange's script-path signature (ExchSig).
type SettleReq struct {
	VaultID   string
	UserNonce []byte `json:",omitempty"`
}
type SettleResp struct {
	ExchSig                []byte
	ExchNonce, ExchPartial []byte `json:",omitempty"`
}

// Adding exchange coins (a splice). The old state's secrets are only swapped
// once the splice has confirmed (CapacityFinish): until then the old vault
// output could still be the one that ends up on the chain.
type CapacityReq struct {
	VaultID   string
	Sats      int64
	UHash     []byte
	UserNonce []byte `json:",omitempty"` // MuSig2 vaults: for the splice's key-path signature
}
type CapacityOffer struct {
	SpliceTx                             string // exchange's wallet inputs already signed
	Prevouts                             []spliceIn
	SpliceSig, ExitSig, RefundSig, RHash []byte
	ExchSigU                             []byte
	State                                StateOffer
	SpliceNonce, SplicePartial           []byte `json:",omitempty"` // MuSig2 vaults, instead of SpliceSig
}
type CapacityCommitReq struct {
	VaultID                                 string
	SpliceSig, ExitSig, RefundSig, UserSigE []byte
	SplicePartial                           []byte `json:",omitempty"` // MuSig2 vaults, instead of SpliceSig
}
type CapacityFinishReq struct {
	VaultID     string
	USecretPrev []byte
}

type CashReq struct{ Account string }
type CashResp struct{ CashCents int64 }

type AdminReq struct {
	Chain, Mode string
	USD         int64
}

// ------------------------------------------------------------------ http helpers

type apiErr struct{ Error string }

// handle wraps fn as a JSON POST endpoint. Errors made with userErr are
// shown to the other side as they are.
func handle[Req, Resp any](fn func(Req) (Resp, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req Req
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(apiErr{"Bad request: " + err.Error()})
			return
		}
		resp, err := fn(req)
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			code := http.StatusBadRequest
			if _, ok := err.(limitErr); ok { // split_limits.go
				code = http.StatusTooManyRequests
			}
			w.WriteHeader(code)
			json.NewEncoder(w).Encode(apiErr{err.Error()})
			return
		}
		json.NewEncoder(w).Encode(resp)
	}
}

var httpClient = &http.Client{Timeout: 30 * time.Second}

// call posts req to the exchange and decodes the answer into resp.
func call(base, path string, req, resp any) error {
	return callAs("the exchange", httpClient, base, path, req, resp)
}

// callAs is call for any of the programs; who names it in error messages.
func callAs(who string, client *http.Client, base, path string, req, resp any) error {
	return callHeaders(who, client, base, path, req, resp, nil)
}

// callHeaders is callAs with extra headers made from the request (the
// wallet's signature on a shared exchange, split_shared.go).
func callHeaders(who string, client *http.Client, base, path string, req, resp any, headers func(path string, body []byte) map[string]string) error {
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	hr, err := http.NewRequest(http.MethodPost, base+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	hr.Header.Set("Content-Type", "application/json")
	if headers != nil {
		for k, v := range headers(path, b) {
			hr.Header.Set(k, v)
		}
	}
	r, err := client.Do(hr)
	if err != nil {
		return userErr("Couldn't reach %s: %v", who, err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		var e apiErr
		json.NewDecoder(r.Body).Decode(&e)
		if e.Error == "" {
			e.Error = r.Status
		}
		return userErr("%s said: %s", strings.ToUpper(who[:1])+who[1:], e.Error)
	}
	if resp == nil {
		return nil
	}
	if err := json.NewDecoder(r.Body).Decode(resp); err != nil {
		return fmt.Errorf("couldn't read %s's answer: %w", who, err)
	}
	return nil
}

var errNoVault = errors.New("no such vault")

type empty struct{}

// tag tells one test chain from another (the hash of block 1), so a saved
// file isn't used on a chain that was started fresh.
func (c *chainConn) tag() string {
	if c.tagged == "" {
		if h, err := c.rpc.GetBlockHash(1); err == nil {
			c.tagged = h.String()
		}
	}
	return c.tagged
}
