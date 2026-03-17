import { mkdirSync } from "node:fs";
import { resolve } from "node:path";
import { loadEnvFile } from "node:process";

import type { Network } from "@parker/protocol";
import {
  resolveParkerNetworkConfig,
  type ParkerNetworkConfig,
} from "@parker/settlement";

export interface CliRuntimeConfig extends ParkerNetworkConfig {
  daemonDir: string;
  indexerUrl?: string;
  profileDir: string;
  peerHost: string;
  peerPort: number;
  runDir: string;
  serverUrl: string;
  websocketUrl: string;
  useMockSettlement: boolean;
  outputJson: boolean;
}

export interface CliFlagMap {
  [key: string]: string | boolean | undefined;
}

function tryLoadEnv(path: string) {
  try {
    loadEnvFile(path);
  } catch {
    // Ignore missing env files; explicit flags still win.
  }
}

tryLoadEnv(".env");
tryLoadEnv("../../.env");

function parseBoolean(input: string | boolean | undefined, fallback = false) {
  if (typeof input === "boolean") {
    return input;
  }
  if (input === undefined) {
    return fallback;
  }
  return input === "1" || input === "true" || input === "yes";
}

function parseNetwork(input: string | boolean | undefined): Network | undefined {
  if (input === "mutinynet" || input === "regtest") {
    return input;
  }
  return undefined;
}

export function resolveCliRuntimeConfig(flags: CliFlagMap): CliRuntimeConfig {
  const network =
    parseNetwork(flags.network) ??
    parseNetwork(process.env.PARKER_NETWORK) ??
    parseNetwork(process.env.VITE_NETWORK) ??
    "regtest";

  const networkOverrides: {
    network: Network;
    arkServerUrl?: string;
    boltzApiUrl?: string;
  } = { network };
  const arkServerUrl =
    typeof flags["ark-server-url"] === "string"
      ? flags["ark-server-url"]
      : process.env.PARKER_ARK_SERVER_URL ?? process.env.VITE_ARK_SERVER_URL;
  if (arkServerUrl) {
    networkOverrides.arkServerUrl = arkServerUrl;
  }
  const boltzApiUrl =
    typeof flags["boltz-url"] === "string"
      ? flags["boltz-url"]
      : process.env.PARKER_BOLTZ_URL ?? process.env.VITE_BOLTZ_URL;
  if (boltzApiUrl) {
    networkOverrides.boltzApiUrl = boltzApiUrl;
  }
  const networkConfig = resolveParkerNetworkConfig(networkOverrides);

  const serverUrl =
    typeof flags["server-url"] === "string"
      ? flags["server-url"]
      : process.env.PARKER_SERVER_URL ?? process.env.VITE_SERVER_URL ?? "http://127.0.0.1:3020";
  const websocketUrl =
    typeof flags["websocket-url"] === "string"
      ? flags["websocket-url"]
      : process.env.PARKER_WEBSOCKET_URL ??
        process.env.WEBSOCKET_URL ??
        `${serverUrl.replace(/^http/, "ws")}/ws`;
  const indexerUrl =
    typeof flags["indexer-url"] === "string"
      ? flags["indexer-url"]
      : process.env.PARKER_INDEXER_URL ?? serverUrl;
  const peerHost =
    typeof flags["peer-host"] === "string"
      ? flags["peer-host"]
      : process.env.PARKER_PEER_HOST ?? "127.0.0.1";
  const peerPortValue =
    typeof flags["peer-port"] === "string"
      ? Number(flags["peer-port"])
      : Number(process.env.PARKER_PEER_PORT ?? 0);
  const peerPort = Number.isFinite(peerPortValue) ? peerPortValue : 0;
  const profileDir = resolve(
    typeof flags["profile-dir"] === "string"
      ? flags["profile-dir"]
      : process.env.PARKER_PROFILE_DIR ?? "apps/cli/data/profiles",
  );
  const daemonDir = resolve(
    typeof flags["daemon-dir"] === "string"
      ? flags["daemon-dir"]
      : process.env.PARKER_DAEMON_DIR ?? "apps/cli/data/daemons",
  );
  const runDir = resolve(
    typeof flags["run-dir"] === "string"
      ? flags["run-dir"]
      : process.env.PARKER_RUN_DIR ?? "apps/cli/data/runs",
  );

  mkdirSync(daemonDir, { recursive: true });
  mkdirSync(profileDir, { recursive: true });
  mkdirSync(runDir, { recursive: true });

  return {
    ...networkConfig,
    daemonDir,
    indexerUrl,
    profileDir,
    peerHost,
    peerPort,
    runDir,
    serverUrl,
    websocketUrl,
    useMockSettlement: parseBoolean(
      flags.mock ?? process.env.PARKER_USE_MOCK_SETTLEMENT ?? process.env.VITE_USE_MOCK_SETTLEMENT,
      false,
    ),
    outputJson: parseBoolean(flags.json, false),
  };
}
