package parker

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	nativeProtocolVersion   = "poker/v1"
	nativeDealerMode        = "host-dealer-v1"
	nativeFundsProvider     = "arkade-table-funds/v1"
	nativeHostHeartbeatMS   = 1000
	nativeHostFailureMS     = 3500
	nativeNextHandDelayMS   = 1000
	nativePollIntervalMS    = 500
	nativeTableSyncInterval = 1 * time.Second
)

type nativeStore struct {
	config      RuntimeConfig
	profileName string
	paths       ProfileDaemonPaths
}

func newNativeStore(profileName string, config RuntimeConfig) *nativeStore {
	return &nativeStore{
		config:      config,
		profileName: profileName,
		paths:       BuildProfileDaemonPaths(config.DaemonDir, profileName),
	}
}

func (store *nativeStore) tableStatePath(tableID string) string {
	return filepath.Join(store.paths.StateDir, "tables", tableID, "state.json")
}

func (store *nativeStore) tableEventsPath(tableID string) string {
	return filepath.Join(store.paths.StateDir, "tables", tableID, "events.ndjson")
}

func (store *nativeStore) tableSnapshotsPath(tableID string) string {
	return filepath.Join(store.paths.StateDir, "tables", tableID, "snapshots.ndjson")
}

func (store *nativeStore) tablePrivateStatePath(tableID string) string {
	return filepath.Join(store.paths.StateDir, "tables", tableID, "private-state.json")
}

func (store *nativeStore) publicAdsPath() string {
	return filepath.Join(store.paths.StateDir, "public-ads.json")
}

func (store *nativeStore) tableFundsPath() string {
	return filepath.Join(store.config.DaemonDir, SlugProfile(store.profileName)+".table-funds.json")
}

func (store *nativeStore) ensureStateDir() error {
	return os.MkdirAll(store.paths.StateDir, 0o755)
}

func (store *nativeStore) readTable(tableID string) (*nativeTableState, error) {
	path := store.tableStatePath(tableID)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, nil
	}
	var table nativeTableState
	if err := json.Unmarshal(data, &table); err != nil {
		return nil, err
	}
	return &table, nil
}

func (store *nativeStore) writeTable(table *nativeTableState) error {
	if err := store.ensureStateDir(); err != nil {
		return err
	}
	path := store.tableStatePath(table.Config.TableID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(table, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := writeAtomicFile(path, data, 0o644); err != nil {
		return err
	}
	return nil
}

func (store *nativeStore) rewriteEvents(tableID string, events []NativeSignedTableEvent) error {
	path := store.tableEventsPath(tableID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	writer := bufio.NewWriter(file)
	for _, event := range events {
		data, err := json.Marshal(event)
		if err != nil {
			return err
		}
		if _, err := writer.Write(append(data, '\n')); err != nil {
			return err
		}
	}
	return writer.Flush()
}

func writeAtomicFile(path string, data []byte, mode os.FileMode) error {
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

func (store *nativeStore) rewriteSnapshots(tableID string, snapshots []NativeCooperativeTableSnapshot) error {
	path := store.tableSnapshotsPath(tableID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	writer := bufio.NewWriter(file)
	for _, snapshot := range snapshots {
		data, err := json.Marshal(snapshot)
		if err != nil {
			return err
		}
		if _, err := writer.Write(append(data, '\n')); err != nil {
			return err
		}
	}
	return writer.Flush()
}

func (store *nativeStore) writePrivateState(tableID string, profileState map[string]any) error {
	path := store.tablePrivateStatePath(tableID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(profileState, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

func (store *nativeStore) listTableIDs() ([]string, error) {
	root := filepath.Join(store.paths.StateDir, "tables")
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			ids = append(ids, entry.Name())
		}
	}
	return ids, nil
}

func (store *nativeStore) upsertPublicAd(ad NativeAdvertisement) error {
	current := map[string]NativeAdvertisement{}
	if data, err := os.ReadFile(store.publicAdsPath()); err == nil {
		_ = json.Unmarshal(data, &current)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	current[ad.TableID] = ad
	data, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(store.publicAdsPath()), 0o755); err != nil {
		return err
	}
	return os.WriteFile(store.publicAdsPath(), data, 0o644)
}

func (store *nativeStore) readPublicAds() (map[string]NativeAdvertisement, error) {
	data, err := os.ReadFile(store.publicAdsPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]NativeAdvertisement{}, nil
		}
		return nil, err
	}
	decoded := map[string]NativeAdvertisement{}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, err
	}
	return decoded, nil
}

func (store *nativeStore) readTableFunds() (NativeTableFundsState, error) {
	data, err := os.ReadFile(store.tableFundsPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return NativeTableFundsState{
				NetworkID: store.config.Network,
				Profile:   store.profileName,
				Tables:    map[string]NativeTableFundsEntry{},
			}, nil
		}
		return NativeTableFundsState{}, err
	}
	var funds NativeTableFundsState
	if err := json.Unmarshal(data, &funds); err != nil {
		return NativeTableFundsState{}, err
	}
	if funds.Tables == nil {
		funds.Tables = map[string]NativeTableFundsEntry{}
	}
	return funds, nil
}

func (store *nativeStore) writeTableFunds(state NativeTableFundsState) error {
	if state.Tables == nil {
		state.Tables = map[string]NativeTableFundsEntry{}
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(store.tableFundsPath(), data, 0o644)
}

func (store *nativeStore) withTableLock(tableID string, fn func() error) error {
	lockPath := store.tableStatePath(tableID) + ".lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return err
	}
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer func() {
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
	}()
	return fn()
}

func encodeInvite(tableID, networkID, hostPeerID, hostPeerURL string) (string, error) {
	payload := map[string]any{
		"hostPeerId":      hostPeerID,
		"hostPeerUrl":     hostPeerURL,
		"networkId":       networkID,
		"protocolVersion": nativeProtocolVersion,
		"tableId":         tableID,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func decodeInvite(invite string) (map[string]any, error) {
	data, err := base64.RawURLEncoding.DecodeString(invite)
	if err != nil {
		return nil, err
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, err
	}
	return decoded, nil
}

func peerHTTPBase(peerURL string) (string, error) {
	if strings.TrimSpace(peerURL) == "" {
		return "", fmt.Errorf("peer URL is required")
	}
	parsed, err := url.Parse(peerURL)
	if err != nil {
		return "", err
	}
	scheme := "http"
	switch parsed.Scheme {
	case "http", "https":
		scheme = parsed.Scheme
	case "ws":
		scheme = "http"
	case "wss":
		scheme = "https"
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("peer URL host is missing")
	}
	return (&url.URL{Scheme: scheme, Host: parsed.Host}).String(), nil
}

func nowISO() string {
	return time.Now().UTC().Format(time.RFC3339)
}

func addMillis(timestamp string, delta int) string {
	base, err := time.Parse(time.RFC3339, timestamp)
	if err != nil {
		base = time.Now().UTC()
	}
	return base.Add(time.Duration(delta) * time.Millisecond).UTC().Format(time.RFC3339)
}

func elapsedMillis(timestamp string) int64 {
	if timestamp == "" {
		return 1 << 62
	}
	value, err := time.Parse(time.RFC3339, timestamp)
	if err != nil {
		return 1 << 62
	}
	return time.Since(value).Milliseconds()
}

func cloneJSON[T any](input T) T {
	data, err := json.Marshal(input)
	if err != nil {
		panic(err)
	}
	var output T
	if err := json.Unmarshal(data, &output); err != nil {
		panic(err)
	}
	return output
}
