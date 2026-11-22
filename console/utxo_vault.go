package main

// Vault scripts and transaction building.
//
//	Vault output V:  [2-of-2]
//	Exit output  E:  [2-of-2]  or  [user alone after the delay]
//
// The exit request (V -> E) is signed by both parties when the vault opens,
// so the user can always start the exit clock on their own. Every trade
// produces a new State whose payout from E the user signs at order placement.
//
// Revocation (Lightning-style). In each state's payout from E, the user's
// share goes straight to the user's wallet, and the exchange's share goes to
// a revocable output R_n:
//
//	R_n:  [<D2> CSV DROP <exchange> CHECKSIG]                  exchange, D2 blocks later
//	  or  [SHA256 <H(r_n)> EQUALVERIFY <user> CHECKSIG]         user, with secret r_n
//
// The exchange picks a random secret r_n for each state and gives the user
// only its hash when the state is signed. When a newer state replaces state
// n, the exchange hands the user r_n. If the exchange later publishes state
// n anyway, the user's watchtower uses r_n to sweep R_n before D2 runs out,
// so publishing an old state costs the exchange its whole share.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

const txFee = 1000 // flat fee in base units; fine for regtest

// scriptNumHex is how DisasmString prints a small number pushed in a script.
func scriptNumHex(n int64) string {
	var b []byte
	for v := n; v > 0; v >>= 8 {
		b = append(b, byte(v&0xff))
	}
	if len(b) > 0 && b[len(b)-1]&0x80 != 0 {
		b = append(b, 0)
	}
	return hex.EncodeToString(b)
}

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
	for _, d := range []int64{v.delay, v.penaltyDelay} {
		s = strings.Replace(s, scriptNumHex(d)+" OP_CHECKSEQUENCEVERIFY", fmt.Sprintf("%d OP_CHECKSEQUENCEVERIFY", d), 1)
	}
	uk, ek := hex.EncodeToString(xonly(v.userKey)), hex.EncodeToString(xonly(v.exchKey))
	s = strings.ReplaceAll(s, uk, "<your key "+uk[:8]+"…>")
	s = strings.ReplaceAll(s, ek, "<exchange key "+ek[:8]+"…>")
	for _, st := range v.States {
		h := hex.EncodeToString(st.revHash)
		s = strings.ReplaceAll(s, h, fmt.Sprintf("<H(r_%d) %s…>", st.Index, h[:8]))
	}
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

	// Revocation. secret is r_n: the exchange creates it and keeps it until
	// a newer state is signed, then hands it to the user (revealed = true).
	// revHash is H(r_n), which the user gets when the state is signed.
	secret   []byte
	revHash  []byte
	revealed bool
	exchLeaf txscript.TapLeaf // <D2> CSV DROP <exchange> CHECKSIG
	revLeaf  txscript.TapLeaf // SHA256 <H(r_n)> EQUALVERIFY <user> CHECKSIG
	R        *tapOutput       // the exchange's share in this state's payout
	rIndex   int              // output index of R in tx, or -1 if no exchange share
	rAmt     int64
}

type Vault struct {
	delay                  int64             // exit window in blocks
	penaltyDelay           int64             // D2: blocks before the exchange can take its share of a published state
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

	// After a state is published: which one, and who took the exchange's share.
	published *State
	sweepTxid string
	sweepKind string // "penalty" (user, with the revocation secret) or "exchange" (after D2)
}

func newVault(userPayout, exchPayout []byte, delay, penaltyDelay int64) *Vault {
	uk, _ := btcec.NewPrivateKey()
	ek, _ := btcec.NewPrivateKey()
	v := &Vault{delay: delay, penaltyDelay: penaltyDelay, userKey: uk, exchKey: ek, userPayout: userPayout, exchPayout: exchPayout}

	ms, err := txscript.NewScriptBuilder().
		AddData(xonly(uk)).AddOp(txscript.OP_CHECKSIGVERIFY).
		AddData(xonly(ek)).AddOp(txscript.OP_CHECKSIG).Script()
	if err != nil {
		panic(err)
	}
	dl, err := txscript.NewScriptBuilder().
		AddInt64(delay).AddOp(txscript.OP_CHECKSEQUENCEVERIFY).AddOp(txscript.OP_DROP).
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
// user's share when there is one. The exchange's share goes to exchScript.
// It returns the transaction and the index and amount of the exchange's
// output (-1 and 0 if there is none).
func (v *Vault) payoutTx(prev wire.OutPoint, amt int64, userSats, exchSats int64, exchScript []byte) (*wire.MsgTx, int, int64) {
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
		tx.AddTxOut(wire.NewTxOut(exch, exchScript))
		return tx, len(tx.TxOut) - 1, exch
	}
	return tx, -1, 0
}

// newRevocation gives a state its secret r_n, the hash H(r_n), and the
// revocable output R_n that will hold the exchange's share.
func (v *Vault) newRevocation(s *State) {
	s.secret = make([]byte, 32)
	if _, err := rand.Read(s.secret); err != nil {
		panic(err)
	}
	h := sha256.Sum256(s.secret)
	s.revHash = h[:]
	el, err := txscript.NewScriptBuilder().
		AddInt64(v.penaltyDelay).AddOp(txscript.OP_CHECKSEQUENCEVERIFY).AddOp(txscript.OP_DROP).
		AddData(xonly(v.exchKey)).AddOp(txscript.OP_CHECKSIG).Script()
	if err != nil {
		panic(err)
	}
	rl, err := txscript.NewScriptBuilder().
		AddOp(txscript.OP_SHA256).AddData(s.revHash).AddOp(txscript.OP_EQUALVERIFY).
		AddData(xonly(v.userKey)).AddOp(txscript.OP_CHECKSIG).Script()
	if err != nil {
		panic(err)
	}
	s.exchLeaf = txscript.NewBaseTapLeaf(el)
	s.revLeaf = txscript.NewBaseTapLeaf(rl)
	s.R = newTapOutput(s.exchLeaf, s.revLeaf)
}

// signState: the exchange creates r_n and sends H(r_n); the user signs the
// post-trade payout from E up front. The exchange then hands over the
// secret of the state this one replaces, revoking it.
func (v *Vault) signState(label string, userSats, exchSats, height int64) *State {
	s := &State{Index: len(v.States), Label: label, UserSats: userSats, ExchSats: exchSats, Height: height}
	v.newRevocation(s)
	s.tx, s.rIndex, s.rAmt = v.payoutTx(v.exitOut(), v.exitAmt, userSats, exchSats, s.R.pkScript)
	s.userPreSig = leafSig(s.tx, v.exitAmt, v.E, v.multisigLeaf, v.userKey)
	if len(v.States) > 0 {
		v.Latest().revealed = true
	}
	v.States = append(v.States, s)
	return s
}

// stateOut is the outpoint of the exchange's share R_n in state s's payout.
func (s *State) stateOut() wire.OutPoint {
	return wire.OutPoint{Hash: s.tx.TxHash(), Index: uint32(s.rIndex)}
}

// penaltySweep: the user takes R_n with the revocation secret r_n.
func (v *Vault) penaltySweep(s *State, secret []byte) *wire.MsgTx {
	tx := wire.NewMsgTx(2)
	prev := s.stateOut()
	tx.AddTxIn(wire.NewTxIn(&prev, nil, nil))
	tx.AddTxOut(wire.NewTxOut(s.rAmt-txFee, v.userPayout))
	sig := leafSig(tx, s.rAmt, s.R, s.revLeaf, v.userKey)
	tx.TxIn[0].Witness = wire.TxWitness{sig, secret, s.revLeaf.Script, s.R.controlBlock(s.revLeaf)}
	return tx
}

// exchangeClaim: the exchange takes R_n alone once D2 blocks have passed.
func (v *Vault) exchangeClaim(s *State) *wire.MsgTx {
	tx := wire.NewMsgTx(2)
	prev := s.stateOut()
	in := wire.NewTxIn(&prev, nil, nil)
	in.Sequence = uint32(v.penaltyDelay)
	tx.AddTxIn(in)
	tx.AddTxOut(wire.NewTxOut(s.rAmt-txFee, v.exchPayout))
	sig := leafSig(tx, s.rAmt, s.R, s.exchLeaf, v.exchKey)
	tx.TxIn[0].Witness = wire.TxWitness{sig, s.exchLeaf.Script, s.R.controlBlock(s.exchLeaf)}
	return tx
}

// publishState: the exchange adds its signature to a user-signed state.
func (v *Vault) publishState(s *State) *wire.MsgTx {
	tx := s.tx.Copy()
	v.witnessBoth(tx, v.exitAmt, v.E, s.userPreSig)
	return tx
}

func (v *Vault) cooperativeSettle() *wire.MsgTx {
	l := v.Latest()
	// Settling spends the vault directly with both signatures, so no old
	// state can be involved and the exchange's share goes straight to it.
	tx, _, _ := v.payoutTx(v.vaultOut, v.vaultAmt, l.UserSats, l.ExchSats, v.exchPayout)
	v.witnessBoth(tx, v.vaultAmt, v.V, nil)
	return tx
}

func (v *Vault) userClaim() *wire.MsgTx {
	tx := wire.NewMsgTx(2)
	prev := v.exitOut()
	in := wire.NewTxIn(&prev, nil, nil)
	in.Sequence = uint32(v.delay)
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
