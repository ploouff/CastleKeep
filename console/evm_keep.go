package main

// Kept EVM test chains for the separate programs. Hardhat forgets its chain
// when it stops, so the chains program puts a small recorder in front of each
// Hardhat node: every request that changes the chain (transactions, balances,
// impersonation, mined blocks and their timestamps) is appended to
// evm-<chain>.jsonl. On the next start the recorder plays the file back into a
// new Hardhat node, block by block with the same timestamps, so the contract,
// the vaults and every transaction hash come back. The chain's clock stands
// still while the programs are stopped.
//
// Test networks only.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcReply struct {
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

// journalRec is one line of evm-<chain>.jsonl.
type journalRec struct {
	M  string          `json:"m"`
	P  json.RawMessage `json:"p,omitempty"`
	R  string          `json:"r,omitempty"`  // a transaction's hash, checked on replay
	TS int64           `json:"ts,omitempty"` // evm_mine: the block's timestamp
}

// recorded lists the methods that change a Hardhat chain.
func recorded(m string) bool {
	switch m {
	case "eth_sendTransaction", "eth_sendRawTransaction", "evm_mine", "hardhat_mine",
		"evm_increaseTime", "evm_setNextBlockTimestamp", "evm_setAutomine", "evm_setIntervalMining",
		"hardhat_setBalance", "hardhat_impersonateAccount", "hardhat_stopImpersonatingAccount",
		"hardhat_setCode", "hardhat_setNonce", "hardhat_setStorageAt":
		return true
	}
	return false
}

type keptEVM struct {
	chain   evmChain
	file    string
	node    *exec.Cmd
	nodeURL string // Hardhat itself
	URL     string // the recorder, which everyone else uses
	mu      sync.Mutex
	out     *os.File
	srv     *http.Server
	Eng     *EthEngine
	Note    string // what happened on start, for the chains window
}

// startKeptEVM starts Hardhat for chain c, replays file into it, and serves
// the recorder on port.
func startKeptEVM(c evmChain, hardhatDir, file string, port int) (*keptEVM, error) {
	k := &keptEVM{chain: c, file: file}
	genesis := savedGenesis(file)
	hp := freePort()
	cmd, err := startHardhatAt(hardhatDir, c.ChainID, hp, genesis)
	if err != nil {
		return nil, err
	}
	k.node, k.nodeURL = cmd, fmt.Sprintf("http://127.0.0.1:%d", hp)
	for i := 0; ; i++ {
		if _, err := k.rpc("eth_chainId"); err == nil {
			break
		}
		if i > 120 {
			k.Stop()
			return nil, fmt.Errorf("the %s test node didn't start", c.Name)
		}
		time.Sleep(500 * time.Millisecond)
	}
	n, bad, err := k.replay()
	if err != nil {
		k.Stop()
		return nil, err
	}
	switch {
	case n > 0 && bad > 0:
		k.Note = fmt.Sprintf("replayed %d saved steps, but %d transactions came out different; start with -fresh if vaults look wrong", n, bad)
	case n > 0:
		k.Note = fmt.Sprintf("carried on from %d saved steps", n)
	}
	if k.out, err = os.OpenFile(file, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err != nil {
		k.Stop()
		return nil, err
	}
	if genesis == 0 {
		// A new chain: remember when its first block is, so a replay can
		// start the new node at the same date.
		r, err := k.rpc("eth_getBlockByNumber", "0x0", false)
		var b struct{ Timestamp string }
		if err == nil && json.Unmarshal(r, &b) == nil {
			ts, _ := strconv.ParseInt(strings.TrimPrefix(b.Timestamp, "0x"), 16, 64)
			line, _ := json.Marshal(journalRec{M: "genesis", TS: ts})
			k.out.Write(append(line, '\n'))
		}
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		k.Stop()
		return nil, fmt.Errorf("port %d is taken (needed for %s)", port, c.Name)
	}
	k.URL = "http://" + ln.Addr().String()
	k.srv = &http.Server{Handler: http.HandlerFunc(k.serve)}
	go k.srv.Serve(ln)
	return k, nil
}

func (k *keptEVM) rpc(method string, params ...any) (json.RawMessage, error) {
	if params == nil {
		params = []any{}
	}
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	return k.post(b)
}

func (k *keptEVM) post(body []byte) (json.RawMessage, error) {
	c := http.Client{Timeout: 60 * time.Second}
	r, err := c.Post(k.nodeURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()
	var rep rpcReply
	if err := json.NewDecoder(r.Body).Decode(&rep); err != nil {
		return nil, err
	}
	if len(rep.Error) > 0 && string(rep.Error) != "null" {
		return nil, fmt.Errorf("%s", rep.Error)
	}
	return rep.Result, nil
}

func (k *keptEVM) latestTime() int64 {
	r, err := k.rpc("eth_getBlockByNumber", "latest", false)
	if err != nil {
		return 0
	}
	var b struct{ Timestamp string }
	json.Unmarshal(r, &b)
	ts, _ := strconv.ParseInt(strings.TrimPrefix(b.Timestamp, "0x"), 16, 64)
	return ts
}

// savedGenesis is the first block's time saved at the top of file (0: none).
func savedGenesis(file string) int64 {
	f, err := os.Open(file)
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if sc.Scan() {
		var j journalRec
		if json.Unmarshal(sc.Bytes(), &j) == nil && j.M == "genesis" {
			return j.TS
		}
	}
	return 0
}

// replay plays the saved file into the fresh node. It returns the number of
// steps and how many transactions got a different hash than before.
func (k *keptEVM) replay() (n, bad int, err error) {
	f, err := os.Open(k.file)
	if os.IsNotExist(err) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var j journalRec
		if json.Unmarshal(sc.Bytes(), &j) != nil {
			continue // a half-written last line
		}
		n++
		var params any = []any{}
		if len(j.P) > 0 {
			params = j.P
		}
		switch j.M {
		case "evm_mine":
			if j.TS > 0 {
				if _, err := k.rpc("evm_setNextBlockTimestamp", j.TS); err != nil {
					return n, bad, fmt.Errorf("replaying %s block at %d: %v", k.chain.Name, j.TS, err)
				}
			}
			if _, err := k.rpc("evm_mine"); err != nil {
				return n, bad, fmt.Errorf("replaying a %s block: %v", k.chain.Name, err)
			}
		case "genesis":
			n-- // read before the node started
		case "evm_increaseTime", "evm_setNextBlockTimestamp":
			// The timestamps saved with each block put the clock back exactly.
		default:
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": j.M, "params": params})
			r, err := k.post(b)
			if err != nil {
				if j.R != "" {
					bad++
				}
				continue
			}
			if j.R != "" {
				var h string
				json.Unmarshal(r, &h)
				if !strings.EqualFold(h, j.R) {
					bad++
				}
			}
		}
	}
	return n, bad, sc.Err()
}

// serve forwards one JSON-RPC request (or a batch) to Hardhat, recording the
// ones that change the chain, in the order the node applied them.
func (k *keptEVM) serve(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var msgs []rpcMsg
	t := bytes.TrimSpace(body)
	if len(t) > 0 && t[0] == '[' {
		json.Unmarshal(t, &msgs)
	} else {
		var m rpcMsg
		json.Unmarshal(t, &m)
		msgs = []rpcMsg{m}
	}
	changes := false
	for _, m := range msgs {
		changes = changes || recorded(m.Method)
	}
	if changes {
		k.mu.Lock()
		defer k.mu.Unlock()
	}
	resp, err := http.Post(k.nodeURL, "application/json", bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if changes {
		var reps []rpcReply
		ot := bytes.TrimSpace(out)
		if len(ot) > 0 && ot[0] == '[' {
			json.Unmarshal(ot, &reps)
		} else {
			var rp rpcReply
			json.Unmarshal(ot, &rp)
			reps = []rpcReply{rp}
		}
		for i, m := range msgs {
			if !recorded(m.Method) || i >= len(reps) || (len(reps[i].Error) > 0 && string(reps[i].Error) != "null") {
				continue
			}
			j := journalRec{M: m.Method, P: m.Params}
			switch m.Method {
			case "eth_sendTransaction", "eth_sendRawTransaction":
				json.Unmarshal(reps[i].Result, &j.R)
			case "evm_mine":
				j.P, j.TS = nil, k.latestTime()
			}
			b, _ := json.Marshal(j)
			k.out.Write(append(b, '\n'))
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	w.Write(out)
}

func (k *keptEVM) Stop() {
	if k.srv != nil {
		k.srv.Close()
	}
	if k.Eng != nil {
		k.Eng.Stop()
	}
	if k.out != nil {
		k.mu.Lock()
		k.out.Close()
		k.mu.Unlock()
	}
	if k.node != nil && k.node.Process != nil {
		k.node.Process.Kill()
		k.node.Wait()
	}
}
