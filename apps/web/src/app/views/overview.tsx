import { Server, Users, Wallet, Gamepad2 } from 'lucide-react';

import type { LocalControllerLogEvent, LocalProfileStatusResponse } from '../../lib/localControllerApi.js';

import { Panel } from '../components/panel.js';
import { StatusBadge } from '../components/status-badge.js';

interface OverviewViewProps {
  profileStatus: LocalProfileStatusResponse | null;
  controllerConnected: boolean;
  spectatorModeOnly: boolean;
  publicTableCount: number;
  localTableCount: number;
  peerCount: number;
  availableSats: number;
  localLogs: LocalControllerLogEvent[];
}

export function OverviewView({
  profileStatus,
  controllerConnected,
  spectatorModeOnly,
  publicTableCount,
  localTableCount,
  peerCount,
  availableSats,
  localLogs,
}: OverviewViewProps) {
  const daemonRunning = profileStatus?.daemon.reachable === true;

  return (
    <div className="space-y-6">
      {/* Status Grid */}
      <div className="grid grid-cols-2 md:grid-cols-4 gap-4">
        <Panel>
          <div className="flex items-start justify-between">
            <div>
              <div className="text-xs text-muted-foreground uppercase tracking-wide">Daemon</div>
              <div className="mt-2">
                <StatusBadge status={daemonRunning ? 'running' : 'stopped'} />
              </div>
            </div>
            <Server className="h-5 w-5 text-primary" />
          </div>
        </Panel>

        <Panel>
          <div className="flex items-start justify-between">
            <div>
              <div className="text-xs text-muted-foreground uppercase tracking-wide">Network</div>
              <div className="mt-2 text-lg text-foreground">{peerCount} Peer{peerCount !== 1 ? 's' : ''}</div>
            </div>
            <Users className="h-5 w-5 text-primary" />
          </div>
        </Panel>

        <Panel>
          <div className="flex items-start justify-between">
            <div>
              <div className="text-xs text-muted-foreground uppercase tracking-wide">Balance</div>
              <div className="mt-2 text-lg text-primary">{availableSats.toLocaleString()} sat</div>
            </div>
            <Wallet className="h-5 w-5 text-primary" />
          </div>
        </Panel>

        <Panel>
          <div className="flex items-start justify-between">
            <div>
              <div className="text-xs text-muted-foreground uppercase tracking-wide">Tables</div>
              <div className="mt-2 text-lg text-foreground">{localTableCount} local / {publicTableCount} public</div>
            </div>
            <Gamepad2 className="h-5 w-5 text-primary" />
          </div>
        </Panel>
      </div>

      {/* System Status */}
      <div className="grid grid-cols-1 md:grid-cols-2 gap-6">
        <Panel title="System Status">
          <div className="space-y-4">
            <div className="flex items-center justify-between">
              <span className="text-sm text-muted-foreground">Daemon</span>
              <StatusBadge status={daemonRunning ? 'running' : 'stopped'} />
            </div>
            <div className="flex items-center justify-between">
              <span className="text-sm text-muted-foreground">SSE Stream</span>
              <StatusBadge status={controllerConnected ? 'connected' : 'disconnected'} />
            </div>
            <div className="flex items-center justify-between">
              <span className="text-sm text-muted-foreground">Controller</span>
              <StatusBadge status={spectatorModeOnly ? 'disconnected' : 'connected'} label={spectatorModeOnly ? 'Spectator only' : 'Active'} />
            </div>
            <div className="flex items-center justify-between">
              <span className="text-sm text-muted-foreground">Mode</span>
              <span className="text-sm text-foreground">{profileStatus?.daemon.metadata?.mode ?? 'player'}</span>
            </div>
          </div>
        </Panel>

        <Panel title="Recent Events">
          <div className="space-y-2 max-h-[220px] overflow-y-auto font-mono text-xs">
            {localLogs.length === 0 && (
              <div className="text-sm text-muted-foreground">No events yet.</div>
            )}
            {localLogs.slice(0, 8).map((entry, index) => (
              <div key={`${entry.level}-${index}`} className="flex gap-3">
                <span className={
                  entry.level === 'error' ? 'text-[#e74c3c]' :
                  entry.level === 'result' ? 'text-[#2ecc71]' :
                  'text-[#3498db]'
                }>
                  [{entry.level.toUpperCase()}]
                </span>
                <span className="text-foreground truncate">{entry.message ?? 'result payload'}</span>
              </div>
            ))}
          </div>
        </Panel>
      </div>
    </div>
  );
}
