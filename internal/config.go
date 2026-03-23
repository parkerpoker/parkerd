package parker

import (
	"path/filepath"
	"strconv"
	"strings"

	cfg "github.com/danieldresner/arkade_fun/internal/config"
)

const (
	MutinynetArkServerURL = cfg.MutinynetArkServerURL
	MutinynetBoltzURL     = cfg.MutinynetBoltzURL
	RegtestArkServerURL   = cfg.RegtestArkServerURL
	RegtestBoltzURL       = cfg.RegtestBoltzURL
)

type RuntimeConfig = cfg.RuntimeConfig
type ProfileDaemonPaths = cfg.ProfileDaemonPaths
type ProfileDaemonMetadata = cfg.ProfileDaemonMetadata

func ResolveRuntimeConfig(flags FlagMap) (RuntimeConfig, error) {
	return cfg.ResolveRuntimeConfig(map[string]string(flags))
}

func BuildProfileDaemonPaths(daemonDir string, profileName string) ProfileDaemonPaths {
	return cfg.BuildProfileDaemonPaths(daemonDir, profileName)
}

func SlugProfile(profileName string) string {
	return cfg.SlugProfile(profileName)
}

func CleanupDaemonArtifacts(paths ProfileDaemonPaths) error {
	return cfg.CleanupDaemonArtifacts(paths)
}

func ReadProfileDaemonMetadata(paths ProfileDaemonPaths) (*ProfileDaemonMetadata, error) {
	return cfg.ReadProfileDaemonMetadata(paths)
}

func WriteProfileDaemonMetadata(paths ProfileDaemonPaths, metadata ProfileDaemonMetadata) error {
	return cfg.WriteProfileDaemonMetadata(paths, metadata)
}

func IsPidAlive(pid int) bool {
	return cfg.IsPidAlive(pid)
}

func FindRepoRoot() (string, error) {
	return cfg.FindRepoRoot()
}

func ResolveLauncherPath(scriptName string) (string, error) {
	root, err := FindRepoRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "scripts", "bin", scriptName), nil
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

func resolveFlagPath(flags FlagMap, key string) string {
	if value, ok := flags[key]; ok {
		return resolvePath(value)
	}
	return ""
}

func resolveEnvPath(env map[string]string, key string) string {
	if value, ok := env[key]; ok {
		return resolvePath(value)
	}
	return ""
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
	return flags[key]
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
