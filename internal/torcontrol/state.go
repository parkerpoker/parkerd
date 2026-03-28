package torcontrol

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

type HiddenServiceState struct {
	CreatedAt   string `json:"createdAt,omitempty"`
	Hostname    string `json:"hostname,omitempty"`
	PrivateKey  string `json:"privateKey"`
	ServiceID   string `json:"serviceId,omitempty"`
	VirtualPort int    `json:"virtualPort,omitempty"`
}

func LoadHiddenServiceState(path string) (*HiddenServiceState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	var state HiddenServiceState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

func SaveHiddenServiceState(path string, state HiddenServiceState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeFileAtomic(path, data, 0o600)
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tempFile, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tempPath := tempFile.Name()
	defer os.Remove(tempPath)

	if _, err := tempFile.Write(data); err != nil {
		tempFile.Close()
		return err
	}
	if err := tempFile.Chmod(mode); err != nil {
		tempFile.Close()
		return err
	}
	if err := tempFile.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}
