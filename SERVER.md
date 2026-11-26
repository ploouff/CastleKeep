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

It does **not**:

- Make the code safe for real money. It has not been audited. Test networks only.
- Limit how much anyone can send: someone could flood the server with requests. Fine for a
  small group of friends; not for the open internet at large.
- Hide anything from the exchange: like any exchange, it sees every trade. The watchtower
  sees its users' vault transactions and amounts.
- Stop people making as many accounts as they like, each with free test cash.
- Protect a wallet's file on a friend's computer: keys are in plain text there unless they
  start the wallet with `-password`.
