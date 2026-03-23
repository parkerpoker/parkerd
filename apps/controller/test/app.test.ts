import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { afterEach, describe, expect, it } from "vitest";

import {
  CliWalletRuntime,
  DaemonRpcClient,
  ProfileStore,
  resolveCliRuntimeConfig,
  type CliRuntimeConfig,
} from "@parker/daemon-runtime";

import { LOCAL_CONTROLLER_HEADER, createControllerApp } from "../src/app.js";

interface TestContext {
  app: Awaited<ReturnType<typeof createControllerApp>>["app"];
  config: CliRuntimeConfig;
  rootDir: string;
}

interface LocalInjectResponse {
  body: string;
  json: <T = unknown>() => T;
  statusCode: number;
}

const TEST_ORIGIN = "http://127.0.0.1:3010";
const contexts = new Set<TestContext>();

describe("controller app", () => {
  afterEach(async () => {
    for (const context of contexts) {
      await stopDaemon(context.config, "alice");
      await context.app.close();
      await rm(context.rootDir, { force: true, recursive: true });
      contexts.delete(context);
    }
  });

  it("rejects browser-facing local routes without the controller header", async () => {
    const context = await createTestContext();

    const response = await context.app.inject({
      method: "GET",
      url: "/api/local/profiles",
    });

    expect(response.statusCode).toBe(400);
    expect(response.json().error).toContain(LOCAL_CONTROLLER_HEADER);
  });

  it("enforces allowed origins and answers CORS preflight for local routes", async () => {
    const context = await createTestContext();

    const rejected = await context.app.inject({
      headers: {
        [LOCAL_CONTROLLER_HEADER]: "1",
        origin: "http://evil.example",
      },
      method: "GET",
      url: "/api/local/profiles",
    });
    expect(rejected.statusCode).toBe(403);

    const preflight = await context.app.inject({
      headers: {
        "access-control-request-headers": LOCAL_CONTROLLER_HEADER,
        origin: TEST_ORIGIN,
      },
      method: "OPTIONS",
      url: "/api/local/profiles",
    });
    expect(preflight.statusCode).toBe(204);
    expect(preflight.headers["access-control-allow-origin"]).toBe(TEST_ORIGIN);
  });

  it("lists profiles and exposes thin controller routes over the daemon", async () => {
    const context = await createTestContext();
    await bootstrapProfile(context.config, "alice", "Alice");

    const profilesResponse = await localInject(context, {
      method: "GET",
      url: "/api/local/profiles",
    });
    expect(profilesResponse.statusCode).toBe(200);
    expect(profilesResponse.json()).toEqual([
      expect.objectContaining({
        nickname: "Alice",
        profileName: "alice",
      }),
    ]);

    const startResponse = await localInject(context, {
      body: {},
      method: "POST",
      url: "/api/local/profiles/alice/daemon/start",
    });
    expect(startResponse.statusCode).toBe(200);
    expect(startResponse.json<{ daemon: { reachable: boolean } }>().daemon.reachable).toBe(true);

    const bootstrapResponse = await localInject(context, {
      body: {
        nickname: "Alice Browser",
      },
      method: "POST",
      url: "/api/local/profiles/alice/bootstrap",
    });
    expect(bootstrapResponse.statusCode).toBe(200);
    expect(
      bootstrapResponse.json<{ mesh: { walletPlayerId: string } }>().mesh.walletPlayerId,
    ).toMatch(/^player-/);

    const walletResponse = await localInject(context, {
      method: "GET",
      url: "/api/local/profiles/alice/wallet",
    });
    expect(walletResponse.statusCode).toBe(200);
    expect(walletResponse.json<{ availableSats: number }>().availableSats).toBeGreaterThan(0);

    const networkBootstrapResponse = await localInject(context, {
      body: {
        alias: "ghost-host",
        peerUrl: "ws://127.0.0.1:39999/mesh",
        roles: ["host"],
      },
      method: "POST",
      url: "/api/local/profiles/alice/network/bootstrap",
    });
    expect(networkBootstrapResponse.statusCode).toBe(200);
    expect(networkBootstrapResponse.json<{ peerUrl: string }>().peerUrl).toBe("ws://127.0.0.1:39999/mesh");

    const peersResponse = await localInject(context, {
      method: "GET",
      url: "/api/local/profiles/alice/network/peers",
    });
    expect(peersResponse.statusCode).toBe(200);
    expect(peersResponse.json()).toEqual(
      expect.arrayContaining([
        expect.objectContaining({
          peerUrl: "ws://127.0.0.1:39999/mesh",
        }),
      ]),
    );

    const createTableResponse = await localInject(context, {
      body: {
        table: {
          bigBlindSats: 100,
          buyInMaxSats: 4000,
          buyInMinSats: 4000,
          name: "Controller Table",
          public: true,
          smallBlindSats: 50,
          spectatorsAllowed: true,
        },
      },
      method: "POST",
      url: "/api/local/profiles/alice/tables",
    });
    expect(createTableResponse.statusCode).toBe(200);
    const tableId = createTableResponse.json<{ table: { tableId: string } }>().table.tableId;

    const getTableResponse = await localInject(context, {
      method: "GET",
      url: `/api/local/profiles/alice/tables/${tableId}`,
    });
    expect(getTableResponse.statusCode).toBe(200);
    expect(getTableResponse.json()).toEqual(
      expect.objectContaining({
        config: expect.objectContaining({
          name: "Controller Table",
          tableId,
        }),
        local: expect.objectContaining({
          myPlayerId: expect.stringMatching(/^player-/),
        }),
      }),
    );

    const publicTablesResponse = await localInject(context, {
      method: "GET",
      url: "/api/local/profiles/alice/tables/public",
    });
    expect(publicTablesResponse.statusCode).toBe(200);
    expect(publicTablesResponse.json()).toEqual(
      expect.arrayContaining([
        expect.objectContaining({
          advertisement: expect.objectContaining({
            tableId,
            tableName: "Controller Table",
          }),
        }),
      ]),
    );
  }, 40_000);

  it("streams initial state and later log events over SSE", async () => {
    const context = await createTestContext({
      indexerUrl: "http://127.0.0.1:9",
    });
    await bootstrapProfile(context.config, "alice", "Alice");

    const startResponse = await localInject(context, {
      body: {},
      method: "POST",
      url: "/api/local/profiles/alice/daemon/start",
    });
    expect(startResponse.statusCode).toBe(200);

    const baseUrl = await context.app.listen({ host: "127.0.0.1", port: 0 });
    const streamResponse = await fetch(`${baseUrl}/api/local/profiles/alice/watch`, {
      headers: {
        [LOCAL_CONTROLLER_HEADER]: "1",
        origin: TEST_ORIGIN,
      },
    });
    expect(streamResponse.status).toBe(200);
    expect(streamResponse.body).toBeTruthy();

    const reader = streamResponse.body!.getReader();
    const decoder = new TextDecoder();
    let buffer = "";
    const receivedEvents: string[] = [];

    const readEvent = async () => {
      for (;;) {
        const separatorIndex = buffer.indexOf("\n\n");
        if (separatorIndex !== -1) {
          const rawEvent = buffer.slice(0, separatorIndex);
          buffer = buffer.slice(separatorIndex + 2);
          const eventLine = rawEvent
            .split("\n")
            .find((line) => line.startsWith("event:"));
          if (eventLine) {
            return eventLine.slice("event:".length).trim();
          }
        }
        const { done, value } = await reader.read();
        if (done) {
          throw new Error("controller SSE stream ended before expected events arrived");
        }
        buffer += decoder.decode(value, { stream: true });
      }
    };

    receivedEvents.push(await readEvent());

    const publicTablesResponse = await fetch(`${baseUrl}/api/local/profiles/alice/tables/public`, {
      headers: {
        [LOCAL_CONTROLLER_HEADER]: "1",
        origin: TEST_ORIGIN,
      },
    });
    expect(publicTablesResponse.status).toBe(200);

    receivedEvents.push(await readEvent());
    await reader.cancel();

    expect(receivedEvents).toEqual(["state", "log"]);
  }, 20_000);
});

async function createTestContext(options: { indexerUrl?: string } = {}) {
  const rootDir = await mkdtemp(join(tmpdir(), "parker-controller-test-"));
  const config = resolveCliRuntimeConfig({
    "daemon-dir": join(rootDir, "daemons"),
    ...(options.indexerUrl ? { "indexer-url": options.indexerUrl } : {}),
    mock: true,
    network: "regtest",
    "peer-port": "0",
    "profile-dir": join(rootDir, "profiles"),
    "run-dir": join(rootDir, "runs"),
  });
  const { app } = await createControllerApp({
    allowedOrigins: [TEST_ORIGIN],
    config,
  });

  const context = {
    app,
    config,
    rootDir,
  };
  contexts.add(context);
  return context;
}

async function bootstrapProfile(config: CliRuntimeConfig, profileName: string, nickname: string) {
  const store = new ProfileStore(config.profileDir);
  const wallet = new CliWalletRuntime(config, store);
  await wallet.bootstrap(profileName, nickname);
}

async function localInject(
  context: TestContext,
  options: {
    body?: unknown;
    method: "GET" | "POST";
    url: string;
  },
) : Promise<LocalInjectResponse> {
  const request: {
    headers: Record<string, string>;
    method: "GET" | "POST";
    payload?: Buffer | NodeJS.ReadableStream | Record<string, unknown> | string;
    url: string;
  } = {
    headers: {
      [LOCAL_CONTROLLER_HEADER]: "1",
      origin: TEST_ORIGIN,
    },
    method: options.method,
    url: options.url,
  };
  if (options.body !== undefined) {
    request.payload = options.body as Buffer | NodeJS.ReadableStream | Record<string, unknown> | string;
  }
  return (await context.app.inject(request)) as LocalInjectResponse;
}

async function stopDaemon(config: CliRuntimeConfig, profileName: string) {
  const client = new DaemonRpcClient(profileName, config);
  try {
    const status = await client.inspect(false);
    if (status.reachable) {
      await client.stopDaemon();
    }
  } catch {
    // Ignore best-effort cleanup failures in tests.
  } finally {
    client.close();
  }
}
