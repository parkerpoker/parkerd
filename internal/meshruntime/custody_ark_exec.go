package meshruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	arklib "github.com/arkade-os/arkd/pkg/ark-lib"
	arkintent "github.com/arkade-os/arkd/pkg/ark-lib/intent"
	arkscript "github.com/arkade-os/arkd/pkg/ark-lib/script"
	arktree "github.com/arkade-os/arkd/pkg/ark-lib/tree"
	arktxutils "github.com/arkade-os/arkd/pkg/ark-lib/txutils"
	arksdk "github.com/arkade-os/go-sdk"
	arkclient "github.com/arkade-os/go-sdk/client"
	arkgrpc "github.com/arkade-os/go-sdk/client/grpc"
	sdktypes "github.com/arkade-os/go-sdk/types"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/parkerpoker/parkerd/internal/tablecustody"
	walletpkg "github.com/parkerpoker/parkerd/internal/wallet"
)

type custodyBatchOutput struct {
	AmountSats    int
	ClaimKey      string
	Onchain       bool
	OwnerPlayerID string
	Script        string
	Tapscripts    []string
}

type custodyBatchResult struct {
	ArkTxID     string
	FinalizedAt string
	IntentID    string
	OutputRefs  map[string][]tablecustody.VTXORef
}

func custodySignerSessionKey(tableID, transitionHash, playerID, derivationPath string) string {
	return tableID + "|" + transitionHash + "|" + playerID + "|" + derivationPath
}

func (runtime *meshRuntime) storeCustodySignerSession(key string, session walletpkg.CustodySignerSession) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.custodySigners[key] = session
}

func (runtime *meshRuntime) loadCustodySignerSession(key string) (walletpkg.CustodySignerSession, bool) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	session, ok := runtime.custodySigners[key]
	return session, ok
}

func (runtime *meshRuntime) deleteCustodySignerSession(key string) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	delete(runtime.custodySigners, key)
}

func (runtime *meshRuntime) newArkTransportClient() (arkclient.TransportClient, error) {
	return arkgrpc.NewClient(runtime.config.ArkServerURL)
}

func custodyBatchExpiry(expiry uint32) arklib.RelativeLocktime {
	if expiry >= 512 {
		return arklib.RelativeLocktime{Type: arklib.LocktimeTypeSecond, Value: expiry}
	}
	return arklib.RelativeLocktime{Type: arklib.LocktimeTypeBlock, Value: expiry}
}

func custodyIntentInputs(inputs []custodyInputSpec) ([]arkintent.Input, []*arklib.TaprootMerkleProof, [][]*psbt.Unknown, error) {
	intentInputs := make([]arkintent.Input, 0, len(inputs))
	leafProofs := make([]*arklib.TaprootMerkleProof, 0, len(inputs))
	arkFields := make([][]*psbt.Unknown, 0, len(inputs))
	for _, input := range inputs {
		hash, err := chainhash.NewHashFromStr(input.Ref.TxID)
		if err != nil {
			return nil, nil, nil, err
		}
		intentInputs = append(intentInputs, arkintent.Input{
			OutPoint: &wire.OutPoint{
				Hash:  *hash,
				Index: input.Ref.VOut,
			},
			Sequence: wire.MaxTxInSequenceNum,
			WitnessUtxo: &wire.TxOut{
				Value:    int64(input.Ref.AmountSats),
				PkScript: input.SpendPath.PKScript,
			},
		})
		leafProofs = append(leafProofs, input.SpendPath.LeafProof)
		taptreeField, err := arktxutils.VtxoTaprootTreeField.Encode(input.SpendPath.Tapscripts)
		if err != nil {
			return nil, nil, nil, err
		}
		arkFields = append(arkFields, []*psbt.Unknown{taptreeField})
	}
	return intentInputs, leafProofs, arkFields, nil
}

func custodyRegisterMessage(onchainIndexes []int, cosignerPubkeys []string) (string, error) {
	validAt := time.Now()
	message, err := arkintent.RegisterMessage{
		BaseMessage: arkintent.BaseMessage{
			Type: arkintent.IntentMessageTypeRegister,
		},
		OnchainOutputIndexes: onchainIndexes,
		ExpireAt:             validAt.Add(2 * time.Minute).Unix(),
		ValidAt:              validAt.Unix(),
		CosignersPublicKeys:  cosignerPubkeys,
	}.Encode()
	if err != nil {
		return "", err
	}
	return message, nil
}

func custodyBuildProofPSBT(message string, inputs []arkintent.Input, outputs []*wire.TxOut, leafProofs []*arklib.TaprootMerkleProof, arkFields [][]*psbt.Unknown) (string, error) {
	proof, err := arkintent.New(message, inputs, outputs)
	if err != nil {
		return "", err
	}
	for i, input := range proof.Inputs {
		var leafProof *arklib.TaprootMerkleProof
		if i == 0 {
			leafProof = leafProofs[0]
		} else {
			leafProof = leafProofs[i-1]
			input.Unknowns = arkFields[i-1]
		}
		input.TaprootLeafScript = []*psbt.TaprootTapLeafScript{{
			ControlBlock: leafProof.ControlBlock,
			Script:       leafProof.Script,
			LeafVersion:  txscript.BaseLeafVersion,
		}}
		proof.Inputs[i] = input
	}
	return proof.B64Encode()
}

func (runtime *meshRuntime) signCustodyPSBTWithPlayer(table nativeTableState, playerID, prevStateHash, requestKey, current string) (string, error) {
	if playerID == runtime.walletID.PlayerID {
		return runtime.walletRuntime.SignCustodyTransaction(runtime.profileName, current)
	}
	seat, ok := seatRecordForPlayer(table, playerID)
	if !ok {
		return "", fmt.Errorf("missing seat for signer %s", playerID)
	}
	if seat.PeerURL == "" {
		return "", fmt.Errorf("missing peer url for signer %s", playerID)
	}
	return runtime.remoteSignCustodyPSBT(seat.PeerURL, nativeCustodyTxSignRequest{
		ExpectedPrevStateHash: prevStateHash,
		PSBT:                  current,
		PlayerID:              playerID,
		TableID:               table.Config.TableID,
		TransitionHash:        requestKey,
	})
}

func (runtime *meshRuntime) fullySignCustodyPSBT(table nativeTableState, prevStateHash, requestKey string, signerIDs []string, unsigned string) (string, error) {
	signed := unsigned
	for _, playerID := range uniqueSortedPlayerIDs(signerIDs) {
		nextSigned, err := runtime.signCustodyPSBTWithPlayer(table, playerID, prevStateHash, requestKey, signed)
		if err != nil {
			return "", err
		}
		signed = nextSigned
	}
	return signed, nil
}

func (runtime *meshRuntime) handleCustodyTxSignFromPeer(request nativeCustodyTxSignRequest) (nativeCustodyTxSignResponse, error) {
	table, err := runtime.requireLocalTable(request.TableID)
	if err != nil {
		return nativeCustodyTxSignResponse{}, err
	}
	if request.PlayerID != runtime.walletID.PlayerID {
		return nativeCustodyTxSignResponse{}, errors.New("custody tx signing request is not addressed to this player")
	}
	if request.ExpectedPrevStateHash != "" && latestCustodyStateHash(*table) != request.ExpectedPrevStateHash {
		return nativeCustodyTxSignResponse{}, errors.New("custody tx signing request references stale state")
	}
	signedPSBT, err := runtime.walletRuntime.SignCustodyTransaction(runtime.profileName, request.PSBT)
	if err != nil {
		return nativeCustodyTxSignResponse{}, err
	}
	return nativeCustodyTxSignResponse{SignedPSBT: signedPSBT}, nil
}

func (runtime *meshRuntime) handleCustodySignerPrepareFromPeer(request nativeCustodySignerPrepareRequest) (nativeCustodySignerPrepareResponse, error) {
	table, err := runtime.requireLocalTable(request.TableID)
	if err != nil {
		return nativeCustodySignerPrepareResponse{}, err
	}
	if request.PlayerID != runtime.walletID.PlayerID {
		return nativeCustodySignerPrepareResponse{}, errors.New("custody signer prepare request is not addressed to this player")
	}
	if request.ExpectedPrevStateHash != "" && latestCustodyStateHash(*table) != request.ExpectedPrevStateHash {
		return nativeCustodySignerPrepareResponse{}, errors.New("custody signer prepare request references stale state")
	}
	session, err := runtime.walletRuntime.NewCustodySignerSession(runtime.profileName, request.DerivationPath)
	if err != nil {
		return nativeCustodySignerPrepareResponse{}, err
	}
	runtime.storeCustodySignerSession(custodySignerSessionKey(request.TableID, request.TransitionHash, request.PlayerID, request.DerivationPath), session)
	return nativeCustodySignerPrepareResponse{SignerPubkeyHex: session.PublicKeyHex}, nil
}

func (runtime *meshRuntime) handleCustodySignerStartFromPeer(request nativeCustodySignerStartRequest) (nativeCustodyAckResponse, error) {
	key := custodySignerSessionKey(request.TableID, request.TransitionHash, request.PlayerID, request.DerivationPath)
	session, ok := runtime.loadCustodySignerSession(key)
	if !ok {
		return nativeCustodyAckResponse{}, errors.New("custody signer session is not available")
	}
	rootBytes, err := hex.DecodeString(request.SweepTapTreeRootHex)
	if err != nil {
		return nativeCustodyAckResponse{}, err
	}
	vtxoTree, err := arktree.NewTxTree(request.VtxoTree)
	if err != nil {
		return nativeCustodyAckResponse{}, err
	}
	if err := session.Session.Init(rootBytes, request.BatchOutputAmountSats, vtxoTree); err != nil {
		return nativeCustodyAckResponse{}, err
	}
	nonces, err := session.Session.GetNonces()
	if err != nil {
		return nativeCustodyAckResponse{}, err
	}
	client, err := runtime.newArkTransportClient()
	if err != nil {
		return nativeCustodyAckResponse{}, err
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := client.SubmitTreeNonces(ctx, request.BatchID, session.Session.GetPublicKey(), nonces); err != nil {
		return nativeCustodyAckResponse{}, err
	}
	return nativeCustodyAckResponse{OK: true}, nil
}

func (runtime *meshRuntime) handleCustodySignerNoncesFromPeer(request nativeCustodySignerNoncesRequest) (nativeCustodyAckResponse, error) {
	key := custodySignerSessionKey(request.TableID, request.TransitionHash, request.PlayerID, request.DerivationPath)
	session, ok := runtime.loadCustodySignerSession(key)
	if !ok {
		return nativeCustodyAckResponse{}, errors.New("custody signer session is not available")
	}
	hasAllNonces, err := session.Session.AggregateNonces(request.TxID, request.Nonces)
	if err != nil {
		return nativeCustodyAckResponse{}, err
	}
	if !hasAllNonces {
		return nativeCustodyAckResponse{OK: true}, nil
	}
	signatures, err := session.Session.Sign()
	if err != nil {
		return nativeCustodyAckResponse{}, err
	}
	client, err := runtime.newArkTransportClient()
	if err != nil {
		return nativeCustodyAckResponse{}, err
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := client.SubmitTreeSignatures(ctx, request.BatchID, session.Session.GetPublicKey(), signatures); err != nil {
		return nativeCustodyAckResponse{}, err
	}
	runtime.deleteCustodySignerSession(key)
	return nativeCustodyAckResponse{OK: true}, nil
}

type custodyBatchEventsHandler struct {
	runtime            *meshRuntime
	table              nativeTableState
	prevStateHash      string
	requestKey         string
	plan               *custodySettlementPlan
	transport          arkclient.TransportClient
	arkConfig          walletpkg.CustodyArkConfig
	intentID           string
	derivationPath     string
	signerSessions     map[string]walletpkg.CustodySignerSession
	signerPubkeys      map[string]string
	batchSessionID     string
	batchExpiry        arklib.RelativeLocktime
	finalVtxoTree      *arktree.TxTree
	finalConnectorTree *arktree.TxTree
}

func (handler *custodyBatchEventsHandler) OnBatchStarted(ctx context.Context, event arkclient.BatchStartedEvent) (bool, error) {
	sum := sha256.Sum256([]byte(handler.intentID))
	hashedIntentID := hex.EncodeToString(sum[:])
	for _, value := range event.HashedIntentIds {
		if value != hashedIntentID {
			continue
		}
		if err := handler.transport.ConfirmRegistration(ctx, handler.intentID); err != nil {
			return false, err
		}
		handler.batchSessionID = event.Id
		handler.batchExpiry = custodyBatchExpiry(uint32(event.BatchExpiry))
		return false, nil
	}
	return true, nil
}

func (handler *custodyBatchEventsHandler) OnBatchFinalized(context.Context, arkclient.BatchFinalizedEvent) error {
	return nil
}

func (handler *custodyBatchEventsHandler) OnBatchFailed(_ context.Context, event arkclient.BatchFailedEvent) error {
	return fmt.Errorf("ark batch failed: %s", event.Reason)
}

func (handler *custodyBatchEventsHandler) OnTreeTxEvent(context.Context, arkclient.TreeTxEvent) error {
	return nil
}

func (handler *custodyBatchEventsHandler) OnTreeSignatureEvent(context.Context, arkclient.TreeSignatureEvent) error {
	return nil
}

func (handler *custodyBatchEventsHandler) OnTreeSigningStarted(ctx context.Context, event arkclient.TreeSigningStartedEvent, vtxoTree *arktree.TxTree) (bool, error) {
	requiredPubkeys := map[string]string{}
	for playerID, pubkeyHex := range handler.signerPubkeys {
		requiredPubkeys[pubkeyHex] = playerID
	}
	found := 0
	for _, pubkeyHex := range event.CosignersPubkeys {
		if _, ok := requiredPubkeys[pubkeyHex]; ok {
			found++
		}
	}
	if found == 0 {
		return true, nil
	}
	if found != len(requiredPubkeys) {
		return false, errors.New("not all custody signer pubkeys were included in tree signing")
	}

	operatorPubkey, err := compressedPubkeyFromHex(handler.arkConfig.ForfeitPubkeyHex)
	if err != nil {
		return false, err
	}
	sweepScript, err := (&arkscript.CSVMultisigClosure{
		MultisigClosure: arkscript.MultisigClosure{PubKeys: []*btcec.PublicKey{operatorPubkey}},
		Locktime:        handler.batchExpiry,
	}).Script()
	if err != nil {
		return false, err
	}
	sweepRoot := txscript.AssembleTaprootScriptTree(txscript.NewBaseTapLeaf(sweepScript)).RootNode.TapHash()

	commitment, err := psbt.NewFromRawBytes(strings.NewReader(event.UnsignedCommitmentTx), true)
	if err != nil {
		return false, err
	}
	batchOutputAmount := commitment.UnsignedTx.TxOut[0].Value
	for playerID := range handler.signerPubkeys {
		if playerID == handler.runtime.walletID.PlayerID {
			session := handler.signerSessions[playerID]
			if err := session.Session.Init(sweepRoot.CloneBytes(), batchOutputAmount, vtxoTree); err != nil {
				return false, err
			}
			nonces, err := session.Session.GetNonces()
			if err != nil {
				return false, err
			}
			if err := handler.transport.SubmitTreeNonces(ctx, event.Id, session.Session.GetPublicKey(), nonces); err != nil {
				return false, err
			}
			continue
		}
		seat, ok := seatRecordForPlayer(handler.table, playerID)
		if !ok || seat.PeerURL == "" {
			return false, fmt.Errorf("missing peer url for custody signer %s", playerID)
		}
		if err := handler.runtime.remoteStartCustodySigner(seat.PeerURL, nativeCustodySignerStartRequest{
			BatchID:               event.Id,
			BatchOutputAmountSats: batchOutputAmount,
			DerivationPath:        handler.derivationPath,
			PlayerID:              playerID,
			SweepTapTreeRootHex:   hex.EncodeToString(sweepRoot.CloneBytes()),
			TableID:               handler.table.Config.TableID,
			TransitionHash:        handler.requestKey,
			VtxoTree:              mustSerializeTxTree(vtxoTree),
		}); err != nil {
			return false, err
		}
	}
	return false, nil
}

func (handler *custodyBatchEventsHandler) OnTreeNoncesAggregated(context.Context, arkclient.TreeNoncesAggregatedEvent) (bool, error) {
	return false, nil
}

func (handler *custodyBatchEventsHandler) OnTreeNonces(ctx context.Context, event arkclient.TreeNoncesEvent) (bool, error) {
	signedCount := 0
	for playerID := range handler.signerPubkeys {
		if playerID == handler.runtime.walletID.PlayerID {
			session := handler.signerSessions[playerID]
			complete, err := session.Session.AggregateNonces(event.Txid, event.Nonces)
			if err != nil {
				return false, err
			}
			if !complete {
				continue
			}
			signatures, err := session.Session.Sign()
			if err != nil {
				return false, err
			}
			if err := handler.transport.SubmitTreeSignatures(ctx, event.Id, session.Session.GetPublicKey(), signatures); err != nil {
				return false, err
			}
			signedCount++
			continue
		}
		seat, ok := seatRecordForPlayer(handler.table, playerID)
		if !ok || seat.PeerURL == "" {
			return false, fmt.Errorf("missing peer url for custody signer %s", playerID)
		}
		if err := handler.runtime.remoteAdvanceCustodySignerNonces(seat.PeerURL, nativeCustodySignerNoncesRequest{
			BatchID:        event.Id,
			DerivationPath: handler.derivationPath,
			Nonces:         event.Nonces,
			PlayerID:       playerID,
			TableID:        handler.table.Config.TableID,
			TxID:           event.Txid,
			TransitionHash: handler.requestKey,
		}); err != nil {
			return false, err
		}
		signedCount++
	}
	return signedCount == len(handler.signerPubkeys), nil
}

func (handler *custodyBatchEventsHandler) OnBatchFinalization(ctx context.Context, event arkclient.BatchFinalizationEvent, vtxoTree, connectorTree *arktree.TxTree) error {
	if vtxoTree != nil {
		commitment, err := psbt.NewFromRawBytes(strings.NewReader(event.Tx), true)
		if err != nil {
			return err
		}
		forfeitPubkey, err := compressedPubkeyFromHex(handler.arkConfig.ForfeitPubkeyHex)
		if err != nil {
			return err
		}
		if err := arktree.ValidateVtxoTree(vtxoTree, commitment, forfeitPubkey, handler.batchExpiry); err != nil {
			return err
		}
	}
	handler.finalVtxoTree = vtxoTree
	handler.finalConnectorTree = connectorTree
	if len(handler.plan.Inputs) == 0 || connectorTree == nil {
		return nil
	}
	connectorLeaves := connectorTree.Leaves()
	if len(connectorLeaves) < len(handler.plan.Inputs) {
		return errors.New("connector tree does not contain enough leaves for custody forfeits")
	}
	signedForfeits := make([]string, 0, len(handler.plan.Inputs))
	for index, input := range handler.plan.Inputs {
		forfeit, err := handler.createSignedForfeit(ctx, input, connectorLeaves[index])
		if err != nil {
			return err
		}
		signedForfeits = append(signedForfeits, forfeit)
	}
	return handler.transport.SubmitSignedForfeitTxs(ctx, signedForfeits, "")
}

func (handler *custodyBatchEventsHandler) createSignedForfeit(ctx context.Context, input custodyInputSpec, connectorLeaf *psbt.Packet) (string, error) {
	parsedForfeitAddr, err := btcutil.DecodeAddress(handler.arkConfig.ForfeitAddress, nil)
	if err != nil {
		return "", err
	}
	forfeitPkScript, err := txscript.PayToAddrScript(parsedForfeitAddr)
	if err != nil {
		return "", err
	}
	var connector *wire.TxOut
	var connectorOutpoint *wire.OutPoint
	for outputIndex, output := range connectorLeaf.UnsignedTx.TxOut {
		if bytes.Equal(arktxutils.ANCHOR_PKSCRIPT, output.PkScript) {
			continue
		}
		connector = output
		connectorOutpoint = &wire.OutPoint{
			Hash:  connectorLeaf.UnsignedTx.TxHash(),
			Index: uint32(outputIndex),
		}
		break
	}
	if connector == nil || connectorOutpoint == nil {
		return "", errors.New("connector output was not found")
	}
	vtxoHash, err := chainhash.NewHashFromStr(input.Ref.TxID)
	if err != nil {
		return "", err
	}
	forfeitTx, err := arktree.BuildForfeitTx(
		[]*wire.OutPoint{{Hash: *vtxoHash, Index: input.Ref.VOut}, connectorOutpoint},
		[]uint32{wire.MaxTxInSequenceNum, wire.MaxTxInSequenceNum},
		[]*wire.TxOut{{Value: int64(input.Ref.AmountSats), PkScript: input.SpendPath.PKScript}, connector},
		forfeitPkScript,
		0,
	)
	if err != nil {
		return "", err
	}
	forfeitTx.Inputs[0].TaprootLeafScript = []*psbt.TaprootTapLeafScript{{
		ControlBlock: input.SpendPath.LeafProof.ControlBlock,
		Script:       input.SpendPath.Script,
		LeafVersion:  txscript.BaseLeafVersion,
	}}
	unsigned, err := forfeitTx.B64Encode()
	if err != nil {
		return "", err
	}
	return handler.runtime.fullySignCustodyPSBT(handler.table, handler.prevStateHash, handler.requestKey, input.SpendPath.PlayerIDs, unsigned)
}

func mustSerializeTxTree(value *arktree.TxTree) arktree.FlatTxTree {
	if value == nil {
		return nil
	}
	serialized, err := value.Serialize()
	if err != nil {
		panic(err)
	}
	return serialized
}

func custodyBatchRequestKey(tableID, scope string, seq int) string {
	return fmt.Sprintf("%s:%s:%d", tableID, scope, seq)
}

func custodySignerDerivationPath(requestKey string) string {
	sum := sha256.Sum256([]byte(requestKey))
	return "parker/custody/" + hex.EncodeToString(sum[:])
}

func custodyBatchOutputFromSpec(spec custodyOutputSpec) custodyBatchOutput {
	return custodyBatchOutput{
		AmountSats:    spec.AmountSats,
		ClaimKey:      spec.ClaimKey,
		OwnerPlayerID: spec.OwnerPlayerID,
		Script:        spec.Script,
		Tapscripts:    append([]string(nil), spec.Tapscripts...),
	}
}

func custodyBatchOutputFromReceiver(claimKey, ownerPlayerID string, receiver sdktypes.Receiver, tapscripts []string) (custodyBatchOutput, error) {
	txOut, onchain, err := receiver.ToTxOut()
	if err != nil {
		return custodyBatchOutput{}, err
	}
	return custodyBatchOutput{
		AmountSats:    int(receiver.Amount),
		ClaimKey:      claimKey,
		Onchain:       onchain,
		OwnerPlayerID: ownerPlayerID,
		Script:        hex.EncodeToString(txOut.PkScript),
		Tapscripts:    append([]string(nil), tapscripts...),
	}, nil
}

func decodeBatchOutputTxOut(output custodyBatchOutput) (*wire.TxOut, error) {
	scriptBytes, err := hex.DecodeString(output.Script)
	if err != nil {
		return nil, err
	}
	return &wire.TxOut{
		Value:    int64(output.AmountSats),
		PkScript: scriptBytes,
	}, nil
}

func custodyOnchainOutputIndexes(outputs []custodyBatchOutput) []int {
	indexes := make([]int, 0)
	for index, output := range outputs {
		if output.Onchain {
			indexes = append(indexes, index)
		}
	}
	return indexes
}

func custodyOutputsRequireTreeSigning(outputs []custodyBatchOutput) bool {
	for _, output := range outputs {
		if !output.Onchain {
			return true
		}
	}
	return false
}

func (runtime *meshRuntime) prepareCustodyBatchSigners(table nativeTableState, prevStateHash, requestKey string, signerIDs []string) (map[string]walletpkg.CustodySignerSession, map[string]string, string, error) {
	derivationPath := custodySignerDerivationPath(requestKey)
	sessions := map[string]walletpkg.CustodySignerSession{}
	pubkeys := map[string]string{}
	for _, playerID := range uniqueSortedPlayerIDs(signerIDs) {
		if strings.TrimSpace(playerID) == "" {
			continue
		}
		if playerID == runtime.walletID.PlayerID {
			session, err := runtime.walletRuntime.NewCustodySignerSession(runtime.profileName, derivationPath)
			if err != nil {
				return nil, nil, "", err
			}
			sessions[playerID] = session
			pubkeys[playerID] = session.PublicKeyHex
			continue
		}
		seat, ok := seatRecordForPlayer(table, playerID)
		if !ok {
			return nil, nil, "", fmt.Errorf("missing seat for custody signer %s", playerID)
		}
		peerURL := firstNonEmptyString(seat.PeerURL, runtime.knownPeerURL(seat.PeerID))
		if peerURL == "" {
			return nil, nil, "", fmt.Errorf("missing peer url for custody signer %s", playerID)
		}
		response, err := runtime.remotePrepareCustodySigner(peerURL, nativeCustodySignerPrepareRequest{
			DerivationPath:        derivationPath,
			ExpectedPrevStateHash: prevStateHash,
			PlayerID:              playerID,
			TableID:               table.Config.TableID,
			TransitionHash:        requestKey,
		})
		if err != nil {
			return nil, nil, "", err
		}
		pubkeys[playerID] = response.SignerPubkeyHex
	}
	return sessions, pubkeys, derivationPath, nil
}

func sortedSignerPubkeys(pubkeys map[string]string) []string {
	values := make([]string, 0, len(pubkeys))
	for _, pubkeyHex := range pubkeys {
		if strings.TrimSpace(pubkeyHex) == "" {
			continue
		}
		values = append(values, pubkeyHex)
	}
	sort.Strings(values)
	return values
}

func custodyBatchTopics(inputs []custodyInputSpec, signerPubkeys map[string]string) []string {
	topics := make([]string, 0, len(inputs)+len(signerPubkeys))
	seen := map[string]struct{}{}
	for _, input := range inputs {
		topic := fundingRefKey(input.Ref)
		if _, ok := seen[topic]; ok {
			continue
		}
		seen[topic] = struct{}{}
		topics = append(topics, topic)
	}
	pubkeys := sortedSignerPubkeys(signerPubkeys)
	for _, pubkeyHex := range pubkeys {
		if _, ok := seen[pubkeyHex]; ok {
			continue
		}
		seen[pubkeyHex] = struct{}{}
		topics = append(topics, pubkeyHex)
	}
	return topics
}

func custodyRefExpiryISO(finalizedAt string, expiry arklib.RelativeLocktime) string {
	if finalizedAt == "" || expiry.Type != arklib.LocktimeTypeSecond || expiry.Value == 0 {
		return ""
	}
	timestamp, err := time.Parse(time.RFC3339, finalizedAt)
	if err != nil {
		return ""
	}
	return timestamp.Add(time.Duration(expiry.Value) * time.Second).UTC().Format(time.RFC3339)
}

func matchCustodyBatchOutputRefs(intentID, arkTxID, finalizedAt string, expiry arklib.RelativeLocktime, outputs []custodyBatchOutput, vtxoTree *arktree.TxTree) (map[string][]tablecustody.VTXORef, error) {
	matched := map[string][]tablecustody.VTXORef{}
	offchainOutputs := 0
	for _, output := range outputs {
		if !output.Onchain {
			offchainOutputs++
		}
	}
	if offchainOutputs == 0 {
		return matched, nil
	}
	if vtxoTree == nil {
		return nil, errors.New("ark batch did not return a vtxo tree for offchain outputs")
	}
	available := make([]tablecustody.VTXORef, 0)
	for _, leaf := range vtxoTree.Leaves() {
		for outputIndex, txOut := range leaf.UnsignedTx.TxOut {
			if bytes.Equal(txOut.PkScript, arktxutils.ANCHOR_PKSCRIPT) {
				continue
			}
			scriptHex := hex.EncodeToString(txOut.PkScript)
			available = append(available, tablecustody.VTXORef{
				AmountSats:  int(txOut.Value),
				ArkIntentID: intentID,
				ArkTxID:     arkTxID,
				ExpiresAt:   custodyRefExpiryISO(finalizedAt, expiry),
				Script:      scriptHex,
				TxID:        leaf.UnsignedTx.TxID(),
				VOut:        uint32(outputIndex),
			})
		}
	}
	used := make([]bool, len(available))
	for _, output := range outputs {
		if output.Onchain {
			continue
		}
		matchIndex := -1
		for index, ref := range available {
			if used[index] {
				continue
			}
			if ref.AmountSats != output.AmountSats || ref.Script != output.Script {
				continue
			}
			matchIndex = index
			break
		}
		if matchIndex < 0 {
			return nil, fmt.Errorf("ark batch output %s could not be matched in finalized vtxo tree", output.ClaimKey)
		}
		used[matchIndex] = true
		ref := available[matchIndex]
		ref.OwnerPlayerID = output.OwnerPlayerID
		ref.Tapscripts = append([]string(nil), output.Tapscripts...)
		matched[output.ClaimKey] = append(matched[output.ClaimKey], ref)
	}
	return matched, nil
}

func (runtime *meshRuntime) executeCustodyBatch(table nativeTableState, prevStateHash, requestKey string, inputs []custodyInputSpec, proofSignerIDs, treeSignerIDs []string, outputs []custodyBatchOutput) (*custodyBatchResult, error) {
	if len(inputs) == 0 {
		return nil, errors.New("custody batch is missing inputs")
	}
	intentInputs, leafProofs, arkFields, err := custodyIntentInputs(inputs)
	if err != nil {
		return nil, err
	}
	txOutputs := make([]*wire.TxOut, 0, len(outputs))
	for _, output := range outputs {
		txOut, err := decodeBatchOutputTxOut(output)
		if err != nil {
			return nil, err
		}
		txOutputs = append(txOutputs, txOut)
	}

	needsTreeSigning := custodyOutputsRequireTreeSigning(outputs)
	if needsTreeSigning && len(treeSignerIDs) == 0 {
		return nil, errors.New("custody batch is missing tree signers")
	}
	proofSignerIDs = uniqueSortedPlayerIDs(proofSignerIDs)
	if len(proofSignerIDs) == 0 {
		return nil, errors.New("custody batch is missing proof signers")
	}

	signerSessions, signerPubkeys, derivationPath, err := runtime.prepareCustodyBatchSigners(table, prevStateHash, requestKey, treeSignerIDs)
	if err != nil {
		return nil, err
	}
	cosignerPubkeys := sortedSignerPubkeys(signerPubkeys)
	message, err := custodyRegisterMessage(custodyOnchainOutputIndexes(outputs), cosignerPubkeys)
	if err != nil {
		return nil, err
	}
	unsignedProof, err := custodyBuildProofPSBT(message, intentInputs, txOutputs, leafProofs, arkFields)
	if err != nil {
		return nil, err
	}
	signedProof, err := runtime.fullySignCustodyPSBT(table, prevStateHash, requestKey, proofSignerIDs, unsignedProof)
	if err != nil {
		return nil, err
	}

	transport, err := runtime.newArkTransportClient()
	if err != nil {
		return nil, err
	}
	defer transport.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	intentID, err := transport.RegisterIntent(ctx, signedProof, message)
	if err != nil {
		return nil, err
	}
	topics := custodyBatchTopics(inputs, signerPubkeys)
	eventsCh, closeStream, err := transport.GetEventStream(ctx, topics)
	if err != nil {
		return nil, err
	}
	defer closeStream()

	arkConfig, err := runtime.arkCustodyConfig()
	if err != nil {
		return nil, err
	}
	handler := &custodyBatchEventsHandler{
		runtime:        runtime,
		table:          table,
		prevStateHash:  prevStateHash,
		requestKey:     requestKey,
		plan:           &custodySettlementPlan{Inputs: append([]custodyInputSpec(nil), inputs...)},
		transport:      transport,
		arkConfig:      arkConfig,
		intentID:       intentID,
		derivationPath: derivationPath,
		signerSessions: signerSessions,
		signerPubkeys:  signerPubkeys,
	}

	options := []arksdk.BatchSessionOption{}
	if !needsTreeSigning {
		options = append(options, arksdk.WithSkipVtxoTreeSigning())
	}
	arkTxID, err := arksdk.JoinBatchSession(ctx, eventsCh, handler, options...)
	if err != nil {
		return nil, err
	}
	finalizedAt := nowISO()
	outputRefs, err := matchCustodyBatchOutputRefs(intentID, arkTxID, finalizedAt, handler.batchExpiry, outputs, handler.finalVtxoTree)
	if err != nil {
		return nil, err
	}
	return &custodyBatchResult{
		ArkTxID:     arkTxID,
		FinalizedAt: finalizedAt,
		IntentID:    intentID,
		OutputRefs:  outputRefs,
	}, nil
}

func assignTransitionBatchRefs(transition *tablecustody.CustodyTransition, outputRefs map[string][]tablecustody.VTXORef) {
	if transition == nil {
		return
	}
	for index := range transition.NextState.StackClaims {
		key := stackClaimKey(transition.NextState.StackClaims[index].PlayerID)
		if refs, ok := outputRefs[key]; ok {
			transition.NextState.StackClaims[index].VTXORefs = append([]tablecustody.VTXORef(nil), refs...)
		}
	}
	for index := range transition.NextState.PotSlices {
		key := potClaimKey(transition.NextState.PotSlices[index].PotID)
		if refs, ok := outputRefs[key]; ok {
			transition.NextState.PotSlices[index].VTXORefs = append([]tablecustody.VTXORef(nil), refs...)
		}
	}
}

func stackProofRefs(state tablecustody.CustodyState) []tablecustody.VTXORef {
	refs := make([]tablecustody.VTXORef, 0)
	for _, claim := range state.StackClaims {
		refs = append(refs, claim.VTXORefs...)
	}
	return refs
}

func (runtime *meshRuntime) finalizeRealCustodyTransition(table *nativeTableState, transition *tablecustody.CustodyTransition) error {
	if table == nil || transition == nil {
		return nil
	}
	plan, err := runtime.buildCustodySettlementPlan(*table, *transition)
	if err != nil {
		return err
	}
	batchOutputs := make([]custodyBatchOutput, 0, len(plan.Outputs))
	for _, output := range plan.Outputs {
		batchOutputs = append(batchOutputs, custodyBatchOutputFromSpec(output))
	}
	requestKey := custodyBatchRequestKey(table.Config.TableID, string(transition.Kind), transition.CustodySeq)
	result, err := runtime.executeCustodyBatch(*table, transition.PrevStateHash, requestKey, plan.Inputs, plan.ProofSignerIDs, plan.TreeSignerIDs, batchOutputs)
	if err != nil {
		return err
	}
	assignTransitionBatchRefs(transition, result.OutputRefs)
	transition.ArkIntentID = result.IntentID
	transition.ArkTxID = result.ArkTxID
	transition.NextState.StateHash = tablecustody.HashCustodyState(transition.NextState)
	transition.NextStateHash = transition.NextState.StateHash

	approvals, err := runtime.collectCustodyApprovals(*table, *transition, runtime.requiredCustodySigners(*table, *transition))
	if err != nil {
		return err
	}
	transition.Approvals = approvals
	transition.Proof = tablecustody.CustodyProof{
		ArkIntentID:     result.IntentID,
		ArkTxID:         result.ArkTxID,
		FinalizedAt:     result.FinalizedAt,
		ReplayValidated: true,
		Signatures:      append([]tablecustody.CustodySignature(nil), approvals...),
		StateHash:       transition.NextStateHash,
		VTXORefs:        stackProofRefs(transition.NextState),
	}
	transition.Proof.TransitionHash = tablecustody.HashCustodyTransition(*transition)
	return tablecustody.ValidateTransition(table.LatestCustodyState, *transition)
}

func latestStackClaimForPlayer(state *tablecustody.CustodyState, playerID string) (tablecustody.StackClaim, bool) {
	if state == nil {
		return tablecustody.StackClaim{}, false
	}
	for _, claim := range state.StackClaims {
		if claim.PlayerID == playerID {
			return claim, true
		}
	}
	return tablecustody.StackClaim{}, false
}

func (runtime *meshRuntime) settleCurrentTableFunds(table nativeTableState, kind string) (*custodyBatchResult, int, string, error) {
	claim, ok := latestStackClaimForPlayer(table.LatestCustodyState, runtime.walletID.PlayerID)
	if !ok {
		return nil, 0, "", errors.New("latest custody state is missing the local stack claim")
	}
	if claim.AmountSats <= 0 {
		return nil, 0, "", errors.New("latest custody state has no spendable stack to settle")
	}
	if len(claim.VTXORefs) == 0 {
		return nil, 0, "", errors.New("latest custody state stack claim is missing vtxo refs")
	}
	inputs := make([]custodyInputSpec, 0, len(claim.VTXORefs))
	for _, ref := range claim.VTXORefs {
		spendPath, err := runtime.selectCustodySpendPath(table, ref, []string{runtime.walletID.PlayerID}, false)
		if err != nil {
			return nil, 0, "", err
		}
		inputs = append(inputs, custodyInputSpec{
			ClaimKey:      stackClaimKey(runtime.walletID.PlayerID),
			OwnerPlayerID: runtime.walletID.PlayerID,
			Ref:           ref,
			SpendPath:     spendPath,
		})
	}
	walletInfo, err := runtime.walletRuntime.GetWallet(runtime.profileName)
	if err != nil {
		return nil, 0, "", err
	}
	if strings.TrimSpace(walletInfo.ArkAddress) == "" {
		return nil, 0, "", errors.New("wallet has no Ark address for cash-out settlement")
	}
	output, err := custodyBatchOutputFromReceiver("wallet-return", runtime.walletID.PlayerID, sdktypes.Receiver{
		To:     walletInfo.ArkAddress,
		Amount: uint64(claim.AmountSats),
	}, nil)
	if err != nil {
		return nil, 0, "", err
	}
	requestKey := custodyBatchRequestKey(table.Config.TableID, kind, table.LatestCustodyState.CustodySeq)
	result, err := runtime.executeCustodyBatch(table, table.LatestCustodyState.StateHash, requestKey, inputs, []string{runtime.walletID.PlayerID}, []string{runtime.walletID.PlayerID}, []custodyBatchOutput{output})
	if err != nil {
		return nil, 0, "", err
	}
	exitProofRef := ""
	if kind == string(tablecustody.TransitionKindEmergencyExit) || kind == "emergency-exit" {
		exitProofRef = tablecustody.BuildExitProofRef(*table.LatestCustodyState, runtime.walletID.PlayerID, claim.VTXORefs, nil)
	}
	return result, claim.AmountSats, exitProofRef, nil
}
