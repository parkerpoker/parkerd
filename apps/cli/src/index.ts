import { createInterface } from "node:readline/promises";
import { stdin as input, stdout as output } from "node:process";

import type { SignedActionPayload } from "@parker/protocol";

import { resolveCliRuntimeConfig, type CliFlagMap } from "./config.js";
import { DaemonRpcClient } from "./daemonClient.js";
import { runHarness, loadHarnessScenario } from "./harness.js";
import { CliLogger } from "./logger.js";
import { loadPlayerScenario, runPlayerScenario } from "./scenario.js";

async function main(argv: string[]) {
  const { command, flags, positionals } = parseArgv(argv);
  const config = resolveCliRuntimeConfig(flags);
  const logger = new CliLogger(config.outputJson);

  if (!command || command === "help") {
    printHelp();
    return;
  }

  if (command === "run-harness") {
    const scenarioFile = requireFlag(flags, "scenario-file");
    await runHarness({
      config,
      logger,
      scenario: await loadHarnessScenario(scenarioFile),
    });
    return;
  }

  if (command === "play-scenario") {
    const scenarioFile = requireFlag(flags, "scenario-file");
    const scenario = await loadPlayerScenario(scenarioFile);
    const scopedLogger = new CliLogger(config.outputJson, scenario.profile);
    const runDir = typeof flags["run-dir"] === "string" ? flags["run-dir"] : undefined;
    scopedLogger.result(
      await runPlayerScenario({
        config,
        logger: scopedLogger,
        ...(runDir ? { runDir } : {}),
        scenario,
      }),
    );
    return;
  }

  if (command === "daemon") {
    const profile = requireFlag(flags, "profile", "player1");
    await runDaemonCommand(profile, positionals, config, logger);
    return;
  }

  const profile = requireFlag(flags, "profile", "player1");
  const client = new DaemonRpcClient(profile, config);
  try {
    switch (command) {
      case "interactive":
        await runInteractive(client, logger);
        break;
      case "bootstrap":
        logger.result(await client.bootstrap(positionals[0]));
        break;
      case "wallet":
        logger.result(await client.walletSummary());
        break;
      case "deposit":
        logger.result(await client.walletDeposit(parseRequiredNumber(positionals[0], "amountSats")));
        break;
      case "withdraw":
        logger.result(
          await client.walletWithdraw(
            parseRequiredNumber(positionals[0], "amountSats"),
            requirePositional(positionals[1], "invoice"),
          ),
        );
        break;
      case "faucet":
        await client.walletFaucet(parseRequiredNumber(positionals[0], "amountSats"));
        logger.result(await client.walletSummary());
        break;
      case "onboard":
        logger.result({ txid: await client.walletOnboard(), wallet: await client.walletSummary() });
        break;
      case "offboard":
        logger.result({
          txid: await client.walletOffboard(
            requirePositional(positionals[0], "address"),
            positionals[1] ? parseRequiredNumber(positionals[1], "amountSats") : undefined,
          ),
        });
        break;
      case "create-table":
        logger.result(await client.createTable({}));
        break;
      case "join-table":
        logger.result(await client.joinTable(requirePositional(positionals[0], "inviteCode"), parseOptionalNumber(positionals[1], 4_000)));
        break;
      case "connect":
        await client.connectCurrentTable();
        logger.result(client.currentState());
        break;
      case "snapshot":
        logger.result(await client.getSnapshot());
        break;
      case "transcript":
        logger.result(await client.getTranscript());
        break;
      case "commit":
        logger.result(await client.commitSeed(false));
        break;
      case "reveal":
        logger.result(await client.commitSeed(true));
        break;
      case "action":
        logger.result(await client.sendAction(parseActionPayload(positionals)));
        break;
      case "peer-send":
        await client.connectCurrentTable();
        await client.sendPeerMessage(positionals.join(" "));
        logger.result(client.currentState());
        break;
      default:
        throw new Error(`unknown command ${command}`);
    }
  } finally {
    client.close();
  }
}

async function runInteractive(client: DaemonRpcClient, logger: CliLogger) {
  const rl = createInterface({ input, output });
  logger.info("interactive mode ready; type `help` for commands");
  try {
    for (;;) {
      const line = (await rl.question("parker> ")).trim();
      if (!line) {
        continue;
      }
      if (line === "exit" || line === "quit") {
        break;
      }
      if (line === "help") {
        printHelp();
        continue;
      }

      const parts = line.split(/\s+/);
      const [command, ...args] = parts;
      try {
        switch (command) {
          case "bootstrap":
            logger.result(await client.bootstrap(args[0]));
            break;
          case "wallet":
            logger.result(await client.walletSummary());
            break;
          case "deposit":
            logger.result(await client.walletDeposit(parseRequiredNumber(args[0], "amountSats")));
            break;
          case "withdraw":
            logger.result(
              await client.walletWithdraw(
                parseRequiredNumber(args[0], "amountSats"),
                requirePositional(args[1], "invoice"),
              ),
            );
            break;
          case "faucet":
            await client.walletFaucet(parseRequiredNumber(args[0], "amountSats"));
            logger.result(await client.walletSummary());
            break;
          case "onboard":
            logger.result(await client.walletOnboard());
            break;
          case "create-table":
            logger.result(await client.createTable({}));
            break;
          case "join-table":
            logger.result(await client.joinTable(requirePositional(args[0], "inviteCode"), parseOptionalNumber(args[1], 4_000)));
            break;
          case "connect":
            await client.connectCurrentTable();
            logger.result(client.currentState());
            break;
          case "snapshot":
            logger.result(await client.getSnapshot());
            break;
          case "transcript":
            logger.result(await client.getTranscript());
            break;
          case "commit":
            logger.result(await client.commitSeed(false));
            break;
          case "reveal":
            logger.result(await client.commitSeed(true));
            break;
          case "action":
            logger.result(await client.sendAction(parseActionPayload(args)));
            break;
          case "peer-send":
            await client.connectCurrentTable();
            await client.sendPeerMessage(args.join(" "));
            logger.result(client.currentState());
            break;
          default:
            logger.error(`unknown interactive command ${command}`);
        }
      } catch (error) {
        logger.error((error as Error).message);
      }
    }
  } finally {
    rl.close();
  }
}

async function runDaemonCommand(
  profile: string,
  positionals: string[],
  config: ReturnType<typeof resolveCliRuntimeConfig>,
  logger: CliLogger,
) {
  const subcommand = positionals[0] ?? "status";
  const client = new DaemonRpcClient(profile, config);

  switch (subcommand) {
    case "start":
      await client.ensureRunning();
      logger.result(await client.inspect(false));
      return;
    case "status":
      logger.result(await client.inspect(false));
      return;
    case "stop":
      await client.stopDaemon();
      logger.result({ profile, stopping: true });
      return;
    case "watch": {
      await client.ensureRunning();
      const stopWatching = await client.watch((event) => {
        logger.result(event);
      });
      await new Promise<void>((resolve) => {
        const onSignal = () => {
          stopWatching();
          process.off("SIGINT", onSignal);
          process.off("SIGTERM", onSignal);
          resolve();
        };
        process.on("SIGINT", onSignal);
        process.on("SIGTERM", onSignal);
      });
      return;
    }
    default:
      throw new Error(`unknown daemon subcommand ${subcommand}`);
  }
}

function parseActionPayload(positionals: string[]): SignedActionPayload {
  const type = requirePositional(positionals[0], "actionType") as SignedActionPayload["type"];
  if (type === "bet" || type === "raise") {
    return {
      type,
      totalSats: parseRequiredNumber(positionals[1], "totalSats"),
    };
  }
  if (type === "fold" || type === "check" || type === "call") {
    return { type };
  }
  throw new Error(`unsupported action ${type}`);
}

function parseArgv(argv: string[]) {
  const [command, ...rest] = argv;
  const flags: CliFlagMap = {};
  const positionals: string[] = [];

  for (let index = 0; index < rest.length; index += 1) {
    const value = rest[index]!;
    if (!value.startsWith("--")) {
      positionals.push(value);
      continue;
    }

    const keyValue = value.slice(2).split("=");
    const key = keyValue[0]!;
    const inlineValue = keyValue[1];
    if (inlineValue !== undefined) {
      flags[key] = inlineValue;
      continue;
    }

    const next = rest[index + 1];
    if (!next || next.startsWith("--")) {
      flags[key] = true;
      continue;
    }

    flags[key] = next;
    index += 1;
  }

  return {
    command,
    flags,
    positionals,
  };
}

function parseOptionalNumber(value: string | undefined, fallback: number) {
  return value ? parseRequiredNumber(value, "number") : fallback;
}

function parseRequiredNumber(value: string | undefined, label: string) {
  if (!value) {
    throw new Error(`${label} is required`);
  }
  const parsed = Number(value);
  if (!Number.isFinite(parsed)) {
    throw new Error(`${label} must be numeric`);
  }
  return parsed;
}

function requireFlag(flags: CliFlagMap, name: string, fallback?: string) {
  const value = flags[name];
  if (typeof value === "string") {
    return value;
  }
  if (fallback !== undefined) {
    return fallback;
  }
  throw new Error(`--${name} is required`);
}

function requirePositional(value: string | undefined, label: string) {
  if (!value) {
    throw new Error(`${label} is required`);
  }
  return value;
}

function printHelp() {
  process.stdout.write(
    [
      "parker-cli commands:",
      "  bootstrap [nickname] --profile <name>",
      "  wallet|deposit <sats>|withdraw <sats> <invoice>|faucet <sats>|onboard|offboard <address> [sats] --profile <name>",
      "  create-table|join-table <invite> [buyIn]|connect|snapshot|transcript|commit|reveal|action <fold|check|call|bet|raise> [sats]|peer-send <message> --profile <name>",
      "  interactive --profile <name>",
      "  daemon <start|status|stop|watch> --profile <name>",
      "  play-scenario --scenario-file <path> [--run-dir <path>]",
      "  run-harness --scenario-file <path>",
      "Shared flags:",
      "  --network <regtest|mutinynet> --server-url <url> --websocket-url <url> --ark-server-url <url> --boltz-url <url> --mock --json",
      "",
    ].join("\n"),
  );
}

void main(process.argv.slice(2)).catch((error) => {
  process.stderr.write(`${(error as Error).message}\n`);
  process.exitCode = 1;
});
