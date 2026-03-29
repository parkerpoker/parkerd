package meshruntime

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/hex"
)

func randomX25519PrivateKeyHex() (string, error) {
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(key.Bytes()), nil
}

func x25519PublicKeyHex(privateKeyHex string) (string, error) {
	privateKeyBytes, err := hex.DecodeString(privateKeyHex)
	if err != nil {
		return "", err
	}
	privateKey, err := ecdh.X25519().NewPrivateKey(privateKeyBytes)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(privateKey.PublicKey().Bytes()), nil
}
