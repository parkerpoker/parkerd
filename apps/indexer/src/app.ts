import Fastify from "fastify";

import {
  publicTableUpdateSchema,
  signedTableAdvertisementSchema,
} from "@parker/protocol";

import { ParkerDatabase } from "./db.js";

export interface CreateAppOptions {
  database?: ParkerDatabase;
}

export async function createApp(options: CreateAppOptions = {}) {
  const app = Fastify({ logger: false });
  const db = options.database ?? new ParkerDatabase();

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

  app.addHook("onClose", async () => {
    db.close();
  });

  return { app, db };
}
