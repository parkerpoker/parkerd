import { Terminal, Activity } from 'lucide-react';

import type { LocalControllerLogEvent } from '../../lib/localControllerApi.js';

import { Panel } from '../components/panel.js';

interface LogsViewProps {
  localLogs: LocalControllerLogEvent[];
}

function toPrettyJson(value: unknown) {
  return JSON.stringify(value, null, 2);
}

export function LogsView({ localLogs }: LogsViewProps) {
  const typeColors: Record<string, string> = {
    info: 'text-[#3498db]',
    result: 'text-[#2ecc71]',
    error: 'text-[#e74c3c]',
  };

  return (
    <div className="space-y-6">
      {/* Summary */}
      <Panel>
        <div className="flex items-center justify-between">
          <div className="flex items-center gap-2 text-sm text-muted-foreground">
            <Terminal className="h-4 w-4 text-primary" />
            <span>Daemon Event Stream</span>
          </div>
          <div className="flex items-center gap-1.5 text-muted-foreground">
            <Activity className="h-3.5 w-3.5 text-[#2ecc71] animate-pulse" />
            <span className="text-xs">{localLogs.length} events</span>
          </div>
        </div>
      </Panel>

      {/* Compact Log List */}
      <Panel title="Event Logs">
        <div className="rounded-md border border-border bg-black/40 p-3 max-h-[200px] overflow-y-auto font-mono text-xs space-y-1">
          {localLogs.length === 0 && (
            <div className="text-muted-foreground">The SSE stream has not delivered local log events yet.</div>
          )}
          {localLogs.map((entry, i) => (
            <div key={`${entry.level}-${i}`} className="flex gap-3">
              <span className={typeColors[entry.level] ?? 'text-muted-foreground'}>
                [{entry.level.toUpperCase()}]
              </span>
              <span className="text-foreground">{entry.message ?? 'result payload'}</span>
            </div>
          ))}
        </div>
      </Panel>

      {/* Detailed Log Entries */}
      <Panel title="Detailed Event Stream">
        <div className="rounded-md border border-border bg-black/40 p-4 max-h-[500px] overflow-y-auto font-mono text-xs space-y-2">
          {localLogs.length === 0 && (
            <div className="text-muted-foreground">No events yet.</div>
          )}
          {localLogs.map((entry, i) => (
            <div key={`detail-${entry.level}-${i}`}>
              <div className="flex gap-3">
                <span className={typeColors[entry.level] ?? 'text-muted-foreground'}>
                  [{entry.level.toUpperCase()}]
                </span>
                <span className="text-foreground">{entry.message ?? 'result payload'}</span>
              </div>
              {entry.data != null && (
                <pre className="pl-16 text-muted-foreground whitespace-pre-wrap">{toPrettyJson(entry.data)}</pre>
              )}
            </div>
          ))}
        </div>
      </Panel>
    </div>
  );
}
