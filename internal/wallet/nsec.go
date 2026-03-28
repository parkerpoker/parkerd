package wallet

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/btcsuite/btcd/btcutil/bech32"
)

const walletNsecMismatchError = "walletNsec does not match the existing profile wallet key"

func decodeWalletNsec(walletNsec string) (string, error) {
	walletNsec = strings.TrimSpace(walletNsec)
	if walletNsec == "" {
		return "", errors.New("walletNsec is required")
	}

	hrp, data, err := bech32.DecodeToBase256(walletNsec)
	if err != nil {
		return "", fmt.Errorf("invalid walletNsec: %w", err)
	}
	if hrp != "nsec" {
		return "", errors.New("invalid walletNsec: expected nsec prefix")
	}
	if len(data) != 32 {
		return "", fmt.Errorf("invalid walletNsec: expected 32-byte private key, received %d bytes", len(data))
	}
	return hex.EncodeToString(data), nil
}

func encodeWalletNsec(privateKeyHex string) (string, error) {
	privateKeyHex = strings.TrimSpace(privateKeyHex)
	if privateKeyHex == "" {
		return "", errors.New("wallet private key is required")
	}

	privateKey, err := hex.DecodeString(privateKeyHex)
	if err != nil {
		return "", fmt.Errorf("encode wallet private key: %w", err)
	}
	if len(privateKey) != 32 {
		return "", fmt.Errorf("encode wallet private key: expected 32 bytes, received %d bytes", len(privateKey))
	}
	return bech32.EncodeFromBase256("nsec", privateKey)
}

func resolveWalletPrivateKeyHex(state PlayerProfileState, walletNsec string) (string, error) {
	existingWalletHex := strings.TrimSpace(state.WalletPrivateKeyHex)
	if existingWalletHex == "" {
		existingWalletHex = strings.TrimSpace(state.PrivateKeyHex)
	}

	requestedWalletHex := ""
	if strings.TrimSpace(walletNsec) != "" {
		var err error
		requestedWalletHex, err = decodeWalletNsec(walletNsec)
		if err != nil {
			return "", err
		}
	}

	switch {
	case requestedWalletHex != "" && existingWalletHex != "" && !strings.EqualFold(existingWalletHex, requestedWalletHex):
		return "", errors.New(walletNsecMismatchError)
	case requestedWalletHex != "":
		return requestedWalletHex, nil
	default:
		return existingWalletHex, nil
	}
}
