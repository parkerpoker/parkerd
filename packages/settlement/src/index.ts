import { secp256k1 } from "@noble/curves/secp256k1";
import { sha256 } from "@noble/hashes/sha2";
import { bytesToHex, concatBytes, hexToBytes, utf8ToBytes } from "@noble/hashes/utils";

import type {
  EscrowState,
  IdentityBinding,
  Network,
  PrivateCardEnvelope,
  SettlementInstruction,
  SwapJobStatus,
  SwapQuote,
  TableFundsOperation,
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

export interface ScopedIdentity {
  id: string;
  privateKeyHex: string;
  publicKeyHex: string;
  scope: "peer" | "protocol" | "wallet";
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

export interface TableFundsParticipant {
  arkAddress: string;
  buyInSats: number;
  peerId: string;
  playerId: string;
}

export interface TableCheckpointRecord {
  balances: Record<string, number>;
  checkpointHash: string;
  participants: TableFundsParticipant[];
  tableId: string;
}

export interface TableFundsStateStore<TState> {
  load(tableId: string): Promise<TState | null>;
  save(tableId: string, state: TState): Promise<void>;
}

export interface TableFundsProvider {
  prepareBuyIn(tableId: string, playerId: string, amountSats: number): Promise<TableFundsOperation>;
  confirmBuyIn(
    tableId: string,
    playerId: string,
    preparedLock: TableFundsOperation,
  ): Promise<TableFundsOperation>;
  recordCheckpoint(record: TableCheckpointRecord): Promise<TableFundsOperation>;
  cooperativeCashOut(
    tableId: string,
    playerId: string,
    balance: number,
    checkpointHash: string,
  ): Promise<TableFundsOperation>;
  cooperativeCloseTable(
    tableId: string,
    balances: Record<string, number>,
    checkpointHash: string,
  ): Promise<TableFundsOperation[]>;
  renewTablePositions(tableId: string): Promise<TableFundsOperation[]>;
  emergencyExit(
    tableId: string,
    playerId: string,
    lastCheckpointHash: string,
    amountSats: number,
  ): Promise<TableFundsOperation>;
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

type CanonicalStructuredData =
  | null
  | boolean
  | number
  | string
  | CanonicalStructuredData[]
  | { [key: string]: CanonicalStructuredData };

export function canonicalizeStructuredData(input: unknown): CanonicalStructuredData {
  if (input === null) {
    return null;
  }

  if (typeof input === "string" || typeof input === "boolean") {
    return input;
  }

  if (typeof input === "number") {
    return Number.isFinite(input) ? input : null;
  }

  if (typeof input === "bigint") {
    return input.toString();
  }

  if (input instanceof Date) {
    return input.toISOString();
  }

  if (Array.isArray(input)) {
    return input.map((value) =>
      value === undefined || typeof value === "function" || typeof value === "symbol"
        ? null
        : canonicalizeStructuredData(value),
    );
  }

  if (ArrayBuffer.isView(input)) {
    return bytesToHex(new Uint8Array(input.buffer, input.byteOffset, input.byteLength));
  }

  if (input && typeof input === "object") {
    const entries = Object.entries(input as Record<string, unknown>)
      .filter(([, value]) => value !== undefined && typeof value !== "function" && typeof value !== "symbol")
      .sort(([left], [right]) => left.localeCompare(right));
    return Object.fromEntries(
      entries.map(([key, value]) => [key, canonicalizeStructuredData(value)]),
    );
  }

  return null;
}

export function stableStringify(input: unknown): string {
  return JSON.stringify(canonicalizeStructuredData(input));
}

export function hashCheckpoint(checkpoint: Omit<TableCheckpoint, "signatures">): string {
  return bytesToHex(sha256(utf8ToBytes(stableStringify(checkpoint))));
}

export function hashMessage(message: string): Uint8Array {
  return sha256(utf8ToBytes(message));
}

function hashStructuredPayload(input: unknown): Uint8Array {
  return sha256(utf8ToBytes(stableStringify(input)));
}

function deriveScopedId(scope: ScopedIdentity["scope"], publicKeyHex: string) {
  const digest = bytesToHex(sha256(hexToBytes(publicKeyHex))).slice(0, 20);
  switch (scope) {
    case "wallet":
      return `player-${digest}`;
    case "protocol":
      return `proto-${digest}`;
    case "peer":
      return `peer-${digest}`;
  }
}

export function createScopedIdentity(
  scope: ScopedIdentity["scope"],
  seedHex = randomHex(32),
): ScopedIdentity {
  const publicKey = secp256k1.getPublicKey(hexToBytes(seedHex), true);
  const publicKeyHex = bytesToHex(publicKey);
  return {
    id: deriveScopedId(scope, publicKeyHex),
    privateKeyHex: seedHex,
    publicKeyHex,
    scope,
  };
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

export function signStructuredData(
  identity: Pick<LocalIdentity | ScopedIdentity, "privateKeyHex">,
  input: unknown,
): string {
  return secp256k1
    .sign(hashStructuredPayload(input), hexToBytes(identity.privateKeyHex))
    .toCompactHex();
}

export function verifyMessage(publicKeyHex: string, message: string, signatureHex: string): boolean {
  return secp256k1.verify(hexToBytes(signatureHex), hashMessage(message), hexToBytes(publicKeyHex));
}

export function verifyStructuredData(
  publicKeyHex: string,
  input: unknown,
  signatureHex: string,
): boolean {
  return secp256k1.verify(
    hexToBytes(signatureHex),
    hashStructuredPayload(input),
    hexToBytes(publicKeyHex),
  );
}

export function unsignedFundsOperation(operation: TableFundsOperation) {
  const { signatureHex, ...unsigned } = operation;
  return canonicalizeStructuredData(unsigned);
}

export function verifyTableFundsOperationSignature(operation: TableFundsOperation): boolean {
  return verifyStructuredData(
    operation.signerPubkeyHex,
    unsignedFundsOperation(operation),
    operation.signatureHex,
  );
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

export function buildIdentityBinding(args: {
  tableId: string;
  peerId: string;
  protocolIdentity: ScopedIdentity;
  walletIdentity: LocalIdentity;
  signedAt?: string;
}): IdentityBinding {
  const bindingBase = {
    tableId: args.tableId,
    peerId: args.peerId,
    protocolId: args.protocolIdentity.id,
    protocolPubkeyHex: args.protocolIdentity.publicKeyHex,
    walletPlayerId: args.walletIdentity.playerId,
    walletPubkeyHex: args.walletIdentity.publicKeyHex,
    signedAt: args.signedAt ?? new Date().toISOString(),
  };

  return {
    ...bindingBase,
    signatureHex: signStructuredData(args.walletIdentity, bindingBase),
  };
}

export function verifyIdentityBinding(binding: IdentityBinding): boolean {
  const unsigned = {
    tableId: binding.tableId,
    peerId: binding.peerId,
    protocolId: binding.protocolId,
    protocolPubkeyHex: binding.protocolPubkeyHex,
    walletPlayerId: binding.walletPlayerId,
    walletPubkeyHex: binding.walletPubkeyHex,
    signedAt: binding.signedAt,
  };
  return verifyStructuredData(binding.walletPubkeyHex, unsigned, binding.signatureHex);
}

function deriveCipherKey(args: {
  senderPrivateKeyHex: string;
  recipientPublicKeyHex: string;
  scope: string;
  nonceHex: string;
}) {
  const sharedSecret = secp256k1.getSharedSecret(
    hexToBytes(args.senderPrivateKeyHex),
    hexToBytes(args.recipientPublicKeyHex),
    true,
  );
  return sha256(
    concatBytes(sharedSecret, utf8ToBytes(args.scope), hexToBytes(args.nonceHex)),
  );
}

function xorWithKeyStream(key: Uint8Array, payload: Uint8Array) {
  const output = new Uint8Array(payload.length);
  let offset = 0;
  let counter = 0;
  while (offset < payload.length) {
    const block = sha256(concatBytes(key, utf8ToBytes(String(counter))));
    const length = Math.min(block.length, payload.length - offset);
    for (let index = 0; index < length; index += 1) {
      output[offset + index] = payload[offset + index]! ^ block[index]!;
    }
    offset += length;
    counter += 1;
  }
  return output;
}

export function encryptScopedPayload(args: {
  senderPrivateKeyHex: string;
  recipientPublicKeyHex: string;
  scope: string;
  payload: unknown;
}): PrivateCardEnvelope {
  const nonceHex = randomHex(16);
  const key = deriveCipherKey({
    senderPrivateKeyHex: args.senderPrivateKeyHex,
    recipientPublicKeyHex: args.recipientPublicKeyHex,
    scope: args.scope,
    nonceHex,
  });
  const plaintext = utf8ToBytes(stableStringify(args.payload));
  const ciphertext = xorWithKeyStream(key, plaintext);
  const ciphertextHex = bytesToHex(ciphertext);
  const authTagHex = bytesToHex(sha256(concatBytes(key, hexToBytes(ciphertextHex))));
  return {
    authTagHex,
    ciphertextHex,
    nonceHex,
  };
}

export function decryptScopedPayload<T>(args: {
  recipientPrivateKeyHex: string;
  senderPublicKeyHex: string;
  scope: string;
  envelope: PrivateCardEnvelope;
}): T {
  const key = deriveCipherKey({
    senderPrivateKeyHex: args.recipientPrivateKeyHex,
    recipientPublicKeyHex: args.senderPublicKeyHex,
    scope: args.scope,
    nonceHex: args.envelope.nonceHex,
  });
  const expectedTag = bytesToHex(
    sha256(concatBytes(key, hexToBytes(args.envelope.ciphertextHex))),
  );
  if (expectedTag !== args.envelope.authTagHex) {
    throw new Error("invalid encrypted payload tag");
  }
  const plaintext = xorWithKeyStream(key, hexToBytes(args.envelope.ciphertextHex));
  return JSON.parse(new TextDecoder().decode(plaintext)) as T;
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
  const createWallet = async (arkProvider: any, indexerProvider: any) =>
    await (sdk as any).Wallet.create({
      identity,
      arkProvider,
      arkServerUrl: networkConfig.arkServerUrl,
      indexerProvider,
    });

  try {
    return await createWallet(
      new (sdk as any).RestArkProvider(networkConfig.arkServerUrl),
      new (sdk as any).RestIndexerProvider(networkConfig.arkServerUrl),
    );
  } catch (error) {
    if (!isArkInfoCompatibilityError(error)) {
      throw error;
    }
    return await createWallet(
      createCompatibleArkProvider(sdk, networkConfig.arkServerUrl),
      createCompatibleIndexerProvider(networkConfig.arkServerUrl),
    );
  }
}

function isArkInfoCompatibilityError(error: unknown) {
  const message = error instanceof Error ? error.message : String(error);
  return message.includes("checkpointTapscript") || message.includes("forfeitPubkey");
}

function createCompatibleArkProvider(sdk: any, arkServerUrl: string) {
  const baseProvider = new sdk.RestArkProvider(arkServerUrl);
  return new Proxy(baseProvider, {
    get(target, property, receiver) {
      if (property === "getInfo") {
        return async () => normalizeArkServerInfo(await target.getInfo(), sdk);
      }
      const value = Reflect.get(target, property, receiver);
      return typeof value === "function" ? value.bind(target) : value;
    },
  });
}

function normalizeArkServerInfo(info: any, sdk: any) {
  if (info.checkpointTapscript && info.forfeitPubkey) {
    return info;
  }
  const compressedSignerPubkey = hexToBytes(info.signerPubkey);
  const xOnlySignerPubkey =
    compressedSignerPubkey.length === 33 ? compressedSignerPubkey.slice(1) : compressedSignerPubkey;
  const timelockValue = BigInt(info.unilateralExitDelay ?? 0n);
  const timelockType = timelockValue < 512n ? "blocks" : "seconds";
  const checkpointTapscript = info.checkpointTapscript
    ? info.checkpointTapscript
    : bytesToHex(
        (sdk as any).CSVMultisigTapscript.encode({
          timelock: {
            type: timelockType,
            value: timelockValue,
          },
          pubkeys: [xOnlySignerPubkey],
        }).script,
      );
  return {
    ...info,
    checkpointTapscript,
    forfeitPubkey: info.forfeitPubkey || info.signerPubkey,
  };
}

function createCompatibleIndexerProvider(serverUrl: string) {
  return {
    serverUrl,
    async getVtxos(opts: {
      outpoints?: Array<{ txid: string; vout: number }>;
      pageIndex?: number;
      pageSize?: number;
      recoverableOnly?: boolean;
      scripts?: string[];
      spendableOnly?: boolean;
      spentOnly?: boolean;
    }) {
      if (!opts?.scripts && !opts?.outpoints) {
        throw new Error("Either scripts or outpoints must be provided");
      }
      if (opts?.scripts && opts?.outpoints) {
        throw new Error("scripts and outpoints are mutually exclusive options");
      }
      const params = new URLSearchParams();
      opts.scripts?.forEach((script) => {
        params.append("scripts", script);
      });
      opts.outpoints?.forEach((outpoint) => {
        params.append("outpoints", `${outpoint.txid}:${outpoint.vout}`);
      });
      if (opts.spendableOnly !== undefined) {
        params.append("spendableOnly", String(opts.spendableOnly));
      }
      if (opts.spentOnly !== undefined) {
        params.append("spentOnly", String(opts.spentOnly));
      }
      if (opts.recoverableOnly !== undefined) {
        params.append("recoverableOnly", String(opts.recoverableOnly));
      }
      if (opts.pageIndex !== undefined) {
        params.append("page.index", String(opts.pageIndex));
      }
      if (opts.pageSize !== undefined) {
        params.append("page.size", String(opts.pageSize));
      }
      const url = `${serverUrl}/v1/vtxos${params.toString() ? `?${params.toString()}` : ""}`;
      const response = await fetch(url);
      if (!response.ok) {
        throw new Error(`Failed to fetch vtxos: ${response.statusText}`);
      }
      return await response.json();
    },
  };
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

async function waitForArkadeBoardingFunds(wallet: any, timeoutMs = 60_000) {
  const start = Date.now();
  while (Date.now() - start < timeoutMs) {
    const balance = await wallet.getBalance();
    const boardingTotal = Number(balance?.boarding?.total ?? 0);
    if (boardingTotal > 0) {
      return;
    }
    await new Promise<void>((resolve) => {
      setTimeout(resolve, 500);
    });
  }
  throw new Error("timed out waiting for Arkade boarding funds");
}

function isRetryableArkadeOnboardError(error: unknown) {
  const message = error instanceof Error ? error.message : String(error);
  return (
    message.includes("missing inputs") ||
    message.includes("missingorspent") ||
    message.includes("No boarding utxos available after deducting fees")
  );
}

async function onboardWithCompatibleRamps(wallet: any, sdk: any) {
  await waitForArkadeBoardingFunds(wallet);
  const ramps = new (sdk as any).Ramps(wallet);
  const start = Date.now();
  while (Date.now() - start < 60_000) {
    const info = await wallet.arkProvider.getInfo().catch(() => undefined);
    try {
      if (info?.fees !== undefined) {
        return await ramps.onboard(info.fees);
      }
      return await ramps.onboard();
    } catch (error) {
      if (!isRetryableArkadeOnboardError(error)) {
        throw error;
      }
      await new Promise<void>((resolve) => {
        setTimeout(resolve, 500);
      });
    }
  }
  throw new Error("timed out waiting for Arkade onboarding inputs");
}

async function offboardWithCompatibleRamps(
  wallet: any,
  sdk: any,
  address: string,
  amountSats?: number,
) {
  const ramps = new (sdk as any).Ramps(wallet);
  const info = await wallet.arkProvider.getInfo().catch(() => undefined);
  if (info?.fees !== undefined) {
    return await ramps.offboard(address, info.fees, amountSats);
  }
  return await ramps.offboard(address, amountSats === undefined ? undefined : BigInt(amountSats));
}

async function recoverArkadeVtxos(wallet: any, sdk: any, thresholdMs: number) {
  if (!(sdk as any).VtxoManager) {
    throw new Error("Arkade VtxoManager is unavailable");
  }
  const manager = new (sdk as any).VtxoManager(wallet, {
    enabled: true,
    thresholdMs,
  });
  return await manager.recoverVtxos();
}

export async function onboardArkadeFunds(config: ArkadeWalletConnectionConfig): Promise<string> {
  const sdk = await import("@arkade-os/sdk");
  const wallet = await connectArkadeWallet(config);
  return await onboardWithCompatibleRamps(wallet, sdk);
}

export async function offboardArkadeFunds(
  args: ArkadeWalletConnectionConfig & { address: string; amountSats?: number },
): Promise<string> {
  const sdk = await import("@arkade-os/sdk");
  const wallet = await connectArkadeWallet(args);
  return await offboardWithCompatibleRamps(wallet, sdk, args.address, args.amountSats);
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

class SignedTableFundsProvider implements TableFundsProvider {
  private readonly positions = new Map<string, TableFundsOperation>();

  constructor(
    private readonly signer: LocalIdentity,
    private readonly networkId: string,
    private readonly providerName: string,
    private readonly defaultExpiryMs: number,
  ) {}

  async prepareBuyIn(
    tableId: string,
    playerId: string,
    amountSats: number,
  ): Promise<TableFundsOperation> {
    return this.createOperation({
      amountSats,
      kind: "buy-in-prepared",
      playerId,
      status: "prepared",
      tableId,
      vtxoExpiry: new Date(Date.now() + this.defaultExpiryMs).toISOString(),
    });
  }

  async confirmBuyIn(
    tableId: string,
    playerId: string,
    preparedLock: TableFundsOperation,
  ): Promise<TableFundsOperation> {
    const operation = this.createOperation({
      amountSats: preparedLock.amountSats,
      kind: "buy-in-locked",
      playerId,
      status: "locked",
      tableId,
      vtxoExpiry: new Date(Date.now() + this.defaultExpiryMs).toISOString(),
    });
    this.positions.set(tableId, operation);
    return operation;
  }

  async recordCheckpoint(record: TableCheckpointRecord): Promise<TableFundsOperation> {
    const amountSats =
      record.balances[this.signer.playerId] ??
      Object.values(record.balances).reduce((total, amount) => total + amount, 0);
    return this.createOperation({
      amountSats,
      checkpointHash: record.checkpointHash,
      kind: "checkpoint-recorded",
      playerId: this.signer.playerId,
      status: "recorded",
      tableId: record.tableId,
      ...(this.positions.get(record.tableId)?.vtxoExpiry
        ? { vtxoExpiry: this.positions.get(record.tableId)!.vtxoExpiry }
        : {}),
    });
  }

  async cooperativeCashOut(
    tableId: string,
    playerId: string,
    balance: number,
    checkpointHash: string,
  ): Promise<TableFundsOperation> {
    return this.createOperation({
      amountSats: balance,
      checkpointHash,
      kind: "cashout",
      playerId,
      status: "completed",
      tableId,
      ...(this.positions.get(tableId)?.vtxoExpiry
        ? { vtxoExpiry: this.positions.get(tableId)!.vtxoExpiry }
        : {}),
    });
  }

  async cooperativeCloseTable(
    tableId: string,
    balances: Record<string, number>,
    checkpointHash: string,
  ): Promise<TableFundsOperation[]> {
    return Object.entries(balances).map(([playerId, amountSats]) =>
      this.createOperation({
        amountSats,
        checkpointHash,
        kind: "close-table",
        playerId,
        status: "completed",
        tableId,
        ...(this.positions.get(tableId)?.vtxoExpiry
          ? { vtxoExpiry: this.positions.get(tableId)!.vtxoExpiry }
          : {}),
      }),
    );
  }

  async renewTablePositions(tableId: string): Promise<TableFundsOperation[]> {
    const current = this.positions.get(tableId);
    if (!current) {
      return [];
    }
    const renewed = this.createOperation({
      amountSats: current.amountSats,
      kind: "renewal",
      playerId: current.playerId,
      status: "renewed",
      tableId,
      vtxoExpiry: new Date(Date.now() + this.defaultExpiryMs).toISOString(),
      ...(current.checkpointHash ? { checkpointHash: current.checkpointHash } : {}),
    });
    this.positions.set(tableId, renewed);
    return [renewed];
  }

  async emergencyExit(
    tableId: string,
    playerId: string,
    lastCheckpointHash: string,
    amountSats: number,
  ): Promise<TableFundsOperation> {
    return this.createOperation({
      amountSats,
      checkpointHash: lastCheckpointHash,
      kind: "emergency-exit",
      playerId,
      status: "exited",
      tableId,
      ...(this.positions.get(tableId)?.vtxoExpiry
        ? { vtxoExpiry: this.positions.get(tableId)!.vtxoExpiry }
        : {}),
    });
  }

  private createOperation(args: {
    amountSats: number;
    checkpointHash?: string | undefined;
    kind: TableFundsOperation["kind"];
    playerId: string;
    status: TableFundsOperation["status"];
    tableId: string;
    vtxoExpiry?: string | undefined;
  }): TableFundsOperation {
    const base = {
      operationId: crypto.randomUUID(),
      kind: args.kind,
      provider: this.providerName,
      tableId: args.tableId,
      playerId: args.playerId,
      networkId: this.networkId,
      amountSats: args.amountSats,
      ...(args.checkpointHash ? { checkpointHash: args.checkpointHash } : {}),
      ...(args.vtxoExpiry ? { vtxoExpiry: args.vtxoExpiry } : {}),
      createdAt: new Date().toISOString(),
      status: args.status,
      signerPubkeyHex: this.signer.publicKeyHex,
    } satisfies Omit<TableFundsOperation, "signatureHex">;

    return {
      ...base,
      signatureHex: signStructuredData(this.signer, base),
    };
  }
}

export interface ArkadeVtxoRef {
  amountSats: number;
  expiresAt?: string | undefined;
  txid: string;
  vout: number;
}

interface ArkadeCheckpointTransfer {
  amountSats: number;
  fromPlayerId: string;
  status: "completed" | "pending";
  toArkAddress: string;
  toPlayerId: string;
  txid?: string | undefined;
}

export interface ArkadeCheckpointState {
  balances: Record<string, number>;
  checkpointHash: string;
  participants: TableFundsParticipant[];
  recordedAt: string;
  transfers: ArkadeCheckpointTransfer[];
}

export interface ArkadeTableFundsState {
  amountSats: number;
  cashoutTxid?: string | undefined;
  checkpoint?: ArkadeCheckpointState;
  emergencyExitTxid?: string | undefined;
  managedVtxos: ArkadeVtxoRef[];
  preparedVtxos: ArkadeVtxoRef[];
  tableId: string;
  vtxoExpiry?: string | undefined;
}

class InMemoryTableFundsStateStore<TState> implements TableFundsStateStore<TState> {
  private readonly stateByTableId = new Map<string, TState>();

  async load(tableId: string): Promise<TState | null> {
    return this.stateByTableId.get(tableId) ?? null;
  }

  async save(tableId: string, state: TState): Promise<void> {
    this.stateByTableId.set(tableId, state);
  }
}

function outpointKey(outpoint: Pick<ArkadeVtxoRef, "txid" | "vout">) {
  return `${outpoint.txid}:${outpoint.vout}`;
}

function sumAmounts(values: Array<{ amountSats: number }>) {
  return values.reduce((total, value) => total + value.amountSats, 0);
}

function minExpiry(refs: ArkadeVtxoRef[]) {
  const expiries = refs
    .map((ref) => ref.expiresAt)
    .filter((value): value is string => Boolean(value))
    .sort((left, right) => Date.parse(left) - Date.parse(right));
  return expiries[0];
}

function dedupeVtxoRefs(refs: ArkadeVtxoRef[]) {
  const byOutpoint = new Map<string, ArkadeVtxoRef>();
  for (const ref of refs) {
    byOutpoint.set(outpointKey(ref), ref);
  }
  return [...byOutpoint.values()];
}

function normalizeBatchExpiry(batchExpiry: number | undefined) {
  if (!batchExpiry || !Number.isFinite(batchExpiry)) {
    return undefined;
  }
  if (batchExpiry < 1_000_000_000) {
    return undefined;
  }
  const ms = batchExpiry > 1_000_000_000_000 ? batchExpiry : batchExpiry * 1_000;
  return new Date(ms).toISOString();
}

function isSettledArkadeCoin(coin: any) {
  const state = coin?.virtualStatus?.state ?? "settled";
  return (
    (state === "settled" || state === "preconfirmed") &&
    (coin?.spentBy === undefined || coin.spentBy === "")
  );
}

function isRetryableArkadeSettleError(error: unknown) {
  const message = error instanceof Error ? error.message : String(error);
  return (
    message.includes("duplicated input") ||
    message.includes("already registered by another intent") ||
    message.includes("missing inputs") ||
    message.includes("missingorspent")
  );
}

function buildCheckpointTransfers(
  previousBalances: Record<string, number>,
  nextBalances: Record<string, number>,
  participants: TableFundsParticipant[],
): ArkadeCheckpointTransfer[] {
  const participantById = new Map(participants.map((participant) => [participant.playerId, participant] as const));
  const losers = Object.entries(nextBalances)
    .map(([playerId, nextBalance]) => ({
      deltaSats: (nextBalance ?? 0) - (previousBalances[playerId] ?? 0),
      playerId,
    }))
    .filter((entry) => entry.deltaSats < 0)
    .sort((left, right) => left.playerId.localeCompare(right.playerId))
    .map((entry) => ({
      amountSats: Math.abs(entry.deltaSats),
      playerId: entry.playerId,
    }));
  const winners = Object.entries(nextBalances)
    .map(([playerId, nextBalance]) => ({
      deltaSats: (nextBalance ?? 0) - (previousBalances[playerId] ?? 0),
      playerId,
    }))
    .filter((entry) => entry.deltaSats > 0)
    .sort((left, right) => left.playerId.localeCompare(right.playerId))
    .map((entry) => ({
      amountSats: entry.deltaSats,
      playerId: entry.playerId,
    }));

  const transfers: ArkadeCheckpointTransfer[] = [];
  let loserIndex = 0;
  let winnerIndex = 0;

  while (loserIndex < losers.length && winnerIndex < winners.length) {
    const loser = losers[loserIndex]!;
    const winner = winners[winnerIndex]!;
    const winnerParticipant = participantById.get(winner.playerId);
    if (!winnerParticipant) {
      throw new Error(`checkpoint participant ${winner.playerId} is missing an Arkade address`);
    }
    const amountSats = Math.min(loser.amountSats, winner.amountSats);
    if (amountSats <= 0) {
      break;
    }
    transfers.push({
      amountSats,
      fromPlayerId: loser.playerId,
      status: "pending",
      toArkAddress: winnerParticipant.arkAddress,
      toPlayerId: winner.playerId,
    });
    loser.amountSats -= amountSats;
    winner.amountSats -= amountSats;
    if (loser.amountSats === 0) {
      loserIndex += 1;
    }
    if (winner.amountSats === 0) {
      winnerIndex += 1;
    }
  }

  const unresolvedLosers = losers.slice(loserIndex).some((entry) => entry.amountSats > 0);
  const unresolvedWinners = winners.slice(winnerIndex).some((entry) => entry.amountSats > 0);
  if (unresolvedLosers || unresolvedWinners) {
    throw new Error("checkpoint balances do not resolve to a deterministic transfer plan");
  }

  return transfers;
}

class ArkadeTableFundsProvider implements TableFundsProvider {
  private readonly networkConfig: ParkerNetworkConfig;
  private readonly store: TableFundsStateStore<ArkadeTableFundsState>;
  private readonly tableOperationLocks = new Map<string, Promise<void>>();
  private walletPromise: Promise<any> | undefined;

  constructor(
    private readonly signer: LocalIdentity,
    networkConfig: Pick<ArkadeWalletConnectionConfig, "arkServerUrl" | "arkadeNetworkName" | "boltzApiUrl" | "network">,
    private readonly providerName: string,
    private readonly defaultExpiryMs: number,
    store?: TableFundsStateStore<ArkadeTableFundsState>,
  ) {
    this.networkConfig = resolveParkerNetworkConfig(networkConfig);
    this.store = store ?? new InMemoryTableFundsStateStore<ArkadeTableFundsState>();
  }

  async prepareBuyIn(
    tableId: string,
    playerId: string,
    amountSats: number,
  ): Promise<TableFundsOperation> {
    return await this.withTableLock(tableId, async () => {
      const wallet = await this.getWallet();
      const spendable = await wallet.getVtxos({ withRecoverable: false });
      const preparedCoins = this.selectVtxos(
        spendable.map((coin: any) => this.toVtxoRef(coin)),
        amountSats,
      );
      if (sumAmounts(preparedCoins) < amountSats) {
        throw new Error(`insufficient Arkade VTXOs to prepare a buy-in of ${amountSats} sats`);
      }
      await this.saveState({
        amountSats: 0,
        managedVtxos: [],
        preparedVtxos: preparedCoins,
        tableId,
        vtxoExpiry: minExpiry(preparedCoins),
      });
      return this.createOperation({
        amountSats,
        kind: "buy-in-prepared",
        playerId,
        status: "prepared",
        tableId,
        vtxoExpiry: minExpiry(preparedCoins),
      });
    });
  }

  async confirmBuyIn(
    tableId: string,
    playerId: string,
    preparedLock: TableFundsOperation,
  ): Promise<TableFundsOperation> {
    return await this.withTableLock(tableId, async () => {
      if (!verifyTableFundsOperationSignature(preparedLock)) {
        throw new Error("prepared buy-in receipt failed wallet-signature verification");
      }
      const state = await this.requireState(tableId);
      if (sumAmounts(state.preparedVtxos) < preparedLock.amountSats) {
        throw new Error("prepared buy-in amount exceeds the reserved Arkade VTXO set");
      }
      await this.rebalancePosition(tableId, state, preparedLock.amountSats, []);
      const confirmed = await this.requireState(tableId);
      return this.createOperation({
        amountSats: preparedLock.amountSats,
        kind: "buy-in-locked",
        playerId,
        status: "locked",
        tableId,
        vtxoExpiry: confirmed.vtxoExpiry,
      });
    });
  }

  async recordCheckpoint(record: TableCheckpointRecord): Promise<TableFundsOperation> {
    return await this.withTableLock(record.tableId, async () => {
      const state = await this.requireState(record.tableId);
      const localBalance = record.balances[this.signer.playerId];
      if (localBalance === undefined) {
        throw new Error(`checkpoint ${record.checkpointHash} does not include the local player balance`);
      }

      const previousBalances =
        state.checkpoint?.balances ??
        Object.fromEntries(record.participants.map((participant) => [participant.playerId, participant.buyInSats]));
      const transfers = buildCheckpointTransfers(previousBalances, record.balances, record.participants);
      const outgoingTransfers = transfers.filter((transfer) => transfer.fromPlayerId === this.signer.playerId);
      const incomingTotal = transfers
        .filter((transfer) => transfer.toPlayerId === this.signer.playerId)
        .reduce((total, transfer) => total + transfer.amountSats, 0);

      const wallet = await this.getWallet();
      const beforeIncomingKeys = new Set<string>(
        (await wallet.getVtxos({ withRecoverable: false })).map((coin: any) => outpointKey(this.toVtxoRef(coin))),
      );

      let txid: string | undefined;
      let completedTransfers = transfers;

      if (outgoingTransfers.length > 0) {
        const outgoingResults = await this.applyCheckpointTransfers(
          record.tableId,
          state,
          localBalance,
          outgoingTransfers,
        );
        txid = outgoingResults[outgoingResults.length - 1]?.txid;
        completedTransfers = transfers.map((transfer) =>
          transfer.fromPlayerId === this.signer.playerId
            ? {
                ...transfer,
                status: "completed",
                txid: outgoingResults.find((candidate) =>
                  candidate.amountSats === transfer.amountSats &&
                  candidate.toPlayerId === transfer.toPlayerId,
                )?.txid,
              }
            : transfer,
        );
      } else if (incomingTotal > 0) {
        const incomingRefs = await this.waitForIncomingTransfers(beforeIncomingKeys, incomingTotal);
        state.managedVtxos = dedupeVtxoRefs([...state.managedVtxos, ...incomingRefs]);
        state.preparedVtxos = [];
        state.amountSats = localBalance;
        state.vtxoExpiry = minExpiry(state.managedVtxos) ?? state.vtxoExpiry;
        await this.saveState(state);
        completedTransfers = transfers.map((transfer) =>
          transfer.toPlayerId === this.signer.playerId ? { ...transfer, status: "completed" } : transfer,
        );
      }

      const nextState = await this.requireState(record.tableId);
      nextState.amountSats = localBalance;
      nextState.checkpoint = {
        balances: { ...record.balances },
        checkpointHash: record.checkpointHash,
        participants: record.participants.map((participant) => ({ ...participant })),
        recordedAt: new Date().toISOString(),
        transfers: completedTransfers,
      };
      nextState.vtxoExpiry = minExpiry(nextState.managedVtxos) ?? nextState.vtxoExpiry;
      await this.saveState(nextState);

      return this.createOperation({
        amountSats: localBalance,
        checkpointHash: record.checkpointHash,
        kind: "checkpoint-recorded",
        playerId: this.signer.playerId,
        status: "recorded",
        tableId: record.tableId,
        vtxoExpiry: nextState.vtxoExpiry,
      });
    });
  }

  async cooperativeCashOut(
    tableId: string,
    playerId: string,
    balance: number,
    checkpointHash: string,
  ): Promise<TableFundsOperation> {
    return await this.withTableLock(tableId, async () => {
      const state = await this.requireState(tableId);
      if (state.checkpoint?.checkpointHash !== checkpointHash) {
        throw new Error("cash-out must use the latest recorded checkpoint hash");
      }
      if (state.amountSats !== balance) {
        throw new Error(`cash-out balance ${balance} does not match the recorded Arkade table position ${state.amountSats}`);
      }
      if (balance === 0) {
        state.cashoutTxid = undefined;
        state.managedVtxos = [];
        state.amountSats = 0;
        state.vtxoExpiry = undefined;
        await this.saveState(state);
        return this.createOperation({
          amountSats: 0,
          checkpointHash,
          kind: "cashout",
          playerId,
          status: "completed",
          tableId,
        });
      }
      if (await this.canReleaseManagedPosition(state, balance)) {
        state.cashoutTxid = undefined;
        state.managedVtxos = [];
        state.amountSats = 0;
        state.vtxoExpiry = undefined;
        await this.saveState(state);
        return this.createOperation({
          amountSats: balance,
          checkpointHash,
          kind: "cashout",
          playerId,
          status: "completed",
          tableId,
        });
      }
      const wallet = await this.getWallet();
      const boardingAddress = await wallet.getBoardingAddress();
      let txid: string | undefined;
      let settleError: unknown;
      for (let attempt = 0; attempt < 10; attempt += 1) {
        const managedCoins = await this.resolveCoins(state.managedVtxos);
        if (managedCoins.length === 0) {
          throw new Error(`no managed Arkade VTXOs are available to cash out table ${tableId}`);
        }
        try {
          txid = await wallet.settle({
            inputs: managedCoins,
            outputs: [
              {
                address: boardingAddress,
                amount: BigInt(balance),
              },
            ],
          });
          break;
        } catch (error) {
          settleError = error;
          if (await this.canReleaseManagedPosition(state, balance)) {
            break;
          }
          if (!isRetryableArkadeSettleError(error) || attempt === 9) {
            break;
          }
          await new Promise<void>((resolve) => {
            setTimeout(resolve, 1_000 * (attempt + 1));
          });
        }
      }
      if (!txid && !(await this.canReleaseManagedPosition(state, balance))) {
        throw settleError instanceof Error ? settleError : new Error(String(settleError));
      }
      state.cashoutTxid = txid;
      state.managedVtxos = [];
      state.amountSats = 0;
      state.vtxoExpiry = undefined;
      await this.saveState(state);
      return this.createOperation({
        amountSats: balance,
        checkpointHash,
        kind: "cashout",
        playerId,
        status: "completed",
        tableId,
      });
    });
  }

  async cooperativeCloseTable(
    tableId: string,
    balances: Record<string, number>,
    checkpointHash: string,
  ): Promise<TableFundsOperation[]> {
    const localBalance = balances[this.signer.playerId];
    if (localBalance === undefined) {
      return [];
    }
    return [
      await this.cooperativeCashOut(tableId, this.signer.playerId, localBalance, checkpointHash),
    ];
  }

  async renewTablePositions(tableId: string): Promise<TableFundsOperation[]> {
    return await this.withTableLock(tableId, async () => {
      const state = await this.store.load(tableId);
      if (!state || state.amountSats <= 0) {
        return [];
      }
      await this.rebalancePosition(tableId, state, state.amountSats, []);
      const renewed = await this.requireState(tableId);
      return [
        this.createOperation({
          amountSats: renewed.amountSats,
          ...(renewed.checkpoint?.checkpointHash ? { checkpointHash: renewed.checkpoint.checkpointHash } : {}),
          kind: "renewal",
          playerId: this.signer.playerId,
          status: "renewed",
          tableId,
          vtxoExpiry: renewed.vtxoExpiry,
        }),
      ];
    });
  }

  async emergencyExit(
    tableId: string,
    playerId: string,
    lastCheckpointHash: string,
    amountSats: number,
  ): Promise<TableFundsOperation> {
    return await this.withTableLock(tableId, async () => {
      const state = await this.requireState(tableId);
      if (state.checkpoint?.checkpointHash !== lastCheckpointHash) {
        throw new Error("emergency exit must use the latest recorded checkpoint hash");
      }
      if (amountSats === 0) {
        state.emergencyExitTxid = undefined;
        state.amountSats = 0;
        state.managedVtxos = [];
        state.vtxoExpiry = undefined;
        await this.saveState(state);
        return this.createOperation({
          amountSats: 0,
          checkpointHash: lastCheckpointHash,
          kind: "emergency-exit",
          playerId,
          status: "exited",
          tableId,
        });
      }
      if (await this.canReleaseManagedPosition(state, amountSats)) {
        state.emergencyExitTxid = undefined;
        state.amountSats = 0;
        state.managedVtxos = [];
        state.vtxoExpiry = undefined;
        await this.saveState(state);
        return this.createOperation({
          amountSats,
          checkpointHash: lastCheckpointHash,
          kind: "emergency-exit",
          playerId,
          status: "exited",
          tableId,
        });
      }
      const wallet = await this.getWallet();
      const boardingAddress = await wallet.getBoardingAddress();
      let settled = false;
      let settleError: unknown;
      for (let attempt = 0; attempt < 10; attempt += 1) {
        try {
          const managedCoins = await this.resolveCoins(state.managedVtxos);
          if (managedCoins.length === 0) {
            throw new Error(`no managed Arkade VTXOs are available to exit table ${tableId}`);
          }
          state.emergencyExitTxid = await wallet.settle({
            inputs: managedCoins,
            outputs: [
              {
                address: boardingAddress,
                amount: BigInt(amountSats),
              },
            ],
          });
          settled = true;
          break;
        } catch (error) {
          settleError = error;
          if (await this.canReleaseManagedPosition(state, amountSats)) {
            break;
          }
          if (!isRetryableArkadeSettleError(error) || attempt === 9) {
            break;
          }
          await new Promise<void>((resolve) => {
            setTimeout(resolve, 1_000 * (attempt + 1));
          });
        }
      }
      if (!settled) {
        if (await this.canReleaseManagedPosition(state, amountSats)) {
          settled = true;
        } else {
          const sdk = await import("@arkade-os/sdk");
          if ((sdk as any).VtxoManager) {
            state.emergencyExitTxid = await recoverArkadeVtxos(wallet, sdk, this.defaultExpiryMs);
            settled = true;
          } else {
            throw settleError instanceof Error ? settleError : new Error(String(settleError));
          }
        }
      }
      state.amountSats = 0;
      state.managedVtxos = [];
      state.vtxoExpiry = undefined;
      await this.saveState(state);
      return this.createOperation({
        amountSats,
        checkpointHash: lastCheckpointHash,
        kind: "emergency-exit",
        playerId,
        status: "exited",
        tableId,
      });
    });
  }

  private createOperation(args: {
    amountSats: number;
    checkpointHash?: string | undefined;
    kind: TableFundsOperation["kind"];
    playerId: string;
    status: TableFundsOperation["status"];
    tableId: string;
    vtxoExpiry?: string | undefined;
  }): TableFundsOperation {
    const base = {
      operationId: crypto.randomUUID(),
      kind: args.kind,
      provider: this.providerName,
      tableId: args.tableId,
      playerId: args.playerId,
      networkId: this.networkConfig.network,
      amountSats: args.amountSats,
      ...(args.checkpointHash ? { checkpointHash: args.checkpointHash } : {}),
      ...(args.vtxoExpiry ? { vtxoExpiry: args.vtxoExpiry } : {}),
      createdAt: new Date().toISOString(),
      status: args.status,
      signerPubkeyHex: this.signer.publicKeyHex,
    } satisfies Omit<TableFundsOperation, "signatureHex">;

    return {
      ...base,
      signatureHex: signStructuredData(this.signer, base),
    };
  }

  private async getWallet() {
    if (!this.walletPromise) {
      this.walletPromise = connectArkadeWallet({
        arkServerUrl: this.networkConfig.arkServerUrl,
        arkadeNetworkName: this.networkConfig.arkadeNetworkName,
        boltzApiUrl: this.networkConfig.boltzApiUrl,
        network: this.networkConfig.network,
        privateKeyHex: this.signer.privateKeyHex,
      });
    }
    return await this.walletPromise;
  }

  private async applyCheckpointTransfers(
    tableId: string,
    state: ArkadeTableFundsState,
    localBalance: number,
    outgoingTransfers: ArkadeCheckpointTransfer[],
  ) {
    const wallet = await this.getWallet();
    const results: Array<{ amountSats: number; toPlayerId: string; txid: string }> = [];
    let remainingBalance =
      state.amountSats > 0 ? state.amountSats : sumAmounts(state.managedVtxos);
    for (const transfer of outgoingTransfers) {
      const beforeKeys = new Set<string>(
        (await this.listWalletVtxos())
          .filter((coin) => isSettledArkadeCoin(coin))
          .map((coin) => outpointKey(this.toVtxoRef(coin))),
      );
      const managedCoins = await this.resolveCoins(state.managedVtxos);
      const txid = await this.sendManagedOffchainTransfer(wallet, managedCoins, transfer.toArkAddress, transfer.amountSats);
      remainingBalance -= transfer.amountSats;
      if (remainingBalance < 0) {
        throw new Error(`checkpoint transfer overpaid table ${tableId}; remaining balance became negative`);
      }
      state.amountSats = remainingBalance;
      state.managedVtxos = remainingBalance > 0 ? [await this.waitForExactNewVtxo(beforeKeys, remainingBalance)] : [];
      state.preparedVtxos = [];
      state.vtxoExpiry = minExpiry(state.managedVtxos);
      await this.saveState(state);
      results.push({
        amountSats: transfer.amountSats,
        toPlayerId: transfer.toPlayerId,
        txid,
      });
    }
    if (remainingBalance !== localBalance) {
      throw new Error(
        `checkpoint transfer remainder ${remainingBalance} does not match expected balance ${localBalance} for table ${tableId}`,
      );
    }
    return results;
  }

  private async listWalletVtxos() {
    const wallet = await this.getWallet();
    return (await wallet.getVtxos({ withRecoverable: false })) as Array<any>;
  }

  private async rebalancePosition(
    tableId: string,
    state: ArkadeTableFundsState,
    nextAmountSats: number,
    payoutOutputs: Array<{ address: string; amount: bigint }>,
  ) {
    const wallet = await this.getWallet();
    const before = await this.listWalletVtxos();
    const beforeKeys = new Set(before.map((coin) => outpointKey(this.toVtxoRef(coin))));
    const ownArkAddress = await wallet.getAddress();
    for (let attempt = 0; attempt < 10; attempt += 1) {
      const managedCoins = state.managedVtxos.length > 0
        ? await this.resolveCoins(state.managedVtxos)
        : await this.resolveCoins(state.preparedVtxos);
      const inputs = [...managedCoins];
      if (inputs.length === 0) {
        throw new Error(`no Arkade VTXOs are available to rebalance table ${tableId}`);
      }
      const totalInputSats = inputs.reduce((total, coin) => total + Number(coin.value ?? coin.amount ?? 0), 0);
      const requestedOutputSats =
        nextAmountSats + payoutOutputs.reduce((total, output) => total + Number(output.amount), 0);
      if (totalInputSats < requestedOutputSats) {
        throw new Error(
          `Arkade rebalance for table ${tableId} requires ${requestedOutputSats} sats but only ${totalInputSats} sats are available`,
        );
      }
      const changeSats = totalInputSats - requestedOutputSats;
      const outputs = [
        ...(changeSats > 0 ? [{ address: ownArkAddress, amount: BigInt(changeSats) }] : []),
        ...payoutOutputs,
        ...(nextAmountSats > 0 ? [{ address: ownArkAddress, amount: BigInt(nextAmountSats) }] : []),
      ];
      try {
        const txid = await wallet.settle({
          inputs,
          outputs,
        });
        state.amountSats = nextAmountSats;
        state.managedVtxos =
          nextAmountSats > 0 ? [await this.waitForExactNewVtxo(beforeKeys, nextAmountSats)] : [];
        state.preparedVtxos = [];
        state.vtxoExpiry = minExpiry(state.managedVtxos);
        await this.saveState(state);
        return txid;
      } catch (error) {
        if (!isRetryableArkadeSettleError(error) || attempt === 9) {
          throw error;
        }
        await new Promise<void>((resolve) => {
          setTimeout(resolve, 1_000 * (attempt + 1));
        });
      }
    }
    throw new Error(`failed to rebalance Arkade table ${tableId}`);
  }

  private async sendManagedOffchainTransfer(
    wallet: any,
    managedCoins: any[],
    recipientArkAddress: string,
    amountSats: number,
  ) {
    if (amountSats <= 0) {
      throw new Error("managed transfer amount must be positive");
    }
    const totalManaged = managedCoins.reduce((total, coin) => total + Number(coin.value ?? coin.amount ?? 0), 0);
    if (totalManaged < amountSats) {
      throw new Error(`managed Arkade position only has ${totalManaged} sats available for a ${amountSats} sat transfer`);
    }

    const [{ buildOffchainTx }, { ArkAddress }, { Transaction }] = await Promise.all([
      import(new URL("../node_modules/@arkade-os/sdk/dist/esm/utils/arkTransaction.js", import.meta.url).href),
      import(new URL("../node_modules/@arkade-os/sdk/dist/esm/script/address.js", import.meta.url).href),
      import("@scure/btc-signer"),
    ]);
    const ownArkAddress = await wallet.getAddress();
    const outputScriptForAddress = (address: string, outputAmountSats: number) => {
      const decoded = ArkAddress.decode(address);
      return BigInt(outputAmountSats) < wallet.dustAmount ? decoded.subdustPkScript : decoded.pkScript;
    };
    const outputs = [
      {
        amount: BigInt(amountSats),
        script: outputScriptForAddress(recipientArkAddress, amountSats),
      },
      ...(totalManaged > amountSats
        ? [
            {
              amount: BigInt(totalManaged - amountSats),
              script: outputScriptForAddress(ownArkAddress, totalManaged - amountSats),
            },
          ]
        : []),
    ];
    const inputs = managedCoins.map((coin) => ({
      tapLeafScript: coin.forfeitTapLeafScript,
      tapTree: coin.tapTree,
      txid: coin.txid,
      value: Number(coin.value ?? coin.amount ?? 0),
      vout: coin.vout,
    }));
    const offchainTx = buildOffchainTx(inputs, outputs, wallet.serverUnrollScript);
    const signedVirtualTx = await wallet.identity.sign(offchainTx.arkTx);
    const encodePsbt = (tx: { toPSBT(): Uint8Array }) => Buffer.from(tx.toPSBT()).toString("base64");
    const { arkTxid, signedCheckpointTxs } = await wallet.arkProvider.submitTx(
      encodePsbt(signedVirtualTx),
      offchainTx.checkpoints.map((checkpoint: { toPSBT(): Uint8Array }) => encodePsbt(checkpoint)),
    );
    const finalCheckpoints = await Promise.all(
      signedCheckpointTxs.map(async (encodedCheckpoint: string) => {
        const tx = Transaction.fromPSBT(Buffer.from(encodedCheckpoint, "base64"));
        const signedCheckpoint = await wallet.identity.sign(tx);
        return encodePsbt(signedCheckpoint);
      }),
    );
    await wallet.arkProvider.finalizeTx(arkTxid, finalCheckpoints);
    return arkTxid;
  }

  private async requireState(tableId: string) {
    const state = await this.store.load(tableId);
    if (!state) {
      throw new Error(`no Arkade table-funds state exists for table ${tableId}`);
    }
    return state;
  }

  private async resolveCoins(refs: ArkadeVtxoRef[], timeoutMs = 30_000) {
    if (refs.length === 0) {
      return [];
    }
    const start = Date.now();
    while (Date.now() - start < timeoutMs) {
      const coins = await this.listWalletVtxos();
      const byOutpoint = new Map(coins.map((coin) => [outpointKey(this.toVtxoRef(coin)), coin] as const));
      const resolved = refs.map((ref) => byOutpoint.get(outpointKey(ref)) ?? null);
      if (resolved.every((coin) => coin && isSettledArkadeCoin(coin))) {
        return resolved as any[];
      }
      await new Promise<void>((resolve) => {
        setTimeout(resolve, 500);
      });
    }
    throw new Error(`managed Arkade VTXOs ${refs.map((ref) => outpointKey(ref)).join(", ")} are not spendable yet`);
  }

  private async canReleaseManagedPosition(state: ArkadeTableFundsState, amountSats: number) {
    try {
      const coins = await this.resolveCoins(state.managedVtxos, 2_000);
      const resolvedAmount = coins.reduce(
        (total, coin) => total + Number(coin.value ?? coin.amount ?? 0),
        0,
      );
      return resolvedAmount === amountSats;
    } catch {
      return false;
    }
  }

  private async saveState(state: ArkadeTableFundsState) {
    await this.store.save(state.tableId, state);
  }

  private async withTableLock<T>(tableId: string, operation: () => Promise<T>): Promise<T> {
    const previous = this.tableOperationLocks.get(tableId) ?? Promise.resolve();
    let release: (() => void) | undefined;
    const current = new Promise<void>((resolve) => {
      release = resolve;
    });
    this.tableOperationLocks.set(tableId, previous.catch(() => undefined).then(() => current));
    await previous.catch(() => undefined);
    try {
      return await operation();
    } finally {
      release?.();
      if (this.tableOperationLocks.get(tableId) === current) {
        this.tableOperationLocks.delete(tableId);
      }
    }
  }

  private selectVtxos(vtxos: ArkadeVtxoRef[], targetAmountSats: number) {
    const sorted = [...vtxos].sort((left, right) => left.amountSats - right.amountSats);
    const selection: ArkadeVtxoRef[] = [];
    let total = 0;
    for (const vtxo of sorted) {
      selection.push(vtxo);
      total += vtxo.amountSats;
      if (total >= targetAmountSats) {
        return selection;
      }
    }
    return selection;
  }

  private toVtxoRef(coin: any): ArkadeVtxoRef {
    return {
      amountSats: Number(coin.value ?? coin.amount ?? 0),
      ...(normalizeBatchExpiry(coin.virtualStatus?.batchExpiry)
        ? { expiresAt: normalizeBatchExpiry(coin.virtualStatus?.batchExpiry) }
        : {}),
      txid: coin.txid,
      vout: coin.vout,
    };
  }

  private async waitForExactNewVtxo(beforeKeys: Set<string>, amountSats: number, timeoutMs = 60_000) {
    const start = Date.now();
    while (Date.now() - start < timeoutMs) {
      const coins = await this.listWalletVtxos();
      const candidate = coins
        .filter((coin) => isSettledArkadeCoin(coin))
        .map((coin) => this.toVtxoRef(coin))
        .find((coin) => !beforeKeys.has(outpointKey(coin)) && coin.amountSats === amountSats);
      if (candidate) {
        return candidate;
      }
      await new Promise<void>((resolve) => {
        setTimeout(resolve, 500);
      });
    }
    throw new Error(`timed out waiting for a new Arkade VTXO worth exactly ${amountSats} sats`);
  }

  private async waitForIncomingTransfers(
    beforeKeys: Set<string>,
    expectedAmountSats: number,
    timeoutMs = 60_000,
  ) {
    const start = Date.now();
    while (Date.now() - start < timeoutMs) {
      const refs = (await this.listWalletVtxos())
        .filter((coin) => isSettledArkadeCoin(coin))
        .map((coin) => this.toVtxoRef(coin))
        .filter((coin) => !beforeKeys.has(outpointKey(coin)))
        .sort((left, right) => left.amountSats - right.amountSats);
      let running = 0;
      const selected: ArkadeVtxoRef[] = [];
      for (const ref of refs) {
        selected.push(ref);
        running += ref.amountSats;
        if (running >= expectedAmountSats) {
          return selected;
        }
      }
      await new Promise<void>((resolve) => {
        setTimeout(resolve, 500);
      });
    }
    throw new Error(`timed out waiting for incoming Arkade transfers worth ${expectedAmountSats} sats`);
  }
}

export function createMockTableFundsProvider(args: {
  signer: LocalIdentity;
  networkId: string;
  expiryMs?: number;
}): TableFundsProvider {
  return new SignedTableFundsProvider(
    args.signer,
    args.networkId,
    "mock-table-funds/v1",
    args.expiryMs ?? 15 * 60_000,
  );
}

export function createArkadeTableFundsProvider(args: {
  arkServerUrl?: string;
  arkadeNetworkName?: "mutinynet" | "regtest";
  boltzApiUrl?: string;
  signer: LocalIdentity;
  networkId: Network;
  expiryMs?: number;
  stateStore?: TableFundsStateStore<ArkadeTableFundsState>;
}): TableFundsProvider {
  return new ArkadeTableFundsProvider(
    args.signer,
    {
      network: args.networkId,
      ...(args.arkServerUrl ? { arkServerUrl: args.arkServerUrl } : {}),
      ...(args.arkadeNetworkName ? { arkadeNetworkName: args.arkadeNetworkName } : {}),
      ...(args.boltzApiUrl ? { boltzApiUrl: args.boltzApiUrl } : {}),
    },
    "arkade-table-funds/v1",
    args.expiryMs ?? 45 * 60_000,
    args.stateStore,
  );
}
