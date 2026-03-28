package parker

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"strconv"
	"testing"
	"time"
)

func TestClassifyPeerTransportTarget(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name       string
		peerURL    string
		wantAddr   string
		wantOnion  bool
		wantUseTor bool
	}{
		{
			name:       "clear parker endpoint",
			peerURL:    "parker://127.0.0.1:9735",
			wantAddr:   "127.0.0.1:9735",
			wantOnion:  false,
			wantUseTor: false,
		},
		{
			name:       "onion parker endpoint",
			peerURL:    "parker://merchantabcdefghijklmnop.onion:9735",
			wantAddr:   "merchantabcdefghijklmnop.onion:9735",
			wantOnion:  true,
			wantUseTor: true,
		},
		{
			name:       "tor scheme forces socks",
			peerURL:    "tor://mesh.internal:9735",
			wantAddr:   "mesh.internal:9735",
			wantOnion:  false,
			wantUseTor: true,
		},
	}

	for _, testCase := range testCases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			target, err := classifyPeerTransportTarget(testCase.peerURL)
			if err != nil {
				t.Fatalf("classify peer target: %v", err)
			}
			if target.Address != testCase.wantAddr {
				t.Fatalf("expected address %q, got %q", testCase.wantAddr, target.Address)
			}
			if target.IsOnion != testCase.wantOnion {
				t.Fatalf("expected onion=%t, got %t", testCase.wantOnion, target.IsOnion)
			}
			if target.UseTor != testCase.wantUseTor {
				t.Fatalf("expected useTor=%t, got %t", testCase.wantUseTor, target.UseTor)
			}
		})
	}
}

func TestDialPeerTransportRejectsOnionWhenTorDisabled(t *testing.T) {
	t.Parallel()

	runtime := &meshRuntime{config: RuntimeConfig{UseTor: false}}
	if _, err := runtime.dialPeerTransport("parker://merchantabcdefghijklmnop.onion:9735"); err == nil {
		t.Fatal("expected onion dial to fail when Tor is disabled")
	}
}

func TestDialViaTorSOCKSUsesRemoteHostnameResolution(t *testing.T) {
	t.Parallel()

	socksAddr, seenTarget := startMockSOCKS5Server(t)
	connection, err := dialViaTorSOCKS(context.Background(), socksAddr, "merchantabcdefghijklmnop.onion:9735", time.Second)
	if err != nil {
		t.Fatalf("dial via socks: %v", err)
	}
	_ = connection.Close()

	select {
	case target := <-seenTarget:
		if target != "merchantabcdefghijklmnop.onion:9735" {
			t.Fatalf("expected SOCKS target to preserve onion hostname, got %q", target)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for SOCKS target")
	}
}

func TestDialViaTorSOCKSTimesOutWhenProxyDoesNotReply(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen mock socks server: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		defer connection.Close()
		time.Sleep(250 * time.Millisecond)
	}()

	started := time.Now()
	_, err = dialViaTorSOCKS(context.Background(), listener.Addr().String(), "merchantabcdefghijklmnop.onion:9735", 50*time.Millisecond)
	if err == nil {
		t.Fatal("expected Tor SOCKS dial to time out")
	}
	if time.Since(started) > 200*time.Millisecond {
		t.Fatalf("expected Tor SOCKS dial timeout to return promptly, took %s", time.Since(started))
	}
}

func startMockSOCKS5Server(t *testing.T) (string, <-chan string) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen mock socks server: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	seenTarget := make(chan string, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		defer connection.Close()

		header := make([]byte, 2)
		if _, err := io.ReadFull(connection, header); err != nil {
			return
		}
		methods := make([]byte, int(header[1]))
		if _, err := io.ReadFull(connection, methods); err != nil {
			return
		}
		if _, err := connection.Write([]byte{0x05, 0x00}); err != nil {
			return
		}

		requestHeader := make([]byte, 4)
		if _, err := io.ReadFull(connection, requestHeader); err != nil {
			return
		}

		host := ""
		switch requestHeader[3] {
		case 0x03:
			length := make([]byte, 1)
			if _, err := io.ReadFull(connection, length); err != nil {
				return
			}
			name := make([]byte, int(length[0]))
			if _, err := io.ReadFull(connection, name); err != nil {
				return
			}
			host = string(name)
		case 0x01:
			ip := make([]byte, 4)
			if _, err := io.ReadFull(connection, ip); err != nil {
				return
			}
			host = net.IP(ip).String()
		case 0x04:
			ip := make([]byte, 16)
			if _, err := io.ReadFull(connection, ip); err != nil {
				return
			}
			host = net.IP(ip).String()
		default:
			return
		}

		portBytes := make([]byte, 2)
		if _, err := io.ReadFull(connection, portBytes); err != nil {
			return
		}
		seenTarget <- net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(portBytes))))

		_, _ = connection.Write([]byte{0x05, 0x00, 0x00, 0x01, 127, 0, 0, 1, 0, 1})
	}()

	return listener.Addr().String(), seenTarget
}
