import { useEffect, useRef, useState } from "react";

import {
  parseServerSocketEvent,
  type ClientSocketEvent,
  type ServerSocketEvent,
} from "@parker/protocol";

interface UseTableSocketArgs {
  wsUrl: string;
  tableId: string | undefined;
  playerId: string | undefined;
  onEvent: (event: ServerSocketEvent) => void;
}

export function useTableSocket({ wsUrl, tableId, playerId, onEvent }: UseTableSocketArgs) {
  const socketRef = useRef<WebSocket | null>(null);
  const onEventRef = useRef(onEvent);
  const [connected, setConnected] = useState(false);

  onEventRef.current = onEvent;

  useEffect(() => {
    if (!tableId || !playerId) {
      return;
    }

    const socket = new WebSocket(wsUrl);
    socketRef.current = socket;

    socket.addEventListener("open", () => {
      setConnected(true);
      socket.send(
        JSON.stringify({
          type: "identify",
          tableId,
          playerId,
        } satisfies ClientSocketEvent),
      );
    });

    socket.addEventListener("message", (event) => {
      const parsed = parseServerSocketEvent(JSON.parse(event.data));
      onEventRef.current(parsed);
    });

    socket.addEventListener("close", () => {
      setConnected(false);
    });

    const interval = window.setInterval(() => {
      socket.send(
        JSON.stringify({
          type: "heartbeat",
          tableId,
          playerId,
          sentAt: new Date().toISOString(),
        } satisfies ClientSocketEvent),
      );
    }, 10_000);

    return () => {
      window.clearInterval(interval);
      socket.close();
      socketRef.current = null;
    };
  }, [playerId, tableId, wsUrl]);

  function sendEvent(event: ClientSocketEvent) {
    socketRef.current?.send(JSON.stringify(event));
  }

  return {
    connected,
    sendEvent,
  };
}
