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
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/wire"
)

// ------------------------------------------------------------------ chain

type chainConn struct {
	rpc *rpcclient.Client
}

func dialChain(addr string) (*chainConn, error) {
	c, err := rpcclient.New(&rpcclient.ConnConfig{Host: addr, User: "castlekeep", Pass: "castlekeep",
		HTTPPostMode: true, DisableTLS: true}, nil)
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

type SettleReq struct{ VaultID string }
type SettleResp struct{ ExchSig []byte }

// Adding exchange coins (a splice). The old state's secrets are only swapped
// once the splice has confirmed (CapacityFinish): until then the old vault
// output could still be the one that ends up on the chain.
type CapacityReq struct {
	VaultID string
	Sats    int64
	UHash   []byte
}
type CapacityOffer struct {
	SpliceTx                             string // exchange's wallet inputs already signed
	Prevouts                             []spliceIn
	SpliceSig, ExitSig, RefundSig, RHash []byte
	ExchSigU                             []byte
	State                                StateOffer
}
type CapacityCommitReq struct {
	VaultID                                 string
	SpliceSig, ExitSig, RefundSig, UserSigE []byte
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
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(apiErr{err.Error()})
			return
		}
		json.NewEncoder(w).Encode(resp)
	}
}

var httpClient = &http.Client{Timeout: 30 * time.Second}

// call posts req to the exchange and decodes the answer into resp.
func call(base, path string, req, resp any) error {
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	r, err := httpClient.Post(base+path, "application/json", bytes.NewReader(b))
	if err != nil {
		return userErr("Couldn't reach the exchange: %v", err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		var e apiErr
		json.NewDecoder(r.Body).Decode(&e)
		if e.Error == "" {
			e.Error = r.Status
		}
		return userErr("The exchange said: %s", e.Error)
	}
	if resp == nil {
		return nil
	}
	if err := json.NewDecoder(r.Body).Decode(resp); err != nil {
		return fmt.Errorf("couldn't read the exchange's answer: %w", err)
	}
	return nil
}

var errNoVault = errors.New("no such vault")

type empty struct{}
