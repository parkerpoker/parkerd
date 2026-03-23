package compat

import (
	"crypto/rand"
	"encoding/hex"
	"path/filepath"
	"strings"
)

type ProfileDaemonPaths struct {
	LogPath      string
	MetadataPath string
	SocketPath   string
	StateDir     string
}

func BuildProfileDaemonPaths(daemonDir, profileName string) ProfileDaemonPaths {
	slug := SlugifyProfile(profileName)
	return ProfileDaemonPaths{
		LogPath:      filepath.Join(daemonDir, slug+".log"),
		MetadataPath: filepath.Join(daemonDir, slug+".json"),
		SocketPath:   filepath.Join(daemonDir, slug+".sock"),
		StateDir:     filepath.Join(daemonDir, slug+".state"),
	}
}

func SlugifyProfile(profileName string) string {
	var builder strings.Builder
	for _, value := range profileName {
		if (value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z') || (value >= '0' && value <= '9') || value == '_' || value == '-' {
			builder.WriteRune(value)
			continue
		}
		builder.WriteByte('_')
	}
	return builder.String()
}

func NewRequestID() string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "parker-request"
	}
	return hex.EncodeToString(buffer)
}
