import { mkdir, readFile, writeFile } from "node:fs/promises";
import { join } from "node:path";

import type { CreateTableRequest, SignedActionPayload } from "@parker/protocol";

import { type CliRuntimeConfig } from "./config.js";
import { DaemonRpcClient } from "./daemonClient.js";
import { CliLogger } from "./logger.js";

export type ScenarioStep =
  | { type: "bootstrap"; nickname?: string }
  | { type: "connect-table" }
  | { type: "create-table"; table?: Partial<CreateTableRequest> }
  | { type: "deposit"; amountSats: number }
  | { type: "join-table"; buyInSats?: number; inviteCode?: string }
  | { type: "nigiri-faucet"; amountSats: number }
  | { type: "offboard"; address: string; amountSats?: number }
  | { type: "onboard" }
  | { type: "peer-send"; message: string }
  | { type: "reveal-seed" }
  | { type: "sleep"; ms: number }
  | { type: "snapshot" }
  | { type: "transcript"; fileName?: string }
  | {
      type: "wait-for";
      condition:
        | "all-committed"
        | "all-revealed"
        | "my-turn"
        | "opponent-seated"
        | "peer-direct"
        | "peer-relay"
        | "phase"
        | "snapshot";
      timeoutMs?: number;
      value?: string;
    }
  | { type: "commit-seed" }
  | {
      type: "action";
      action: SignedActionPayload["type"];
      amountSats?: number;
    }
  | { type: "withdraw"; amountSats: number; invoice: string };

export interface PlayerScenario {
  inviteCode?: string;
  nickname?: string;
  profile: string;
  steps: ScenarioStep[];
}

export async function loadPlayerScenario(path: string): Promise<PlayerScenario> {
  return JSON.parse(await readFile(path, "utf8")) as PlayerScenario;
}

export async function runPlayerScenario(args: {
  config: CliRuntimeConfig;
  logger: CliLogger;
  runDir?: string | undefined;
  scenario: PlayerScenario;
}) {
  const client = new DaemonRpcClient(args.scenario.profile, args.config);
  try {
    for (const step of args.scenario.steps) {
      await runScenarioStep({
        client,
        logger: args.logger,
        runDir: args.runDir,
        scenario: args.scenario,
        step,
      });
    }
    return client.currentState();
  } finally {
    client.close();
  }
}

async function runScenarioStep(args: {
  client: DaemonRpcClient;
  logger: CliLogger;
  runDir?: string | undefined;
  scenario: PlayerScenario;
  step: ScenarioStep;
}) {
  switch (args.step.type) {
    case "bootstrap": {
      const result = await args.client.bootstrap(args.step.nickname ?? args.scenario.nickname);
      args.logger.info("profile bootstrapped", {
        nickname: result.state.nickname,
        playerId: result.identity.playerId,
      });
      return;
    }
    case "connect-table": {
      await args.client.connectCurrentTable();
      return;
    }
    case "create-table": {
      const created = await args.client.createTable(args.step.table);
      if (args.runDir) {
        await writeSharedInviteCode(args.runDir, created.table.inviteCode);
      }
      args.logger.info("table created", {
        inviteCode: created.table.inviteCode,
        tableId: created.table.tableId,
      });
      return;
    }
    case "deposit": {
      const quote = await args.client.walletDeposit(args.step.amountSats);
      args.logger.info("deposit quote created", quote);
      return;
    }
    case "join-table": {
      const inviteCode =
        args.step.inviteCode ??
        args.scenario.inviteCode ??
        (args.runDir ? await waitForSharedInviteCode(args.runDir) : undefined);
      if (!inviteCode) {
        throw new Error("join-table step requires an invite code or run directory");
      }
      const snapshot = await args.client.joinTable(inviteCode, args.step.buyInSats);
      args.logger.info("joined table", {
        inviteCode,
        tableId: snapshot.table.tableId,
      });
      return;
    }
    case "nigiri-faucet": {
      await args.client.walletFaucet(args.step.amountSats);
      return;
    }
    case "offboard": {
      const txid = await args.client.walletOffboard(args.step.address, args.step.amountSats);
      args.logger.info("offboard submitted", { txid });
      return;
    }
    case "onboard": {
      const txid = await args.client.walletOnboard();
      args.logger.info("onboard submitted", { txid });
      return;
    }
    case "peer-send": {
      await args.client.sendPeerMessage(args.step.message);
      return;
    }
    case "reveal-seed": {
      await args.client.commitSeed(true);
      return;
    }
    case "sleep": {
      await delay(args.step.ms);
      return;
    }
    case "snapshot": {
      args.logger.info("snapshot", await args.client.getSnapshot());
      return;
    }
    case "transcript": {
      const transcript = await args.client.getTranscript();
      if (args.runDir) {
        const fileName = args.step.fileName ?? `${args.scenario.profile}-transcript.json`;
        await mkdir(args.runDir, { recursive: true });
        await writeFile(join(args.runDir, fileName), `${JSON.stringify(transcript, null, 2)}\n`, "utf8");
      }
      args.logger.info("transcript captured", {
        checkpoints: transcript.checkpoints.length,
        events: transcript.events.length,
      });
      return;
    }
    case "wait-for": {
      const step = args.step;
      await args.client.waitForCondition(
        step.condition,
        (snapshot, peerStatus) => {
          if (step.condition === "snapshot") {
            return Boolean(snapshot);
          }
          if (step.condition === "peer-direct" || step.condition === "peer-relay") {
            return peerStatus === "relay";
          }
          if (!snapshot) {
            return false;
          }
          switch (step.condition) {
            case "opponent-seated":
              return snapshot.seats.every((seat) => seat.player.playerId !== "open-seat");
            case "all-committed":
              return snapshot.commitments.length === 2;
            case "all-revealed":
              return (
                snapshot.commitments.length === 2 &&
                snapshot.commitments.every((commitment) => Boolean(commitment.revealSeed))
              );
            case "my-turn": {
              const identity = args.client.currentState().identity;
              const seat = snapshot.seats.find((candidate) => candidate.player.playerId === identity?.playerId);
              return Boolean(snapshot.checkpoint && seat && snapshot.checkpoint.actingSeatIndex === seat.seatIndex);
            }
            case "phase":
              return snapshot.checkpoint?.phase === step.value;
            default:
              return false;
          }
        },
        step.timeoutMs,
      );
      return;
    }
    case "commit-seed": {
      await args.client.commitSeed(false);
      return;
    }
    case "action": {
      await args.client.sendAction(buildActionPayload(args.step.action, args.step.amountSats));
      return;
    }
    case "withdraw": {
      const status = await args.client.walletWithdraw(args.step.amountSats, args.step.invoice);
      args.logger.info("withdrawal submitted", status);
      return;
    }
  }
}

function buildActionPayload(type: SignedActionPayload["type"], amountSats?: number): SignedActionPayload {
  switch (type) {
    case "bet":
      return { type: "bet", totalSats: amountSats ?? 0 };
    case "raise":
      return { type: "raise", totalSats: amountSats ?? 0 };
    case "call":
      return { type: "call" };
    case "check":
      return { type: "check" };
    case "fold":
      return { type: "fold" };
    default:
      throw new Error(`unsupported scenario action ${String(type)}`);
  }
}

function delay(ms: number) {
  return new Promise<void>((resolve) => {
    setTimeout(resolve, ms);
  });
}

async function waitForSharedInviteCode(runDir: string, timeoutMs = 30_000) {
  const start = Date.now();
  const file = join(runDir, "invite-code.txt");
  while (Date.now() - start < timeoutMs) {
    try {
      const code = (await readFile(file, "utf8")).trim();
      if (code) {
        return code;
      }
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "ENOENT") {
        throw error;
      }
    }
    await delay(250);
  }
  throw new Error("timed out waiting for shared invite code");
}

async function writeSharedInviteCode(runDir: string, inviteCode: string) {
  await mkdir(runDir, { recursive: true });
  await writeFile(join(runDir, "invite-code.txt"), `${inviteCode}\n`, "utf8");
}
