package parker

import "testing"

func TestTransportBootstrapAndState(t *testing.T) {
	config, err := ResolveRuntimeConfig(FlagMap{
		"datadir": t.TempDir(),
		"mock":    "true",
	})
	if err != nil {
		t.Fatalf("resolve runtime config: %v", err)
	}

	runtime, err := newDaemonRuntimeAdapter("alice", config, "player")
	if err != nil {
		t.Fatalf("new daemon runtime adapter: %v", err)
	}
	defer runtime.Close()

	bootstrap, err := runtime.Bootstrap("Alice")
	if err != nil {
		t.Fatalf("bootstrap runtime: %v", err)
	}
	transport := decodeMap(MustMarshalJSON(bootstrap["transport"]))
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
	if normalizeStateMap(state["transport"])["transportWireVersion"] != float64(transportWireVersion) {
		t.Fatalf("expected transport wire version %d", transportWireVersion)
	}
}

func TestTransportBootstrapPeerPersistsEndpoint(t *testing.T) {
	config, err := ResolveRuntimeConfig(FlagMap{
		"datadir": t.TempDir(),
		"mock":    "true",
	})
	if err != nil {
		t.Fatalf("resolve runtime config: %v", err)
	}

	remote, err := newDaemonRuntimeAdapter("bob", config, "player")
	if err != nil {
		t.Fatalf("new remote daemon runtime adapter: %v", err)
	}
	defer remote.Close()
	if _, err := remote.Bootstrap("Bob"); err != nil {
		t.Fatalf("bootstrap remote daemon runtime adapter: %v", err)
	}

	runtime, err := newDaemonRuntimeAdapter("alice", config, "player")
	if err != nil {
		t.Fatalf("new daemon runtime adapter: %v", err)
	}
	defer runtime.Close()
	if err := runtime.Start(); err != nil {
		t.Fatalf("start runtime: %v", err)
	}

	peerEndpoint := remote.inner.selfPeerURL()
	peer, err := runtime.BootstrapPeer(peerEndpoint, "Bob", []string{"witness"})
	if err != nil {
		t.Fatalf("bootstrap peer: %v", err)
	}
	summary, ok := peer.(TransportPeerSummary)
	if !ok {
		t.Fatalf("expected transport peer summary, received %T", peer)
	}
	if summary.Endpoint != peerEndpoint {
		t.Fatalf("expected endpoint to persist, received %q", summary.Endpoint)
	}

	peersValue, err := runtime.NetworkPeers()
	if err != nil {
		t.Fatalf("network peers: %v", err)
	}
	peers, ok := peersValue.([]TransportPeerSummary)
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

func TestTransportEndpointsForPeerURLOnlySetsDirectOnionForOnionEndpoints(t *testing.T) {
	t.Parallel()

	clear := transportEndpointsForPeerURL("parker://127.0.0.1:9735", []string{"https://mailbox.example"})
	if clear.Endpoint != "parker://127.0.0.1:9735" {
		t.Fatalf("expected clear endpoint to be preserved, got %q", clear.Endpoint)
	}
	if clear.DirectOnion != "" {
		t.Fatalf("expected clear endpoint to leave directOnion empty, got %q", clear.DirectOnion)
	}

	onion := transportEndpointsForPeerURL("parker://merchantabcdefghijklmnop.onion:9735", nil)
	if onion.DirectOnion != onion.Endpoint {
		t.Fatalf("expected onion endpoint to populate directOnion, got endpoint=%q directOnion=%q", onion.Endpoint, onion.DirectOnion)
	}
}
