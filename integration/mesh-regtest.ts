import { spawn } from "node:child_process";
import { mkdir, mkdtemp, readFile, writeFile } from "node:fs/promises";
import { createServer } from "node:net";
import { homedir, tmpdir } from "node:os";
import { join } from "node:path";

import type {
  CooperativeTableSnapshot,
  MeshPlayerActionPayload,
  MeshTableConfig,
  PublicTableState,
  SignedTableEvent,
} from "@parker/protocol";

import { createApp } from "../apps/server/src/app.js";
import { ParkerDatabase } from "../apps/server/src/db.js";
import { resolveCliRuntimeConfig } from "../apps/cli/src/config.js";
import { ProfileDaemon } from "../apps/cli/src/daemonProcess.js";
import { DaemonRpcClient } from "../apps/cli/src/daemonClient.js";
const BUY_IN_SATS = 4_000;
const FAUCET_AMOUNT_SATS = 100_000;
const LIVE_BIG_BLIND_SATS = 800;
const LIVE_SMALL_BLIND_SATS = 400;
const WALLET_MIN_AVAILABLE_SATS = 20_000;

interface HarnessArgs {
  baseDir?: string;
  keepNigiri: boolean;
  startNigiri: boolean;
}

interface ScenarioSummary {
  actions: Array<{ action: MeshPlayerActionPayload["type"]; actor: string; phase: string | null }>;
  balances: Record<string, number>;
  checkpointHash: string;
  initialTotalSats: number;
  name: string;
  receipts: Array<Record<string, unknown>>;
  tableId: string;
}

interface ScenarioEnvironment {
  alpha: DaemonHarnessPeer;
  app: Awaited<ReturnType<typeof createApp>>["app"];
  baseDir: string;
  beta: DaemonHarnessPeer;
  gamma: DaemonHarnessPeer;
  host: DaemonHarnessPeer;
  serverUrl: string;
  tableId?: string;
  witness: DaemonHarnessPeer;
}

interface DaemonHarnessPeer {
  alias: string;
  client: DaemonRpcClient;
  daemon: ProfileDaemon;
  profile: string;
  walletPlayerId?: string;
}

interface MeshTableView {
  config: MeshTableConfig;
  events: SignedTableEvent[];
  latestFullySignedSnapshot: CooperativeTableSnapshot | null;
  latestSnapshot: CooperativeTableSnapshot | null;
  publicState: PublicTableState | null;
}

async function main() {
  const args = parseArgs(process.argv.slice(2));
  const baseDir = args.baseDir ?? (await mkdtemp(join(tmpdir(), "parker-mesh-regtest-")));
  const nigiriDatadir = args.startNigiri ? resolveNigiriDatadir(baseDir) : undefined;
  let startedNigiri = false;

  if (args.startNigiri) {
    await ensureNigiriStarted(nigiriDatadir!);
    startedNigiri = true;
  }

  try {
    const summaries: ScenarioSummary[] = [];
    summaries.push(await runFailoverScenario(baseDir, nigiriDatadir));
    summaries.push(await runAbortScenario(baseDir, nigiriDatadir));

    const summaryPath = join(baseDir, "summary.json");
    await writeFile(
      summaryPath,
      `${JSON.stringify(
        {
          generatedAt: new Date().toISOString(),
          notes: [
            "The v1 mesh runtime currently seats exactly two bankroll participants per table.",
            "A third player daemon is still started to validate the daemon mesh topology and public table discovery.",
          ],
          scenarios: summaries,
        },
        null,
        2,
      )}\n`,
      "utf8",
    );

    log("result", {
      baseDir,
      scenarioCount: summaries.length,
      summaryPath,
    });
  } finally {
    if (startedNigiri && !args.keepNigiri) {
      await runNigiriCommand(nigiriDatadir, ["stop"]);
    }
  }
}

async function runFailoverScenario(rootDir: string, nigiriDatadir?: string): Promise<ScenarioSummary> {
  const env = await createScenarioEnvironment(rootDir, "between-hand-failover", nigiriDatadir, {
    fundGamma: true,
  });
  try {
    await bootstrapMeshTable(env, { publicTable: true });
    await playHandUntilSettled(env.alpha, env.beta, env.tableId!, {
      plan: "raise-then-default",
    });
    await waitForLatestSettlementSnapshot(env.alpha.client, env.tableId!, 1);

    await env.host.daemon.stop();

    await waitFor(async () => {
      const state = (await env.alpha.client.meshGetTable(env.tableId!)) as MeshTableView;
      return state.events.some((event) => event.body.type === "HostRotated");
    }, 20_000);

    await waitFor(async () => {
      const state = (await env.alpha.client.meshGetTable(env.tableId!)) as MeshTableView;
      return Boolean(state.publicState?.handId && state.publicState.handNumber >= 2);
    }, 20_000);

    const actions = await playHandUntilSettled(env.alpha, env.beta, env.tableId!, {
      plan: "all-in-then-default",
    });
    const settled = await waitForLatestSettlementSnapshot(env.alpha.client, env.tableId!, 2);
    const balances = settled.latestFullySignedSnapshot?.chipBalances ?? {};
    const total = sumValues(balances);
    assert(total === BUY_IN_SATS * 2, `expected total settled chips ${BUY_IN_SATS * 2}, received ${total}`);

    const alphaRenewals = (await env.alpha.client.meshRenew(env.tableId!)) as Array<Record<string, unknown>>;
    const betaRenewals = (await env.beta.client.meshRenew(env.tableId!)) as Array<Record<string, unknown>>;
    const renewals = [...alphaRenewals, ...betaRenewals];
    log("renewals", {
      count: renewals.length,
      scenario: "between-hand-failover",
      tableId: env.tableId,
    });

    const alphaCashOut = (await env.alpha.client.meshCashOut(env.tableId!)) as Record<string, unknown>;
    const betaCashOut = (await env.beta.client.meshCashOut(env.tableId!)) as Record<string, unknown>;
    const receipts = [alphaCashOut, betaCashOut, ...renewals];
    const cashoutTotal =
      Number((alphaCashOut.receipt as Record<string, unknown>).amountSats ?? 0) +
      Number((betaCashOut.receipt as Record<string, unknown>).amountSats ?? 0);
    assert(cashoutTotal === BUY_IN_SATS * 2, `expected cooperative cash-out total ${BUY_IN_SATS * 2}, received ${cashoutTotal}`);

    const checkpointHash = String(alphaCashOut.checkpointHash);
    assert(checkpointHash.length === 64, "cooperative cash-out checkpoint hash is invalid");
    log("scenario-complete", {
      balances,
      checkpointHash,
      scenario: "between-hand-failover",
      tableId: env.tableId,
    });

    return {
      actions,
      balances,
      checkpointHash,
      initialTotalSats: BUY_IN_SATS * 2,
      name: "between-hand-failover",
      receipts: [
        alphaCashOut,
        betaCashOut,
        ...renewals,
        await readFundsState(env, env.alpha.profile),
        await readFundsState(env, env.beta.profile),
      ],
      tableId: env.tableId!,
    };
  } finally {
    await shutdownScenarioEnvironment(env);
  }
}

async function runAbortScenario(rootDir: string, nigiriDatadir?: string): Promise<ScenarioSummary> {
  const env = await createScenarioEnvironment(rootDir, "mid-hand-abort", nigiriDatadir, {
    fundGamma: false,
  });
  try {
    await bootstrapMeshTable(env, { publicTable: false });
    await playHandUntilSettled(env.alpha, env.beta, env.tableId!, {
      plan: "default",
    });
    await waitForLatestSettlementSnapshot(env.alpha.client, env.tableId!, 1);
    await waitFor(async () => {
      const state = (await env.alpha.client.meshGetTable(env.tableId!)) as MeshTableView;
      return Boolean(state.publicState?.handId && state.publicState.handNumber >= 2);
    }, 20_000);

    const beforeCrash = (await env.alpha.client.meshGetTable(env.tableId!)) as MeshTableView;
    const actingPeer = currentActor(env.alpha, env.beta, beforeCrash.publicState);
    await sendActionWithRetry(actingPeer, nextDefaultAction(beforeCrash.publicState!, actingPeer), env.tableId!);

    await env.host.daemon.stop();

    await waitFor(async () => {
      const state = (await env.alpha.client.meshGetTable(env.tableId!)) as MeshTableView;
      return state.events.some((event) => event.body.type === "HandAbort");
    }, 20_000);

    const aborted = await waitForLatestSettlementSnapshot(env.alpha.client, env.tableId!, 1, 2);
    const balances = aborted.latestFullySignedSnapshot?.chipBalances ?? {};
    const total = sumValues(balances);
    assert(total === BUY_IN_SATS * 2, `expected rollback chips ${BUY_IN_SATS * 2}, received ${total}`);

    const alphaCashOut = (await env.alpha.client.meshCashOut(env.tableId!)) as Record<string, unknown>;
    const betaEmergencyExit = (await env.beta.client.meshExit(env.tableId!)) as Record<string, unknown>;
    const settledTotal =
      Number((alphaCashOut.receipt as Record<string, unknown>).amountSats ?? 0) +
      Number((betaEmergencyExit.receipt as Record<string, unknown>).amountSats ?? 0);
    assert(settledTotal === BUY_IN_SATS * 2, `expected exit total ${BUY_IN_SATS * 2}, received ${settledTotal}`);

    const checkpointHash = String(alphaCashOut.checkpointHash);
    assert(checkpointHash.length === 64, "abort cash-out checkpoint hash is invalid");
    log("scenario-complete", {
      balances,
      checkpointHash,
      scenario: "mid-hand-abort",
      tableId: env.tableId,
    });

    return {
      actions: [
        {
          action: nextDefaultAction(beforeCrash.publicState!, actingPeer).type,
          actor: actingPeer.profile,
          phase: beforeCrash.publicState?.phase ?? null,
        },
      ],
      balances,
      checkpointHash,
      initialTotalSats: BUY_IN_SATS * 2,
      name: "mid-hand-abort",
      receipts: [
        alphaCashOut,
        betaEmergencyExit,
        await readFundsState(env, env.alpha.profile),
        await readFundsState(env, env.beta.profile),
      ],
      tableId: env.tableId!,
    };
  } finally {
    await shutdownScenarioEnvironment(env);
  }
}

async function createScenarioEnvironment(
  rootDir: string,
  scenarioName: string,
  nigiriDatadir?: string,
  options?: {
    fundGamma?: boolean;
  },
): Promise<ScenarioEnvironment> {
  const baseDir = join(rootDir, scenarioName);
  const serverPort = await getFreePort();
  const [hostPort, witnessPort, alphaPort, betaPort, gammaPort] = await getPeerPorts(5);
  const serverUrl = `http://127.0.0.1:${serverPort}`;
  const config = resolveCliRuntimeConfig({
    "ark-server-url": "http://127.0.0.1:7070",
    "boltz-url": "http://127.0.0.1:9069",
    "daemon-dir": join(baseDir, "daemons"),
    "indexer-url": serverUrl,
    mock: false,
    network: "regtest",
    ...(nigiriDatadir ? { "nigiri-datadir": nigiriDatadir } : {}),
    "profile-dir": join(baseDir, "profiles"),
    "run-dir": join(baseDir, "runs"),
    "server-url": serverUrl,
    "websocket-url": `${serverUrl.replace(/^http/, "ws")}/ws`,
  });
  const { app } = await createApp({
    database: new ParkerDatabase(":memory:"),
    network: "regtest",
    websocketUrl: `${serverUrl.replace(/^http/, "ws")}/ws`,
  });
  await app.listen({ host: "127.0.0.1", port: serverPort });

  const env: ScenarioEnvironment = {
    alpha: createHarnessPeer("alpha", "player", config, alphaPort),
    app,
    baseDir,
    beta: createHarnessPeer("beta", "player", config, betaPort),
    gamma: createHarnessPeer("gamma", "player", config, gammaPort),
    host: createHarnessPeer("host", "host", config, hostPort),
    serverUrl,
    witness: createHarnessPeer("witness", "witness", config, witnessPort),
  };

  await Promise.all([
    env.host.daemon.start(),
    env.witness.daemon.start(),
    env.alpha.daemon.start(),
    env.beta.daemon.start(),
    env.gamma.daemon.start(),
  ]);

  const [hostBootstrap, witnessBootstrap, alphaBootstrap, betaBootstrap, gammaBootstrap] = await Promise.all([
    env.host.client.bootstrap("Host"),
    env.witness.client.bootstrap("Witness"),
    env.alpha.client.bootstrap("Alpha"),
    env.beta.client.bootstrap("Beta"),
    env.gamma.client.bootstrap("Gamma"),
  ]);
  env.host.walletPlayerId = (hostBootstrap as any).mesh?.walletPlayerId;
  env.witness.walletPlayerId = (witnessBootstrap as any).mesh?.walletPlayerId;
  env.alpha.walletPlayerId = (alphaBootstrap as any).mesh?.walletPlayerId;
  env.beta.walletPlayerId = (betaBootstrap as any).mesh?.walletPlayerId;
  env.gamma.walletPlayerId = (gammaBootstrap as any).mesh?.walletPlayerId;

  await fundWallet(env.alpha.client, env.alpha.profile);
  await fundWallet(env.beta.client, env.beta.profile);
  if (options?.fundGamma ?? true) {
    await fundWallet(env.gamma.client, env.gamma.profile);
  }

  return env;
}

async function bootstrapMeshTable(env: ScenarioEnvironment, args: { publicTable: boolean }) {
  const witnessStatus = await env.witness.client.inspect(false);
  const witnessPeerUrl = (witnessStatus.state as any)?.mesh?.peer?.peerUrl;
  assert(typeof witnessPeerUrl === "string" && witnessPeerUrl.length > 0, "witness peer URL is unavailable");

  const witnessPeer = (await env.host.client.meshBootstrapPeer(witnessPeerUrl, "Witness", ["witness"])) as {
    peerId: string;
  };
  const created = (await env.host.client.meshCreateTable({
    bigBlindSats: LIVE_BIG_BLIND_SATS,
    buyInMaxSats: BUY_IN_SATS,
    buyInMinSats: BUY_IN_SATS,
    name: `${args.publicTable ? "Public" : "Private"} Regtest Table`,
    public: args.publicTable,
    smallBlindSats: LIVE_SMALL_BLIND_SATS,
    witnessPeerIds: [witnessPeer.peerId],
  })) as {
    inviteCode: string;
    table: { tableId: string };
  };
  env.tableId = created.table.tableId;

  if (args.publicTable) {
    await waitFor(async () => {
      const tables = (await env.gamma.client.meshPublicTables()) as Array<{ advertisement: { tableId: string } }>;
      return tables.some((table) => table.advertisement.tableId === env.tableId);
    }, 20_000);
  }

  await Promise.all([
    env.alpha.client.meshTableJoin(created.inviteCode, BUY_IN_SATS),
    env.beta.client.meshTableJoin(created.inviteCode, BUY_IN_SATS),
  ]);

  await waitFor(async () => {
    const state = (await env.alpha.client.meshGetTable(env.tableId!)) as MeshTableView;
    return Boolean(state.publicState?.handId);
  }, 30_000);
}

async function playHandUntilSettled(
  alpha: DaemonHarnessPeer,
  beta: DaemonHarnessPeer,
  tableId: string,
  args: { plan: "all-in-then-default" | "default" | "raise-then-default" },
) {
  const actions: Array<{ action: MeshPlayerActionPayload["type"]; actor: string; phase: string | null }> = [];
  let specialActionUsed = false;

  for (let turn = 0; turn < 24; turn += 1) {
    const state = (await alpha.client.meshGetTable(tableId)) as MeshTableView;
    const publicState = state.publicState;
    if (!publicState || publicState.phase === "settled") {
      return actions;
    }
    const actor = currentActor(alpha, beta, publicState);
    const payload =
      !specialActionUsed && args.plan === "raise-then-default"
        ? chooseRaiseAction(publicState, actor)
        : !specialActionUsed && args.plan === "all-in-then-default"
          ? chooseAllInAction(publicState, actor)
          : nextDefaultAction(publicState, actor);
    specialActionUsed ||= payload.type === "raise" || payload.type === "bet";
    try {
      await sendActionWithRetry(actor, payload, tableId);
      actions.push({
        action: payload.type,
        actor: actor.profile,
        phase: publicState.phase,
      });
    } catch (error) {
      throw error;
    }
    await waitFor(async () => {
      const next = (await alpha.client.meshGetTable(tableId)) as MeshTableView;
      return next.events.length > state.events.length;
    }, 10_000);
  }

  throw new Error(`hand ${tableId} did not settle in time`);
}

function chooseRaiseAction(publicState: PublicTableState, actor: DaemonHarnessPeer): MeshPlayerActionPayload {
  const playerId = currentPlayerId(actor);
  const contribution = publicState.roundContributions[playerId] ?? 0;
  const toCall = Math.max(0, publicState.currentBetSats - contribution);
  if (toCall > 0) {
    return { type: "raise", totalSats: Math.max(publicState.minRaiseToSats, LIVE_BIG_BLIND_SATS) };
  }
  return { type: "bet", totalSats: Math.max(publicState.minRaiseToSats || LIVE_BIG_BLIND_SATS, LIVE_BIG_BLIND_SATS) };
}

function chooseAllInAction(publicState: PublicTableState, actor: DaemonHarnessPeer): MeshPlayerActionPayload {
  const playerId = currentPlayerId(actor);
  const contribution = publicState.roundContributions[playerId] ?? 0;
  const maxTotalSats = contribution + (publicState.chipBalances[playerId] ?? 0);
  return publicState.currentBetSats > contribution
    ? { type: "raise", totalSats: maxTotalSats }
    : { type: "bet", totalSats: maxTotalSats };
}

function currentActor(alpha: DaemonHarnessPeer, beta: DaemonHarnessPeer, publicState: PublicTableState | null | undefined) {
  assert(publicState, "public state is unavailable");
  const alphaPlayerId = currentPlayerId(alpha);
  const alphaSeat = publicState.seatedPlayers.find((player) => player.playerId === alphaPlayerId);
  assert(alphaSeat, "alpha seat is missing");
  return publicState.actingSeatIndex === alphaSeat.seatIndex ? alpha : beta;
}

function currentPlayerId(peer: DaemonHarnessPeer) {
  const playerId = peer.walletPlayerId;
  assert(typeof playerId === "string" && playerId.length > 0, `player id for ${peer.profile} is unavailable`);
  return playerId;
}

function nextDefaultAction(publicState: PublicTableState, actor: DaemonHarnessPeer): MeshPlayerActionPayload {
  const playerId = currentPlayerId(actor);
  const contribution = publicState.roundContributions[playerId] ?? 0;
  const toCall = Math.max(0, publicState.currentBetSats - contribution);
  return toCall > 0 ? { type: "call" } : { type: "check" };
}

async function sendActionWithRetry(
  actor: DaemonHarnessPeer,
  payload: MeshPlayerActionPayload,
  tableId: string,
) {
  for (let attempt = 0; attempt < 40; attempt += 1) {
    try {
      await actor.client.meshSendAction(payload, tableId);
      return;
    } catch (error) {
      if (
        (error as Error).message.includes("cannot act while") ||
        (error as Error).message.includes("hand is still starting") ||
        (error as Error).message.includes("hand is not active")
      ) {
        await delay(100);
        continue;
      }
      throw error;
    }
  }
  throw new Error(`action ${payload.type} did not become valid in time`);
}

async function waitForLatestSettlementSnapshot(
  client: DaemonRpcClient,
  tableId: string,
  handNumber: number,
  minEpoch = 0,
) {
  return (await waitForValue(async () => {
    const state = (await client.meshGetTable(tableId)) as MeshTableView;
    if (
      state.latestFullySignedSnapshot &&
      state.latestFullySignedSnapshot.epoch >= minEpoch &&
      state.latestFullySignedSnapshot.handNumber >= handNumber &&
      (state.latestFullySignedSnapshot.phase === null || state.latestFullySignedSnapshot.phase === "settled")
    ) {
      return state;
    }
    return null;
  }, 30_000)) as MeshTableView;
}

async function fundWallet(client: DaemonRpcClient, profile: string) {
  log("wallet-funding", { profile, stage: "faucet" });
  await client.walletFaucet(FAUCET_AMOUNT_SATS);
  log("wallet-funding", { profile, stage: "onboard" });
  let onboardTxid: string | undefined;
  const startedAt = Date.now();
  while (Date.now() - startedAt < 180_000) {
    const wallet = await client.walletSummary();
    if (wallet.availableSats >= WALLET_MIN_AVAILABLE_SATS) {
      log("wallet-funded", { onboardTxid, profile });
      return;
    }
    try {
      onboardTxid = (await client.walletOnboard()) as string;
    } catch (error) {
      const message = (error as Error).message;
      if (
        message.includes("No boarding utxos available after deducting fees") ||
        message.includes("missing inputs") ||
        message.includes("missingorspent") ||
        message.includes("already registered by another intent") ||
        message.includes("timed out")
      ) {
        await delay(1_000);
        continue;
      }
      throw error;
    }
    await delay(1_000);
  }
  const wallet = await client.walletSummary();
  throw new Error(`wallet ${profile} never reached ${WALLET_MIN_AVAILABLE_SATS} available sats (available=${wallet.availableSats}, total=${wallet.totalSats})`);
}

async function waitForWallet(client: DaemonRpcClient, minimumAvailableSats: number, timeoutMs: number) {
  await waitFor(async () => {
    const wallet = await client.walletSummary();
    return wallet.availableSats >= minimumAvailableSats;
  }, timeoutMs);
}

function createHarnessPeer(profile: string, mode: "host" | "player" | "witness", config: ReturnType<typeof resolveCliRuntimeConfig>, peerPort: number): DaemonHarnessPeer {
  const peerConfig = {
    ...config,
    peerPort,
  };
  return {
    alias: profile,
    client: new DaemonRpcClient(profile, peerConfig),
    daemon: new ProfileDaemon(profile, peerConfig, mode),
    profile,
  };
}

async function shutdownScenarioEnvironment(env: ScenarioEnvironment) {
  await Promise.allSettled([
    env.alpha.daemon.stop(),
    env.beta.daemon.stop(),
    env.gamma.daemon.stop(),
    env.host.daemon.stop(),
    env.witness.daemon.stop(),
  ]);
  await env.app.close();
}

async function readFundsState(env: ScenarioEnvironment, profile: string) {
  const path = join(env.baseDir, "daemons", `${profile.replace(/[^a-zA-Z0-9_-]/g, "_")}.table-funds.json`);
  for (let attempt = 0; attempt < 4; attempt += 1) {
    try {
      const raw = await readFile(path, "utf8");
      if (!raw.trim()) {
        await delay(25);
        continue;
      }
      return JSON.parse(raw) as Record<string, unknown>;
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code === "ENOENT") {
        return { path, status: "missing" } as const;
      }
      if (error instanceof SyntaxError && attempt < 3) {
        await delay(25);
        continue;
      }
      throw error;
    }
  }
  return { path, status: "unreadable" } as const;
}

function log(event: string, data: Record<string, unknown>) {
  process.stdout.write(`${JSON.stringify({ data, event, timestamp: new Date().toISOString() })}\n`);
}

function parseArgs(argv: string[]): HarnessArgs {
  const args: HarnessArgs = {
    keepNigiri: false,
    startNigiri: false,
  };
  for (let index = 0; index < argv.length; index += 1) {
    const value = argv[index];
    switch (value) {
      case "--base-dir":
        args.baseDir = argv[index + 1];
        index += 1;
        break;
      case "--keep-nigiri":
        args.keepNigiri = true;
        break;
      case "--start-nigiri":
        args.startNigiri = true;
        break;
      default:
        throw new Error(`unknown argument ${value}`);
    }
  }
  return args;
}

async function runCommand(command: string, args: string[]) {
  const child = spawn(command, args, {
    stdio: "inherit",
  });
  await new Promise<void>((resolve, reject) => {
    child.once("error", reject);
    child.once("exit", (code) => {
      if (code === 0) {
        resolve();
        return;
      }
      reject(new Error(`${command} ${args.join(" ")} exited with code ${code ?? "unknown"}`));
    });
  });
}

async function runCommandOutput(command: string, args: string[]) {
  const child = spawn(command, args, {
    stdio: ["ignore", "pipe", "pipe"],
  });
  let stdout = "";
  let stderr = "";
  child.stdout.on("data", (chunk) => {
    stdout += chunk.toString();
  });
  child.stderr.on("data", (chunk) => {
    stderr += chunk.toString();
  });
  await new Promise<void>((resolve, reject) => {
    child.once("error", reject);
    child.once("exit", (code) => {
      if (code === 0) {
        resolve();
        return;
      }
      reject(new Error(stderr.trim() || `${command} ${args.join(" ")} exited with code ${code ?? "unknown"}`));
    });
  });
  return stdout;
}

async function runNigiriCommand(datadir: string | undefined, args: string[]) {
  const prefixedArgs = datadir ? ["--datadir", datadir, ...args] : args;
  await runCommand("nigiri", prefixedArgs);
}

async function runNigiriCommandOutput(datadir: string | undefined, args: string[]) {
  const prefixedArgs = datadir ? ["--datadir", datadir, ...args] : args;
  return await runCommandOutput("nigiri", prefixedArgs);
}

function resolveNigiriDatadir(baseDir: string) {
  return join(
    homedir(),
    "Library",
    "Application Support",
    "Nigiri",
    "parker-mesh-regtest",
    baseDir.replace(/[^a-zA-Z0-9_-]/g, "_"),
  );
}

async function ensureNigiriStarted(datadir: string) {
  await mkdir(datadir, { recursive: true });
  try {
    await runNigiriCommand(datadir, ["start", "--ark", "--ln", "--ci"]);
  } catch (error) {
    log("nigiri-start-nonzero", {
      datadir,
      message: error instanceof Error ? error.message : String(error),
    });
  }

  const info = await waitForNigiriReady(60_000);
  await seedArkLiquidity(datadir);
  log("nigiri-ready", {
    datadir,
    signerPubkey: info.signerPubkey,
    version: info.version,
    vtxoMinAmount: info.vtxoMinAmount,
  });
}

async function waitForNigiriReady(timeoutMs: number) {
  return await waitForValue(async () => {
    try {
      const response = await fetch("http://127.0.0.1:7070/v1/info");
      if (!response.ok) {
        return null;
      }
      return (await response.json()) as {
        signerPubkey: string;
        version: string;
        vtxoMinAmount: string;
      };
    } catch {
      return null;
    }
  }, timeoutMs);
}

async function seedArkLiquidity(datadir: string) {
  await waitForValue(async () => {
    try {
      const status = await runNigiriCommandOutput(datadir, ["arkd", "wallet", "status"]);
      return status.includes("unlocked: true") && status.includes("synced: true") ? status : null;
    } catch {
      return null;
    }
  }, 30_000);

  const address = (await runNigiriCommandOutput(datadir, ["arkd", "wallet", "address"])).trim();
  assert(address.length > 0, "arkd wallet address is unavailable");

  for (let index = 0; index < 10; index += 1) {
    await runNigiriCommand(datadir, ["faucet", address]);
  }

  const balanceText = await waitForValue(async () => {
    try {
      const output = await runNigiriCommandOutput(datadir, ["arkd", "wallet", "balance"]);
      return readArkdMainBalance(output) > 0 ? output : null;
    } catch {
      return null;
    }
  }, 30_000);
  log("ark-liquidity-seeded", {
    address,
    balance: readArkdMainBalance(balanceText),
    datadir,
  });
}

function readArkdMainBalance(output: string) {
  const match = output.match(/main account\s+available:\s+([0-9.]+)/m);
  return match ? Number(match[1]) : 0;
}

async function getPeerPorts(count: number) {
  return await Promise.all(Array.from({ length: count }, () => getFreePort()));
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

async function waitFor(predicate: () => boolean | Promise<boolean>, timeoutMs: number) {
  const start = Date.now();
  while (Date.now() - start < timeoutMs) {
    if (await predicate()) {
      return;
    }
    await delay(100);
  }
  throw new Error(`condition was not met within ${timeoutMs}ms`);
}

async function waitForValue<T>(producer: () => Promise<T | null>, timeoutMs: number): Promise<T> {
  const start = Date.now();
  while (Date.now() - start < timeoutMs) {
    const value = await producer();
    if (value !== null) {
      return value;
    }
    await delay(100);
  }
  throw new Error(`condition was not met within ${timeoutMs}ms`);
}

function delay(ms: number) {
  return new Promise<void>((resolve) => {
    setTimeout(resolve, ms);
  });
}

function sumValues(values: Record<string, number>) {
  return Object.values(values).reduce((total, value) => total + value, 0);
}

function assert(condition: unknown, message: string): asserts condition {
  if (!condition) {
    throw new Error(message);
  }
}

void main().catch((error) => {
  process.stderr.write(`${(error as Error).message}\n`);
  process.exitCode = 1;
});
