package settlementcore

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"reflect"
	"testing"

	secp256k1 "github.com/decred/dcrd/dcrec/secp256k1/v4"
)

func TestStableStringifyCanonicalizesStructuredData(t *testing.T) {
	got, err := StableStringify(map[string]any{
		"b": 2,
		"a": map[string]any{
			"z": []byte{0x0a, 0xff},
			"y": []any{"x", nil},
		},
		"c": math.NaN(),
		"d": []int{3, 1},
	})
	if err != nil {
		t.Fatalf("stable stringify: %v", err)
	}

	want := `{"a":{"y":["x",null],"z":"0aff"},"b":2,"c":null,"d":[3,1]}`
	if got != want {
		t.Fatalf("unexpected stable json\nwant: %s\ngot:  %s", want, got)
	}
}

func TestIdentityDerivationMatchesCompressedPublicKeyHash(t *testing.T) {
	seedHex := "11" + "22" + "33" + "44" + "55" + "66" + "77" + "88" + "99" + "aa" + "bb" + "cc" + "dd" + "ee" + "ff" + "00" +
		"11" + "22" + "33" + "44" + "55" + "66" + "77" + "88" + "99" + "aa" + "bb" + "cc" + "dd" + "ee" + "ff" + "00"

	identity, err := CreateLocalIdentity(seedHex)
	if err != nil {
		t.Fatalf("create local identity: %v", err)
	}

	seedBytes, err := hex.DecodeString(seedHex)
	if err != nil {
		t.Fatalf("decode seed: %v", err)
	}
	privateKey := secp256k1.PrivKeyFromBytes(seedBytes)
	publicKeyHex := hex.EncodeToString(privateKey.PubKey().SerializeCompressed())
	sum := sha256.Sum256(privateKey.PubKey().SerializeCompressed())
	wantPlayerID := "player-" + hex.EncodeToString(sum[:])[:20]

	if identity.PublicKeyHex != publicKeyHex {
		t.Fatalf("unexpected public key\nwant: %s\ngot:  %s", publicKeyHex, identity.PublicKeyHex)
	}
	if identity.PlayerID != wantPlayerID {
		t.Fatalf("unexpected player id\nwant: %s\ngot:  %s", wantPlayerID, identity.PlayerID)
	}

	protocolIdentity, err := CreateScopedIdentity(ProtocolIdentityScope, seedHex)
	if err != nil {
		t.Fatalf("create protocol identity: %v", err)
	}
	if protocolIdentity.ID[:6] != "proto-" {
		t.Fatalf("unexpected protocol prefix: %s", protocolIdentity.ID)
	}

	peerIdentity, err := CreateScopedIdentity(PeerIdentityScope, seedHex)
	if err != nil {
		t.Fatalf("create peer identity: %v", err)
	}
	if peerIdentity.ID[:5] != "peer-" {
		t.Fatalf("unexpected peer prefix: %s", peerIdentity.ID)
	}
}

func TestSigningHelpersAreDeterministicAndVerifiable(t *testing.T) {
	identity, err := CreateLocalIdentity("aa" + "bb" + "cc" + "dd" + "ee" + "ff" + "00" + "11" + "22" + "33" + "44" + "55" + "66" + "77" + "88" + "99" +
		"aa" + "bb" + "cc" + "dd" + "ee" + "ff" + "00" + "11" + "22" + "33" + "44" + "55" + "66" + "77" + "88" + "99")
	if err != nil {
		t.Fatalf("create local identity: %v", err)
	}

	messageSignature, err := SignMessage(identity.PrivateKeyHex, "hello, parker")
	if err != nil {
		t.Fatalf("sign message: %v", err)
	}
	ok, err := VerifyMessage(identity.PublicKeyHex, "hello, parker", messageSignature)
	if err != nil {
		t.Fatalf("verify message: %v", err)
	}
	if !ok {
		t.Fatal("message signature did not verify")
	}

	payload := map[string]any{
		"b": 2,
		"a": "one",
	}
	first, err := SignStructuredData(identity.PrivateKeyHex, payload)
	if err != nil {
		t.Fatalf("sign structured data: %v", err)
	}
	second, err := SignStructuredData(identity.PrivateKeyHex, map[string]any{
		"a": "one",
		"b": 2,
	})
	if err != nil {
		t.Fatalf("sign structured data: %v", err)
	}
	if first != second {
		t.Fatalf("structured signatures should be deterministic\nfirst:  %s\nsecond: %s", first, second)
	}
	ok, err = VerifyStructuredData(identity.PublicKeyHex, payload, first)
	if err != nil {
		t.Fatalf("verify structured data: %v", err)
	}
	if !ok {
		t.Fatal("structured signature did not verify")
	}
}

func TestIdentityBindingRoundTrip(t *testing.T) {
	wallet, err := CreateLocalIdentity("11" + "22" + "33" + "44" + "55" + "66" + "77" + "88" + "99" + "aa" + "bb" + "cc" + "dd" + "ee" + "ff" + "00" +
		"11" + "22" + "33" + "44" + "55" + "66" + "77" + "88" + "99" + "aa" + "bb" + "cc" + "dd" + "ee" + "ff" + "00")
	if err != nil {
		t.Fatalf("create wallet identity: %v", err)
	}
	protocol, err := CreateScopedIdentity(ProtocolIdentityScope, "33"+"44"+"55"+"66"+"77"+"88"+"99"+"aa"+"bb"+"cc"+"dd"+"ee"+"ff"+"00"+"11"+"22"+
		"33"+"44"+"55"+"66"+"77"+"88"+"99"+"aa"+"bb"+"cc"+"dd"+"ee"+"ff"+"00"+"11")
	if err != nil {
		t.Fatalf("create protocol identity: %v", err)
	}

	binding, err := BuildIdentityBinding("table-123", "peer-abc", protocol, wallet, "2026-03-22T12:34:56.000Z")
	if err != nil {
		t.Fatalf("build identity binding: %v", err)
	}

	ok, err := VerifyIdentityBinding(binding)
	if err != nil {
		t.Fatalf("verify identity binding: %v", err)
	}
	if !ok {
		t.Fatal("binding did not verify")
	}

	clone := binding
	clone.PeerID = "peer-def"
	ok, err = VerifyIdentityBinding(clone)
	if err != nil {
		t.Fatalf("verify mutated binding: %v", err)
	}
	if ok {
		t.Fatal("mutated binding unexpectedly verified")
	}
}

func TestCanonicalizeMatchesExpectedGoShapes(t *testing.T) {
	canonical := CanonicalizeStructuredData(struct {
		A []byte
		B map[string]any
	}{
		A: []byte{0xde, 0xad},
		B: map[string]any{
			"z": 1,
			"y": true,
		},
	})

	want := map[string]any{
		"A": "dead",
		"B": map[string]any{
			"y": true,
			"z": int64(1),
		},
	}
	if !reflect.DeepEqual(canonical, want) {
		t.Fatalf("unexpected canonical structure\nwant: %#v\ngot:  %#v", want, canonical)
	}
}
