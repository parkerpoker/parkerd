package meshruntime

import (
	"net"
	"testing"

	cfg "github.com/parkerpoker/parkerd/internal/config"
)

func TestHiddenServiceTargetAddressUsesOverrideHost(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	runtime := &meshRuntime{
		config: cfg.RuntimeConfig{
			PeerHost:      "0.0.0.0",
			TorTargetHost: "host.docker.internal",
		},
		listener: listener,
	}

	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	want := net.JoinHostPort("host.docker.internal", port)
	if got := runtime.hiddenServiceTargetAddressLocked(); got != want {
		t.Fatalf("expected hidden service target %q, got %q", want, got)
	}
}
