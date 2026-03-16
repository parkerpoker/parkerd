import { useEffect, useRef, useState, type ChangeEvent } from "react";

import {
  buildCommitmentHash,
  createHoldemHand,
  deriveDeckSeed,
} from "@parker/game-engine";
import type {
  ClientSocketEvent,
  ServerSocketEvent,
  SignedActionPayload,
  SwapJobStatus,
  SwapQuote,
  TableSnapshot,
} from "@parker/protocol";
import { randomHex, type LocalIdentity, type WalletSummary } from "@parker/settlement";

import { PokerCard } from "./components/PokerCard.js";
import { useTableSocket } from "./hooks/useTableSocket.js";
import {
  WEBSOCKET_URL,
  createTable,
  getTable,
  getTableByInvite,
  joinTable,
  postCommitment,
  postDelegation,
} from "./lib/api.js";
import { WalletWorkerClient } from "./lib/walletClient.js";

const MOCK_MODE = import.meta.env.VITE_USE_MOCK_SETTLEMENT !== "false";

function formatSats(amount?: number) {
  return `${amount ?? 0} sats`;
}

function createPlayerProfile(identity: LocalIdentity, wallet: WalletSummary, nickname: string) {
  return {
    playerId: identity.playerId,
    nickname,
    joinedAt: new Date().toISOString(),
    pubkeyHex: identity.publicKeyHex,
    arkAddress: wallet.arkAddress,
    boardingAddress: wallet.boardingAddress,
  };
}

function seedStorageKey(tableId: string, handNumber: number) {
  return `parker:${tableId}:hand:${handNumber}:seed`;
}

function deriveLocalHoleCards(snapshot: TableSnapshot, playerId: string, seatIndex: number) {
  const checkpoint = snapshot.checkpoint;
  if (!checkpoint) {
    return ["XX", "XX"] as const;
  }

  if (checkpoint.phase === "settled" && checkpoint.holeCardsByPlayerId[playerId]) {
    return checkpoint.holeCardsByPlayerId[playerId]!;
  }

  const reveals = snapshot.commitments.filter((commitment) => commitment.revealSeed);
  if (reveals.length !== 2) {
    return ["XX", "XX"] as const;
  }

  const deckSeedHex = deriveDeckSeed({
    tableId: snapshot.table.tableId,
    handNumber: checkpoint.handNumber,
    commitments: snapshot.commitments,
    reveals,
  });
  const hand = createHoldemHand({
    handId: checkpoint.handId,
    handNumber: checkpoint.handNumber,
    deckSeedHex,
    dealerSeatIndex: checkpoint.dealerSeatIndex as 0 | 1,
    smallBlindSats: snapshot.table.smallBlindSats,
    bigBlindSats: snapshot.table.bigBlindSats,
    seats: [
      {
        playerId: snapshot.seats[0]!.player.playerId,
        stackSats:
          checkpoint.playerStacks[snapshot.seats[0]!.player.playerId] ?? snapshot.seats[0]!.buyInSats,
      },
      {
        playerId: snapshot.seats[1]!.player.playerId,
        stackSats:
          checkpoint.playerStacks[snapshot.seats[1]!.player.playerId] ?? snapshot.seats[1]!.buyInSats,
      },
    ],
  });

  return hand.players[seatIndex]!.holeCards;
}

export function App() {
  const [walletClient, setWalletClient] = useState<WalletWorkerClient | null>(null);
  const [identity, setIdentity] = useState<LocalIdentity | null>(null);
  const [wallet, setWallet] = useState<WalletSummary | null>(null);
  const [nickname, setNickname] = useState("River Runner");
  const [tableSnapshot, setTableSnapshot] = useState<TableSnapshot | null>(null);
  const [inviteCode, setInviteCode] = useState("");
  const [depositAmount, setDepositAmount] = useState(5_000);
  const [withdrawAmount, setWithdrawAmount] = useState(2_000);
  const [withdrawInvoice, setWithdrawInvoice] = useState("lnmockinvoice");
  const [betInput, setBetInput] = useState(200);
  const [swapQuote, setSwapQuote] = useState<SwapQuote | null>(null);
  const [withdrawalStatus, setWithdrawalStatus] = useState<SwapJobStatus | null>(null);
  const [statusMessage, setStatusMessage] = useState("Bootstrapping Parker wallet...");
  const [peerMessages, setPeerMessages] = useState<string[]>([]);
  const [peerStatus, setPeerStatus] = useState("offline");
  const autoDelegationCheckpointRef = useRef<string | null>(null);

  useEffect(() => {
    void WalletWorkerClient.create()
      .then(async (client) => {
        setWalletClient(client);
        const bootstrap = await client.bootstrap(nickname);
        setIdentity(bootstrap.identity);
        setWallet(bootstrap.wallet);
        setStatusMessage("Wallet ready on Mutinynet MVP.");
      })
      .catch((error: Error) => {
        setStatusMessage(error.message);
      });
  }, []);

  const seat = tableSnapshot?.seats.find((candidate) => candidate.player.playerId === identity?.playerId);
  const opponentSeat = tableSnapshot?.seats.find((candidate) => candidate.player.playerId !== identity?.playerId);
  const holeCards =
    tableSnapshot && identity && seat ? deriveLocalHoleCards(tableSnapshot, identity.playerId, seat.seatIndex) : ["XX", "XX"];
  const checkpoint = tableSnapshot?.checkpoint;
  const myContribution = checkpoint && identity ? checkpoint.roundContributions[identity.playerId] ?? 0 : 0;
  const toCall = checkpoint ? Math.max(0, checkpoint.currentBetSats - myContribution) : 0;

  const { connected, sendEvent } = useTableSocket({
    wsUrl: WEBSOCKET_URL,
    tableId: tableSnapshot?.table.tableId,
    playerId: identity?.playerId,
    onEvent: (event: ServerSocketEvent) => {
      if (event.type === "table-snapshot") {
        setTableSnapshot(event.snapshot);
        if (identity) {
          const remoteSeat = event.snapshot.seats.find((candidate) => candidate.player.playerId !== identity.playerId);
          if (!remoteSeat || remoteSeat.player.playerId === "open-seat") {
            setPeerStatus("offline");
          }
        }
      }
      if (event.type === "checkpoint" && tableSnapshot) {
        setTableSnapshot((previous) =>
          previous
            ? {
                ...previous,
                checkpoint: event.checkpoint,
              }
            : previous,
        );
      }
      if (event.type === "peer-message") {
        setPeerMessages((messages) => [event.message, ...messages].slice(0, 12));
      }
      if (event.type === "presence") {
        if (event.playerId !== identity?.playerId) {
          setPeerStatus(event.status === "online" ? "relay" : "offline");
        }
        setStatusMessage(`${event.playerId} is ${event.status}`);
      }
      if (event.type === "error") {
        setStatusMessage(event.message);
      }
    },
  });

  useEffect(() => {
    if (
      !walletClient ||
      !tableSnapshot ||
      !identity ||
      !seat ||
      !checkpoint ||
      checkpoint.actingSeatIndex !== seat.seatIndex ||
      checkpoint.phase === "settled" ||
      autoDelegationCheckpointRef.current === checkpoint.checkpointId
    ) {
      return;
    }

    autoDelegationCheckpointRef.current = checkpoint.checkpointId;
    void walletClient
      .createTimeoutDelegation({
        tableId: tableSnapshot.table.tableId,
        checkpointId: checkpoint.checkpointId,
        actingSeatIndex: seat.seatIndex,
        delegatedPlayerId: identity.playerId,
        settlementAddress: wallet?.arkAddress ?? "tark1fallback",
        validAfter: checkpoint.nextActionDeadline ?? new Date().toISOString(),
        expiresAt: new Date(
          Date.parse(checkpoint.nextActionDeadline ?? new Date().toISOString()) + 60_000,
        ).toISOString(),
      })
      .then((delegation) => postDelegation(tableSnapshot.table.tableId, delegation))
      .then(setTableSnapshot)
      .catch((error: Error) => {
        setStatusMessage(error.message);
      });
  }, [checkpoint, identity, seat, tableSnapshot, wallet, walletClient]);

  async function refreshWallet() {
    if (!walletClient) {
      return;
    }
    setWallet(await walletClient.getWallet());
  }

  async function handleCreateTable() {
    if (!identity || !wallet) {
      return;
    }

    const created = await createTable({
      host: createPlayerProfile(identity, wallet, nickname),
      smallBlindSats: 50,
      bigBlindSats: 100,
      buyInMinSats: 4_000,
      buyInMaxSats: 8_000,
      commitmentDeadlineSeconds: 20,
      actionTimeoutSeconds: 25,
    });
    const snapshot = await getTable(created.table.tableId);
    setTableSnapshot(snapshot);
    setInviteCode(created.table.inviteCode);
    setStatusMessage(`Private table ${created.table.inviteCode} opened.`);
  }

  async function handleJoinTable() {
    if (!identity || !wallet || !inviteCode) {
      return;
    }

    await joinTable({
      inviteCode,
      player: createPlayerProfile(identity, wallet, nickname),
      buyInSats: 4_000,
    });
    const snapshot = await getTableByInvite(inviteCode);
    setTableSnapshot(snapshot);
    setStatusMessage(`Joined table ${inviteCode}.`);
  }

  async function handleCommitSeed(reveal = false) {
    if (!tableSnapshot || !identity || !seat) {
      return;
    }

    const handNumber = checkpoint?.handNumber ?? 1;
    const key = seedStorageKey(tableSnapshot.table.tableId, handNumber);
    const existingSeed = localStorage.getItem(key) ?? randomHex(32);
    localStorage.setItem(key, existingSeed);

    const commitmentHash = buildCommitmentHash({
      tableId: tableSnapshot.table.tableId,
      seatIndex: seat.seatIndex,
      playerId: identity.playerId,
      seedHex: existingSeed,
    });

    const snapshot = await postCommitment(tableSnapshot.table.tableId, {
      tableId: tableSnapshot.table.tableId,
      handNumber,
      seatIndex: seat.seatIndex,
      playerId: identity.playerId,
      commitmentHash,
      revealSeed: reveal ? existingSeed : undefined,
      revealedAt: reveal ? new Date().toISOString() : undefined,
    });
    setTableSnapshot(snapshot);
    setStatusMessage(reveal ? "Seed revealed." : "Seed committed.");
  }

  async function signAndSendAction(payload: SignedActionPayload) {
    if (!walletClient || !identity || !seat || !tableSnapshot?.checkpoint) {
      return;
    }

    const unsignedAction = {
      tableId: tableSnapshot.table.tableId,
      handId: tableSnapshot.checkpoint.handId,
      checkpointId: tableSnapshot.checkpoint.checkpointId,
      clientSeq: Date.now(),
      actorPlayerId: identity.playerId,
      actorSeatIndex: seat.seatIndex,
      sentAt: new Date().toISOString(),
      signerPubkeyHex: identity.publicKeyHex,
      payload,
    };
    const signatureHex = await walletClient.signMessage(JSON.stringify(unsignedAction));

    const action = {
      ...unsignedAction,
      signatureHex,
    };
    sendEvent({
      type: "signed-action",
      action,
    });
    sendPeerMessage(JSON.stringify({ type: "mirror-action", action }));
  }

  function sendPeerMessage(message: string) {
    if (!tableSnapshot || !identity || !opponentSeat) {
      return;
    }
    sendEvent({
      type: "peer-message",
      tableId: tableSnapshot.table.tableId,
      fromPlayerId: identity.playerId,
      targetPlayerId: opponentSeat.player.playerId,
      message,
    } satisfies ClientSocketEvent);
  }

  async function handleDeposit() {
    if (!walletClient) {
      return;
    }
    const quote = await walletClient.createDepositQuote(depositAmount);
    setSwapQuote(quote);
    await refreshWallet();
  }

  async function handleWithdrawal() {
    if (!walletClient) {
      return;
    }
    const status = await walletClient.submitWithdrawal(withdrawAmount, withdrawInvoice);
    setWithdrawalStatus(status);
    await refreshWallet();
  }

  const myTurn = Boolean(checkpoint && seat && checkpoint.actingSeatIndex === seat.seatIndex);
  const allCommitted = tableSnapshot?.commitments.length === 2;
  const allRevealed =
    tableSnapshot?.commitments.length === 2 &&
    tableSnapshot.commitments.every((commitment) => Boolean(commitment.revealSeed));

  return (
    <main className="shell">
      <section className="hero">
        <p className="eyebrow">Parker / Private tables / Mutinynet MVP</p>
        <h1>Peer-to-peer poker with self-custodial Arkade rails.</h1>
        <p className="hero-copy">
          Deterministic dealing, no server custody, Lightning in and out, and a watchtower-backed
          table escrow model for heads-up Hold&apos;em.
        </p>
        <div className="status-row">
          <span className="pill">{MOCK_MODE ? "Mock settlement mode" : "Live Arkade mode"}</span>
          <span className="pill">{connected ? "WebSocket live" : "WebSocket idle"}</span>
          <span className="pill">Relay: {peerStatus}</span>
        </div>
      </section>

      <section className="layout">
        <article className="panel wallet-panel">
          <h2>Wallet</h2>
          <label className="field">
            <span>Nickname</span>
                <input
                  value={nickname}
                  onChange={(event: ChangeEvent<HTMLInputElement>) => setNickname(event.target.value)}
                />
          </label>
          <div className="wallet-balance">{formatSats(wallet?.availableSats)}</div>
          <p className="muted">Ark address: {wallet?.arkAddress ?? "booting..."}</p>
          <p className="muted">Boarding: {wallet?.boardingAddress ?? "booting..."}</p>
          <div className="split">
            <label className="field">
              <span>Deposit sats</span>
              <input
                type="number"
                value={depositAmount}
                    onChange={(event: ChangeEvent<HTMLInputElement>) =>
                      setDepositAmount(Number(event.target.value))
                    }
              />
            </label>
            <button onClick={() => void handleDeposit()}>Create LN deposit</button>
          </div>
          <div className="split">
            <label className="field">
              <span>Withdraw sats</span>
              <input
                type="number"
                value={withdrawAmount}
                    onChange={(event: ChangeEvent<HTMLInputElement>) =>
                      setWithdrawAmount(Number(event.target.value))
                    }
              />
            </label>
            <label className="field">
              <span>Lightning invoice</span>
              <input
                value={withdrawInvoice}
                    onChange={(event: ChangeEvent<HTMLInputElement>) =>
                      setWithdrawInvoice(event.target.value)
                    }
              />
            </label>
          </div>
          <button onClick={() => void handleWithdrawal()}>Run withdrawal</button>
          {swapQuote ? (
            <div className="note">
              <strong>Deposit invoice</strong>
              <code>{swapQuote.invoice}</code>
            </div>
          ) : null}
          {withdrawalStatus ? (
            <div className="note">
              <strong>Withdrawal</strong>
              <p>{withdrawalStatus.details}</p>
            </div>
          ) : null}
        </article>

        <article className="panel control-panel">
          <h2>Private Table</h2>
          <div className="button-row">
            <button onClick={() => void handleCreateTable()}>Create table</button>
            <input
              placeholder="Invite code"
              value={inviteCode}
              onChange={(event: ChangeEvent<HTMLInputElement>) =>
                setInviteCode(event.target.value.toUpperCase())
              }
            />
            <button onClick={() => void handleJoinTable()}>Join table</button>
          </div>
          <p className="muted">{statusMessage}</p>
          {tableSnapshot ? (
            <>
              <div className="table-meta">
                <div>
                  <span>Invite</span>
                  <strong>{tableSnapshot.table.inviteCode}</strong>
                </div>
                <div>
                  <span>Status</span>
                  <strong>{tableSnapshot.table.status}</strong>
                </div>
                <div>
                  <span>Escrow</span>
                  <strong>{tableSnapshot.escrow?.contractAddress ?? "pending"}</strong>
                </div>
              </div>
              <div className="seat-list">
                {tableSnapshot.seats.map((candidate) => (
                  <div key={candidate.seatIndex} className="seat-chip">
                    <strong>Seat {candidate.seatIndex + 1}</strong>
                    <span>{candidate.player.nickname}</span>
                    <small>{formatSats(candidate.buyInSats)}</small>
                  </div>
                ))}
              </div>
              <div className="button-row">
                <button disabled={!seat} onClick={() => void handleCommitSeed(false)}>
                  Commit seed
                </button>
                <button disabled={!allCommitted} onClick={() => void handleCommitSeed(true)}>
                  Reveal seed
                </button>
              </div>
              <p className="muted">
                Commitments: {tableSnapshot.commitments.length}/2. Reveals:{" "}
                {tableSnapshot.commitments.filter((commitment) => commitment.revealSeed).length}/2.
              </p>
              <p className="muted">Relayed peer messages: {peerMessages.length}</p>
            </>
          ) : (
            <p className="muted">Create or join a table to begin the commitment flow.</p>
          )}
        </article>
      </section>

      <section className="panel table-panel">
        <header className="table-header">
          <div>
            <p className="eyebrow">Heads-up no-limit Hold&apos;em</p>
            <h2>Table Escrow</h2>
          </div>
          <div className="countdown">
            {checkpoint?.nextActionDeadline
              ? `Action clock: ${new Date(checkpoint.nextActionDeadline).toLocaleTimeString()}`
              : "Waiting for commitments"}
          </div>
        </header>
        <div className="board-row">
          {checkpoint?.board.length ? (
            checkpoint.board.map((card) => <PokerCard key={card} code={card} />)
          ) : (
            <>
              <PokerCard concealed />
              <PokerCard concealed />
              <PokerCard concealed />
              <PokerCard concealed />
              <PokerCard concealed />
            </>
          )}
        </div>
        <div className="hand-row">
          <div className="hand-panel">
            <h3>Your hand</h3>
            <div className="card-row">
              {holeCards.map((card) => (
                <PokerCard key={card} code={card} concealed={card === "XX"} />
              ))}
            </div>
          </div>
          <div className="hand-panel">
            <h3>Opponent</h3>
            <div className="card-row">
              {(checkpoint?.phase === "settled"
                ? checkpoint.holeCardsByPlayerId[opponentSeat?.player.playerId ?? ""] ?? ["XX", "XX"]
                : ["XX", "XX"]
              ).map((card) => (
                <PokerCard key={card} code={card} concealed={card === "XX"} />
              ))}
            </div>
          </div>
        </div>
        <div className="table-meta game-state">
          <div>
            <span>Pot</span>
            <strong>{formatSats(checkpoint?.potSats)}</strong>
          </div>
          <div>
            <span>Current bet</span>
            <strong>{formatSats(checkpoint?.currentBetSats)}</strong>
          </div>
          <div>
            <span>Transcript</span>
            <strong>{checkpoint?.transcriptHash.slice(0, 12) ?? "pending"}</strong>
          </div>
        </div>
        <div className="seat-list">
          {tableSnapshot?.seats.map((candidate) => (
            <div key={candidate.seatIndex} className="seat-chip wide">
              <strong>{candidate.player.nickname}</strong>
              <span>
                Stack:{" "}
                {formatSats(
                  checkpoint?.playerStacks[candidate.player.playerId] ?? candidate.buyInSats,
                )}
              </span>
              <small>
                Round:{" "}
                {formatSats(checkpoint?.roundContributions[candidate.player.playerId] ?? 0)}
              </small>
            </div>
          ))}
        </div>

        {myTurn && allRevealed ? (
          <div className="action-panel">
            <h3>Your action</h3>
            <div className="button-row">
              {toCall > 0 ? (
                <>
                  <button onClick={() => void signAndSendAction({ type: "fold" })}>Fold</button>
                  <button onClick={() => void signAndSendAction({ type: "call" })}>
                    Call {formatSats(toCall)}
                  </button>
                  <input
                    type="number"
                    value={betInput}
                    onChange={(event: ChangeEvent<HTMLInputElement>) =>
                      setBetInput(Number(event.target.value))
                    }
                  />
                  <button onClick={() => void signAndSendAction({ type: "raise", totalSats: betInput })}>
                    Raise to {formatSats(betInput)}
                  </button>
                </>
              ) : (
                <>
                  <button onClick={() => void signAndSendAction({ type: "check" })}>Check</button>
                  <input
                    type="number"
                    value={betInput}
                    onChange={(event: ChangeEvent<HTMLInputElement>) =>
                      setBetInput(Number(event.target.value))
                    }
                  />
                  <button onClick={() => void signAndSendAction({ type: "bet", totalSats: betInput })}>
                    Bet {formatSats(betInput)}
                  </button>
                </>
              )}
            </div>
          </div>
        ) : (
          <p className="muted table-waiting">
            {allRevealed
              ? "Waiting for the acting seat."
              : "Both players must commit and reveal seeds before the first deal."}
          </p>
        )}
      </section>
    </main>
  );
}
