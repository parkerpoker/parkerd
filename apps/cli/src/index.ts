import { createInterface } from "node:readline/promises";
import { stdin as input, stdout as output } from "node:process";

import {
  CliLogger,
  DaemonRpcClient,
  resolveCliRuntimeConfig,
  type CliFlagMap,
  type MeshRuntimeMode,
} from "@parker/daemon-runtime";

async function main(argv: string[]) {
  const { command, flags, positionals } = parseArgv(argv);
  const config = resolveCliRuntimeConfig(flags);
  const logger = new CliLogger(config.outputJson);

  if (!command || command === "help") {
    printHelp();
    return;
  }

  if (command === "daemon") {
    const profile = requireFlag(flags, "profile", "player1");
    await runDaemonCommand(profile, positionals, flags, config, logger);
    return;
  }

  const profile = requireFlag(flags, "profile", "player1");
  const client = new DaemonRpcClient(profile, config);
  try {
    if (command === "network") {
      switch (positionals[0]) {
        case "peers":
          logger.result(await client.meshNetworkPeers());
          return;
        case "bootstrap":
          if (positionals[1] !== "add") {
            throw new Error("network bootstrap requires the `add` subcommand");
          }
          logger.result(await client.meshBootstrapPeer(requirePositional(positionals[2], "peerUrl"), positionals[3]));
          return;
        default:
          throw new Error(`unknown network subcommand ${positionals[0] ?? ""}`.trim());
      }
    }

    if (command === "table") {
      switch (positionals[0]) {
        case "create":
          logger.result(
            await client.meshCreateTable({
              ...(flags.name ? { name: requireFlag(flags, "name") } : {}),
              ...(flags.public ? { public: true } : {}),
            }),
          );
          return;
        case "announce":
          logger.result(await client.meshTableAnnounce(positionals[1]));
          return;
        case "join":
          logger.result(
            await client.meshTableJoin(
              requirePositional(positionals[1], "inviteCode"),
              parseOptionalNumber(positionals[2], 4_000),
            ),
          );
          return;
        case "watch":
          if (positionals[1]) {
            logger.result(await client.meshGetTable(positionals[1]));
            return;
          }
          await runDaemonWatch(client, logger);
          return;
        case "rotate-host":
          logger.result(await client.meshRotateHost(positionals[1]));
          return;
        case "action":
          logger.result(await client.meshSendAction(parseMeshActionPayload(positionals.slice(1))));
          return;
        case "public":
          logger.result(await client.meshPublicTables());
          return;
        default:
          throw new Error(`unknown table subcommand ${positionals[0] ?? ""}`.trim());
      }
    }

    if (command === "funds") {
      switch (positionals[0]) {
        case "buy-in":
          logger.result(
            await client.meshTableJoin(
              requirePositional(positionals[1], "inviteCode"),
              parseOptionalNumber(positionals[2], 4_000),
            ),
          );
          return;
        case "cashout":
          logger.result(await client.meshCashOut(positionals[1]));
          return;
        case "renew":
          logger.result(await client.meshRenew(positionals[1]));
          return;
        case "exit":
          logger.result(await client.meshExit(positionals[1]));
          return;
        default:
          throw new Error(`unknown funds subcommand ${positionals[0] ?? ""}`.trim());
      }
    }

    if (command === "wallet") {
      switch (positionals[0] ?? "summary") {
        case "summary":
          logger.result(await client.walletSummary());
          return;
        case "deposit":
          logger.result(await client.walletDeposit(parseRequiredNumber(positionals[1], "amountSats")));
          return;
        case "withdraw":
          logger.result(
            await client.walletWithdraw(
              parseRequiredNumber(positionals[1], "amountSats"),
              requirePositional(positionals[2], "invoice"),
            ),
          );
          return;
        case "faucet":
          await client.walletFaucet(parseRequiredNumber(positionals[1], "amountSats"));
          logger.result(await client.walletSummary());
          return;
        case "onboard":
          logger.result({ txid: await client.walletOnboard(), wallet: await client.walletSummary() });
          return;
        case "offboard":
          logger.result({
            txid: await client.walletOffboard(
              requirePositional(positionals[1], "address"),
              positionals[2] ? parseRequiredNumber(positionals[2], "amountSats") : undefined,
            ),
          });
          return;
        default:
          throw new Error(`unknown wallet subcommand ${positionals[0] ?? ""}`.trim());
      }
    }

    switch (command) {
      case "interactive":
        await runInteractive(client, logger);
        break;
      case "bootstrap":
        logger.result(await client.bootstrap(positionals[0]));
        break;
      default:
        throw new Error(`unknown command ${command}`);
    }
  } finally {
    client.close();
  }
}

async function runDaemonWatch(client: DaemonRpcClient, logger: CliLogger) {
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
        if (command === "bootstrap") {
          logger.result(await client.bootstrap(args[0]));
          continue;
        }
        if (command === "wallet") {
          const [subcommand = "summary", ...rest] = args;
          switch (subcommand) {
            case "summary":
              logger.result(await client.walletSummary());
              break;
            case "deposit":
              logger.result(await client.walletDeposit(parseRequiredNumber(rest[0], "amountSats")));
              break;
            case "withdraw":
              logger.result(
                await client.walletWithdraw(
                  parseRequiredNumber(rest[0], "amountSats"),
                  requirePositional(rest[1], "invoice"),
                ),
              );
              break;
            case "faucet":
              await client.walletFaucet(parseRequiredNumber(rest[0], "amountSats"));
              logger.result(await client.walletSummary());
              break;
            case "onboard":
              logger.result(await client.walletOnboard());
              break;
            case "offboard":
              logger.result(await client.walletOffboard(requirePositional(rest[0], "address"), rest[1] ? parseRequiredNumber(rest[1], "amountSats") : undefined));
              break;
            default:
              logger.error(`unknown wallet subcommand ${subcommand}`);
          }
          continue;
        }
        if (command === "table" && args[0] === "action") {
          logger.result(await client.meshSendAction(parseMeshActionPayload(args.slice(1))));
          continue;
        }
        logger.error(`unknown interactive command ${command}`);
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
  flags: CliFlagMap,
  config: ReturnType<typeof resolveCliRuntimeConfig>,
  logger: CliLogger,
) {
  const subcommand = positionals[0] ?? "status";
  const client = new DaemonRpcClient(profile, config);
  const mode = parseMeshRuntimeMode(flags.mode);

  switch (subcommand) {
    case "start":
      await client.ensureRunning(mode);
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

function parseMeshActionPayload(positionals: string[]) {
  const type = requirePositional(positionals[0], "actionType");
  if (type === "bet" || type === "raise") {
    return {
      type,
      totalSats: parseRequiredNumber(positionals[1], "totalSats"),
    } as const;
  }
  if (type === "fold" || type === "check" || type === "call") {
    return { type } as const;
  }
  throw new Error(`unsupported mesh action ${type}`);
}

function parseMeshRuntimeMode(value: string | boolean | undefined): MeshRuntimeMode | undefined {
  if (value === "player" || value === "host" || value === "witness" || value === "indexer") {
    return value;
  }
  return undefined;
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
      "  wallet [summary|deposit <sats>|withdraw <sats> <invoice>|faucet <sats>|onboard|offboard <address> [sats]] --profile <name>",
      "  network peers|bootstrap add <peerUrl> [alias] --profile <name>",
      "  table create [--name <name>] [--public] | announce [tableId] | join <invite> [buyIn] | public | watch [tableId] | rotate-host [tableId] | action <fold|check|call|bet|raise> [sats] --profile <name>",
      "  funds buy-in <invite> [buyIn] | cashout [tableId] | renew [tableId] | exit [tableId] --profile <name>",
      "  daemon <start|status|stop|watch> --profile <name> [--mode <player|host|witness|indexer>]",
      "  interactive --profile <name>",
      "Shared flags:",
      "  --network <regtest|mutinynet> --indexer-url <url> --ark-server-url <url> --boltz-url <url> --peer-host <host> --peer-port <port> --mock --json",
      "",
    ].join("\n"),
  );
}

void main(process.argv.slice(2)).catch((error) => {
  process.stderr.write(`${(error as Error).message}\n`);
  process.exitCode = 1;
});
