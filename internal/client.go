package parker

import daemonpkg "github.com/parkerpoker/parkerd/internal/daemon"

type Client = daemonpkg.Client
type WatchSession = daemonpkg.WatchSession

func NewClient(profileName string, config RuntimeConfig) *Client {
	return daemonpkg.NewClient(profileName, config)
}
