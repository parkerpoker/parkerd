import type { DaemonRuntimeState } from "@parker/daemon-runtime";

import { localControllerHeaders } from "./localControllerApi.js";

const LOCAL_CONTROLLER_BASE = import.meta.env.VITE_LOCAL_CONTROLLER_URL ?? "";

export interface LocalControllerStreamHandlers {
  onLog?: (payload: {
    data?: unknown;
    level: "error" | "info" | "result";
    message?: string;
    scope?: string;
  }) => void;
  onOpen?: () => void;
  onState?: (payload: DaemonRuntimeState) => void;
}

export interface LocalControllerStreamSubscription {
  close: () => void;
  done: Promise<void>;
}

export function subscribeToLocalController(
  profile: string,
  handlers: LocalControllerStreamHandlers,
): LocalControllerStreamSubscription {
  const abortController = new AbortController();

  const done = (async () => {
    const response = await fetch(`${LOCAL_CONTROLLER_BASE}/api/local/profiles/${profile}/watch`, {
      headers: localControllerHeaders(),
      signal: abortController.signal,
    });

    if (!response.ok) {
      throw new Error(await readErrorMessage(response));
    }

    if (!response.body) {
      throw new Error("controller stream body is unavailable");
    }

    handlers.onOpen?.();

    const reader = response.body.getReader();
    const decoder = new TextDecoder();
    let buffer = "";

    for (;;) {
      const { done: readerDone, value } = await reader.read();
      if (readerDone) {
        return;
      }

      buffer += decoder.decode(value, { stream: true });
      for (;;) {
        const separatorIndex = buffer.indexOf("\n\n");
        if (separatorIndex === -1) {
          break;
        }
        const rawEvent = buffer.slice(0, separatorIndex);
        buffer = buffer.slice(separatorIndex + 2);
        dispatchEvent(rawEvent, handlers);
      }
    }
  })();

  return {
    close: () => {
      abortController.abort();
    },
    done,
  };
}

function dispatchEvent(rawEvent: string, handlers: LocalControllerStreamHandlers) {
  const lines = rawEvent
    .split("\n")
    .map((line) => line.trimEnd())
    .filter((line) => line.length > 0 && !line.startsWith(":"));
  if (lines.length === 0) {
    return;
  }

  let eventType = "message";
  const dataLines: string[] = [];

  for (const line of lines) {
    if (line.startsWith("event:")) {
      eventType = line.slice("event:".length).trim();
      continue;
    }
    if (line.startsWith("data:")) {
      dataLines.push(line.slice("data:".length).trim());
    }
  }

  if (dataLines.length === 0) {
    return;
  }

  const payload = JSON.parse(dataLines.join("\n")) as unknown;
  if (eventType === "state") {
    handlers.onState?.(payload as DaemonRuntimeState);
    return;
  }

  if (eventType === "log") {
    handlers.onLog?.(
      payload as {
        data?: unknown;
        level: "error" | "info" | "result";
        message?: string;
        scope?: string;
      },
    );
  }
}

async function readErrorMessage(response: Response) {
  try {
    const payload = (await response.json()) as { error?: string; message?: string };
    return payload.error ?? payload.message ?? `controller stream failed with ${response.status}`;
  } catch {
    return (await response.text()) || `controller stream failed with ${response.status}`;
  }
}
