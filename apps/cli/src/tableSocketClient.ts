import WebSocket from "ws";

import {
  parseClientSocketEvent,
  parseServerSocketEvent,
  type ClientSocketEvent,
  type ServerSocketEvent,
} from "@parker/protocol";

export interface TableSocketClientOptions {
  onEvent: (event: ServerSocketEvent) => void | Promise<void>;
  playerId: string;
  tableId: string;
  wsUrl: string;
}

export class TableSocketClient {
  private readonly socket: WebSocket;
  private heartbeatTimer: NodeJS.Timeout | undefined;

  private constructor(
    private readonly options: TableSocketClientOptions,
    socket: WebSocket,
  ) {
    this.socket = socket;
  }

  static async connect(options: TableSocketClientOptions) {
    const socket = new WebSocket(options.wsUrl);
    await new Promise<void>((resolve, reject) => {
      socket.once("open", () => resolve());
      socket.once("error", reject);
    });

    const client = new TableSocketClient(options, socket);
    client.bind();
    client.send({
      type: "identify",
      tableId: options.tableId,
      playerId: options.playerId,
    });
    return client;
  }

  close() {
    if (this.heartbeatTimer) {
      clearInterval(this.heartbeatTimer);
      this.heartbeatTimer = undefined;
    }
    this.socket.close();
  }

  send(event: ClientSocketEvent) {
    const parsed = parseClientSocketEvent(event);
    this.socket.send(JSON.stringify(parsed));
  }

  private bind() {
    this.socket.on("message", (raw: WebSocket.RawData) => {
      const event = parseServerSocketEvent(JSON.parse(raw.toString()));
      void this.options.onEvent(event);
    });
    this.heartbeatTimer = setInterval(() => {
      this.send({
        type: "heartbeat",
        tableId: this.options.tableId,
        playerId: this.options.playerId,
        sentAt: new Date().toISOString(),
      });
    }, 10_000);
  }
}
