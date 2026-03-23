package compat

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	DefaultRegtestArkServerURL   = "http://127.0.0.1:7070"
	DefaultRegtestBoltzURL       = "http://127.0.0.1:9069"
	DefaultMutinynetArkServerURL = "https://mutinynet.arkade.sh"
	DefaultMutinynetBoltzURL     = "https://api.boltz.mutinynet.arkade.sh"
	DefaultPeerHost              = "127.0.0.1"
)

type FlagMap map[string]string

type optionalString struct {
	value string
	ok    bool
}

type Config struct {
	ArkServerURL      string
	BoltzURL          string
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

func (flags FlagMap) Lookup(key string) (string, bool) {
	value, ok := flags[key]
	return value, ok
}

func ParseFlagMap(argv []string) (FlagMap, []string) {
	flags := FlagMap{}
	positionals := make([]string, 0, len(argv))
	for index := 0; index < len(argv); index += 1 {
		value := argv[index]
		if !strings.HasPrefix(value, "--") {
			positionals = append(positionals, value)
			continue
		}

		trimmed := strings.TrimPrefix(value, "--")
		keyValue := strings.SplitN(trimmed, "=", 2)
		key := keyValue[0]
		if len(keyValue) == 2 {
			flags[key] = keyValue[1]
			continue
		}

		if index+1 >= len(argv) || strings.HasPrefix(argv[index+1], "--") {
			flags[key] = "true"
			continue
		}

		flags[key] = argv[index+1]
		index += 1
	}
	return flags, positionals
}

func ResolveConfig(flags FlagMap) (Config, error) {
	loadDefaultEnvFiles()

	network := firstValidNetwork(
		flagValue(flags, "network"),
		envValue("PARKER_NETWORK"),
		envValue("VITE_NETWORK"),
		"regtest",
	)

	arkServerURL := pickString(
		asOptional(flagValuePreserveEmpty(flags, "ark-server-url")),
		asOptional(envValuePreserveEmpty("PARKER_ARK_SERVER_URL")),
		asOptional(envValuePreserveEmpty("VITE_ARK_SERVER_URL")),
	)
	if arkServerURL == "" {
		arkServerURL = defaultArkServerURL(network)
	}

	boltzURL := pickString(
		asOptional(flagValuePreserveEmpty(flags, "boltz-url")),
		asOptional(envValuePreserveEmpty("PARKER_BOLTZ_URL")),
		asOptional(envValuePreserveEmpty("VITE_BOLTZ_URL")),
	)
	if boltzURL == "" {
		boltzURL = defaultBoltzURL(network)
	}

	daemonDir, err := resolvePathWithFallback(flags, "daemon-dir", "PARKER_DAEMON_DIR", "apps/daemon/data/daemons")
	if err != nil {
		return Config{}, fmt.Errorf("resolve daemon dir: %w", err)
	}
	profileDir, err := resolvePathWithFallback(flags, "profile-dir", "PARKER_PROFILE_DIR", "apps/daemon/data/profiles")
	if err != nil {
		return Config{}, fmt.Errorf("resolve profile dir: %w", err)
	}
	runDir, err := resolvePathWithFallback(flags, "run-dir", "PARKER_RUN_DIR", "apps/daemon/data/runs")
	if err != nil {
		return Config{}, fmt.Errorf("resolve run dir: %w", err)
	}

	nigiriDatadir := pickString(
		asOptional(flagValuePreserveEmpty(flags, "nigiri-datadir")),
		asOptional(envValuePreserveEmpty("PARKER_NIGIRI_DATADIR")),
	)
	if nigiriDatadir != "" {
		nigiriDatadir, err = filepath.Abs(nigiriDatadir)
		if err != nil {
			return Config{}, fmt.Errorf("resolve nigiri datadir: %w", err)
		}
	}

	peerHost := pickString(asOptional(flagValuePreserveEmpty(flags, "peer-host")), asOptional(envValuePreserveEmpty("PARKER_PEER_HOST")))
	if peerHost == "" {
		peerHost = DefaultPeerHost
	}

	peerPort := 0
	if raw, ok := firstPresent(asOptional(flagValuePreserveEmpty(flags, "peer-port")), asOptional(envValuePreserveEmpty("PARKER_PEER_PORT"))); ok {
		parsed, parseErr := strconv.Atoi(raw)
		if parseErr == nil {
			peerPort = parsed
		}
	}

	cfg := Config{
		ArkServerURL:      arkServerURL,
		BoltzURL:          boltzURL,
		DaemonDir:         daemonDir,
		IndexerURL:        pickString(asOptional(flagValuePreserveEmpty(flags, "indexer-url")), asOptional(envValuePreserveEmpty("PARKER_INDEXER_URL")), asOptional(envValuePreserveEmpty("VITE_INDEXER_URL"))),
		NigiriDatadir:     nigiriDatadir,
		Network:           network,
		OutputJSON:        parseBool(pickString(asOptional(flagValuePreserveEmpty(flags, "json"))), false),
		PeerHost:          peerHost,
		PeerPort:          peerPort,
		ProfileDir:        profileDir,
		RunDir:            runDir,
		UseMockSettlement: parseBool(pickString(asOptional(flagValuePreserveEmpty(flags, "mock")), asOptional(envValuePreserveEmpty("PARKER_USE_MOCK_SETTLEMENT")), asOptional(envValuePreserveEmpty("VITE_USE_MOCK_SETTLEMENT"))), false),
	}

	for _, dir := range []string{cfg.DaemonDir, cfg.ProfileDir, cfg.RunDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return Config{}, fmt.Errorf("create %s: %w", dir, err)
		}
	}

	return cfg, nil
}

func ApplyConfigEnv(base []string, cfg Config) []string {
	env := envSliceToMap(base)
	env["PARKER_NETWORK"] = cfg.Network
	env["PARKER_ARK_SERVER_URL"] = cfg.ArkServerURL
	env["PARKER_BOLTZ_URL"] = cfg.BoltzURL
	env["PARKER_PROFILE_DIR"] = cfg.ProfileDir
	env["PARKER_DAEMON_DIR"] = cfg.DaemonDir
	env["PARKER_RUN_DIR"] = cfg.RunDir
	env["PARKER_PEER_HOST"] = cfg.PeerHost
	env["PARKER_PEER_PORT"] = strconv.Itoa(cfg.PeerPort)
	env["PARKER_USE_MOCK_SETTLEMENT"] = strconv.FormatBool(cfg.UseMockSettlement)
	if cfg.IndexerURL != "" {
		env["PARKER_INDEXER_URL"] = cfg.IndexerURL
	} else {
		delete(env, "PARKER_INDEXER_URL")
	}
	if cfg.NigiriDatadir != "" {
		env["PARKER_NIGIRI_DATADIR"] = cfg.NigiriDatadir
	} else {
		delete(env, "PARKER_NIGIRI_DATADIR")
	}
	return mapToEnvSlice(env)
}

func SetEnvValue(base []string, key, value string) []string {
	env := envSliceToMap(base)
	env[key] = value
	return mapToEnvSlice(env)
}

func resolvePathWithFallback(flags FlagMap, flagKey, envKey, fallback string) (string, error) {
	if value, ok := flagValuePreserveEmpty(flags, flagKey); ok {
		return filepath.Abs(value)
	}
	if value, ok := envValuePreserveEmpty(envKey); ok {
		return filepath.Abs(value)
	}
	return filepath.Abs(fallback)
}

func flagValue(flags FlagMap, key string) string {
	value, _ := flags.Lookup(key)
	return value
}

func flagValuePreserveEmpty(flags FlagMap, key string) (string, bool) {
	return flags.Lookup(key)
}

func envValue(key string) string {
	value, _ := os.LookupEnv(key)
	return value
}

func envValuePreserveEmpty(key string) (string, bool) {
	return os.LookupEnv(key)
}

func firstPresent(values ...optionalString) (string, bool) {
	for _, item := range values {
		if item.ok {
			return item.value, true
		}
	}
	return "", false
}

func pickString(values ...optionalString) string {
	for _, raw := range values {
		if raw.ok {
			return raw.value
		}
	}
	return ""
}

func asOptional(value string, ok bool) optionalString {
	return optionalString{value: value, ok: ok}
}

func firstValidNetwork(values ...string) string {
	for _, value := range values {
		if value == "regtest" || value == "mutinynet" {
			return value
		}
	}
	return "regtest"
}

func defaultArkServerURL(network string) string {
	if network == "mutinynet" {
		return DefaultMutinynetArkServerURL
	}
	return DefaultRegtestArkServerURL
}

func defaultBoltzURL(network string) string {
	if network == "mutinynet" {
		return DefaultMutinynetBoltzURL
	}
	return DefaultRegtestBoltzURL
}

func parseBool(input string, fallback bool) bool {
	if input == "" {
		return fallback
	}
	switch strings.ToLower(input) {
	case "1", "true", "yes":
		return true
	case "0", "false", "no":
		return false
	default:
		return fallback
	}
}

func loadDefaultEnvFiles() {
	cwd, err := os.Getwd()
	if err != nil {
		return
	}
	loadEnvFile(filepath.Join(cwd, ".env"))
	loadEnvFile(filepath.Clean(filepath.Join(cwd, "../../.env")))
}

func loadEnvFile(path string) {
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
		keyValue := strings.SplitN(line, "=", 2)
		if len(keyValue) != 2 {
			continue
		}
		key := strings.TrimSpace(keyValue[0])
		value := strings.TrimSpace(keyValue[1])
		value = strings.Trim(value, `"'`)
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		_ = os.Setenv(key, value)
	}
}

func envSliceToMap(base []string) map[string]string {
	env := make(map[string]string, len(base))
	for _, entry := range base {
		keyValue := strings.SplitN(entry, "=", 2)
		if len(keyValue) != 2 {
			continue
		}
		env[keyValue[0]] = keyValue[1]
	}
	return env
}

func mapToEnvSlice(env map[string]string) []string {
	result := make([]string, 0, len(env))
	for key, value := range env {
		result = append(result, fmt.Sprintf("%s=%s", key, value))
	}
	return result
}
