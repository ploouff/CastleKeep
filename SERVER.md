# Running Castlekeep on a server, so friends can try it

**Test networks only** (Bitcoin signet and the public EVM test networks). The coins have no
value. The code has not been audited: don't put anything of value near it.

This puts the chains program, the exchange and the watchtower on a small rented server.
Your friends run only their **wallet**, on their own computer, so their keys never leave
it. Everything they send the exchange is signed by their wallet.

```
 friend's PC                        your server (Caddy in front, HTTPS)
 ┌────────────────┐   https   ┌──────────────────────────────────────────────┐
 │ wallet (keys)  │ ────────▶ │ /exchange → exchange          127.0.0.1:9200 │
 │ page on        │ ────────▶ │ /chains   → chains + gateway  127.0.0.1:9100 │
 │ 127.0.0.1:9300 │ ────────▶ │ /tower    → watchtower        127.0.0.1:9400 │
 └────────────────┘           │ signet node; the public EVM test networks    │
                              └──────────────────────────────────────────────┘
```

## What you need

- **A small rented server** (a "VPS") running **Ubuntu 24.04**: 2 GB of memory and 40 GB of
  disk or more (the signet chain is a few GB). Many companies rent these, usually for about
  $5 to $10 a month. You'll get an IP address and a password or a key to log in.
- **A web address for it**, so HTTPS works: either a domain you own (about $10 a year), or
  a free name from a dynamic-DNS service. Point it at the server's IP address (an "A
  record"). Below, `castlekeep.example.com` stands for your address.
- **About an hour**, plus a few hours for the signet download the first time.

## 1. Log in to the server

In Command Prompt on your PC (Windows has `ssh` built in):

```
ssh root@YOUR.SERVER.IP
```

Everything until step 8 is typed on the server.

## 2. Install the tools

```
apt update
apt install -y git golang-go caddy
```

If `apt` can't find `caddy`, follow the install steps for Debian/Ubuntu on caddyserver.com,
then carry on.

Make a separate user for Castlekeep (it doesn't need to run as root):

```
adduser --disabled-password --gecos "" castlekeep
```

## 3. Get Castlekeep and the Bitcoin node

```
su - castlekeep
git clone https://github.com/ploouff/CastleKeep.git
cd CastleKeep/console
go install github.com/btcsuite/btcd@v0.24.2
go build -o castlekeep .
exit
```

(`btcd` ends up in `/home/castlekeep/go/bin`, where Castlekeep looks for it. Litecoin isn't
used on the public test networks, so there's no `ltcd`.)

## 4. Run the three programs as services

Services start by themselves when the server starts, and restart if they stop. Create three
files. The quickest way is to paste each block below into the terminal: `cat` writes
everything up to the line `EOF` into the file.

```
cat > /etc/systemd/system/castlekeep-chains.service <<'EOF'
[Unit]
Description=Castlekeep test chains (shared)
After=network-online.target
Wants=network-online.target

[Service]
User=castlekeep
WorkingDirectory=/home/castlekeep/CastleKeep/console
ExecStart=/home/castlekeep/CastleKeep/console/castlekeep chains -network testnets -exit-window 1h -shared
Restart=on-failure

[Install]
WantedBy=multi-user.target
EOF
```

```
cat > /etc/systemd/system/castlekeep-exchange.service <<'EOF'
[Unit]
Description=Castlekeep exchange (shared)
After=castlekeep-chains.service

[Service]
User=castlekeep
WorkingDirectory=/home/castlekeep/CastleKeep/console
ExecStart=/home/castlekeep/CastleKeep/console/castlekeep exchange -shared -no-browser
Restart=on-failure

[Install]
WantedBy=multi-user.target
EOF
```

```
cat > /etc/systemd/system/castlekeep-watchtower.service <<'EOF'
[Unit]
Description=Castlekeep watchtower (shared)
After=castlekeep-chains.service

[Service]
User=castlekeep
WorkingDirectory=/home/castlekeep/CastleKeep/console
ExecStart=/home/castlekeep/CastleKeep/console/castlekeep watchtower -shared -no-browser
Restart=on-failure

[Install]
WantedBy=multi-user.target
EOF
```

Start them:

```
systemctl daemon-reload
systemctl enable --now castlekeep-chains castlekeep-exchange castlekeep-watchtower
```

The exchange and watchtower wait until the chains are ready. Watch the signet download with
`journalctl -u castlekeep-chains -f` (Ctrl+C stops watching, not the program).

`-exit-window 1h` makes the exit window on the EVM networks one hour, so your friends can try
an exit in an afternoon. Leave it out for the full 24 hours. On signet the window is always
144 blocks (about a day).

The exchange takes at most 100 accounts and the watchtower at most 100 wallets (see
"Limits" below). For a bigger group, add `-max-accounts 200` to the exchange's `ExecStart`
line and `-max-wallets 200` to the watchtower's.

## 5. HTTPS in front (Caddy)

The three programs only listen on the server itself. Caddy answers on the internet with
HTTPS (it gets the certificate by itself) and passes requests on. Replace the file
`/etc/caddy/Caddyfile` (use your own address in the first line):

```
cat > /etc/caddy/Caddyfile <<'EOF'
castlekeep.example.com {
	redir /exchange /exchange/
	handle_path /exchange/* {
		reverse_proxy 127.0.0.1:9200
	}
	redir /tower /tower/
	handle_path /tower/* {
		reverse_proxy 127.0.0.1:9400
	}
	handle_path /chains/* {
		reverse_proxy 127.0.0.1:9100
	}
	handle {
		respond "Castlekeep test server: test networks only." 200
	}
}
EOF
systemctl reload caddy
```

Open `https://castlekeep.example.com` in a browser: it should say "Castlekeep test server".

Optional, a firewall that only lets in SSH and the web:

```
ufw allow OpenSSH
ufw allow 80
ufw allow 443
ufw enable
```

## 6. Your admin page

The exchange prints the admin page's address with a secret token when it starts:

```
journalctl -u castlekeep-exchange | grep token
```

Open it as `https://castlekeep.example.com/exchange/?token=...` (the part after `?token=`
from that line). Keep the token to yourself: with it, anyone can switch the exchange to
offline or malicious and change prices. The watchtower's page works the same way
(`journalctl -u castlekeep-watchtower | grep token`, then `/tower/?token=...`).

## 7. Give the exchange and watchtower test coins

The exchange needs coins on each network: to put the contract on the EVM networks, and to
lock coins into vaults when someone buys. Its addresses are in its log:

```
journalctl -u castlekeep-exchange | grep -E "address|wallet at"
```

Send faucet coins to each one (search for "signet faucet", "Sepolia faucet", "Amoy faucet"
and so on). Within half a minute of the coins arriving on an EVM network, the exchange
puts the contract there. The watchtower needs only a little (0.01 is plenty) at its fee
address on each EVM network, to pay gas if it ever has to step in:
`journalctl -u castlekeep-watchtower | grep fee`.

## 8. What your friends do (on their own computer)

1. Install **Go** (go.dev/dl) and **Git** (git-scm.com). On Windows, the default options
   of both installers are fine.
2. In Command Prompt:
   ```
   git clone https://github.com/ploouff/CastleKeep.git
   cd CastleKeep\console
   go run . wallet -name bob -chains https://castlekeep.example.com/chains -exchange https://castlekeep.example.com/exchange -watchtower https://castlekeep.example.com/tower
   ```
   (Each friend uses their own name instead of `bob`.)
3. Their wallet page opens at `http://127.0.0.1:9300`, on their own computer. The
   **Start here** box walks them through it: copy an address, get faucet coins, lock, trade,
   settle. They don't need to download the signet chain: their wallet reads it through your
   server's gateway.

Each time, they start the wallet with the same line: it remembers the watchtower, but not
the exchange's and chains' addresses. To save typing, put the line in a `.bat` file.

## Updating

```
su - castlekeep
cd CastleKeep && git pull && cd console && go build -o castlekeep .
exit
systemctl restart castlekeep-chains castlekeep-exchange castlekeep-watchtower
```

Everything is kept between restarts (in `/home/castlekeep/castlekeep-data`).

## What shared mode does, and what it doesn't

`-shared` turns on these checks:

- **Every request to the exchange is signed** by the wallet's own account key (with the
  time and a random value). The exchange keeps the first key each account used, refuses a
  request it has already seen or that is more than 5 minutes old, and only lets a wallet
  touch its own account, cash, orders and vaults.
- **On the EVM networks**, a wallet proves a vault is its own (a signature from the key that
  opened it) before the exchange registers it, so nobody can take its number first.
- **Only you (with the admin token)** can change the exchange's behaviour or prices, see its
  admin page, or see the watchtower's page. In your friends' wallets the test tools for the
  exchange's behaviour say so instead.
- **The chains program's gateway** passes on to the Bitcoin node only the calls the programs
  use: reading blocks, transactions, unspent coins, the waiting list and the fee estimate,
  finding coins sent to an address, and sending a transaction. Its faucet, mining and
  contract endpoints only answer programs on the server itself.
- **Limits**, so one wallet (or a script) can't slow the server down for everyone or fill
  its disk. Going over one gets a plain message saying which limit it was:

  | What | Limit |
  | --- | --- |
  | Requests from one internet address (each of the three programs) | 60 a second on average, up to 600 at once |
  | Requests from one exchange account | 30 a second on average, up to 300 at once |
  | Accounts on the exchange, wallets on the watchtower | 100 each (`-max-accounts`, `-max-wallets`) |
  | New accounts (or watchtower wallets) from one address | 5 a day |
  | Bitcoin vaults per account that aren't closed | 10 (vaults that were never finished make room by themselves) |
  | Open orders per account | 50 |
  | "Add test cash" | stops at $100,000 of cash |
  | What the watchtower keeps per wallet | 200 vaults, 16 MB of signed transactions |
  | Calls from outside to the Bitcoin node at once | 16 (others wait up to 10 seconds) |
  | Size of one request to the exchange | 256 KB |

  A wallet sends the exchange about 7 requests a second, however many tabs of its page
  are open (measured with all seven chains and an open order: the page's prices, cash
  and order books are reused for 2 seconds). So the rates leave room for several wallets
  on one internet connection. The programs
  read each visitor's address from what Caddy passes on, so keep them on 127.0.0.1 behind
  Caddy as above. Requests from the server itself (the exchange and watchtower reading the
  chains) aren't counted. A connection that sends its request too slowly is cut off after
  a minute.

It does **not**:

- Make the code safe for real money. It has not been audited. Test networks only.
- Stop someone with many internet addresses: the limits count per address (an IPv6
  address counts per block of 2^64, the size one connection usually gets), so more
  addresses means more requests and more new accounts. Fine for a group of friends; not
  for the open internet at large.
- Limit vaults on the EVM networks: each one already costs its owner a deposit and gas
  before the exchange hears of it.
- Hide anything from the exchange: like any exchange, it sees every trade. The watchtower
  sees its users' vault transactions and amounts.
- Protect a wallet's file on a friend's computer: keys are in plain text there unless they
  start the wallet with `-password`.
