package parker

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	storepkg "github.com/danieldresner/arkade_fun/internal/storage"
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
	repository  *storepkg.RuntimeRepository
}

func newNativeStore(profileName string, config RuntimeConfig) (*nativeStore, error) {
	repository, err := storepkg.OpenRuntimeRepository(config, profileName)
	if err != nil {
		return nil, err
	}
	return &nativeStore{
		config:      config,
		profileName: profileName,
		paths:       BuildProfileDaemonPaths(config.DaemonDir, profileName),
		repository:  repository,
	}, nil
}

func (store *nativeStore) close() error {
	return store.repository.Close()
}

func (store *nativeStore) readTable(tableID string) (*nativeTableState, error) {
	data, err := store.repository.LoadTableState(tableID)
	if err != nil {
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
	data, err := json.MarshalIndent(table, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return store.repository.SaveTableState(table.Config.TableID, data)
}

func (store *nativeStore) rewriteEvents(tableID string, events []NativeSignedTableEvent) error {
	encoded := make([][]byte, 0, len(events))
	for _, event := range events {
		data, err := json.Marshal(event)
		if err != nil {
			return err
		}
		encoded = append(encoded, data)
	}
	return store.repository.ReplaceEvents(tableID, encoded)
}

func (store *nativeStore) rewriteSnapshots(tableID string, snapshots []NativeCooperativeTableSnapshot) error {
	encoded := make([][]byte, 0, len(snapshots))
	for _, snapshot := range snapshots {
		data, err := json.Marshal(snapshot)
		if err != nil {
			return err
		}
		encoded = append(encoded, data)
	}
	return store.repository.ReplaceSnapshots(tableID, encoded)
}

func (store *nativeStore) writePrivateState(tableID string, profileState map[string]any) error {
	data, err := json.MarshalIndent(profileState, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return store.repository.SavePrivateState(tableID, data)
}

func (store *nativeStore) listTableIDs() ([]string, error) {
	return store.repository.ListTableIDs()
}

func (store *nativeStore) upsertPublicAd(ad NativeAdvertisement) error {
	data, err := json.MarshalIndent(ad, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return store.repository.UpsertPublicAd(ad.TableID, data)
}

func (store *nativeStore) readPublicAds() (map[string]NativeAdvertisement, error) {
	records, err := store.repository.LoadPublicAds()
	if err != nil {
		return nil, err
	}
	decoded := make(map[string]NativeAdvertisement, len(records))
	for tableID, raw := range records {
		if len(raw) == 0 {
			continue
		}
		var ad NativeAdvertisement
		if err := json.Unmarshal(raw, &ad); err != nil {
			return nil, err
		}
		decoded[tableID] = ad
	}
	return decoded, nil
}

func (store *nativeStore) readTableFunds() (NativeTableFundsState, error) {
	data, err := store.repository.LoadTableFunds()
	if err != nil {
		return NativeTableFundsState{}, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return NativeTableFundsState{
			NetworkID: store.config.Network,
			Profile:   store.profileName,
			Tables:    map[string]NativeTableFundsEntry{},
		}, nil
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
	return store.repository.SaveTableFunds(data)
}

func (store *nativeStore) withTableLock(tableID string, fn func() error) error {
	return store.repository.WithTableLock(tableID, fn)
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
