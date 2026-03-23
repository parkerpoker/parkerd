import { rm, readFile, writeFile } from "node:fs/promises";

import type { ProfileDaemonMetadata } from "./daemonProtocol.js";
import type { ProfileDaemonPaths } from "./daemonPaths.js";

export async function cleanupProfileDaemonArtifacts(paths: ProfileDaemonPaths) {
  await Promise.all([
    rm(paths.socketPath, { force: true }),
    rm(paths.metadataPath, { force: true }),
  ]);
}

export function isPidAlive(pid: number) {
  try {
    process.kill(pid, 0);
    return true;
  } catch (error) {
    const code = (error as NodeJS.ErrnoException).code;
    return code !== "ESRCH";
  }
}

export async function readProfileDaemonMetadata(paths: ProfileDaemonPaths): Promise<ProfileDaemonMetadata | null> {
  try {
    return JSON.parse(await readFile(paths.metadataPath, "utf8")) as ProfileDaemonMetadata;
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") {
      return null;
    }
    throw error;
  }
}

export async function writeProfileDaemonMetadata(
  paths: ProfileDaemonPaths,
  metadata: ProfileDaemonMetadata,
) {
  await writeFile(paths.metadataPath, `${JSON.stringify(metadata, null, 2)}\n`, "utf8");
}
