import { describe, expect, it } from "vitest";

import contract from "./fixtures/daemon-parity-contract.json";
import { buildProfileDaemonPaths } from "../src/daemonPaths.js";
import {
  DAEMON_HEARTBEAT_INTERVAL_MS,
  DAEMON_METHODS,
  DAEMON_WATCH_EVENTS,
} from "../src/daemonProtocol.js";

describe("daemon parity contract", () => {
  it("matches the exported RPC method and watch surface", () => {
    expect(DAEMON_METHODS).toEqual(contract.methods);
    expect(DAEMON_WATCH_EVENTS).toEqual(contract.transport.event.eventNames);
    expect(DAEMON_HEARTBEAT_INTERVAL_MS).toBe(contract.metadata.heartbeatMs);
  });

  it("matches the documented daemon artifact path rules", () => {
    expect(buildProfileDaemonPaths("/tmp/parker-daemons", "alice/witness")).toEqual({
      logPath: "/tmp/parker-daemons/alice_witness.log",
      metadataPath: "/tmp/parker-daemons/alice_witness.json",
      socketPath: "/tmp/parker-daemons/alice_witness.sock",
      stateDir: "/tmp/parker-daemons/alice_witness.state",
    });
    expect(contract.paths.slugRule).toBe("replace non [a-zA-Z0-9_-] with _");
  });
});
