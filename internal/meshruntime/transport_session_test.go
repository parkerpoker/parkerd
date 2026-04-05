package meshruntime

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	transportpkg "github.com/parkerpoker/parkerd/internal/transport"
)

func closeSessionManagersForTest(t *testing.T, runtimes ...*meshRuntime) {
	t.Helper()
	t.Cleanup(func() {
		for _, runtime := range runtimes {
			if runtime == nil {
				continue
			}
			runtime.closeSessionManager()
		}
	})
}

func TestExchangePeerTransportV3UsesVersion3EndToEnd(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")
	closeSessionManagersForTest(t, host, guest)

	peerInfo, err := guest.fetchPeerInfo(host.selfPeerURL())
	if err != nil {
		t.Fatalf("fetch host peer info: %v", err)
	}
	request, requestKey, err := guest.newOutboundEnvelope(
		nativeTransportMessagePeerProbe,
		nativeTransportChannelDiscovery,
		"",
		"",
		map[string]any{},
		"",
	)
	if err != nil {
		t.Fatalf("build v3 peer probe: %v", err)
	}

	response, err := guest.exchangePeerTransportV3(host.selfPeerURL(), peerInfo, request)
	if err != nil {
		t.Fatalf("send v3 peer probe: %v", err)
	}
	if response.TransportWireVersion != transportpkg.WireVersion {
		t.Fatalf("response wire version: got %d, want %d", response.TransportWireVersion, transportpkg.WireVersion)
	}

	body, err := guest.decodeResponseEnvelope(response, requestKey)
	if err != nil {
		t.Fatalf("decode v3 peer probe response: %v", err)
	}
	var manifest nativePeerSelf
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatalf("unmarshal peer manifest: %v", err)
	}
	if manifest.TransportWireVersion != transportpkg.WireVersion {
		t.Fatalf("manifest wire version: got %d, want %d", manifest.TransportWireVersion, transportpkg.WireVersion)
	}
}

func TestExchangePeerTransportV3RejectsWireDowngrade(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")
	closeSessionManagersForTest(t, host, guest)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	staticPrivBytes, err := hex.DecodeString(host.transportPrivate)
	if err != nil {
		t.Fatalf("decode host transport private key: %v", err)
	}
	staticPubBytes, err := hex.DecodeString(host.transportPublic)
	if err != nil {
		t.Fatalf("decode host transport public key: %v", err)
	}

	serverErr := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()

		preface := make([]byte, transportpkg.V3PrefaceLen())
		if _, err := io.ReadFull(conn, preface); err != nil {
			serverErr <- err
			return
		}
		if !transportpkg.IsV3Preface(preface) {
			serverErr <- fmt.Errorf("unexpected v3 preface %q", string(preface))
			return
		}
		noise, err := transportpkg.AcceptV3Session(conn, staticPrivBytes, staticPubBytes, 5*time.Second)
		if err != nil {
			serverErr <- err
			return
		}
		request, err := transportpkg.ReadV3Request(conn, noise, 5*time.Second)
		if err != nil {
			serverErr <- err
			return
		}
		if request.TransportWireVersion != transportpkg.WireVersion {
			serverErr <- fmt.Errorf("request wire version: got %d, want %d", request.TransportWireVersion, transportpkg.WireVersion)
			return
		}

		body, err := json.Marshal(host.self)
		if err != nil {
			serverErr <- err
			return
		}
		// Intentionally sign the response as v2 to verify the client rejects the downgrade.
		response, err := host.buildEnvelope(nativeTransportMessagePeerManifest, nativeTransportChannelDiscovery, "", "", body, nil, request.MessageID)
		if err != nil {
			serverErr <- err
			return
		}
		if err := transportpkg.WriteV3Response(conn, noise, response, 5*time.Second); err != nil {
			serverErr <- err
			return
		}
		serverErr <- nil
	}()

	request, _, err := guest.newOutboundEnvelope(
		nativeTransportMessagePeerProbe,
		nativeTransportChannelDiscovery,
		"",
		"",
		map[string]any{},
		"",
	)
	if err != nil {
		t.Fatalf("build v3 peer probe: %v", err)
	}

	peerURL := "parker://" + listener.Addr().String()
	_, err = guest.exchangePeerTransportV3(peerURL, host.self, request)
	if err == nil {
		t.Fatal("expected wire downgrade error")
	}
	if !transportpkg.IsWireDowngrade(err) {
		t.Fatalf("expected wire downgrade error, got %v", err)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("downgrade test server: %v", err)
	}
}

func TestHandleV3PeerTransportConnectionDoesNotBlockConcurrentRequests(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")
	closeSessionManagersForTest(t, host, guest)

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)

	peerInfo, err := guest.fetchPeerInfo(host.selfPeerURL())
	if err != nil {
		t.Fatalf("fetch host peer info: %v", err)
	}

	setupRequest, setupKey, err := guest.newOutboundEnvelope(
		nativeTransportMessagePeerProbe,
		nativeTransportChannelDiscovery,
		"",
		"",
		map[string]any{},
		"",
	)
	if err != nil {
		t.Fatalf("build setup probe: %v", err)
	}
	setupResponse, err := guest.exchangePeerTransportV3(host.selfPeerURL(), peerInfo, setupRequest)
	if err != nil {
		t.Fatalf("send setup probe: %v", err)
	}
	if _, err := guest.decodeResponseEnvelope(setupResponse, setupKey); err != nil {
		t.Fatalf("decode setup probe response: %v", err)
	}

	slowBody, err := guest.buildTableFetchRequest(tableID)
	if err != nil {
		t.Fatalf("build slow fetch request body: %v", err)
	}
	slowRequest, slowKey, err := guest.newOutboundEnvelope(
		nativeTransportMessageTablePull,
		nativeTransportChannelTable,
		tableID,
		peerInfo.Peer.PeerID,
		slowBody,
		peerInfo.TransportPubkeyHex,
	)
	if err != nil {
		t.Fatalf("build slow fetch request: %v", err)
	}
	fastRequest, fastKey, err := guest.newOutboundEnvelope(
		nativeTransportMessagePeerProbe,
		nativeTransportChannelDiscovery,
		"",
		"",
		map[string]any{},
		"",
	)
	if err != nil {
		t.Fatalf("build fast probe request: %v", err)
	}

	lockHeld := make(chan struct{})
	releaseLock := make(chan struct{})
	lockErr := make(chan error, 1)
	go func() {
		lockErr <- host.store.withTableLock(tableID, func() error {
			close(lockHeld)
			<-releaseLock
			return nil
		})
	}()
	<-lockHeld

	slowDone := make(chan error, 1)
	go func() {
		response, err := guest.exchangePeerTransportV3(host.selfPeerURL(), peerInfo, slowRequest)
		if err == nil {
			_, err = guest.decodeResponseEnvelope(response, slowKey)
		}
		slowDone <- err
	}()

	fastDone := make(chan error, 1)
	go func() {
		response, err := guest.exchangePeerTransportV3(host.selfPeerURL(), peerInfo, fastRequest)
		if err == nil {
			_, err = guest.decodeResponseEnvelope(response, fastKey)
		}
		fastDone <- err
	}()

	select {
	case err := <-fastDone:
		if err != nil {
			close(releaseLock)
			<-lockErr
			t.Fatalf("fast request failed: %v", err)
		}
	case <-time.After(1500 * time.Millisecond):
		close(releaseLock)
		<-lockErr
		t.Fatal("fast request blocked behind slow request on the same v3 session")
	}

	select {
	case err := <-slowDone:
		close(releaseLock)
		<-lockErr
		t.Fatalf("slow request completed before releasing the table lock: %v", err)
	default:
	}

	close(releaseLock)
	if err := <-lockErr; err != nil {
		t.Fatalf("release table lock: %v", err)
	}
	if err := <-slowDone; err != nil {
		t.Fatalf("slow request failed after releasing the table lock: %v", err)
	}
}

func TestCachePeerInfoInvalidatesCanonicalSessionOnAliasKeyRotation(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")
	closeSessionManagersForTest(t, host, guest)

	canonicalURL := host.selfPeerURL()
	aliasURL := strings.Replace(canonicalURL, "parker://", "tcp://", 1)
	if aliasURL == canonicalURL {
		t.Fatalf("expected alias URL to differ from canonical URL %q", canonicalURL)
	}

	peerInfo, err := guest.fetchPeerInfo(aliasURL)
	if err != nil {
		t.Fatalf("fetch host peer info through alias: %v", err)
	}
	request, requestKey, err := guest.newOutboundEnvelope(
		nativeTransportMessagePeerProbe,
		nativeTransportChannelDiscovery,
		"",
		"",
		map[string]any{},
		"",
	)
	if err != nil {
		t.Fatalf("build canonical v3 probe: %v", err)
	}
	response, err := guest.exchangePeerTransportV3(canonicalURL, peerInfo, request)
	if err != nil {
		t.Fatalf("send canonical v3 probe: %v", err)
	}
	if _, err := guest.decodeResponseEnvelope(response, requestKey); err != nil {
		t.Fatalf("decode canonical v3 probe response: %v", err)
	}

	sm := guest.ensureSessionManager()
	if !sm.HasSession(canonicalURL) {
		t.Fatalf("expected canonical session for %s to exist", canonicalURL)
	}

	rotated := peerInfo
	rotated.TransportPubkeyHex = guest.transportPublic
	guest.cachePeerInfo(aliasURL, rotated)

	if sm.HasSession(canonicalURL) {
		t.Fatalf("expected canonical session for %s to be invalidated by alias key rotation", canonicalURL)
	}
	if sm.HasSession(aliasURL) {
		t.Fatalf("expected alias session for %s to be invalidated by key rotation", aliasURL)
	}
}
