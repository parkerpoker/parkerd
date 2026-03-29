package parker

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/parkerpoker/parkerd/internal/torcontrol"
)

type runtimeHiddenService struct {
	Hostname      string
	PrivateKey    string
	ServiceID     string
	TargetAddress string
	VirtualPort   int
}

func (runtime *meshRuntime) ensureAdvertisedPeerURLLocked() error {
	runtime.clearPeerURL = runtime.clearPeerURLLocked()
	if runtime.config.UseTor && runtime.listener != nil && runtime.torService == nil {
		if err := runtime.registerTorHiddenServiceLocked(); err != nil {
			return err
		}
	}
	if runtime.config.UseTor && runtime.torService != nil {
		runtime.self.Peer.PeerURL = fmt.Sprintf("parker://%s:%d", runtime.torService.Hostname, runtime.torService.VirtualPort)
		return nil
	}
	runtime.self.Peer.PeerURL = runtime.clearPeerURL
	return nil
}

func (runtime *meshRuntime) clearPeerURLLocked() string {
	host := listenerAdvertiseHost(runtime.listener, runtime.config.PeerHost)
	port := listenerAdvertisePort(runtime.listener, runtime.config.PeerPort)
	if host == "" || port <= 0 {
		return ""
	}
	return fmt.Sprintf("parker://%s:%d", host, port)
}

func (runtime *meshRuntime) registerTorHiddenServiceLocked() error {
	targetAddress := runtime.hiddenServiceTargetAddressLocked()
	if targetAddress == "" {
		return fmt.Errorf("tor hidden service target address is unavailable")
	}

	state, err := torcontrol.LoadHiddenServiceState(runtime.hiddenServiceStatePath())
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	service, err := (torcontrol.Client{
		ControlAddr: runtime.config.TorControlAddr,
		CookieAuth:  runtime.config.TorCookieAuth,
		DialTimeout: 5 * time.Second,
	}).AddOnion(ctx, torcontrol.HiddenServiceRequest{
		Key:           hiddenServicePrivateKey(state),
		TargetAddress: targetAddress,
		VirtualPort:   runtime.config.DirectOnionPort,
	})
	if err != nil {
		return err
	}

	runtime.torService = &runtimeHiddenService{
		Hostname:      service.Hostname,
		PrivateKey:    service.PrivateKey,
		ServiceID:     service.ServiceID,
		TargetAddress: service.TargetAddress,
		VirtualPort:   service.VirtualPort,
	}
	return torcontrol.SaveHiddenServiceState(runtime.hiddenServiceStatePath(), torcontrol.HiddenServiceState{
		CreatedAt:   nowISO(),
		Hostname:    service.Hostname,
		PrivateKey:  service.PrivateKey,
		ServiceID:   service.ServiceID,
		VirtualPort: service.VirtualPort,
	})
}

func (runtime *meshRuntime) unregisterTorHiddenService(service *runtimeHiddenService) error {
	if service == nil || strings.TrimSpace(service.ServiceID) == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return (torcontrol.Client{
		ControlAddr: runtime.config.TorControlAddr,
		CookieAuth:  runtime.config.TorCookieAuth,
		DialTimeout: 5 * time.Second,
	}).DelOnion(ctx, service.ServiceID)
}

func (runtime *meshRuntime) hiddenServiceTargetAddressLocked() string {
	host := strings.TrimSpace(runtime.config.TorTargetHost)
	if host == "" {
		host = listenerTargetHost(runtime.listener, runtime.config.PeerHost)
	}
	port := listenerAdvertisePort(runtime.listener, runtime.config.PeerPort)
	if host == "" || port <= 0 {
		return ""
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

func (runtime *meshRuntime) hiddenServiceStatePath() string {
	return filepath.Join(runtime.store.paths.StateDir, "tor-hidden-service.json")
}

func hiddenServicePrivateKey(state *torcontrol.HiddenServiceState) string {
	if state == nil {
		return ""
	}
	return strings.TrimSpace(state.PrivateKey)
}

func listenerAdvertisePort(listener net.Listener, fallback int) int {
	if tcpAddr, ok := listenerAddr(listener); ok && tcpAddr.Port > 0 {
		return tcpAddr.Port
	}
	return fallback
}

func listenerAdvertiseHost(listener net.Listener, configuredHost string) string {
	if tcpAddr, ok := listenerAddr(listener); ok && tcpAddr.IP != nil && !tcpAddr.IP.IsUnspecified() {
		return tcpAddr.IP.String()
	}
	host := strings.TrimSpace(configuredHost)
	switch host {
	case "", "0.0.0.0", "::":
		return "127.0.0.1"
	case "localhost":
		return "127.0.0.1"
	default:
		return host
	}
}

func listenerTargetHost(listener net.Listener, configuredHost string) string {
	if tcpAddr, ok := listenerAddr(listener); ok && tcpAddr.IP != nil {
		switch {
		case tcpAddr.IP.IsUnspecified():
			return "127.0.0.1"
		case tcpAddr.IP.IsLoopback():
			return tcpAddr.IP.String()
		default:
			return tcpAddr.IP.String()
		}
	}

	host := strings.TrimSpace(configuredHost)
	switch host {
	case "", "0.0.0.0", "::", "localhost":
		return "127.0.0.1"
	default:
		if ip := net.ParseIP(host); ip != nil {
			if ip.IsUnspecified() {
				return "127.0.0.1"
			}
			return ip.String()
		}
		return host
	}
}

func listenerAddr(listener net.Listener) (*net.TCPAddr, bool) {
	if listener == nil {
		return nil, false
	}
	tcpAddr, ok := listener.Addr().(*net.TCPAddr)
	return tcpAddr, ok
}

func ensureRuntimeStateDir(path string) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	return os.MkdirAll(path, 0o700)
}
