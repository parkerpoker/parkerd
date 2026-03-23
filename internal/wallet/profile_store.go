package wallet

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
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

func (store *ProfileStore) ListProfileNames() ([]string, error) {
	entries, err := os.ReadDir(store.profileDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		names = append(names, strings.TrimSuffix(entry.Name(), ".json"))
	}
	sort.Strings(names)
	return names, nil
}

func (store *ProfileStore) LoadSummary(profileName string) (*LocalProfileSummary, error) {
	state, err := store.Load(profileName)
	if err != nil || state == nil {
		return nil, err
	}
	summary := ToLocalProfileSummary(*state)
	return &summary, nil
}

func (store *ProfileStore) ListProfiles() ([]LocalProfileSummary, error) {
	names, err := store.ListProfileNames()
	if err != nil {
		return nil, err
	}

	summaries := make([]LocalProfileSummary, 0, len(names))
	for _, name := range names {
		state, err := store.Load(name)
		if err != nil {
			return nil, err
		}
		if state == nil {
			continue
		}
		summaries = append(summaries, ToLocalProfileSummary(*state))
	}
	return summaries, nil
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

func ToLocalProfileSummary(profile PlayerProfileState) LocalProfileSummary {
	summary := LocalProfileSummary{
		HasPeerIdentity:     profile.PeerPrivateKeyHex != "",
		HasProtocolIdentity: profile.ProtocolPrivateKeyHex != "",
		HasWalletIdentity:   profile.WalletPrivateKeyHex != "",
		KnownPeerCount:      len(profile.KnownPeers),
		MeshTableCount:      len(profile.MeshTables),
		Nickname:            profile.Nickname,
		ProfileName:         profile.ProfileName,
	}
	if profile.CurrentMeshTableID != "" {
		summary.CurrentMeshTableID = profile.CurrentMeshTableID
	}
	if profile.CurrentTable != nil {
		summary.CurrentTableID = profile.CurrentTable.TableID
	}
	return summary
}
