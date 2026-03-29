package meshruntime

import (
	"encoding/json"
	"sort"
	"strings"

	cfg "github.com/parkerpoker/parkerd/internal/config"
	storepkg "github.com/parkerpoker/parkerd/internal/storage"
	transportpkg "github.com/parkerpoker/parkerd/internal/transport"
)

type TransportStore struct {
	config      cfg.RuntimeConfig
	ownsRepo    bool
	profileName string
	paths       cfg.ProfileDaemonPaths
	repository  *storepkg.RuntimeRepository
}

func NewTransportStore(profileName string, config cfg.RuntimeConfig) (*TransportStore, error) {
	repository, err := storepkg.OpenRuntimeRepository(config, profileName)
	if err != nil {
		return nil, err
	}
	return NewTransportStoreWithRepository(profileName, config, repository, true), nil
}

func NewTransportStoreWithRepository(profileName string, config cfg.RuntimeConfig, repository *storepkg.RuntimeRepository, ownsRepo bool) *TransportStore {
	return &TransportStore{
		config:      config,
		ownsRepo:    ownsRepo,
		profileName: profileName,
		paths:       cfg.BuildProfileDaemonPaths(config.DaemonDir, profileName),
		repository:  repository,
	}
}

func (store *TransportStore) Close() error {
	if !store.ownsRepo {
		return nil
	}
	return store.repository.Close()
}

func (store *TransportStore) ReadManifest() (*transportpkg.TransportPeerManifest, error) {
	data, err := store.repository.LoadTransportManifest()
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, nil
	}
	var manifest transportpkg.TransportPeerManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, err
	}
	return &manifest, nil
}

func (store *TransportStore) WriteManifest(manifest transportpkg.TransportPeerManifest) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return store.repository.SaveTransportManifest(data)
}

func (store *TransportStore) ListPeers() ([]transportpkg.TransportPeerSummary, error) {
	records, err := store.repository.ListTransportPeers()
	if err != nil {
		return nil, err
	}
	peers := make([]transportpkg.TransportPeerSummary, 0, len(records))
	for _, raw := range records {
		if len(raw) == 0 {
			continue
		}
		var peer transportpkg.TransportPeerSummary
		if err := json.Unmarshal(raw, &peer); err != nil {
			return nil, err
		}
		peers = append(peers, peer)
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].PeerID < peers[j].PeerID })
	return peers, nil
}

func (store *TransportStore) WritePeer(peer transportpkg.TransportPeerSummary) error {
	data, err := json.MarshalIndent(peer, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return store.repository.SaveTransportPeer(peer.PeerID, data)
}

func (store *TransportStore) QueueState() (transportpkg.TransportQueueState, error) {
	outbox, err := store.repository.ListTransportOutbox()
	if err != nil {
		return transportpkg.TransportQueueState{}, err
	}
	inbox, err := store.repository.ListTransportInbox()
	if err != nil {
		return transportpkg.TransportQueueState{}, err
	}
	dedupe, err := store.repository.ListTransportDedupe()
	if err != nil {
		return transportpkg.TransportQueueState{}, err
	}
	deadLetters, err := store.repository.ListTransportDeadLetters()
	if err != nil {
		return transportpkg.TransportQueueState{}, err
	}
	return transportpkg.TransportQueueState{
		DeadLetter: len(deadLetters),
		Dedupe:     len(dedupe),
		Inbox:      len(inbox),
		Outbox:     len(outbox),
	}, nil
}

func (store *TransportStore) SaveOutbox(envelope transportpkg.TransportEnvelope) error {
	data, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return store.repository.SaveTransportOutboxEntry(envelope.MessageID, data)
}

func (store *TransportStore) SaveInbox(envelope transportpkg.TransportEnvelope) error {
	data, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return store.repository.SaveTransportInboxEntry(envelope.MessageID, data)
}

func (store *TransportStore) SaveDedupe(record transportpkg.TransportDedupeRecord) error {
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return store.repository.SaveTransportDedupeEntry(record.SenderPeerID+":"+record.DedupeKey, data)
}

func (store *TransportStore) SaveDeadLetter(entry transportpkg.TransportDeadLetter) error {
	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return store.repository.SaveTransportDeadLetter(entry.Envelope.MessageID, data)
}
