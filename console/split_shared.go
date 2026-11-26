package main

// Shared mode: the exchange, the chains program and the watchtower on a server
// that other people's wallets reach over the internet (see SERVER.md).
//
//   - Every request a wallet sends the exchange carries its account, its
//     account key, the time and a signature (signedHeaders). On a shared
//     exchange (-shared) the exchange checks the signature, keeps the first key
//     each account used, refuses a request it has already seen, and checks that
//     the account or vault in the request belongs to the signer (authorize).
//   - Changing the exchange's modes and prices, the admin page's data and
//     /v1/tick need the operator's admin token (adminOK).
//   - On the EVM chains a wallet proves a vault is its own before the exchange
//     registers it (claimDigest), so nobody else can take the vault's number.
//   - The chains program passes on to the Bitcoin node only the calls the
//     programs need (rpcGateway), and its faucet, mine and contract endpoints
//     only answer programs on the same computer (localOnly).
//   - Limits on how often and how much (split_limits.go): requests a second
//     per address and per account, accounts, vaults, orders, test cash, what
//     the watchtower keeps, calls to the node at once.
//
// Without -shared everything works as before (one computer, no checks).
// Test networks only. Not audited.

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"math/big"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
)

const (
	hdrAccount = "X-Castlekeep-Account"
	hdrKey     = "X-Castlekeep-Key"
	hdrTime    = "X-Castlekeep-Time"
	hdrSig     = "X-Castlekeep-Sig"
	hdrNonce   = "X-Castlekeep-Nonce"
	hdrAdmin   = "X-Castlekeep-Admin"

	requestWindow = 300 // seconds either side of the exchange's clock a signed request is good for
)

// requestDigest is what a wallet signs: the path, the time, a random value
// (so two identical requests in the same second still differ) and the body.
func requestDigest(path, ts, nonce string, body []byte) []byte {
	bh := sha256.Sum256(body)
	h := sha256.Sum256([]byte("castlekeep-request-v1\n" + path + "\n" + ts + "\n" + nonce + "\n" + hex.EncodeToString(bh[:])))
	return h[:]
}

// signedHeaders signs one request with an account key (the wallet's side).
func signedHeaders(account string, key *btcec.PrivateKey) func(path string, body []byte) map[string]string {
	return func(path string, body []byte) map[string]string {
		ts, nonce := strconv.FormatInt(time.Now().Unix(), 10), newToken()
		sig, err := schnorr.Sign(key, requestDigest(path, ts, nonce, body))
		if err != nil {
			return nil
		}
		return map[string]string{hdrAccount: account, hdrKey: hex.EncodeToString(xonly(key)), hdrTime: ts,
			hdrNonce: nonce, hdrSig: hex.EncodeToString(sig.Serialize())}
	}
}

// call sends a request to the exchange, signed with the wallet's account key.
func (w *Wallet) call(path string, req, resp any) error {
	err := callHeaders("the exchange", httpClient, w.base, path, req, resp, signedHeaders(w.Account, w.acctKey))
	w.reads.sent(path)
	return err
}

// look is call for what the page shows (prices, cash, order books): an
// answer is reused for a moment (readCache, split_limits.go).
func (w *Wallet) look(path string, req, resp any) error {
	return w.reads.look(path, req, resp, func() error { return w.call(path, req, resp) })
}

// validAccount: letters, digits, - and _ only, as wallets make them (name-1a2b3c4d).
func validAccount(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func newToken() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(apiErr{msg})
}

// ------------------------------------------------------------------ the exchange

// Shared makes the exchange check every request (see the top of this file).
// The admin token is kept in the exchange's file; a new one is made if there
// is none. Call it after Load.
func (x *Exchange) Shared() string {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.shared = true
	if x.guard == nil {
		x.guard = newGuard(DefaultLimits())
	}
	if x.adminToken == "" {
		x.adminToken = newToken()
		x.save()
	}
	return x.adminToken
}

// authed wraps an exchange endpoint: on a shared exchange the request must be
// signed, and may only touch the signer's own account and vaults.
func authed[Req, Resp any](x *Exchange, fn func(Req) (Resp, error)) http.HandlerFunc {
	inner := handle(fn)
	return func(w http.ResponseWriter, r *http.Request) {
		if !x.shared {
			inner(w, r)
			return
		}
		most := int64(1 << 20)
		if g := x.limits(); g != nil && g.Body > 0 {
			most = g.Body
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, most+1))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "Bad request.")
			return
		}
		if int64(len(body)) > most {
			writeErr(w, http.StatusRequestEntityTooLarge, "That request is too big.")
			return
		}
		acct, err := x.checkSigned(r, body)
		if err == nil {
			var req Req
			if json.Unmarshal(body, &req) == nil { // a bad body is reported by handle
				err = x.authorize(acct, req)
			}
		}
		if err != nil {
			code := http.StatusForbidden
			if _, ok := err.(limitErr); ok {
				code = http.StatusTooManyRequests
			}
			writeErr(w, code, err.Error())
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		inner(w, r)
	}
}

// checkSigned checks a request's signature and returns its account. The
// first key an account signs with is kept: later requests must use it.
func (x *Exchange) checkSigned(r *http.Request, body []byte) (string, error) {
	acct, keyHex, ts, sigHex := r.Header.Get(hdrAccount), r.Header.Get(hdrKey), r.Header.Get(hdrTime), r.Header.Get(hdrSig)
	nonce := r.Header.Get(hdrNonce)
	if acct == "" || sigHex == "" {
		return "", userErr("This exchange is shared, so your wallet must sign every request. Update your wallet program.")
	}
	if !validAccount(acct) {
		return "", userErr("Bad account name.")
	}
	t, err := strconv.ParseInt(ts, 10, 64)
	now := time.Now().Unix()
	if err != nil || t < now-requestWindow || t > now+requestWindow {
		return "", userErr("Your request's time is more than 5 minutes off the exchange's. Check your computer's clock.")
	}
	kb, err1 := hex.DecodeString(keyHex)
	sb, err2 := hex.DecodeString(sigHex)
	if err1 != nil || err2 != nil {
		return "", userErr("Your request's signature can't be read.")
	}
	pk, err1 := schnorr.ParsePubKey(kb)
	sig, err2 := schnorr.ParseSignature(sb)
	if err1 != nil || err2 != nil || len(nonce) > 64 || !sig.Verify(requestDigest(r.URL.Path, ts, nonce, body), pk) {
		return "", userErr("Your request's signature doesn't check out.")
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if known, ok := x.acctKeys[acct]; ok && known != keyHex {
		return "", userErr("That account belongs to another wallet (a different key).")
	}
	if _, dup := x.seen[sigHex]; dup {
		return "", userErr("This request was already used once.")
	}
	if len(x.seen) > 1000 {
		for k, at := range x.seen {
			if at < now-2*requestWindow {
				delete(x.seen, k)
			}
		}
	}
	x.seen[sigHex] = now
	if x.guard != nil && !x.guard.acct.allow(acct) {
		return "", limitErr(tooManyWallet)
	}
	if _, ok := x.acctKeys[acct]; !ok {
		if x.guard != nil {
			if err := x.guard.newAddress(addrOf(r), len(x.acctKeys), "account"); err != nil {
				return "", err
			}
		}
		x.acctKeys[acct] = keyHex
		x.save()
	}
	return acct, nil
}

// authorize checks that a signed request only touches the signer's own
// account and vaults. Every request type the exchange takes is listed: one
// that isn't is refused.
func (x *Exchange) authorize(acct string, req any) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	mine := func(a string) error {
		if a != acct {
			return userErr("That account isn't yours.")
		}
		return nil
	}
	vault := func(id string) error {
		if v := x.vaults[id]; v != nil {
			return mine(v.account)
		}
		return nil // an unknown vault is the endpoint's to report
	}
	evault := func(chain, id string) error {
		if xe := x.evm[evmID(chain)]; xe != nil {
			if v := xe.vaults[id]; v != nil {
				return mine(v.account)
			}
		}
		return nil
	}
	leg := func(chain, id string) error {
		if isEVM(chain) {
			return evault(chain, id)
		}
		return vault(id)
	}
	both := func(a, b error) error {
		if a != nil {
			return a
		}
		return b
	}
	switch r := req.(type) {
	case OpenKeysReq:
		return mine(r.Account)
	case OpenStartReq:
		return vault(r.VaultID)
	case OpenFinishReq:
		return vault(r.VaultID)
	case OrderReq:
		return vault(r.VaultID)
	case CommitReq:
		return vault(r.VaultID)
	case SettleReq:
		return vault(r.VaultID)
	case CapacityReq:
		return vault(r.VaultID)
	case CapacityCommitReq:
		return vault(r.VaultID)
	case CapacityFinishReq:
		return vault(r.VaultID)
	case CashReq:
		return mine(r.Account)
	case EthChainReq, InfoReq:
		return nil // prices and addresses, the same for everyone
	case EthOpenReq:
		return both(mine(r.Account), evault(r.Chain, r.VaultID))
	case EthOrderReq:
		return evault(r.Chain, r.VaultID)
	case EthCommitReq:
		return evault(r.Chain, r.VaultID)
	case EthCapacityReq:
		return evault(r.Chain, r.VaultID)
	case EthSettleReq:
		return evault(r.Chain, r.VaultID)
	case SwapReq:
		return both(mine(r.Account), both(leg(r.From.Chain, r.From.VaultID), leg(r.To.Chain, r.To.VaultID)))
	case PlaceReq:
		return both(mine(r.Account), leg(r.Chain, r.VaultID))
	case CancelReq:
		return mine(r.Account)
	case BookReq:
		if r.Account == "" {
			return nil
		}
		return mine(r.Account)
	case FillReq:
		return mine(r.Account)
	}
	return userErr("That request isn't allowed on a shared exchange.")
}

// adminOK: off a shared exchange anyone on this computer is the operator; on
// one, the request must carry the admin token.
func (x *Exchange) adminOK(r *http.Request) bool {
	x.mu.Lock()
	shared, tok := x.shared, x.adminToken
	x.mu.Unlock()
	return !shared || (tok != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get(hdrAdmin)), []byte(tok)) == 1)
}

// adminOnly lets through only the operator (adminOK).
func adminOnly(ok func(*http.Request) bool, h http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !ok(r) {
			writeErr(w, http.StatusForbidden, "Only the operator can do this on a shared server (it needs the admin token).")
			return
		}
		h.ServeHTTP(w, r)
	}
}

// Shared puts the watchtower in shared mode: its page (which lists every
// wallet it watches) needs the admin token. Call it after Load.
func (t *Tower) Shared() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.shared = true
	if t.guard == nil {
		t.guard = newGuard(DefaultLimits())
	}
	if t.adminToken == "" {
		t.adminToken = newToken()
		t.save()
	}
	return t.adminToken
}

func (t *Tower) adminOK(r *http.Request) bool {
	t.mu.Lock()
	shared, tok := t.shared, t.adminToken
	t.mu.Unlock()
	return !shared || (tok != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get(hdrAdmin)), []byte(tok)) == 1)
}

// claimDigest is what a wallet's EVM key signs to show a vault is its own
// before the exchange registers it under the wallet's account.
func claimDigest(chainID *big.Int, contract, vaultID, account string) []byte {
	id := "0"
	if chainID != nil {
		id = chainID.String()
	}
	return keccak([]byte("castlekeep-claim\n" + id + "\n" + strings.ToLower(contract) + "\n" + vaultID + "\n" + account))
}

// ------------------------------------------------------------------ the chains program

// gatewayMethods are the only node calls the gateway passes on: what the
// exchange, wallets and watchtower use to read the chain and send
// transactions. Nothing that changes the node, its wallet or its peers.
var gatewayMethods = map[string]bool{
	"getblockcount": true, "getblockhash": true, "getrawtransaction": true, "gettxout": true,
	"getrawmempool": true, "estimatefee": true, "searchrawtransactions": true, "sendrawtransaction": true,
	"getinfo": true, // the node client asks for the version before sending a transaction
}

var gatewayClient = &http.Client{Timeout: 30 * time.Second}

// rpcGateway passes allowed JSON-RPC calls on to a node (addr gives its
// host:port), adding the node's password itself. g limits how many calls
// from outside the server reach the node at once (nil: no limit).
func rpcGateway(addr func() string, g *guard) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST a JSON-RPC call.", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var call struct {
			Method string `json:"method"`
		}
		if err != nil || json.Unmarshal(body, &call) != nil {
			http.Error(w, "Send one JSON-RPC call (not a batch).", http.StatusBadRequest)
			return
		}
		if !gatewayMethods[strings.ToLower(call.Method)] {
			http.Error(w, "The gateway doesn't pass on "+call.Method+".", http.StatusForbidden)
			return
		}
		if addrOf(r) != "" {
			release, ok := g.enterNode()
			if !ok {
				http.Error(w, "The node is busy. Try again in a moment.", http.StatusServiceUnavailable)
				return
			}
			defer release()
		}
		up, err := http.NewRequest(http.MethodPost, "http://"+addr(), bytes.NewReader(body))
		if err != nil {
			http.Error(w, "The node isn't ready.", http.StatusBadGateway)
			return
		}
		up.SetBasicAuth("castlekeep", "castlekeep")
		up.Header.Set("Content-Type", "application/json")
		resp, err := gatewayClient.Do(up)
		if err != nil {
			http.Error(w, "The node didn't answer.", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, io.LimitReader(resp.Body, 64<<20))
	}
}

// localOnly answers only programs on this computer that came straight here,
// not through a web server in front (which adds X-Forwarded-For).
func localOnly(on bool, h http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if on && (r.Header.Get("X-Forwarded-For") != "" || !isLoopback(r.RemoteAddr)) {
			writeErr(w, http.StatusForbidden, "That's only for programs on the server itself.")
			return
		}
		h.ServeHTTP(w, r)
	}
}

func isLoopback(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// gatewayURL is the address a program uses for a chain's node: the gateway on
// the chains program when it is shared, otherwise the node itself.
func gatewayURL(chains string, c ChainStatus) string {
	if c.Shared && c.Gateway != "" {
		return strings.TrimRight(chains, "/") + c.Gateway
	}
	return c.RPC
}
