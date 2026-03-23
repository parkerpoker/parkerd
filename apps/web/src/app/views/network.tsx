import type { LocalProfileSummary } from '../../types/parker.js';
import { ChevronDown } from 'lucide-react';

import type { LocalProfileStatusResponse } from '../../lib/localControllerApi.js';

import { Button } from '../components/button.js';
import { InputField } from '../components/input-field.js';
import { Panel } from '../components/panel.js';
import { StatusBadge } from '../components/status-badge.js';

interface NetworkViewProps {
  profiles: LocalProfileSummary[];
  selectedProfile: string | null;
  onSelectProfile: (profile: string | null) => void;
  profileStatus: LocalProfileStatusResponse | null;
  controllerAvailable: boolean | null;
  controllerConnected: boolean;
  nickname: string;
  onNicknameChange: (value: string) => void;
  peerEndpoint: string;
  onPeerEndpointChange: (value: string) => void;
  peerAlias: string;
  onPeerAliasChange: (value: string) => void;
  peers: Array<{ peerId: string; alias?: string | undefined; endpoint?: string; peerUrl?: string; roles: string[] }>;
  busyAction: string | null;
  onAction: (label: string, action: () => Promise<unknown>) => void;
  notice: string | null;
  resultPayload: string | null;
  latestSnapshot: { snapshotId?: string } | null | undefined;
  latestCheckpoint: { snapshotId?: string; signatures: unknown[]; createdAt?: string } | null | undefined;
  selectedLocalTableId: string | null;
  onStartDaemon: () => void;
  onStopDaemon: () => void;
  onBootstrapIdentity: () => void;
  onAddPeer: () => void;
  onRenew: () => void;
  onCashOut: () => void;
  onEmergencyExit: () => void;
}

function formatTimestamp(value?: string | null) {
  if (!value) return 'n/a';
  return new Date(value).toLocaleString();
}

export function NetworkView({
  profiles,
  selectedProfile,
  onSelectProfile,
  profileStatus,
  controllerAvailable,
  controllerConnected,
  nickname,
  onNicknameChange,
  peerEndpoint,
  onPeerEndpointChange,
  peerAlias,
  onPeerAliasChange,
  peers,
  busyAction,
  notice,
  resultPayload,
  latestSnapshot,
  latestCheckpoint,
  selectedLocalTableId,
  onStartDaemon,
  onStopDaemon,
  onBootstrapIdentity,
  onAddPeer,
  onRenew,
  onCashOut,
  onEmergencyExit,
}: NetworkViewProps) {
  const localMesh = profileStatus?.daemon.state?.mesh;
  const localTransport = profileStatus?.daemon.state?.transport;
  const daemonRunning = profileStatus?.daemon.reachable === true;

  return (
    <div className="grid grid-cols-1 lg:grid-cols-3 gap-6">
      {/* Profile / Daemon */}
      <Panel title="Profile / Daemon">
        <div className="space-y-4">
          <div className="space-y-1.5">
            <label className="text-xs text-muted-foreground uppercase tracking-wide">Profile</label>
            <div className="relative">
              <select
                className="w-full rounded-md border border-border bg-input px-3 py-2 pr-8 text-sm text-foreground focus:border-primary focus:outline-none focus:ring-1 focus:ring-primary appearance-none"
                disabled={!controllerAvailable || profiles.length === 0}
                value={selectedProfile ?? ''}
                onChange={(e) => onSelectProfile(e.target.value || null)}
              >
                <option value="">Choose a local profile</option>
                {profiles.map((p) => (
                  <option key={p.profileName} value={p.profileName}>
                    {p.nickname} ({p.profileName})
                  </option>
                ))}
              </select>
              <ChevronDown className="absolute right-3 top-1/2 -translate-y-1/2 h-4 w-4 text-muted-foreground pointer-events-none" />
            </div>
          </div>

          <InputField
            label="Bootstrap nickname"
            value={nickname}
            onChange={(e) => onNicknameChange(e.target.value)}
            placeholder={selectedProfile ?? 'nickname'}
          />

          <div className="space-y-2">
            <label className="text-xs text-muted-foreground uppercase tracking-wide">Status</label>
            <div className="flex flex-wrap gap-2">
              <StatusBadge status={daemonRunning ? 'running' : 'stopped'} label={`Daemon: ${daemonRunning ? 'running' : 'stopped'}`} />
              <StatusBadge status={controllerConnected ? 'connected' : 'disconnected'} label={`Stream: ${controllerConnected ? 'connected' : 'disconnected'}`} />
            </div>
          </div>

          <div className="grid grid-cols-2 gap-2">
            <div className="rounded-md border border-border bg-secondary/50 p-2 text-xs">
              <div className="text-muted-foreground">Mode</div>
              <div className="text-foreground">{profileStatus?.daemon.metadata?.mode ?? 'player'}</div>
            </div>
            <div className="rounded-md border border-border bg-secondary/50 p-2 text-xs">
              <div className="text-muted-foreground">Peer ID</div>
              <div className="text-foreground truncate">{localTransport?.peer.peerId ?? localMesh?.peer.peerId ?? 'n/a'}</div>
            </div>
          </div>

          <div className="flex gap-2">
            <Button variant="primary" onClick={onStartDaemon} disabled={!selectedProfile || busyAction !== null} className="flex-1">
              Start Daemon
            </Button>
            <Button variant="secondary" onClick={onStopDaemon} disabled={!selectedProfile || busyAction !== null} className="flex-1">
              Stop Daemon
            </Button>
          </div>
          <Button variant="secondary" onClick={onBootstrapIdentity} disabled={!selectedProfile || busyAction !== null} className="w-full">
            Bootstrap Identity
          </Button>

          {notice && <p className="rounded-md border border-primary/30 bg-primary/5 p-3 text-xs text-muted-foreground">{notice}</p>}
          {resultPayload && (
            <pre className="rounded-md border border-border bg-black/40 p-3 max-h-[200px] overflow-auto font-mono text-xs text-foreground">{resultPayload}</pre>
          )}
        </div>
      </Panel>

      {/* Peers */}
      <Panel title="Network">
        <div className="space-y-4">
          <div className="flex items-center gap-2 text-sm text-muted-foreground">
            <span>Connected Peers:</span>
            <span className="text-foreground">{peers.length}</span>
          </div>

          <InputField label="Bootstrap Endpoint" value={peerEndpoint} onChange={(e) => onPeerEndpointChange(e.target.value)} placeholder="tor://peer.onion:9735" />
          <InputField label="Alias (optional)" value={peerAlias} onChange={(e) => onPeerAliasChange(e.target.value)} placeholder="Peer alias" />
          <Button variant="primary" onClick={onAddPeer} disabled={!selectedProfile || busyAction !== null} className="w-full">
            Add Peer
          </Button>

          {peers.length > 0 ? (
            <div className="space-y-2 max-h-[200px] overflow-y-auto">
              {peers.map((peer) => (
                <div key={peer.peerId} className="rounded-md border border-border bg-secondary/50 p-2 text-xs">
                  <div className="text-foreground truncate">{peer.alias ?? peer.peerId}</div>
                  <div className="text-muted-foreground truncate">{peer.endpoint ?? peer.peerUrl ?? 'n/a'}</div>
                  <div className="text-muted-foreground">{peer.roles.join(', ') || 'no roles'}</div>
                </div>
              ))}
            </div>
          ) : (
            <div className="rounded-md border border-dashed border-border p-4 text-center text-sm text-muted-foreground">
              No peers connected
            </div>
          )}
        </div>
      </Panel>

      {/* Settlement */}
      <Panel title="Settlement">
        <div className="space-y-4">
          <div className="grid grid-cols-2 gap-3">
            <div className="rounded-md border border-border bg-secondary/50 p-3">
              <div className="text-xs text-muted-foreground uppercase tracking-wide">Latest Snapshot</div>
              <div className="mt-1 text-sm text-foreground font-mono">{latestSnapshot?.snapshotId ?? 'n/a'}</div>
            </div>
            <div className="rounded-md border border-border bg-secondary/50 p-3">
              <div className="text-xs text-muted-foreground uppercase tracking-wide">Latest Checkpoint</div>
              <div className="mt-1 text-sm text-foreground font-mono">{latestCheckpoint?.snapshotId ?? 'n/a'}</div>
            </div>
            <div className="rounded-md border border-border bg-secondary/50 p-3">
              <div className="text-xs text-muted-foreground uppercase tracking-wide">Signatures</div>
              <div className="mt-1 text-sm text-foreground">{latestCheckpoint?.signatures.length ?? 0} peer(s)</div>
            </div>
            <div className="rounded-md border border-border bg-secondary/50 p-3">
              <div className="text-xs text-muted-foreground uppercase tracking-wide">Updated</div>
              <div className="mt-1 text-sm text-foreground">{formatTimestamp(latestCheckpoint?.createdAt)}</div>
            </div>
          </div>

          <Button variant="secondary" onClick={onRenew} disabled={!selectedProfile || !selectedLocalTableId || busyAction !== null} className="w-full">
            Renew Session
          </Button>
          <Button variant="primary" onClick={onCashOut} disabled={!selectedProfile || !selectedLocalTableId || busyAction !== null} className="w-full">
            Cash Out
          </Button>
          <Button variant="danger" onClick={onEmergencyExit} disabled={!selectedProfile || !selectedLocalTableId || busyAction !== null} className="w-full">
            Emergency Exit
          </Button>

          <div className="rounded-md border border-primary/30 bg-primary/5 p-3 text-xs text-muted-foreground">
            Settlement operations are cryptographically secured. Emergency exit available for uncooperative scenarios.
          </div>
        </div>
      </Panel>
    </div>
  );
}
