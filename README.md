# Castlekeep prototype

A working proof of concept for **Castlekeep**: trade bitcoin on an exchange without
the exchange ever holding your coins. It runs on a private Bitcoin **regtest** chain
(test coins only), using a real Bitcoin node (btcd) to enforce every rule.

> **Test network only.** Nothing here has been audited. Never use it with real bitcoin.

## What it proves

| Scenario | What happens | Enforced by |
| --- | --- | --- |
| A. Happy path | User locks 1 BTC, sells 0.6 BTC, both sign, coins return straight away | 2-of-2 Taproot script |
| B. Exchange disappears | User broadcasts the pre-signed exit request; a claim at 50 blocks is rejected; at 144 blocks the user claims alone | `OP_CHECKSEQUENCEVERIFY` (BIP 68/112) |
| C. Reversal attack | User sells at $65k, price rises, user tries to exit with the coins; the exchange publishes the state the user pre-signed, so the buyer still gets the BTC | Sign-at-order-placement + response window |
| D. Exchange theft | Exchange signs a spend paying itself everything; the node rejects it | 2-of-2 Taproot script |

Sample output is in `sample-output.txt`.

## How the vault is built

```
Vault output V   (Taproot, unspendable key path)
  â””â”€ leaf: <user> CHECKSIGVERIFY <exchange> CHECKSIG          â† both must sign

Exit output E    (reached only via the pre-signed exit request V â†’ E)
  â”œâ”€ leaf: <user> CHECKSIGVERIFY <exchange> CHECKSIG          â† exchange publishes the latest state
  â””â”€ leaf: <144> CHECKSEQUENCEVERIFY DROP <user> CHECKSIG     â† user alone, 144 blocks later
```

- **Exit request.** Signed by both parties when the vault is created, so the user can
  start the 144-block (about 24-hour) clock on their own at any time.
- **Sign at order placement.** When the user places an order, they sign the post-trade
  split of the exit output. If the user later tries to exit, the exchange adds its
  signature and publishes it inside the window.
- The internal key is the BIP 341 "nothing up my sleeve" point, so there is no hidden
  key-path spend.

## Run it

You need Go 1.21+ and btcd.

```bash
go install github.com/btcsuite/btcd@v0.24.2   # installs btcd into $(go env GOPATH)/bin
cd castlekeep-proto
go mod tidy
go run . -btcd "$(go env GOPATH)/bin/btcd"
```

The program starts its own regtest node in a temporary folder, mines 500 blocks to
activate CSV, SegWit and Taproot, runs the four scenarios, then shuts the node down.
Add `-keep` to keep the chain data afterwards.

## Not implemented yet (deliberately)

- **Revocation penalties.** The exchange holds every state the user has pre-signed, so
  in principle it could publish an *older* one. Short order expiry and Lightning-style
  revocation keys are the planned fix (version 2).
- **No real order book, prices or cash side.** Trades are simulated; only the BTC side is
  self-custodied.
- **No leverage check on-chain.** "Never owe more than the vault holds" is enforced by
  the exchange's risk engine, not by script.
- **Watchtower is simulated.** The scenario calls the exchange's response directly.
- **Script-path cooperative spends.** A production version would use a MuSig2 key-path
  spend for cheaper, more private settlements.
- **Fees are flat (1,000 sats).** No fee bumping for congested blocks.

