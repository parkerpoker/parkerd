import { useEffect, useMemo, useState } from "react";

import type { PublicTableView } from "@parker/protocol";

import { PokerCard } from "./components/PokerCard.js";
import { getPublicTable, listPublicTables } from "./lib/api.js";

function formatSats(value?: number) {
  return `${value ?? 0} sats`;
}

function formatPhase(value: string | null | undefined) {
  if (!value) {
    return "waiting";
  }
  return value;
}

export function App() {
  const [tables, setTables] = useState<PublicTableView[]>([]);
  const [selectedTableId, setSelectedTableId] = useState<string | null>(null);
  const [selectedTable, setSelectedTable] = useState<PublicTableView | null>(null);
  const [statusMessage, setStatusMessage] = useState("Loading public Parker tables...");

  useEffect(() => {
    let cancelled = false;

    async function refresh() {
      try {
        const nextTables = await listPublicTables();
        if (cancelled) {
          return;
        }
        setTables(nextTables);
        const nextSelectedId = selectedTableId ?? nextTables[0]?.advertisement.tableId ?? null;
        setSelectedTableId(nextSelectedId);
        if (nextSelectedId) {
          setSelectedTable(await getPublicTable(nextSelectedId));
        } else {
          setSelectedTable(null);
        }
        setStatusMessage(
          nextTables.length > 0
            ? `${nextTables.length} public table${nextTables.length === 1 ? "" : "s"} indexed`
            : "No public tables are indexed right now.",
        );
      } catch (error) {
        if (!cancelled) {
          setStatusMessage((error as Error).message);
        }
      }
    }

    void refresh();
    const interval = window.setInterval(() => {
      void refresh();
    }, 3_000);

    return () => {
      cancelled = true;
      window.clearInterval(interval);
    };
  }, [selectedTableId]);

  const updates = useMemo(() => selectedTable?.recentUpdates ?? [], [selectedTable]);
  const latestState = selectedTable?.latestState;

  return (
    <main style={{ margin: "0 auto", maxWidth: 1100, padding: "2rem 1.25rem" }}>
      <header style={{ marginBottom: "2rem" }}>
        <p style={{ letterSpacing: "0.12em", margin: 0, textTransform: "uppercase" }}>
          Parker Network
        </p>
        <h1 style={{ marginBottom: "0.5rem" }}>Read-only public table browser</h1>
        <p style={{ margin: 0, maxWidth: 720 }}>
          The website is no longer part of gameplay. It only reflects signed public table ads and
          spectator updates collected by optional indexers.
        </p>
      </header>

      <section
        style={{
          display: "grid",
          gap: "1.5rem",
          gridTemplateColumns: "minmax(280px, 360px) minmax(0, 1fr)",
        }}
      >
        <aside
          style={{
            border: "1px solid rgba(0,0,0,0.12)",
            borderRadius: 20,
            padding: "1rem",
          }}
        >
          <h2 style={{ marginTop: 0 }}>Public Tables</h2>
          <p>{statusMessage}</p>
          <div style={{ display: "grid", gap: "0.75rem" }}>
            {tables.map((table) => (
              <button
                key={table.advertisement.tableId}
                onClick={() => setSelectedTableId(table.advertisement.tableId)}
                style={{
                  background:
                    selectedTableId === table.advertisement.tableId ? "rgba(0,0,0,0.08)" : "white",
                  border: "1px solid rgba(0,0,0,0.12)",
                  borderRadius: 16,
                  cursor: "pointer",
                  padding: "0.9rem",
                  textAlign: "left",
                }}
              >
                <strong>{table.advertisement.tableName}</strong>
                <div>{formatSats(table.advertisement.stakes.smallBlindSats)} / {formatSats(table.advertisement.stakes.bigBlindSats)}</div>
                <div>
                  {table.advertisement.occupiedSeats}/{table.advertisement.seatCount} seats,{" "}
                  {table.advertisement.witnessCount} witness
                  {table.advertisement.witnessCount === 1 ? "" : "es"}
                </div>
                <div>{table.advertisement.visibility} table</div>
              </button>
            ))}
          </div>
        </aside>

        <section
          style={{
            border: "1px solid rgba(0,0,0,0.12)",
            borderRadius: 20,
            padding: "1.25rem",
          }}
        >
          {!selectedTable ? (
            <p style={{ margin: 0 }}>Select a public table to inspect its read-only view.</p>
          ) : (
            <>
              <header style={{ marginBottom: "1.25rem" }}>
                <h2 style={{ marginBottom: "0.5rem" }}>{selectedTable.advertisement.tableName}</h2>
                <p style={{ margin: 0 }}>
                  Host {selectedTable.advertisement.hostPeerId} · {selectedTable.advertisement.visibility} ·{" "}
                  {formatSats(selectedTable.advertisement.stakes.smallBlindSats)} /{" "}
                  {formatSats(selectedTable.advertisement.stakes.bigBlindSats)}
                </p>
              </header>

              <div style={{ display: "grid", gap: "1rem", gridTemplateColumns: "repeat(2, minmax(0, 1fr))" }}>
                <div>
                  <strong>Status</strong>
                  <div>{formatPhase(latestState?.phase)}</div>
                </div>
                <div>
                  <strong>Pot</strong>
                  <div>{formatSats(latestState?.potSats)}</div>
                </div>
                <div>
                  <strong>Current Bet</strong>
                  <div>{formatSats(latestState?.currentBetSats)}</div>
                </div>
                <div>
                  <strong>Spectators</strong>
                  <div>{selectedTable.advertisement.spectatorsAllowed ? "Allowed" : "Off"}</div>
                </div>
              </div>

              <section style={{ marginTop: "1.5rem" }}>
                <h3>Board</h3>
                <div style={{ display: "flex", gap: "0.5rem", minHeight: 96 }}>
                  {(latestState?.board ?? []).map((card) => (
                    <PokerCard key={card} code={card} />
                  ))}
                </div>
              </section>

              <section style={{ marginTop: "1.5rem" }}>
                <h3>Players</h3>
                <div style={{ display: "grid", gap: "0.75rem" }}>
                  {(latestState?.seatedPlayers ?? []).map((player) => (
                    <div
                      key={player.playerId}
                      style={{
                        border: "1px solid rgba(0,0,0,0.08)",
                        borderRadius: 14,
                        padding: "0.9rem",
                      }}
                    >
                      <strong>{player.nickname}</strong>
                      <div>Seat {player.seatIndex + 1}</div>
                      <div>Status: {player.status}</div>
                      <div>Stack: {formatSats(latestState?.chipBalances[player.playerId])}</div>
                    </div>
                  ))}
                </div>
              </section>

              <section style={{ marginTop: "1.5rem" }}>
                <h3>Recent Public Updates</h3>
                <div style={{ display: "grid", gap: "0.75rem" }}>
                  {updates.map((update, index) => (
                    <div
                      key={`${update.type}-${index}`}
                      style={{
                        border: "1px solid rgba(0,0,0,0.08)",
                        borderRadius: 14,
                        padding: "0.9rem",
                      }}
                    >
                      <strong>{update.type}</strong>
                      <div>{"publishedAt" in update ? update.publishedAt : "n/a"}</div>
                    </div>
                  ))}
                </div>
              </section>
            </>
          )}
        </section>
      </section>
    </main>
  );
}
