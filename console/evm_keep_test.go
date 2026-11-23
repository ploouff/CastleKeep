package main

// The kept EVM chain: trade through the recorder, stop the Hardhat node,
// start a new one from the saved file, and check that the chain came back
// exactly (same blocks, same contract, same vault, same transaction hashes).

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestKeptEVMChainRestarts(t *testing.T) {
	if !hardhatInstalled(defaultHardhatDir()) {
		t.Skipf("Hardhat isn't installed in %s", defaultHardhatDir())
	}
	c, _ := evmByID("pol")
	file := filepath.Join(t.TempDir(), "evm-pol.jsonl")
	port := freePort()
	k, err := startKeptEVM(c, defaultHardhatDir(), file, port)
	if err != nil {
		t.Fatal(err)
	}
	e := NewEthEngine(evmConfig(c, "", "", k.URL))
	e.UseAccount(NewAccount())
	e.Start()
	for !e.Snapshot().Ready {
		if s := e.Snapshot(); s.InitErr != "" {
			k.Stop()
			t.Fatal(s.InitErr)
		}
		time.Sleep(200 * time.Millisecond)
	}
	do := func(r ActionReq) {
		t.Helper()
		if err := e.Do(r); err != nil {
			k.Stop()
			t.Fatalf("%s: %v", r.Action, err)
		}
	}
	do(ActionReq{Action: "lock", Sats: oneCoin})
	do(ActionReq{Action: "mine", N: 1})
	do(ActionReq{Action: "order", Side: "sell", Sats: 30_000_000})
	do(ActionReq{Action: "exit"})
	do(ActionReq{Action: "mine", Secs: 3600})

	type look struct {
		Height   int64
		Hash     string
		Contract string
		Vault    chainVault
		Amount   int64
		Receipt  int
		Time     int64
	}
	lookAt := func(ec *EthEngine, id string, openTx string) look {
		var l look
		l.Height, l.Contract, l.Time = ec.height(), ec.contract, ec.chainTime()
		raw, _ := ec.call("eth_getBlockByNumber", "latest", false)
		var b struct{ Hash string }
		json.Unmarshal(raw, &b)
		l.Hash = b.Hash
		e.mu.Lock()
		vid := e.vault.id
		e.mu.Unlock()
		cv, err := ec.readVaultID(vid)
		if err != nil {
			t.Fatal(err)
		}
		l.Vault = chainVault{exitStart: cv.exitStart, bestNonce: cv.bestNonce, bestUser: cv.bestUser, hasBest: cv.hasBest, closed: cv.closed}
		l.Amount = cv.amount
		l.Receipt = ec.receipt(openTx)
		return l
	}
	e.mu.Lock()
	openTx := e.vault.openTx
	e.mu.Unlock()
	before := lookAt(e, "", openTx)
	ep := e.Endpoint()
	e.Stop()
	k.Stop()

	k2, err := startKeptEVM(c, defaultHardhatDir(), file, port)
	if err != nil {
		t.Fatal(err)
	}
	defer k2.Stop()
	t.Log("on restart:", k2.Note)
	after := lookAt(newEthLight(ep), "", openTx)
	if before.Height != after.Height || before.Hash != after.Hash || before.Contract != after.Contract ||
		before.Vault != after.Vault || before.Amount != after.Amount || after.Receipt != 1 {
		t.Fatalf("the chain didn't come back the same:\nbefore %+v\nafter  %+v", before, after)
	}
	if after.Time < before.Time {
		t.Fatalf("the clock went back: %d before, %d after", before.Time, after.Time)
	}
	t.Logf("same block #%d (%s), same vault (exit started at %d), contract %s", after.Height, after.Hash[:10], after.Vault.exitStart, after.Contract)
}
