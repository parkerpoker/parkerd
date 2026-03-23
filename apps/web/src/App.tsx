import { startTransition, useEffect, useState } from "react";

import type {
  LocalProfileSummary,
  MeshTableView,
} from "@parker/daemon-runtime";
import type { PublicTableView } from "@parker/protocol";

import { PokerCard } from "./components/PokerCard.js";
import {
  announceLocalTable,
  bootstrapLocalPeer,
  bootstrapLocalProfile,
  cashOutLocalTable,
  createLocalTable,
  exitLocalTable,
  getLocalProfileStatus,
  getLocalTable,
  joinLocalTable,
  listLocalProfiles,
  listLocalPublicTables,
  renewLocalTable,
  requestLocalDeposit,
  requestLocalFaucet,
  requestLocalOffboard,
  requestLocalOnboard,
  requestLocalWithdraw,
  rotateLocalHost,
  startLocalDaemon,
  stopLocalDaemon,
  submitLocalTableAction,
  type LocalControllerLogEvent,
  type LocalProfileStatusResponse,
} from "./lib/localControllerApi.js";
import { subscribeToLocalController } from "./lib/localControllerStream.js";
import { getPublicTable, listPublicTables } from "./lib/indexerApi.js";

const ACTIVE_PROFILE_STORAGE_KEY = "parker-active-profile";
const DEFAULT_REGTEST_FAUCET_SATS = "25000";
const DEFAULT_TABLE_BUYIN_SATS = "4000";
const DEFAULT_DEPOSIT_SATS = "5000";
const DEFAULT_WITHDRAW_SATS = "2000";

function formatSats(value?: number | null) {
  return `${value ?? 0} sats`;
}

function formatPhase(value: string | null | undefined) {
  return value ?? "waiting";
}

function formatTimestamp(value?: string | null) {
  if (!value) {
    return "n/a";
  }
  return new Date(value).toLocaleString();
}

function toPrettyJson(value: unknown) {
  return JSON.stringify(value, null, 2);
}

function readStoredProfile() {
  try {
    return window.localStorage.getItem(ACTIVE_PROFILE_STORAGE_KEY);
  } catch {
    return null;
  }
}

function writeStoredProfile(profile: string | null) {
  try {
    if (!profile) {
      window.localStorage.removeItem(ACTIVE_PROFILE_STORAGE_KEY);
      return;
    }
    window.localStorage.setItem(ACTIVE_PROFILE_STORAGE_KEY, profile);
  } catch {
    // Ignore local storage failures.
  }
}

export function App() {
  const [publicTables, setPublicTables] = useState<PublicTableView[]>([]);
  const [selectedPublicTableId, setSelectedPublicTableId] = useState<string | null>(null);
  const [selectedPublicTable, setSelectedPublicTable] = useState<PublicTableView | null>(null);
  const [publicStatus, setPublicStatus] = useState("Loading public table ads...");

  const [controllerAvailable, setControllerAvailable] = useState<boolean | null>(null);
  const [controllerStatus, setControllerStatus] = useState("Checking for the local Parker controller...");
  const [controllerConnected, setControllerConnected] = useState(false);
  const [profiles, setProfiles] = useState<LocalProfileSummary[]>([]);
  const [selectedProfile, setSelectedProfile] = useState<string | null>(readStoredProfile);
  const [profileStatus, setProfileStatus] = useState<LocalProfileStatusResponse | null>(null);
  const [localPublicTables, setLocalPublicTables] = useState<PublicTableView[]>([]);
  const [selectedLocalTableId, setSelectedLocalTableId] = useState<string | null>(null);
  const [selectedLocalTable, setSelectedLocalTable] = useState<MeshTableView | null>(null);
  const [localLogs, setLocalLogs] = useState<LocalControllerLogEvent[]>([]);
  const [resultPayload, setResultPayload] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [busyAction, setBusyAction] = useState<string | null>(null);
  const [streamRevision, setStreamRevision] = useState(0);

  const [nickname, setNickname] = useState("");
  const [depositAmount, setDepositAmount] = useState(DEFAULT_DEPOSIT_SATS);
  const [withdrawAmount, setWithdrawAmount] = useState(DEFAULT_WITHDRAW_SATS);
  const [withdrawInvoice, setWithdrawInvoice] = useState("lnmock-example-invoice");
  const [faucetAmount, setFaucetAmount] = useState(DEFAULT_REGTEST_FAUCET_SATS);
  const [offboardAddress, setOffboardAddress] = useState("bcrt1qexampleoffboardaddress000000000");
  const [offboardAmount, setOffboardAmount] = useState("");
  const [peerAlias, setPeerAlias] = useState("");
  const [peerUrl, setPeerUrl] = useState("ws://127.0.0.1:7777/mesh");
  const [createTableName, setCreateTableName] = useState("Browser Table");
  const [createSmallBlind, setCreateSmallBlind] = useState("50");
  const [createBigBlind, setCreateBigBlind] = useState("100");
  const [createBuyInMin, setCreateBuyInMin] = useState(DEFAULT_TABLE_BUYIN_SATS);
  const [createBuyInMax, setCreateBuyInMax] = useState(DEFAULT_TABLE_BUYIN_SATS);
  const [joinInviteCode, setJoinInviteCode] = useState("");
  const [joinBuyIn, setJoinBuyIn] = useState(DEFAULT_TABLE_BUYIN_SATS);
  const [actionTotal, setActionTotal] = useState("");

  const networkName = import.meta.env.VITE_NETWORK ?? "regtest";
  const localMesh = profileStatus?.daemon.state?.mesh;
  const localWallet = profileStatus?.daemon.state?.wallet;
  const localTableSummaries = localMesh?.tables ?? [];
  const peers = localMesh?.peers ?? [];
  const selectedPublicTableUpdates = selectedPublicTable?.recentUpdates ?? [];
  const selectedLocalPublicState = selectedLocalTable?.publicState;
  const selectedLocalPlayers = selectedLocalPublicState?.seatedPlayers ?? [];
  const localPlayerId = selectedLocalTable?.local.myPlayerId ?? null;
  const localPlayer = selectedLocalPlayers.find((player) => player.playerId === localPlayerId) ?? null;
  const actingPlayer =
    selectedLocalPlayers.find((player) => player.seatIndex === selectedLocalPublicState?.actingSeatIndex) ?? null;
  const latestSnapshot = selectedLocalTable?.latestSnapshot;
  const latestCheckpoint = selectedLocalTable?.latestFullySignedSnapshot;
  const spectatorModeOnly = controllerAvailable !== true || !selectedProfile;

  useEffect(() => {
    writeStoredProfile(selectedProfile);
  }, [selectedProfile]);

  useEffect(() => {
    let cancelled = false;

    async function refreshPublicView() {
      try {
        const nextTables = await listPublicTables();
        if (cancelled) {
          return;
        }

        startTransition(() => {
          setPublicTables(nextTables);
        });

        const preferredTableId =
          selectedPublicTableId && nextTables.some((table) => table.advertisement.tableId === selectedPublicTableId)
            ? selectedPublicTableId
            : nextTables[0]?.advertisement.tableId ?? null;
        setSelectedPublicTableId(preferredTableId);
        setPublicStatus(
          nextTables.length > 0
            ? `${nextTables.length} indexed public table${nextTables.length === 1 ? "" : "s"}`
            : "No public tables are indexed right now.",
        );
      } catch (error) {
        if (!cancelled) {
          setPublicStatus((error as Error).message);
        }
      }
    }

    void refreshPublicView();
    const interval = window.setInterval(() => {
      void refreshPublicView();
    }, 4_000);

    return () => {
      cancelled = true;
      window.clearInterval(interval);
    };
  }, [selectedPublicTableId]);

  useEffect(() => {
    if (!selectedPublicTableId) {
      setSelectedPublicTable(null);
      return;
    }

    let cancelled = false;
    void getPublicTable(selectedPublicTableId)
      .then((table) => {
        if (!cancelled) {
          setSelectedPublicTable(table);
        }
      })
      .catch((error) => {
        if (!cancelled) {
          setPublicStatus((error as Error).message);
        }
      });

    return () => {
      cancelled = true;
    };
  }, [selectedPublicTableId]);

  useEffect(() => {
    let cancelled = false;

    async function refreshProfiles() {
      try {
        const nextProfiles = await listLocalProfiles();
        if (cancelled) {
          return;
        }

        setControllerAvailable(true);
        setControllerStatus("Local controller detected. Browser control is available on this machine.");
        startTransition(() => {
          setProfiles(nextProfiles);
        });

        const nextSelectedProfile =
          selectedProfile && nextProfiles.some((profile) => profile.profileName === selectedProfile)
            ? selectedProfile
            : nextProfiles[0]?.profileName ?? null;
        if (nextSelectedProfile !== selectedProfile) {
          setSelectedProfile(nextSelectedProfile);
        }
        if (nextProfiles.length === 0) {
          setNotice("The controller is running, but no local profiles were found yet.");
        }
      } catch (error) {
        if (cancelled) {
          return;
        }
        setControllerAvailable(false);
        setControllerConnected(false);
        setProfiles([]);
        setProfileStatus(null);
        setSelectedLocalTableId(null);
        setSelectedLocalTable(null);
        setControllerStatus((error as Error).message);
      }
    }

    void refreshProfiles();

    return () => {
      cancelled = true;
    };
  }, [selectedProfile]);

  useEffect(() => {
    if (!controllerAvailable || !selectedProfile) {
      setProfileStatus(null);
      setSelectedLocalTableId(null);
      setSelectedLocalTable(null);
      return;
    }

    const profile = selectedProfile;
    let cancelled = false;

    async function refreshSelectedProfile() {
      try {
        const nextStatus = await getLocalProfileStatus(profile);
        if (cancelled) {
          return;
        }

        setProfileStatus(nextStatus);

        if (!nextStatus.daemon.reachable) {
          setControllerConnected(false);
          setLocalPublicTables([]);
          setSelectedLocalTable(null);
          setSelectedLocalTableId(
            nextStatus.profile.currentMeshTableId ??
              nextStatus.profile.currentTableId ??
              nextStatus.daemon.state?.mesh?.tables[0]?.tableId ??
              null,
          );
          return;
        }

        const nextKnownTableIds = new Set(nextStatus.daemon.state?.mesh?.tables.map((table) => table.tableId) ?? []);
        const nextTableId =
          selectedLocalTableId && nextKnownTableIds.has(selectedLocalTableId)
            ? selectedLocalTableId
            : nextStatus.profile.currentMeshTableId ??
              nextStatus.profile.currentTableId ??
              nextStatus.daemon.state?.mesh?.tables[0]?.tableId ??
              null;
        setSelectedLocalTableId(nextTableId);

        try {
          const nextLocalPublicTables = await listLocalPublicTables(profile);
          if (!cancelled) {
            setLocalPublicTables(nextLocalPublicTables as PublicTableView[]);
          }
        } catch {
          if (!cancelled) {
            setLocalPublicTables([]);
          }
        }
      } catch (error) {
        if (!cancelled) {
          setControllerConnected(false);
          setControllerStatus((error as Error).message);
        }
      }
    }

    void refreshSelectedProfile();

    return () => {
      cancelled = true;
    };
  }, [controllerAvailable, selectedLocalTableId, selectedProfile]);

  useEffect(() => {
    if (!controllerAvailable || !selectedProfile || !profileStatus?.daemon.reachable || !selectedLocalTableId) {
      setSelectedLocalTable(null);
      return;
    }

    let cancelled = false;
    void getLocalTable(selectedProfile, selectedLocalTableId)
      .then((table) => {
        if (!cancelled) {
          setSelectedLocalTable(table);
        }
      })
      .catch((error) => {
        if (!cancelled) {
          setNotice((error as Error).message);
          setSelectedLocalTable(null);
        }
      });

    return () => {
      cancelled = true;
    };
  }, [controllerAvailable, profileStatus?.daemon.reachable, selectedLocalTableId, selectedProfile]);

  useEffect(() => {
    if (!controllerAvailable || !selectedProfile || !profileStatus?.daemon.reachable) {
      return;
    }

    let cancelled = false;
    let reconnectTimer: number | undefined;
    const subscription = subscribeToLocalController(selectedProfile, {
      onLog: (payload) => {
        if (cancelled) {
          return;
        }
        setLocalLogs((current) => [payload, ...current].slice(0, 12));
        if (payload.level === "error" && payload.message) {
          setNotice(payload.message);
        }
      },
      onOpen: () => {
        if (!cancelled) {
          setControllerConnected(true);
        }
      },
      onState: () => {
        if (cancelled) {
          return;
        }
        setControllerConnected(true);
        void getLocalProfileStatus(selectedProfile)
          .then((nextStatus) => {
            if (cancelled) {
              return;
            }
            setProfileStatus(nextStatus);
            const nextKnownTableIds = new Set(nextStatus.daemon.state?.mesh?.tables.map((table) => table.tableId) ?? []);
            const nextTableId =
              selectedLocalTableId && nextKnownTableIds.has(selectedLocalTableId)
                ? selectedLocalTableId
                : nextStatus.profile.currentMeshTableId ??
                  nextStatus.profile.currentTableId ??
                  nextStatus.daemon.state?.mesh?.tables[0]?.tableId ??
                  null;
            setSelectedLocalTableId(nextTableId);
            if (nextTableId) {
              void getLocalTable(selectedProfile, nextTableId)
                .then((table) => {
                  if (!cancelled) {
                    setSelectedLocalTable(table);
                  }
                })
                .catch(() => {
                  if (!cancelled) {
                    setSelectedLocalTable(null);
                  }
                });
            } else {
              setSelectedLocalTable(null);
            }
          })
          .catch((error) => {
            if (!cancelled) {
              setControllerStatus((error as Error).message);
            }
          });
      },
    });

    subscription.done.catch((error) => {
      if (cancelled) {
        return;
      }
      if (error instanceof DOMException && error.name === "AbortError") {
        return;
      }
      setControllerConnected(false);
      setControllerStatus((error as Error).message);
      reconnectTimer = window.setTimeout(() => {
        setStreamRevision((value) => value + 1);
      }, 1_500);
    });

    return () => {
      cancelled = true;
      if (reconnectTimer) {
        window.clearTimeout(reconnectTimer);
      }
      subscription.close();
    };
  }, [controllerAvailable, profileStatus?.daemon.reachable, selectedLocalTableId, selectedProfile, streamRevision]);

  async function runControllerAction(label: string, action: () => Promise<unknown>) {
    setBusyAction(label);
    setNotice(null);
    setResultPayload(null);

    try {
      const result = await action();
      setNotice(`${label} completed.`);
      setResultPayload(result === undefined ? null : toPrettyJson(result));
      if (selectedProfile) {
        const nextStatus = await getLocalProfileStatus(selectedProfile);
        setProfileStatus(nextStatus);
      }
      if (selectedLocalTableId && selectedProfile) {
        try {
          const nextTable = await getLocalTable(selectedProfile, selectedLocalTableId);
          setSelectedLocalTable(nextTable);
        } catch {
          setSelectedLocalTable(null);
        }
      }
      try {
        const nextProfiles = await listLocalProfiles();
        startTransition(() => {
          setProfiles(nextProfiles);
        });
      } catch {
        // Ignore secondary refresh failures.
      }
      try {
        const nextTables = await listPublicTables();
        startTransition(() => {
          setPublicTables(nextTables);
        });
      } catch {
        // Ignore public refresh failures after local actions.
      }
    } catch (error) {
      setNotice((error as Error).message);
    } finally {
      setBusyAction(null);
    }
  }

  return (
    <main className="shell">
      <header className="hero">
        <div className="eyebrow">Parker Local Controller UI</div>
        <div className="hero-grid">
          <div>
            <h1>One browser. Two views. Zero keys in the tab.</h1>
            <p className="hero-copy">
              Public tables still come from the indexer. Wallet control, table joins, gameplay
              actions, and settlement requests now stay on localhost through the controller, which
              adapts browser-safe HTTP and SSE onto the existing daemon RPC.
            </p>
          </div>
          <div className="hero-status">
            <div className="mode-chip">
              <strong>Public spectator</strong>
              <span>{publicStatus}</span>
            </div>
            <div className={`mode-chip ${spectatorModeOnly ? "offline" : "online"}`}>
              <strong>{spectatorModeOnly ? "Controller unavailable" : "Controller mode active"}</strong>
              <span>{controllerStatus}</span>
            </div>
            <div className={`mode-chip ${controllerConnected ? "online" : "offline"}`}>
              <strong>Local stream</strong>
              <span>{controllerConnected ? "Receiving daemon state over SSE." : "Disconnected or idle."}</span>
            </div>
          </div>
        </div>
      </header>

      <section className="dashboard">
        <section className="panel">
          <div className="panel-head">
            <div>
              <div className="eyebrow">Profile Shell</div>
              <h2>Local controller</h2>
            </div>
            <div className="pill">{selectedProfile ?? "spectator only"}</div>
          </div>

          <div className="field">
            <label htmlFor="profile-select">Selected profile</label>
            <select
              id="profile-select"
              className="select"
              disabled={!controllerAvailable || profiles.length === 0}
              value={selectedProfile ?? ""}
              onChange={(event) => {
                setLocalLogs([]);
                setSelectedLocalTable(null);
                setSelectedLocalTableId(null);
                setSelectedProfile(event.target.value || null);
              }}
            >
              <option value="">Choose a local profile</option>
              {profiles.map((profile) => (
                <option key={profile.profileName} value={profile.profileName}>
                  {profile.nickname} ({profile.profileName})
                </option>
              ))}
            </select>
          </div>

          <div className="status-grid">
            <div className="status-card">
              <strong>Daemon</strong>
              <span>{profileStatus?.daemon.reachable ? "running" : "stopped"}</span>
            </div>
            <div className="status-card">
              <strong>Mode</strong>
              <span>{profileStatus?.daemon.metadata?.mode ?? "player"}</span>
            </div>
            <div className="status-card">
              <strong>Peer</strong>
              <span>{localMesh?.peer.peerId ?? "not bootstrapped"}</span>
            </div>
            <div className="status-card">
              <strong>Wallet player</strong>
              <span>{localMesh?.peer.walletPlayerId ?? "not bootstrapped"}</span>
            </div>
          </div>

          <div className="field">
            <label htmlFor="nickname-input">Bootstrap nickname</label>
            <input
              id="nickname-input"
              placeholder={selectedProfile ?? "nickname"}
              value={nickname}
              onChange={(event) => setNickname(event.target.value)}
            />
          </div>

          <div className="button-row">
            <button
              disabled={!selectedProfile || busyAction !== null}
              onClick={() => {
                if (!selectedProfile) {
                  return;
                }
                void runControllerAction("Start daemon", async () => await startLocalDaemon(selectedProfile));
              }}
            >
              Start daemon
            </button>
            <button
              className="secondary"
              disabled={!selectedProfile || busyAction !== null}
              onClick={() => {
                if (!selectedProfile) {
                  return;
                }
                void runControllerAction("Stop daemon", async () => await stopLocalDaemon(selectedProfile));
              }}
            >
              Stop daemon
            </button>
            <button
              className="secondary"
              disabled={!selectedProfile || busyAction !== null}
              onClick={() => {
                if (!selectedProfile) {
                  return;
                }
                void runControllerAction("Bootstrap identity", async () =>
                  await bootstrapLocalProfile(selectedProfile, nickname ? { nickname } : undefined),
                );
              }}
            >
              Bootstrap identity
            </button>
          </div>

          {notice ? <p className="note">{notice}</p> : null}
          {resultPayload ? <pre className="result-block">{resultPayload}</pre> : null}
        </section>

        <section className="panel">
          <div className="panel-head">
            <div>
              <div className="eyebrow">Wallet Panel</div>
              <h2>Bankroll control</h2>
            </div>
            <div className="wallet-balance">{formatSats(localWallet?.availableSats)}</div>
          </div>

          <div className="status-grid">
            <div className="status-card">
              <strong>Available</strong>
              <span>{formatSats(localWallet?.availableSats)}</span>
            </div>
            <div className="status-card">
              <strong>Total</strong>
              <span>{formatSats(localWallet?.totalSats)}</span>
            </div>
            <div className="status-card">
              <strong>Ark receive</strong>
              <code>{localWallet?.arkAddress ?? "n/a"}</code>
            </div>
            <div className="status-card">
              <strong>Boarding</strong>
              <code>{localWallet?.boardingAddress ?? "n/a"}</code>
            </div>
          </div>

          <div className="two-up">
            <div className="field">
              <label htmlFor="deposit-amount">Deposit quote</label>
              <input
                id="deposit-amount"
                value={depositAmount}
                onChange={(event) => setDepositAmount(event.target.value)}
              />
              <button
                disabled={!selectedProfile || busyAction !== null}
                onClick={() => {
                  if (!selectedProfile) {
                    return;
                  }
                  void runControllerAction("Create deposit quote", async () =>
                    await requestLocalDeposit(selectedProfile, Number(depositAmount)),
                  );
                }}
              >
                Create deposit quote
              </button>
            </div>

            <div className="field">
              <label htmlFor="withdraw-amount">Withdraw Lightning invoice</label>
              <input
                id="withdraw-amount"
                value={withdrawAmount}
                onChange={(event) => setWithdrawAmount(event.target.value)}
              />
              <input
                value={withdrawInvoice}
                onChange={(event) => setWithdrawInvoice(event.target.value)}
              />
              <button
                disabled={!selectedProfile || busyAction !== null}
                onClick={() => {
                  if (!selectedProfile) {
                    return;
                  }
                  void runControllerAction("Submit withdrawal", async () =>
                    await requestLocalWithdraw(
                      selectedProfile,
                      Number(withdrawAmount),
                      withdrawInvoice,
                    ),
                  );
                }}
              >
                Send withdrawal
              </button>
            </div>
          </div>

          <div className="two-up">
            <div className="field">
              <label htmlFor="faucet-amount">Regtest faucet</label>
              <input
                id="faucet-amount"
                value={faucetAmount}
                onChange={(event) => setFaucetAmount(event.target.value)}
              />
              <button
                disabled={!selectedProfile || busyAction !== null || networkName !== "regtest"}
                onClick={() => {
                  if (!selectedProfile) {
                    return;
                  }
                  void runControllerAction("Request regtest faucet", async () =>
                    await requestLocalFaucet(selectedProfile, Number(faucetAmount)),
                  );
                }}
              >
                Request faucet
              </button>
            </div>

            <div className="field">
              <label htmlFor="offboard-address">Offboard to on-chain address</label>
              <input
                id="offboard-address"
                value={offboardAddress}
                onChange={(event) => setOffboardAddress(event.target.value)}
              />
              <input
                placeholder="optional sats"
                value={offboardAmount}
                onChange={(event) => setOffboardAmount(event.target.value)}
              />
              <div className="button-row">
                <button
                  disabled={!selectedProfile || busyAction !== null}
                  onClick={() => {
                    if (!selectedProfile) {
                      return;
                    }
                    void runControllerAction("Onboard wallet", async () =>
                      await requestLocalOnboard(selectedProfile),
                    );
                  }}
                >
                  Onboard
                </button>
                <button
                  className="secondary"
                  disabled={!selectedProfile || busyAction !== null}
                  onClick={() => {
                    if (!selectedProfile) {
                      return;
                    }
                    void runControllerAction("Offboard wallet", async () =>
                      await requestLocalOffboard(
                        selectedProfile,
                        offboardAddress,
                        offboardAmount ? Number(offboardAmount) : undefined,
                      ),
                    );
                  }}
                >
                  Offboard
                </button>
              </div>
            </div>
          </div>
        </section>

        <section className="panel">
          <div className="panel-head">
            <div>
              <div className="eyebrow">Network Panel</div>
              <h2>Peers and bootstrap hints</h2>
            </div>
            <div className="pill">{peers.length} peer{peers.length === 1 ? "" : "s"}</div>
          </div>

          <div className="field">
            <label htmlFor="peer-url">Bootstrap peer URL</label>
            <input id="peer-url" value={peerUrl} onChange={(event) => setPeerUrl(event.target.value)} />
            <input
              placeholder="optional alias"
              value={peerAlias}
              onChange={(event) => setPeerAlias(event.target.value)}
            />
            <button
              disabled={!selectedProfile || busyAction !== null}
              onClick={() => {
                if (!selectedProfile) {
                  return;
                }
                void runControllerAction("Add bootstrap peer", async () =>
                  await bootstrapLocalPeer(selectedProfile, {
                    ...(peerAlias ? { alias: peerAlias } : {}),
                    peerUrl,
                    roles: ["host", "witness", "player"],
                  }),
                );
              }}
            >
              Add bootstrap peer
            </button>
          </div>

          <div className="list-grid">
            {peers.length === 0 ? <p className="muted">No peers have been learned yet.</p> : null}
            {peers.map((peer) => (
              <div key={peer.peerId} className="list-card">
                <strong>{peer.alias ?? peer.peerId}</strong>
                <code>{peer.peerUrl}</code>
                <span>{peer.roles.join(", ") || "no roles"}</span>
              </div>
            ))}
          </div>
        </section>

        <section className="panel">
          <div className="panel-head">
            <div>
              <div className="eyebrow">Tables Panel</div>
              <h2>Create, join, and inspect</h2>
            </div>
            <div className="pill">{localTableSummaries.length} local table{localTableSummaries.length === 1 ? "" : "s"}</div>
          </div>

          <div className="table-chip-row">
            {localTableSummaries.length === 0 ? <p className="muted">No local table selected.</p> : null}
            {localTableSummaries.map((table) => (
              <button
                key={table.tableId}
                className={`table-chip ${selectedLocalTableId === table.tableId ? "selected" : ""}`}
                onClick={() => setSelectedLocalTableId(table.tableId)}
              >
                <strong>{table.tableName}</strong>
                <span>{table.status}</span>
              </button>
            ))}
          </div>

          <div className="two-up">
            <div className="field-stack">
              <div className="field">
                <label htmlFor="create-name">Create local table</label>
                <input id="create-name" value={createTableName} onChange={(event) => setCreateTableName(event.target.value)} />
                <div className="inline-fields">
                  <input value={createSmallBlind} onChange={(event) => setCreateSmallBlind(event.target.value)} />
                  <input value={createBigBlind} onChange={(event) => setCreateBigBlind(event.target.value)} />
                </div>
                <div className="inline-fields">
                  <input value={createBuyInMin} onChange={(event) => setCreateBuyInMin(event.target.value)} />
                  <input value={createBuyInMax} onChange={(event) => setCreateBuyInMax(event.target.value)} />
                </div>
              </div>
              <button
                disabled={!selectedProfile || busyAction !== null}
                onClick={() => {
                  if (!selectedProfile) {
                    return;
                  }
                  void runControllerAction("Create table", async () =>
                    await createLocalTable(selectedProfile, {
                      bigBlindSats: Number(createBigBlind),
                      buyInMaxSats: Number(createBuyInMax),
                      buyInMinSats: Number(createBuyInMin),
                      name: createTableName,
                      public: true,
                      smallBlindSats: Number(createSmallBlind),
                      spectatorsAllowed: true,
                    }),
                  );
                }}
              >
                Create public table
              </button>
            </div>

            <div className="field-stack">
              <div className="field">
                <label htmlFor="join-invite">Join by invite</label>
                <input id="join-invite" value={joinInviteCode} onChange={(event) => setJoinInviteCode(event.target.value)} />
                <input value={joinBuyIn} onChange={(event) => setJoinBuyIn(event.target.value)} />
              </div>
              <button
                disabled={!selectedProfile || busyAction !== null || !joinInviteCode}
                onClick={() => {
                  if (!selectedProfile) {
                    return;
                  }
                  void runControllerAction("Join table", async () =>
                    await joinLocalTable(selectedProfile, joinInviteCode, Number(joinBuyIn)),
                  );
                }}
              >
                Join invite
              </button>
            </div>
          </div>

          <div className="button-row">
            <button
              disabled={!selectedProfile || !selectedLocalTableId || busyAction !== null}
              onClick={() => {
                if (!selectedProfile || !selectedLocalTableId) {
                  return;
                }
                void runControllerAction("Announce table", async () =>
                  await announceLocalTable(selectedProfile, selectedLocalTableId),
                );
              }}
            >
              Announce table
            </button>
            <button
              className="secondary"
              disabled={!selectedProfile || !selectedLocalTableId || busyAction !== null}
              onClick={() => {
                if (!selectedProfile || !selectedLocalTableId) {
                  return;
                }
                void runControllerAction("Rotate host", async () =>
                  await rotateLocalHost(selectedProfile, selectedLocalTableId),
                );
              }}
            >
              Rotate host
            </button>
          </div>

          <div className="subhead">Public ads visible to this daemon</div>
          <div className="list-grid compact">
            {localPublicTables.length === 0 ? <p className="muted">No daemon-side public ads are cached yet.</p> : null}
            {localPublicTables.map((table) => (
              <div key={table.advertisement.tableId} className="list-card">
                <strong>{table.advertisement.tableName}</strong>
                <span>
                  {formatSats(table.advertisement.stakes.smallBlindSats)} /{" "}
                  {formatSats(table.advertisement.stakes.bigBlindSats)}
                </span>
                <span>{table.advertisement.visibility}</span>
              </div>
            ))}
          </div>
        </section>

        <section className="panel gameplay-panel">
          <div className="panel-head">
            <div>
              <div className="eyebrow">Gameplay Panel</div>
              <h2>Private local table view</h2>
            </div>
            <div className="pill">{selectedLocalPublicState?.phase ? selectedLocalPublicState.phase : "waiting"}</div>
          </div>

          {!selectedLocalTable ? (
            <p className="muted">
              Pick a local table to inspect the current private state, local hole cards, and action composer.
            </p>
          ) : (
            <>
              <div className="table-summary">
                <div>
                  <strong>{selectedLocalTable.config.name}</strong>
                  <span>{selectedLocalTable.config.visibility} table</span>
                </div>
                <div>
                  <strong>Pot</strong>
                  <span>{formatSats(selectedLocalPublicState?.potSats)}</span>
                </div>
                <div>
                  <strong>Acting player</strong>
                  <span>{actingPlayer?.nickname ?? "waiting"}</span>
                </div>
                <div>
                  <strong>You</strong>
                  <span>{localPlayer?.nickname ?? "spectator"}</span>
                </div>
              </div>

              <div className="board-strip">
                {(selectedLocalPublicState?.board ?? []).map((card) => (
                  <PokerCard key={card} code={card} />
                ))}
              </div>

              <div className="player-grid">
                {selectedLocalPlayers.map((player) => (
                  <div key={player.playerId} className={`player-card ${player.playerId === localPlayerId ? "me" : ""}`}>
                    <strong>{player.nickname}</strong>
                    <span>Seat {player.seatIndex + 1}</span>
                    <span>{player.status}</span>
                    <span>{formatSats(selectedLocalPublicState?.chipBalances[player.playerId])}</span>
                  </div>
                ))}
              </div>

              <div className="subhead">Local hole cards</div>
              <div className="card-strip">
                {(selectedLocalTable.local.myHoleCards ?? ["XX", "XX"]).map((card, index) => (
                  <PokerCard key={`${card}-${index}`} code={card} concealed={card === "XX"} />
                ))}
              </div>

              <div className="action-box">
                <div className="subhead">Allowed actions</div>
                <p className="muted">
                  {selectedLocalTable.local.canAct
                    ? "It is your turn. The controller will send the action to the daemon."
                    : "The daemon says it is not your turn yet."}
                </p>
                <input
                  placeholder="bet / raise total sats"
                  value={actionTotal}
                  onChange={(event) => setActionTotal(event.target.value)}
                />
                <div className="button-row">
                  {selectedLocalTable.local.legalActions.length === 0 ? (
                    <span className="muted">No legal actions are available.</span>
                  ) : null}
                  {selectedLocalTable.local.legalActions.map((action) => (
                    <button
                      key={`${action.type}-${action.minTotalSats ?? 0}-${action.maxTotalSats ?? 0}`}
                      disabled={!selectedProfile || !selectedLocalTableId || busyAction !== null}
                      onClick={() => {
                        if (!selectedProfile || !selectedLocalTableId) {
                          return;
                        }
                        const payload =
                          action.type === "bet" || action.type === "raise"
                            ? {
                                totalSats: Number(actionTotal || String(action.minTotalSats ?? 0)),
                                type: action.type,
                              }
                            : { type: action.type };
                        void runControllerAction(`Submit ${action.type}`, async () =>
                          await submitLocalTableAction(selectedProfile, selectedLocalTableId, payload),
                        );
                      }}
                    >
                      {action.type}
                    </button>
                  ))}
                </div>
              </div>
            </>
          )}
        </section>

        <section className="panel">
          <div className="panel-head">
            <div>
              <div className="eyebrow">Settlement Panel</div>
              <h2>Snapshots and exits</h2>
            </div>
            <div className="pill">{latestCheckpoint ? "checkpoint ready" : "no checkpoint yet"}</div>
          </div>

          <div className="status-grid">
            <div className="status-card">
              <strong>Latest snapshot</strong>
              <span>{latestSnapshot?.snapshotId ?? "n/a"}</span>
            </div>
            <div className="status-card">
              <strong>Latest checkpoint</strong>
              <span>{latestCheckpoint?.snapshotId ?? "n/a"}</span>
            </div>
            <div className="status-card">
              <strong>Signed by</strong>
              <span>{latestCheckpoint?.signatures.length ?? 0} peer(s)</span>
            </div>
            <div className="status-card">
              <strong>Updated</strong>
              <span>{formatTimestamp(latestCheckpoint?.createdAt)}</span>
            </div>
          </div>

          <div className="button-row">
            <button
              disabled={!selectedProfile || !selectedLocalTableId || busyAction !== null}
              onClick={() => {
                if (!selectedProfile || !selectedLocalTableId) {
                  return;
                }
                void runControllerAction("Renew table positions", async () =>
                  await renewLocalTable(selectedProfile, selectedLocalTableId),
                );
              }}
            >
              Renew
            </button>
            <button
              disabled={!selectedProfile || !selectedLocalTableId || busyAction !== null}
              onClick={() => {
                if (!selectedProfile || !selectedLocalTableId) {
                  return;
                }
                void runControllerAction("Cash out", async () =>
                  await cashOutLocalTable(selectedProfile, selectedLocalTableId),
                );
              }}
            >
              Cash out
            </button>
            <button
              className="secondary"
              disabled={!selectedProfile || !selectedLocalTableId || busyAction !== null}
              onClick={() => {
                if (!selectedProfile || !selectedLocalTableId) {
                  return;
                }
                void runControllerAction("Emergency exit", async () =>
                  await exitLocalTable(selectedProfile, selectedLocalTableId),
                );
              }}
            >
              Emergency exit
            </button>
          </div>

          {latestCheckpoint ? (
            <pre className="result-block">{toPrettyJson(latestCheckpoint)}</pre>
          ) : (
            <p className="muted">
              Cooperative settlement data will appear here once the table has produced a signed checkpoint.
            </p>
          )}
        </section>

        <section className="panel spectator-panel">
          <div className="panel-head">
            <div>
              <div className="eyebrow">Public Browser</div>
              <h2>Indexer-backed spectator view</h2>
            </div>
            <div className="pill">{publicTables.length} indexed</div>
          </div>

          <div className="public-layout">
            <div className="list-grid compact">
              {publicTables.map((table) => (
                <button
                  key={table.advertisement.tableId}
                  className={`public-table-card ${selectedPublicTableId === table.advertisement.tableId ? "selected" : ""}`}
                  onClick={() => setSelectedPublicTableId(table.advertisement.tableId)}
                >
                  <strong>{table.advertisement.tableName}</strong>
                  <span>
                    {formatSats(table.advertisement.stakes.smallBlindSats)} /{" "}
                    {formatSats(table.advertisement.stakes.bigBlindSats)}
                  </span>
                  <span>
                    {table.advertisement.occupiedSeats}/{table.advertisement.seatCount} seats
                  </span>
                </button>
              ))}
            </div>

            <div className="spectator-detail">
              {!selectedPublicTable ? (
                <p className="muted">Select a public table to inspect the spectator state.</p>
              ) : (
                <>
                  <div className="table-summary">
                    <div>
                      <strong>{selectedPublicTable.advertisement.tableName}</strong>
                      <span>{selectedPublicTable.advertisement.visibility}</span>
                    </div>
                    <div>
                      <strong>Phase</strong>
                      <span>{formatPhase(selectedPublicTable.latestState?.phase)}</span>
                    </div>
                    <div>
                      <strong>Pot</strong>
                      <span>{formatSats(selectedPublicTable.latestState?.potSats)}</span>
                    </div>
                    <div>
                      <strong>Host</strong>
                      <span>{selectedPublicTable.advertisement.hostPeerId}</span>
                    </div>
                  </div>

                  <div className="board-strip">
                    {(selectedPublicTable.latestState?.board ?? []).map((card) => (
                      <PokerCard key={card} code={card} />
                    ))}
                  </div>

                  <div className="player-grid">
                    {(selectedPublicTable.latestState?.seatedPlayers ?? []).map((player) => (
                      <div key={player.playerId} className="player-card">
                        <strong>{player.nickname}</strong>
                        <span>Seat {player.seatIndex + 1}</span>
                        <span>{player.status}</span>
                        <span>{formatSats(selectedPublicTable.latestState?.chipBalances[player.playerId])}</span>
                      </div>
                    ))}
                  </div>

                  <div className="subhead">Recent public updates</div>
                  <div className="list-grid compact">
                    {selectedPublicTableUpdates.map((update, index) => (
                      <div key={`${update.type}-${index}`} className="list-card">
                        <strong>{update.type}</strong>
                        <span>{formatTimestamp("publishedAt" in update ? update.publishedAt : null)}</span>
                      </div>
                    ))}
                  </div>
                </>
              )}
            </div>
          </div>
        </section>

        <section className="panel">
          <div className="panel-head">
            <div>
              <div className="eyebrow">Daemon Logs</div>
              <h2>Live local events</h2>
            </div>
            <div className="pill">{localLogs.length} recent</div>
          </div>

          <div className="list-grid compact">
            {localLogs.length === 0 ? <p className="muted">The SSE stream has not delivered local log events yet.</p> : null}
            {localLogs.map((entry, index) => (
              <div key={`${entry.level}-${index}`} className="list-card">
                <strong>{entry.level}</strong>
                <span>{entry.message ?? "result payload"}</span>
                {entry.data ? <code>{toPrettyJson(entry.data)}</code> : null}
              </div>
            ))}
          </div>
        </section>
      </section>
    </main>
  );
}
