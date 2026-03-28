package parker

import (
	"encoding/json"
	"sort"
	"strings"

	storepkg "github.com/danieldresner/arkade_fun/internal/storage"
)

type transportStore struct {
	config      RuntimeConfig
	ownsRepo    bool
	profileName string
	paths       ProfileDaemonPaths
	repository  *storepkg.RuntimeRepository
}

func newTransportStore(profileName string, config RuntimeConfig) (*transportStore, error) {
	repository, err := storepkg.OpenRuntimeRepository(config, profileName)
	if err != nil {
		return nil, err
	}
	return newTransportStoreWithRepository(profileName, config, repository, true), nil
}

func newTransportStoreWithRepository(profileName string, config RuntimeConfig, repository *storepkg.RuntimeRepository, ownsRepo bool) *transportStore {
	return &transportStore{
		config:      config,
		ownsRepo:    ownsRepo,
		profileName: profileName,
		paths:       BuildProfileDaemonPaths(config.DaemonDir, profileName),
		repository:  repository,
	}
}

func (store *transportStore) close() error {
	if !store.ownsRepo {
		return nil
	}
	return store.repository.Close()
}

func (store *transportStore) readManifest() (*TransportPeerManifest, error) {
	data, err := store.repository.LoadTransportManifest()
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, nil
	}
	var manifest TransportPeerManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, err
	}
	return &manifest, nil
}

func (store *transportStore) writeManifest(manifest TransportPeerManifest) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return store.repository.SaveTransportManifest(data)
}

func (store *transportStore) listPeers() ([]TransportPeerSummary, error) {
	records, err := store.repository.ListTransportPeers()
	if err != nil {
		return nil, err
	}
	peers := make([]TransportPeerSummary, 0, len(records))
	for _, raw := range records {
		if len(raw) == 0 {
			continue
		}
		var peer TransportPeerSummary
		if err := json.Unmarshal(raw, &peer); err != nil {
			return nil, err
		}
		peers = append(peers, peer)
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].PeerID < peers[j].PeerID })
	return peers, nil
}

func (store *transportStore) writePeer(peer TransportPeerSummary) error {
	data, err := json.MarshalIndent(peer, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return store.repository.SaveTransportPeer(peer.PeerID, data)
}

func (store *transportStore) queueState() (TransportQueueState, error) {
	outbox, err := store.repository.ListTransportOutbox()
	if err != nil {
		return TransportQueueState{}, err
	}
	inbox, err := store.repository.ListTransportInbox()
	if err != nil {
		return TransportQueueState{}, err
	}
	dedupe, err := store.repository.ListTransportDedupe()
	if err != nil {
		return TransportQueueState{}, err
	}
	deadLetters, err := store.repository.ListTransportDeadLetters()
	if err != nil {
		return TransportQueueState{}, err
	}
	return TransportQueueState{
		DeadLetter: len(deadLetters),
		Dedupe:     len(dedupe),
		Inbox:      len(inbox),
		Outbox:     len(outbox),
	}, nil
}

func (store *transportStore) saveOutbox(envelope TransportEnvelope) error {
	data, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return store.repository.SaveTransportOutboxEntry(envelope.MessageID, data)
}

func (store *transportStore) saveInbox(envelope TransportEnvelope) error {
	data, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return store.repository.SaveTransportInboxEntry(envelope.MessageID, data)
}

func (store *transportStore) saveDedupe(record TransportDedupeRecord) error {
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return store.repository.SaveTransportDedupeEntry(record.SenderPeerID+":"+record.DedupeKey, data)
}

func (store *transportStore) saveDeadLetter(entry TransportDeadLetter) error {
	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return store.repository.SaveTransportDeadLetter(entry.Envelope.MessageID, data)
}
