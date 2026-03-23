import { startTransition, useEffect, useState } from "react";

import type {
  LocalProfileSummary,
  MeshTableView,
  PublicTableView,
} from "./types/parker.js";

import { Header } from "./app/components/header.js";
import { OverviewView } from "./app/views/overview.js";
import { NetworkView } from "./app/views/network.js";
import { WalletView } from "./app/views/wallet.js";
import { TablesView } from "./app/views/tables.js";
import { LogsView } from "./app/views/logs.js";

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
  const [activeTab, setActiveTab] = useState("overview");

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
  const [peerEndpoint, setPeerEndpoint] = useState("tor://peer.onion:9735");
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
  const localTransport = profileStatus?.daemon.state?.transport;
  const localWallet = profileStatus?.daemon.state?.wallet;
  const localTableSummaries = localMesh?.tables ?? [];
  const peers = localTransport?.peers ?? localMesh?.peers ?? [];
  const latestSnapshot = selectedLocalTable?.latestSnapshot;
  const latestCheckpoint = selectedLocalTable?.latestFullySignedSnapshot;
  const spectatorModeOnly = controllerAvailable !== true || !selectedProfile;

  // --- Effects (unchanged from original) ---

  useEffect(() => {
    document.documentElement.classList.add("dark");
  }, []);

  useEffect(() => {
    writeStoredProfile(selectedProfile);
  }, [selectedProfile]);

  useEffect(() => {
    let cancelled = false;

    async function refreshPublicView() {
      try {
        const nextTables = await listPublicTables();
        if (cancelled) return;

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
        if (!cancelled) setSelectedPublicTable(table);
      })
      .catch((error) => {
        if (!cancelled) setPublicStatus((error as Error).message);
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
        if (cancelled) return;

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
        if (cancelled) return;
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
        if (cancelled) return;

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
          if (!cancelled) setLocalPublicTables(nextLocalPublicTables as PublicTableView[]);
        } catch {
          if (!cancelled) setLocalPublicTables([]);
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
        if (!cancelled) setSelectedLocalTable(table);
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
        if (cancelled) return;
        setLocalLogs((current) => [payload, ...current].slice(0, 12));
        if (payload.level === "error" && payload.message) {
          setNotice(payload.message);
        }
      },
      onOpen: () => {
        if (!cancelled) setControllerConnected(true);
      },
      onState: () => {
        if (cancelled) return;
        setControllerConnected(true);
        void getLocalProfileStatus(selectedProfile)
          .then((nextStatus) => {
            if (cancelled) return;
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
                  if (!cancelled) setSelectedLocalTable(table);
                })
                .catch(() => {
                  if (!cancelled) setSelectedLocalTable(null);
                });
            } else {
              setSelectedLocalTable(null);
            }
          })
          .catch((error) => {
            if (!cancelled) setControllerStatus((error as Error).message);
          });
      },
    });

    subscription.done.catch((error) => {
      if (cancelled) return;
      if (error instanceof DOMException && error.name === "AbortError") return;
      setControllerConnected(false);
      setControllerStatus((error as Error).message);
      reconnectTimer = window.setTimeout(() => {
        setStreamRevision((value) => value + 1);
      }, 1_500);
    });

    return () => {
      cancelled = true;
      if (reconnectTimer) window.clearTimeout(reconnectTimer);
      subscription.close();
    };
  }, [controllerAvailable, profileStatus?.daemon.reachable, selectedLocalTableId, selectedProfile, streamRevision]);

  // --- Action runner (unchanged) ---

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

  // --- Render ---

  const renderView = () => {
    switch (activeTab) {
      case "overview":
        return (
          <OverviewView
            profileStatus={profileStatus}
            controllerConnected={controllerConnected}
            spectatorModeOnly={spectatorModeOnly}
            publicTableCount={publicTables.length}
            localTableCount={localTableSummaries.length}
            peerCount={peers.length}
            availableSats={localWallet?.availableSats ?? 0}
            localLogs={localLogs}
          />
        );
      case "network":
        return (
          <NetworkView
            profiles={profiles}
            selectedProfile={selectedProfile}
            onSelectProfile={(profile) => {
              setLocalLogs([]);
              setSelectedLocalTable(null);
              setSelectedLocalTableId(null);
              setSelectedProfile(profile);
            }}
            profileStatus={profileStatus}
            controllerAvailable={controllerAvailable}
            controllerConnected={controllerConnected}
            nickname={nickname}
            onNicknameChange={setNickname}
            peerEndpoint={peerEndpoint}
            onPeerEndpointChange={setPeerEndpoint}
            peerAlias={peerAlias}
            onPeerAliasChange={setPeerAlias}
            peers={peers}
            busyAction={busyAction}
            onAction={runControllerAction}
            notice={notice}
            resultPayload={resultPayload}
            latestSnapshot={latestSnapshot}
            latestCheckpoint={latestCheckpoint}
            selectedLocalTableId={selectedLocalTableId}
            onStartDaemon={() => {
              if (!selectedProfile) return;
              void runControllerAction("Start daemon", async () => await startLocalDaemon(selectedProfile));
            }}
            onStopDaemon={() => {
              if (!selectedProfile) return;
              void runControllerAction("Stop daemon", async () => await stopLocalDaemon(selectedProfile));
            }}
            onBootstrapIdentity={() => {
              if (!selectedProfile) return;
              void runControllerAction("Bootstrap identity", async () =>
                await bootstrapLocalProfile(selectedProfile, nickname ? { nickname } : undefined),
              );
            }}
            onAddPeer={() => {
              if (!selectedProfile) return;
              void runControllerAction("Add bootstrap peer", async () =>
                await bootstrapLocalPeer(selectedProfile, {
                  ...(peerAlias ? { alias: peerAlias } : {}),
                  endpoint: peerEndpoint,
                  peerUrl: peerEndpoint,
                  roles: ["host", "witness", "player"],
                }),
              );
            }}
            onRenew={() => {
              if (!selectedProfile || !selectedLocalTableId) return;
              void runControllerAction("Renew table positions", async () =>
                await renewLocalTable(selectedProfile, selectedLocalTableId),
              );
            }}
            onCashOut={() => {
              if (!selectedProfile || !selectedLocalTableId) return;
              void runControllerAction("Cash out", async () =>
                await cashOutLocalTable(selectedProfile, selectedLocalTableId),
              );
            }}
            onEmergencyExit={() => {
              if (!selectedProfile || !selectedLocalTableId) return;
              void runControllerAction("Emergency exit", async () =>
                await exitLocalTable(selectedProfile, selectedLocalTableId),
              );
            }}
          />
        );
      case "wallet":
        return (
          <WalletView
            profileStatus={profileStatus}
            selectedProfile={selectedProfile}
            busyAction={busyAction}
            networkName={networkName}
            depositAmount={depositAmount}
            onDepositAmountChange={setDepositAmount}
            withdrawAmount={withdrawAmount}
            onWithdrawAmountChange={setWithdrawAmount}
            withdrawInvoice={withdrawInvoice}
            onWithdrawInvoiceChange={setWithdrawInvoice}
            faucetAmount={faucetAmount}
            onFaucetAmountChange={setFaucetAmount}
            offboardAddress={offboardAddress}
            onOffboardAddressChange={setOffboardAddress}
            offboardAmount={offboardAmount}
            onOffboardAmountChange={setOffboardAmount}
            onDeposit={() => {
              if (!selectedProfile) return;
              void runControllerAction("Create deposit quote", async () =>
                await requestLocalDeposit(selectedProfile, Number(depositAmount)),
              );
            }}
            onWithdraw={() => {
              if (!selectedProfile) return;
              void runControllerAction("Submit withdrawal", async () =>
                await requestLocalWithdraw(selectedProfile, Number(withdrawAmount), withdrawInvoice),
              );
            }}
            onFaucet={() => {
              if (!selectedProfile) return;
              void runControllerAction("Request regtest faucet", async () =>
                await requestLocalFaucet(selectedProfile, Number(faucetAmount)),
              );
            }}
            onOnboard={() => {
              if (!selectedProfile) return;
              void runControllerAction("Onboard wallet", async () =>
                await requestLocalOnboard(selectedProfile),
              );
            }}
            onOffboard={() => {
              if (!selectedProfile) return;
              void runControllerAction("Offboard wallet", async () =>
                await requestLocalOffboard(
                  selectedProfile,
                  offboardAddress,
                  offboardAmount ? Number(offboardAmount) : undefined,
                ),
              );
            }}
          />
        );
      case "tables":
        return (
          <TablesView
            selectedProfile={selectedProfile}
            busyAction={busyAction}
            localTableSummaries={localTableSummaries}
            selectedLocalTableId={selectedLocalTableId}
            onSelectLocalTable={setSelectedLocalTableId}
            selectedLocalTable={selectedLocalTable}
            localPublicTables={localPublicTables}
            publicTables={publicTables}
            selectedPublicTableId={selectedPublicTableId}
            onSelectPublicTable={setSelectedPublicTableId}
            selectedPublicTable={selectedPublicTable}
            createTableName={createTableName}
            onCreateTableNameChange={setCreateTableName}
            createSmallBlind={createSmallBlind}
            onCreateSmallBlindChange={setCreateSmallBlind}
            createBigBlind={createBigBlind}
            onCreateBigBlindChange={setCreateBigBlind}
            createBuyInMin={createBuyInMin}
            onCreateBuyInMinChange={setCreateBuyInMin}
            createBuyInMax={createBuyInMax}
            onCreateBuyInMaxChange={setCreateBuyInMax}
            joinInviteCode={joinInviteCode}
            onJoinInviteCodeChange={setJoinInviteCode}
            joinBuyIn={joinBuyIn}
            onJoinBuyInChange={setJoinBuyIn}
            actionTotal={actionTotal}
            onActionTotalChange={setActionTotal}
            onCreate={() => {
              if (!selectedProfile) return;
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
            onJoin={() => {
              if (!selectedProfile) return;
              void runControllerAction("Join table", async () =>
                await joinLocalTable(selectedProfile, joinInviteCode, Number(joinBuyIn)),
              );
            }}
            onAnnounce={() => {
              if (!selectedProfile || !selectedLocalTableId) return;
              void runControllerAction("Announce table", async () =>
                await announceLocalTable(selectedProfile, selectedLocalTableId),
              );
            }}
            onRotateHost={() => {
              if (!selectedProfile || !selectedLocalTableId) return;
              void runControllerAction("Rotate host", async () =>
                await rotateLocalHost(selectedProfile, selectedLocalTableId),
              );
            }}
            onSubmitAction={(actionType, totalSats) => {
              if (!selectedProfile || !selectedLocalTableId) return;
              const payload =
                totalSats !== undefined
                  ? { totalSats, type: actionType }
                  : { type: actionType };
              void runControllerAction(`Submit ${actionType}`, async () =>
                await submitLocalTableAction(selectedProfile, selectedLocalTableId, payload),
              );
            }}
          />
        );
      case "logs":
        return <LogsView localLogs={localLogs} />;
      default:
        return <OverviewView
          profileStatus={profileStatus}
          controllerConnected={controllerConnected}
          spectatorModeOnly={spectatorModeOnly}
          publicTableCount={publicTables.length}
          localTableCount={localTableSummaries.length}
          peerCount={peers.length}
          availableSats={localWallet?.availableSats ?? 0}
          localLogs={localLogs}
        />;
    }
  };

  return (
    <div className="min-h-screen bg-background">
      <Header activeTab={activeTab} onTabChange={setActiveTab} />

      <main className="p-6">
        {renderView()}
      </main>

      <footer className="mt-6 pb-6 text-center text-xs text-muted-foreground">
        <div className="flex items-center justify-center gap-4">
          <span>{publicStatus}</span>
          <span>&bull;</span>
          <span>{controllerStatus}</span>
        </div>
      </footer>
    </div>
  );
}
