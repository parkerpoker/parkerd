package meshruntime

import (
	"bufio"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/parkerpoker/parkerd/internal/settlementcore"
	"github.com/parkerpoker/parkerd/internal/tablecustody"
	transportpkg "github.com/parkerpoker/parkerd/internal/transport"
)

const (
	TransportWireVersion = 2

	nativeTransportChannelDiscovery = "discovery"
	nativeTransportChannelTable     = "table"
	nativeTransportChannelSync      = "sync"

	nativeTransportEncryptionNone                         = "none"
	nativeTransportEncryptionX25519GCM                    = "x25519-aes256gcm"
	nativeTransportMessagePeerManifest                    = "peer.manifest"
	nativeTransportMessagePeerProbe                       = "peer.manifest.get"
	nativeTransportMessageTablePull                       = "table.state.pull"
	nativeTransportMessageTablePush                       = "table.state.push"
	nativeTransportMessageTableJoinReq                    = "table.join.request"
	nativeTransportMessageTableJoinResp                   = "table.join.response"
	nativeTransportMessageTableActReq                     = "table.action.request"
	nativeTransportMessageTableActResp                    = "table.action.response"
	nativeTransportMessageTableFundsReq                   = "table.funds.request"
	nativeTransportMessageTableFundsResp                  = "table.funds.response"
	nativeTransportMessageTableCustodyReq                 = "table.custody.request"
	nativeTransportMessageTableCustodyResp                = "table.custody.response"
	nativeTransportMessageTableCustodySignReq             = "table.custody.sign.request"
	nativeTransportMessageTableCustodySignResp            = "table.custody.sign.response"
	nativeTransportMessageTableCustodySignerPrepareReq    = "table.custody.signer.prepare.request"
	nativeTransportMessageTableCustodySignerPrepareResp   = "table.custody.signer.prepare.response"
	nativeTransportMessageTableCustodySignerStartReq      = "table.custody.signer.start.request"
	nativeTransportMessageTableCustodySignerStartResp     = "table.custody.signer.start.response"
	nativeTransportMessageTableCustodySignerNoncesReq     = "table.custody.signer.nonces.request"
	nativeTransportMessageTableCustodySignerNoncesResp    = "table.custody.signer.nonces.response"
	nativeTransportMessageTableCustodySignerAggNoncesReq  = "table.custody.signer.aggregated_nonces.request"
	nativeTransportMessageTableCustodySignerAggNoncesResp = "table.custody.signer.aggregated_nonces.response"
	nativeTransportMessageTableHandReq                    = "table.hand.request"
	nativeTransportMessageTableHandResp                   = "table.hand.response"
	nativeTransportMessageAck                             = "ack"
	nativeTransportMessageNack                            = "nack"

	nativeTransportRequestTTL      = 30 * time.Second
	nativeTransportReadTimeout     = 10 * time.Second
	nativeTransportWriteTimeout    = 20 * time.Second
	nativeTransportExchangeTimeout = 20 * time.Second
	nativePeerInfoCacheTTL         = 30 * time.Second
)

type nativeTransportError struct {
	Error string `json:"error"`
}

func (runtime *meshRuntime) servePeerTransport(listener net.Listener) {
	for {
		connection, err := listener.Accept()
		if err != nil {
			runtime.mu.Lock()
			stopped := runtime.listener == nil || runtime.listener != listener
			runtime.mu.Unlock()
			if stopped {
				return
			}
			continue
		}
		if !runtime.beginBackgroundTask() {
			_ = connection.Close()
			continue
		}
		go func() {
			defer runtime.endBackgroundTask()
			runtime.handlePeerTransportConnection(connection)
		}()
	}
}

func (runtime *meshRuntime) handlePeerTransportConnection(connection net.Conn) {
	defer connection.Close()
	_ = connection.SetReadDeadline(time.Now().Add(nativeTransportReadTimeout))

	reader := bufio.NewReader(connection)
	line, err := readTrimmedLine(reader)
	if err != nil || line == "" {
		return
	}
	_ = connection.SetReadDeadline(time.Time{})

	var request transportpkg.TransportEnvelope
	if err := json.Unmarshal([]byte(line), &request); err != nil {
		_ = connection.SetWriteDeadline(time.Now().Add(nativeTransportWriteTimeout))
		_ = writeJSONLine(connection, runtime.plainNack("", err))
		return
	}

	response, err := runtime.handlePeerTransportEnvelope(request)
	if err != nil {
		response = runtime.nackFromRequest(request, err)
	}
	_ = connection.SetWriteDeadline(time.Now().Add(nativeTransportWriteTimeout))
	_ = writeJSONLine(connection, response)
}

func (runtime *meshRuntime) handlePeerTransportEnvelope(request transportpkg.TransportEnvelope) (response transportpkg.TransportEnvelope, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			response = transportpkg.TransportEnvelope{}
			err = fmt.Errorf("transport handler panic: %v", recovered)
		}
		if err != nil {
			debugMeshf("transport request failed type=%s table=%s sender=%s error=%v", request.MessageType, request.TableID, request.SenderPeerID, err)
		}
	}()

	body, sharedSecret, err := runtime.decodeIncomingEnvelope(request)
	if err != nil {
		return transportpkg.TransportEnvelope{}, err
	}

	switch request.MessageType {
	case nativeTransportMessagePeerProbe:
		return runtime.encodeResponseEnvelope(request, nativeTransportMessagePeerManifest, nativeTransportChannelDiscovery, nil, runtime.self)
	case nativeTransportMessageTablePull:
		var fetch nativeTableFetchRequest
		if err := json.Unmarshal(body, &fetch); err != nil {
			return transportpkg.TransportEnvelope{}, err
		}
		debugMeshf("table pull request table=%s sender=%s player=%s", fetch.TableID, request.SenderPeerID, fetch.PlayerID)
		table, err := runtime.store.readTable(fetch.TableID)
		if err != nil || table == nil {
			return transportpkg.TransportEnvelope{}, fmt.Errorf("table %s not found", fetch.TableID)
		}
		viewerPlayerID := runtime.tableViewerPlayerIDFromFetch(fetch, *table)
		return runtime.encodeResponseEnvelope(request, nativeTransportMessageTablePush, nativeTransportChannelTable, sharedSecret, runtime.networkTableView(*table, viewerPlayerID))
	case nativeTransportMessageTableJoinReq:
		var join nativeJoinRequest
		if err := json.Unmarshal(body, &join); err != nil {
			return transportpkg.TransportEnvelope{}, err
		}
		debugMeshf("table join request table=%s sender=%s player=%s peer_url=%s", join.TableID, request.SenderPeerID, join.WalletPlayerID, join.Peer.PeerURL)
		table, err := runtime.handleJoinFromPeer(join)
		if err != nil {
			return transportpkg.TransportEnvelope{}, err
		}
		return runtime.encodeResponseEnvelope(request, nativeTransportMessageTableJoinResp, nativeTransportChannelTable, sharedSecret, runtime.networkTableView(table, join.WalletPlayerID))
	case nativeTransportMessageTableActReq:
		var action nativeActionRequest
		if err := json.Unmarshal(body, &action); err != nil {
			return transportpkg.TransportEnvelope{}, err
		}
		table, err := runtime.handleActionFromPeer(action)
		if err != nil {
			return transportpkg.TransportEnvelope{}, err
		}
		return runtime.encodeResponseEnvelope(request, nativeTransportMessageTableActResp, nativeTransportChannelTable, sharedSecret, runtime.networkTableView(table, action.PlayerID))
	case nativeTransportMessageTableFundsReq:
		var fundsRequest nativeFundsRequest
		if err := json.Unmarshal(body, &fundsRequest); err != nil {
			return transportpkg.TransportEnvelope{}, err
		}
		response, err := runtime.handleFundsFromPeer(fundsRequest)
		if err != nil {
			return transportpkg.TransportEnvelope{}, err
		}
		return runtime.encodeResponseEnvelope(request, nativeTransportMessageTableFundsResp, nativeTransportChannelTable, sharedSecret, response)
	case nativeTransportMessageTableHandReq:
		var handMessage nativeHandMessageRequest
		if err := json.Unmarshal(body, &handMessage); err != nil {
			return transportpkg.TransportEnvelope{}, err
		}
		table, err := runtime.handleHandMessageFromPeer(handMessage)
		if err != nil {
			return transportpkg.TransportEnvelope{}, err
		}
		return runtime.encodeResponseEnvelope(request, nativeTransportMessageTableHandResp, nativeTransportChannelTable, sharedSecret, runtime.networkTableView(table, handMessage.PlayerID))
	case nativeTransportMessageTableCustodyReq:
		var approvalRequest nativeCustodyApprovalRequest
		if err := json.Unmarshal(body, &approvalRequest); err != nil {
			return transportpkg.TransportEnvelope{}, err
		}
		response, err := runtime.handleCustodyApprovalFromPeer(approvalRequest)
		if err != nil {
			return transportpkg.TransportEnvelope{}, err
		}
		return runtime.encodeResponseEnvelope(request, nativeTransportMessageTableCustodyResp, nativeTransportChannelTable, sharedSecret, response)
	case nativeTransportMessageTableCustodySignReq:
		var signRequest nativeCustodyTxSignRequest
		if err := json.Unmarshal(body, &signRequest); err != nil {
			return transportpkg.TransportEnvelope{}, err
		}
		response, err := runtime.handleCustodyTxSignFromPeer(signRequest)
		if err != nil {
			return transportpkg.TransportEnvelope{}, err
		}
		return runtime.encodeResponseEnvelope(request, nativeTransportMessageTableCustodySignResp, nativeTransportChannelTable, sharedSecret, response)
	case nativeTransportMessageTableCustodySignerPrepareReq:
		var prepareRequest nativeCustodySignerPrepareRequest
		if err := json.Unmarshal(body, &prepareRequest); err != nil {
			return transportpkg.TransportEnvelope{}, err
		}
		response, err := runtime.handleCustodySignerPrepareFromPeer(prepareRequest)
		if err != nil {
			return transportpkg.TransportEnvelope{}, err
		}
		return runtime.encodeResponseEnvelope(request, nativeTransportMessageTableCustodySignerPrepareResp, nativeTransportChannelTable, sharedSecret, response)
	case nativeTransportMessageTableCustodySignerStartReq:
		var startRequest nativeCustodySignerStartRequest
		if err := json.Unmarshal(body, &startRequest); err != nil {
			return transportpkg.TransportEnvelope{}, err
		}
		response, err := runtime.handleCustodySignerStartFromPeer(startRequest)
		if err != nil {
			return transportpkg.TransportEnvelope{}, err
		}
		return runtime.encodeResponseEnvelope(request, nativeTransportMessageTableCustodySignerStartResp, nativeTransportChannelTable, sharedSecret, response)
	case nativeTransportMessageTableCustodySignerNoncesReq:
		var noncesRequest nativeCustodySignerNoncesRequest
		if err := json.Unmarshal(body, &noncesRequest); err != nil {
			return transportpkg.TransportEnvelope{}, err
		}
		response, err := runtime.handleCustodySignerNoncesFromPeer(noncesRequest)
		if err != nil {
			return transportpkg.TransportEnvelope{}, err
		}
		return runtime.encodeResponseEnvelope(request, nativeTransportMessageTableCustodySignerNoncesResp, nativeTransportChannelTable, sharedSecret, response)
	case nativeTransportMessageTableCustodySignerAggNoncesReq:
		var noncesRequest nativeCustodySignerAggregatedNoncesRequest
		if err := json.Unmarshal(body, &noncesRequest); err != nil {
			return transportpkg.TransportEnvelope{}, err
		}
		response, err := runtime.handleCustodySignerAggregatedNoncesFromPeer(noncesRequest)
		if err != nil {
			return transportpkg.TransportEnvelope{}, err
		}
		return runtime.encodeResponseEnvelope(request, nativeTransportMessageTableCustodySignerAggNoncesResp, nativeTransportChannelTable, sharedSecret, response)
	case nativeTransportMessageTablePush:
		var syncRequest nativeTableSyncRequest
		if err := json.Unmarshal(body, &syncRequest); err != nil {
			return transportpkg.TransportEnvelope{}, err
		}
		if err := runtime.acceptTableSync(syncRequest); err != nil {
			return transportpkg.TransportEnvelope{}, err
		}
		return runtime.encodeResponseEnvelope(request, nativeTransportMessageAck, nativeTransportChannelSync, sharedSecret, map[string]any{"ok": true})
	default:
		return transportpkg.TransportEnvelope{}, fmt.Errorf("unsupported transport message type %q", request.MessageType)
	}
}

func (runtime *meshRuntime) exchangePeerTransport(peerURL string, request transportpkg.TransportEnvelope) (transportpkg.TransportEnvelope, error) {
	if !runtime.isRunning() {
		return transportpkg.TransportEnvelope{}, errors.New("runtime is closed")
	}
	connection, err := runtime.dialPeerTransport(peerURL)
	if err != nil {
		return transportpkg.TransportEnvelope{}, err
	}
	defer connection.Close()
	if !runtime.isRunning() {
		return transportpkg.TransportEnvelope{}, errors.New("runtime is closed")
	}

	_ = connection.SetDeadline(time.Now().Add(nativeTransportExchangeTimeout))
	if err := writeJSONLine(connection, request); err != nil {
		return transportpkg.TransportEnvelope{}, err
	}
	if !runtime.isRunning() {
		return transportpkg.TransportEnvelope{}, errors.New("runtime is closed")
	}

	line, err := readTrimmedLine(bufio.NewReader(connection))
	if err != nil {
		return transportpkg.TransportEnvelope{}, err
	}
	var response transportpkg.TransportEnvelope
	if err := json.Unmarshal([]byte(line), &response); err != nil {
		return transportpkg.TransportEnvelope{}, err
	}
	return response, nil
}

func (runtime *meshRuntime) fetchPeerInfo(peerURL string) (nativePeerSelf, error) {
	if cached, ok := runtime.cachedPeerInfo(peerURL); ok {
		return cached, nil
	}
	request, _, err := runtime.newOutboundEnvelope(nativeTransportMessagePeerProbe, nativeTransportChannelDiscovery, "", "", map[string]any{}, "")
	if err != nil {
		return nativePeerSelf{}, err
	}
	response, err := runtime.exchangePeerTransport(peerURL, request)
	if err != nil {
		return nativePeerSelf{}, err
	}
	body, err := runtime.decodeResponseEnvelope(response, "")
	if err != nil {
		return nativePeerSelf{}, err
	}
	var decoded nativePeerSelf
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nativePeerSelf{}, err
	}
	if strings.TrimSpace(response.SignatureKeyID) == "" || response.SignatureKeyID != decoded.Peer.ProtocolPubkeyHex {
		return nativePeerSelf{}, errors.New("peer manifest protocol key mismatch")
	}
	expectedProtocolID, err := settlementcore.DeriveScopedID(settlementcore.ProtocolIdentityScope, decoded.Peer.ProtocolPubkeyHex)
	if err != nil {
		return nativePeerSelf{}, err
	}
	if decoded.ProtocolID != expectedProtocolID {
		return nativePeerSelf{}, errors.New("peer manifest protocol id mismatch")
	}
	runtime.cachePeerInfo(peerURL, decoded)
	return decoded, nil
}

func (runtime *meshRuntime) cachedPeerInfo(peerURL string) (nativePeerSelf, bool) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()

	cached, ok := runtime.peerInfoCache[peerURL]
	if !ok {
		return nativePeerSelf{}, false
	}
	if time.Since(cached.FetchedAt) > nativePeerInfoCacheTTL {
		delete(runtime.peerInfoCache, peerURL)
		if canonical := strings.TrimSpace(cached.PeerSelf.Peer.PeerURL); canonical != "" {
			delete(runtime.peerInfoCache, canonical)
		}
		return nativePeerSelf{}, false
	}
	return cached.PeerSelf, true
}

func (runtime *meshRuntime) cachePeerInfo(peerURL string, peerInfo nativePeerSelf) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()

	if runtime.peerInfoCache == nil {
		runtime.peerInfoCache = map[string]nativeCachedPeerInfo{}
	}
	cached := nativeCachedPeerInfo{
		FetchedAt: time.Now(),
		PeerSelf:  peerInfo,
	}
	if strings.TrimSpace(peerURL) != "" {
		runtime.peerInfoCache[peerURL] = cached
	}
	if canonical := strings.TrimSpace(peerInfo.Peer.PeerURL); canonical != "" {
		runtime.peerInfoCache[canonical] = cached
	}
}

func (runtime *meshRuntime) fetchRemoteTable(peerURL, tableID string) (*nativeTableState, error) {
	requestBody, err := runtime.buildTableFetchRequest(tableID)
	if err != nil {
		return nil, err
	}
	peerInfo, err := runtime.fetchPeerInfo(peerURL)
	if err != nil {
		return nil, err
	}
	request, requestKey, err := runtime.newOutboundEnvelope(
		nativeTransportMessageTablePull,
		nativeTransportChannelTable,
		tableID,
		peerInfo.Peer.PeerID,
		requestBody,
		peerInfo.TransportPubkeyHex,
	)
	if err != nil {
		return nil, err
	}
	response, err := runtime.exchangePeerTransport(peerURL, request)
	if err != nil {
		return nil, err
	}
	body, err := runtime.decodeResponseEnvelope(response, requestKey)
	if err != nil {
		return nil, err
	}
	var table nativeTableState
	if err := json.Unmarshal(body, &table); err != nil {
		return nil, err
	}
	return &table, nil
}

func (runtime *meshRuntime) remoteJoin(peerURL string, input nativeJoinRequest) (nativeTableState, error) {
	return runtime.sendPeerTableRequest(peerURL, nativeTransportMessageTableJoinReq, nativeTransportMessageTableJoinResp, input.TableID, input)
}

func (runtime *meshRuntime) remoteAction(peerURL string, input nativeActionRequest) (nativeTableState, error) {
	return runtime.sendPeerTableRequest(peerURL, nativeTransportMessageTableActReq, nativeTransportMessageTableActResp, input.TableID, input)
}

func (runtime *meshRuntime) remoteFunds(peerURL string, input nativeFundsRequest) (nativeFundsResponse, error) {
	if runtime.fundsSenderHook != nil {
		return runtime.fundsSenderHook(peerURL, input)
	}
	peerInfo, err := runtime.fetchPeerInfo(peerURL)
	if err != nil {
		return nativeFundsResponse{}, err
	}
	request, requestKey, err := runtime.newOutboundEnvelope(nativeTransportMessageTableFundsReq, nativeTransportChannelTable, input.TableID, peerInfo.Peer.PeerID, input, peerInfo.TransportPubkeyHex)
	if err != nil {
		return nativeFundsResponse{}, err
	}
	response, err := runtime.exchangePeerTransport(peerURL, request)
	if err != nil {
		return nativeFundsResponse{}, err
	}
	body, err := runtime.decodeResponseEnvelope(response, requestKey)
	if err != nil {
		return nativeFundsResponse{}, err
	}
	if response.MessageType != nativeTransportMessageTableFundsResp {
		return nativeFundsResponse{}, fmt.Errorf("unexpected transport response %q", response.MessageType)
	}
	var decoded nativeFundsResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nativeFundsResponse{}, err
	}
	return decoded, nil
}

func (runtime *meshRuntime) remoteHandMessage(peerURL string, input nativeHandMessageRequest) (nativeTableState, error) {
	return runtime.sendPeerTableRequest(peerURL, nativeTransportMessageTableHandReq, nativeTransportMessageTableHandResp, input.TableID, input)
}

func (runtime *meshRuntime) remoteApproveCustody(peerURL string, input nativeCustodyApprovalRequest) (tablecustody.CustodySignature, error) {
	peerInfo, err := runtime.fetchPeerInfo(peerURL)
	if err != nil {
		return tablecustody.CustodySignature{}, err
	}
	request, requestKey, err := runtime.newOutboundEnvelope(nativeTransportMessageTableCustodyReq, nativeTransportChannelTable, input.TableID, peerInfo.Peer.PeerID, input, peerInfo.TransportPubkeyHex)
	if err != nil {
		return tablecustody.CustodySignature{}, err
	}
	response, err := runtime.exchangePeerTransport(peerURL, request)
	if err != nil {
		return tablecustody.CustodySignature{}, err
	}
	body, err := runtime.decodeResponseEnvelope(response, requestKey)
	if err != nil {
		return tablecustody.CustodySignature{}, err
	}
	if response.MessageType != nativeTransportMessageTableCustodyResp {
		return tablecustody.CustodySignature{}, fmt.Errorf("unexpected transport response %q", response.MessageType)
	}
	var decoded nativeCustodyApprovalResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return tablecustody.CustodySignature{}, err
	}
	return decoded.Approval, nil
}

func (runtime *meshRuntime) remoteSignCustodyPSBT(peerURL string, input nativeCustodyTxSignRequest) (string, error) {
	peerInfo, err := runtime.fetchPeerInfo(peerURL)
	if err != nil {
		return "", err
	}
	request, requestKey, err := runtime.newOutboundEnvelope(nativeTransportMessageTableCustodySignReq, nativeTransportChannelTable, input.TableID, peerInfo.Peer.PeerID, input, peerInfo.TransportPubkeyHex)
	if err != nil {
		return "", err
	}
	response, err := runtime.exchangePeerTransport(peerURL, request)
	if err != nil {
		return "", err
	}
	body, err := runtime.decodeResponseEnvelope(response, requestKey)
	if err != nil {
		return "", err
	}
	if response.MessageType != nativeTransportMessageTableCustodySignResp {
		return "", fmt.Errorf("unexpected transport response %q", response.MessageType)
	}
	var decoded nativeCustodyTxSignResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return "", err
	}
	return decoded.SignedPSBT, nil
}

func (runtime *meshRuntime) remotePrepareCustodySigner(peerURL string, input nativeCustodySignerPrepareRequest) (nativeCustodySignerPrepareResponse, error) {
	peerInfo, err := runtime.fetchPeerInfo(peerURL)
	if err != nil {
		return nativeCustodySignerPrepareResponse{}, err
	}
	request, requestKey, err := runtime.newOutboundEnvelope(nativeTransportMessageTableCustodySignerPrepareReq, nativeTransportChannelTable, input.TableID, peerInfo.Peer.PeerID, input, peerInfo.TransportPubkeyHex)
	if err != nil {
		return nativeCustodySignerPrepareResponse{}, err
	}
	response, err := runtime.exchangePeerTransport(peerURL, request)
	if err != nil {
		return nativeCustodySignerPrepareResponse{}, err
	}
	body, err := runtime.decodeResponseEnvelope(response, requestKey)
	if err != nil {
		return nativeCustodySignerPrepareResponse{}, err
	}
	if response.MessageType != nativeTransportMessageTableCustodySignerPrepareResp {
		return nativeCustodySignerPrepareResponse{}, fmt.Errorf("unexpected transport response %q", response.MessageType)
	}
	var decoded nativeCustodySignerPrepareResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nativeCustodySignerPrepareResponse{}, err
	}
	return decoded, nil
}

func (runtime *meshRuntime) remoteStartCustodySigner(peerURL string, input nativeCustodySignerStartRequest) error {
	peerInfo, err := runtime.fetchPeerInfo(peerURL)
	if err != nil {
		return err
	}
	request, requestKey, err := runtime.newOutboundEnvelope(nativeTransportMessageTableCustodySignerStartReq, nativeTransportChannelTable, input.TableID, peerInfo.Peer.PeerID, input, peerInfo.TransportPubkeyHex)
	if err != nil {
		return err
	}
	response, err := runtime.exchangePeerTransport(peerURL, request)
	if err != nil {
		return err
	}
	body, err := runtime.decodeResponseEnvelope(response, requestKey)
	if err != nil {
		return err
	}
	if response.MessageType != nativeTransportMessageTableCustodySignerStartResp {
		return fmt.Errorf("unexpected transport response %q", response.MessageType)
	}
	var decoded nativeCustodyAckResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return err
	}
	if !decoded.OK {
		return errors.New("remote custody signer start was not acknowledged")
	}
	return nil
}

func (runtime *meshRuntime) remoteAdvanceCustodySignerNonces(peerURL string, input nativeCustodySignerNoncesRequest) (bool, error) {
	peerInfo, err := runtime.fetchPeerInfo(peerURL)
	if err != nil {
		return false, err
	}
	request, requestKey, err := runtime.newOutboundEnvelope(nativeTransportMessageTableCustodySignerNoncesReq, nativeTransportChannelTable, input.TableID, peerInfo.Peer.PeerID, input, peerInfo.TransportPubkeyHex)
	if err != nil {
		return false, err
	}
	response, err := runtime.exchangePeerTransport(peerURL, request)
	if err != nil {
		return false, err
	}
	body, err := runtime.decodeResponseEnvelope(response, requestKey)
	if err != nil {
		return false, err
	}
	if response.MessageType != nativeTransportMessageTableCustodySignerNoncesResp {
		return false, fmt.Errorf("unexpected transport response %q", response.MessageType)
	}
	var decoded nativeCustodyAckResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return false, err
	}
	if !decoded.OK {
		return false, errors.New("remote custody signer nonces were not acknowledged")
	}
	return decoded.Signed, nil
}

func (runtime *meshRuntime) remoteAdvanceCustodySignerAggregatedNonces(peerURL string, input nativeCustodySignerAggregatedNoncesRequest) (bool, error) {
	peerInfo, err := runtime.fetchPeerInfo(peerURL)
	if err != nil {
		return false, err
	}
	request, requestKey, err := runtime.newOutboundEnvelope(nativeTransportMessageTableCustodySignerAggNoncesReq, nativeTransportChannelTable, input.TableID, peerInfo.Peer.PeerID, input, peerInfo.TransportPubkeyHex)
	if err != nil {
		return false, err
	}
	response, err := runtime.exchangePeerTransport(peerURL, request)
	if err != nil {
		return false, err
	}
	body, err := runtime.decodeResponseEnvelope(response, requestKey)
	if err != nil {
		return false, err
	}
	if response.MessageType != nativeTransportMessageTableCustodySignerAggNoncesResp {
		return false, fmt.Errorf("unexpected transport response %q", response.MessageType)
	}
	var decoded nativeCustodyAckResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return false, err
	}
	if !decoded.OK {
		return false, errors.New("remote custody signer aggregated nonces were not acknowledged")
	}
	return decoded.Signed, nil
}

func (runtime *meshRuntime) sendPeerTableRequest(peerURL, requestType, responseType, tableID string, input any) (nativeTableState, error) {
	peerInfo, err := runtime.fetchPeerInfo(peerURL)
	if err != nil {
		return nativeTableState{}, err
	}
	request, requestKey, err := runtime.newOutboundEnvelope(requestType, nativeTransportChannelTable, tableID, peerInfo.Peer.PeerID, input, peerInfo.TransportPubkeyHex)
	if err != nil {
		return nativeTableState{}, err
	}
	response, err := runtime.exchangePeerTransport(peerURL, request)
	if err != nil {
		return nativeTableState{}, err
	}
	body, err := runtime.decodeResponseEnvelope(response, requestKey)
	if err != nil {
		return nativeTableState{}, err
	}
	if response.MessageType != responseType {
		return nativeTableState{}, fmt.Errorf("unexpected transport response %q", response.MessageType)
	}
	var table nativeTableState
	if err := json.Unmarshal(body, &table); err != nil {
		return nativeTableState{}, err
	}
	return table, nil
}

func (runtime *meshRuntime) sendTableSync(peerURL string, input nativeTableSyncRequest) error {
	peerInfo, err := runtime.fetchPeerInfo(peerURL)
	if err != nil {
		return err
	}
	request, requestKey, err := runtime.newOutboundEnvelope(nativeTransportMessageTablePush, nativeTransportChannelSync, input.Table.Config.TableID, peerInfo.Peer.PeerID, input, peerInfo.TransportPubkeyHex)
	if err != nil {
		return err
	}
	response, err := runtime.exchangePeerTransport(peerURL, request)
	if err != nil {
		return err
	}
	body, err := runtime.decodeResponseEnvelope(response, requestKey)
	if err != nil {
		return err
	}
	if response.MessageType == nativeTransportMessageNack {
		var nack nativeTransportError
		if json.Unmarshal(body, &nack) == nil && nack.Error != "" {
			return errors.New(nack.Error)
		}
	}
	if response.MessageType != nativeTransportMessageAck {
		return fmt.Errorf("unexpected transport response %q", response.MessageType)
	}
	return nil
}

func (runtime *meshRuntime) encodeResponseEnvelope(request transportpkg.TransportEnvelope, messageType, channel string, sharedSecret []byte, body any) (transportpkg.TransportEnvelope, error) {
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return transportpkg.TransportEnvelope{}, err
	}
	return runtime.buildEnvelope(messageType, channel, request.TableID, request.SenderPeerID, bodyBytes, sharedSecret, request.MessageID)
}

func (runtime *meshRuntime) nackFromRequest(request transportpkg.TransportEnvelope, err error) transportpkg.TransportEnvelope {
	sharedSecret, _ := runtime.sharedSecretForInbound(request)
	envelope, nackErr := runtime.encodeResponseEnvelope(request, nativeTransportMessageNack, nativeTransportChannelSync, sharedSecret, nativeTransportError{Error: err.Error()})
	if nackErr == nil {
		return envelope
	}
	return runtime.plainNack(request.MessageID, err)
}

func (runtime *meshRuntime) plainNack(retryOf string, err error) transportpkg.TransportEnvelope {
	body, _ := json.Marshal(nativeTransportError{Error: err.Error()})
	envelope, encodeErr := runtime.buildEnvelope(nativeTransportMessageNack, nativeTransportChannelSync, "", "", body, nil, retryOf)
	if encodeErr != nil {
		return transportpkg.TransportEnvelope{MessageType: nativeTransportMessageNack, RetryOf: retryOf}
	}
	return envelope
}

func (runtime *meshRuntime) newOutboundEnvelope(messageType, channel, tableID, recipientID string, body any, recipientTransportPub string) (transportpkg.TransportEnvelope, string, error) {
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return transportpkg.TransportEnvelope{}, "", err
	}
	var sharedSecret []byte
	var requestKey string
	if strings.TrimSpace(recipientTransportPub) != "" {
		var ephemeralPub string
		sharedSecret, requestKey, ephemeralPub, err = deriveOutboundSharedSecret(recipientTransportPub)
		if err != nil {
			return transportpkg.TransportEnvelope{}, "", err
		}
		envelope, err := runtime.buildEnvelope(messageType, channel, tableID, recipientID, bodyBytes, sharedSecret, "")
		if err != nil {
			return transportpkg.TransportEnvelope{}, "", err
		}
		envelope.EncryptionEphemeral = ephemeralPub
		unsigned := rawJSONMap(envelope)
		delete(unsigned, "signature")
		envelope.Signature, err = settlementcore.SignStructuredData(runtime.protocolIdentity.PrivateKeyHex, unsigned)
		if err != nil {
			return transportpkg.TransportEnvelope{}, "", err
		}
		return envelope, requestKey, nil
	}
	envelope, err := runtime.buildEnvelope(messageType, channel, tableID, recipientID, bodyBytes, nil, "")
	return envelope, "", err
}

func (runtime *meshRuntime) buildEnvelope(messageType, channel, tableID, recipientID string, body []byte, sharedSecret []byte, retryOf string) (transportpkg.TransportEnvelope, error) {
	messageID := randomUUID()
	if retryOf != "" {
		messageID = randomUUID()
	}
	createdAt := nowISO()
	bodyHash := sha256.Sum256(body)
	envelope := transportpkg.TransportEnvelope{
		Attempt:              1,
		BodyHash:             hex.EncodeToString(bodyHash[:]),
		Channel:              channel,
		CreatedAt:            createdAt,
		DedupeKey:            messageID,
		EncryptionMode:       nativeTransportEncryptionNone,
		ExpiresAt:            createdAtFromTime(time.Now().Add(nativeTransportRequestTTL)),
		MessageID:            messageID,
		MessageType:          messageType,
		RecipientID:          recipientID,
		RetryOf:              retryOf,
		SenderPeerID:         runtime.selfPeerID(),
		SignatureKeyID:       runtime.protocolIdentity.PublicKeyHex,
		TableID:              tableID,
		TransportWireVersion: TransportWireVersion,
	}
	if sharedSecret != nil {
		nonce, ciphertext, err := encryptSharedSecret(sharedSecret, body)
		if err != nil {
			return transportpkg.TransportEnvelope{}, err
		}
		envelope.BodyCiphertext = base64.RawStdEncoding.EncodeToString(ciphertext)
		envelope.EncryptionMode = nativeTransportEncryptionX25519GCM
		envelope.Nonce = hex.EncodeToString(nonce)
		envelope.EncryptionKeyID = runtime.transportKeyID
	} else {
		envelope.BodyCiphertext = base64.RawStdEncoding.EncodeToString(body)
	}
	unsigned := rawJSONMap(envelope)
	delete(unsigned, "signature")
	signature, err := settlementcore.SignStructuredData(runtime.protocolIdentity.PrivateKeyHex, unsigned)
	if err != nil {
		return transportpkg.TransportEnvelope{}, err
	}
	envelope.Signature = signature
	return envelope, nil
}

func (runtime *meshRuntime) decodeIncomingEnvelope(envelope transportpkg.TransportEnvelope) ([]byte, []byte, error) {
	if envelope.TransportWireVersion != TransportWireVersion {
		return nil, nil, fmt.Errorf("unsupported transport wire version %d", envelope.TransportWireVersion)
	}
	unsigned := rawJSONMap(envelope)
	delete(unsigned, "signature")
	ok, err := settlementcore.VerifyStructuredData(envelope.SignatureKeyID, unsigned, envelope.Signature)
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		return nil, nil, errors.New("transport envelope signature is invalid")
	}
	sharedSecret, err := runtime.sharedSecretForInbound(envelope)
	if err != nil {
		return nil, nil, err
	}
	body, err := decodeEnvelopeBody(envelope, sharedSecret)
	if err != nil {
		return nil, nil, err
	}
	return body, sharedSecret, nil
}

func (runtime *meshRuntime) decodeResponseEnvelope(envelope transportpkg.TransportEnvelope, requestKey string) ([]byte, error) {
	if envelope.MessageType == nativeTransportMessageNack && strings.TrimSpace(envelope.Signature) == "" {
		return nil, errors.New("transport request failed")
	}
	if envelope.TransportWireVersion != TransportWireVersion {
		return nil, fmt.Errorf("unsupported transport wire version %d", envelope.TransportWireVersion)
	}
	unsigned := rawJSONMap(envelope)
	delete(unsigned, "signature")
	ok, err := settlementcore.VerifyStructuredData(envelope.SignatureKeyID, unsigned, envelope.Signature)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("transport response signature is invalid")
	}
	var sharedSecret []byte
	if requestKey != "" && envelope.EncryptionMode == nativeTransportEncryptionX25519GCM {
		sharedSecret, err = hex.DecodeString(requestKey)
		if err != nil {
			return nil, err
		}
	}
	body, err := decodeEnvelopeBody(envelope, sharedSecret)
	if err != nil {
		return nil, err
	}
	if envelope.MessageType == nativeTransportMessageNack {
		var nack nativeTransportError
		if json.Unmarshal(body, &nack) == nil && nack.Error != "" {
			return nil, errors.New(nack.Error)
		}
		return nil, errors.New("transport request failed")
	}
	return body, nil
}

func (runtime *meshRuntime) sharedSecretForInbound(envelope transportpkg.TransportEnvelope) ([]byte, error) {
	if envelope.EncryptionMode == nativeTransportEncryptionNone {
		return nil, nil
	}
	return deriveSharedSecretFromPrivate(runtime.transportPrivate, envelope.EncryptionEphemeral)
}

func decodeEnvelopeBody(envelope transportpkg.TransportEnvelope, sharedSecret []byte) ([]byte, error) {
	body, err := base64.RawStdEncoding.DecodeString(envelope.BodyCiphertext)
	if err != nil {
		return nil, err
	}
	switch envelope.EncryptionMode {
	case nativeTransportEncryptionNone:
	case nativeTransportEncryptionX25519GCM:
		if len(sharedSecret) == 0 {
			return nil, errors.New("missing transport shared secret")
		}
		nonce, err := hex.DecodeString(envelope.Nonce)
		if err != nil {
			return nil, err
		}
		body, err = decryptSharedSecret(sharedSecret, nonce, body)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unsupported transport encryption mode %q", envelope.EncryptionMode)
	}
	sum := sha256.Sum256(body)
	if envelope.BodyHash != "" && envelope.BodyHash != hex.EncodeToString(sum[:]) {
		return nil, errors.New("transport body hash mismatch")
	}
	return body, nil
}

func deriveOutboundSharedSecret(recipientPublicHex string) ([]byte, string, string, error) {
	privateHex, err := randomX25519PrivateKeyHex()
	if err != nil {
		return nil, "", "", err
	}
	privateBytes, err := hex.DecodeString(privateHex)
	if err != nil {
		return nil, "", "", err
	}
	privateKey, err := ecdh.X25519().NewPrivateKey(privateBytes)
	if err != nil {
		return nil, "", "", err
	}
	recipientBytes, err := hex.DecodeString(recipientPublicHex)
	if err != nil {
		return nil, "", "", err
	}
	recipientKey, err := ecdh.X25519().NewPublicKey(recipientBytes)
	if err != nil {
		return nil, "", "", err
	}
	sharedSecret, err := privateKey.ECDH(recipientKey)
	if err != nil {
		return nil, "", "", err
	}
	return sharedSecret, hex.EncodeToString(sharedSecret), hex.EncodeToString(privateKey.PublicKey().Bytes()), nil
}

func deriveSharedSecretFromPrivate(privateHex, otherKeyHex string) ([]byte, error) {
	privateBytes, err := hex.DecodeString(privateHex)
	if err != nil {
		return nil, err
	}
	privateKey, err := ecdh.X25519().NewPrivateKey(privateBytes)
	if err != nil {
		return nil, err
	}
	otherBytes, err := hex.DecodeString(otherKeyHex)
	if err != nil {
		return nil, err
	}
	publicKey, err := ecdh.X25519().NewPublicKey(otherBytes)
	if err != nil {
		return nil, err
	}
	return privateKey.ECDH(publicKey)
}

func encryptSharedSecret(sharedSecret, plaintext []byte) ([]byte, []byte, error) {
	key := sha256.Sum256(sharedSecret)
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	return nonce, gcm.Seal(nil, nonce, plaintext, nil), nil
}

func decryptSharedSecret(sharedSecret, nonce, ciphertext []byte) ([]byte, error) {
	key := sha256.Sum256(sharedSecret)
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, nonce, ciphertext, nil)
}

func (runtime *meshRuntime) buildTableFetchRequest(tableID string) (nativeTableFetchRequest, error) {
	request := nativeTableFetchRequest{TableID: tableID}
	if runtime.walletID.PlayerID == "" || runtime.walletID.PrivateKeyHex == "" {
		return request, nil
	}
	request.PlayerID = runtime.walletID.PlayerID
	request.SignedAt = nowISO()
	signatureHex, err := settlementcore.SignStructuredData(runtime.walletID.PrivateKeyHex, nativeTableFetchAuthPayload(tableID, runtime.walletID.PlayerID, request.SignedAt))
	if err != nil {
		return nativeTableFetchRequest{}, err
	}
	request.SignatureHex = signatureHex
	return request, nil
}

func (runtime *meshRuntime) tableViewerPlayerIDFromFetch(request nativeTableFetchRequest, table nativeTableState) string {
	playerID := strings.TrimSpace(request.PlayerID)
	signedAt := strings.TrimSpace(request.SignedAt)
	signatureHex := strings.TrimSpace(request.SignatureHex)
	if playerID == "" || signedAt == "" || signatureHex == "" {
		return ""
	}
	if !timestampWithinWindow(signedAt, nativeTableFetchAuthMaxAge) {
		return ""
	}
	seat, ok := seatRecordForPlayer(table, playerID)
	if !ok || strings.TrimSpace(seat.WalletPubkeyHex) == "" {
		return ""
	}
	ok, err := settlementcore.VerifyStructuredData(seat.WalletPubkeyHex, nativeTableFetchAuthPayload(table.Config.TableID, playerID, signedAt), signatureHex)
	if err != nil || !ok {
		return ""
	}
	return playerID
}

func createdAtFromTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339)
}
