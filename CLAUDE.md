# Castlekeep: notes for Claude

Castlekeep is a prototype of self-custodial exchange settlement: the user locks coins in a
vault that needs both the user's and the exchange's signatures, trades against it, and can
always get out alone after an exit window. **Test networks only: local test chains, Bitcoin's public signet, or the public EVM test
networks** Sepolia, Polygon Amoy, Base Sepolia, BNB Chain testnet and Avalanche Fuji (coins
with no value; the user approved signet on 2026-11-23 and the EVM test networks on 2026-11-25).
Never use real coins, mainnet, or real money, and never describe the code as secure or
audited (it isn't).

The user runs Windows with Command Prompt and prefers plain, step-by-step explanations.

## Layout

| Path | What it is |
| --- | --- |
| `console/` | Go web app (module `castlekeep-console`) for Bitcoin, Litecoin and the EVM chains (Ethereum, Polygon, Base, BNB Chain, Avalanche). Main work happens here. |
| `console/engine.go` | Shared types (`Engine`, `ActionReq`, `Snapshot`, `VaultView`, event log). Amounts are always in 1e-8 coin units ("sats"; on ETH 1 unit = 1e10 wei). |
| `console/account.go` | `Account` (one cash balance shared by every engine via `UseAccount`), `accountView` for `/api/account`, and `swap`. |
| `console/utxo.go` | `UTXOEngine`: runs btcd/ltcd in regtest, handles actions, the exchange's and user's watchtowers, snapshots. |
| `console/utxo_vault.go` | Taproot scripts and transaction building for the BTC/LTC vault, states and revocation outputs. |
| `console/eth.go` | `EthEngine`: JSON-RPC to Hardhat/Anvil, deploys `contract/CastleVault.bin`, signs states. |
| `console/contract/` | `CastleVault.sol` plus compiled `.bin`/`.abi.json` (the `.bin` is embedded with `go:embed`). |
| `console/web/index.html` | Single-page UI (embedded). Talks to `/api/chains`, `/api/state?chain=`, `/api/account`, `/api/action`. For newcomers: a "Start here" panel (`startSteps`/`renderStart`: five steps ticked off from the snapshot, buttons via `data-step`), a glossary (`glossary(c)`), and a **Test tools** switch (`body.tools`; anything with class `tt` is hidden until it's on: exchange modes, price setting, steal, cheat, recipes). Remembered in localStorage (`ck-tools`, `ck-start-hidden`). Mine buttons stay visible. |
| `console/scenario_test.go` | Scenario tests that drive each engine through `Do(ActionReq)`. |
| `console/swap_test.go` | Swap and shared-account tests (need all three chains). |
| `console/split_core.go`, `split_net.go` | Two-party BTC/LTC vault built from public keys only, chain access, and the messages between the separate programs. |
| `console/split_exchange.go` | Exchange server (`Exchange`, HTTP `/v1/...`): exchange keys, wallet, prices, cash, its own watchtower (`Tick`). |
| `console/split_wallet.go` | Wallet client (`Wallet`): user keys and secrets, checks every exchange signature and amount, its own watchtower (`Tick`). |
| `console/split_test.go` | Tests running the exchange as a real HTTP server against the wallet on the same test node. |
| `console/split_apps.go` | The `chains`, `exchange`, `wallet` subcommands; `walletEngine` adapts the wallet to the page's `Engine` API. |
| `console/split_eth.go`, `split_apps_eth.go`, `split_eth_test.go` | Ethereum in the separate programs, and its tests. |
| `console/split_swap.go`, `split_swap_test.go` | Swaps across the three chains in the separate programs. |
| `console/split_tower.go`, `split_tower_test.go`, `web/tower.html` | Separate watchtower: `go run . watchtower` (:9400, page `web/tower.html`), wallet `-watchtower URL` (saved as `wSave.Tower`; `off` calls `/v1/tower/forget`). Holds no user keys: the wallet sends `TowerUpdate` (BTC/LTC `TowerBreach` = signed penalty sweep per revoked state, `TowerGen` = signed user copy of each generation's newest state if it pays more than the deposit; EVM newest state + both sigs) and the tower's only key is an EVM fee key. Rules: `utxoAgree` calls `towerBefore` (tower gets state n+1 before u_n is revealed; unreachable = trade refused; `Swap` checks first), updates only move forward (`mergeTowerUTXO`), token per wallet (sha256 stored). `Tower.Tick` checks BTC/LTC once per new block. Wallet `watch` recognises the tower's sweep by txid (`penaltySweep`). |
| `console/split_shared.go`, `split_shared_test.go`, `SERVER.md` | Shared mode (`-shared` on chains, exchange, watchtower) for a server friends' wallets reach over HTTPS (Caddy in front; programs stay on 127.0.0.1; pages use relative paths so they work under `/exchange/`, `/tower/`). Exchange: every endpoint goes through `authed` (wallet signs with `Wallet.acctKey` via `w.call`/`signedHeaders`: account, key, time, nonce, schnorr sig over `requestDigest`; first key per account kept in `x.acctKeys`; replays (`x.seen`) and requests >5 min off refused; `authorize` type-switches every request type to check the account/vault is the signer's, unknown types refused). `/v1/admin`, `/v1/state`, `/v1/tick` and the tower's `/v1/tower/view` need `X-Castlekeep-Admin` = admin token (`adminOnly`; saved in the files; page passes `?token=`). EVM: `EthOpenReq.Claim` = owner's EVM sig on `claimDigest`, required when shared. Chains: `/rpc/<id>` gateway (`rpcGateway`, allow-list `gatewayMethods`), `ChainStatus.Gateway`/`Shared` make other programs use it (`gatewayURL`, `dialChain` accepts http(s) URLs); `/faucet`, `/mine`, `/contract` are `localOnly` (loopback and no X-Forwarded-For). `-shared` refuses regtest. Without `-shared` nothing changes. |
| `console/split_book.go`, `split_book_test.go` | Order book (separate programs): limit orders, matching (best price, then oldest), reservations, fills each wallet checks against its own order before signing. Page: the "Order book" panel in `index.html` (only shown when the snapshot has `book`), actions `place`/`cancel` via `bookAction`; several wallets with `go run . wallet -name bob -port 9301`. |
| `console/split_save.go`, `split_save_test.go` | Saving: the exchange (`exchange.json`) and each wallet (`wallet-<name>.json`) save only inputs (keys, amounts, signatures, secrets, some tx hex) after every request/tick and rebuild scripts and txs on `Load`. Half-done steps (`pend`) are dropped. Chains not running this time stay in the file unchanged (`x.kept`/`w.kept`, `notRunning`). Files are refused if made on another chain (`chainConn.tag` = hash of block 1). Kept chains: `keepChain`, `keepBlocks`/`replayBlocks`, `miner.json`, `eth.json` (kept contract). Flags `-data`, `-fresh` (renames old data, never deletes). Default folder `%USERPROFILE%\castlekeep-data`. |
| `console/split_signet.go` | Bitcoin signet: `go run . chains -network signet` (Bitcoin only; btcd with `--signet --addrindex`, data in `castlekeep-data\chains-signet`). No mining or faucet; exchange and wallets find coins sent to their addresses with `searchrawtransactions` (`coinsTo`, every 15 s, or the page's "Check for coins" button). |
| `console/evm_sign.go` | Local EIP-1559 (type 2) signing with RLP (`sendSigned`), used by `sendRaw` when `EthEngine.signLocal` is on (`EthEndpoint.SignLocally`) and `signer` has the key (set in `AttachEVM` for exchange and wallet). `TestSplitSignedLocally` runs the separate programs on Hardhat with impersonation off. Used on the public EVM test networks (`evm_public.go`); also signs contract creation (empty `to`), tip = max(node tip, gas price - base fee, 1 gwei if 0). |
| `console/evm_public.go` | Public EVM test networks: `go run . chains -network testnets` (signet + Sepolia, Amoy, Base Sepolia, BNB Chain testnet, Fuji; `-coins`, `-rpc id=url`, `-exit-window 1m..24h`, which also applies to local EVM chains). `checkTestChain` asks `eth_chainId` and refuses main networks (`mainNets`, `evmChain.MainID`) and wrong chains. `publicEVM` in the chains program runs no node; `/contract` keeps the address the exchange reports after `codeOK` (on-chain code is part of `CastleVault.bin`), saved in `castlekeep-data/chains-signet/evm-public-<id>.json`. Exchange `evmSetup` deploys once its address has coins, then `announce`s; wallet `evmSetup` looks the address up and checks the code (`we.checked`) before `EthOpen`. `rpcCache` keeps reads 4 s (`cacheFor`; tests set `cacheFor`, `setupEvery`, `lookEvery` to 0) and final receipts. `rpcClient` has a 30 s timeout. Public node URLs and explorers are in `evmChains` (`RPC`, `Explorer`). Page: `ChainInfo.Public`/`Explorer`, `isPublic(c)` in `index.html`. Tests: `evm_public_test.go` (Hardhat with the Sepolia chain ID, no impersonation). |
| `console/musig.go`, `musig_test.go` | MuSig2 key path for BTC/LTC vaults (settle and splice): combined key, `musigNonce` (single use), `keySpend` (`sign`, `verifyPartial`, `combine`), `keyPathNote` for the page. Uses `btcec/v2/schnorr/musig2` from the pinned btcec v2.3.4 (no new dependency). See "Design in one screen". |
| `console/seal.go` | Password-protected save files: `-password` / `-remove-password` on exchange and wallet (`startSealed`, `passwordFor`), scrypt N=32768 + AES-256-GCM, sealed JSON `{"castlekeep":"sealed",...}`; `x.seal`/`w.seal` used by `save()` and `Load` (`readSaved`). A plain file is copied to `.unprotected-<time>` before sealing. `CASTLEKEEP_PASSWORD` for scripts/tests. Password prompt uses `golang.org/x/crypto/ssh/terminal` (already pinned). |
| `console/fees.go` | Separate programs, BTC/LTC: fee estimate (`feeRate`: btcd `estimatefee`, else 2 sat/vB), "Speed up" (`SpeedUp`, `bump`: child pays for parent from the wallet's output of a waiting tx, plus confirmed coins if needed), `bumpable` (skips a tx that already has a mempool child), watchtower `autoBump` after 2 blocks during an exit. New vaults have `vaultDef.anchors`: the exit request gets a 330-unit output for each side (`OpenKeysResp.Anchors`, saved as `defRec.Anchors`; old vaults rebuild without). |
| `console/evm.go` | The EVM chains (`evmChains`: eth, pol, base, bnb, avax) with chain IDs of their public TEST networks, test prices, icon colour/badge; `evmConfig`, `startHardhat` (writes a temp config with `chainId`, `allowBlocksWithSameTimestamp`, optional `initialDate`), `chainOrder`. All EVM chains use `EthEngine` / `xEth` / `wEth` keyed by chain ID (`x.evm`, `w.evm`; empty `Chain` in a message means eth). |
| `console/evm_keep.go` | Kept EVM chains for the separate programs: a recorder in front of each Hardhat node writes state-changing RPCs to `chains/evm-<id>.jsonl` (first line: genesis time) and replays them on start with the same block timestamps, so hashes match (`TestKeptEVMChainRestarts`). Recorders on ports chains+10.. (9110 eth, 9111 pol, 9112 base, 9113 bnb, 9114 avax). |
| `TESTING.md` | How to run the tests and try each feature by hand. |
| `prototype/` | Original Bitcoin CLI demo (separate Go module). |
| `solana/` | Untested Anchor design. Not compiled. |

## Design in one screen

**Bitcoin / Litecoin (`utxo_vault.go`)**

```
Vault V  (Taproot, NUMS internal key, so no key path)
  └─ <user> CHECKSIGVERIFY <exch> CHECKSIG
Exit E   (reached by the exit request V -> E, pre-signed by both when the vault opens)
  ├─ <user> CHECKSIGVERIFY <exch> CHECKSIG                    either side publishes a state payout
  └─ <delay> CSV DROP <user> CHECKSIGVERIFY <exch> CHECKSIG   pre-signed pay-back of deposits after
                                                               144 (BTC) / 576 (LTC) blocks
State n has two payouts from E, both signed by both sides:
  exchange's copy: user share -> user wallet;   exchange share -> R_n
  user's copy:     exchange share -> exchange;  user share     -> U_n
R_n  ├─ <D2> CSV DROP <exch> CHECKSIG                 exchange claims after 6 (BTC) / 24 (LTC) blocks
     └─ SHA256 <H(r_n)> EQUALVERIFY <user> CHECKSIG   user sweeps with the exchange's secret r_n
U_n  ├─ <D2> CSV DROP <user> CHECKSIG                 user claims after D2
     └─ SHA256 <H(u_n)> EQUALVERIFY <exch> CHECKSIG   exchange sweeps with the user's secret u_n
```

- The user signs the exchange's copy **at order placement**, so a trade can't be undone.
  The exchange signs the user's copy on the fill.
- Each side picks a random secret per state and gives the other its hash. When state n+1
  is signed, both hand over their secret for state n (it is now "revoked"; `revealed`).
- If the exchange publishes a revoked state, the user's watchtower sweeps R_n with r_n
  ("penalty sweep", `sweepKind` "penalty"). If the user publishes one (the "cheat"
  action), the exchange's watchtower sweeps U_n with u_n ("exchange-penalty").
- Claim alone = the pre-signed pay-back of deposits (`refundTx`). With no exchange deposit
  that is the whole vault to the user, as before. The user's watchtower publishes the
  latest state during the window if it pays the user more than their deposit.
- Buying capacity ("capacity" action, `addCapacity` + `spliceTx`): one tx signed by both
  spends V plus exchange wallet coins into a bigger V (same scripts), re-signs exit and
  pay-back (`applySplice`), bumps `v.gen` (states from older gens can't be published) and
  signs a new state (fee 0.2% from the user's share). The exchange wallet starts with 20
  coins (`exchangeStock`); the snapshot reports the change from that.
- Cooperative settlement spends V directly with both signatures (no revocation needed).
- MuSig2 (`musig.go`): new vaults have V's internal key = MuSig2(user, exch) (sorted, x-only
  keys as even-y points; `evenKey` negates a private key whose point is odd) instead of the
  NUMS key. Settle and splice spend V through the key path (one 64-byte sig); exit request,
  pay-back and state payouts stay on the scripts, and E keeps the NUMS key. `vaultDef.musig` /
  `setMuSig2`, `OpenKeysResp.MuSig2`, `defRec.MuSig2`; old vaults rebuild without it.
  Messages: `SettleReq.UserNonce` -> `SettleResp.ExchNonce/ExchPartial`;
  `CapacityReq.UserNonce` -> `CapacityOffer.SpliceNonce/SplicePartial` ->
  `CapacityCommitReq.SplicePartial`. `musigNonce` wipes its secret after one `sign`; secret
  nonces are never saved (pending splices are dropped on load). `newVaultsMuSig2` (tests)
  makes old-style vaults. The one-program console signs with both keys (`musigBoth`).

**Separate programs (work in progress)**: `split_*.go`. Message order: opening
gets the exchange's signatures on exit, pay-back and state #0 before the deposit is sent;
an order gets the exchange's signature on the user's copy first, then the user signs and
reveals u_{n-1}, then the exchange reveals r_{n-1}; a splice only swaps the old state's
secrets after it confirms (`/v1/capacity/finish`). Run as `go run . chains` (nodes, faucet,
mine on :9100), `go run . exchange` (:9200, admin page `web/exchange.html`) and
`go run . wallet` (:9300, the usual page via `walletEngine` in `split_apps.go`), plus optionally `go run . watchtower` (:9400, `split_tower.go`). ETH is in `split_eth.go` (exchange proposes a
split, wallet signs, exchange signs; settle signature only for the latest state, after
which the exchange stops trading on that vault; same rule on BTC/LTC). Swaps: `split_swap.go` (`/v1/swap` prepares
both sides, signs the buying side first and takes its price from cash; the wallet checks
prices and both splits, completes the buying side, then the selling side, which credits
the sale). No steal demo there. All three programs keep their data between runs (see
`split_save.go`); BTC/LTC RPC is on fixed ports (chains port + 1, + 2) so the others can
reconnect after the chains restart. Bitcoin signet: `split_signet.go`.

**Ethereum (`eth.go`, `CastleVault.sol`)**: both sides sign every state (nonce = state
index). Settling (`settle`) needs both signatures on a separate `settleHash`
message, signed only at settlement time for the latest state, so old trade signatures
can't close the vault early. After changing the `.sol`, recompile with the solc that
came with Hardhat (no download needed), from the `console` folder:
`node contract\compile.js %USERPROFILE%\castlekeep-eth\node_modules\solc contract`
(solc 0.8.26, optimizer 200 runs, evm cancun; writes `CastleVault.bin` and `.abi.json`).
During the 24 h exit window `submitState` only accepts a higher nonce; `finalize`
pays the best submitted state, or refunds each side's deposit if none was submitted. The
exchange can `fund` "buying capacity" (fee 0.2% from the user's share). The user's
watchtower submits the latest state when an older one shows up, or when the user bought
more than they deposited.

**One account, swaps (`account.go`)**: all engines share one `*Account` for cash (tests use
`testAcct`). `/api/action` with `action: "swap"`, `chain` = coin sold, `to` = coin bought,
`sats` = amount sold, calls `swap()`. It locks both engines (sorted by chain ID), checks
both legs with `canTrade`, then calls `recordTrade` on the buying leg first and the selling
leg second. Orders use the same `canTrade`/`recordTrade` plus cash. A swap is two vault
states, not one transaction and not an atomic swap.

The UTXO engine and the ETH engine each run both parties' logic in-process: "watchtower"
functions react after each `mine` action, and the exchange `mode` is honest / offline /
malicious.

## Running on Windows (Command Prompt)

- Go is installed. `btcd.exe` and `ltcd.exe` are in `%USERPROFILE%\go\bin` (the default
  path the console looks in).
- **Do not rebuild ltcd.** `go install` for ltcd needs cgo and a C compiler, which isn't
  installed; the existing `ltcd.exe` was prebuilt.
- Hardhat is installed in `%USERPROFILE%\castlekeep-eth`. The console, the chains program and
  the tests start their own Hardhat node per EVM chain from there (`-hardhat` to change;
  `node ...\hardhat\internal\cli\bootstrap.js node --config <temp>`). A Hardhat window at 8545
  is only used with `-eth-rpc`, or by the Ethereum tests when one is already running.
- Start the console:
  ```
  cd console
  go run .
  ```
  It opens `http://127.0.0.1:8080` with all seven chains (`-chains btc,ltc,eth` for fewer).
  Each chain mines ~500 blocks / deploys the contract first (about a minute). Ctrl+C stops it
  and its nodes. Flags: `-port`, `-no-browser`, `-eth-rpc`, `-hardhat`, `-chains`.
- Run the scenario tests (the EVM ones start their own Hardhat nodes; skipped with a message
  if Hardhat isn't installed):
  ```
  cd console
  go test -v -timeout 30m
  ```
  The swap tests (`TestSwap`) need btcd, ltcd and the Ethereum node together.
  Set `CASTLEKEEP_ETH_RPC` to use another Ethereum node URL. Don't run the console and the
  tests against the same Hardhat node at the same time: both mine blocks and move its clock.

## Shell gotchas (from PowerShell)

- PowerShell's execution policy is Restricted, so `.ps1` shims fail. Use `npm.cmd` /
  `npx.cmd`. Don't change the execution policy.
- npm is v11, which blocks package install scripts unless allowed with `--allow-scripts`.
- Node v26 is installed.

## Code gotchas

- **Dependency versions matter.** `btcec/v2 v2.3.4`'s `ecdsa.SignCompact` returns a single
  value, which broke the build once. `eth.go` therefore signs with
  `github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa`. Check signatures of library calls
  against the versions pinned in `go.mod` before using them.
- btcd regtest needs ~500 blocks for Taproot; `CheckForks` is only on for btcd (ltcd can't
  report soft-fork status).
- LTC uses `chaincfg.RegressionNetParams` with HRP `rltc` for addresses only.
- The ETH engine turns automining off on the node (`evm_setAutomine false`), so nothing
  confirms until something calls `evm_mine`. Clock skips (`evm_increaseTime`) are permanent
  on that node.
- Fees are flat (`txFee = 1000` base units) on BTC/LTC.
- **btcd and ltcd delete a regtest chain every time they start** (`removeRegressionDB`).
  Kept chains therefore stay on regtest and replay `blocks.txt` with `submitblock` on start.
  Don't switch them to simnet: ltcd's simnet stops mining at block 100 with no error.
- btcd's `getblock` wants verbosity `0` (a number) for raw hex; `keepBlocks` falls back to `false`.
- rpcclient retries for ~20 s against a node that's gone; `UTXOEngine.Stop` sets `e.node = nil`
  so a second Stop doesn't hang.
- Windows Defender has flagged a `go build` copy of the console (`ck.exe`) as "potentially unwanted"
  and stopped it mid-run (the signet node kept running on its own). Use `go run .`, and stop a
  leftover node with its `stop` RPC. Don't change Defender settings.
- `Do` errors wrapped with `userErr` are shown to the user; refused transactions are
  *not* Go errors: they appear as `TxInfo.Status == "rejected"` plus a "bad" event.

## Rules for changes

- Keep every scenario test passing on all three chains; run `go test` before and after.
- Explain the plan before large changes. Don't push to GitHub or delete files without asking.
- Keep comments and UI text plain and accurate. Don't claim anything is secure or audited.
