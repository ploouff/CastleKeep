# Castlekeep console

A clickable version of Castlekeep for **Bitcoin, Litecoin, Ethereum, Polygon, Base, BNB Chain
and Avalanche**. It starts a
private test chain for each coin on your computer and opens a web page where you can lock
coins into a vault, trade, settle, request an exit, move the chain forward, and switch the
exchange between honest, offline and malicious.

Every button creates real transactions, and the chain itself (a Bitcoin node, a Litecoin
node, or the CastleVault smart contract on an EVM test node) decides whether each one is accepted or refused.

![Castlekeep console on Ethereum](preview-eth.png)

> **Test networks only.** The coins have no value, nothing leaves your computer, and the
> code hasn't been audited. Never use it with real coins.

## How each chain works

| Chain | How the vault is built | Exit window | Old-state attack |
| --- | --- | --- | --- |
| **Bitcoin** | Taproot 2-of-2 script, pre-signed exit request, `CHECKSEQUENCEVERIFY` timelock | 144 blocks (about 24 hours) | **Penalty, both ways:** whoever publishes a state gets their own share locked for 6 blocks. Once a state is replaced, each side holds the other's revocation secret for it, so publishing an old state lets the other side take the cheater's share |
| **Litecoin** | The same Taproot scripts as Bitcoin | 576 blocks (blocks are 2.5 minutes, so also about 24 hours) | Same as Bitcoin, with a 24-block lock on the exchange's share |
| **Ethereum** | `CastleVault` smart contract (`contract/CastleVault.sol`). Both sides sign every state. The exchange can lock its own ETH so you can buy from it. | 24 hours on the chain's clock | **Closed:** during an exit only a state with a higher number can replace an older one, and your watchtower submits the newest |
| **Polygon, Base, BNB Chain, Avalanche** | The same `CastleVault` contract and code as Ethereum, each on its own local test node with the chain ID of that network's public test network (Amoy 80002, Base Sepolia 84532, BNB testnet 97, Fuji 43113). Coins: POL, ETH (on Base), BNB, AVAX | 24 hours on the chain's clock | Closed, as on Ethereum |

### MuSig2: settling looks like a normal payment (Bitcoin and Litecoin)

New Bitcoin and Litecoin vaults combine your key and the exchange's into one key with
MuSig2 (BIP 327), used as the vault output's Taproot internal key. Settling with the exchange
and adding buying capacity (a splice) then use that key: one 64-byte signature that only
both of you together can make. On the chain it looks like an ordinary single-key payment, so
it is smaller (cheaper) and doesn't show that it was a vault or that two parties signed.

- Nothing else changes: the exit request, the pay-back after the exit window and the
  revocation penalties still use the scripts, which stay hidden unless they are needed.
- Signing takes the same messages as before: your wallet sends a fresh public nonce, the
  exchange answers with its nonce and its partial signature, and your wallet checks it, adds
  its own and checks the final signature before broadcasting.
- A nonce signs once: its secret half is wiped as soon as it has been used, and nonces are
  never saved. If a program stops halfway, that attempt is dropped and started again.
- Vaults opened before this (and vaults from an exchange that doesn't offer MuSig2) keep the
  old 2-of-2 script path and work as before.
- The transaction inspector says "Key path" for these spends, and the vault's scripts list
  starts with the combined key.

### One account, and swapping coins

All the chains belong to **one test account**. The bar at the top of every tab shows
every coin (wallet plus your share of each vault), one cash balance, and the
total in test dollars. Cash from selling one coin can be spent on another, and **Add
$10,000 test cash** works on every tab.

The **Swap into another coin** box sells some of this tab's coin and buys another coin with
the money, in one order and at the prices shown. No cash is used. Behind the scenes it is
two signed vault states, one on each chain:

1. The exchange signs the new state for the coin you buy.
2. Only then do you sign the sale of the coin you sell.

So you never give up one coin without already holding the other. It is not one blockchain
transaction (the chains can't see each other), and it is not a hash-locked "atomic swap".
Each vault is protected by its own chain's rules, exactly as for a normal trade.

What you can swap into follows the buying rules below: any coin you sold from that vault,
plus whatever buying capacity the exchange has locked there.

### Buying new coins (all chains)

Without help you can only buy back what you sold, because the vault only holds your coins.
On every chain you can ask the exchange to **lock its own coins in your vault** ("buying
capacity"), for a 0.2% fee taken from your share. You can then buy up to that amount with
your cash, or with a swap from another coin, and the coins you buy are in the vault from
the start.

- **Ethereum and the other EVM chains:** the exchange sends coins to the contract (`fund`), as often as needed.
- **Bitcoin and Litecoin:** you and the exchange both sign one transaction (a "splice")
  that spends the vault plus some of the exchange's coins into a bigger vault with the same
  scripts. The exit request and the pay-back are signed again, so the old ones (and every
  older state) stop working. The exchange can add more later with another splice. Its test
  wallet starts with 20 coins.

If an exit happens and nobody publishes a state, each side simply gets back what it put
in. Your watchtower publishes your latest state during the exit window, so the coins you
bought still count. With the watchtower off, they go back to the exchange.

## Set up (Windows, Command Prompt)

You need [Go](https://go.dev/dl/) 1.21 or newer. Each chain needs its own test node; any
chain you skip shows setup instructions in its tab, and the others still work.

**Bitcoin**
```
go install github.com/btcsuite/btcd@v0.24.2
```

**Litecoin**
```
go install github.com/ltcsuite/ltcd@v0.23.6
```

**Ethereum, Polygon, Base, BNB Chain and Avalanche: option A, Hardhat (tested)**

Install [Node.js](https://nodejs.org) (the LTS version). Then make the folder
`%USERPROFILE%\castlekeep-eth` and, in it:
```
npm init -y
npm install hardhat@2
```
Castlekeep starts its own Hardhat node for each EVM chain from that folder (one node per
chain, each with its own chain ID). You don't need to keep a Hardhat window open any more.
Use `-hardhat <folder>` if you installed it somewhere else.

**Ethereum: option B, Anvil**

Anvil is part of Foundry. Download the Windows release from Foundry's GitHub releases
page, unzip it, and put `anvil.exe` in `%USERPROFILE%\.foundry\bin`. Castlekeep starts it
automatically for every EVM chain (with `--chain-id`). This path uses the same calls as
Hardhat but hasn't been tested yet.

## Run it

Download the repository (**Code → Download ZIP**), unzip it, and open Command Prompt in the
`console` folder:

```
go mod tidy
go run .
```

To use a Hardhat node you started yourself for Ethereum (the other EVM chains still get
their own):

```
go run . -eth-rpc http://127.0.0.1:8545
```

Your browser opens at `http://127.0.0.1:8080`. Each chain mines its first blocks or
deploys its contract while its tab shows a progress screen; this takes about a minute.
Keep Command Prompt open while you use the console, and press **Ctrl+C** to stop. Each run
starts fresh.

**Options**

| Flag | Use |
| --- | --- |
| `-eth-rpc URL` | Use an Ethereum dev node that's already running (Hardhat or Anvil) |
| `-chains btc,eth,pol` | Run only some chains (default: all seven) |
| `-hardhat <folder>` | Where Hardhat is installed (default `%USERPROFILE%\castlekeep-eth`) |
| `-btcd`, `-ltcd`, `-anvil` | Point to a node program that isn't in its default folder |
| `-port 9000` | Use a different port if 8080 is taken |
| `-no-browser` | Don't open the browser automatically |

## Separate exchange and wallet programs (preview)

The console above runs both sides in one program. You can also run the exchange and your
wallet as **separate programs** that never share private keys: they only send each other
signatures over the network, and each reads the chain itself. Open three Command Prompt
windows in the `console` folder and start, in this order:

1. `go run . chains` starts the Bitcoin and Litecoin test nodes and a Hardhat node for each
   EVM chain (Ethereum, Polygon, Base, BNB Chain, Avalanche). Wait until it has mined its
   first blocks and deployed the contracts (about a minute). Add `-eth-rpc
   http://127.0.0.1:8545` to use a Hardhat node you started yourself for Ethereum.
2. `go run . exchange` is the exchange server. It holds only the exchange's keys, its wallet
   (20 test coins per chain), prices and cash. Its admin page opens at
   `http://127.0.0.1:9200`, where you set prices and switch it between honest, offline and
   malicious, and see every vault as the exchange sees it.
3. `go run . wallet` is your wallet. It holds only your keys and revocation secrets and
   opens the usual page at `http://127.0.0.1:9300`.

Your wallet checks every signature and amount the exchange sends before signing anything:

- When you lock coins, the deposit is only sent after the exchange has signed the exit
  request, the pay-back and the opening state.
- For each trade, the exchange signs your copy of the new state first. You then sign
  its copy and hand over your secret for the old state, and the exchange hands over its own.
- When the exchange adds coins (a splice), the secrets for the state before it are only
  swapped once the splice has confirmed.
- On Ethereum, the exchange proposes each new split, your wallet checks and signs it, and
  then the exchange signs. Settling needs a separate settle signature from both, only for
  the latest state.
- Once the exchange has signed a settlement, it stops trading on that vault, so an old
  settlement can't be used after later trades.

Swaps work across all three coins in the separate programs too. The exchange prepares both
sides at once and signs the side you buy first; your wallet checks both splits and the
prices before signing anything, completes the buying side, then signs the selling side.
The price of what you buy comes out of your cash as soon as the exchange signs it (your
balance can dip below zero for that moment), and the money from the sale goes back in once
your wallet signs the selling side, so a completed swap leaves your cash unchanged.

### Order book (several wallets)

Run more than one wallet against the same exchange, each with a name and its own port,
each in its own window:

```
go run . wallet -name alice
go run . wallet -name bob -port 9301
```

Each wallet page then has an **Order book** panel under the Exchange panel: the open sell
orders (asks) and buy orders (bids) from every wallet, your own orders with a Cancel
button, recent trades, and a form for a limit order (side, price per coin, amount).

- Orders match best price first, then oldest first. The trade happens at the price of the
  order that was already waiting, and that price becomes the coin's price.
- The exchange sits in the middle of every trade. When Alice sells to Bob, Alice's vault
  gets a new state that moves the coins to the exchange's side (her cash goes up), and
  Bob's vault gets one that moves the same amount from the exchange's side to his (his
  cash goes down). So to buy, your vault needs buying capacity from the exchange.
- While an order is open, the coins, buying capacity and cash it would use are set aside.
- Each wallet signs its own side of a fill, and only if it matches an order it placed:
  same coin and side, no more than is left, at its price or better. A resting order is
  signed by the wallet's background check within a couple of seconds.

The admin page shows every open order, by account, and the recent trades.

Not in the separate programs: the "Have the exchange try to steal" demo (the exchange's keys aren't in your wallet to try it with). Ctrl+C in the chains window
stops the test nodes.

### Saved between runs

The three programs keep everything in `%USERPROFILE%\castlekeep-data`, so you can close
them and carry on later:

- `chains\` keeps the Bitcoin and Litecoin test chains. (btcd and ltcd wipe a private test
  chain every time they start, so the chains program saves each block it mines and feeds
  them back in when it starts again. The node checks every block again.)
- `chains\evm-pol.jsonl` (one per EVM chain) keeps that chain. Hardhat forgets its chain when
  it stops, so the chains program records every change (transactions, balances, mined blocks
  and their times) and plays the record back into a new Hardhat node on the next start. The
  blocks, contract and vaults come back exactly; the chain's clock stands still while the
  programs are closed.
- `exchange.json` holds the exchange's keys, its coins, every vault with its signed
  states and secrets, cash balances, prices and the order books.
- `wallet-alice.json` (one per wallet name) holds your keys, revocation secrets, vaults
  and orders.

These files hold private keys in plain text. That's fine for test coins only.

**Protecting them with a password.** Start the exchange or a wallet with `-password`:

```
go run . wallet -name alice -password
```

It asks for a new password twice (not shown as you type) and from then on saves the file
encrypted: the password is stretched with scrypt and the file sealed with AES-256-GCM. An
existing plain file is copied to `wallet-alice.json.unprotected-<date>` first, so nothing is
lost; delete that copy yourself once the password works. Every later start asks for the
password (with or without `-password`); after three wrong tries the program stops without
changing anything. `-remove-password` saves the file in plain text again. There is no way to
get the keys back without the password. For scripts, `CASTLEKEEP_PASSWORD` can hold the
password instead of typing it. The chains' own files hold no user keys and stay plain.

Anything half-done when a program stopped (for example an order waiting for the second
signature) is dropped, and the other side just asks again. On the private test chains,
transactions still waiting for a block when the chains window closes are lost too, so mine
a block before closing it. With `-eth-rpc`, Ethereum lives in your own Hardhat window
instead: if that was restarted, its vaults are gone with it, and the programs say so.

If the chains program runs fewer networks than last time (for example
`chains -network testnets -coins eth` after using signet), the exchange and wallets say
which ones aren't running and keep their saved keys, coins and vaults in the file
unchanged, ready for when those networks run again.

To start over, add `-fresh` (for example `go run . chains -fresh`). The old data is
renamed (`… .old-<date>`), never deleted. If you start the chains fresh, start the exchange
and the wallets with `-fresh` too: they refuse a file made on different chains. Use
`-data <folder>` to keep the files somewhere else.

### Speeding up a waiting transaction (Bitcoin and Litecoin)

Most vault transactions are signed by both sides long before they are sent (the exit request,
the pay-back, each state), so their fee is fixed when they are signed. If fees rise, one of
them could wait a long time for a block. The separate programs handle this with "child pays
for parent": your wallet spends its own output of the waiting transaction in a new one with a
higher fee, and miners take both together.

- Vaults opened from now on have a small extra output for each side (330 base units) on the
  exit request, so either side can speed up an exit. Older saved vaults keep their old exit
  request (its signatures stay valid) and can still speed up the transactions that pay you
  directly: a published state, the pay-back, a settlement and your deposit.
- When one of your transactions is waiting, the wallet page shows **Speed up your waiting
  transaction**. It aims for the node's fee estimate, or 2 sat/vB when btcd has none (new
  private chains, a freshly synced signet node; the page says which), and at least twice the
  rate the waiting transaction pays.
- During an exit, your watchtower does this by itself once a transaction of yours has waited 2
  blocks.
- A pair that has been sped up can't be sped up again.

The one-program console still uses a flat fee: there you mine the blocks yourself.

### A separate watchtower (keeps watching while your wallet is closed)

Your wallet's own watchtower only works while the wallet program runs. On Bitcoin, if the
exchange publishes an old state while it's closed, you have 6 blocks (about an hour on
signet) to take the exchange's share before the exchange can keep it. The watchtower program
keeps watching for you:

```
go run . watchtower
go run . wallet -name alice -watchtower http://127.0.0.1:9400
```

The wallet remembers the watchtower (it's saved in the wallet's file); `-watchtower off` stops
using it and tells the watchtower to forget everything it holds for you.

- **It holds no keys to your coins.** After every trade your wallet hands it transactions
  signed in advance that can only pay you: for each state the exchange has revoked, the
  penalty sweep that takes the exchange's share; for each generation, your copy of the newest
  state, signed by both, when it pays you more than your deposit. On the EVM chains it holds
  your newest state with both signatures, which anyone may submit during an exit. Its only key
  is a fee wallet for the gas on the EVM chains (the chains program funds it on the local
  chains; on a public network, send it a little test coin).
- **What it does:** if the exchange publishes an old Bitcoin or Litecoin state, it sends the
  penalty sweep. If an exit request confirms and nobody publishes a state, it publishes your
  newest one. On the EVM chains, during an exit, it submits your newest state if the contract
  holds an older one (or none, when yours pays you more than your deposit).
- **Your wallet updates it before giving anything up.** On Bitcoin and Litecoin the wallet
  hands the watchtower each new state *before* it gives the exchange its secret for the old
  one, so the watchtower never holds a state the exchange could punish. If the watchtower
  can't be reached, the trade (or swap) is stopped before anything is handed over, and the
  page says so. Updates only go forward: the watchtower never swaps a newer state for an older
  one, and it only takes updates from the wallet that signed up with it (a random token).
- **Its page** (`http://127.0.0.1:9400`) lists each wallet's vaults, what it holds and what it
  did. The wallet page shows a line about it under **Your watchtower is on**.
- **Limits:** it sees your vault transactions and amounts (a real one would get them encrypted
  until needed), and the transactions signed in advance have the flat test fee, so it can't
  speed them up. To keep it running all the time it needs a computer that stays on.

### Bitcoin signet (public test network)

Signet is Bitcoin's public test network: real blocks about every 10 minutes, made by the
network, and coins with no value. Bitcoin only (Litecoin has no signet).

```
go run . chains -network signet
go run . exchange
go run . wallet -name alice
```

- The first `chains -network signet` downloads the signet chain into
  `castlekeep-data\chains-signet` (a few GB; it can take hours). The window shows how far
  it has got. Later starts only catch up.
- There's no Mine button and no built-in faucet. Each wallet page shows **your signet
  address**: get free coins from a public signet faucet (search the web for "signet
  faucet") and send them there, then press **Check for coins sent to your address**.
  The wallet also looks by itself every 15 seconds.
- The exchange's admin page shows the exchange's signet address. Send it coins too if you
  want to buy into a vault (buying capacity comes from its wallet).
- Everything waits for real blocks: a lock confirms in about 10 minutes, the cheat window
  is 6 blocks (about an hour), and the exit window is 144 blocks (about a day). The
  watchtowers keep running while the programs are open, and everything is saved if you
  close them.
- Anything you send on signet is public, like on the real network.

### Public EVM test networks (Sepolia, Amoy, Base Sepolia, BNB Chain testnet, Fuji)

```
go run . chains -network testnets -exit-window 1h
go run . exchange
go run . wallet -name alice
```

`-network testnets` is Bitcoin signet (as above) plus the public test networks of the five
EVM chains. Their coins have no value and come from public faucets.

| Coin | Public test network | Chain ID | Free public node used |
| --- | --- | --- | --- |
| Ethereum | Sepolia | 11155111 | ethereum-sepolia-rpc.publicnode.com |
| Polygon | Amoy | 80002 | polygon-amoy-bor-rpc.publicnode.com |
| Base | Base Sepolia | 84532 | sepolia.base.org |
| BNB Chain | BNB Chain testnet | 97 | bsc-testnet-rpc.publicnode.com |
| Avalanche | Fuji | 43113 | api.avax-test.network |

- **Safety check:** before using a node, the chains program asks it which chain it is on.
  A main network (real coins) is always refused, and so is any chain that isn't the
  expected test network. That chain is then left out.
- **Choosing networks:** `-coins eth,pol` uses only those, and leaving out `btc` skips signet
  (no signet download). `-rpc eth=https://...,pol=https://...` uses other nodes, for
  example your own from a node provider.
- **Exit window:** `-exit-window` sets it for new EVM vaults, from `1m` to `24h` (default
  `24h`). With `1h` you can try "the exchange disappears" in an afternoon. Bitcoin's exit
  window is counted in blocks and stays 144 (about a day on signet).
- **Coins:** there's no Mine button and no faucet button. Each wallet page shows **your
  address** on that network, with a link to the network's block explorer; send it test coins
  from a faucet. The exchange's addresses are on its admin page and in its window.
- **The contract:** the exchange puts the CastleVault contract on each network by itself once
  its address there has test coins, and tells the chains program the address (kept in
  `castlekeep-data\chains-signet\evm-public-<coin>.json`). Your wallet only uses a contract
  whose code is exactly the CastleVault code built into the program, and refuses to lock
  until then.
- **Each program signs its own transactions** (EIP-1559). Nobody else can send for them.
- **How much to send** (rough, fees change): the exchange needs enough to put the contract
  up plus what it will lock for buyers. Sepolia, Base Sepolia, BNB Chain testnet and Fuji
  fees are tiny now; Amoy's are higher, so send the exchange about 0.5 POL there. The page
  starts with 0.01 to lock and 0.005 to trade.
- Reads from the public nodes are kept for 4 seconds, so the pages and watchtowers don't
  flood them. A node that is busy or down only slows things down; the watchtowers try again.
- Everything you do on these networks is public.

## The screen

| Area | What it does |
| --- | --- |
| **Account bar** | Every coin you hold, the shared cash balance and the total value, on every tab. |
| **Chain tabs** | Switch between the coins. Each has a small coloured badge (drawn by the page, not the coin's official logo). A green dot means ready. |
| **Top-right buttons** | Move the chain forward. On Bitcoin and Litecoin these mine blocks; on the EVM chains they mine one block or skip the clock ahead an hour or a day. |
| **Your wallet** | Test coins only you control. Get more from the faucet, or lock some into a vault. On the EVM chains, gas fees come out of this. |
| **Vault** | The latest signed split, the exit clock, and the Settle, Request exit and Claim (or Pay out) buttons. On Ethereum it also shows the newest state submitted during an exit. |
| **Exchange** | Set the price, sell or buy, swap into another coin, choose how the exchange behaves, and switch your watchtower on or off. |
| **Activity and Transaction inspector** | Everything that happened, and the exact rule that accepted or refused each transaction. |

## Things to try

Each tab's "Things to try" box has the steps for that chain. The best comparison:

1. **Bitcoin tab:** sell 0.5, buy 0.5 back, set the exchange to malicious, request an exit,
   mine twice. The exchange publishes your *older* state, and your watchtower uses that
   state's revocation secret to take the exchange's share (a "penalty sweep"). Mine once
   more: you get the whole vault back. With the watchtower off, the exchange keeps its share.
2. **Ethereum tab:** do the same, then mine twice. The exchange submits the old state, your
   watchtower submits the newer one, and after **Skip 1 day** and **Pay out after the
   window** you get the whole vault back.
3. **Ethereum tab again with the watchtower switched off:** the old state stands. That's
   why someone must watch on your behalf during an exit.

**Swapping:** open a vault on Bitcoin (lock 1 BTC, mine a block). On the Ethereum tab, lock
1 ETH, mine a block, ask the exchange to lock 1 ETH and mine again. Back on the Bitcoin tab,
swap 0.04 BTC into ETH. Both vaults get a new state, and the account bar shows about
0.87 more ETH and 0.04 less BTC.

**Buying new ETH:** on the Ethereum tab, lock 1 ETH and mine a block. In the vault, ask
the exchange to lock 1 ETH and mine a block. Click **Add $10,000 test cash**, choose
**Buy**, buy 0.8 ETH, then settle and mine. Your wallet gets about 1.8 ETH back.

## Files

| File | Purpose |
| --- | --- |
| `main.go` | Starts the chains and the web server |
| `engine.go` | Types shared by every chain |
| `account.go` | The shared test account (one cash balance) and coin-to-coin swaps |
| `utxo.go`, `utxo_vault.go` | Bitcoin and Litecoin engine: Taproot scripts and transactions |
| `musig.go` | MuSig2: the combined key, nonces, partial signatures for settling and splices |
| `evm_public.go` | The public EVM test networks (Sepolia, Amoy, Base Sepolia, BNB Chain testnet, Fuji) |
| `eth.go` | Ethereum engine: JSON-RPC, Keccak, signatures and contract calls |
| `evm.go` | The EVM chains (Ethereum, Polygon, Base, BNB Chain, Avalanche): names, chain IDs, test prices, icon colours, and starting Hardhat |
| `evm_keep.go` | Keeps the EVM test chains between runs (records changes, plays them back) |
| `contract/CastleVault.sol` | The Ethereum smart contract (`.bin` is the compiled bytecode) |
| `web/index.html` | The interface |
| `split_core.go`, `split_net.go` | Separate programs: the vault built from public keys only, and the messages between them |
| `split_exchange.go`, `split_wallet.go` | The exchange server and your wallet as separate programs |
| `split_apps.go`, `web/exchange.html` | The `chains`, `exchange` and `wallet` commands, and the exchange's admin page |
| `split_eth.go`, `split_apps_eth.go` | The EVM chains in the separate programs |
| `split_swap.go` | Swaps in the separate programs |
| `split_tower.go`, `web/tower.html` | The separate watchtower program and its page, and the wallet's side of it |

## Known limits

- **Claim alone on Bitcoin and Litecoin pays back deposits.** Without buying capacity only
  you deposit, so you get the whole vault. With it, the exchange gets its deposit back too,
  and your watchtower publishes your latest state if it pays you more.
- **Revocation on Bitcoin and Litecoin only works if someone is watching.** Your watchtower
  must sweep an old state within 6 blocks (BTC) or 24 blocks (LTC). Ethereum uses state numbers.
  In the separate programs, the watchtower program does this while your wallet is closed, as
  long as it keeps running.
- **Buying capacity waits for a block each time.** You can ask for more as often as you like
  (up to 5 coins per request), but only one request waits for a block at a time. On signet
  that is about 10 minutes per request.
- **Changing the contract leaves old EVM vaults behind.** When the contract's code changes, the
  chains program puts the new code on its kept local chains at the next start, and the
  exchange and wallets drop vaults saved on the old contract (test coins only). Settle EVM
  vaults before updating. On the public test networks the exchange puts the new code up itself.
- **The cash side is custodial**, as on any exchange. One cash balance is shared by all coins.
- **Swaps are two signed states, not one transaction.** They are only as safe as the two
  vaults they touch, and there is no hash-locked atomic swap.
- **Flat fees on Bitcoin and Litecoin**, and no fee bumping.
- **Nothing is saved.** Restarting starts new chains.
- **Solana is a design only**: see `../solana`.
