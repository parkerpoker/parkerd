import { execFile } from "node:child_process";
import { promisify } from "node:util";

import type { Network, SwapJobStatus, SwapQuote, TimeoutDelegation } from "@parker/protocol";
import {
  createArkadeDepositQuote,
  createLocalIdentity,
  createTimeoutDelegation,
  getArkadeWalletSummary,
  offboardArkadeFunds,
  onboardArkadeFunds,
  randomHex,
  signMessage,
  submitArkadeWithdrawal,
  type LocalIdentity,
  type WalletSummary,
} from "@parker/settlement";

import type { CliRuntimeConfig } from "./config.js";
import { ProfileStore, type PlayerProfileState } from "./profileStore.js";

const execFileAsync = promisify(execFile);

export interface BootstrapResult {
  identity: LocalIdentity;
  state: PlayerProfileState;
  wallet: WalletSummary;
}

export class CliWalletRuntime {
  constructor(
    private readonly config: CliRuntimeConfig,
    private readonly store: ProfileStore,
  ) {}

  async bootstrap(profileName: string, nickname?: string): Promise<BootstrapResult> {
    const existing = await this.store.load(profileName);
    const identity = existing ? createLocalIdentity(existing.privateKeyHex) : createLocalIdentity();
    const state: PlayerProfileState = existing ?? {
      handSeeds: {},
      nickname: nickname ?? profileName,
      privateKeyHex: identity.privateKeyHex,
      profileName,
    };
    state.nickname = nickname ?? state.nickname;
    if (this.config.useMockSettlement && !state.mockWallet) {
      state.mockWallet = createMockWallet(identity.playerId);
    }
    await this.store.save(state);
    return {
      identity,
      state,
      wallet: await this.getWallet(profileName),
    };
  }

  async createDepositQuote(profileName: string, amountSats: number): Promise<SwapQuote> {
    const { identity, state } = await this.ensureBootstrap(profileName);
    if (this.config.useMockSettlement) {
      const feeSats = Math.max(12, Math.floor(amountSats * 0.0025));
      state.mockWallet = {
        ...(state.mockWallet ?? createMockWallet(identity.playerId)),
        availableSats: (state.mockWallet?.availableSats ?? 50_000) + amountSats - feeSats,
        totalSats: (state.mockWallet?.totalSats ?? 50_000) + amountSats - feeSats,
      };
      await this.store.save(state);
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

    return await createArkadeDepositQuote({
      ...this.arkadeConfig(identity.privateKeyHex),
      amountSats,
    });
  }

  async createTimeoutDelegation(
    profileName: string,
    args: Omit<Parameters<typeof createTimeoutDelegation>[0], "signer">,
  ): Promise<TimeoutDelegation> {
    const { identity } = await this.ensureBootstrap(profileName);
    return createTimeoutDelegation({
      ...args,
      signer: identity,
    });
  }

  async getIdentity(profileName: string) {
    return (await this.ensureBootstrap(profileName)).identity;
  }

  async getWallet(profileName: string): Promise<WalletSummary> {
    const { identity, state } = await this.ensureBootstrap(profileName);
    if (this.config.useMockSettlement) {
      return state.mockWallet ?? createMockWallet(identity.playerId);
    }
    return await getArkadeWalletSummary(this.arkadeConfig(identity.privateKeyHex));
  }

  async nigiriFaucet(profileName: string, amountSats: number) {
    const wallet = await this.getWallet(profileName);
    if (this.config.useMockSettlement) {
      throw new Error("nigiri faucet is not available in mock settlement mode");
    }
    await execFileAsync("nigiri", ["faucet", wallet.boardingAddress, String(amountSats)]);
  }

  async offboard(profileName: string, address: string, amountSats?: number) {
    if (this.config.useMockSettlement) {
      throw new Error("offboard is not available in mock settlement mode");
    }
    const { identity } = await this.ensureBootstrap(profileName);
    const request = {
      ...this.arkadeConfig(identity.privateKeyHex),
      address,
    } as Parameters<typeof offboardArkadeFunds>[0];
    if (amountSats !== undefined) {
      request.amountSats = amountSats;
    }
    return await offboardArkadeFunds(request);
  }

  async onboard(profileName: string) {
    if (this.config.useMockSettlement) {
      throw new Error("onboard is not available in mock settlement mode");
    }
    const { identity } = await this.ensureBootstrap(profileName);
    return await onboardArkadeFunds(this.arkadeConfig(identity.privateKeyHex));
  }

  async saveState(state: PlayerProfileState) {
    await this.store.save(state);
  }

  async signMessage(profileName: string, message: string) {
    const { identity } = await this.ensureBootstrap(profileName);
    return signMessage(identity, message);
  }

  async submitWithdrawal(profileName: string, amountSats: number, lightningInvoice: string): Promise<SwapJobStatus> {
    const { identity, state } = await this.ensureBootstrap(profileName);
    if (this.config.useMockSettlement) {
      const feeSats = Math.max(12, Math.floor(amountSats * 0.003));
      state.mockWallet = {
        ...(state.mockWallet ?? createMockWallet(identity.playerId)),
        availableSats: Math.max(0, (state.mockWallet?.availableSats ?? 50_000) - amountSats - feeSats),
        totalSats: Math.max(0, (state.mockWallet?.totalSats ?? 50_000) - amountSats - feeSats),
      };
      await this.store.save(state);
      const now = new Date().toISOString();
      return {
        swapId: randomHex(12),
        direction: "withdrawal",
        status: "completed",
        createdAt: now,
        updatedAt: now,
        details: `Mock withdrawal sent for ${lightningInvoice.slice(0, 24)}`,
      };
    }
    return await submitArkadeWithdrawal({
      ...this.arkadeConfig(identity.privateKeyHex),
      lightningInvoice,
    });
  }

  private arkadeConfig(privateKeyHex: string) {
    return {
      privateKeyHex,
      network: this.config.network as Network,
      arkServerUrl: this.config.arkServerUrl,
      boltzApiUrl: this.config.boltzApiUrl,
      arkadeNetworkName: this.config.arkadeNetworkName,
    };
  }

  private async ensureBootstrap(profileName: string): Promise<{ identity: LocalIdentity; state: PlayerProfileState }> {
    const state = await this.store.load(profileName);
    if (!state) {
      throw new Error(`profile ${profileName} is not initialized; run bootstrap first`);
    }
    return {
      identity: createLocalIdentity(state.privateKeyHex),
      state,
    };
  }
}

function createMockWallet(playerId: string): WalletSummary {
  return {
    availableSats: 50_000,
    totalSats: 50_000,
    arkAddress: `tark1${playerId.slice(-16)}`,
    boardingAddress: `bcrt1q${playerId.slice(-20).padEnd(20, "0")}`,
  };
}
