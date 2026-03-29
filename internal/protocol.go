package parker

import daemonpkg "github.com/parkerpoker/parkerd/internal/daemon"

const DaemonRequestTimeoutMS = daemonpkg.DaemonRequestTimeoutMS

type RequestEnvelope = daemonpkg.RequestEnvelope
type ResponseEnvelope = daemonpkg.ResponseEnvelope
type EventEnvelope = daemonpkg.EventEnvelope
