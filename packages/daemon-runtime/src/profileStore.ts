import { mkdir, readdir, readFile, rename, writeFile } from "node:fs/promises";
import { join } from "node:path";

import type { MeshTableConfig } from "@parker/protocol";
import type { WalletSummary } from "@parker/settlement";

export interface TableSessionState {
  inviteCode: string;
  seatIndex: 0 | 1;
  tableId: string;
}

export interface KnownPeerState {
  alias?: string;
  lastSeenAt?: string;
  peerId: string;
  peerUrl: string;
  protocolPubkeyHex?: string;
  relayPeerId?: string;
  roles?: Array<"player" | "host" | "witness" | "indexer">;
}

export interface MeshTableReferenceState {
  config?: MeshTableConfig;
  currentEpoch: number;
  hostPeerId: string;
  hostPeerUrl: string;
  role: "host" | "player" | "witness";
  tableId: string;
  visibility: "private" | "public";
}

export interface PlayerProfileState {
  currentTable?: TableSessionState;
  currentMeshTableId?: string;
  handSeeds: Record<string, string>;
  knownPeers?: KnownPeerState[];
  meshTables?: Record<string, MeshTableReferenceState>;
  mockWallet?: WalletSummary;
  nickname: string;
  peerPrivateKeyHex?: string;
  privateKeyHex: string;
  protocolPrivateKeyHex?: string;
  profileName: string;
  walletPrivateKeyHex?: string;
}

export interface LocalProfileSummary {
  currentMeshTableId?: string;
  currentTableId?: string;
  hasPeerIdentity: boolean;
  hasProtocolIdentity: boolean;
  hasWalletIdentity: boolean;
  knownPeerCount: number;
  meshTableCount: number;
  nickname: string;
  profileName: string;
}

const PROFILE_LOAD_RETRY_MS = 25;
const PROFILE_LOAD_RETRIES = 3;
const profileWriteLocks = new Map<string, Promise<void>>();

export class ProfileStore {
  constructor(private readonly profileDir: string) {}

  async listProfileNames(): Promise<string[]> {
    try {
      const entries = await readdir(this.profileDir, { withFileTypes: true });
      return entries
        .filter((entry) => entry.isFile() && entry.name.endsWith(".json"))
        .map((entry) => entry.name.slice(0, -".json".length))
        .sort((left, right) => left.localeCompare(right));
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code === "ENOENT") {
        return [];
      }
      throw error;
    }
  }

  async listProfiles(): Promise<LocalProfileSummary[]> {
    const profiles = await Promise.all(
      (await this.listProfileNames()).map(async (profileName) => {
        const profile = await this.load(profileName);
        return profile ? toLocalProfileSummary(profile) : null;
      }),
    );
    return profiles.filter((profile): profile is LocalProfileSummary => profile !== null);
  }

  async loadSummary(profileName: string): Promise<LocalProfileSummary | null> {
    const profile = await this.load(profileName);
    return profile ? toLocalProfileSummary(profile) : null;
  }

  async load(profileName: string): Promise<PlayerProfileState | null> {
    for (let attempt = 0; attempt < PROFILE_LOAD_RETRIES; attempt += 1) {
      try {
        const raw = await readFile(this.pathFor(profileName), "utf8");
        if (!raw.trim()) {
          if (attempt < PROFILE_LOAD_RETRIES - 1) {
            await this.sleep(PROFILE_LOAD_RETRY_MS);
            continue;
          }
          return null;
        }
        return JSON.parse(raw) as PlayerProfileState;
      } catch (error) {
        if ((error as NodeJS.ErrnoException).code === "ENOENT") {
          return null;
        }
        if (error instanceof SyntaxError && attempt < PROFILE_LOAD_RETRIES - 1) {
          await this.sleep(PROFILE_LOAD_RETRY_MS);
          continue;
        }
        throw error;
      }
    }
    return null;
  }

  async save(state: PlayerProfileState): Promise<void> {
    await mkdir(this.profileDir, { recursive: true });
    const path = this.pathFor(state.profileName);
    const previousWrite = profileWriteLocks.get(path) ?? Promise.resolve();
    const nextWrite = previousWrite.catch(() => undefined).then(async () => {
      const tempPath = `${path}.${process.pid}.${crypto.randomUUID()}.tmp`;
      try {
        await writeFile(tempPath, `${JSON.stringify(state, null, 2)}\n`, "utf8");
        await rename(tempPath, path);
      } catch (error) {
        if ((error as NodeJS.ErrnoException).code === "ENOENT") {
          return;
        }
        throw error;
      }
    });
    profileWriteLocks.set(path, nextWrite);
    try {
      await nextWrite;
    } finally {
      if (profileWriteLocks.get(path) === nextWrite) {
        profileWriteLocks.delete(path);
      }
    }
  }

  private pathFor(profileName: string) {
    return join(this.profileDir, `${profileName}.json`);
  }

  private async sleep(timeoutMs: number) {
    await new Promise<void>((resolve) => {
      setTimeout(resolve, timeoutMs);
    });
  }
}

function toLocalProfileSummary(profile: PlayerProfileState): LocalProfileSummary {
  return {
    ...(profile.currentMeshTableId ? { currentMeshTableId: profile.currentMeshTableId } : {}),
    ...(profile.currentTable ? { currentTableId: profile.currentTable.tableId } : {}),
    hasPeerIdentity: Boolean(profile.peerPrivateKeyHex),
    hasProtocolIdentity: Boolean(profile.protocolPrivateKeyHex),
    hasWalletIdentity: Boolean(profile.walletPrivateKeyHex),
    knownPeerCount: profile.knownPeers?.length ?? 0,
    meshTableCount: Object.keys(profile.meshTables ?? {}).length,
    nickname: profile.nickname,
    profileName: profile.profileName,
  };
}
