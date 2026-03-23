import type { MeshTableView } from '@parker/daemon-runtime';
import type { PublicTableView } from '@parker/protocol';
import { Eye, Users, Coins } from 'lucide-react';

import { PokerCard } from '../../components/PokerCard.js';

import { Button } from '../components/button.js';
import { InputField } from '../components/input-field.js';
import { Panel } from '../components/panel.js';

interface TablesViewProps {
  selectedProfile: string | null;
  busyAction: string | null;
  localTableSummaries: Array<{ tableId: string; tableName: string; status: string }>;
  selectedLocalTableId: string | null;
  onSelectLocalTable: (tableId: string) => void;
  selectedLocalTable: MeshTableView | null;
  localPublicTables: PublicTableView[];
  publicTables: PublicTableView[];
  selectedPublicTableId: string | null;
  onSelectPublicTable: (tableId: string) => void;
  selectedPublicTable: PublicTableView | null;
  createTableName: string;
  onCreateTableNameChange: (value: string) => void;
  createSmallBlind: string;
  onCreateSmallBlindChange: (value: string) => void;
  createBigBlind: string;
  onCreateBigBlindChange: (value: string) => void;
  createBuyInMin: string;
  onCreateBuyInMinChange: (value: string) => void;
  createBuyInMax: string;
  onCreateBuyInMaxChange: (value: string) => void;
  joinInviteCode: string;
  onJoinInviteCodeChange: (value: string) => void;
  joinBuyIn: string;
  onJoinBuyInChange: (value: string) => void;
  actionTotal: string;
  onActionTotalChange: (value: string) => void;
  onCreate: () => void;
  onJoin: () => void;
  onAnnounce: () => void;
  onRotateHost: () => void;
  onSubmitAction: (actionType: string, totalSats?: number) => void;
}

function formatSats(value?: number | null) {
  return `${value ?? 0} sats`;
}

function formatPhase(value: string | null | undefined) {
  return value ?? 'waiting';
}

function formatTimestamp(value?: string | null) {
  if (!value) return 'n/a';
  return new Date(value).toLocaleString();
}

export function TablesView({
  selectedProfile,
  busyAction,
  localTableSummaries,
  selectedLocalTableId,
  onSelectLocalTable,
  selectedLocalTable,
  localPublicTables,
  publicTables,
  selectedPublicTableId,
  onSelectPublicTable,
  selectedPublicTable,
  createTableName,
  onCreateTableNameChange,
  createSmallBlind,
  onCreateSmallBlindChange,
  createBigBlind,
  onCreateBigBlindChange,
  createBuyInMin,
  onCreateBuyInMinChange,
  createBuyInMax,
  onCreateBuyInMaxChange,
  joinInviteCode,
  onJoinInviteCodeChange,
  joinBuyIn,
  onJoinBuyInChange,
  actionTotal,
  onActionTotalChange,
  onCreate,
  onJoin,
  onAnnounce,
  onRotateHost,
  onSubmitAction,
}: TablesViewProps) {
  const disabled = !selectedProfile || busyAction !== null;
  const selectedLocalPublicState = selectedLocalTable?.publicState;
  const selectedLocalPlayers = selectedLocalPublicState?.seatedPlayers ?? [];
  const localPlayerId = selectedLocalTable?.local.myPlayerId ?? null;
  const localPlayer = selectedLocalPlayers.find((p) => p.playerId === localPlayerId) ?? null;
  const actingPlayer = selectedLocalPlayers.find((p) => p.seatIndex === selectedLocalPublicState?.actingSeatIndex) ?? null;
  const selectedPublicTableUpdates = selectedPublicTable?.recentUpdates ?? [];

  return (
    <div className="space-y-6">
      {/* Gameplay + Table Controls */}
      <div className="grid grid-cols-1 lg:grid-cols-3 gap-6">
        {/* Gameplay */}
        <div className="lg:col-span-2">
          <Panel title="Gameplay">
            <div className="space-y-4">
              {/* Local table selector chips */}
              <div className="flex flex-wrap gap-2">
                {localTableSummaries.length === 0 && (
                  <span className="text-sm text-muted-foreground">No local tables.</span>
                )}
                {localTableSummaries.map((table) => (
                  <button
                    key={table.tableId}
                    onClick={() => onSelectLocalTable(table.tableId)}
                    className={`px-3 py-1.5 text-xs rounded-md border transition-colors ${
                      selectedLocalTableId === table.tableId
                        ? 'border-primary bg-primary/20 text-primary'
                        : 'border-border bg-secondary/50 text-muted-foreground hover:text-foreground'
                    }`}
                  >
                    {table.tableName} ({table.status})
                  </button>
                ))}
              </div>

              {!selectedLocalTable ? (
                <div className="rounded-xl border border-border bg-secondary/30 p-8 text-center">
                  <div className="text-lg text-primary">No Active Game</div>
                  <div className="text-sm text-muted-foreground mt-1">Join or create a table to begin</div>
                </div>
              ) : (
                <>
                  {/* Table summary */}
                  <div className="grid grid-cols-2 md:grid-cols-4 gap-3">
                    <div className="rounded-md border border-border bg-secondary/50 p-2 text-xs">
                      <div className="text-muted-foreground">Table</div>
                      <div className="text-foreground">{selectedLocalTable.config.name}</div>
                    </div>
                    <div className="rounded-md border border-border bg-secondary/50 p-2 text-xs">
                      <div className="text-muted-foreground">Pot</div>
                      <div className="text-foreground">{formatSats(selectedLocalPublicState?.potSats)}</div>
                    </div>
                    <div className="rounded-md border border-border bg-secondary/50 p-2 text-xs">
                      <div className="text-muted-foreground">Acting</div>
                      <div className="text-foreground">{actingPlayer?.nickname ?? 'waiting'}</div>
                    </div>
                    <div className="rounded-md border border-border bg-secondary/50 p-2 text-xs">
                      <div className="text-muted-foreground">You</div>
                      <div className="text-foreground">{localPlayer?.nickname ?? 'spectator'}</div>
                    </div>
                  </div>

                  {/* Board */}
                  <div className="flex gap-2 min-h-[80px] items-center flex-wrap">
                    {(selectedLocalPublicState?.board ?? []).map((card) => (
                      <PokerCard key={card} code={card} />
                    ))}
                  </div>

                  {/* Players */}
                  <div className="grid grid-cols-2 md:grid-cols-4 gap-2">
                    {selectedLocalPlayers.map((player) => (
                      <div
                        key={player.playerId}
                        className={`rounded-md border p-2 text-xs ${
                          player.playerId === localPlayerId
                            ? 'border-primary/40 bg-primary/10'
                            : 'border-border bg-secondary/50'
                        }`}
                      >
                        <div className="text-foreground font-medium">{player.nickname}</div>
                        <div className="text-muted-foreground">Seat {player.seatIndex + 1} &bull; {player.status}</div>
                        <div className="text-foreground">{formatSats(selectedLocalPublicState?.chipBalances[player.playerId])}</div>
                      </div>
                    ))}
                  </div>

                  {/* Hole cards */}
                  <div className="space-y-2">
                    <div className="text-xs text-muted-foreground uppercase tracking-wide">Hole Cards</div>
                    <div className="flex gap-2">
                      {(selectedLocalTable.local.myHoleCards ?? ['XX', 'XX']).map((card, i) => (
                        <PokerCard key={`${card}-${i}`} code={card} concealed={card === 'XX'} />
                      ))}
                    </div>
                  </div>

                  {/* Action composer */}
                  <div className="border-t border-border pt-4 space-y-3">
                    <div className="text-xs text-muted-foreground uppercase tracking-wide">Actions</div>
                    <p className="text-xs text-muted-foreground">
                      {selectedLocalTable.local.canAct
                        ? 'It is your turn.'
                        : 'Not your turn yet.'}
                    </p>
                    <InputField
                      placeholder="bet / raise total sats"
                      value={actionTotal}
                      onChange={(e) => onActionTotalChange(e.target.value)}
                    />
                    <div className="flex flex-wrap gap-2">
                      {selectedLocalTable.local.legalActions.length === 0 && (
                        <span className="text-xs text-muted-foreground">No legal actions available.</span>
                      )}
                      {selectedLocalTable.local.legalActions.map((action) => (
                        <Button
                          key={`${action.type}-${action.minTotalSats ?? 0}-${action.maxTotalSats ?? 0}`}
                          variant={action.type === 'fold' ? 'danger' : action.type === 'raise' || action.type === 'bet' ? 'primary' : 'secondary'}
                          disabled={disabled}
                          onClick={() => {
                            const totalSats =
                              action.type === 'bet' || action.type === 'raise'
                                ? Number(actionTotal || String(action.minTotalSats ?? 0))
                                : undefined;
                            onSubmitAction(action.type, totalSats);
                          }}
                        >
                          {action.type}
                        </Button>
                      ))}
                    </div>
                  </div>
                </>
              )}
            </div>
          </Panel>
        </div>

        {/* Create / Join */}
        <div className="space-y-6">
          <Panel title="Create Table">
            <div className="space-y-3">
              <InputField label="Table Name" value={createTableName} onChange={(e) => onCreateTableNameChange(e.target.value)} placeholder="Table name" />
              <div className="grid grid-cols-2 gap-3">
                <InputField label="Small Blind" type="number" value={createSmallBlind} onChange={(e) => onCreateSmallBlindChange(e.target.value)} placeholder="50" />
                <InputField label="Big Blind" type="number" value={createBigBlind} onChange={(e) => onCreateBigBlindChange(e.target.value)} placeholder="100" />
              </div>
              <div className="grid grid-cols-2 gap-3">
                <InputField label="Buy-in Min" type="number" value={createBuyInMin} onChange={(e) => onCreateBuyInMinChange(e.target.value)} placeholder="4000" />
                <InputField label="Buy-in Max" type="number" value={createBuyInMax} onChange={(e) => onCreateBuyInMaxChange(e.target.value)} placeholder="4000" />
              </div>
              <Button variant="primary" onClick={onCreate} disabled={disabled} className="w-full">
                Create Public Table
              </Button>
            </div>
          </Panel>

          <Panel title="Join Table">
            <div className="space-y-3">
              <InputField label="Invite Code" value={joinInviteCode} onChange={(e) => onJoinInviteCodeChange(e.target.value)} placeholder="Enter invite code" />
              <InputField label="Buy-in" type="number" value={joinBuyIn} onChange={(e) => onJoinBuyInChange(e.target.value)} placeholder="4000" />
              <Button variant="primary" onClick={onJoin} disabled={disabled || !joinInviteCode} className="w-full">
                Join
              </Button>
            </div>
          </Panel>

          <Panel title="Hosting">
            <div className="flex gap-2">
              <Button variant="secondary" onClick={onAnnounce} disabled={disabled || !selectedLocalTableId} className="flex-1">
                Announce
              </Button>
              <Button variant="secondary" onClick={onRotateHost} disabled={disabled || !selectedLocalTableId} className="flex-1">
                Rotate Host
              </Button>
            </div>
            {localPublicTables.length > 0 && (
              <div className="mt-4 space-y-2">
                <div className="text-xs text-muted-foreground uppercase tracking-wide">Daemon Public Ads</div>
                {localPublicTables.map((table) => (
                  <div key={table.advertisement.tableId} className="rounded-md border border-border bg-secondary/50 p-2 text-xs">
                    <div className="text-foreground">{table.advertisement.tableName}</div>
                    <div className="text-muted-foreground">
                      {formatSats(table.advertisement.stakes.smallBlindSats)} / {formatSats(table.advertisement.stakes.bigBlindSats)}
                    </div>
                  </div>
                ))}
              </div>
            )}
          </Panel>
        </div>
      </div>

      {/* Public Browser */}
      <Panel title="Public Browser">
        <div className="space-y-4">
          <div className="flex items-center gap-2 text-sm text-muted-foreground">
            <Eye className="h-4 w-4 text-primary" />
            <span>Indexed Public Tables</span>
            <span className="ml-auto text-foreground">{publicTables.length}</span>
          </div>

          <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-3">
            {publicTables.map((table) => (
              <button
                key={table.advertisement.tableId}
                onClick={() => onSelectPublicTable(table.advertisement.tableId)}
                className={`rounded-lg border p-4 text-left transition-colors ${
                  selectedPublicTableId === table.advertisement.tableId
                    ? 'border-primary bg-primary/10'
                    : 'border-border bg-card/30 hover:bg-card/50'
                }`}
              >
                <div className="flex items-start justify-between mb-2">
                  <div className="text-sm text-foreground">{table.advertisement.tableName}</div>
                  <div className="text-xs px-2 py-0.5 rounded-md border border-primary/30 bg-primary/10 text-primary">
                    {formatPhase(table.latestState?.phase)}
                  </div>
                </div>
                <div className="grid grid-cols-3 gap-2 text-xs text-muted-foreground">
                  <div className="flex items-center gap-1"><Users className="h-3 w-3" />{table.advertisement.occupiedSeats}/{table.advertisement.seatCount}</div>
                  <div className="flex items-center gap-1"><Coins className="h-3 w-3" />{table.latestState?.potSats ?? 0}</div>
                  <div className="truncate">{table.advertisement.visibility}</div>
                </div>
              </button>
            ))}
          </div>

          {selectedPublicTable && (
            <div className="border-t border-border pt-4 space-y-4">
              <div className="grid grid-cols-2 md:grid-cols-4 gap-3">
                <div className="rounded-md border border-border bg-secondary/50 p-2 text-xs">
                  <div className="text-muted-foreground">Table</div>
                  <div className="text-foreground">{selectedPublicTable.advertisement.tableName}</div>
                </div>
                <div className="rounded-md border border-border bg-secondary/50 p-2 text-xs">
                  <div className="text-muted-foreground">Phase</div>
                  <div className="text-foreground">{formatPhase(selectedPublicTable.latestState?.phase)}</div>
                </div>
                <div className="rounded-md border border-border bg-secondary/50 p-2 text-xs">
                  <div className="text-muted-foreground">Pot</div>
                  <div className="text-foreground">{formatSats(selectedPublicTable.latestState?.potSats)}</div>
                </div>
                <div className="rounded-md border border-border bg-secondary/50 p-2 text-xs">
                  <div className="text-muted-foreground">Host</div>
                  <div className="text-foreground truncate">{selectedPublicTable.advertisement.hostPeerId}</div>
                </div>
              </div>

              <div className="flex gap-2 min-h-[80px] items-center flex-wrap">
                {(selectedPublicTable.latestState?.board ?? []).map((card) => (
                  <PokerCard key={card} code={card} />
                ))}
              </div>

              <div className="grid grid-cols-2 md:grid-cols-4 gap-2">
                {(selectedPublicTable.latestState?.seatedPlayers ?? []).map((player) => (
                  <div key={player.playerId} className="rounded-md border border-border bg-secondary/50 p-2 text-xs">
                    <div className="text-foreground font-medium">{player.nickname}</div>
                    <div className="text-muted-foreground">Seat {player.seatIndex + 1} &bull; {player.status}</div>
                    <div className="text-foreground">{formatSats(selectedPublicTable.latestState?.chipBalances[player.playerId])}</div>
                  </div>
                ))}
              </div>

              {selectedPublicTableUpdates.length > 0 && (
                <div className="space-y-1">
                  <div className="text-xs text-muted-foreground uppercase tracking-wide">Recent Updates</div>
                  {selectedPublicTableUpdates.map((update, i) => (
                    <div key={`${update.type}-${i}`} className="rounded border border-border bg-secondary/30 px-2 py-1.5 text-xs">
                      <span className="text-primary">{update.type}</span>
                      <span className="text-muted-foreground"> &bull; {formatTimestamp('publishedAt' in update ? (update as { publishedAt?: string }).publishedAt : null)}</span>
                    </div>
                  ))}
                </div>
              )}
            </div>
          )}
        </div>
      </Panel>
    </div>
  );
}
