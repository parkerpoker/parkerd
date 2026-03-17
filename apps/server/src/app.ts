import Fastify from "fastify";
import fastifyWebsocket from "@fastify/websocket";

import {
  clientSocketEventSchema,
  deckCommitmentSchema,
  publicTableUpdateSchema,
  signedTableAdvertisementSchema,
  type Network,
  parseClientSocketEvent,
  serverSocketEventSchema,
  timeoutDelegationSchema,
} from "@parker/protocol";

import { ParkerDatabase } from "./db.js";
import { ParkerTableService } from "./service.js";

export interface CreateAppOptions {
  database?: ParkerDatabase;
  network?: Network;
  websocketUrl?: string;
}

export async function createApp(options: CreateAppOptions = {}) {
  const app = Fastify({ logger: false });
  await app.register(fastifyWebsocket);

  const db = options.database ?? new ParkerDatabase();
  const serviceConfig = {
    websocketUrl: options.websocketUrl ?? "ws://localhost:3020/ws",
  } as const;
  const service = new ParkerTableService(
    db,
    options.network
      ? {
          ...serviceConfig,
          network: options.network,
        }
      : serviceConfig,
  );

  type LiveSocket = { send: (payload: string) => void };
  const sockets = new Map<string, Map<string, LiveSocket>>();

  function broadcast(tableId: string, payload: unknown) {
    const parsed = serverSocketEventSchema.parse(payload);
    const serialized = JSON.stringify(parsed);
    const tableSockets = sockets.get(tableId);
    if (!tableSockets) {
      return;
    }
    for (const socket of tableSockets.values()) {
      socket.send(serialized);
    }
  }

  function sendTo(socket: LiveSocket, payload: unknown) {
    const parsed = serverSocketEventSchema.parse(payload);
    socket.send(JSON.stringify(parsed));
  }

  app.get("/health", async () => ({ ok: true }));

  app.post("/api/indexer/table-ads", async (request) => {
    const ad = signedTableAdvertisementSchema.parse(request.body);
    db.savePublicTableAd(ad);
    return { ok: true };
  });

  app.post("/api/indexer/table-updates", async (request) => {
    const update = publicTableUpdateSchema.parse(request.body);
    db.savePublicUpdate(update);
    return { ok: true };
  });

  app.get("/api/public/tables", async () => db.listPublicTableViews());

  app.get("/api/public/tables/:tableId", async (request, reply) => {
    const view = db.getPublicTableView((request.params as { tableId: string }).tableId);
    if (!view) {
      return reply.code(404).send({ message: "public table not found" });
    }
    return view;
  });

  app.post("/api/tables", async (request) => service.createTable(request.body));

  app.post("/api/tables/join", async (request) => service.joinTable(request.body));

  app.get("/api/tables/by-invite/:inviteCode", async (request, reply) => {
    const snapshot = db.getSnapshotByInviteCode((request.params as { inviteCode: string }).inviteCode);
    if (!snapshot) {
      return reply.code(404).send({ message: "table not found" });
    }
    return snapshot;
  });

  app.get("/api/tables/:tableId", async (request) =>
    service.getSnapshot((request.params as { tableId: string }).tableId),
  );

  app.get("/api/tables/:tableId/transcript", async (request) =>
    service.listTranscript((request.params as { tableId: string }).tableId),
  );

  app.post("/api/tables/:tableId/commitments", async (request) => {
    const payload = deckCommitmentSchema.parse(request.body);
    const tableId = (request.params as { tableId: string }).tableId;
    const snapshot = service.saveCommitment(tableId, payload);
    broadcast(tableId, {
      type: "table-snapshot",
      snapshot,
    });
    if (snapshot.checkpoint) {
      broadcast(tableId, {
        type: "checkpoint",
        checkpoint: snapshot.checkpoint,
      });
    }
    return snapshot;
  });

  app.post("/api/tables/:tableId/delegations", async (request) => {
    const delegation = timeoutDelegationSchema.parse(request.body);
    const tableId = (request.params as { tableId: string }).tableId;
    const snapshot = service.saveDelegation(tableId, delegation);
    broadcast(tableId, {
      type: "table-snapshot",
      snapshot,
    });
    return snapshot;
  });

  app.get("/ws", { websocket: true }, (socket) => {
    let tableId: string | null = null;
    let playerId: string | null = null;

    socket.on("message", (raw: Buffer) => {
      const parsed = parseClientSocketEvent(JSON.parse(raw.toString()));
      clientSocketEventSchema.parse(parsed);

      switch (parsed.type) {
        case "identify": {
          tableId = parsed.tableId;
          playerId = parsed.playerId;
          const tableSockets = sockets.get(parsed.tableId) ?? new Map<string, LiveSocket>();
          tableSockets.set(parsed.playerId, socket as unknown as LiveSocket);
          sockets.set(parsed.tableId, tableSockets);
          sendTo(socket as unknown as LiveSocket, {
            type: "table-snapshot",
            snapshot: service.getSnapshot(parsed.tableId),
          });
          for (const connectedPlayerId of tableSockets.keys()) {
            if (connectedPlayerId === parsed.playerId) {
              continue;
            }
            sendTo(socket as unknown as LiveSocket, {
              type: "presence",
              tableId: parsed.tableId,
              playerId: connectedPlayerId,
              status: "online",
            });
          }
          broadcast(parsed.tableId, {
            type: "presence",
            tableId: parsed.tableId,
            playerId: parsed.playerId,
            status: "online",
          });
          break;
        }
        case "peer-message": {
          const targetSocket = sockets.get(parsed.tableId)?.get(parsed.targetPlayerId);
          targetSocket?.send(
            JSON.stringify({
              type: "peer-message",
              tableId: parsed.tableId,
              fromPlayerId: parsed.fromPlayerId,
              message: parsed.message,
            }),
          );
          break;
        }
        case "signed-action": {
          const snapshot = service.processSignedAction(parsed.action);
          broadcast(parsed.action.tableId, {
            type: "table-snapshot",
            snapshot,
          });
          if (snapshot.checkpoint) {
            broadcast(parsed.action.tableId, {
              type: "checkpoint",
              checkpoint: snapshot.checkpoint,
            });
          }
          break;
        }
        case "checkpoint": {
          broadcast(parsed.checkpoint.tableId, parsed);
          break;
        }
        case "heartbeat": {
          broadcast(parsed.tableId, {
            type: "presence",
            tableId: parsed.tableId,
            playerId: parsed.playerId,
            status: "online",
          });
          break;
        }
      }
    });

    socket.on("close", () => {
      if (!tableId || !playerId) {
        return;
      }
      const tableSockets = sockets.get(tableId);
      tableSockets?.delete(playerId);
      broadcast(tableId, {
        type: "presence",
        tableId,
        playerId,
        status: "offline",
      });
    });
  });

  const interval = setInterval(() => {
    service.runTimeoutSweep();
  }, 1000);

  app.addHook("onClose", async () => {
    clearInterval(interval);
    db.close();
  });

  return { app, service, db };
}
