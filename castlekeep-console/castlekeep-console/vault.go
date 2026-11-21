package main

// Vault scripts and transaction building.
//
//	Vault output V:  [2-of-2]
//	Exit output  E:  [2-of-2]  or  [user alone after 144 blocks]
//
// The exit request (V -> E) is signed by both parties when the vault opens,
// so the user can always start the 144-block clock on their own. Every trade
// produces a new State whose payout from E the user signs at order placement.

import (
	"encoding/hex"
	"strings"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

const (
	exitDelay = 144  // blocks after the exit request confirms
	txFee     = 1000 // flat fee in sats; fine for regtest
	sat       = 100_000_000
)

// numsKey: BIP 341 "nothing up my sleeve" point. No one knows its private
// key, so the Taproot key path is disabled and only the scripts can spend.
var numsKey = func() *btcec.PublicKey {
	b, _ := hex.DecodeString("50929b74c1a04954b78b4b6035e97a5e078a5a0f28ec96d547bfee9ace803ac0")
	k, err := schnorr.ParsePubKey(b)
	if err != nil {
		panic(err)
	}
	return k
}()

func xonly(k *btcec.PrivateKey) []byte { return schnorr.SerializePubKey(k.PubKey()) }

// walletScript is an ordinary BIP 86 Taproot address for a key.
func walletScript(k *btcec.PrivateKey) []byte {
	s, err := txscript.PayToTaprootScript(txscript.ComputeTaprootKeyNoScript(k.PubKey()))
	if err != nil {
		panic(err)
	}
	return s
}

type tapOutput struct {
	pkScript []byte
	tree     *txscript.IndexedTapScriptTree
}

func newTapOutput(leaves ...txscript.TapLeaf) *tapOutput {
	tree := txscript.AssembleTaprootScriptTree(leaves...)
	root := tree.RootNode.TapHash()
	pk, err := txscript.PayToTaprootScript(txscript.ComputeTaprootOutputKey(numsKey, root[:]))
	if err != nil {
		panic(err)
	}
	return &tapOutput{pkScript: pk, tree: tree}
}

func (o *tapOutput) controlBlock(leaf txscript.TapLeaf) []byte {
	idx := o.tree.LeafProofIndex[leaf.TapHash()]
	cb := o.tree.LeafMerkleProofs[idx].ToControlBlock(numsKey)
	b, err := cb.ToBytes()
	if err != nil {
		panic(err)
	}
	return b
}

func leafSig(tx *wire.MsgTx, amt int64, out *tapOutput, leaf txscript.TapLeaf, key *btcec.PrivateKey) []byte {
	fetcher := txscript.NewCannedPrevOutputFetcher(out.pkScript, amt)
	hashes := txscript.NewTxSigHashes(tx, fetcher)
	sig, err := txscript.RawTxInTapscriptSignature(tx, hashes, 0, amt, out.pkScript, leaf, txscript.SigHashDefault, key)
	if err != nil {
		panic(err)
	}
	return sig
}

func disasm(script []byte) string {
	s, err := txscript.DisasmString(script)
	if err != nil {
		return hex.EncodeToString(script)
	}
	return s
}

// friendlyScript renders a vault script with labelled keys and decimal numbers.
func (v *Vault) friendlyScript(script []byte) string {
	s := disasm(script)
	s = strings.Replace(s, "9000 OP_CHECKSEQUENCEVERIFY", "144 OP_CHECKSEQUENCEVERIFY", 1)
	uk, ek := hex.EncodeToString(xonly(v.userKey)), hex.EncodeToString(xonly(v.exchKey))
	s = strings.ReplaceAll(s, uk, "<your key "+uk[:8]+"…>")
	s = strings.ReplaceAll(s, ek, "<exchange key "+ek[:8]+"…>")
	return s
}

// State is one co-signable split of the vault (in vault sats).
type State struct {
	Index      int
	Label      string
	UserSats   int64
	ExchSats   int64
	Height     int64
	userPreSig []byte
	tx         *wire.MsgTx // payout from the exit output
}

type Vault struct {
	userKey, exchKey       *btcec.PrivateKey // per-vault signing keys
	userPayout, exchPayout []byte            // persistent wallets

	multisigLeaf txscript.TapLeaf
	delayLeaf    txscript.TapLeaf
	V, E         *tapOutput

	vaultOut wire.OutPoint
	vaultAmt int64

	exitTx  *wire.MsgTx
	exitAmt int64

	States []*State

	// lifecycle
	fundTxid     string
	exitSent     bool
	closeTxid    string
	closeKind    string // "settled", "claimed", "published"
	refusedClaim int
}

func newVault(userPayout, exchPayout []byte) *Vault {
	uk, _ := btcec.NewPrivateKey()
	ek, _ := btcec.NewPrivateKey()
	v := &Vault{userKey: uk, exchKey: ek, userPayout: userPayout, exchPayout: exchPayout}

	ms, err := txscript.NewScriptBuilder().
		AddData(xonly(uk)).AddOp(txscript.OP_CHECKSIGVERIFY).
		AddData(xonly(ek)).AddOp(txscript.OP_CHECKSIG).Script()
	if err != nil {
		panic(err)
	}
	dl, err := txscript.NewScriptBuilder().
		AddInt64(exitDelay).AddOp(txscript.OP_CHECKSEQUENCEVERIFY).AddOp(txscript.OP_DROP).
		AddData(xonly(uk)).AddOp(txscript.OP_CHECKSIG).Script()
	if err != nil {
		panic(err)
	}
	v.multisigLeaf = txscript.NewBaseTapLeaf(ms)
	v.delayLeaf = txscript.NewBaseTapLeaf(dl)
	v.V = newTapOutput(v.multisigLeaf)
	v.E = newTapOutput(v.multisigLeaf, v.delayLeaf)
	return v
}

func (v *Vault) Latest() *State { return v.States[len(v.States)-1] }

func (v *Vault) exitOut() wire.OutPoint { return wire.OutPoint{Hash: v.exitTx.TxHash(), Index: 0} }

func (v *Vault) witnessBoth(tx *wire.MsgTx, amt int64, out *tapOutput, userSig []byte) {
	if userSig == nil {
		userSig = leafSig(tx, amt, out, v.multisigLeaf, v.userKey)
	}
	exchSig := leafSig(tx, amt, out, v.multisigLeaf, v.exchKey)
	tx.TxIn[0].Witness = wire.TxWitness{exchSig, userSig, v.multisigLeaf.Script, out.controlBlock(v.multisigLeaf)}
}

// presignExit builds and co-signs the V -> E exit request.
func (v *Vault) presignExit() {
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(wire.NewTxIn(&v.vaultOut, nil, nil))
	v.exitAmt = v.vaultAmt - txFee
	tx.AddTxOut(wire.NewTxOut(v.exitAmt, v.E.pkScript))
	v.witnessBoth(tx, v.vaultAmt, v.V, nil)
	v.exitTx = tx
}

// payoutTx splits `amt` according to the state's ratio; fee comes from the
// user's share when there is one.
func (v *Vault) payoutTx(prev wire.OutPoint, amt int64, userSats, exchSats int64) *wire.MsgTx {
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(wire.NewTxIn(&prev, nil, nil))
	user := int64(float64(userSats) * float64(amt) / float64(userSats+exchSats))
	exch := amt - user
	if user > txFee+546 {
		user -= txFee
	} else {
		exch += user - txFee
		user = 0
	}
	if user > 0 {
		tx.AddTxOut(wire.NewTxOut(user, v.userPayout))
	}
	if exch > 0 {
		tx.AddTxOut(wire.NewTxOut(exch, v.exchPayout))
	}
	return tx
}

// signState: the user signs the post-trade payout from E up front.
func (v *Vault) signState(label string, userSats, exchSats, height int64) *State {
	s := &State{Index: len(v.States), Label: label, UserSats: userSats, ExchSats: exchSats, Height: height}
	s.tx = v.payoutTx(v.exitOut(), v.exitAmt, userSats, exchSats)
	s.userPreSig = leafSig(s.tx, v.exitAmt, v.E, v.multisigLeaf, v.userKey)
	v.States = append(v.States, s)
	return s
}

// publishState: the exchange adds its signature to a user-signed state.
func (v *Vault) publishState(s *State) *wire.MsgTx {
	tx := s.tx.Copy()
	v.witnessBoth(tx, v.exitAmt, v.E, s.userPreSig)
	return tx
}

func (v *Vault) cooperativeSettle() *wire.MsgTx {
	l := v.Latest()
	tx := v.payoutTx(v.vaultOut, v.vaultAmt, l.UserSats, l.ExchSats)
	v.witnessBoth(tx, v.vaultAmt, v.V, nil)
	return tx
}

func (v *Vault) userClaim() *wire.MsgTx {
	tx := wire.NewMsgTx(2)
	prev := v.exitOut()
	in := wire.NewTxIn(&prev, nil, nil)
	in.Sequence = exitDelay
	tx.AddTxIn(in)
	tx.AddTxOut(wire.NewTxOut(v.exitAmt-txFee, v.userPayout))
	sig := leafSig(tx, v.exitAmt, v.E, v.delayLeaf, v.userKey)
	tx.TxIn[0].Witness = wire.TxWitness{sig, v.delayLeaf.Script, v.E.controlBlock(v.delayLeaf)}
	return tx
}

// exchangeOnlySpend: the exchange signs alone and fakes the user's slot.
func (v *Vault) exchangeOnlySpend(prev wire.OutPoint, amt int64, out *tapOutput) *wire.MsgTx {
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(wire.NewTxIn(&prev, nil, nil))
	tx.AddTxOut(wire.NewTxOut(amt-txFee, v.exchPayout))
	exchSig := leafSig(tx, amt, out, v.multisigLeaf, v.exchKey)
	tx.TxIn[0].Witness = wire.TxWitness{exchSig, exchSig, v.multisigLeaf.Script, out.controlBlock(v.multisigLeaf)}
	return tx
}
