import { mkdir, readFile, rename, writeFile } from "node:fs/promises";
import { dirname } from "node:path";

import type { ArkadeTableFundsState, TableFundsStateStore } from "@parker/settlement";

const TABLE_FUNDS_LOAD_RETRY_MS = 25;
const TABLE_FUNDS_LOAD_RETRIES = 4;
const tableFundsWriteLocks = new Map<string, Promise<void>>();

export class JsonTableFundsStateStore implements TableFundsStateStore<ArkadeTableFundsState> {
  constructor(private readonly path: string) {}

  async load(tableId: string): Promise<ArkadeTableFundsState | null> {
    const current = await this.readAll();
    return current[tableId] ?? null;
  }

  async save(tableId: string, state: ArkadeTableFundsState): Promise<void> {
    const current = await this.readAll();
    current[tableId] = state;
    await mkdir(dirname(this.path), { recursive: true });
    const previousWrite = tableFundsWriteLocks.get(this.path) ?? Promise.resolve();
    const nextWrite = previousWrite.catch(() => undefined).then(async () => {
      const tempPath = `${this.path}.${process.pid}.${crypto.randomUUID()}.tmp`;
      await writeFile(tempPath, `${JSON.stringify(current, null, 2)}\n`, "utf8");
      await rename(tempPath, this.path);
    });
    tableFundsWriteLocks.set(this.path, nextWrite);
    try {
      await nextWrite;
    } finally {
      if (tableFundsWriteLocks.get(this.path) === nextWrite) {
        tableFundsWriteLocks.delete(this.path);
      }
    }
  }

  private async readAll(): Promise<Record<string, ArkadeTableFundsState>> {
    for (let attempt = 0; attempt < TABLE_FUNDS_LOAD_RETRIES; attempt += 1) {
      try {
        const raw = await readFile(this.path, "utf8");
        if (!raw.trim()) {
          if (attempt < TABLE_FUNDS_LOAD_RETRIES - 1) {
            await this.sleep(TABLE_FUNDS_LOAD_RETRY_MS);
            continue;
          }
          return {};
        }
        return JSON.parse(raw) as Record<string, ArkadeTableFundsState>;
      } catch (error) {
        if ((error as NodeJS.ErrnoException).code === "ENOENT") {
          return {};
        }
        if (error instanceof SyntaxError && attempt < TABLE_FUNDS_LOAD_RETRIES - 1) {
          await this.sleep(TABLE_FUNDS_LOAD_RETRY_MS);
          continue;
        }
        throw error;
      }
    }
    return {};
  }

  private async sleep(timeoutMs: number) {
    await new Promise<void>((resolve) => {
      setTimeout(resolve, timeoutMs);
    });
  }
}
