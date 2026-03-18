import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createServer } from "node:net";

import { afterEach, describe, expect, it } from "vitest";

import { createApp } from "../../server/src/app.js";
import { ParkerDatabase } from "../../server/src/db.js";
import type { CliRuntimeConfig } from "../src/config.js";
import { CliLogger } from "../src/logger.js";
import { MeshRuntime } from "../src/meshRuntime.js";

describe("MeshRuntime", () => {
  let cleanupDir: string | undefined;

  afterEach(async () => {
    if (cleanupDir) {
      await rm(cleanupDir, { force: true, recursive: true });
      cleanupDir = undefined;
    }
  });

  it("plays a daemon-to-daemon hand, publishes to the public indexer, and replays after restart", async () => {
    cleanupDir = await mkdtemp(join(tmpdir(), "parker-mesh-"));
    const port = await getFreePort();
    const [hostPort, witnessPort, alphaPort, betaPort, replayPort] = await getPeerPorts(5);
    const serverUrl = `http://127.0.0.1:${port}`;
    const { app } = await createApp({
      database: new ParkerDatabase(":memory:"),
      network: "regtest",
      websocketUrl: `${serverUrl.replace(/^http/, "ws")}/ws`,
    });
    await app.listen({ host: "127.0.0.1", port });

    const host = new MeshRuntime("host", buildConfig(cleanupDir, serverUrl, hostPort!), new CliLogger(true));
    const witness = new MeshRuntime("witness", buildConfig(cleanupDir, serverUrl, witnessPort!), new CliLogger(true));
    const alpha = new MeshRuntime("alpha", buildConfig(cleanupDir, serverUrl, alphaPort!), new CliLogger(true));
    const beta = new MeshRuntime("beta", buildConfig(cleanupDir, serverUrl, betaPort!), new CliLogger(true));

    try {
      await Promise.all([
        host.start("host"),
        witness.start("witness"),
        alpha.start("player"),
        beta.start("player"),
      ]);

      const witnessPeer = witness.currentState().peer;
      const bootstrapPeer = (await host.addBootstrapPeer(witnessPeer.peerUrl!, "Witness", ["witness"])) as {
        peerId: string;
      };

      const created = (await host.createTable({
        name: "Mesh Test Table",
        public: true,
        witnessPeerIds: [bootstrapPeer.peerId],
      })) as {
        inviteCode: string;
        table: { tableId: string };
      };

      await waitFor(async () => {
        const response = await fetch(`${serverUrl}/api/public/tables`);
        const tables = (await response.json()) as Array<{ advertisement: { tableId: string } }>;
        return tables.some((table) => table.advertisement.tableId === created.table.tableId);
      });

      await Promise.all([
        alpha.joinTable(created.inviteCode, 4_000),
        beta.joinTable(created.inviteCode, 4_000),
      ]);

      await waitFor(async () => Boolean((await alpha.getTableState(created.table.tableId)).publicState?.handId));

      await playHandUntilSettled(alpha, beta, created.table.tableId);

      const settled = await alpha.getTableState(created.table.tableId);
      expect(settled.publicState?.phase).toBe("settled");

      alpha.close();
      const replayed = new MeshRuntime("alpha", buildConfig(cleanupDir, serverUrl, replayPort!), new CliLogger(true));
      try {
        await replayed.start("player");
        const replayedState = await replayed.getTableState(created.table.tableId);
        expect(replayedState.events.length).toBeGreaterThan(0);
        expect(replayedState.publicState?.handNumber).toBeGreaterThanOrEqual(1);
      } finally {
        replayed.close();
      }
    } finally {
      host.close();
      witness.close();
      alpha.close();
      beta.close();
      await app.close();
    }
  }, 30_000);

  it("survives host failure between hands with witness rotation", async () => {
    cleanupDir = await mkdtemp(join(tmpdir(), "parker-mesh-failover-"));
    const [hostPort, witnessPort, alphaPort, betaPort] = await getPeerPorts(4);
    const host = new MeshRuntime("host", buildConfig(cleanupDir, "http://127.0.0.1:3020", hostPort!), new CliLogger(true));
    const witness = new MeshRuntime("witness", buildConfig(cleanupDir, "http://127.0.0.1:3020", witnessPort!), new CliLogger(true));
    const alpha = new MeshRuntime("alpha", buildConfig(cleanupDir, "http://127.0.0.1:3020", alphaPort!), new CliLogger(true));
    const beta = new MeshRuntime("beta", buildConfig(cleanupDir, "http://127.0.0.1:3020", betaPort!), new CliLogger(true));

    try {
      await Promise.all([
        host.start("host"),
        witness.start("witness"),
        alpha.start("player"),
        beta.start("player"),
      ]);

      const bootstrapPeer = (await host.addBootstrapPeer(
        witness.currentState().peer.peerUrl!,
        "Witness",
        ["witness"],
      )) as { peerId: string };

      const created = (await host.createTable({
        witnessPeerIds: [bootstrapPeer.peerId],
      })) as { inviteCode: string; table: { tableId: string } };

      await Promise.all([
        alpha.joinTable(created.inviteCode, 4_000),
        beta.joinTable(created.inviteCode, 4_000),
      ]);
      await waitFor(async () => Boolean((await alpha.getTableState(created.table.tableId)).publicState?.handId));
      await playHandUntilSettled(alpha, beta, created.table.tableId);

      const oldHostPeerId = host.currentState().peer.peerId;
      host.close();

      await waitFor(async () => {
        const state = await alpha.getTableState(created.table.tableId);
        return state.config.hostPeerId !== oldHostPeerId || state.events.some((event) => event.body.type === "HostRotated");
      }, 12_000);

      const nextState = await alpha.getTableState(created.table.tableId);
      expect(nextState.events.some((event) => event.body.type === "HostRotated")).toBe(true);
    } finally {
      witness.close();
      alpha.close();
      beta.close();
    }
  }, 30_000);

  it("aborts mid-hand on host failure and cashes out from a signed checkpoint", async () => {
    cleanupDir = await mkdtemp(join(tmpdir(), "parker-mesh-abort-"));
    const [hostPort, witnessPort, alphaPort, betaPort] = await getPeerPorts(4);
    const host = new MeshRuntime("host", buildConfig(cleanupDir, "http://127.0.0.1:3020", hostPort!), new CliLogger(true));
    const witness = new MeshRuntime("witness", buildConfig(cleanupDir, "http://127.0.0.1:3020", witnessPort!), new CliLogger(true));
    const alpha = new MeshRuntime("alpha", buildConfig(cleanupDir, "http://127.0.0.1:3020", alphaPort!), new CliLogger(true));
    const beta = new MeshRuntime("beta", buildConfig(cleanupDir, "http://127.0.0.1:3020", betaPort!), new CliLogger(true));

    try {
      await Promise.all([
        host.start("host"),
        witness.start("witness"),
        alpha.start("player"),
        beta.start("player"),
      ]);

      const bootstrapPeer = (await host.addBootstrapPeer(
        witness.currentState().peer.peerUrl!,
        "Witness",
        ["witness"],
      )) as { peerId: string };

      const created = (await host.createTable({
        witnessPeerIds: [bootstrapPeer.peerId],
      })) as { inviteCode: string; table: { tableId: string } };

      await Promise.all([
        alpha.joinTable(created.inviteCode, 4_000),
        beta.joinTable(created.inviteCode, 4_000),
      ]);
      await waitFor(async () => Boolean((await alpha.getTableState(created.table.tableId)).publicState?.handId));

      const firstState = await alpha.getTableState(created.table.tableId);
      const actingPlayer =
        firstState.publicState?.actingSeatIndex ===
        firstState.publicState?.seatedPlayers.find(
          (player) => player.playerId === alpha.currentState().peer.walletPlayerId,
        )?.seatIndex
          ? alpha
          : beta;
      await sendActionWithRetry(actingPlayer, { type: "call" }, created.table.tableId);
      host.close();

      await waitFor(async () => {
        const state = await alpha.getTableState(created.table.tableId);
        return state.events.some((event) => event.body.type === "HandAbort");
      }, 12_000);

      const abortedState = await alpha.getTableState(created.table.tableId);
      const rollbackSnapshot = abortedState.latestFullySignedSnapshot;
      const abortIndex = abortedState.events.findIndex((event) => event.body.type === "HandAbort");
      expect(rollbackSnapshot).not.toBeNull();
      expect(abortIndex).toBeGreaterThanOrEqual(0);
      expect(abortedState.publicState?.handNumber).toBe(rollbackSnapshot?.handNumber);
      expect(abortedState.publicState?.phase ?? null).toBe(rollbackSnapshot?.phase ?? null);

      await new Promise<void>((resolve) => {
        setTimeout(resolve, 1_500);
      });

      const stableRollback = await alpha.getTableState(created.table.tableId);
      expect(stableRollback.events.slice(abortIndex + 1).some((event) => event.body.type === "HandStart")).toBe(false);
      expect(stableRollback.publicState?.handNumber).toBe(rollbackSnapshot?.handNumber);
      expect(stableRollback.publicState?.phase ?? null).toBe(rollbackSnapshot?.phase ?? null);

      const cashout = (await alpha.cashOut(created.table.tableId)) as {
        checkpointHash: string;
        receipt: { checkpointHash?: string };
      };
      expect(cashout.checkpointHash).toHaveLength(64);
      expect(cashout.receipt.checkpointHash).toBe(cashout.checkpointHash);

      const alphaContext = (alpha as unknown as { contextByTableId: Map<string, { buyInReceipts: Map<string, { vtxoExpiry?: string }> }> }).contextByTableId;
      const receipt = alphaContext.get(created.table.tableId)?.buyInReceipts.values().next().value;
      if (receipt) {
        receipt.vtxoExpiry = new Date(Date.now() + 60_000).toISOString();
      }
      expect(alpha.currentState().fundsWarnings.length).toBeGreaterThanOrEqual(0);
      const renewals = (await alpha.renewFunds(created.table.tableId)) as Array<{ kind: string }>;
      expect(renewals[0]?.kind).toBe("renewal");
    } finally {
      witness.close();
      alpha.close();
      beta.close();
    }
  }, 30_000);
});

function buildConfig(root: string, serverUrl: string, peerPort: number): CliRuntimeConfig {
  return {
    arkServerUrl: "http://127.0.0.1:7070",
    arkadeNetworkName: "regtest",
    boltzApiUrl: "http://127.0.0.1:9069",
    daemonDir: join(root, "daemons"),
    indexerUrl: serverUrl,
    network: "regtest",
    outputJson: true,
    peerHost: "127.0.0.1",
    peerPort,
    profileDir: join(root, "profiles"),
    runDir: join(root, "runs"),
    serverUrl,
    useMockSettlement: true,
    websocketUrl: `${serverUrl.replace(/^http/, "ws")}/ws`,
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

async function getPeerPorts(count: number) {
  return await Promise.all(Array.from({ length: count }, () => getFreePort()));
}

async function playHandUntilSettled(alpha: MeshRuntime, beta: MeshRuntime, tableId: string) {
  for (let turn = 0; turn < 24; turn += 1) {
    const state = await alpha.getTableState(tableId);
    const publicState = state.publicState;
    if (!publicState || publicState.phase === "settled") {
      return;
    }
    const alphaSeat = publicState.seatedPlayers.find(
      (player) => player.playerId === alpha.currentState().peer.walletPlayerId,
    )!;
    const betaSeat = publicState.seatedPlayers.find(
      (player) => player.playerId === beta.currentState().peer.walletPlayerId,
    )!;
    const actor = publicState.actingSeatIndex === alphaSeat.seatIndex ? alpha : beta;
    const actorPlayerId = actor.currentState().peer.walletPlayerId!;
    const contribution = publicState.roundContributions[actorPlayerId] ?? 0;
    const toCall = Math.max(0, publicState.currentBetSats - contribution);
    try {
      await sendActionWithRetry(actor, toCall > 0 ? { type: "call" } : { type: "check" }, tableId);
    } catch (error) {
      throw error;
    }
    await waitFor(async () => {
      const next = await alpha.getTableState(tableId);
      return next.events.length > state.events.length;
    });
    if (betaSeat && alphaSeat) {
      continue;
    }
  }
  throw new Error("hand did not settle in time");
}

async function sendActionWithRetry(runtime: MeshRuntime, payload: { type: "call" } | { type: "check" }, tableId: string) {
  for (let attempt = 0; attempt < 40; attempt += 1) {
    try {
      await runtime.sendAction(payload, tableId);
      return;
    } catch (error) {
      if (
        (error as Error).message.includes("cannot act while") ||
        (error as Error).message.includes("hand is still starting") ||
        (error as Error).message.includes("hand is not active")
      ) {
        await new Promise<void>((resolve) => {
          setTimeout(resolve, 100);
        });
        continue;
      }
      throw error;
    }
  }
  throw new Error("action did not become valid in time");
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
