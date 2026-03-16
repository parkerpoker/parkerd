import { resolveCliRuntimeConfig, type CliFlagMap } from "./config.js";
import { ProfileDaemon } from "./daemonProcess.js";

function parseFlags(argv: string[]) {
  const flags: CliFlagMap = {};
  for (let index = 0; index < argv.length; index += 1) {
    const value = argv[index]!;
    if (!value.startsWith("--")) {
      continue;
    }
    const keyValue = value.slice(2).split("=");
    const key = keyValue[0]!;
    const inlineValue = keyValue[1];
    if (inlineValue !== undefined) {
      flags[key] = inlineValue;
      continue;
    }
    const next = argv[index + 1];
    if (!next || next.startsWith("--")) {
      flags[key] = true;
      continue;
    }
    flags[key] = next;
    index += 1;
  }
  return flags;
}

async function main(argv: string[]) {
  const flags = parseFlags(argv);
  const profileValue = flags.profile;
  if (typeof profileValue !== "string" || !profileValue) {
    throw new Error("--profile is required for daemon startup");
  }
  const config = resolveCliRuntimeConfig(flags);
  const daemon = new ProfileDaemon(profileValue, config);
  await daemon.start();
}

void main(process.argv.slice(2)).catch((error) => {
  process.stderr.write(`${(error as Error).message}\n`);
  process.exitCode = 1;
});
