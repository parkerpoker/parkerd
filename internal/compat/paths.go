package compat

import (
	"crypto/sha1"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const maxUnixSocketPathLen = 103

type ProfileDaemonPaths struct {
	LogPath      string
	MetadataPath string
	SocketPath   string
	StateDir     string
}

func BuildProfileDaemonPaths(daemonDir, profileName string) ProfileDaemonPaths {
	slug := SlugifyProfile(profileName)
	socketPath := filepath.Join(daemonDir, slug+".sock")
	if len(socketPath) > maxUnixSocketPathLen {
		socketPath = shortenedSocketPath(daemonDir, slug)
	}
	return ProfileDaemonPaths{
		LogPath:      filepath.Join(daemonDir, slug+".log"),
		MetadataPath: filepath.Join(daemonDir, slug+".json"),
		SocketPath:   socketPath,
		StateDir:     filepath.Join(daemonDir, slug+".state"),
	}
}

func shortenedSocketPath(daemonDir, slug string) string {
	sum := sha1.Sum([]byte(filepath.Clean(daemonDir) + "\x00" + slug))
	shortSlug := slug
	if len(shortSlug) > 24 {
		shortSlug = shortSlug[:24]
	}
	return filepath.Join(os.TempDir(), "parker-sockets", fmt.Sprintf("%s-%x.sock", shortSlug, sum[:6]))
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
