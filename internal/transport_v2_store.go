package parker

import (
	"encoding/json"
	"sort"
	"strings"

	storepkg "github.com/danieldresner/arkade_fun/internal/storage"
)

type transportV2Store struct {
	config      RuntimeConfig
	profileName string
	paths       ProfileDaemonPaths
	repository  *storepkg.RuntimeRepository
}

func newTransportV2Store(profileName string, config RuntimeConfig) (*transportV2Store, error) {
	repository, err := storepkg.OpenRuntimeRepository(config, profileName)
	if err != nil {
		return nil, err
	}
	return &transportV2Store{
		config:      config,
		profileName: profileName,
		paths:       BuildProfileDaemonPaths(config.DaemonDir, profileName),
		repository:  repository,
	}, nil
}

func (store *transportV2Store) close() error {
	return store.repository.Close()
}

func (store *transportV2Store) readManifest() (*TransportV2PeerManifest, error) {
	data, err := store.repository.LoadTransportManifest()
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, nil
	}
	var manifest TransportV2PeerManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, err
	}
	return &manifest, nil
}

func (store *transportV2Store) writeManifest(manifest TransportV2PeerManifest) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return store.repository.SaveTransportManifest(data)
}

func (store *transportV2Store) listPeers() ([]TransportV2PeerSummary, error) {
	records, err := store.repository.ListTransportPeers()
	if err != nil {
		return nil, err
	}
	peers := make([]TransportV2PeerSummary, 0, len(records))
	for _, raw := range records {
		if len(raw) == 0 {
			continue
		}
		var peer TransportV2PeerSummary
		if err := json.Unmarshal(raw, &peer); err != nil {
			return nil, err
		}
		peers = append(peers, peer)
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].PeerID < peers[j].PeerID })
	return peers, nil
}

func (store *transportV2Store) writePeer(peer TransportV2PeerSummary) error {
	data, err := json.MarshalIndent(peer, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return store.repository.SaveTransportPeer(peer.PeerID, data)
}

func (store *transportV2Store) queueState() (TransportV2QueueState, error) {
	outbox, err := store.repository.ListTransportOutbox()
	if err != nil {
		return TransportV2QueueState{}, err
	}
	inbox, err := store.repository.ListTransportInbox()
	if err != nil {
		return TransportV2QueueState{}, err
	}
	dedupe, err := store.repository.ListTransportDedupe()
	if err != nil {
		return TransportV2QueueState{}, err
	}
	deadLetters, err := store.repository.ListTransportDeadLetters()
	if err != nil {
		return TransportV2QueueState{}, err
	}
	return TransportV2QueueState{
		DeadLetter: len(deadLetters),
		Dedupe:     len(dedupe),
		Inbox:      len(inbox),
		Outbox:     len(outbox),
	}, nil
}

func (store *transportV2Store) saveOutbox(envelope TransportV2Envelope) error {
	data, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return store.repository.SaveTransportOutboxEntry(envelope.MessageID, data)
}

func (store *transportV2Store) saveInbox(envelope TransportV2Envelope) error {
	data, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return store.repository.SaveTransportInboxEntry(envelope.MessageID, data)
}

func (store *transportV2Store) saveDedupe(record TransportV2DedupeRecord) error {
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return store.repository.SaveTransportDedupeEntry(record.SenderPeerID+":"+record.DedupeKey, data)
}

func (store *transportV2Store) saveDeadLetter(entry TransportV2DeadLetter) error {
	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return store.repository.SaveTransportDeadLetter(entry.Envelope.MessageID, data)
}
