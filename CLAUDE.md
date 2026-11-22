# Castlekeep: notes for Claude

Castlekeep is a prototype of self-custodial exchange settlement: the user locks coins in a
vault that needs both the user's and the exchange's signatures, trades against it, and can
always get out alone after an exit window. **Local test chains only.** Never use real coins,
mainnet, or real money, and never describe the code as secure or audited (it isn't).

The user runs Windows with Command Prompt and prefers plain, step-by-step explanations.

## Layout

| Path | What it is |
| --- | --- |
| `console/` | Go web app (module `castlekeep-console`) for Bitcoin, Litecoin and Ethereum. Main work happens here. |
| `console/engine.go` | Shared types (`Engine`, `ActionReq`, `Snapshot`, `VaultView`, event log). Amounts are always in 1e-8 coin units ("sats"; on ETH 1 unit = 1e10 wei). |
| `console/utxo.go` | `UTXOEngine`: runs btcd/ltcd in regtest, handles actions, the exchange's and user's watchtowers, snapshots. |
| `console/utxo_vault.go` | Taproot scripts and transaction building for the BTC/LTC vault, states and revocation outputs. |
| `console/eth.go` | `EthEngine`: JSON-RPC to Hardhat/Anvil, deploys `contract/CastleVault.bin`, signs states. |
| `console/contract/` | `CastleVault.sol` plus compiled `.bin`/`.abi.json` (the `.bin` is embedded with `go:embed`). |
| `console/web/index.html` | Single-page UI (embedded). Talks to `/api/chains`, `/api/state?chain=`, `/api/action`. |
| `console/scenario_test.go` | Scenario tests that drive each engine through `Do(ActionReq)`. |
| `prototype/` | Original Bitcoin CLI demo (separate Go module). |
| `solana/` | Untested Anchor design. Not compiled. |

## Design in one screen

**Bitcoin / Litecoin (`utxo_vault.go`)**

```
Vault V  (Taproot, NUMS internal key, so no key path)
  └─ <user> CHECKSIGVERIFY <exch> CHECKSIG
Exit E   (reached by the exit request V -> E, pre-signed by both when the vault opens)
  ├─ <user> CHECKSIGVERIFY <exch> CHECKSIG          exchange publishes a state payout
  └─ <delay> CSV DROP <user> CHECKSIG                 user alone after 144 (BTC) / 576 (LTC) blocks
State n payout from E: user share -> user wallet; exchange share -> revocable output R_n
R_n      (Taproot, NUMS internal key)
  ├─ <D2> CSV DROP <exch> CHECKSIG                    exchange claims after 6 (BTC) / 24 (LTC) blocks
  └─ SHA256 <H(r_n)> EQUALVERIFY <user> CHECKSIG      user sweeps with revocation secret r_n
```

- The user signs each state's payout **at order placement**, so a trade can't be undone.
- The exchange picks a random secret r_n per state and gives H(r_n) when the state is
  signed. When state n+1 is signed, it hands the user r_n (state n is now "revoked").
- If the exchange publishes a revoked state, the user's watchtower sweeps R_n with r_n
  before D2 expires ("penalty sweep"), so the user ends up with the whole vault.
- Cooperative settlement spends V directly with both signatures (no revocation needed).

**Ethereum (`eth.go`, `CastleVault.sol`)**: both sides sign every state (nonce = state
index). During the 24 h exit window `submitState` only accepts a higher nonce; `finalize`
pays the best submitted state, or refunds each side's deposit if none was submitted. The
exchange can `fund` "buying capacity" (fee 0.2% from the user's share). The user's
watchtower submits the latest state when an older one shows up, or when the user bought
more than they deposited.

The UTXO engine and the ETH engine each run both parties' logic in-process: "watchtower"
functions react after each `mine` action, and the exchange `mode` is honest / offline /
malicious.

## Running on Windows (Command Prompt)

- Go is installed. `btcd.exe` and `ltcd.exe` are in `%USERPROFILE%\go\bin` (the default
  path the console looks in).
- **Do not rebuild ltcd.** `go install` for ltcd needs cgo and a C compiler, which isn't
  installed; the existing `ltcd.exe` was prebuilt.
- Hardhat runs in its own window at `http://127.0.0.1:8545` (started from
  `%USERPROFILE%\castlekeep-eth` with `npx hardhat node`). Leave it running.
- Start the console:
  ```
  cd console
  go run . -eth-rpc http://127.0.0.1:8545
  ```
  It opens `http://127.0.0.1:8080`. Each chain mines ~500 blocks / deploys the contract
  first (about a minute). Ctrl+C stops it and its btcd/ltcd. Flags: `-port`, `-no-browser`.
- Run the scenario tests (needs Hardhat running for the Ethereum ones; they are skipped
  with a message if it isn't reachable):
  ```
  cd console
  go test -v -timeout 30m
  ```
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
- `Do` errors wrapped with `userErr` are shown to the user; refused transactions are
  *not* Go errors: they appear as `TxInfo.Status == "rejected"` plus a "bad" event.

## Rules for changes

- Keep every scenario test passing on all three chains; run `go test` before and after.
- Explain the plan before large changes. Don't push to GitHub or delete files without asking.
- Keep comments and UI text plain and accurate. Don't claim anything is secure or audited.
