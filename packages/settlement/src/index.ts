import { secp256k1 } from "@noble/curves/secp256k1";
import { sha256 } from "@noble/hashes/sha2";
import { bytesToHex, hexToBytes, utf8ToBytes } from "@noble/hashes/utils";

import type {
  EscrowState,
  SettlementInstruction,
  SwapJobStatus,
  SwapQuote,
  TableCheckpoint,
  TimeoutDelegation,
} from "@parker/protocol";

export const MUTINYNET_ARK_SERVER_URL = "https://mutinynet.arkade.sh";
export const MUTINYNET_BOLTZ_URL = "https://api.boltz.mutinynet.arkade.sh";

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
  arkServerUrl?: string;
  boltzApiUrl?: string;
  networkName?: string;
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
    network: "mutinynet",
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
  const sdk = await import("@arkade-os/sdk");
  const identity = (sdk as any).SingleKey.fromHex(config.privateKeyHex);
  return await (sdk as any).Wallet.create({
    identity,
    arkServerUrl: config.arkServerUrl ?? MUTINYNET_ARK_SERVER_URL,
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
  const wallet = await connectArkadeWallet(config);
  const swaps = await import("@arkade-os/boltz-swap");
  const swapProvider = new (swaps as any).BoltzSwapProvider({
    apiUrl: config.boltzApiUrl ?? MUTINYNET_BOLTZ_URL,
    network: (config.networkName ?? "mutinynet") as never,
  });
  return new (swaps as any).ArkadeLightning({
    wallet,
    swapProvider,
  });
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
      arkAddress: `tark1${identity.playerId.slice(-16)}`,
      boardingAddress: `tb1q${identity.playerId.slice(-20)}`,
    };
  }

  async createDepositQuote(identity: LocalIdentity, amountSats: number): Promise<SwapQuote> {
    const now = new Date();
    return {
      quoteId: crypto.randomUUID(),
      direction: "deposit",
      amountSats,
      feeSats: Math.max(5, Math.floor(amountSats * 0.0025)),
      invoice: `lnmutiny${identity.playerId}${amountSats}`,
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

export function createMockSettlementProvider(): SettlementProvider {
  return new MockSettlementProvider();
}
