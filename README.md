[README.md](https://github.com/user-attachments/files/35209137/README.md)
# Castlekeep

**Trade bitcoin on an exchange without the exchange ever holding your coins.**

Castlekeep is a prototype for self-custodial exchange settlement. Instead of depositing
BTC into an exchange's wallet, you lock it into a Bitcoin vault that needs **both your key
and the exchange's key** to move. You trade against that locked balance as normal. If the
exchange disappears or turns hostile, you can always get your coins back on your own after
a 144-block (about 24-hour) window.

![Castlekeep console](console/preview.png)

> **Test network only.** Everything here runs on a private Bitcoin test chain with
> worthless test coins. The code has not been audited. Never use it with real bitcoin.

## Why

When you deposit coins on a normal exchange, the exchange takes custody of them. If it
fails, is hacked or misuses customer funds, as FTX did in 2022, your coins can be lost or
frozen for years. Big institutions avoid this with "off-exchange settlement" through
third-party custodians, but nothing like that exists for ordinary users.

Castlekeep explores a version where the user is their own custodian, and Bitcoin's own
rules, not a company's promise, stop the exchange from taking the coins.

## How it works

```
Vault output   (Taproot, no key-path spend)
  └─ <your key> CHECKSIGVERIFY <exchange key> CHECKSIG            both must sign

Exit output    (reached by an exit request both sides pre-sign when the vault opens)
  ├─ <your key> CHECKSIGVERIFY <exchange key> CHECKSIG            exchange publishes a state you signed
  └─ 144 CHECKSEQUENCEVERIFY DROP <your key> CHECKSIG             you alone, 144 blocks later
```

1. **Lock.** You move BTC into the 2-of-2 vault. You and the exchange both pre-sign an
   exit request, so you can always start the exit clock without the exchange.
2. **Trade.** Each order creates a new split of the vault. You sign that split *before* the
   order is sent, so a filled trade can't be undone.
3. **Settle.** When you're done, both sides sign and your share returns straight away.
4. **Exit.** If the exchange won't cooperate, you broadcast the exit request. The exchange
   has 144 blocks to publish your latest signed split. After that, you can claim alone.

| What someone tries | What stops it |
| --- | --- |
| The exchange takes the coins | The vault script needs your signature |
| The exchange vanishes | Exit request, then claim alone after 144 blocks |
| You sell, the price rises, you try to exit with the coins | The exchange publishes the sale you already signed |

## What's in this repository

| Folder | What it is |
| --- | --- |
| [`console/`](console/) | A clickable web app. Lock, trade, settle, exit, mine blocks and switch the exchange between honest, offline and malicious, all in your browser. **Start here.** |
| [`prototype/`](prototype/) | A command-line version that runs four scenarios automatically and checks every result. |

Both start their own Bitcoin node (btcd) in regtest mode, so every accept or reject comes
from real Bitcoin consensus rules.

## Quick start

You need [Go](https://go.dev/dl/) 1.21 or newer.

```
go install github.com/btcsuite/btcd@v0.24.2
```

Then download this repository (**Code → Download ZIP** on GitHub), unzip it, and run:

```
cd console
go mod tidy
go run .
```

Your browser opens the console. See [`console/README.md`](console/README.md) for a full guide.

## Status and known limits

This is a proof of concept, not a product.

- **No revocation yet.** The exchange holds every state you've signed, so a malicious
  exchange can publish an *older* one. Version 2 will add Lightning-style revocation keys
  so that publishing an outdated state forfeits the whole vault.
- **Spot trading only.** There's no leverage, because losses must never exceed what's locked.
- **Only the BTC side is self-custodied.** Cash from sales sits with the exchange as normal.
- **Flat fees**, no fee bumping, no real order book.

## Roadmap

- [ ] Revocation penalties for outdated states
- [ ] Run on signet, Bitcoin's public test network
- [ ] MuSig2 key-path settlements (cheaper and more private)
- [ ] Real fee estimation and fee bumping
- [ ] Lightning-channel version for near-instant deposits and withdrawals

## Feedback

If you can see an attack or a flaw in the design, please open an issue. That's the most
useful thing anyone can contribute right now.
