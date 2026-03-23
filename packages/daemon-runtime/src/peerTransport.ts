import type { AddressInfo } from "node:net";

import WebSocket, { WebSocketServer } from "ws";

import {
  parseMeshWireFrame,
  type MeshRequestBody,
  type MeshResponseBody,
  type MeshWireFrame,
  type PeerAddress,
} from "@parker/protocol";
import {
  signStructuredData,
  verifyStructuredData,
  type ScopedIdentity,
} from "@parker/settlement";

import type { MeshRuntimeMode } from "./meshTypes.js";

interface PendingRequest {
  reject: (error: Error) => void;
  resolve: (body: MeshResponseBody | undefined) => void;
  timeout: NodeJS.Timeout;
}

interface SocketState {
  peerId?: string;
  socket: WebSocket;
}

export interface PeerTransportOptions {
  alias: string;
  onFrame: (peerId: string, frame: MeshWireFrame) => void | Promise<void>;
  onPeerSeen?: (peer: PeerAddress) => void;
  peerIdentity: ScopedIdentity;
  protocolIdentity: ScopedIdentity;
  roles: MeshRuntimeMode[];
}

export class PeerTransport {
  private readonly confirmedPeerIds = new Set<string>();
  private readonly confirmedPeerWaiters = new Map<
    string,
    Array<{
      resolve: (peer: PeerAddress) => void;
      timeout: NodeJS.Timeout;
    }>
  >();
  private readonly knownPeers = new Map<string, PeerAddress>();
  private listenUrl = "";
  private readonly pendingRequests = new Map<string, PendingRequest>();
  private server: WebSocketServer | undefined;
  private readonly sockets = new Map<string, SocketState>();

  constructor(private readonly options: PeerTransportOptions) {}

  async start(host: string, port: number) {
    this.server = new WebSocketServer({
      host,
      path: "/mesh",
      port,
    });

    this.server.on("connection", (socket) => {
      this.bindSocket({ socket });
    });

    await new Promise<void>((resolve, reject) => {
      this.server?.once("listening", () => resolve());
      this.server?.once("error", reject);
    });

    const address = this.server.address();
    if (!address || typeof address === "string") {
      throw new Error("mesh transport failed to bind");
    }
    this.listenUrl = `ws://${host}:${(address as AddressInfo).port}/mesh`;
    return this.listenUrl;
  }

  close() {
    for (const pending of this.pendingRequests.values()) {
      clearTimeout(pending.timeout);
      pending.reject(new Error("peer transport closed"));
    }
    this.pendingRequests.clear();
    for (const state of this.sockets.values()) {
      state.socket.close();
    }
    this.sockets.clear();
    this.server?.close();
    this.server = undefined;
  }

  registerKnownPeer(peer: PeerAddress) {
    this.knownPeers.set(peer.peerId, peer);
  }

  listKnownPeers() {
    return [...this.knownPeers.values()];
  }

  getListenUrl() {
    return this.listenUrl;
  }

  async waitForConfirmedPeer(peerUrl: string, timeoutMs: number) {
    const existing = [...this.knownPeers.values()].find(
      (candidate) => candidate.peerUrl === peerUrl && this.confirmedPeerIds.has(candidate.peerId),
    );
    if (existing) {
      return existing;
    }

    return await new Promise<PeerAddress>((resolve, reject) => {
      const timeout = setTimeout(() => {
        const waiters = this.confirmedPeerWaiters.get(peerUrl) ?? [];
        this.confirmedPeerWaiters.set(
          peerUrl,
          waiters.filter((waiter) => waiter.resolve !== resolve),
        );
        reject(new Error(`timed out waiting for confirmed peer ${peerUrl}`));
      }, timeoutMs);

      const waiters = this.confirmedPeerWaiters.get(peerUrl) ?? [];
      waiters.push({ resolve, timeout });
      this.confirmedPeerWaiters.set(peerUrl, waiters);
    });
  }

  async request(
    peerId: string,
    body: MeshRequestBody,
    timeoutMs = 5_000,
  ): Promise<MeshResponseBody | undefined> {
    const requestId = crypto.randomUUID();
    const socket = await this.ensureConnection(peerId);
    const result = await new Promise<MeshResponseBody | undefined>((resolve, reject) => {
      const timeout = setTimeout(() => {
        this.pendingRequests.delete(requestId);
        reject(new Error(`timed out waiting for peer ${peerId}`));
      }, timeoutMs);

      this.pendingRequests.set(requestId, {
        reject,
        resolve,
        timeout,
      });
      socket.send(
        JSON.stringify({
          kind: "request",
          requestId,
          body,
        } satisfies MeshWireFrame),
      );
    });
    return result;
  }

  async send(peerId: string, frame: MeshWireFrame, relayPeerId?: string) {
    try {
      const socket = await this.ensureConnection(peerId);
      socket.send(JSON.stringify(frame));
    } catch (error) {
      if (!relayPeerId) {
        throw error;
      }
      const relaySocket = await this.ensureConnection(relayPeerId);
      relaySocket.send(
        JSON.stringify({
          kind: "relay-forward",
          targetPeerId: peerId,
          frameJson: JSON.stringify(frame),
        } satisfies MeshWireFrame),
      );
    }
  }

  async broadcast(peerIds: string[], frame: MeshWireFrame) {
    await Promise.allSettled(
      peerIds.map(async (peerId) => {
        const relayPeerId = this.knownPeers.get(peerId)?.relayPeerId;
        await this.send(peerId, frame, relayPeerId);
      }),
    );
  }

  private bindSocket(state: SocketState) {
    state.socket.on("message", (raw: WebSocket.RawData) => {
      void this.handleRawMessage(state, raw.toString());
    });

    state.socket.on("close", () => {
      if (state.peerId) {
        this.sockets.delete(state.peerId);
      }
    });

    state.socket.on("error", () => {
      if (state.peerId) {
        this.sockets.delete(state.peerId);
      }
    });

    void this.sendHello(state.socket);
  }

  private async connect(peer: PeerAddress) {
    const existing = this.sockets.get(peer.peerId);
    if (existing && existing.socket.readyState === WebSocket.OPEN) {
      return existing.socket;
    }
    const socket = new WebSocket(peer.peerUrl);
    await new Promise<void>((resolve, reject) => {
      socket.once("open", () => resolve());
      socket.once("error", reject);
    });
    const state: SocketState = {
      peerId: peer.peerId,
      socket,
    };
    this.sockets.set(peer.peerId, state);
    this.bindSocket(state);
    return socket;
  }

  private async ensureConnection(peerId: string) {
    const state = this.sockets.get(peerId);
    if (state?.socket.readyState === WebSocket.OPEN) {
      return state.socket;
    }
    const peer = this.knownPeers.get(peerId);
    if (!peer) {
      throw new Error(`unknown peer ${peerId}`);
    }
    return await this.connect(peer);
  }

  private async handleRawMessage(state: SocketState, raw: string) {
    const frame = parseMeshWireFrame(JSON.parse(raw));
    if (frame.kind === "hello") {
      const unsignedHello = {
        alias: frame.alias,
        peerId: frame.peerId,
        peerPubkeyHex: frame.peerPubkeyHex,
        peerUrl: frame.peerUrl,
        protocolId: frame.protocolId,
        protocolPubkeyHex: frame.protocolPubkeyHex,
        roles: frame.roles,
        sentAt: frame.sentAt,
      };
      if (!verifyStructuredData(frame.peerPubkeyHex, unsignedHello, frame.signatureHex)) {
        throw new Error("invalid peer hello signature");
      }
      for (const [knownPeerId, knownPeer] of this.knownPeers.entries()) {
        if (knownPeer.peerUrl === frame.peerUrl && knownPeerId !== frame.peerId) {
          this.confirmedPeerIds.delete(knownPeerId);
          this.knownPeers.delete(knownPeerId);
          this.sockets.delete(knownPeerId);
        }
      }
      state.peerId = frame.peerId;
      this.sockets.set(frame.peerId, state);
      const peer: PeerAddress = {
        alias: frame.alias,
        peerId: frame.peerId,
        peerUrl: frame.peerUrl,
        protocolPubkeyHex: frame.protocolPubkeyHex,
        roles: frame.roles,
        lastSeenAt: frame.sentAt,
      };
      this.confirmedPeerIds.add(frame.peerId);
      this.knownPeers.set(frame.peerId, peer);
      const waiters = this.confirmedPeerWaiters.get(frame.peerUrl) ?? [];
      for (const waiter of waiters) {
        clearTimeout(waiter.timeout);
        waiter.resolve(peer);
      }
      this.confirmedPeerWaiters.delete(frame.peerUrl);
      this.options.onPeerSeen?.(peer);
      return;
    }

    if (frame.kind === "response") {
      const pending = this.pendingRequests.get(frame.requestId);
      if (!pending) {
        return;
      }
      clearTimeout(pending.timeout);
      this.pendingRequests.delete(frame.requestId);
      if (!frame.ok) {
        pending.reject(new Error(frame.error ?? "peer request failed"));
        return;
      }
      pending.resolve(frame.body);
      return;
    }

    if (frame.kind === "relay-forward") {
      const socket = this.sockets.get(frame.targetPeerId)?.socket;
      if (socket?.readyState === WebSocket.OPEN) {
        socket.send(frame.frameJson);
      }
      return;
    }

    if (!state.peerId) {
      throw new Error("received non-hello frame before peer identification");
    }

    await this.options.onFrame(state.peerId, frame);
  }

  private async sendHello(socket: WebSocket) {
    const unsignedHello = {
      alias: this.options.alias,
      peerId: this.options.peerIdentity.id,
      peerPubkeyHex: this.options.peerIdentity.publicKeyHex,
      peerUrl: this.listenUrl,
      protocolId: this.options.protocolIdentity.id,
      protocolPubkeyHex: this.options.protocolIdentity.publicKeyHex,
      roles: this.options.roles,
      sentAt: new Date().toISOString(),
    };
    socket.send(
      JSON.stringify({
        ...unsignedHello,
        kind: "hello",
        signatureHex: signStructuredData(this.options.peerIdentity, unsignedHello),
      } satisfies MeshWireFrame),
    );
  }
}
