import type { LogEnvelope } from "./logger.js";
import { type DaemonEventEnvelope, type DaemonRuntimeState } from "./daemonProtocol.js";
import { DaemonRpcClient } from "./daemonClient.js";

export interface DaemonWatchBridgeHandlers {
  onEvent?: (event: DaemonEventEnvelope) => void;
  onLog?: (payload: LogEnvelope) => void;
  onState?: (payload: DaemonRuntimeState) => void;
}

export async function bridgeDaemonWatch(
  client: DaemonRpcClient,
  handlers: DaemonWatchBridgeHandlers,
) {
  const stopWatching = await client.watch((event) => {
    handlers.onEvent?.(event);
    if (event.event === "state") {
      handlers.onState?.(event.payload as DaemonRuntimeState);
      return;
    }
    handlers.onLog?.(event.payload as LogEnvelope);
  });

  handlers.onState?.(client.currentState());
  return stopWatching;
}
