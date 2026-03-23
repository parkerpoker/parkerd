package settlementcore

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
	"time"

	secp256k1 "github.com/decred/dcrd/dcrec/secp256k1/v4"
	secp256k1ecdsa "github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

const (
	jsTimestampLayout = "2006-01-02T15:04:05.000Z"
)

type IdentityScope string

const (
	WalletIdentityScope   IdentityScope = "wallet"
	ProtocolIdentityScope IdentityScope = "protocol"
	PeerIdentityScope     IdentityScope = "peer"
)

type LocalIdentity struct {
	PlayerID      string
	PrivateKeyHex string
	PublicKeyHex  string
}

type ScopedIdentity struct {
	ID            string
	PrivateKeyHex string
	PublicKeyHex  string
	Scope         IdentityScope
}

type IdentityBinding struct {
	PeerID            string `json:"peerId"`
	ProtocolID        string `json:"protocolId"`
	ProtocolPubkeyHex string `json:"protocolPubkeyHex"`
	SignedAt          string `json:"signedAt"`
	SignatureHex      string `json:"signatureHex"`
	TableID           string `json:"tableId"`
	WalletPlayerID    string `json:"walletPlayerId"`
	WalletPubkeyHex   string `json:"walletPubkeyHex"`
}

func CanonicalizeStructuredData(input any) any {
	return canonicalizeValue(reflect.ValueOf(input))
}

func StableStringify(input any) (string, error) {
	canonical := CanonicalizeStructuredData(input)
	data, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func HashStructuredDataHex(input any) (string, error) {
	canonical, err := StableStringify(input)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:]), nil
}

func HashCheckpoint(checkpoint any) (string, error) {
	return HashStructuredDataHex(checkpoint)
}

func HashMessage(message string) []byte {
	sum := sha256.Sum256([]byte(message))
	return sum[:]
}

func DeriveScopedID(scope IdentityScope, publicKeyHex string) (string, error) {
	if scope != WalletIdentityScope && scope != ProtocolIdentityScope && scope != PeerIdentityScope {
		return "", fmt.Errorf("unsupported identity scope %q", scope)
	}

	publicKeyBytes, err := hex.DecodeString(publicKeyHex)
	if err != nil {
		return "", fmt.Errorf("decode public key: %w", err)
	}

	sum := sha256.Sum256(publicKeyBytes)
	return fmt.Sprintf("%s-%s", scopePrefix(scope), hex.EncodeToString(sum[:])[:20]), nil
}

func CreateLocalIdentity(seedHex string) (LocalIdentity, error) {
	privateKeyHex, _, publicKeyHex, err := privateKeyAndPublicKeyHex(seedHex)
	if err != nil {
		return LocalIdentity{}, err
	}

	playerID, err := DerivePlayerID(publicKeyHex)
	if err != nil {
		return LocalIdentity{}, err
	}

	return LocalIdentity{
		PlayerID:      playerID,
		PrivateKeyHex: privateKeyHex,
		PublicKeyHex:  publicKeyHex,
	}, nil
}

func CreateScopedIdentity(scope IdentityScope, seedHex string) (ScopedIdentity, error) {
	privateKeyHex, _, publicKeyHex, err := privateKeyAndPublicKeyHex(seedHex)
	if err != nil {
		return ScopedIdentity{}, err
	}

	id, err := DeriveScopedID(scope, publicKeyHex)
	if err != nil {
		return ScopedIdentity{}, err
	}

	return ScopedIdentity{
		ID:            id,
		PrivateKeyHex: privateKeyHex,
		PublicKeyHex:  publicKeyHex,
		Scope:         scope,
	}, nil
}

func DerivePlayerID(publicKeyHex string) (string, error) {
	publicKeyBytes, err := hex.DecodeString(publicKeyHex)
	if err != nil {
		return "", fmt.Errorf("decode public key: %w", err)
	}
	sum := sha256.Sum256(publicKeyBytes)
	return fmt.Sprintf("player-%s", hex.EncodeToString(sum[:])[:20]), nil
}

func SignMessage(privateKeyHex string, message string) (string, error) {
	return signCompactHex(privateKeyHex, HashMessage(message))
}

func VerifyMessage(publicKeyHex string, message string, signatureHex string) (bool, error) {
	publicKeyBytes, err := hex.DecodeString(publicKeyHex)
	if err != nil {
		return false, fmt.Errorf("decode public key: %w", err)
	}
	pubKey, err := secp256k1.ParsePubKey(publicKeyBytes)
	if err != nil {
		return false, err
	}

	signatureBytes, err := hex.DecodeString(signatureHex)
	if err != nil {
		return false, fmt.Errorf("decode signature: %w", err)
	}
	sig, err := parseCompactSignature(signatureBytes)
	if err != nil {
		return false, err
	}
	return sig.Verify(HashMessage(message), pubKey), nil
}

func SignStructuredData(privateKeyHex string, input any) (string, error) {
	stable, err := StableStringify(input)
	if err != nil {
		return "", err
	}
	return signCompactHex(privateKeyHex, HashMessage(stable))
}

func VerifyStructuredData(publicKeyHex string, input any, signatureHex string) (bool, error) {
	stable, err := StableStringify(input)
	if err != nil {
		return false, err
	}

	publicKeyBytes, err := hex.DecodeString(publicKeyHex)
	if err != nil {
		return false, fmt.Errorf("decode public key: %w", err)
	}
	pubKey, err := secp256k1.ParsePubKey(publicKeyBytes)
	if err != nil {
		return false, err
	}

	signatureBytes, err := hex.DecodeString(signatureHex)
	if err != nil {
		return false, fmt.Errorf("decode signature: %w", err)
	}
	sig, err := parseCompactSignature(signatureBytes)
	if err != nil {
		return false, err
	}
	return sig.Verify(HashMessage(stable), pubKey), nil
}

func BuildIdentityBinding(tableID, peerID string, protocolIdentity ScopedIdentity, walletIdentity LocalIdentity, signedAt string) (IdentityBinding, error) {
	if signedAt == "" {
		signedAt = nowISO()
	}

	unsigned := map[string]any{
		"peerId":            peerID,
		"protocolId":        protocolIdentity.ID,
		"protocolPubkeyHex": protocolIdentity.PublicKeyHex,
		"signedAt":          signedAt,
		"tableId":           tableID,
		"walletPlayerId":    walletIdentity.PlayerID,
		"walletPubkeyHex":   walletIdentity.PublicKeyHex,
	}

	signatureHex, err := SignStructuredData(walletIdentity.PrivateKeyHex, unsigned)
	if err != nil {
		return IdentityBinding{}, err
	}

	return IdentityBinding{
		PeerID:            peerID,
		ProtocolID:        protocolIdentity.ID,
		ProtocolPubkeyHex: protocolIdentity.PublicKeyHex,
		SignedAt:          signedAt,
		SignatureHex:      signatureHex,
		TableID:           tableID,
		WalletPlayerID:    walletIdentity.PlayerID,
		WalletPubkeyHex:   walletIdentity.PublicKeyHex,
	}, nil
}

func VerifyIdentityBinding(binding IdentityBinding) (bool, error) {
	unsigned := map[string]any{
		"peerId":            binding.PeerID,
		"protocolId":        binding.ProtocolID,
		"protocolPubkeyHex": binding.ProtocolPubkeyHex,
		"signedAt":          binding.SignedAt,
		"tableId":           binding.TableID,
		"walletPlayerId":    binding.WalletPlayerID,
		"walletPubkeyHex":   binding.WalletPubkeyHex,
	}
	return VerifyStructuredData(binding.WalletPubkeyHex, unsigned, binding.SignatureHex)
}

func RandomHex(byteLength int) (string, error) {
	if byteLength < 0 {
		return "", fmt.Errorf("byte length must be non-negative")
	}
	bytes := make([]byte, byteLength)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

func canonicalizeValue(value reflect.Value) any {
	if !value.IsValid() {
		return nil
	}

	for value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return nil
		}
		value = value.Elem()
	}

	if value.Type() == reflect.TypeOf(time.Time{}) {
		return value.Interface().(time.Time).UTC().Truncate(time.Millisecond).Format(jsTimestampLayout)
	}

	if value.Type() == reflect.TypeOf(json.Number("")) {
		return json.Number(value.Interface().(json.Number).String())
	}

	switch value.Kind() {
	case reflect.String:
		return value.String()
	case reflect.Bool:
		return value.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return value.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return value.Uint()
	case reflect.Float32, reflect.Float64:
		number := value.Float()
		if math.IsNaN(number) || math.IsInf(number, 0) {
			return nil
		}
		return number
	case reflect.Slice:
		if value.IsNil() {
			return nil
		}
		if value.Type().Elem().Kind() == reflect.Uint8 {
			return hex.EncodeToString(value.Bytes())
		}
		items := make([]any, value.Len())
		for index := range items {
			items[index] = canonicalizeValue(value.Index(index))
		}
		return items
	case reflect.Array:
		if value.Type().Elem().Kind() == reflect.Uint8 {
			bytes := make([]byte, value.Len())
			for index := range bytes {
				bytes[index] = byte(value.Index(index).Uint())
			}
			return hex.EncodeToString(bytes)
		}
		items := make([]any, value.Len())
		for index := range items {
			items[index] = canonicalizeValue(value.Index(index))
		}
		return items
	case reflect.Map:
		if value.IsNil() {
			return nil
		}
		keys := value.MapKeys()
		sort.Slice(keys, func(left, right int) bool {
			return keyToString(keys[left]) < keyToString(keys[right])
		})
		output := make(map[string]any, len(keys))
		for _, key := range keys {
			output[keyToString(key)] = canonicalizeValue(value.MapIndex(key))
		}
		return output
	case reflect.Struct:
		return canonicalizeStruct(value)
	default:
		if value.CanInterface() {
			if marshaler, ok := value.Interface().(json.Marshaler); ok {
				if data, err := marshaler.MarshalJSON(); err == nil {
					var decoded any
					if err := json.Unmarshal(data, &decoded); err == nil {
						return canonicalizeValue(reflect.ValueOf(decoded))
					}
				}
			}
		}
	}

	return nil
}

func canonicalizeStruct(value reflect.Value) any {
	if value.Type() == reflect.TypeOf(time.Time{}) {
		return value.Interface().(time.Time).UTC().Truncate(time.Millisecond).Format(jsTimestampLayout)
	}

	output := map[string]any{}
	typ := value.Type()
	for index := 0; index < value.NumField(); index++ {
		fieldType := typ.Field(index)
		if fieldType.PkgPath != "" {
			continue
		}

		name, omitEmpty, skip := parseJSONTag(fieldType.Tag.Get("json"))
		if skip {
			continue
		}
		if name == "" {
			name = fieldType.Name
		}

		fieldValue := value.Field(index)
		if omitEmpty && fieldValue.IsZero() {
			continue
		}
		output[name] = canonicalizeValue(fieldValue)
	}
	return output
}

func parseJSONTag(tag string) (name string, omitEmpty bool, skip bool) {
	if tag == "" {
		return "", false, false
	}

	parts := strings.Split(tag, ",")
	if len(parts) > 0 {
		name = parts[0]
	}
	for _, part := range parts[1:] {
		if part == "omitempty" {
			omitEmpty = true
		}
	}
	if name == "-" {
		return "", false, true
	}
	return name, omitEmpty, false
}

func keyToString(value reflect.Value) string {
	if value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return ""
		}
		value = value.Elem()
	}
	if value.Kind() == reflect.String {
		return value.String()
	}
	return fmt.Sprint(value.Interface())
}

func scopePrefix(scope IdentityScope) string {
	switch scope {
	case WalletIdentityScope:
		return "player"
	case ProtocolIdentityScope:
		return "proto"
	case PeerIdentityScope:
		return "peer"
	default:
		return string(scope)
	}
}

func privateKeyAndPublicKeyHex(seedHex string) (string, *secp256k1.PrivateKey, string, error) {
	if seedHex == "" {
		randomSeed, err := RandomHex(32)
		if err != nil {
			return "", nil, "", err
		}
		seedHex = randomSeed
	}

	seedBytes, err := hex.DecodeString(seedHex)
	if err != nil {
		return "", nil, "", fmt.Errorf("decode private key: %w", err)
	}

	privateKey := secp256k1.PrivKeyFromBytes(seedBytes)
	publicKeyHex := hex.EncodeToString(privateKey.PubKey().SerializeCompressed())
	return seedHex, privateKey, publicKeyHex, nil
}

func signCompactHex(privateKeyHex string, hash []byte) (string, error) {
	_, privateKey, _, err := privateKeyAndPublicKeyHex(privateKeyHex)
	if err != nil {
		return "", err
	}

	signature := secp256k1ecdsa.Sign(privateKey, hash)
	if signature == nil {
		return "", errors.New("failed to sign message")
	}

	var compact [64]byte
	var r, s [32]byte
	rScalar := signature.R()
	sScalar := signature.S()
	rScalar.PutBytes(&r)
	sScalar.PutBytes(&s)
	copy(compact[0:32], r[:])
	copy(compact[32:64], s[:])
	return hex.EncodeToString(compact[:]), nil
}

func parseCompactSignature(signature []byte) (*secp256k1ecdsa.Signature, error) {
	if len(signature) != 64 {
		return nil, fmt.Errorf("invalid compact signature length %d", len(signature))
	}

	var r, s secp256k1.ModNScalar
	if overflow := r.SetByteSlice(signature[:32]); overflow || r.IsZero() {
		return nil, errors.New("invalid signature r value")
	}
	if overflow := s.SetByteSlice(signature[32:]); overflow || s.IsZero() {
		return nil, errors.New("invalid signature s value")
	}
	return secp256k1ecdsa.NewSignature(&r, &s), nil
}

func nowISO() string {
	return time.Now().UTC().Truncate(time.Millisecond).Format(jsTimestampLayout)
}
