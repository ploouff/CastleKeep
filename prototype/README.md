# Castlekeep prototype (command line)

The first proof of concept for Castlekeep. It starts its own Bitcoin node in regtest mode,
then runs four scenarios automatically and checks each result against real Bitcoin
consensus rules. For a clickable version, see [`../console`](../console).

> **Test network only.** Nothing here has been audited. Never use it with real bitcoin.

## What it proves

| Scenario | What happens | Enforced by |
| --- | --- | --- |
| A. Happy path | Lock 1 BTC, sell 0.6, both sign, coins return straight away | 2-of-2 Taproot script |
| B. Exchange disappears | Exit request; a claim at 50 blocks is refused; at 144 blocks the user claims alone | `OP_CHECKSEQUENCEVERIFY` (BIP 68/112) |
| C. Reversal attack | User sells at $65k, price rises, user tries to exit with the coins; the exchange publishes the signed sale | Sign-at-order-placement plus the response window |
| D. Exchange theft | Exchange signs a spend paying itself everything; the node refuses it | 2-of-2 Taproot script |

Full output from a passing run is in [`sample-output.txt`](sample-output.txt).

## Run it

You need [Go](https://go.dev/dl/) 1.21 or newer, and btcd:

```
go install github.com/btcsuite/btcd@v0.24.2
```

Then, in this folder:

**Windows (Command Prompt)**
```
go mod tidy
go run . -btcd %USERPROFILE%\go\bin\btcd.exe
```

**Mac or Linux**
```
go mod tidy
go run . -btcd "$(go env GOPATH)/bin/btcd"
```

It mines 500 blocks to activate CSV, SegWit and Taproot, runs the four scenarios, prints a
tick or cross for each check, and ends with **"All checks passed."** Add `-keep` to keep
the chain data afterwards.

## How the vault is built

```
Vault output V   (Taproot, unspendable key path)
  └─ leaf: <user> CHECKSIGVERIFY <exchange> CHECKSIG          both must sign

Exit output E    (reached only via the pre-signed exit request V → E)
  ├─ leaf: <user> CHECKSIGVERIFY <exchange> CHECKSIG          exchange publishes the latest state
  └─ leaf: <144> CHECKSEQUENCEVERIFY DROP <user> CHECKSIG     user alone, 144 blocks later
```

The Taproot internal key is the BIP 341 "nothing up my sleeve" point, so there is no hidden
key-path spend.

## Not implemented yet

- Revocation penalties for outdated states
- A real order book, prices or cash side (trades are simulated)
- On-chain leverage checks (enforced by the exchange's risk engine instead)
- A real watchtower (the scenario calls the exchange's response directly)
- MuSig2 key-path settlements
- Fee estimation and fee bumping (fees are a flat 1,000 sats)
