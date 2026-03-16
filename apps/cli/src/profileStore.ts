import { mkdir, readFile, writeFile } from "node:fs/promises";
import { join } from "node:path";

import type { WalletSummary } from "@parker/settlement";

export interface TableSessionState {
  inviteCode: string;
  seatIndex: 0 | 1;
  tableId: string;
}

export interface PlayerProfileState {
  currentTable?: TableSessionState;
  handSeeds: Record<string, string>;
  mockWallet?: WalletSummary;
  nickname: string;
  privateKeyHex: string;
  profileName: string;
}

export class ProfileStore {
  constructor(private readonly profileDir: string) {}

  async load(profileName: string): Promise<PlayerProfileState | null> {
    try {
      const raw = await readFile(this.pathFor(profileName), "utf8");
      return JSON.parse(raw) as PlayerProfileState;
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code === "ENOENT") {
        return null;
      }
      throw error;
    }
  }

  async save(state: PlayerProfileState): Promise<void> {
    await mkdir(this.profileDir, { recursive: true });
    await writeFile(this.pathFor(state.profileName), `${JSON.stringify(state, null, 2)}\n`, "utf8");
  }

  private pathFor(profileName: string) {
    return join(this.profileDir, `${profileName}.json`);
  }
}
