export interface LogEnvelope {
  data?: unknown;
  level: "error" | "info" | "result";
  message?: string;
  scope?: string;
}

export interface CliLoggerOptions {
  muteOutput?: boolean;
  sink?: (payload: LogEnvelope) => void;
}

export class CliLogger {
  constructor(
    private readonly json: boolean,
    private readonly scope?: string,
    private readonly options: CliLoggerOptions = {},
  ) {}

  info(message: string, data?: unknown) {
    this.emit("info", message, data);
  }

  error(message: string, data?: unknown) {
    this.emit("error", message, data);
  }

  result(data: unknown) {
    const payload: LogEnvelope =
      this.scope === undefined ? { data, level: "result" } : { data, level: "result", scope: this.scope };
    this.options.sink?.(payload);
    if (this.options.muteOutput) {
      return;
    }
    if (this.json) {
      process.stdout.write(`${JSON.stringify(payload)}\n`);
      return;
    }

    process.stdout.write(`${formatValue(data)}\n`);
  }

  private emit(level: "info" | "error", message: string, data?: unknown) {
    const payload =
      data === undefined
        ? this.scope === undefined
          ? { level, message }
          : { level, message, scope: this.scope }
        : this.scope === undefined
          ? { data, level, message }
          : { data, level, message, scope: this.scope };
    this.options.sink?.(payload);
    if (this.options.muteOutput) {
      return;
    }
    if (this.json) {
      process.stdout.write(`${JSON.stringify(payload)}\n`);
      return;
    }

    const prefix = this.scope ? `[${this.scope}] ` : "";
    const suffix = data === undefined ? "" : ` ${formatValue(data)}`;
    const line = `${prefix}${message}${suffix}\n`;
    if (level === "error") {
      process.stderr.write(line);
      return;
    }
    process.stdout.write(line);
  }
}

function formatValue(value: unknown): string {
  if (typeof value === "string") {
    return value;
  }
  return JSON.stringify(value, null, 2);
}
