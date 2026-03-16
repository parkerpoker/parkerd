import { useEffect, useRef, useState } from "react";

import type { SignalPayload } from "@parker/protocol";

interface UsePeerConnectionArgs {
  enabled: boolean;
  isOfferer: boolean;
  localPlayerId: string | undefined;
  remotePlayerId: string | undefined;
  sendSignal: (payload: SignalPayload) => void;
}

function normalizeSignalPayload(
  description?: RTCSessionDescriptionInit | null,
  candidate?: RTCIceCandidate | null,
): SignalPayload {
  if (description) {
    return {
      description: JSON.stringify(description),
    };
  }

  return {
    candidate: candidate?.candidate,
    sdpMid: candidate?.sdpMid ?? undefined,
    sdpMLineIndex: candidate?.sdpMLineIndex ?? undefined,
  };
}

export function usePeerConnection({
  enabled,
  isOfferer,
  sendSignal,
}: UsePeerConnectionArgs) {
  const peerRef = useRef<RTCPeerConnection | null>(null);
  const channelRef = useRef<RTCDataChannel | null>(null);
  const [status, setStatus] = useState("offline");
  const [peerMessages, setPeerMessages] = useState<string[]>([]);

  function attachDataChannel(channel: RTCDataChannel) {
    channelRef.current = channel;
    channel.onopen = () => setStatus("direct");
    channel.onclose = () => setStatus("offline");
    channel.onmessage = (event) => {
      setPeerMessages((messages: string[]) => [event.data, ...messages].slice(0, 12));
    };
  }

  async function ensureConnection() {
    if (peerRef.current) {
      return peerRef.current;
    }

    const peer = new RTCPeerConnection({
      iceServers: [{ urls: "stun:stun.l.google.com:19302" }],
    });
    peerRef.current = peer;
    setStatus("signaling");

    peer.onicecandidate = (event) => {
      if (event.candidate) {
        sendSignal(normalizeSignalPayload(null, event.candidate));
      }
    };
    peer.onconnectionstatechange = () => {
      setStatus(peer.connectionState);
    };
    peer.ondatachannel = (event) => {
      attachDataChannel(event.channel);
    };

    if (isOfferer) {
      attachDataChannel(peer.createDataChannel("parker"));
      const offer = await peer.createOffer();
      await peer.setLocalDescription(offer);
      sendSignal(normalizeSignalPayload(offer, null));
    }

    return peer;
  }

  async function handleSignal(signal: SignalPayload) {
    if (!enabled) {
      return;
    }

    const peer = await ensureConnection();
    if (signal.description) {
      const description = JSON.parse(signal.description) as RTCSessionDescriptionInit;
      await peer.setRemoteDescription(description);
      if (description.type === "offer") {
        const answer = await peer.createAnswer();
        await peer.setLocalDescription(answer);
        sendSignal(normalizeSignalPayload(answer, null));
      }
      return;
    }

    if (signal.candidate) {
      const candidateInit: RTCIceCandidateInit = {
        candidate: signal.candidate,
      };
      if (signal.sdpMid !== undefined) {
        candidateInit.sdpMid = signal.sdpMid;
      }
      if (signal.sdpMLineIndex !== undefined) {
        candidateInit.sdpMLineIndex = signal.sdpMLineIndex;
      }
      await peer.addIceCandidate(new RTCIceCandidate(candidateInit));
    }
  }

  function sendPeerMessage(message: string) {
    if (channelRef.current?.readyState === "open") {
      channelRef.current.send(message);
    }
  }

  useEffect(() => {
    if (!enabled) {
      peerRef.current?.close();
      peerRef.current = null;
      channelRef.current = null;
      setStatus("offline");
      return;
    }

    void ensureConnection();

    return () => {
      peerRef.current?.close();
      peerRef.current = null;
      channelRef.current = null;
      setStatus("offline");
    };
  }, [enabled, isOfferer]);

  return {
    status,
    peerMessages,
    handleSignal,
    sendPeerMessage,
  };
}
