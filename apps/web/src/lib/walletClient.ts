import type { SwapJobStatus, SwapQuote, TimeoutDelegation } from "@parker/protocol";
import {
  MUTINYNET_ARK_SERVER_URL,
  MUTINYNET_BOLTZ_URL,
  createLocalIdentity,
  createTimeoutDelegation,
  randomHex,
  signMessage,
  type LocalIdentity,
  type WalletSummary,
} from "@parker/settlement";

const USE_MOCK = import.meta.env.VITE_USE_MOCK_SETTLEMENT !== "false";
const LIVE_DB_NAME = "parker-live-wallet";
const LIVE_DB_VERSION = 1;
const LIVE_KEY_STORAGE_KEY = "parker-live-private-key";

interface WalletResponse<T> {
  id: string;
  ok: boolean;
  result?: T;
  error?: string;
}

type WalletCommand =
  | { id: string; type: "bootstrap"; nickname: string }
  | { id: string; type: "get-identity" }
  | { id: string; type: "get-wallet" }
  | { id: string; type: "create-deposit-quote"; amountSats: number }
  | { id: string; type: "submit-withdrawal"; amountSats: number; invoice: string }
  | { id: string; type: "sign-message"; message: string }
  | {
      id: string;
      type: "create-timeout-delegation";
      args: Omit<Parameters<typeof createTimeoutDelegation>[0], "signer">;
    };

interface MockRuntime {
  mode: "mock";
  registration: ServiceWorkerRegistration;
}

interface LiveRuntime {
  mode: "live";
  registration: ServiceWorkerRegistration;
  identity: LocalIdentity;
  wallet: {
    getAddress: () => Promise<string>;
    getBoardingAddress: () => Promise<string>;
    getBalance: () => Promise<{ available: number; total: number }>;
  };
  lightning: {
    createLightningInvoice: (args: { amount: number; description?: string }) => Promise<{
      invoice: string;
      paymentHash: string;
    }>;
    sendLightningPayment: (args: { invoice: string }) => Promise<Record<string, unknown>>;
  };
}

type WalletRuntime = MockRuntime | LiveRuntime;

function serviceWorkerUrl() {
  return import.meta.env.DEV ? "/src/wallet-service-worker.ts" : "/wallet-service-worker.js";
}

async function waitForServiceWorkerActivation(registration: ServiceWorkerRegistration) {
  const pendingWorker =
    registration.active ?? registration.waiting ?? registration.installing;
  if (!pendingWorker) {
    throw new Error("wallet worker is not available");
  }

  if (registration.active) {
    return registration;
  }

  const activeWorker = pendingWorker;

  await new Promise<void>((resolve, reject) => {
    const timeout = window.setTimeout(() => {
      reject(new Error("wallet worker did not activate in time"));
    }, 15_000);

    function cleanup() {
      window.clearTimeout(timeout);
      activeWorker.removeEventListener("statechange", onStateChange);
    }

    function onStateChange() {
      if (activeWorker.state === "activated") {
        cleanup();
        resolve();
      }
    }

    activeWorker.addEventListener("statechange", onStateChange);
    onStateChange();
  });

  return registration;
}

async function registerServiceWorker() {
  if (!("serviceWorker" in navigator)) {
    throw new Error("service workers are not available in this browser");
  }

  const registration = await navigator.serviceWorker.register(serviceWorkerUrl(), {
    type: "module",
    scope: "/",
  });
  return await waitForServiceWorkerActivation(registration);
}

function getActiveServiceWorker(registration: ServiceWorkerRegistration) {
  const worker =
    registration.active ?? registration.waiting ?? registration.installing;
  if (!worker) {
    throw new Error("wallet worker is not active yet");
  }
  return worker;
}

async function ensureLivePrivateKeyHex() {
  const existing = localStorage.getItem(LIVE_KEY_STORAGE_KEY);
  if (existing) {
    return existing;
  }

  const sdk = await import("@arkade-os/sdk");
  const identity = (sdk as any).SingleKey.fromRandomBytes();
  const privateKeyHex = identity.toHex();
  localStorage.setItem(LIVE_KEY_STORAGE_KEY, privateKeyHex);
  return privateKeyHex;
}

async function buildLiveRuntime(registration: ServiceWorkerRegistration): Promise<LiveRuntime> {
  const privateKeyHex = await ensureLivePrivateKeyHex();
  const localIdentity = createLocalIdentity(privateKeyHex);
  const sdk = await import("@arkade-os/sdk");
  const swaps = await import("@arkade-os/boltz-swap");
  const serviceWorker = getActiveServiceWorker(registration);
  const identity = (sdk as any).SingleKey.fromHex(privateKeyHex);
  const wallet = await (sdk as any).ServiceWorkerWallet.create({
    serviceWorker,
    identity,
    arkServerUrl: import.meta.env.VITE_ARK_SERVER_URL ?? MUTINYNET_ARK_SERVER_URL,
    dbName: LIVE_DB_NAME,
    dbVersion: LIVE_DB_VERSION,
  });
  const lightning = new (swaps as any).ArkadeLightning({
    wallet,
    arkProvider: new (sdk as any).RestArkProvider(
      import.meta.env.VITE_ARK_SERVER_URL ?? MUTINYNET_ARK_SERVER_URL,
    ),
    indexerProvider: new (sdk as any).RestIndexerProvider(
      import.meta.env.VITE_ARK_SERVER_URL ?? MUTINYNET_ARK_SERVER_URL,
    ),
    swapProvider: new (swaps as any).BoltzSwapProvider({
      apiUrl: import.meta.env.VITE_BOLTZ_URL ?? MUTINYNET_BOLTZ_URL,
      network: "mutinynet",
      referralId: "parker",
    }),
  });

  return {
    mode: "live",
    registration,
    identity: localIdentity,
    wallet,
    lightning,
  };
}

async function getLiveWalletSummary(runtime: LiveRuntime): Promise<WalletSummary> {
  const [arkAddress, boardingAddress, balance] = await Promise.all([
    runtime.wallet.getAddress(),
    runtime.wallet.getBoardingAddress(),
    runtime.wallet.getBalance(),
  ]);

  return {
    availableSats: balance.available,
    totalSats: balance.total,
    arkAddress,
    boardingAddress,
  };
}

export class WalletWorkerClient {
  private constructor(private readonly runtime: WalletRuntime) {}

  static async create() {
    const registration = await registerServiceWorker();
    if (USE_MOCK) {
      return new WalletWorkerClient({
        mode: "mock",
        registration,
      });
    }
    return new WalletWorkerClient(await buildLiveRuntime(registration));
  }

  private async requestMock<T>(command: WalletCommand): Promise<T> {
    const worker = getActiveServiceWorker(this.runtime.registration);
    return await new Promise<T>((resolve, reject) => {
      const channel = new MessageChannel();
      channel.port1.onmessage = (event) => {
        const response = event.data as WalletResponse<T>;
        if (response.ok) {
          resolve(response.result as T);
        } else {
          reject(new Error(response.error ?? "wallet worker error"));
        }
      };
      worker.postMessage(command, [channel.port2]);
    });
  }

  async bootstrap(nickname: string) {
    if (this.runtime.mode === "mock") {
      return await this.requestMock<{ identity: LocalIdentity; wallet: WalletSummary }>({
        id: crypto.randomUUID(),
        type: "bootstrap",
        nickname,
      });
    }

    return {
      identity: this.runtime.identity,
      wallet: await getLiveWalletSummary(this.runtime),
    };
  }

  async getIdentity() {
    if (this.runtime.mode === "mock") {
      return await this.requestMock<LocalIdentity>({
        id: crypto.randomUUID(),
        type: "get-identity",
      });
    }
    return this.runtime.identity;
  }

  async getWallet() {
    if (this.runtime.mode === "mock") {
      return await this.requestMock<WalletSummary>({
        id: crypto.randomUUID(),
        type: "get-wallet",
      });
    }
    return await getLiveWalletSummary(this.runtime);
  }

  async createDepositQuote(amountSats: number) {
    if (this.runtime.mode === "mock") {
      return await this.requestMock<SwapQuote>({
        id: crypto.randomUUID(),
        type: "create-deposit-quote",
        amountSats,
      });
    }

    const response = await this.runtime.lightning.createLightningInvoice({
      amount: amountSats,
      description: "Parker deposit",
    });
    return {
      quoteId: crypto.randomUUID(),
      direction: "deposit",
      amountSats,
      feeSats: 0,
      invoice: response.invoice,
      paymentHash: response.paymentHash,
      expiresAt: new Date(Date.now() + 15 * 60_000).toISOString(),
    } satisfies SwapQuote;
  }

  async submitWithdrawal(amountSats: number, invoice: string) {
    if (this.runtime.mode === "mock") {
      return await this.requestMock<SwapJobStatus>({
        id: crypto.randomUUID(),
        type: "submit-withdrawal",
        amountSats,
        invoice,
      });
    }

    const response = await this.runtime.lightning.sendLightningPayment({
      invoice,
    });
    const now = new Date().toISOString();
    return {
      swapId: String(response.paymentHash ?? response.id ?? randomHex(12)),
      direction: "withdrawal",
      status: "completed",
      createdAt: now,
      updatedAt: now,
      details: typeof response.preimage === "string" ? `Preimage ${response.preimage}` : "Lightning withdrawal sent.",
    } satisfies SwapJobStatus;
  }

  async signMessage(message: string) {
    if (this.runtime.mode === "mock") {
      return await this.requestMock<string>({
        id: crypto.randomUUID(),
        type: "sign-message",
        message,
      });
    }
    return signMessage(this.runtime.identity, message);
  }

  async createTimeoutDelegation(args: Omit<Parameters<typeof createTimeoutDelegation>[0], "signer">) {
    if (this.runtime.mode === "mock") {
      return await this.requestMock<TimeoutDelegation>({
        id: crypto.randomUUID(),
        type: "create-timeout-delegation",
        args,
      });
    }
    return createTimeoutDelegation({
      ...args,
      signer: this.runtime.identity,
    });
  }
}
