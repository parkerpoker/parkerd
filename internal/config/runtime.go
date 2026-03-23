package config

import (
	"bufio"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const (
	MutinynetArkServerURL = "https://mutinynet.arkade.sh"
	MutinynetBoltzURL     = "https://api.boltz.mutinynet.arkade.sh"
	RegtestArkServerURL   = "http://127.0.0.1:7070"
	RegtestBoltzURL       = "http://127.0.0.1:9069"
	maxUnixSocketPathLen  = 103

	DefaultControllerHost = "127.0.0.1"
	DefaultControllerPort = 3030
	DefaultIndexerHost    = "0.0.0.0"
	DefaultIndexerPort    = 3020
)

type RuntimeConfig struct {
	ArkServerURL      string
	ArkadeNetworkName string
	BoltzAPIURL       string
	CacheRedisAddr    string
	CacheRedisDB      int
	CacheRedisPass    string
	CacheType         string
	CoreDBDSN         string
	CoreDBType        string
	DataDir           string
	DaemonDir         string
	EventDBDSN        string
	EventDBType       string
	IndexerHost       string
	IndexerPort       int
	IndexerURL        string
	NigiriDatadir     string
	Network           string
	OutputJSON        bool
	PeerHost          string
	PeerPort          int
	ProfileDir        string
	RunDir            string
	UseMockSettlement bool
}

type ProfileDaemonPaths struct {
	LogPath      string
	MetadataPath string
	SocketPath   string
	StateDir     string
}

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

func ResolveRuntimeConfig(flags map[string]string) (RuntimeConfig, error) {
	env := collectEnv()

	network := firstNonEmpty(
		flags["network"],
		env["PARKER_NETWORK"],
		env["ARKD_NETWORK"],
		env["VITE_NETWORK"],
		"regtest",
	)
	if network != "mutinynet" && network != "regtest" {
		network = "regtest"
	}

	cfg := RuntimeConfig{
		ArkServerURL:      RegtestArkServerURL,
		ArkadeNetworkName: "regtest",
		BoltzAPIURL:       RegtestBoltzURL,
		CacheType:         "memory",
		CoreDBType:        "sqlite",
		EventDBType:       "badger",
		IndexerHost:       DefaultIndexerHost,
		IndexerPort:       DefaultIndexerPort,
		Network:           network,
		PeerHost:          "127.0.0.1",
	}
	if network == "mutinynet" {
		cfg.ArkServerURL = MutinynetArkServerURL
		cfg.ArkadeNetworkName = "mutinynet"
		cfg.BoltzAPIURL = MutinynetBoltzURL
	}

	cfg.ArkServerURL = firstNonEmpty(
		flags["ark-server-url"],
		env["PARKER_ARK_SERVER_URL"],
		env["ARKD_ARK_SERVER_URL"],
		env["VITE_ARK_SERVER_URL"],
		cfg.ArkServerURL,
	)
	cfg.BoltzAPIURL = firstNonEmpty(
		flags["boltz-url"],
		env["PARKER_BOLTZ_URL"],
		env["ARKD_BOLTZ_URL"],
		env["VITE_BOLTZ_URL"],
		cfg.BoltzAPIURL,
	)
	cfg.IndexerURL = firstNonEmpty(
		flags["indexer-url"],
		env["PARKER_INDEXER_URL"],
		env["ARKD_INDEXER_URL"],
		env["VITE_INDEXER_URL"],
	)
	cfg.NigiriDatadir = firstNonEmpty(
		resolveOptionalPath(flags["nigiri-datadir"]),
		resolveOptionalPath(env["PARKER_NIGIRI_DATADIR"]),
		resolveOptionalPath(env["ARKD_NIGIRI_DATADIR"]),
	)
	cfg.DataDir = resolvePath(firstNonEmpty(
		flags["datadir"],
		env["PARKER_DATADIR"],
		env["ARKD_DATADIR"],
		"apps/daemon/data",
	))
	cfg.DaemonDir = resolvePath(firstNonEmpty(
		flags["daemon-dir"],
		env["PARKER_DAEMON_DIR"],
		filepath.Join(cfg.DataDir, "daemons"),
	))
	cfg.ProfileDir = resolvePath(firstNonEmpty(
		flags["profile-dir"],
		env["PARKER_PROFILE_DIR"],
		filepath.Join(cfg.DataDir, "profiles"),
	))
	cfg.RunDir = resolvePath(firstNonEmpty(
		flags["run-dir"],
		env["PARKER_RUN_DIR"],
		filepath.Join(cfg.DataDir, "runs"),
	))

	cfg.CoreDBType = normalizeChoice(
		firstNonEmpty(flags["db-type"], env["PARKER_DB_TYPE"], env["ARKD_DB_TYPE"], cfg.CoreDBType),
		[]string{"sqlite", "badger", "postgres"},
		cfg.CoreDBType,
	)
	cfg.EventDBType = normalizeChoice(
		firstNonEmpty(flags["event-db-type"], env["PARKER_EVENT_DB_TYPE"], env["ARKD_EVENT_DB_TYPE"], cfg.EventDBType),
		[]string{"badger", "postgres"},
		cfg.EventDBType,
	)
	cfg.CacheType = normalizeChoice(
		firstNonEmpty(flags["cache-type"], env["PARKER_CACHE_TYPE"], cfg.CacheType),
		[]string{"memory", "redis"},
		cfg.CacheType,
	)

	cfg.CoreDBDSN = resolveBackendDSN(
		cfg.CoreDBType,
		firstNonEmpty(
			flags["db-dsn"],
			flags["db-path"],
			env["PARKER_DB_DSN"],
			env["ARKD_DB_DSN"],
			env["PARKER_DB_PATH"],
			env["ARKD_DB_PATH"],
		),
		filepath.Join(cfg.DataDir, "storage", "core.sqlite"),
		filepath.Join(cfg.DataDir, "storage", "core.badger"),
	)
	cfg.EventDBDSN = resolveBackendDSN(
		cfg.EventDBType,
		firstNonEmpty(
			flags["event-db-dsn"],
			flags["event-db-path"],
			env["PARKER_EVENT_DB_DSN"],
			env["ARKD_EVENT_DB_DSN"],
			env["PARKER_EVENT_DB_PATH"],
			env["ARKD_EVENT_DB_PATH"],
		),
		filepath.Join(cfg.DataDir, "storage", "events.sqlite"),
		filepath.Join(cfg.DataDir, "storage", "events.badger"),
	)
	cfg.CacheRedisAddr = firstNonEmpty(
		flags["cache-redis-addr"],
		env["PARKER_CACHE_REDIS_ADDR"],
		"127.0.0.1:6379",
	)
	cfg.CacheRedisPass = firstNonEmpty(
		flags["cache-redis-password"],
		env["PARKER_CACHE_REDIS_PASSWORD"],
	)
	cfg.CacheRedisDB = parseOptionalInt(
		firstNonEmpty(flags["cache-redis-db"], env["PARKER_CACHE_REDIS_DB"]),
		0,
	)
	cfg.PeerHost = firstNonEmpty(
		flags["peer-host"],
		env["PARKER_PEER_HOST"],
		env["ARKD_PEER_HOST"],
		cfg.PeerHost,
	)
	cfg.PeerPort = parseOptionalInt(
		firstNonEmpty(flags["peer-port"], env["PARKER_PEER_PORT"], env["ARKD_PEER_PORT"]),
		0,
	)
	cfg.IndexerHost = firstNonEmpty(
		flags["indexer-host"],
		env["PARKER_INDEXER_HOST"],
		env["HOST"],
		cfg.IndexerHost,
	)
	cfg.IndexerPort = parseOptionalInt(
		firstNonEmpty(flags["indexer-port"], env["PARKER_INDEXER_PORT"], env["PORT"]),
		cfg.IndexerPort,
	)
	cfg.UseMockSettlement = parseBoolean(
		firstNonEmpty(
			flags["mock"],
			env["PARKER_USE_MOCK_SETTLEMENT"],
			env["ARKD_USE_MOCK_SETTLEMENT"],
			env["VITE_USE_MOCK_SETTLEMENT"],
		),
		false,
	)
	cfg.OutputJSON = parseBoolean(flags["json"], false)

	for _, path := range []string{
		cfg.DataDir,
		cfg.DaemonDir,
		cfg.ProfileDir,
		cfg.RunDir,
		filepath.Dir(cfg.CoreDBDSN),
		filepath.Dir(cfg.EventDBDSN),
	} {
		if shouldEnsureDir(path) {
			if err := os.MkdirAll(path, 0o755); err != nil {
				return RuntimeConfig{}, err
			}
		}
	}

	return cfg, nil
}

func BuildProfileDaemonPaths(daemonDir string, profileName string) ProfileDaemonPaths {
	slug := SlugProfile(profileName)
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

func SlugProfile(profileName string) string {
	var builder strings.Builder
	for _, char := range profileName {
		switch {
		case char >= 'a' && char <= 'z':
			builder.WriteRune(char)
		case char >= 'A' && char <= 'Z':
			builder.WriteRune(char)
		case char >= '0' && char <= '9':
			builder.WriteRune(char)
		case char == '_' || char == '-':
			builder.WriteRune(char)
		default:
			builder.WriteByte('_')
		}
	}
	return builder.String()
}

func CleanupDaemonArtifacts(paths ProfileDaemonPaths) error {
	var joined error
	if err := os.Remove(paths.SocketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		joined = errors.Join(joined, err)
	}
	if err := os.Remove(paths.MetadataPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		joined = errors.Join(joined, err)
	}
	return joined
}

func ReadProfileDaemonMetadata(paths ProfileDaemonPaths) (*ProfileDaemonMetadata, error) {
	data, err := os.ReadFile(paths.MetadataPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	var metadata ProfileDaemonMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return nil, err
	}
	return &metadata, nil
}

func WriteProfileDaemonMetadata(paths ProfileDaemonPaths, metadata ProfileDaemonMetadata) error {
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(paths.MetadataPath, data, 0o644)
}

func IsPidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || !errors.Is(err, syscall.ESRCH)
}

func FindRepoRoot() (string, error) {
	candidates := make([]string, 0, 2)
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, wd)
	}
	if executable, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Dir(executable))
	}

	for _, start := range candidates {
		if root := walkToRepoRoot(start); root != "" {
			return root, nil
		}
	}

	return "", fmt.Errorf("unable to locate parker workspace root")
}

func collectEnv() map[string]string {
	env := map[string]string{}
	for _, entry := range os.Environ() {
		parts := strings.SplitN(entry, "=", 2)
		key := parts[0]
		value := ""
		if len(parts) == 2 {
			value = parts[1]
		}
		env[key] = value
	}

	for _, path := range []string{".env", filepath.Join("..", "..", ".env")} {
		loadEnvFile(path, env)
	}

	return env
}

func loadEnvFile(path string, env map[string]string) {
	file, err := os.Open(path)
	if err != nil {
		return
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if _, exists := env[key]; exists {
			continue
		}
		env[key] = strings.Trim(strings.TrimSpace(value), `"'`)
	}
}

func parseBoolean(input string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(input)) {
	case "":
		return fallback
	case "1", "true", "yes":
		return true
	case "0", "false", "no":
		return false
	default:
		return fallback
	}
}

func parseOptionalInt(input string, fallback int) int {
	if strings.TrimSpace(input) == "" {
		return fallback
	}
	value, err := strconv.Atoi(strings.TrimSpace(input))
	if err != nil {
		return fallback
	}
	return value
}

func normalizeChoice(value string, allowed []string, fallback string) string {
	for _, candidate := range allowed {
		if value == candidate {
			return value
		}
	}
	return fallback
}

func resolveBackendDSN(kind string, configured string, sqliteFallback string, badgerFallback string) string {
	switch kind {
	case "sqlite":
		return resolvePath(firstNonEmpty(configured, sqliteFallback))
	case "badger":
		return resolvePath(firstNonEmpty(configured, badgerFallback))
	default:
		return strings.TrimSpace(configured)
	}
}

func resolveOptionalPath(path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	return resolvePath(path)
}

func resolvePath(path string) string {
	if path == "" {
		return ""
	}
	resolved, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return resolved
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func walkToRepoRoot(start string) string {
	current := resolvePath(start)
	for {
		if fileExists(filepath.Join(current, "package.json")) &&
			dirExists(filepath.Join(current, "apps")) &&
			dirExists(filepath.Join(current, "scripts")) {
			return current
		}

		parent := filepath.Dir(current)
		if parent == current {
			return ""
		}
		current = parent
	}
}

func shouldEnsureDir(path string) bool {
	if path == "" {
		return false
	}
	if strings.Contains(path, "://") {
		return false
	}
	return !strings.HasPrefix(path, "file:")
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
