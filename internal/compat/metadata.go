package compat

import (
	"encoding/json"
	"errors"
	"os"
	"syscall"
)

type ProfileDaemonMetadata struct {
	LastHeartbeat string `json:"lastHeartbeat"`
	LogPath       string `json:"logPath"`
	Mode          string `json:"mode,omitempty"`
	PeerID        string `json:"peerId,omitempty"`
	PeerURL       string `json:"peerUrl,omitempty"`
	PID           int    `json:"pid"`
	Profile       string `json:"profile"`
	ProtocolID    string `json:"protocolId,omitempty"`
	SocketPath    string `json:"socketPath"`
	StartedAt     string `json:"startedAt"`
	Status        string `json:"status"`
}

func CleanupProfileDaemonArtifacts(paths ProfileDaemonPaths) error {
	var combined error
	if err := os.Remove(paths.SocketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		combined = errors.Join(combined, err)
	}
	if err := os.Remove(paths.MetadataPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		combined = errors.Join(combined, err)
	}
	return combined
}

func ReadProfileDaemonMetadata(paths ProfileDaemonPaths) (*ProfileDaemonMetadata, error) {
	payload, err := os.ReadFile(paths.MetadataPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	var metadata ProfileDaemonMetadata
	if err := json.Unmarshal(payload, &metadata); err != nil {
		return nil, err
	}
	return &metadata, nil
}

func WriteProfileDaemonMetadata(paths ProfileDaemonPaths, metadata ProfileDaemonMetadata) error {
	payload, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	return os.WriteFile(paths.MetadataPath, payload, 0o644)
}

func IsPidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}
