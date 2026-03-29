package parker

import daemonpkg "github.com/parkerpoker/parkerd/internal/daemon"

const NativeStartupBanner = daemonpkg.NativeStartupBanner

type ProxyDaemon = daemonpkg.ProxyDaemon

func NewProxyDaemon(profileName string, config RuntimeConfig, mode string) (*ProxyDaemon, error) {
	return daemonpkg.NewProxyDaemon(profileName, config, mode)
}
