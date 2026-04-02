# Tor Support

`parker-daemon` can use an existing Tor instance for two things:

- registering a v3 onion service that forwards to the mesh peer TCP listener
- dialing remote `.onion` peers through Tor SOCKS5

Tor changes transport reachability only. It does not change custody proof semantics: deterministic timeout/showdown recovery still uses the same stored CSV recovery bundles, the same `RecoveryWitness` proof surface, and the same eventual-after-`U` liveness tradeoff as direct `parker://` transport.

Tor support is opt-in. Set `PARKER_USE_TOR=true` to enable hidden-service registration and outbound `.onion` dialing. When Tor is off, Parker keeps the existing `parker://<ip>:<port>` behavior and `.onion` dials fail with a clear error.

## Prerequisites

Your Tor instance must expose:

- `SocksPort 9050`
- `ControlPort 9051`
- `CookieAuthentication 1`
- a readable `DataDirectory` that contains `control_auth_cookie`

Example `torrc`:

```conf
SocksPort 0.0.0.0:9050
ControlPort 0.0.0.0:9051
CookieAuthentication 1
DataDirectory /var/lib/tor
```

## Environment

```bash
PARKER_USE_TOR=true
PARKER_TOR_SOCKS_ADDR=127.0.0.1:9050
PARKER_TOR_CONTROL_ADDR=127.0.0.1:9051
PARKER_TOR_COOKIE_AUTH=auto
PARKER_TOR_TARGET_HOST=host.docker.internal
```

`PARKER_TOR_COOKIE_AUTH` resolution order:

1. If it looks like a file path, Parker uses that exact path.
2. Otherwise Parker asks Tor for `COOKIEFILE` via `PROTOCOLINFO 1`.
3. If Tor does not provide a path, Parker falls back to common locations such as `~/.tor/control_auth_cookie`, `/usr/local/var/lib/tor/control_auth_cookie`, `/opt/homebrew/var/lib/tor/control_auth_cookie`, and `/var/lib/tor/control_auth_cookie`.

`PARKER_TOR_COOKIE_AUTH=true` and `PARKER_TOR_COOKIE_AUTH=auto` both mean “auto-discover the cookie path.”

`PARKER_TOR_TARGET_HOST` is optional. Use it when Tor is running in another container or network namespace and `127.0.0.1:<peerPort>` would point back at Tor instead of the Parker daemon. Parker will keep the listener port and swap only the target host.

## Compose Example

```yaml
services:
  tor:
    image: your-tor-image
    command: ["tor", "-f", "/etc/tor/torrc"]
    volumes:
      - ./ops/tor/torrc:/etc/tor/torrc:ro
      - tor-data:/var/lib/tor

  parker-daemon:
    environment:
      PARKER_USE_TOR: "true"
      PARKER_TOR_SOCKS_ADDR: "tor:9050"
      PARKER_TOR_CONTROL_ADDR: "tor:9051"
      PARKER_TOR_COOKIE_AUTH: "/var/lib/tor/control_auth_cookie"
      PARKER_TOR_TARGET_HOST: "host.docker.internal"
    volumes:
      - tor-data:/var/lib/tor:ro
    depends_on:
      - tor

volumes:
  tor-data: {}
```

The Parker container needs read access to Tor’s cookie file when the control port is provided by another container.

## Runtime Behavior

- When Tor is enabled and the mesh peer listener is ready, Parker issues `ADD_ONION` for the listener target and advertises `parker://<service>.onion:<direct-onion-port>`.
- The returned private key is persisted in the daemon state directory at `<daemon-dir>/<profile>.state/tor-hidden-service.json`, so the onion address stays stable across restarts.
- On shutdown Parker issues `DEL_ONION` for the service created during that run.
- Outbound `.onion` peers are dialed through `PARKER_TOR_SOCKS_ADDR` with SOCKS hostname forwarding, so Tor resolves the onion name remotely.
- If a Tor-backed peer becomes unreachable during a deterministic contested-pot timeout, recovery still follows the same custody rules: the live cooperative path can stall, but a stored recovery bundle can still execute after `U` without changing the accepted money result.

## Security Notes

- Keep the Tor control port reachable only from localhost or a private container network you trust.
- Treat `control_auth_cookie` and `tor-hidden-service.json` like secrets. Both grant control over the hidden service identity.
- If you do not want onion advertisement or `.onion` dialing, leave `PARKER_USE_TOR` unset or `false`.
