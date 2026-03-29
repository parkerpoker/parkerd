package parker

import cfg "github.com/parkerpoker/parkerd/internal/config"

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
	return cfg.ResolveLauncherPath(scriptName)
}
