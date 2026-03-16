/// <reference lib="dom" />
/// <reference lib="webworker" />

import { Worker as ArkadeWorker } from "@arkade-os/sdk";
import {
  createLocalIdentity,
  createTimeoutDelegation,
  randomHex,
  signMessage,
  type LocalIdentity,
  type WalletSummary,
} from "@parker/settlement";
import type { SwapJobStatus, SwapQuote } from "@parker/protocol";

declare const self: ServiceWorkerGlobalScope;

const MOCK_MODE = import.meta.env.VITE_USE_MOCK_SETTLEMENT !== "false";

interface StoredWalletState {
  identity: LocalIdentity;
  wallet: WalletSummary;
  nickname: string;
}

interface WalletRequest {
  id: string;
  type:
    | "bootstrap"
    | "get-identity"
    | "get-wallet"
    | "create-deposit-quote"
    | "submit-withdrawal"
    | "sign-message"
    | "create-timeout-delegation";
  nickname?: string;
  amountSats?: number;
  invoice?: string;
  message?: string;
  args?: Omit<Parameters<typeof createTimeoutDelegation>[0], "signer">;
}

const DB_NAME = "parker-wallet";
const STORE_NAME = "wallet";

function openDb() {
  return new Promise<IDBDatabase>((resolve, reject) => {
    const request = indexedDB.open(DB_NAME, 1);
    request.onupgradeneeded = () => {
      request.result.createObjectStore(STORE_NAME);
    };
    request.onsuccess = () => resolve(request.result);
    request.onerror = () => reject(request.error);
  });
}

async function loadState() {
  const db = await openDb();
  return await new Promise<StoredWalletState | null>((resolve, reject) => {
    const tx = db.transaction(STORE_NAME, "readonly");
    const store = tx.objectStore(STORE_NAME);
    const request = store.get("active");
    request.onsuccess = () => resolve((request.result as StoredWalletState | undefined) ?? null);
    request.onerror = () => reject(request.error);
  });
}

async function saveState(state: StoredWalletState) {
  const db = await openDb();
  return await new Promise<void>((resolve, reject) => {
    const tx = db.transaction(STORE_NAME, "readwrite");
    tx.objectStore(STORE_NAME).put(state, "active");
    tx.oncomplete = () => resolve();
    tx.onerror = () => reject(tx.error);
  });
}

async function ensureState(nickname = "River Runner"): Promise<StoredWalletState> {
  const existing = await loadState();
  if (existing) {
    return existing;
  }

  const identity = createLocalIdentity();
  const state = {
    identity,
    nickname,
    wallet: {
      availableSats: 50_000,
      totalSats: 50_000,
      arkAddress: `tark1${identity.playerId.slice(-16)}`,
      boardingAddress: `tb1q${identity.playerId.slice(-20)}`,
    },
  };
  await saveState(state);
  return state;
}

async function createDepositQuote(amountSats: number): Promise<SwapQuote> {
  const state = await ensureState();
  const feeSats = Math.max(12, Math.floor(amountSats * 0.0025));
  state.wallet.availableSats += amountSats - feeSats;
  state.wallet.totalSats += amountSats - feeSats;
  await saveState(state);
  return {
    quoteId: crypto.randomUUID(),
    direction: "deposit",
    amountSats,
    feeSats,
    invoice: `lnmock${randomHex(12)}`,
    paymentHash: randomHex(32),
    expiresAt: new Date(Date.now() + 5 * 60_000).toISOString(),
  };
}

async function submitWithdrawal(amountSats: number, invoice: string): Promise<SwapJobStatus> {
  const state = await ensureState();
  const feeSats = Math.max(12, Math.floor(amountSats * 0.003));
  state.wallet.availableSats = Math.max(0, state.wallet.availableSats - amountSats - feeSats);
  state.wallet.totalSats = Math.max(0, state.wallet.totalSats - amountSats - feeSats);
  await saveState(state);
  const now = new Date().toISOString();
  return {
    swapId: randomHex(12),
    direction: "withdrawal",
    status: "completed",
    createdAt: now,
    updatedAt: now,
    details: `Mock Boltz payout created for ${invoice.slice(0, 24)}`,
  };
}

self.addEventListener("install", () => {
  void self.skipWaiting();
});

self.addEventListener("activate", (event) => {
  event.waitUntil(self.clients.claim());
});

if (MOCK_MODE) {
  self.addEventListener("message", (event: ExtendableMessageEvent) => {
    const request = event.data as WalletRequest;
    const port = event.ports[0];
    if (!port) {
      return;
    }

    async function respond() {
      switch (request.type) {
        case "bootstrap": {
          const state = await ensureState(request.nickname);
          return {
            identity: state.identity,
            wallet: state.wallet,
          };
        }
        case "get-identity": {
          return (await ensureState()).identity;
        }
        case "get-wallet": {
          return (await ensureState()).wallet;
        }
        case "create-deposit-quote": {
          return await createDepositQuote(request.amountSats ?? 0);
        }
        case "submit-withdrawal": {
          return await submitWithdrawal(request.amountSats ?? 0, request.invoice ?? "");
        }
        case "sign-message": {
          const state = await ensureState();
          return signMessage(state.identity, request.message ?? "");
        }
        case "create-timeout-delegation": {
          const state = await ensureState();
          return createTimeoutDelegation({
            ...(request.args ?? {
              tableId: "",
              checkpointId: "",
              actingSeatIndex: 0,
              delegatedPlayerId: state.identity.playerId,
              settlementAddress: state.wallet.arkAddress,
              validAfter: new Date().toISOString(),
              expiresAt: new Date().toISOString(),
            }),
            signer: state.identity,
          });
        }
        default:
          throw new Error(`unsupported wallet command ${request.type}`);
      }
    }

    void respond()
      .then((result) => {
        port.postMessage({
          id: request.id,
          ok: true,
          result,
        });
      })
      .catch((error: Error) => {
        port.postMessage({
          id: request.id,
          ok: false,
          error: error.message,
        });
      });
  });
} else {
  void new ArkadeWorker("parker-live-wallet", 1).start();
}
