package main

// Vault scripts and transaction building.
//
//	Vault output V:  [2-of-2]
//	Exit output  E:  [2-of-2]  or  [2-of-2 after the delay: pay back deposits]
//
// The exit request (V -> E) and the pay-back (E -> each side's deposit, only
// valid after the delay) are signed by both parties when the vault opens, so
// the user can always get out on their own. Every trade produces a new State.
//
// Two-way revocation (Lightning-style). Each state has two payouts from E,
// both signed by both sides, so either side can publish the latest state:
//
//	the exchange's copy:  user's share -> user wallet,  exchange's share -> R_n
//	the user's copy:      exchange's share -> exchange, user's share     -> U_n
//
//	R_n:  [<D2> CSV DROP <exchange> CHECKSIG]                exchange, D2 blocks later
//	  or  [SHA256 <H(r_n)> EQUALVERIFY <user> CHECKSIG]       user, with the exchange's secret r_n
//	U_n:  [<D2> CSV DROP <user> CHECKSIG]                    user, D2 blocks later
//	  or  [SHA256 <H(u_n)> EQUALVERIFY <exchange> CHECKSIG]   exchange, with the user's secret u_n
//
// Each side picks a random secret per state and gives the other only its
// hash. When a newer state replaces state n, both hand over their secret for
// n. Whoever publishes a replaced copy loses their share of it to the other
// side's watchtower, as long as that watchtower acts within D2 blocks.
//
// The user signs the exchange's copy at order placement, so a trade can't be
// undone; the exchange signs the user's copy when it fills the order.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
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
		if st.uHash != nil {
			h = hex.EncodeToString(st.uHash)
			s = strings.ReplaceAll(s, h, fmt.Sprintf("<H(u_%d) %s…>", st.Index, h[:8]))
		}
	}
	return s
}

// State is one co-signable split of the vault (in vault sats).
type State struct {
	Index      int
	gen        int // which version of the vault (bumped by each splice) this state pays out from
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

	// The user's copy: the exchange pre-signs it, the user can publish it.
	// u_n is the user's secret; the exchange gets it once the state is
	// replaced (revealed covers both secrets: they are swapped together).
	utx        *wire.MsgTx
	exchPreSig []byte
	uSecret    []byte
	uHash      []byte
	userLeaf   txscript.TapLeaf // <D2> CSV DROP <user> CHECKSIG
	uRevLeaf   txscript.TapLeaf // SHA256 <H(u_n)> EQUALVERIFY <exchange> CHECKSIG
	U          *tapOutput       // the user's share in the user's copy
	uIndex     int              // output index of U in utx, or -1
	uAmt       int64
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

	// What each side put in, and the pre-signed pay-back of exactly that,
	// valid only after the exit delay.
	userDeposit, exchDeposit int64
	refundTx                 *wire.MsgTx

	States []*State
	gen    int // bumped each time the exchange adds coins (a splice); older states can't be published

	// lifecycle
	fundTxid     string
	exitSent     bool
	closeTxid    string
	closeKind    string // "settled", "claimed", "published"
	refusedClaim int

	// After a state is published: which one, by whom ("exchange" or "user"),
	// and who took the publisher's delayed share.
	published   *State
	publishedBy string
	sweepTxid   string
	// "penalty": the user took the exchange's share with r_n
	// "exchange": the exchange took its own share after D2
	// "exchange-penalty": the exchange took the user's share with u_n
	// "user": the user took their own share after D2
	sweepKind string
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
	// After the delay both keys are still needed, but both signed the
	// pay-back transaction when the vault opened, so the user can use it alone.
	dl, err := txscript.NewScriptBuilder().
		AddInt64(delay).AddOp(txscript.OP_CHECKSEQUENCEVERIFY).AddOp(txscript.OP_DROP).
		AddData(xonly(uk)).AddOp(txscript.OP_CHECKSIGVERIFY).
		AddData(xonly(ek)).AddOp(txscript.OP_CHECKSIG).Script()
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

// witnessLeaf completes a two-signature spend through leaf. Either side's
// signature may have been made earlier (pre-signed); nil means "sign now".
func (v *Vault) witnessLeaf(tx *wire.MsgTx, amt int64, out *tapOutput, leaf txscript.TapLeaf, userSig, exchSig []byte) {
	if userSig == nil {
		userSig = leafSig(tx, amt, out, leaf, v.userKey)
	}
	if exchSig == nil {
		exchSig = leafSig(tx, amt, out, leaf, v.exchKey)
	}
	tx.TxIn[0].Witness = wire.TxWitness{exchSig, userSig, leaf.Script, out.controlBlock(leaf)}
}

func (v *Vault) witnessBoth(tx *wire.MsgTx, amt int64, out *tapOutput, userSig []byte) {
	v.witnessLeaf(tx, amt, out, v.multisigLeaf, userSig, nil)
}

// presignExit builds and co-signs the V -> E exit request, and the pay-back
// of each side's deposit from E once the delay has passed.
func (v *Vault) presignExit() {
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(wire.NewTxIn(&v.vaultOut, nil, nil))
	v.exitAmt = v.vaultAmt - txFee
	tx.AddTxOut(wire.NewTxOut(v.exitAmt, v.E.pkScript))
	v.witnessBoth(tx, v.vaultAmt, v.V, nil)
	v.exitTx = tx

	rf, _, _, _, _ := v.payoutTx(v.exitOut(), v.exitAmt, v.userDeposit, v.exchDeposit, v.userPayout, v.exchPayout)
	rf.TxIn[0].Sequence = uint32(v.delay)
	v.witnessLeaf(rf, v.exitAmt, v.E, v.delayLeaf, nil, nil)
	v.refundTx = rf
}

// payoutTx splits amt in the ratio userSats:exchSats; the fee comes from the
// user's share when there is one. It returns the transaction and the index
// and amount of each side's output (-1 and 0 when a side gets nothing).
func (v *Vault) payoutTx(prev wire.OutPoint, amt int64, userSats, exchSats int64, userScript, exchScript []byte) (tx *wire.MsgTx, ui int, ua int64, ei int, ea int64) {
	return splitPayout(prev, amt, userSats, exchSats, userScript, exchScript)
}

// splitPayout is payoutTx without a vault, shared with the two-party code.
func splitPayout(prev wire.OutPoint, amt int64, userSats, exchSats int64, userScript, exchScript []byte) (tx *wire.MsgTx, ui int, ua int64, ei int, ea int64) {
	tx = wire.NewMsgTx(2)
	tx.AddTxIn(wire.NewTxIn(&prev, nil, nil))
	// Exact integer maths: userSats*amt can pass 2^53, where float64 rounds.
	user := new(big.Int).Div(new(big.Int).Mul(big.NewInt(userSats), big.NewInt(amt)), big.NewInt(userSats+exchSats)).Int64()
	exch := amt - user
	if user > txFee+546 {
		user -= txFee
	} else {
		exch += user - txFee
		user = 0
	}
	ui, ei = -1, -1
	if user > 0 {
		tx.AddTxOut(wire.NewTxOut(user, userScript))
		ui, ua = len(tx.TxOut)-1, user
	}
	if exch > 0 {
		tx.AddTxOut(wire.NewTxOut(exch, exchScript))
		ei, ea = len(tx.TxOut)-1, exch
	}
	return
}

func newSecret() (secret, hash []byte) {
	secret = make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		panic(err)
	}
	h := sha256.Sum256(secret)
	return secret, h[:]
}

// revocableOutput: the owner can take it after D2 blocks, or the other side
// at any time with the owner's secret for this state.
func (v *Vault) revocableOutput(owner, other *btcec.PrivateKey, hash []byte) (ownerLeaf, revLeaf txscript.TapLeaf, out *tapOutput) {
	ol, err := txscript.NewScriptBuilder().
		AddInt64(v.penaltyDelay).AddOp(txscript.OP_CHECKSEQUENCEVERIFY).AddOp(txscript.OP_DROP).
		AddData(xonly(owner)).AddOp(txscript.OP_CHECKSIG).Script()
	if err != nil {
		panic(err)
	}
	rl, err := txscript.NewScriptBuilder().
		AddOp(txscript.OP_SHA256).AddData(hash).AddOp(txscript.OP_EQUALVERIFY).
		AddData(xonly(other)).AddOp(txscript.OP_CHECKSIG).Script()
	if err != nil {
		panic(err)
	}
	ownerLeaf, revLeaf = txscript.NewBaseTapLeaf(ol), txscript.NewBaseTapLeaf(rl)
	return ownerLeaf, revLeaf, newTapOutput(ownerLeaf, revLeaf)
}

// newRevocation gives a state both secrets, their hashes, and the revocable
// outputs R_n (exchange's share in its copy) and U_n (user's share in theirs).
func (v *Vault) newRevocation(s *State) {
	s.secret, s.revHash = newSecret()
	s.exchLeaf, s.revLeaf, s.R = v.revocableOutput(v.exchKey, v.userKey, s.revHash)
	s.uSecret, s.uHash = newSecret()
	s.userLeaf, s.uRevLeaf, s.U = v.revocableOutput(v.userKey, v.exchKey, s.uHash)
}

// signState: each side creates its secret and sends the other its hash. The
// user signs the exchange's copy of the payout up front; the exchange signs
// the user's copy. Then both hand over their secret for the state this one
// replaces, revoking it.
func (v *Vault) signState(label string, userSats, exchSats, height int64) *State {
	s := &State{Index: len(v.States), Label: label, UserSats: userSats, ExchSats: exchSats, Height: height, gen: v.gen}
	v.newRevocation(s)
	s.tx, _, _, s.rIndex, s.rAmt = v.payoutTx(v.exitOut(), v.exitAmt, userSats, exchSats, v.userPayout, s.R.pkScript)
	s.userPreSig = leafSig(s.tx, v.exitAmt, v.E, v.multisigLeaf, v.userKey)
	s.utx, s.uIndex, s.uAmt, _, _ = v.payoutTx(v.exitOut(), v.exitAmt, userSats, exchSats, s.U.pkScript, v.exchPayout)
	s.exchPreSig = leafSig(s.utx, v.exitAmt, v.E, v.multisigLeaf, v.exchKey)
	if len(v.States) > 0 {
		v.Latest().revealed = true
	}
	v.States = append(v.States, s)
	return s
}

// stateOut is the outpoint of the exchange's share R_n in its copy of state s.
func (s *State) stateOut() wire.OutPoint {
	return wire.OutPoint{Hash: s.tx.TxHash(), Index: uint32(s.rIndex)}
}

// userOut is the outpoint of the user's share U_n in the user's copy.
func (s *State) userOut() wire.OutPoint {
	return wire.OutPoint{Hash: s.utx.TxHash(), Index: uint32(s.uIndex)}
}

// spendLeaf builds a one-input, one-output spend of a revocable output.
func spendLeaf(prev wire.OutPoint, amt int64, out *tapOutput, leaf txscript.TapLeaf, seq int64, key *btcec.PrivateKey, payTo []byte, extra ...[]byte) *wire.MsgTx {
	tx := wire.NewMsgTx(2)
	in := wire.NewTxIn(&prev, nil, nil)
	if seq > 0 {
		in.Sequence = uint32(seq)
	}
	tx.AddTxIn(in)
	tx.AddTxOut(wire.NewTxOut(amt-txFee, payTo))
	sig := leafSig(tx, amt, out, leaf, key)
	w := wire.TxWitness{sig}
	w = append(w, extra...)
	tx.TxIn[0].Witness = append(w, leaf.Script, out.controlBlock(leaf))
	return tx
}

// penaltySweep: the user takes R_n with the exchange's revocation secret r_n.
func (v *Vault) penaltySweep(s *State, secret []byte) *wire.MsgTx {
	return spendLeaf(s.stateOut(), s.rAmt, s.R, s.revLeaf, 0, v.userKey, v.userPayout, secret)
}

// exchangeClaim: the exchange takes R_n alone once D2 blocks have passed.
func (v *Vault) exchangeClaim(s *State) *wire.MsgTx {
	return spendLeaf(s.stateOut(), s.rAmt, s.R, s.exchLeaf, v.penaltyDelay, v.exchKey, v.exchPayout)
}

// exchPenaltySweep: the exchange takes U_n with the user's revocation secret u_n.
func (v *Vault) exchPenaltySweep(s *State, secret []byte) *wire.MsgTx {
	return spendLeaf(s.userOut(), s.uAmt, s.U, s.uRevLeaf, 0, v.exchKey, v.exchPayout, secret)
}

// userCollect: the user takes U_n alone once D2 blocks have passed.
func (v *Vault) userCollect(s *State) *wire.MsgTx {
	return spendLeaf(s.userOut(), s.uAmt, s.U, s.userLeaf, v.penaltyDelay, v.userKey, v.userPayout)
}

// publishState: the exchange adds its signature to its user-signed copy.
func (v *Vault) publishState(s *State) *wire.MsgTx {
	tx := s.tx.Copy()
	v.witnessBoth(tx, v.exitAmt, v.E, s.userPreSig)
	return tx
}

// userPublish: the user adds their signature to their exchange-signed copy.
func (v *Vault) userPublish(s *State) *wire.MsgTx {
	tx := s.utx.Copy()
	v.witnessLeaf(tx, v.exitAmt, v.E, v.multisigLeaf, nil, s.exchPreSig)
	return tx
}

func (v *Vault) cooperativeSettle() *wire.MsgTx {
	l := v.Latest()
	// Settling spends the vault directly with both signatures, so no old
	// state can be involved and each share goes straight to its owner.
	tx, _, _, _, _ := v.payoutTx(v.vaultOut, v.vaultAmt, l.UserSats, l.ExchSats, v.userPayout, v.exchPayout)
	v.witnessBoth(tx, v.vaultAmt, v.V, nil)
	return tx
}

// userClaim is the pre-signed pay-back of deposits, valid after the delay.
func (v *Vault) userClaim() *wire.MsgTx { return v.refundTx.Copy() }

// exchangeOnlySpend: the exchange signs alone and fakes the user's slot.
func (v *Vault) exchangeOnlySpend(prev wire.OutPoint, amt int64, out *tapOutput) *wire.MsgTx {
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(wire.NewTxIn(&prev, nil, nil))
	tx.AddTxOut(wire.NewTxOut(amt-txFee, v.exchPayout))
	exchSig := leafSig(tx, amt, out, v.multisigLeaf, v.exchKey)
	tx.TxIn[0].Witness = wire.TxWitness{exchSig, exchSig, v.multisigLeaf.Script, out.controlBlock(v.multisigLeaf)}
	return tx
}

// current reports whether a state pays out from the vault as it is now.
// States signed before a splice spend an exit output that can no longer
// exist, so they can't be published.
func (v *Vault) current(s *State) bool { return s.gen == v.gen }

// spliceTx is how the exchange adds coins to an open vault. One transaction,
// signed by both, spends the vault and some of the exchange's wallet coins
// into a bigger vault with the same scripts. The old vault output is gone, so
// the old exit request and every older state stop working. The exchange pays
// the fee from its own coins.
func (v *Vault) spliceTx(ins []utxo, exchScript []byte, exchWallet *btcec.PrivateKey, sats int64) (*wire.MsgTx, error) {
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(wire.NewTxIn(&v.vaultOut, nil, nil))
	prevs := map[wire.OutPoint]*wire.TxOut{v.vaultOut: wire.NewTxOut(v.vaultAmt, v.V.pkScript)}
	var in int64
	for _, u := range ins {
		op := u.op
		tx.AddTxIn(wire.NewTxIn(&op, nil, nil))
		prevs[op] = wire.NewTxOut(u.amt, exchScript)
		in += u.amt
	}
	tx.AddTxOut(wire.NewTxOut(v.vaultAmt+sats, v.V.pkScript))
	if change := in - sats - txFee; change > 546 {
		tx.AddTxOut(wire.NewTxOut(change, exchScript))
	}
	hashes := txscript.NewTxSigHashes(tx, txscript.NewMultiPrevOutFetcher(prevs))
	userSig, err := txscript.RawTxInTapscriptSignature(tx, hashes, 0, v.vaultAmt, v.V.pkScript, v.multisigLeaf, txscript.SigHashDefault, v.userKey)
	if err != nil {
		return nil, err
	}
	exchSig, err := txscript.RawTxInTapscriptSignature(tx, hashes, 0, v.vaultAmt, v.V.pkScript, v.multisigLeaf, txscript.SigHashDefault, v.exchKey)
	if err != nil {
		return nil, err
	}
	tx.TxIn[0].Witness = wire.TxWitness{exchSig, userSig, v.multisigLeaf.Script, v.V.controlBlock(v.multisigLeaf)}
	for i := 1; i < len(tx.TxIn); i++ {
		p := prevs[tx.TxIn[i].PreviousOutPoint]
		wit, err := txscript.TaprootWitnessSignature(tx, hashes, i, p.Value, p.PkScript, txscript.SigHashDefault, exchWallet)
		if err != nil {
			return nil, err
		}
		tx.TxIn[i].Witness = wit
	}
	return tx, nil
}

// applySplice moves the vault onto the splice output once it is accepted:
// the exchange's deposit grows, the exit request and pay-back are signed
// again, and older states can no longer be published.
func (v *Vault) applySplice(tx *wire.MsgTx, sats int64) {
	v.vaultOut = wire.OutPoint{Hash: tx.TxHash(), Index: 0}
	v.vaultAmt += sats
	v.exchDeposit += sats
	v.gen++
	v.presignExit()
}
