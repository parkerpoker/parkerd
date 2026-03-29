package parker

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"

	storepkg "github.com/parkerpoker/parkerd/internal/storage"
)

const (
	nativeProtocolVersion       = "poker/v1"
	nativeDealerMode            = "dealerless-transcript-v1"
	nativeFundsProvider         = "arkade-table-funds/v1"
	nativeHostHeartbeatMS       = 1000
	nativeHostFailureMS         = 3500
	nativeHandProtocolTimeoutMS = 1500
	nativeNextHandDelayMS       = 1000
	nativePollIntervalMS        = 500
	nativeTableSyncInterval     = 1 * time.Second
)

type meshStore struct {
	config      RuntimeConfig
	profileName string
	paths       ProfileDaemonPaths
	repository  *storepkg.RuntimeRepository
}

func newMeshStore(profileName string, config RuntimeConfig) (*meshStore, error) {
	repository, err := storepkg.OpenRuntimeRepository(config, profileName)
	if err != nil {
		return nil, err
	}
	return &meshStore{
		config:      config,
		profileName: profileName,
		paths:       BuildProfileDaemonPaths(config.DaemonDir, profileName),
		repository:  repository,
	}, nil
}

func (store *meshStore) close() error {
	return store.repository.Close()
}

func (store *meshStore) readTable(tableID string) (*nativeTableState, error) {
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

func (store *meshStore) writeTable(table *nativeTableState) error {
	data, err := json.MarshalIndent(table, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return store.repository.SaveTableState(table.Config.TableID, data)
}

func (store *meshStore) rewriteEvents(tableID string, events []NativeSignedTableEvent) error {
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

func (store *meshStore) rewriteSnapshots(tableID string, snapshots []NativeCooperativeTableSnapshot) error {
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

func (store *meshStore) writePrivateState(tableID string, profileState map[string]any) error {
	data, err := json.MarshalIndent(profileState, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return store.repository.SavePrivateState(tableID, data)
}

func (store *meshStore) listTableIDs() ([]string, error) {
	return store.repository.ListTableIDs()
}

func (store *meshStore) upsertPublicAd(ad NativeAdvertisement) error {
	data, err := json.MarshalIndent(ad, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return store.repository.UpsertPublicAd(ad.TableID, data)
}

func (store *meshStore) readPublicAds() (map[string]NativeAdvertisement, error) {
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

func (store *meshStore) readTableFunds() (NativeTableFundsState, error) {
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

func (store *meshStore) writeTableFunds(state NativeTableFundsState) error {
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

func (store *meshStore) withTableLock(tableID string, fn func() error) error {
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
