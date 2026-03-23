package wallet

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

const (
	profileLoadRetries = 3
	profileRetryDelay  = 25 * time.Millisecond
)

type ProfileStore struct {
	profileDir string
}

func NewProfileStore(profileDir string) *ProfileStore {
	return &ProfileStore{profileDir: profileDir}
}

func (store *ProfileStore) Load(profileName string) (*PlayerProfileState, error) {
	path := store.pathFor(profileName)
	for attempt := 0; attempt < profileLoadRetries; attempt++ {
		data, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, nil
			}
			return nil, err
		}
		if len(data) == 0 {
			if attempt < profileLoadRetries-1 {
				time.Sleep(profileRetryDelay)
				continue
			}
			return nil, nil
		}

		var state PlayerProfileState
		if err := json.Unmarshal(data, &state); err != nil {
			if attempt < profileLoadRetries-1 {
				time.Sleep(profileRetryDelay)
				continue
			}
			return nil, err
		}
		return &state, nil
	}
	return nil, nil
}

func (store *ProfileStore) Save(state PlayerProfileState) error {
	if err := os.MkdirAll(store.profileDir, 0o755); err != nil {
		return err
	}

	path := store.pathFor(state.ProfileName)
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	if err := writeFileAtomic(path, data, 0o644); err != nil {
		return err
	}
	return nil
}

func (store *ProfileStore) pathFor(profileName string) string {
	return filepath.Join(store.profileDir, profileName+".json")
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
