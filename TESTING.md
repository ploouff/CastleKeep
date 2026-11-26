# Testing Castlekeep

Test networks only. Nothing here uses real coins, and the code has not been audited.

All commands are for PowerShell or Command Prompt on Windows, run from the folders shown.

## A. The automatic tests

1. Hardhat must be installed in `%USERPROFILE%\castlekeep-eth` (see `console\README.md`). The
   tests start their own Hardhat node for each EVM chain (Ethereum, Polygon, Base, BNB Chain,
   Avalanche) and stop them at the end. If a Hardhat node is already running at
   `http://127.0.0.1:8545`, the Ethereum tests use that one instead.
2. In a Command Prompt window:
   ```
   cd console
   go test -v -timeout 30m
   ```
3. It takes under a minute. The last lines should say `PASS` and `ok castlekeep-console`.
   Every `--- PASS` line is one scenario; a `--- FAIL` line is followed by what was expected
   and what happened.

If you use your own Hardhat node at 8545, stop any console that uses it first (Ctrl+C): both
mine blocks and move its clock. Without Hardhat, the EVM tests are skipped with a message.

## B. The one-program console

```
cd console
go run .
```

The page opens at `http://127.0.0.1:8080`, with a tab for each coin. Wait for the green dots
on the tabs. (It starts its own Hardhat node for each EVM chain. Add `-chains btc,ltc,eth` to
run fewer chains.)

**First visit.** The **Start here** box walks through a first try in five steps (get coins,
lock, confirm, trade, get your coins back), ticking each off; its buttons do the step or show
where it is. **Hide this guide** puts it away, and the link under the tabs brings it back.

**Test tools.** The recipes below that make the exchange misbehave, change the price or
cheat need **Test tools** switched on (top right; the page remembers). With it off, those
buttons are hidden, and the page says so if the exchange was left offline or malicious.

**Buy new BTC.** On the Bitcoin tab: lock `1`, Mine 1 block. In the vault's "Buy more than
you sell" box click **Ask the exchange to lock BTC**, Mine 1 block. Click **Add $10,000 test
cash** six times (0.8 BTC costs $52,000). In the Exchange panel choose **Buy**, enter `0.8`,
**Sign and buy**. **Settle with the exchange**, Mine 1 block. Your wallet goes up by about
0.798 BTC.

**Cheat as the user.** On Bitcoin: lock 1, mine, sell 0.6. Click **Request exit**, then
straight away **Publish an older state yourself**. Mine 1 block. The vault note says the
exchange's watchtower took your share: you lose the whole vault.

**Swap coins.** Open a vault on Bitcoin and on Ethereum (lock 1, mine, on each). On Ethereum
ask the exchange to lock 1 ETH and mine. On the Bitcoin tab, in "Swap into another coin",
enter `0.04`, pick **into ETH**, **Sign and swap**. The account bar shows 0.04 less BTC and
about 0.867 more ETH; cash and the total are unchanged.

**A malicious exchange.** Lock, mine, sell 0.5, buy 0.5 back. Set the exchange to
**Malicious**, **Request exit**, mine twice (on Ethereum: then **Skip 1 day** and **Pay out
after the window**). You get the whole vault back.

## C. The separate programs

**The quick way, one window:**

```
cd console
go run . start -name alice
```

It starts the chains, then the exchange and the watchtower, then your wallet, labels every
line with the program it came from, opens your wallet's page and prints the other pages'
addresses. Check: Ctrl+C stops everything within a few seconds and says "Everything has
stopped" (no `btcd`, `ltcd` or `node` left in Task Manager). Started twice, the second one
says the ports are in use and stops; `-ports 9600` runs a second set beside the first.

**Or three windows**, in this order. The chains program starts its own Hardhat node for each
EVM chain, so you don't need a Hardhat window:

```
cd console
go run . chains
```
Wait about a minute, until it has mined its first blocks and deployed the contracts.
```
cd console
go run . exchange
```
```
cd console
go run . wallet
```

- Your wallet: `http://127.0.0.1:9300` (the usual page)
- The exchange's admin page: `http://127.0.0.1:9200`

Try any recipe from B on the wallet page; they behave the same, except "Have the exchange
try to steal", which explains why it isn't available there (the exchange's keys aren't in
your wallet).

On the admin page, switch a coin between Honest, Offline and Malicious, then request an exit
from your wallet and mine: the admin page's Vaults table shows what the exchange did, and
your wallet shows what your watchtower did. Change a price there: the wallet's price and swap
estimate follow it.

Ctrl+C in each window stops it; the chains window also stops its test nodes.

## D. The order book (two users)

Start the chains and the exchange as in C, then two wallets instead of one, each in its own
window:

```
cd console
go run . wallet -name alice
```
```
cd console
go run . wallet -name bob -port 9301
```

- Alice: `http://127.0.0.1:9300` · Bob: `http://127.0.0.1:9301` · admin: `http://127.0.0.1:9200`

1. **Both lock coins.** On each page, Bitcoin tab: lock `1`, Mine 1 block.
2. **Bob gets buying capacity.** On Bob's page: **Ask the exchange to lock BTC**, Mine 1 block
   twice. Click **Add $10,000 test cash** twice.
3. **Alice sells.** On Alice's page, in the Order book panel: **Sell**, price `64000`,
   amount `0.3`, **Place sell order**. Both pages' book now shows 0.3 BTC at $64,000.
4. **Bob buys above her price.** On Bob's page: **Buy**, price `64500`, amount `0.2`,
   **Place buy order**. It matches straight away at Alice's $64,000:
   - Bob's vault has a new state "Bought 0.2 BTC at $64,000 (order book)", and his cash is
     $12,800 lower.
   - Within a couple of seconds Alice's wallet signs her side: her share is 0.8 BTC and her
     cash is $12,800 higher.
   - The book shows 0.1 BTC left at $64,000, and the price is now $64,000.
5. **Cancel.** On Alice's page, **Cancel** next to her order. The book is empty again.
6. **Refused orders** (red message): selling more than your vault holds, buying with no
   buying capacity, or buying with too little cash.
7. The admin page's **Order books** section lists every open order by account
   (`alice-…`, `bob-…`) and the recent trades.

Works the same on Litecoin and on every EVM chain (there, Bob asks the exchange to lock that
chain's coin, for example AVAX).

## E. Saving and restarting

1. Do D up to step 3 (Alice's sell order is waiting in the book).
2. Close all four windows (Ctrl+C, or just close them). Mine a block first if anything is
   waiting for one: on the private chains, unconfirmed transactions are lost on a restart.
3. Start them again, same commands, same order. The exchange and wallet windows say
   `Carrying on from …\castlekeep-data\…`.
4. Both pages show the same vaults, states, cash and Alice's order as before.
5. Carry on with D step 4: Bob's buy matches Alice's saved order.

To start over: add `-fresh` to all four commands. The old files are renamed, not deleted.

**With a password.** Close the wallet window and start it again with `-password` added. Type
a password twice (at least 8 characters; nothing shows as you type). Open
`%USERPROFILE%\castlekeep-data\wallet-alice.json` in Notepad: it now starts with
`"castlekeep": "sealed"` and shows no keys. Restart the wallet: it asks for the password, and
a wrong one three times stops it with nothing changed.

The automatic tests `TestSave…` do the same without windows: they stop the exchange and
both wallets mid-trade, load them from their files, check nothing changed, fill the saved
order, and check the exchange's watchtower still catches an old state using a secret it
saved. `TestKeptChainRestarts` stops and restarts a kept Bitcoin chain.

## F. Bitcoin signet (public test network)

1. In the first window: `go run . chains -network signet`. The first time it downloads the
   signet chain (a few GB; it can take hours). Wait until it stops printing
   "Downloading…" (the page and the other programs wait for it too).
2. `go run . exchange` and `go run . wallet -name alice`, as in C.
3. On Alice's page (Bitcoin signet tab), copy **your signet address**. Get coins from a
   public signet faucet (search the web for "signet faucet"), send them to that address,
   and press **Check for coins sent to your address**.
4. Lock `0.002` and wait for the next block (about 10 minutes). The vault opens.
5. Sell `0.001`, then **Settle with the exchange**. Your coins come back in the next block.
6. To try buying into the vault, send signet coins to the exchange's address (shown on its
   admin page) first, then ask the exchange to lock some in your vault.

There's no Mine button on signet: blocks come by themselves. The exit window is 144 blocks,
about a day.

## G. The other EVM chains (Polygon, Base, BNB Chain, Avalanche)

They run the same contract as Ethereum, each on its own local test chain, and appear as their
own tabs (one-program console and separate programs alike). The coin icons are simple
coloured badges drawn by the page.

1. On the Polygon tab: lock `1`, Mine 1 block, sell `0.4`. Your cash goes up by $0.40 (POL's
   test price is $1).
2. On the Avalanche tab: lock `1`, mine, **Ask the exchange to lock AVAX** (`1`), mine twice.
3. On the Bitcoin tab: lock `1`, mine, then swap `0.0004` BTC **into AVAX**. The Avalanche vault
   shows "Swapped 0.0004 BTC for 0.86666666 AVAX".
4. Close all three windows and start them again: the chains window says `Polygon: carried on
   from … saved steps`, and both vaults are as you left them.

Base's coin is ETH too; the swap list calls it "ETH on Base" to tell it apart from Ethereum.

## H. Public EVM test networks (Sepolia, Amoy, Base Sepolia, BNB Chain testnet, Fuji)

The automatic tests `TestPublicEVMNetwork` and `TestPublicEVMRestart` run this on a Hardhat
node that behaves like a public network (it mines by itself and can't send for anyone), and
`TestPublicChainCheck` checks that a main network is refused. By hand:

1. First window: `go run . chains -network testnets -exit-window 1h`. To start with just
   Sepolia and skip the signet download: `-coins eth`. Each network prints "connected".
2. Second window: `go run . exchange`. It prints the exchange's address on each network.
3. Third window: `go run . wallet -name alice`. It prints your addresses; they are also on
   the page under **Your wallet**.
4. From a faucet for that network, send test coins to the **exchange's** address. Within
   about half a minute the exchange window says it put the CastleVault contract on the
   network, and the chains window shows its address.
5. Send test coins to **your** address and press **Check for coins sent to your address**.
6. Lock `0.01` and wait a few seconds: the vault opens. Sell `0.005`, then settle.
7. Exit test: lock again, set the exchange to **Offline** (Test tools on, or on its admin page), **Request exit**, wait the hour,
   then press **Pay out after the window**.
8. The link under your address opens your transactions on the network's block explorer.

## I. MuSig2 settlements (Bitcoin and Litecoin)

`go test -run MuSig2 -v` checks that settlements and splices on new vaults are a single
64-byte signature on the chain, that a damaged partial signature from the exchange is refused
(and settling again then works), that a nonce can't sign twice, that nothing about a half-done
signing is saved, and that vaults opened without MuSig2 still use the script. By hand:

1. Open a Bitcoin vault (console or separate programs), trade, then **Settle with the
   exchange** and mine a block.
2. In **Activity**, click **Inspect transaction** under "Cooperative settlement": the
   Transaction inspector says "Key path: one MuSig2 signature…".
3. Under **Show the vault's scripts**, the first entry is the combined key; the scripts below
   it are the hidden backup paths.

## J. The separate watchtower (while your wallet is closed)

`go test -run Tower -v` checks the following on Bitcoin, Litecoin, Ethereum and Base:
- With the wallet closed, the watchtower takes the exchange's share when the exchange
  publishes an old state.
- It publishes your newest state when an exit confirms and nobody else does.
- On the EVM chains, it submits your newest state over an old one the exchange submitted.
- It keeps working after a restart from its file.
- A trade is stopped, with nothing handed over, while the watchtower can't be reached.
- It never goes back to an older state, and it refuses another wallet's token.

By hand:

1. Start the chains and the exchange as in C. In a third window: `go run . watchtower`. Its page
   opens at `http://127.0.0.1:9400`.
2. Fourth window: `go run . wallet -name alice -watchtower http://127.0.0.1:9400`. Under
   **Your watchtower is on**, the wallet page now has a line about the separate watchtower.
3. On the Bitcoin tab: lock `1`, Mine 1 block, sell `0.5`, buy `0.5` back. The watchtower's page
   shows the vault with 1 penalty sweep ready.
4. Switch **Your watchtower is off** (as if your wallet were closed) and set the exchange to
   **Malicious** on the admin page.
5. **Request exit**, then Mine 1 block: the exchange publishes your old state. Wait a few
   seconds and Mine 1 block again: the watchtower's page says it sent the penalty sweep. Mine
   once more: your wallet shows the penalty sweep, and you get the whole vault.
6. Close the watchtower window, then try to sell: the trade is stopped with "Your watchtower
   couldn't be reached", and nothing was handed over. Start the watchtower again to carry on.

## K. Shared mode (a server friends use)

`go test -run Shared -v` checks the following:
- A shared exchange refuses unsigned requests, another wallet's vaults and cash, an account
  name used with a different key, a replayed request, a request more than 5 minutes old, and
  a request changed after signing.
- Modes, prices and the admin data need the admin token, and it and the account keys are kept
  across restarts.
- On Ethereum, nobody else can register your vault's number.
- The gateway passes on only the allowed node calls, and the faucet only answers programs on
  the same computer.
- A wallet that reaches the chain only through the gateway can lock, trade and settle.
- A shared watchtower's page needs the token.
- `go test -run "Limit|AddrOf|GatewayNodeCalls" -v` checks the limits: requests a second
  from one address and one account (429 Too Many Requests), new accounts a day from one
  address and in all, vaults (unfinished ones make room), open orders, the "Add test cash"
  ceiling, request size, what the watchtower keeps per wallet, and calls to the node at once.
  Requests from the server itself aren't counted, and without `-shared` nothing is limited.
  `TestWalletPageReads` checks the wallet reuses the page's reads for 2 seconds and asks
  again straight after anything it does itself.

By hand on your own PC (no server needed), with the public test networks:

1. `go run . chains -network testnets -coins eth -shared`
2. `go run . exchange -shared`: it prints the admin page's address with `?token=...`. Open it
   without the token: the page shows "Only the operator…". With the token it works.
3. `go run . wallet -name alice`. Its page works as usual; with **Test tools** on, switching
   the exchange to Offline is refused ("Only the operator…").

For a real server, follow [SERVER.md](SERVER.md).

`go test -run "ServerAddress|SavedServer|TowerAnswers|StartHelpers|FilesPerNetwork" -v` checks
the wallet's `-server` option (the three addresses on a server, remembered in the wallet's
file, a watchtower only when one really answers there), the helpers of `go run . start`, and
that the public test networks keep their files in `castlekeep-data\testnets`.

## L. The page itself

On the wallet page (any of the ways above):

- The account bar is one row on a laptop screen. Pick a coin in it: its tab opens. Make the
  window narrow: only the totals show, with **Show coins**.
- With no vault, the vault panel has the box to lock coins. Type an amount and press Enter:
  the Lock button shows it's working, then a message in the bottom-right says what happened
  ("Locked 0.5 BTC into a new vault…").
- Before the deposit confirms, the Exchange panel says why trading is off. Mine a block: it
  turns on.
- Settle and mine: the closed vault shows the box to open the next one.
- Switch on **Test tools**, set the exchange to Offline and press **Settle with the exchange**:
  the message is red and says why.
- **Max** next to an amount fills in the most you can use: your share to sell or swap, your
  wallet less the network fee to lock. In the order book, pick a price: the limit order form
  switches side and fills in that price.
- Stop the exchange (Ctrl+C in its window) with a wallet page open: a banner says the
  exchange isn't answering and trading waits. Start it again: the banner goes away.
- During an exit (Request exit, then a block), the guide's last step says the exit window is
  running; once it's over, its button takes your coins.
- `go run . start` without `-name` opens the wallet you used last, and says which.
- Switch Windows to dark mode (Settings, Personalization, Colors): all three pages follow.

## What a failure looks like

- Automatic tests: a `--- FAIL` line with the expected and actual values.
- Pages: a red message in the bottom-right corner and a red "Refused" entry in the Activity
  log. Some refusals are expected, for example **Claim alone** before the exit window ends.
