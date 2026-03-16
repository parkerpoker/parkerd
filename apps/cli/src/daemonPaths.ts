import { join } from "node:path";

export interface ProfileDaemonPaths {
  logPath: string;
  metadataPath: string;
  socketPath: string;
}

export function buildProfileDaemonPaths(daemonDir: string, profileName: string): ProfileDaemonPaths {
  const slug = profileName.replace(/[^a-zA-Z0-9_-]/g, "_");
  return {
    logPath: join(daemonDir, `${slug}.log`),
    metadataPath: join(daemonDir, `${slug}.json`),
    socketPath: join(daemonDir, `${slug}.sock`),
  };
}
