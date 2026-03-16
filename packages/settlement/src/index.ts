import { secp256k1 } from "@noble/curves/secp256k1";
import { sha256 } from "@noble/hashes/sha2";
import { bytesToHex, hexToBytes, utf8ToBytes } from "@noble/hashes/utils";

import type {
  EscrowState,
  Network,
  SettlementInstruction,
  SwapJobStatus,
  SwapQuote,
  TableCheckpoint,
  TimeoutDelegation,
} from "@parker/protocol";

export const MUTINYNET_ARK_SERVER_URL = "https://mutinynet.arkade.sh";
export const MUTINYNET_BOLTZ_URL = "https://api.boltz.mutinynet.arkade.sh";
export const REGTEST_ARK_SERVER_URL = "http://127.0.0.1:7070";
export const REGTEST_BOLTZ_URL = "http://127.0.0.1:9069";

export interface ParkerNetworkConfig {
  network: Network;
  arkServerUrl: string;
  boltzApiUrl: string;
  arkadeNetworkName: "mutinynet" | "regtest";
}

export const PARKER_NETWORK_CONFIGS: Record<Network, ParkerNetworkConfig> = {
  mutinynet: {
    network: "mutinynet",
    arkServerUrl: MUTINYNET_ARK_SERVER_URL,
    boltzApiUrl: MUTINYNET_BOLTZ_URL,
    arkadeNetworkName: "mutinynet",
  },
  regtest: {
    network: "regtest",
    arkServerUrl: REGTEST_ARK_SERVER_URL,
    boltzApiUrl: REGTEST_BOLTZ_URL,
    arkadeNetworkName: "regtest",
  },
};

export interface LocalIdentity {
  playerId: string;
  privateKeyHex: string;
  publicKeyHex: string;
}

export interface WalletSummary {
  availableSats: number;
  totalSats: number;
  arkAddress: string;
  boardingAddress: string;
}

export interface TableEscrowRequest {
  tableId: string;
  network: Network;
  participantPubkeys: [string, string];
  watchtowerPubkey: string;
  totalLockedSats: number;
  refundDelayBlocks: number;
  currentCheckpointId?: string;
}

export interface SettlementProvider {
  createLocalIdentity(seedHex?: string): LocalIdentity;
  getWalletSummary(identity: LocalIdentity): Promise<WalletSummary>;
  createDepositQuote(identity: LocalIdentity, amountSats: number): Promise<SwapQuote>;
  submitWithdrawal(identity: LocalIdentity, lightningInvoice: string): Promise<SwapJobStatus>;
  buildTableEscrow(request: TableEscrowRequest): Promise<EscrowState>;
  createSettlementInstruction(args: {
    tableId: string;
    checkpointId?: string;
    kind: SettlementInstruction["kind"];
    outputs: SettlementInstruction["outputs"];
  }): SettlementInstruction;
}

export interface ArkadeWalletConnectionConfig {
  privateKeyHex: string;
  network?: Network;
  arkServerUrl?: string;
  boltzApiUrl?: string;
  arkadeNetworkName?: "mutinynet" | "regtest";
}

export function resolveParkerNetworkConfig(
  config: Pick<ArkadeWalletConnectionConfig, "network" | "arkServerUrl" | "boltzApiUrl" | "arkadeNetworkName">,
): ParkerNetworkConfig {
  const defaults = PARKER_NETWORK_CONFIGS[config.network ?? "mutinynet"];
  return {
    network: config.network ?? defaults.network,
    arkServerUrl: config.arkServerUrl ?? defaults.arkServerUrl,
    boltzApiUrl: config.boltzApiUrl ?? defaults.boltzApiUrl,
    arkadeNetworkName: config.arkadeNetworkName ?? defaults.arkadeNetworkName,
  };
}

function mockArkAddress(playerId: string) {
  return `tark1${playerId.slice(-16)}`;
}

function mockBoardingAddress(playerId: string) {
  return `bcrt1q${playerId.slice(-20).padEnd(20, "0")}`;
}

function stableStringify(input: unknown): string {
  if (Array.isArray(input)) {
    return `[${input.map((value) => stableStringify(value)).join(",")}]`;
  }

  if (input && typeof input === "object") {
    const entries = Object.entries(input as Record<string, unknown>).sort(([left], [right]) =>
      left.localeCompare(right),
    );
    return `{${entries
      .map(([key, value]) => `${JSON.stringify(key)}:${stableStringify(value)}`)
      .join(",")}}`;
  }

  return JSON.stringify(input);
}

export function hashCheckpoint(checkpoint: Omit<TableCheckpoint, "signatures">): string {
  return bytesToHex(sha256(utf8ToBytes(stableStringify(checkpoint))));
}

export function hashMessage(message: string): Uint8Array {
  return sha256(utf8ToBytes(message));
}

export function createLocalIdentity(seedHex = randomHex(32)): LocalIdentity {
  const publicKey = secp256k1.getPublicKey(hexToBytes(seedHex), true);
  const publicKeyHex = bytesToHex(publicKey);
  const playerId = `player-${bytesToHex(sha256(publicKey)).slice(0, 20)}`;
  return {
    playerId,
    privateKeyHex: seedHex,
    publicKeyHex,
  };
}

export function signMessage(identity: LocalIdentity, message: string): string {
  return secp256k1.sign(hashMessage(message), hexToBytes(identity.privateKeyHex)).toCompactHex();
}

export function verifyMessage(publicKeyHex: string, message: string, signatureHex: string): boolean {
  return secp256k1.verify(hexToBytes(signatureHex), hashMessage(message), hexToBytes(publicKeyHex));
}

export function signCheckpoint(identity: LocalIdentity, checkpoint: Omit<TableCheckpoint, "signatures">): string {
  return signMessage(identity, hashCheckpoint(checkpoint));
}

export function verifyCheckpointSignature(args: {
  publicKeyHex: string;
  checkpoint: Omit<TableCheckpoint, "signatures">;
  signatureHex: string;
}): boolean {
  return verifyMessage(args.publicKeyHex, hashCheckpoint(args.checkpoint), args.signatureHex);
}

export function createTimeoutDelegation(args: {
  tableId: string;
  checkpointId: string;
  actingSeatIndex: number;
  delegatedPlayerId: string;
  settlementAddress: string;
  validAfter: string;
  expiresAt: string;
  signer: LocalIdentity;
}): TimeoutDelegation {
  const delegationBase = {
    delegationId: crypto.randomUUID(),
    tableId: args.tableId,
    checkpointId: args.checkpointId,
    actingSeatIndex: args.actingSeatIndex,
    delegatedPlayerId: args.delegatedPlayerId,
    delegatedAction: "timeout-fold" as const,
    validAfter: args.validAfter,
    expiresAt: args.expiresAt,
    settlementAddress: args.settlementAddress,
  };

  return {
    ...delegationBase,
    signatureHex: signMessage(args.signer, stableStringify(delegationBase)),
  };
}

export function buildEscrowDescriptor(request: TableEscrowRequest): EscrowState {
  const scriptFingerprint = bytesToHex(
    sha256(
      utf8ToBytes(
        [
          request.tableId,
          ...request.participantPubkeys,
          request.watchtowerPubkey,
          request.totalLockedSats,
          request.refundDelayBlocks,
          request.currentCheckpointId ?? "",
        ].join("|"),
      ),
    ),
  );

  return {
    escrowId: crypto.randomUUID(),
    tableId: request.tableId,
    network: request.network,
    contractAddress: `descriptor:${scriptFingerprint.slice(0, 24)}`,
    totalLockedSats: request.totalLockedSats,
    participantPubkeys: [...request.participantPubkeys, request.watchtowerPubkey],
    watchtowerPubkey: request.watchtowerPubkey,
    refundDelayBlocks: request.refundDelayBlocks,
    cooperativePath: `multisig(${request.participantPubkeys.join(",")},${request.watchtowerPubkey})`,
    renewalPath: `delegated-renewal(${request.currentCheckpointId ?? "genesis"})`,
    refundPath: `csv-multisig(${request.refundDelayBlocks})`,
    currentCheckpointId: request.currentCheckpointId,
    status: "funded",
  };
}

export function randomHex(byteLength: number): string {
  const bytes = crypto.getRandomValues(new Uint8Array(byteLength));
  return bytesToHex(bytes);
}

export async function connectArkadeWallet(config: ArkadeWalletConnectionConfig) {
  const networkConfig = resolveParkerNetworkConfig(config);
  const sdk = await import("@arkade-os/sdk");
  const identity = (sdk as any).SingleKey.fromHex(config.privateKeyHex);
  return await (sdk as any).Wallet.create({
    identity,
    arkServerUrl: networkConfig.arkServerUrl,
  });
}

export async function getArkadeWalletSummary(config: ArkadeWalletConnectionConfig): Promise<WalletSummary> {
  const wallet = await connectArkadeWallet(config);
  const [arkAddress, boardingAddress, balance] = await Promise.all([
    wallet.getAddress(),
    wallet.getBoardingAddress(),
    wallet.getBalance(),
  ]);

  return {
    availableSats: balance.available,
    totalSats: balance.total,
    arkAddress,
    boardingAddress,
  };
}

async function createArkadeLightningClient(config: ArkadeWalletConnectionConfig) {
  const networkConfig = resolveParkerNetworkConfig(config);
  const wallet = await connectArkadeWallet(config);
  const swaps = await import("@arkade-os/boltz-swap");
  const swapProvider = new (swaps as any).BoltzSwapProvider({
    apiUrl: networkConfig.boltzApiUrl,
    network: networkConfig.arkadeNetworkName as never,
  });
  return new (swaps as any).ArkadeLightning({
    wallet,
    swapProvider,
  });
}

export async function onboardArkadeFunds(config: ArkadeWalletConnectionConfig): Promise<string> {
  const sdk = await import("@arkade-os/sdk");
  const wallet = await connectArkadeWallet(config);
  const ramps = new (sdk as any).Ramps(wallet);
  const info = await wallet.arkProvider.getInfo();
  return await ramps.onboard(info.fees);
}

export async function offboardArkadeFunds(
  args: ArkadeWalletConnectionConfig & { address: string; amountSats?: number },
): Promise<string> {
  const sdk = await import("@arkade-os/sdk");
  const wallet = await connectArkadeWallet(args);
  const ramps = new (sdk as any).Ramps(wallet);
  const info = await wallet.arkProvider.getInfo();
  return await ramps.offboard(args.address, info.fees, args.amountSats);
}

export async function createArkadeDepositQuote(args: ArkadeWalletConnectionConfig & { amountSats: number }): Promise<SwapQuote> {
  const lightning = await createArkadeLightningClient(args);
  const invoiceResponse = await lightning.createLightningInvoice({
    amount: args.amountSats,
  });

  return {
    quoteId: crypto.randomUUID(),
    direction: "deposit",
    amountSats: args.amountSats,
    feeSats: 0,
    invoice: invoiceResponse.invoice,
    paymentHash: invoiceResponse.paymentHash,
    expiresAt: new Date(Date.now() + 15 * 60_000).toISOString(),
  };
}

export async function submitArkadeWithdrawal(args: ArkadeWalletConnectionConfig & { lightningInvoice: string }): Promise<SwapJobStatus> {
  const lightning = await createArkadeLightningClient(args);
  const payment = await lightning.sendLightningPayment({
    invoice: args.lightningInvoice,
  });
  const now = new Date().toISOString();

  return {
    swapId: payment.paymentHash ?? payment.id ?? crypto.randomUUID(),
    direction: "withdrawal",
    status: "completed",
    createdAt: now,
    updatedAt: now,
    details: payment.preimage ? `Preimage ${payment.preimage}` : "Lightning withdrawal sent.",
  };
}

class MockSettlementProvider implements SettlementProvider {
  private readonly balances = new Map<string, number>();
  constructor(private readonly network: Network) {}

  createLocalIdentity(seedHex?: string): LocalIdentity {
    const identity = createLocalIdentity(seedHex);
    this.balances.set(identity.playerId, 50_000);
    return identity;
  }

  async getWalletSummary(identity: LocalIdentity): Promise<WalletSummary> {
    const balance = this.balances.get(identity.playerId) ?? 0;
    return {
      availableSats: balance,
      totalSats: balance,
      arkAddress: mockArkAddress(identity.playerId),
      boardingAddress: mockBoardingAddress(identity.playerId),
    };
  }

  async createDepositQuote(identity: LocalIdentity, amountSats: number): Promise<SwapQuote> {
    const now = new Date();
    return {
      quoteId: crypto.randomUUID(),
      direction: "deposit",
      amountSats,
      feeSats: Math.max(5, Math.floor(amountSats * 0.0025)),
      invoice: `ln${this.network}${identity.playerId}${amountSats}`,
      paymentHash: bytesToHex(sha256(utf8ToBytes(`${identity.playerId}:${amountSats}:${now.toISOString()}`))),
      expiresAt: new Date(now.getTime() + 5 * 60_000).toISOString(),
    };
  }

  async submitWithdrawal(identity: LocalIdentity, lightningInvoice: string): Promise<SwapJobStatus> {
    const current = this.balances.get(identity.playerId) ?? 0;
    const debit = Math.min(current, 5_000);
    this.balances.set(identity.playerId, current - debit);
    const now = new Date().toISOString();
    return {
      swapId: bytesToHex(sha256(utf8ToBytes(lightningInvoice))).slice(0, 24),
      direction: "withdrawal",
      status: "completed",
      createdAt: now,
      updatedAt: now,
      details: `Mock withdrawal sent for invoice ${lightningInvoice.slice(0, 16)}...`,
    };
  }

  async buildTableEscrow(request: TableEscrowRequest): Promise<EscrowState> {
    return buildEscrowDescriptor(request);
  }

  createSettlementInstruction(args: {
    tableId: string;
    checkpointId?: string;
    kind: SettlementInstruction["kind"];
    outputs: SettlementInstruction["outputs"];
  }): SettlementInstruction {
    return {
      instructionId: crypto.randomUUID(),
      tableId: args.tableId,
      checkpointId: args.checkpointId,
      kind: args.kind,
      outputs: args.outputs,
      createdAt: new Date().toISOString(),
    };
  }
}

export function createMockSettlementProvider(network: Network = "mutinynet"): SettlementProvider {
  return new MockSettlementProvider(network);
}
