package main

// Shared types for every chain engine. Amounts are always in units of
// 0.00000001 of the chain's coin (satoshis for BTC, litoshis for LTC, and
// 10 gwei for ETH), so the interface formats every chain the same way.

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

const unit = 100_000_000 // base units per coin

type MineButton struct {
	Label string `json:"label"`
	N     int    `json:"n,omitempty"`
	Secs  int64  `json:"secs,omitempty"`
	Title string `json:"title,omitempty"`
}

type ChainInfo struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	Ticker       string       `json:"ticker"`
	ClockUnit    string       `json:"clockUnit"` // "blocks" or "seconds"
	BlockMinutes float64      `json:"blockMinutes"`
	MineButtons  []MineButton `json:"mineButtons"`
	ClaimLabel   string       `json:"claimLabel"`
	DefaultLock  string       `json:"defaultLock"`
	DefaultOrder string       `json:"defaultOrder"`
	Model        string       `json:"model"`              // one-line description of how the vault works here
	DualFund     bool         `json:"dualFund"`           // the exchange can lock buying capacity
	TestCash     bool         `json:"testCash"`           // a button to add test dollars
	Signet       bool         `json:"signet,omitempty"`   // Bitcoin's public signet: no mine buttons, no faucet
	EVM          bool         `json:"evm,omitempty"`      // runs the CastleVault contract (Ethereum, Polygon, Base, BNB Chain, Avalanche)
	Color        string       `json:"color,omitempty"`    // coin icon colour
	Badge        string       `json:"badge,omitempty"`    // coin icon text
	Public       bool         `json:"public,omitempty"`   // a public EVM test network (Sepolia, Amoy, ...): no mine buttons, no faucet
	Explorer     string       `json:"explorer,omitempty"` // that network's block explorer
}

type ActionReq struct {
	Chain  string `json:"chain"`
	Action string `json:"action"`
	Sats   int64  `json:"sats"`
	N      int    `json:"n"`
	Secs   int64  `json:"secs"`
	USD    int64  `json:"usd"`
	Mode   string `json:"mode"`
	Side   string `json:"side"`
	By     string `json:"by"`
	On     bool   `json:"on"`
	To     string `json:"to"` // swap: the chain whose coin you buy
	ID     int    `json:"id"` // order book: the order to cancel
}

type Event struct {
	ID     int    `json:"id"`
	Height int64  `json:"height"`
	Time   string `json:"time"`
	Kind   string `json:"kind"` // info, ok, bad, warn, sign
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
	Txid   string `json:"txid,omitempty"`
}

type IO struct {
	Label string `json:"label"`
	Sats  int64  `json:"sats"`
	Ref   string `json:"ref"`
}

type TxInfo struct {
	Txid    string `json:"txid"`
	Label   string `json:"label"`
	Path    string `json:"path"`
	Script  string `json:"script,omitempty"`
	Inputs  []IO   `json:"inputs"`
	Outputs []IO   `json:"outputs"`
	Status  string `json:"status"` // mempool, confirmed, rejected
	Height  int64  `json:"height,omitempty"`
	Error   string `json:"error,omitempty"`
	Seq     int    `json:"seq"`
}

type StateView struct {
	Index    int    `json:"index"`
	Label    string `json:"label"`
	UserSats int64  `json:"userSats"`
	ExchSats int64  `json:"exchSats"`
	Height   int64  `json:"height"`
	// Bitcoin and Litecoin only: the hash of this state's revocation secret,
	// and the secret itself once the exchange has handed it over.
	RevHash string `json:"revHash,omitempty"`
	Secret  string `json:"secret,omitempty"`
}

type VaultView struct {
	Status       string      `json:"status"` // funding, open, exit_pending, exit_window, ready, closing, closed
	Address      string      `json:"address"`
	Locked       int64       `json:"locked"`
	UserSats     int64       `json:"userSats"`
	ExchSats     int64       `json:"exchSats"`
	States       []StateView `json:"states"`
	ExitSent     bool        `json:"exitSent"`
	ClockStarted bool        `json:"clockStarted"`
	ClockTotal   int64       `json:"clockTotal"`
	ClockDone    int64       `json:"clockDone"`
	ClockReady   bool        `json:"clockReady"`
	BestState    int         `json:"bestState"` // Ethereum: newest state submitted in the window, -1 if none
	CloseTxid    string      `json:"closeTxid,omitempty"`
	CloseKind    string      `json:"closeKind,omitempty"`
	Scripts      []Labelled  `json:"scripts"`
	UserDeposit  int64       `json:"userDeposit"`
	ExchDeposit  int64       `json:"exchDeposit"`
	FundPending  bool        `json:"fundPending"`
	// Bitcoin and Litecoin only: the state the exchange published (-1 if
	// none), the delay on its share, and who took that share.
	Revocation     bool   `json:"revocation"`
	PenaltyDelay   int64  `json:"penaltyDelay,omitempty"`
	PublishedState int    `json:"publishedState"`
	PublishedBy    string `json:"publishedBy,omitempty"` // "exchange" or "user"
	SweepKind      string `json:"sweepKind,omitempty"`
	SweepTxid      string `json:"sweepTxid,omitempty"`
}

type Labelled struct {
	Label string `json:"label"`
	Text  string `json:"text"`
}

type Snapshot struct {
	Chain       ChainInfo  `json:"chain"`
	Available   bool       `json:"available"`
	Unavailable string     `json:"unavailable,omitempty"`
	Ready       bool       `json:"ready"`
	Progress    string     `json:"progress"`
	InitErr     string     `json:"initErr,omitempty"`
	Height      int64      `json:"height"`
	Mempool     int        `json:"mempool"`
	ChainTime   string     `json:"chainTime,omitempty"`
	Price       int64      `json:"price"`
	Mode        string     `json:"mode"`
	Watchtower  bool       `json:"watchtower"`
	HasUserWT   bool       `json:"hasUserWatchtower"`
	WalletSats  int64      `json:"walletSats"`
	WalletPend  int64      `json:"walletPending"`
	CashCents   int64      `json:"cashCents"`
	ExchSats    int64      `json:"exchWalletSats"`
	Vault       *VaultView `json:"vault"`
	Events      []Event    `json:"events"`
	Txs         []*TxInfo  `json:"txs"`
	ReceiveAddr string     `json:"receiveAddr,omitempty"`  // signet: where to send coins to this wallet
	Who         string     `json:"who,omitempty"`          // separate programs: the wallet's name
	Bumpable    int        `json:"bumpable,omitempty"`     // separate programs: your transactions waiting for a block that can be sped up
	FeeNote     string     `json:"feeNote,omitempty"`      // separate programs: the node's fee estimate
	Book        *BookResp  `json:"book,omitempty"`         // separate programs: this coin's order book
	Tower       string     `json:"tower,omitempty"`        // separate programs: the separate watchtower, in a line
	ExchDown    string     `json:"exchangeDown,omitempty"` // separate programs: why the exchange can't be reached right now
	Exchange    string     `json:"exchange,omitempty"`     // separate programs: the exchange's address
}

type Engine interface {
	Info() ChainInfo
	Start()
	Stop()
	Snapshot() Snapshot
	Do(req ActionReq) error
	UseAccount(acct *Account)
}

// ------------------------------------------------------------------ shared helpers

var errUser = errors.New("")

func userErr(f string, a ...any) error { return fmt.Errorf("%w%s", errUser, fmt.Sprintf(f, a...)) }

func fmtCoin(s int64, ticker string) string {
	neg := s < 0
	if neg {
		s = -s
	}
	str := fmt.Sprintf("%d.%08d", s/unit, s%unit)
	str = strings.TrimRight(strings.TrimRight(str, "0"), ".")
	if neg {
		str = "-" + str
	}
	return str + " " + ticker
}

func fmtUSD(cents int64) string {
	d := cents / 100
	s := fmt.Sprintf("%d", d)
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return "$" + string(out)
}

// eventLog is embedded by engines.
type eventLog struct {
	events []Event
}

func (l *eventLog) add(height int64, kind, title, detail, txid string) {
	l.events = append(l.events, Event{
		ID: len(l.events) + 1, Height: height, Time: time.Now().Format("15:04:05"),
		Kind: kind, Title: title, Detail: detail, Txid: txid,
	})
	if len(l.events) > 300 {
		l.events = l.events[len(l.events)-300:]
	}
}

// tradeBook is the trading state both engine types keep. The cash balance
// lives in the Account, which every chain shares.
type tradeBook struct {
	acct  *Account
	price int64
	mode  string
}

// UseAccount makes the engine share acct's cash balance with other chains.
func (t *tradeBook) UseAccount(acct *Account) { t.acct = acct }

func (t *tradeBook) tradePrice() int64 { return t.price }

// addTestCash is the "Add $10,000 test cash" button, on every chain.
func (t *tradeBook) addTestCash(log func(kind, title, detail, txid string)) {
	bal := t.acct.add(testCashCents)
	log("info", "Added $10,000 of test cash on the exchange",
		fmt.Sprintf("Your account's cash is now %s, shared by every coin. Like any exchange balance, the exchange holds this cash.", fmtUSD(bal)), "")
}

func orderCost(sats, price int64) int64 { return sats * price * 100 / unit } // cents

func describeHours(minutes float64) string {
	if minutes < 60 {
		return fmt.Sprintf("about %.0f minutes", minutes)
	}
	h := minutes / 60
	if h < 48 {
		return fmt.Sprintf("about %.0f hours", h)
	}
	return fmt.Sprintf("about %.0f days", h/24)
}
