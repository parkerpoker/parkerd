package parker

import "testing"

func TestTransportV2BootstrapAndState(t *testing.T) {
	config, err := ResolveRuntimeConfig(FlagMap{
		"datadir":        t.TempDir(),
		"mock":           "true",
		"transport-mode": "v2",
	})
	if err != nil {
		t.Fatalf("resolve runtime config: %v", err)
	}

	runtime, err := newTransportV2Runtime("alice", config, "player")
	if err != nil {
		t.Fatalf("new transport v2 runtime: %v", err)
	}
	defer runtime.Close()

	bootstrap, err := runtime.Bootstrap("Alice")
	if err != nil {
		t.Fatalf("bootstrap runtime: %v", err)
	}
	transport := decodeMap(MustMarshalJSON(bootstrap["transport"]))
	if transport["transportMode"] != "v2" {
		t.Fatalf("expected transport mode v2, received %#v", transport["transportMode"])
	}

	peer := normalizeStateMap(transport["peer"])
	if stringValue(peer["peerId"]) == "" {
		t.Fatalf("expected peer id in bootstrap state")
	}
	if stringValue(peer["protocolId"]) == "" {
		t.Fatalf("expected protocol id in bootstrap state")
	}

	state, err := runtime.CurrentState()
	if err != nil {
		t.Fatalf("current state: %v", err)
	}
	if normalizeStateMap(state["transport"])["transportWireVersion"] != float64(transportV2WireVersion) {
		t.Fatalf("expected transport wire version %d", transportV2WireVersion)
	}
}

func TestTransportV2BootstrapPeerPersistsEndpoint(t *testing.T) {
	config, err := ResolveRuntimeConfig(FlagMap{
		"datadir":        t.TempDir(),
		"mock":           "true",
		"transport-mode": "v2",
	})
	if err != nil {
		t.Fatalf("resolve runtime config: %v", err)
	}

	runtime, err := newTransportV2Runtime("alice", config, "player")
	if err != nil {
		t.Fatalf("new transport v2 runtime: %v", err)
	}
	defer runtime.Close()
	if err := runtime.Start(); err != nil {
		t.Fatalf("start runtime: %v", err)
	}

	peer, err := runtime.BootstrapPeer("tor://peer.onion:9735", "Bob", []string{"witness"})
	if err != nil {
		t.Fatalf("bootstrap peer: %v", err)
	}
	summary, ok := peer.(TransportV2PeerSummary)
	if !ok {
		t.Fatalf("expected transport peer summary, received %T", peer)
	}
	if summary.Endpoint != "tor://peer.onion:9735" {
		t.Fatalf("expected endpoint to persist, received %q", summary.Endpoint)
	}

	peersValue, err := runtime.NetworkPeers()
	if err != nil {
		t.Fatalf("network peers: %v", err)
	}
	peers, ok := peersValue.([]TransportV2PeerSummary)
	if !ok {
		t.Fatalf("expected transport peer list, received %T", peersValue)
	}
	if len(peers) != 1 {
		t.Fatalf("expected 1 peer, received %d", len(peers))
	}
	if peers[0].Roles[0] != "witness" {
		t.Fatalf("expected witness role, received %#v", peers[0].Roles)
	}
}
