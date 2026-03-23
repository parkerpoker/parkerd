import { readFile, stat } from "node:fs/promises";
import { extname, resolve } from "node:path";

import Fastify, { type FastifyReply } from "fastify";

import {
  bridgeDaemonWatch,
  DaemonRpcClient,
  ProfileStore,
  resolveCliRuntimeConfig,
  type CliRuntimeConfig,
  type LocalProfileSummary,
  type MeshRuntimeMode,
} from "@parker/daemon-runtime";

export const LOCAL_CONTROLLER_HEADER = "X-Parker-Local-Controller";
const LOCAL_CONTROLLER_HEADER_KEY = LOCAL_CONTROLLER_HEADER.toLowerCase();
const DEFAULT_CONTROLLER_PORT = 3030;
const DEFAULT_DEV_WEB_PORT = 3010;
const INDEX_HTML = "index.html";

export interface CreateControllerAppOptions {
  allowedOrigins?: string[];
  config?: CliRuntimeConfig;
  controllerPort?: number;
  webDistDir?: string;
}

class ControllerError extends Error {
  constructor(
    readonly statusCode: number,
    message: string,
  ) {
    super(message);
  }
}

export async function createControllerApp(options: CreateControllerAppOptions = {}) {
  const config = options.config ?? resolveCliRuntimeConfig({});
  const controllerPort = options.controllerPort ?? DEFAULT_CONTROLLER_PORT;
  const allowedOrigins = options.allowedOrigins ?? resolveAllowedOrigins(controllerPort);
  const allowedOriginSet = new Set(allowedOrigins);
  const profileStore = new ProfileStore(config.profileDir);
  const app = Fastify({ logger: false });
  const hasBundledWeb = options.webDistDir ? await fileExists(resolve(options.webDistDir, INDEX_HTML)) : false;

  app.setErrorHandler(async (error, _request, reply) => {
    const statusCode =
      error instanceof ControllerError
        ? error.statusCode
        : error instanceof Error
          ? mapErrorStatus(error.message)
          : 500;
    const message = error instanceof Error ? error.message : "internal controller error";
    await reply.code(statusCode).send({ error: message, statusCode });
  });

  app.addHook("onRequest", async (request, reply) => {
    if (!request.url.startsWith("/api/local/")) {
      return;
    }

    const origin = request.headers.origin;
    if (origin) {
      reply.header("Access-Control-Allow-Headers", `Content-Type, ${LOCAL_CONTROLLER_HEADER}`);
      reply.header("Access-Control-Allow-Methods", "GET,POST,OPTIONS");
      reply.header("Vary", "Origin");
      if (!allowedOriginSet.has(origin)) {
        throw new ControllerError(403, `origin ${origin} is not allowed by the local controller`);
      }
      reply.header("Access-Control-Allow-Origin", origin);
    }

    if (request.method === "OPTIONS") {
      const requestedHeaders = String(request.headers["access-control-request-headers"] ?? "")
        .split(",")
        .map((header) => header.trim().toLowerCase())
        .filter(Boolean);
      if (!requestedHeaders.includes(LOCAL_CONTROLLER_HEADER_KEY)) {
        throw new ControllerError(400, `${LOCAL_CONTROLLER_HEADER} is required for browser access`);
      }
      await reply.code(204).send();
      return;
    }

    if (!request.headers[LOCAL_CONTROLLER_HEADER_KEY]) {
      throw new ControllerError(400, `${LOCAL_CONTROLLER_HEADER} header is required`);
    }
  });

  app.get("/health", async () => ({
    allowedOrigins,
    bind: "127.0.0.1",
    ok: true,
    profilesDir: config.profileDir,
    publicIndexerConfigured: Boolean(config.indexerUrl),
    webBundleAvailable: hasBundledWeb,
  }));

  app.get("/api/local/profiles", async () => await profileStore.listProfiles());

  app.get("/api/local/profiles/:profile/status", async (request) => {
    const profile = (request.params as { profile: string }).profile;
    return await withProfile(profileStore, config, profile, async ({ summary, client }) => ({
      daemon: await client.inspect(false),
      profile: summary,
    }));
  });

  app.post("/api/local/profiles/:profile/daemon/start", async (request) => {
    const profile = (request.params as { profile: string }).profile;
    const body = asRecord(request.body);
    const mode = parseMode(body.mode);
    return await withProfile(profileStore, config, profile, async ({ client }) => {
      await client.ensureRunning(mode);
      return {
        daemon: await client.inspect(false),
        profile: await requireProfileSummary(profileStore, profile),
      };
    });
  });

  app.post("/api/local/profiles/:profile/daemon/stop", async (request) => {
    const profile = (request.params as { profile: string }).profile;
    return await withRunningProfile(profileStore, config, profile, async ({ summary, client }) => {
      await client.stopDaemon();
      await waitForDaemonReachability(client, false);
      return {
        daemon: await client.inspect(false),
        profile: summary,
      };
    });
  });

  app.get("/api/local/profiles/:profile/watch", async (request, reply) => {
    const profile = (request.params as { profile: string }).profile;
    await requireProfileSummary(profileStore, profile);
    const client = new DaemonRpcClient(profile, config);
    const daemon = await client.inspect(false);
    if (!daemon.reachable) {
      client.close();
      throw new ControllerError(503, "daemon is not running");
    }

    reply.hijack();
    reply.raw.writeHead(200, {
      "Cache-Control": "no-store",
      Connection: "keep-alive",
      "Content-Type": "text/event-stream; charset=utf-8",
      "X-Accel-Buffering": "no",
    });

    let closed = false;
    let keepaliveTimer: NodeJS.Timeout | undefined;
    let stopWatching: (() => void) | undefined;

    const closeStream = () => {
      if (closed) {
        return;
      }
      closed = true;
      if (keepaliveTimer) {
        clearInterval(keepaliveTimer);
      }
      stopWatching?.();
      client.close();
      if (!reply.raw.writableEnded) {
        reply.raw.end();
      }
    };

    request.raw.once("aborted", closeStream);
    request.raw.once("close", closeStream);

    keepaliveTimer = setInterval(() => {
      if (!closed) {
        reply.raw.write(": keepalive\n\n");
      }
    }, 15_000);

    try {
      stopWatching = await bridgeDaemonWatch(client, {
        onLog: (payload) => {
          if (!closed) {
            reply.raw.write(formatSseEvent("log", payload));
          }
        },
        onState: (payload) => {
          if (!closed) {
            reply.raw.write(formatSseEvent("state", payload));
          }
        },
      });
      if (closed) {
        stopWatching();
        client.close();
      }
    } catch (error) {
      closeStream();
      client.close();
    }
  });

  app.post("/api/local/profiles/:profile/bootstrap", async (request) => {
    const profile = (request.params as { profile: string }).profile;
    const body = asRecord(request.body);
    const nickname = optionalString(body.nickname);
    return await withProfile(profileStore, config, profile, async ({ client }) => {
      await client.ensureRunning();
      return await client.bootstrap(nickname);
    });
  });

  app.get("/api/local/profiles/:profile/wallet", async (request) => {
    const profile = (request.params as { profile: string }).profile;
    return await withRunningProfile(profileStore, config, profile, async ({ client }) => await client.walletSummary());
  });

  app.post("/api/local/profiles/:profile/wallet/deposit", async (request) => {
    const profile = (request.params as { profile: string }).profile;
    const body = asRecord(request.body);
    return await withRunningProfile(profileStore, config, profile, async ({ client }) =>
      await client.walletDeposit(requirePositiveInt(body.amountSats, "amountSats")),
    );
  });

  app.post("/api/local/profiles/:profile/wallet/withdraw", async (request) => {
    const profile = (request.params as { profile: string }).profile;
    const body = asRecord(request.body);
    return await withRunningProfile(profileStore, config, profile, async ({ client }) =>
      await client.walletWithdraw(
        requirePositiveInt(body.amountSats, "amountSats"),
        requireString(body.invoice, "invoice"),
      ),
    );
  });

  app.post("/api/local/profiles/:profile/wallet/faucet", async (request) => {
    const profile = (request.params as { profile: string }).profile;
    const body = asRecord(request.body);
    return await withRunningProfile(profileStore, config, profile, async ({ client }) => {
      await client.walletFaucet(requirePositiveInt(body.amountSats, "amountSats"));
      return await client.inspect(false);
    });
  });

  app.post("/api/local/profiles/:profile/wallet/onboard", async (request) => {
    const profile = (request.params as { profile: string }).profile;
    return await withRunningProfile(profileStore, config, profile, async ({ client }) => await client.walletOnboard());
  });

  app.post("/api/local/profiles/:profile/wallet/offboard", async (request) => {
    const profile = (request.params as { profile: string }).profile;
    const body = asRecord(request.body);
    return await withRunningProfile(profileStore, config, profile, async ({ client }) =>
      await client.walletOffboard(
        requireString(body.address, "address"),
        body.amountSats === undefined ? undefined : requirePositiveInt(body.amountSats, "amountSats"),
      ),
    );
  });

  app.get("/api/local/profiles/:profile/network/peers", async (request) => {
    const profile = (request.params as { profile: string }).profile;
    return await withRunningProfile(profileStore, config, profile, async ({ client }) => await client.meshNetworkPeers());
  });

  app.post("/api/local/profiles/:profile/network/bootstrap", async (request) => {
    const profile = (request.params as { profile: string }).profile;
    const body = asRecord(request.body);
    return await withRunningProfile(profileStore, config, profile, async ({ client }) =>
      await client.meshBootstrapPeer(
        requireString(body.peerUrl, "peerUrl"),
        optionalString(body.alias),
        parseRoles(body.roles),
      ),
    );
  });

  app.get("/api/local/profiles/:profile/tables/public", async (request) => {
    const profile = (request.params as { profile: string }).profile;
    return await withRunningProfile(profileStore, config, profile, async ({ client }) => await client.meshPublicTables());
  });

  app.post("/api/local/profiles/:profile/tables", async (request) => {
    const profile = (request.params as { profile: string }).profile;
    const body = asRecord(request.body);
    const table = body.table && typeof body.table === "object" && !Array.isArray(body.table)
      ? (body.table as Record<string, unknown>)
      : Object.keys(body).length > 0
        ? body
        : undefined;
    return await withRunningProfile(profileStore, config, profile, async ({ client }) => await client.meshCreateTable(table));
  });

  app.post("/api/local/profiles/:profile/tables/join", async (request) => {
    const profile = (request.params as { profile: string }).profile;
    const body = asRecord(request.body);
    return await withRunningProfile(profileStore, config, profile, async ({ client }) =>
      await client.meshTableJoin(
        requireString(body.inviteCode, "inviteCode"),
        body.buyInSats === undefined ? undefined : requirePositiveInt(body.buyInSats, "buyInSats"),
      ),
    );
  });

  app.get("/api/local/profiles/:profile/tables/:tableId", async (request) => {
    const { profile, tableId } = request.params as { profile: string; tableId: string };
    return await withRunningProfile(profileStore, config, profile, async ({ client }) => await client.meshGetTable(tableId));
  });

  app.post("/api/local/profiles/:profile/tables/:tableId/announce", async (request) => {
    const { profile, tableId } = request.params as { profile: string; tableId: string };
    return await withRunningProfile(profileStore, config, profile, async ({ client }) => await client.meshTableAnnounce(tableId));
  });

  app.post("/api/local/profiles/:profile/tables/:tableId/action", async (request) => {
    const { profile, tableId } = request.params as { profile: string; tableId: string };
    const body = asRecord(request.body);
    const payload = asRecord(body.payload);
    return await withRunningProfile(profileStore, config, profile, async ({ client }) =>
      await client.meshSendAction(
        {
          ...payload,
          type: requireString(payload.type, "payload.type"),
        } as Parameters<DaemonRpcClient["meshSendAction"]>[0],
        tableId,
      ),
    );
  });

  app.post("/api/local/profiles/:profile/tables/:tableId/rotate-host", async (request) => {
    const { profile, tableId } = request.params as { profile: string; tableId: string };
    return await withRunningProfile(profileStore, config, profile, async ({ client }) => await client.meshRotateHost(tableId));
  });

  app.post("/api/local/profiles/:profile/tables/:tableId/cashout", async (request) => {
    const { profile, tableId } = request.params as { profile: string; tableId: string };
    return await withRunningProfile(profileStore, config, profile, async ({ client }) => await client.meshCashOut(tableId));
  });

  app.post("/api/local/profiles/:profile/tables/:tableId/renew", async (request) => {
    const { profile, tableId } = request.params as { profile: string; tableId: string };
    return await withRunningProfile(profileStore, config, profile, async ({ client }) => await client.meshRenew(tableId));
  });

  app.post("/api/local/profiles/:profile/tables/:tableId/exit", async (request) => {
    const { profile, tableId } = request.params as { profile: string; tableId: string };
    return await withRunningProfile(profileStore, config, profile, async ({ client }) => await client.meshExit(tableId));
  });

  app.get("/api/public/tables", async (_request, reply) => {
    await proxyIndexerRequest(reply, config.indexerUrl, "/api/public/tables");
  });

  app.get("/api/public/tables/:tableId", async (request, reply) => {
    const { tableId } = request.params as { tableId: string };
    await proxyIndexerRequest(reply, config.indexerUrl, `/api/public/tables/${tableId}`);
  });

  if (hasBundledWeb && options.webDistDir) {
    app.get("/", async (_request, reply) => {
      await sendBundledAsset(reply, options.webDistDir!, "/");
    });

    app.get("/*", async (request, reply) => {
      if (request.url.startsWith("/api/") || request.url === "/health") {
        await reply.callNotFound();
        return;
      }
      await sendBundledAsset(reply, options.webDistDir!, request.url);
    });
  }

  return {
    allowedOrigins,
    app,
    config,
  };
}

async function withProfile<T>(
  profileStore: ProfileStore,
  config: CliRuntimeConfig,
  profile: string,
  handler: (args: {
    client: DaemonRpcClient;
    summary: LocalProfileSummary;
  }) => Promise<T>,
) {
  const summary = await requireProfileSummary(profileStore, profile);
  const client = new DaemonRpcClient(profile, config);
  try {
    return await handler({ client, summary });
  } finally {
    client.close();
  }
}

async function withRunningProfile<T>(
  profileStore: ProfileStore,
  config: CliRuntimeConfig,
  profile: string,
  handler: (args: {
    client: DaemonRpcClient;
    summary: LocalProfileSummary;
  }) => Promise<T>,
) {
  return await withProfile(profileStore, config, profile, async ({ client, summary }) => {
    const status = await client.inspect(false);
    if (!status.reachable) {
      throw new ControllerError(503, "daemon is not running");
    }
    return await handler({ client, summary });
  });
}

async function waitForDaemonReachability(client: DaemonRpcClient, reachable: boolean, timeoutMs = 5_000) {
  const start = Date.now();
  while (Date.now() - start < timeoutMs) {
    const status = await client.inspect(false);
    if (status.reachable === reachable) {
      return status;
    }
    await sleep(100);
  }
  throw new ControllerError(
    503,
    reachable ? "daemon did not become reachable in time" : "daemon did not stop in time",
  );
}

async function requireProfileSummary(profileStore: ProfileStore, profile: string) {
  const summary = await profileStore.loadSummary(profile);
  if (!summary) {
    throw new ControllerError(404, `profile ${profile} was not found`);
  }
  return summary;
}

async function proxyIndexerRequest(reply: FastifyReply, indexerUrl: string | undefined, path: string) {
  if (!indexerUrl) {
    throw new ControllerError(503, "indexer is not configured");
  }

  const response = await fetch(`${indexerUrl}${path}`);
  const body = await response.text();
  reply.code(response.status);
  reply.header("Cache-Control", "no-store");
  reply.type(response.headers.get("content-type") ?? "application/json; charset=utf-8");
  await reply.send(body);
}

function resolveAllowedOrigins(controllerPort = DEFAULT_CONTROLLER_PORT) {
  const configuredOrigins = process.env.PARKER_CONTROLLER_ALLOWED_ORIGINS
    ?.split(",")
    .map((origin) => origin.trim())
    .filter(Boolean);
  if (configuredOrigins && configuredOrigins.length > 0) {
    return configuredOrigins;
  }

  return [
    `http://127.0.0.1:${DEFAULT_DEV_WEB_PORT}`,
    `http://localhost:${DEFAULT_DEV_WEB_PORT}`,
    `http://127.0.0.1:${controllerPort}`,
    `http://localhost:${controllerPort}`,
  ];
}

function formatSseEvent(event: "log" | "state", payload: unknown) {
  return `event: ${event}\ndata: ${JSON.stringify(payload)}\n\n`;
}

function parseMode(input: unknown): MeshRuntimeMode | undefined {
  if (input === undefined) {
    return undefined;
  }
  if (input === "player" || input === "host" || input === "witness" || input === "indexer") {
    return input;
  }
  throw new ControllerError(400, "mode must be one of player, host, witness, or indexer");
}

function parseRoles(input: unknown): MeshRuntimeMode[] | undefined {
  if (input === undefined) {
    return undefined;
  }
  if (!Array.isArray(input)) {
    throw new ControllerError(400, "roles must be an array");
  }

  return input.map((role) => parseMode(role) ?? "player");
}

function asRecord(input: unknown): Record<string, unknown> {
  if (!input || typeof input !== "object" || Array.isArray(input)) {
    return {};
  }
  return input as Record<string, unknown>;
}

function optionalString(input: unknown) {
  if (input === undefined || input === null || input === "") {
    return undefined;
  }
  if (typeof input !== "string") {
    throw new ControllerError(400, "expected a string");
  }
  return input;
}

function requireString(input: unknown, field: string) {
  if (typeof input !== "string" || !input.trim()) {
    throw new ControllerError(400, `${field} is required`);
  }
  return input;
}

function requirePositiveInt(input: unknown, field: string) {
  if (typeof input !== "number" || !Number.isInteger(input) || input <= 0) {
    throw new ControllerError(400, `${field} must be a positive integer`);
  }
  return input;
}

function mapErrorStatus(message: string) {
  const normalized = message.toLowerCase();
  if (normalized.includes("not found") || normalized.includes("unknown table") || normalized.includes("is not initialized")) {
    return 404;
  }
  if (normalized.includes("not running") || normalized.includes("timed out waiting for daemon") || normalized.includes("unavailable")) {
    return 503;
  }
  if (
    normalized.includes("invalid") ||
    normalized.includes("illegal") ||
    normalized.includes("required") ||
    normalized.includes("outside the table limits")
  ) {
    return 400;
  }
  if (
    normalized.includes("cannot") ||
    normalized.includes("rejected") ||
    normalized.includes("failed") ||
    normalized.includes("must be initiated")
  ) {
    return 409;
  }
  return 500;
}

async function sendBundledAsset(reply: FastifyReply, webDistDir: string, requestPath: string) {
  const assetPath = await resolveBundledAssetPath(webDistDir, requestPath);
  const body = await readFile(assetPath);
  reply.header("Cache-Control", assetPath.endsWith(INDEX_HTML) ? "no-store" : "public, max-age=31536000, immutable");
  reply.type(contentTypeFor(assetPath));
  await reply.send(body);
}

async function resolveBundledAssetPath(webDistDir: string, requestPath: string) {
  const pathname = decodeURIComponent(requestPath.split("?")[0] ?? "/");
  const candidate = pathname === "/" ? INDEX_HTML : pathname.replace(/^\/+/, "");
  const resolvedPath = resolve(webDistDir, candidate);
  if (resolvedPath.startsWith(resolve(webDistDir)) && (await fileExists(resolvedPath))) {
    const candidateStat = await stat(resolvedPath);
    if (candidateStat.isFile()) {
      return resolvedPath;
    }
  }
  return resolve(webDistDir, INDEX_HTML);
}

function contentTypeFor(pathname: string) {
  switch (extname(pathname)) {
    case ".css":
      return "text/css; charset=utf-8";
    case ".html":
      return "text/html; charset=utf-8";
    case ".js":
      return "text/javascript; charset=utf-8";
    case ".json":
      return "application/json; charset=utf-8";
    case ".svg":
      return "image/svg+xml";
    default:
      return "application/octet-stream";
  }
}

async function fileExists(pathname: string) {
  try {
    await stat(pathname);
    return true;
  } catch {
    return false;
  }
}

function sleep(timeoutMs: number) {
  return new Promise<void>((resolve) => {
    setTimeout(resolve, timeoutMs);
  });
}
