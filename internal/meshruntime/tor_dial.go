package meshruntime

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const peerTransportDialTimeout = 5 * time.Second

type peerTransportTarget struct {
	Address string
	Host    string
	IsOnion bool
	Scheme  string
	UseTor  bool
}

func classifyPeerTransportTarget(peerURL string) (peerTransportTarget, error) {
	if strings.TrimSpace(peerURL) == "" {
		return peerTransportTarget{}, fmt.Errorf("peer endpoint is required")
	}
	parsed, err := url.Parse(peerURL)
	if err != nil {
		return peerTransportTarget{}, err
	}
	switch parsed.Scheme {
	case "parker", "tcp", "tor", "":
	default:
		return peerTransportTarget{}, fmt.Errorf("unsupported peer endpoint scheme %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return peerTransportTarget{}, fmt.Errorf("peer endpoint host is missing")
	}
	host, _, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		return peerTransportTarget{}, err
	}
	isOnion := isOnionHost(host)
	return peerTransportTarget{
		Address: parsed.Host,
		Host:    host,
		IsOnion: isOnion,
		Scheme:  parsed.Scheme,
		UseTor:  isOnion || parsed.Scheme == "tor",
	}, nil
}

func isOnionPeerURL(peerURL string) bool {
	target, err := classifyPeerTransportTarget(peerURL)
	return err == nil && target.IsOnion
}

func isOnionHost(host string) bool {
	host = strings.TrimSpace(strings.Trim(host, "[]"))
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	return strings.HasSuffix(host, ".onion") && host != ".onion"
}

func (runtime *meshRuntime) dialPeerTransport(peerURL string) (net.Conn, error) {
	target, err := classifyPeerTransportTarget(peerURL)
	if err != nil {
		return nil, err
	}
	if target.UseTor {
		if !runtime.config.UseTor {
			return nil, fmt.Errorf("peer endpoint %s requires Tor but PARKER_USE_TOR is disabled", peerURL)
		}
		return dialViaTorSOCKS(context.Background(), runtime.config.TorSocksAddr, target.Address, peerTransportDialTimeout)
	}
	return net.DialTimeout("tcp", target.Address, peerTransportDialTimeout)
}

func dialViaTorSOCKS(ctx context.Context, socksAddr, address string, timeout time.Duration) (net.Conn, error) {
	if strings.TrimSpace(socksAddr) == "" {
		return nil, fmt.Errorf("tor SOCKS address is required")
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline && timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	connection, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", socksAddr)
	if err != nil {
		return nil, fmt.Errorf("connect Tor SOCKS proxy at %s: %w", socksAddr, err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		if err := connection.SetDeadline(deadline); err != nil {
			_ = connection.Close()
			return nil, err
		}
	}
	if err := writeSOCKS5Greeting(connection); err != nil {
		_ = connection.Close()
		return nil, err
	}
	if err := writeSOCKS5Connect(connection, address); err != nil {
		_ = connection.Close()
		return nil, err
	}
	_ = connection.SetDeadline(time.Time{})
	return connection, nil
}

func writeSOCKS5Greeting(connection net.Conn) error {
	if _, err := connection.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		return fmt.Errorf("write Tor SOCKS greeting: %w", err)
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(connection, reply); err != nil {
		return fmt.Errorf("read Tor SOCKS greeting reply: %w", err)
	}
	if reply[0] != 0x05 {
		return fmt.Errorf("unexpected Tor SOCKS version %d", reply[0])
	}
	if reply[1] != 0x00 {
		return fmt.Errorf("Tor SOCKS proxy does not allow no-auth method (0x%02x)", reply[1])
	}
	return nil
}

func writeSOCKS5Connect(connection net.Conn, address string) error {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("invalid Tor SOCKS target port %q", portText)
	}

	addressType, encodedHost, err := encodeSOCKS5Address(host)
	if err != nil {
		return err
	}
	request := make([]byte, 0, 4+len(encodedHost)+2)
	request = append(request, 0x05, 0x01, 0x00, addressType)
	request = append(request, encodedHost...)
	request = binary.BigEndian.AppendUint16(request, uint16(port))
	if _, err := connection.Write(request); err != nil {
		return fmt.Errorf("write Tor SOCKS connect request: %w", err)
	}

	reply := make([]byte, 4)
	if _, err := io.ReadFull(connection, reply); err != nil {
		return fmt.Errorf("read Tor SOCKS connect reply: %w", err)
	}
	if reply[0] != 0x05 {
		return fmt.Errorf("unexpected Tor SOCKS connect version %d", reply[0])
	}
	if reply[1] != 0x00 {
		return fmt.Errorf("Tor SOCKS connect failed: %s", socks5ReplyText(reply[1]))
	}
	if err := discardSOCKS5Address(connection, reply[3]); err != nil {
		return err
	}
	return nil
}

func encodeSOCKS5Address(host string) (byte, []byte, error) {
	host = strings.TrimSpace(strings.Trim(host, "[]"))
	if ip := net.ParseIP(host); ip != nil {
		if ipv4 := ip.To4(); ipv4 != nil {
			return 0x01, ipv4, nil
		}
		if ipv6 := ip.To16(); ipv6 != nil {
			return 0x04, ipv6, nil
		}
	}
	if len(host) == 0 || len(host) > 255 {
		return 0, nil, fmt.Errorf("invalid Tor SOCKS hostname %q", host)
	}
	encoded := make([]byte, 1+len(host))
	encoded[0] = byte(len(host))
	copy(encoded[1:], host)
	return 0x03, encoded, nil
}

func discardSOCKS5Address(reader io.Reader, addressType byte) error {
	size := 0
	switch addressType {
	case 0x01:
		size = net.IPv4len
	case 0x04:
		size = net.IPv6len
	case 0x03:
		length := make([]byte, 1)
		if _, err := io.ReadFull(reader, length); err != nil {
			return fmt.Errorf("read Tor SOCKS domain length: %w", err)
		}
		size = int(length[0])
	default:
		return fmt.Errorf("unexpected Tor SOCKS address type 0x%02x", addressType)
	}

	if size > 0 {
		buffer := make([]byte, size)
		if _, err := io.ReadFull(reader, buffer); err != nil {
			return fmt.Errorf("read Tor SOCKS bound address: %w", err)
		}
	}
	portBytes := make([]byte, 2)
	if _, err := io.ReadFull(reader, portBytes); err != nil {
		return fmt.Errorf("read Tor SOCKS bound port: %w", err)
	}
	return nil
}

func socks5ReplyText(code byte) string {
	switch code {
	case 0x01:
		return "general server failure"
	case 0x02:
		return "connection not allowed by ruleset"
	case 0x03:
		return "network unreachable"
	case 0x04:
		return "host unreachable"
	case 0x05:
		return "connection refused"
	case 0x06:
		return "TTL expired"
	case 0x07:
		return "command not supported"
	case 0x08:
		return "address type not supported"
	default:
		return fmt.Sprintf("unknown reply 0x%02x", code)
	}
}
