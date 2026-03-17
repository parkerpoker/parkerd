import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createServer } from "node:net";

import { afterEach, describe, expect, it } from "vitest";

import { createApp } from "../../server/src/app.js";
import { ParkerDatabase } from "../../server/src/db.js";
import type { CliRuntimeConfig } from "../src/config.js";
import { CliLogger } from "../src/logger.js";
import { ParkerPlayerClient } from "../src/playerClient.js";

describe("ParkerPlayerClient", () => {
  let cleanupDir: string | undefined;

  afterEach(async () => {
    if (cleanupDir) {
      await rm(cleanupDir, { force: true, recursive: true });
      cleanupDir = undefined;
    }
  });

  it("plays through table setup with live websocket relay messaging", async () => {
    cleanupDir = await mkdtemp(join(tmpdir(), "parker-cli-"));
    const port = await getFreePort();
    const serverUrl = `http://127.0.0.1:${port}`;
    const websocketUrl = `${serverUrl.replace(/^http/, "ws")}/ws`;
    const { app } = await createApp({
      database: new ParkerDatabase(":memory:"),
      network: "regtest",
      websocketUrl,
    });
    await app.listen({ host: "127.0.0.1", port });

    const logger = new CliLogger(true);
    const host = new ParkerPlayerClient(
      "alpha",
      buildConfig({
        daemonDir: join(cleanupDir, "daemons"),
        profileDir: join(cleanupDir, "profiles"),
        runDir: join(cleanupDir, "runs"),
        serverUrl,
        websocketUrl,
      }),
      logger,
    );
    const guest = new ParkerPlayerClient(
      "beta",
      buildConfig({
        daemonDir: join(cleanupDir, "daemons"),
        profileDir: join(cleanupDir, "profiles"),
        runDir: join(cleanupDir, "runs"),
        serverUrl,
        websocketUrl,
      }),
      logger,
    );

    try {
      await host.bootstrap("Alpha");
      await guest.bootstrap("Beta");

      const created = await host.createTable({});
      await guest.joinTable(created.table.inviteCode, 4_000);

      await Promise.all([host.connectCurrentTable(), guest.connectCurrentTable()]);
      await waitFor(
        () =>
          host.currentState().peerStatus === "relay" &&
          guest.currentState().peerStatus === "relay",
      );

      await Promise.all([host.commitSeed(false), guest.commitSeed(false)]);
      await Promise.all([
        host.waitForCondition("all committed", (snapshot) => snapshot?.commitments.length === 2),
        guest.waitForCondition("all committed", (snapshot) => snapshot?.commitments.length === 2),
      ]);

      await Promise.all([host.commitSeed(true), guest.commitSeed(true)]);
      await Promise.all([
        host.waitForCondition(
          "all revealed",
          (snapshot) =>
            Boolean(
              snapshot &&
                snapshot.commitments.length === 2 &&
                snapshot.commitments.every((commitment) => commitment.revealSeed),
            ),
        ),
        guest.waitForCondition(
          "all revealed",
          (snapshot) =>
            Boolean(
              snapshot &&
                snapshot.commitments.length === 2 &&
                snapshot.commitments.every((commitment) => commitment.revealSeed),
          ),
        ),
      ]);

      const snapshot = await host.getSnapshot();
      expect(snapshot.checkpoint?.phase).toBe("preflop");

      await host.sendPeerMessage("hello-beta");
      await waitFor(() => guest.currentState().peerMessages.includes("hello-beta"));

      const actingSeatIndex = snapshot.checkpoint?.actingSeatIndex;
      if (actingSeatIndex === 0) {
        await host.sendAction({ type: "call" });
      } else if (actingSeatIndex === 1) {
        await guest.sendAction({ type: "call" });
      }

      await waitFor(async () => {
        const transcript = await host.getTranscript();
        return transcript.events.length > 0;
      });
    } finally {
      host.close();
      guest.close();
      await app.close();
    }
  }, 20_000);
});

function buildConfig(args: {
  daemonDir: string;
  profileDir: string;
  runDir: string;
  serverUrl: string;
  websocketUrl: string;
}): CliRuntimeConfig {
  return {
    arkServerUrl: "http://127.0.0.1:7070",
    arkadeNetworkName: "regtest",
    boltzApiUrl: "http://127.0.0.1:9069",
    daemonDir: args.daemonDir,
    indexerUrl: args.serverUrl,
    network: "regtest",
    outputJson: true,
    peerHost: "127.0.0.1",
    peerPort: 0,
    profileDir: args.profileDir,
    runDir: args.runDir,
    serverUrl: args.serverUrl,
    useMockSettlement: true,
    websocketUrl: args.websocketUrl,
  };
}

async function getFreePort() {
  return await new Promise<number>((resolve, reject) => {
    const server = createServer();
    server.listen(0, "127.0.0.1", () => {
      const address = server.address();
      if (!address || typeof address === "string") {
        reject(new Error("failed to acquire a free TCP port"));
        return;
      }
      const { port } = address;
      server.close((error) => {
        if (error) {
          reject(error);
          return;
        }
        resolve(port);
      });
    });
    server.once("error", reject);
  });
}

async function waitFor(predicate: () => boolean | Promise<boolean>, timeoutMs = 10_000) {
  const start = Date.now();
  while (Date.now() - start < timeoutMs) {
    if (await predicate()) {
      return;
    }
    await new Promise<void>((resolve) => {
      setTimeout(resolve, 100);
    });
  }
  throw new Error(`condition was not met within ${timeoutMs}ms`);
}
