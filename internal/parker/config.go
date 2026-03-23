package parker

import (
	"bufio"
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
)

type RuntimeConfig struct {
	ArkServerURL      string
	ArkadeNetworkName string
	BoltzAPIURL       string
	DaemonDir         string
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

func ResolveRuntimeConfig(flags FlagMap) (RuntimeConfig, error) {
	env := collectEnv()

	network := firstNonEmpty(
		stringFlag(flags, "network"),
		env["PARKER_NETWORK"],
		env["VITE_NETWORK"],
		"regtest",
	)
	if network != "mutinynet" && network != "regtest" {
		network = "regtest"
	}

	config := RuntimeConfig{
		Network:           network,
		ArkServerURL:      RegtestArkServerURL,
		BoltzAPIURL:       RegtestBoltzURL,
		ArkadeNetworkName: "regtest",
	}
	if network == "mutinynet" {
		config.ArkServerURL = MutinynetArkServerURL
		config.BoltzAPIURL = MutinynetBoltzURL
		config.ArkadeNetworkName = "mutinynet"
	}

	config.ArkServerURL = firstNonEmpty(
		stringFlag(flags, "ark-server-url"),
		env["PARKER_ARK_SERVER_URL"],
		env["VITE_ARK_SERVER_URL"],
		config.ArkServerURL,
	)
	config.BoltzAPIURL = firstNonEmpty(
		stringFlag(flags, "boltz-url"),
		env["PARKER_BOLTZ_URL"],
		env["VITE_BOLTZ_URL"],
		config.BoltzAPIURL,
	)
	config.IndexerURL = firstNonEmpty(
		stringFlag(flags, "indexer-url"),
		env["PARKER_INDEXER_URL"],
		env["VITE_INDEXER_URL"],
	)
	config.NigiriDatadir = firstNonEmpty(
		resolveFlagPath(flags, "nigiri-datadir"),
		resolveEnvPath(env, "PARKER_NIGIRI_DATADIR"),
	)
	config.PeerHost = firstNonEmpty(
		stringFlag(flags, "peer-host"),
		env["PARKER_PEER_HOST"],
		"127.0.0.1",
	)
	config.PeerPort = parseOptionalInt(
		firstNonEmpty(stringFlag(flags, "peer-port"), env["PARKER_PEER_PORT"]),
		0,
	)
	config.ProfileDir = resolvePath(firstNonEmpty(
		stringFlag(flags, "profile-dir"),
		env["PARKER_PROFILE_DIR"],
		"apps/daemon/data/profiles",
	))
	config.DaemonDir = resolvePath(firstNonEmpty(
		stringFlag(flags, "daemon-dir"),
		env["PARKER_DAEMON_DIR"],
		"apps/daemon/data/daemons",
	))
	config.RunDir = resolvePath(firstNonEmpty(
		stringFlag(flags, "run-dir"),
		env["PARKER_RUN_DIR"],
		"apps/daemon/data/runs",
	))
	config.UseMockSettlement = parseBoolean(
		firstNonEmpty(stringFlag(flags, "mock"), env["PARKER_USE_MOCK_SETTLEMENT"], env["VITE_USE_MOCK_SETTLEMENT"]),
		false,
	)
	config.OutputJSON = parseBoolean(firstNonEmpty(stringFlag(flags, "json")), false)

	for _, path := range []string{config.DaemonDir, config.ProfileDir, config.RunDir} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			return RuntimeConfig{}, err
		}
	}

	return config, nil
}

func BuildProfileDaemonPaths(daemonDir string, profileName string) ProfileDaemonPaths {
	slug := SlugProfile(profileName)
	return ProfileDaemonPaths{
		LogPath:      filepath.Join(daemonDir, slug+".log"),
		MetadataPath: filepath.Join(daemonDir, slug+".json"),
		SocketPath:   filepath.Join(daemonDir, slug+".sock"),
		StateDir:     filepath.Join(daemonDir, slug+".state"),
	}
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

func ResolveLauncherPath(scriptName string) (string, error) {
	root, err := FindRepoRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "scripts", "bin", scriptName), nil
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
	if input == "" {
		return fallback
	}
	value, err := strconv.Atoi(input)
	if err != nil {
		return fallback
	}
	return value
}

func resolveFlagPath(flags FlagMap, key string) string {
	value := stringFlag(flags, key)
	if value == "" {
		return ""
	}
	return resolvePath(value)
}

func resolveEnvPath(env map[string]string, key string) string {
	value := env[key]
	if value == "" {
		return ""
	}
	return resolvePath(value)
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

func stringFlag(flags FlagMap, key string) string {
	if value, ok := flags[key]; ok {
		return value
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
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

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
