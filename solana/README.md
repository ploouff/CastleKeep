# Castlekeep on Solana (design sketch)

> **Not compiled or tested.** This folder is a design and a first draft of the program.
> Solana's build tools couldn't run in the environment where it was written, and on
> Windows they need WSL. Treat every line as unreviewed until it has tests.

## How Castlekeep maps onto Solana

Solana has no Bitcoin-style scripts. Like Ethereum, it runs programs (Solana's word for
smart contracts), written in Rust, usually with the Anchor framework. The design follows
the Ethereum version, because that version already closes the "old state" gap.

| Castlekeep idea | Bitcoin / Litecoin | Ethereum | Solana (this sketch) |
| --- | --- | --- | --- |
| Where the coins sit | Taproot output | `CastleVault` contract balance | A program-owned account (PDA) per vault |
| Both must sign | 2-of-2 script | Two ECDSA signatures checked with `ecrecover` | Both parties co-sign the `settle` transaction |
| Signed trade states | User pre-signs | Both sign, newest nonce wins | Both sign (Ed25519), newest nonce wins |
| Checking state signatures | Script | `ecrecover` | Solana's built-in Ed25519 program, checked via the Instructions sysvar |
| Exit window | 144 / 576 blocks | 24 hours (block timestamps) | 24 hours (`Clock` sysvar) |

### Instructions

| Instruction | Who | What it does |
| --- | --- | --- |
| `open(vault_id, exchange, exit_delay, amount)` | User | Creates the vault account and moves SOL into it |
| `settle(user_amount)` | User **and** exchange co-sign | Pays out immediately |
| `request_exit()` | User or exchange | Starts the window |
| `submit_state(nonce, user_amount)` | Anyone (watchtowers) | Records a both-signed state; only a higher nonce replaces an older one |
| `finalize()` | Anyone | After the window, pays the newest state, or everything to the user if none |

### The tricky part: verifying signatures

A Solana program can't cheaply check a signature over arbitrary bytes itself. Instead,
the transaction that calls `submit_state` also includes two calls to Solana's built-in
Ed25519 program, one for each signer. The runtime rejects the whole transaction if
either signature is invalid. `submit_state` then reads the transaction's other
instructions (through the Instructions sysvar) to confirm:

- those calls checked **the vault's user key and exchange key**, and
- the signed bytes are **exactly this state's message** (vault, id, nonce, user amount), and
- the offsets point inside the same instruction, so nobody can point them at other data.

Getting that last check wrong is a known source of real Solana exploits, which is why
this needs careful tests and outside review before anything else.

## Trying it (Windows)

1. **Install WSL** (Windows Subsystem for Linux): open PowerShell as administrator and
   run `wsl --install`, then restart. The Solana tools run inside it.
2. **Inside WSL, install** Rust, the Solana CLI and Anchor by following the official
   installation guide on Solana's documentation site.
3. **Create an Anchor project**, replace its `programs/<name>/src/lib.rs` with the file in
   this folder, and update `declare_id!` with your program ID (`anchor keys list`).
4. **Build and test** with `anchor build` and `anchor test`, which runs a local validator.
   Write tests for every scenario the console already covers: normal trade, exchange
   disappears, undo a sale, malicious old state, attempted theft. Add one more: a
   transaction whose Ed25519 offsets point at the wrong data must be refused.

Connecting Solana to the console would then need a Go Solana engine like the Ethereum
one: a local validator, keypairs for both parties, and transaction building that
includes the Ed25519 instructions.

## Other coins worth considering

| Coin or network | Fit | Notes |
| --- | --- | --- |
| **Polygon, Arbitrum, Base, BNB Chain, Avalanche C-Chain** | Excellent | These run the Ethereum Virtual Machine, so `CastleVault.sol` deploys unchanged. Only the RPC URL and chain ID differ. The cheapest way to add five networks. |
| **Stablecoins (USDC, USDT) on those networks** | Good | Extend the contract to hold an ERC-20 token. This could make the cash side of a trade self-custodied too. Swapping coin for stablecoin atomically is the hard part. |
| **Bitcoin Cash** | Possible | Supports timelocks and multisig but not Taproot, so the vault would use older P2SH scripts. |
| **Dogecoin** | Harder | Lacks SegWit and Taproot. Check which timelock opcodes it supports before designing for it. |
| **Cardano** | Possible, different model | Smart contracts are validator scripts (for example, written in Aiken) on a UTXO model. |
| **XRP** | Awkward | No general smart contracts. Would need payment channels, escrow and multisig accounts combined. |
| **Monero** | Not practical | Its privacy design doesn't support the scripts this needs. |
