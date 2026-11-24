package main

// MuSig2 (BIP 327) for the Bitcoin and Litecoin vaults.
//
// New vaults combine the user's key and the exchange's key into one MuSig2
// key and use it as the vault output's Taproot internal key, instead of the
// "nothing up my sleeve" key that disabled the key path. The scripts are
// unchanged and still there as hidden backup paths (exit request, pay-back,
// revocation). Only the two cooperative spends of the vault output use the
// key path:
//
//   - settling with the exchange, and
//   - a splice (the exchange adding buying capacity).
//
// On the chain those look like an ordinary single-signature Taproot payment:
// one 64-byte signature, no script, no second key. They still need both
// sides: the combined signature can only be made from both partial
// signatures.
//
// Signing takes the same number of messages as before. The wallet sends a
// fresh public nonce; the exchange answers with its public nonce and its
// partial signature; the wallet checks that partial signature, adds its own
// and checks the final signature before using it.
//
// Nonces: a MuSig2 secret nonce must never sign twice (that can reveal the
// private key). Each signing gets a new random nonce (musigNonce), its secret
// half is wiped as soon as it has signed, and secret nonces are never saved:
// if a program stops halfway, that attempt is thrown away and started again
// with new nonces.
//
// Test networks only. Not audited.

import (
	"bytes"
	"encoding/hex"
	"errors"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// musigKeys parses the two x-only vault keys. x-only keys stand for the point
// with an even y, which is what both sides combine.
func musigKeys(user, exch []byte) ([]*btcec.PublicKey, error) {
	u, err := schnorr.ParsePubKey(user)
	if err != nil {
		return nil, err
	}
	x, err := schnorr.ParsePubKey(exch)
	if err != nil {
		return nil, err
	}
	return []*btcec.PublicKey{u, x}, nil
}

// musigInternal is the combined key (before the Taproot tweak), used as the
// vault output's internal key.
func musigInternal(user, exch []byte) (*btcec.PublicKey, error) {
	keys, err := musigKeys(user, exch)
	if err != nil {
		return nil, err
	}
	agg, _, _, err := musig2.AggregateKeys(keys, true)
	if err != nil {
		return nil, err
	}
	return agg.PreTweakedKey, nil
}

// evenKey is k, or its negation, whichever has the public key with an even
// y. Both have the same x-only public key and make the same Schnorr
// signatures, but MuSig2 needs the private key that matches the even point.
func evenKey(k *btcec.PrivateKey) *btcec.PrivateKey {
	if k.PubKey().SerializeCompressed()[0] == 0x02 {
		return k
	}
	s := k.Key
	s.Negate()
	return btcec.PrivKeyFromScalar(&s)
}

// musigNonce is one signer's nonce for one signing. Its secret half signs
// once and is then wiped.
type musigNonce struct {
	n    *musig2.Nonces
	used bool
}

func newMusigNonce(k *btcec.PrivateKey) (*musigNonce, error) {
	n, err := musig2.GenNonces(musig2.WithPublicKey(evenKey(k).PubKey()))
	if err != nil {
		return nil, err
	}
	return &musigNonce{n: n}, nil
}

func (m *musigNonce) pub() []byte { return append([]byte(nil), m.n.PubNonce[:]...) }

// keySpend is what a key-path signature on input idx covers: the output's
// keys, its script tree root and the transaction's signature hash.
type keySpend struct {
	keys []*btcec.PublicKey
	root []byte
	msg  [32]byte
	out  []byte // the output key (x-only), which the final signature must verify against
}

func newKeySpend(user, exch []byte, out *tapOutput, tx *wire.MsgTx, idx int, fetch txscript.PrevOutputFetcher) (*keySpend, error) {
	if out.internal == numsKey {
		return nil, errors.New("this vault has no MuSig2 key path")
	}
	keys, err := musigKeys(user, exch)
	if err != nil {
		return nil, err
	}
	h, err := txscript.CalcTaprootSignatureHash(txscript.NewTxSigHashes(tx, fetch), txscript.SigHashDefault, tx, idx, fetch)
	if err != nil {
		return nil, err
	}
	ks := &keySpend{keys: keys, root: out.root(), out: out.pkScript[2:]}
	copy(ks.msg[:], h)
	return ks, nil
}

func pubNonce(b []byte) ([musig2.PubNonceSize]byte, error) {
	var n [musig2.PubNonceSize]byte
	if len(b) != musig2.PubNonceSize {
		return n, errors.New("a MuSig2 nonce must be 66 bytes")
	}
	copy(n[:], b)
	return n, nil
}

// sign makes this side's partial signature with its nonce and the other
// side's public nonce. The nonce can't sign again afterwards.
func (ks *keySpend) sign(k *btcec.PrivateKey, mine *musigNonce, theirs []byte) (*musig2.PartialSignature, error) {
	if mine == nil || mine.used {
		return nil, errors.New("this nonce has already signed; a new one is needed")
	}
	other, err := pubNonce(theirs)
	if err != nil {
		return nil, err
	}
	agg, err := musig2.AggregateNonces([][musig2.PubNonceSize]byte{mine.n.PubNonce, other})
	if err != nil {
		return nil, err
	}
	mine.used = true
	defer func() { mine.n.SecNonce = [musig2.SecNonceSize]byte{} }()
	return musig2.Sign(mine.n.SecNonce, evenKey(k), agg, ks.keys, ks.msg,
		musig2.WithSortedKeys(), musig2.WithTaprootSignTweak(ks.root))
}

// verifyPartial checks the other side's partial signature (32 bytes).
func (ks *keySpend) verifyPartial(sig []byte, signer []byte, theirNonce, myNonce []byte) (*musig2.PartialSignature, bool) {
	if len(sig) != 32 {
		return nil, false
	}
	var ps musig2.PartialSignature
	if ps.Decode(bytes.NewReader(sig)) != nil {
		return nil, false
	}
	tn, err := pubNonce(theirNonce)
	if err != nil {
		return nil, false
	}
	mn, err := pubNonce(myNonce)
	if err != nil {
		return nil, false
	}
	agg, err := musig2.AggregateNonces([][musig2.PubNonceSize]byte{mn, tn})
	if err != nil {
		return nil, false
	}
	pk, err := schnorr.ParsePubKey(signer)
	if err != nil {
		return nil, false
	}
	return &ps, ps.Verify(tn, agg, ks.keys, pk, ks.msg, musig2.WithSortedKeys(), musig2.WithTaprootSignTweak(ks.root))
}

// combine makes the final 64-byte signature from both partial signatures
// and checks it against the vault's output key.
func (ks *keySpend) combine(mine, theirs *musig2.PartialSignature) ([]byte, error) {
	sig := musig2.CombineSigs(mine.R, []*musig2.PartialSignature{mine, theirs},
		musig2.WithTaprootTweakedCombine(ks.msg, ks.keys, ks.root, true))
	pk, err := schnorr.ParsePubKey(ks.out)
	if err != nil {
		return nil, err
	}
	if !sig.Verify(ks.msg[:], pk) {
		return nil, errors.New("the combined MuSig2 signature doesn't verify")
	}
	return sig.Serialize(), nil
}

func encodePartial(ps *musig2.PartialSignature) []byte {
	var b bytes.Buffer
	ps.Encode(&b)
	return b.Bytes()
}

// musigBoth signs a key-path spend with both keys at once (the one-program
// console holds both).
func musigBoth(ks *keySpend, userKey, exchKey *btcec.PrivateKey) ([]byte, error) {
	un, err := newMusigNonce(userKey)
	if err != nil {
		return nil, err
	}
	xn, err := newMusigNonce(exchKey)
	if err != nil {
		return nil, err
	}
	upub, xpub := un.pub(), xn.pub()
	us, err := ks.sign(userKey, un, xpub)
	if err != nil {
		return nil, err
	}
	xs, err := ks.sign(exchKey, xn, upub)
	if err != nil {
		return nil, err
	}
	return ks.combine(us, xs)
}

// newVaultsMuSig2 is whether the exchange opens new vaults with the MuSig2
// key path. Tests switch it off to act like an exchange from before musig.go.
var newVaultsMuSig2 = true

// keyPathNote describes the MuSig2 key path for the page's list of the
// vault's spending paths (nil if the vault has none).
func keyPathNote(out *tapOutput) []Labelled {
	if !out.keyPath() {
		return nil
	}
	return []Labelled{{"Settling and adding capacity: your key and the exchange's combined into one (MuSig2). It needs both of you, but on the chain it looks like an ordinary single-key payment, and the scripts below stay hidden",
		"key path: one signature for the combined key " + hex.EncodeToString(schnorr.SerializePubKey(out.internal))[:16] + "…"}}
}
