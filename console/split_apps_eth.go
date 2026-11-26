package main

// Ethereum in the three separate programs: node check, the exchange's admin
// view, and the adapter that lets the usual page drive the wallet's ETH.

import (
	"bytes"
	"encoding/json"

	"net/http"
	"sort"
	"time"
)

// ethNodeUp reports whether an Ethereum node answers at url.
func ethNodeUp(url string) bool {
	c := http.Client{Timeout: 3 * time.Second}
	r, err := c.Post(url, "application/json", bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`)))
	if err != nil {
		return false
	}
	defer r.Body.Close()
	var out struct{ Result string }
	return json.NewDecoder(r.Body).Decode(&out) == nil && out.Result != ""
}

// ethAdminView adds the EVM chains to the admin page's data (x.mu held).
func (x *Exchange) ethAdminView(out *XStateResp) {
	for _, xe := range x.evm {
		x.evmAdminView(xe, out)
	}
	sort.Slice(out.Vaults, func(i, j int) bool { return out.Vaults[i].ID < out.Vaults[j].ID })
}

func (x *Exchange) evmAdminView(xe *xEth, out *XStateResp) {
	name, addr := evmName(xe.id), ""
	if xe.c.public {
		c, _ := evmByID(xe.id)
		name, addr = c.netName(), xe.addr
	}
	out.Chains = append(out.Chains, XChainView{xe.id, xe.ticker, xe.mode, xe.price, fromWei(xe.c.balance(xe.addr)), addr, name, evmColor(xe.id), evmBadge(xe.id), xe.setupNote})
	for id, v := range xe.vaults {
		if len(v.states) == 0 {
			continue
		}
		st := "open"
		if v.closed {
			st = "closed"
		} else if v.settling {
			st = "settling"
		}
		l := v.latest()
		out.Vaults = append(out.Vaults, XVaultView{xe.id + "-" + id, v.account, xe.id, st, l.UserSats, l.ExchSats, len(v.states), v.events})
	}
}

// walletEthEngine lets the usual page drive the wallet's Ethereum.
type walletEthEngine struct {
	w            *Wallet
	id           string // eth, pol, base, bnb or avax
	info         ChainInfo
	chains, exch string
	eventLog
	copied   int
	lastNote string // public test networks: the last setup note shown
}

func (e *walletEthEngine) Info() ChainInfo     { return e.info }
func (e *walletEthEngine) Start()              {}
func (e *walletEthEngine) Stop()               {}
func (e *walletEthEngine) UseAccount(*Account) {}

func (e *walletEthEngine) Do(r ActionReq) error {
	defer e.w.Save()
	w := e.w
	var err error
	ok := ""
	switch r.Action {
	case "faucet":
		if e.info.Public {
			if bal := w.EthBalance(e.id); bal > 0 {
				ok = "Your address holds " + fmtCoin(bal, e.info.Ticker)
			} else {
				err = userErr("Nothing has arrived at %s yet. Coins from a faucet show up in a block or two (a few seconds).", w.EthAddress(e.id))
			}
			break
		}
		if err = call(e.chains, "/faucet", FaucetReq{Chain: e.id, Addr: w.evm[e.id].addr, Sats: unit}, nil); err == nil {
			ok = "Received 1 test " + e.info.Ticker
		}
	case "mine":
		if e.info.Public {
			err = userErr("Blocks come by themselves on this network; there's nothing to mine.")
			break
		}
		if err = call(e.chains, "/mine", MineReq{Chain: e.id, N: r.N, Secs: r.Secs}, nil); err == nil {
			call(e.exch, "/v1/tick", empty{}, nil)
			w.Tick()
			ok = "Moved the " + e.info.Name + " test chain forward"
		}
	case "lock":
		if err = w.EthOpen(e.id, r.Sats); err == nil {
			ok = "Locked " + fmtCoin(r.Sats, e.info.Ticker) + ". " + e.nextBlock() + "; your wallet then agrees state #0 with the exchange"
		}
	case "order":
		if err = w.EthOrder(e.id, r.Side, r.Sats); err == nil {
			ok = "Order filled: " + r.Side + " " + fmtCoin(r.Sats, e.info.Ticker) + ". Both signatures checked"
		}
	case "settle":
		if err = w.EthSettle(e.id); err == nil {
			ok = "Settled: both signed the settle message for the latest state"
		}
	case "exit":
		if err = w.EthExit(e.id); err == nil {
			ok = "You requested an exit"
		}
	case "claim":
		if err = w.EthClaim(e.id); err == nil {
			ok = "Paid out after the window"
		}
	case "capacity":
		if err = w.EthCapacity(e.id, r.Sats); err == nil {
			ok = "The exchange is locking " + fmtCoin(r.Sats, e.info.Ticker) + ". " + map[bool]string{true: "It confirms in the next block, in a few seconds", false: "Mine a block to confirm it"}[e.info.Public]
		}
	case "cash":
		if _, err = w.AddCash(); err == nil {
			ok = "Added $10,000 of test cash on the exchange"
		}
	case "mode":
		if err = w.Admin(e.id, r.Mode, 0); err == nil {
			ok = "Exchange is now " + r.Mode
		}
	case "price":
		err = w.Admin(e.id, "", r.USD)
	case "watchtower":
		w.mu.Lock()
		w.WT = r.On
		w.mu.Unlock()
		ok = map[bool]string{true: "Your watchtower is on", false: "Your watchtower is off"}[r.On]
	case "steal":
		err = userErr("In the separate programs the exchange's keys live in the exchange server, so the wallet can't make it try. The one-program console has this demo.")
	default:
		var handled bool
		if ok, err, handled = w.bookAction(e.id, e.info.Ticker, r); !handled {
			err = userErr("Unknown action.")
		}
	}
	h := e.w.evm[e.id].c.height()
	if err != nil {
		e.add(h, "bad", "Refused", err.Error(), "")
		return err
	}
	if ok != "" {
		e.add(h, "ok", ok, "", "")
	}
	return nil
}

func (e *walletEthEngine) Snapshot() Snapshot {
	s := Snapshot{Chain: e.info, Available: true, Ready: true, Progress: "Ready", Watchtower: e.w.WT, HasUserWT: true, Txs: []*TxInfo{}}
	var info InfoResp
	if err := e.w.look("/v1/info", InfoReq{e.id}, &info); err == nil {
		s.Price, s.Mode = info.Price, info.Mode
	} else {
		s.Mode = "offline"
	}
	var cr CashResp
	if e.w.look("/v1/cashbalance", CashReq{e.w.Account}, &cr) == nil {
		s.CashCents = cr.CashCents
	}
	s.Who, s.Book = e.w.Name, e.w.bookSnapshot(e.id)
	e.w.mu.Lock()
	defer e.w.mu.Unlock()
	s.Tower = e.w.towerNote(e.id)
	we := e.w.evm[e.id]
	c := we.c
	s.Height = c.height()
	now := c.chainTime()
	s.ChainTime = time.Unix(now, 0).UTC().Format("2 Jan 15:04 UTC")
	s.WalletSats = fromWei(c.balance(we.addr))
	if e.info.Public {
		s.ReceiveAddr = we.addr
		if we.setupNote != "" && we.setupNote != e.lastNote {
			e.add(s.Height, "info", "Setting up "+e.info.Name, we.setupNote, "")
		}
		e.lastNote = we.setupNote
	}
	v := we.vault
	if v == nil {
		s.Events = e.events
		return s
	}
	for ; e.copied < len(v.Events); e.copied++ {
		e.add(s.Height, "sign", v.Events[e.copied], "", "")
	}
	s.Events = e.events
	cv, _ := c.readVaultID(v.id)
	vv := &VaultView{Locked: cv.amount, BestState: -1, PublishedState: -1, ClockTotal: we.delay,
		UserDeposit: cv.userDep, ExchDeposit: cv.exchDep, FundPending: v.capPending, ExitSent: cv.exitStart != 0,
		Address: c.contract + " (vault #" + v.id.String() + ")", CloseKind: v.closed, CloseTxid: v.closeTx,
		Scripts: []Labelled{
			{"Settle now: both sign a separate settle message for the latest state", "settle(id, nonce, userAmount, userSig, exchangeSig)\nsignatures are on settleHash(...), not on a trade state\n" + ruleSig},
			{"During the " + durText(we.delay) + " exit window", "submitState(...)\nrequire(nonce > bestNonce)   // only a newer state wins"},
			{"After the window", "finalize(id)\npays the newest submitted state, or refunds each side its own deposit if nobody submitted one"},
		}}
	if len(v.states) > 0 {
		l := v.latest()
		vv.UserSats, vv.ExchSats = l.UserSats, l.ExchSats
	} else {
		vv.UserSats = cv.amount
	}
	for _, st := range v.states {
		vv.States = append(vv.States, StateView{Index: st.Index, Label: st.Label, UserSats: st.UserSats, ExchSats: st.ExchSats})
	}
	if cv.exitStart != 0 {
		vv.ClockStarted = true
		done := now - cv.exitStart
		if done > we.delay {
			done = we.delay
		}
		vv.ClockDone, vv.ClockReady = done, done >= we.delay
		if cv.hasBest {
			vv.BestState = int(cv.bestNonce)
		}
	}
	switch {
	case cv.closed || (v.closeTx != "" && c.receipt(v.closeTx) == 1):
		vv.Status = "closed"
		if vv.CloseKind == "" {
			vv.CloseKind = "finalized"
		}
	case v.closeTx != "":
		vv.Status = "closing"
	case c.receipt(v.openTx) != 1 || len(v.states) == 0 || v.capPending:
		vv.Status = "funding"
	case v.exitTx != "" && cv.exitStart == 0:
		vv.Status = "exit_pending"
	case cv.exitStart != 0 && vv.ClockReady:
		vv.Status = "ready"
	case cv.exitStart != 0:
		vv.Status = "exit_window"
	default:
		vv.Status = "open"
	}
	s.Vault = vv
	return s
}

// nextBlock says how the next block comes: by itself on a public test
// network, or with the mine button on a local one.
func (e *walletEthEngine) nextBlock() string {
	if e.info.Public {
		return "It confirms in the next block, in a few seconds"
	}
	return "Mine a block"
}
