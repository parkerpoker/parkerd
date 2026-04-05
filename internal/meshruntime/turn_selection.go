package meshruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	arksdk "github.com/arkade-os/go-sdk"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/parkerpoker/parkerd/internal/game"
	"github.com/parkerpoker/parkerd/internal/settlementcore"
	"github.com/parkerpoker/parkerd/internal/tablecustody"
)

type mockOperatorKeyPair struct {
	PrivateKeyHex string
	PublicKeyHex  string
}

var mockOperatorKeyCache sync.Map

func candidateIntentAckPayload(ack tablecustody.CandidateIntentAck) map[string]any {
	return map[string]any{
		"acceptedAt":           ack.AcceptedAt,
		"candidateHash":        ack.CandidateHash,
		"decisionIndex":        ack.DecisionIndex,
		"epoch":                ack.Epoch,
		"handId":               ack.HandID,
		"intentId":             ack.IntentID,
		"prevCustodyStateHash": ack.PrevCustodyStateHash,
		"tableId":              ack.TableID,
		"turnAnchorHash":       ack.TurnAnchorHash,
		"type":                 "candidate-intent-ack",
	}
}

func mockOperatorSigningKeyHex(label string) (string, string) {
	if cached, ok := mockOperatorKeyCache.Load(label); ok {
		pair := cached.(mockOperatorKeyPair)
		return pair.PrivateKeyHex, pair.PublicKeyHex
	}
	sum := sha256.Sum256([]byte(label))
	privateKey, publicKey := btcec.PrivKeyFromBytes(sum[:])
	pair := mockOperatorKeyPair{
		PrivateKeyHex: hex.EncodeToString(privateKey.Serialize()),
		PublicKeyHex:  hex.EncodeToString(publicKey.SerializeCompressed()),
	}
	actual, _ := mockOperatorKeyCache.LoadOrStore(label, pair)
	pair = actual.(mockOperatorKeyPair)
	return pair.PrivateKeyHex, pair.PublicKeyHex
}

func (runtime *meshRuntime) candidateIntentAckSigningKeyHex(operatorPubkeyHex string) string {
	if strings.TrimSpace(operatorPubkeyHex) == "" {
		return ""
	}
	if runtime.protocolIdentity.PublicKeyHex == operatorPubkeyHex {
		return runtime.protocolIdentity.PrivateKeyHex
	}
	if runtime.walletID.PublicKeyHex == operatorPubkeyHex {
		return runtime.walletID.PrivateKeyHex
	}
	if runtime.config.UseMockSettlement || runtime.custodyBatchExecute != nil {
		privateKeyHex, publicKeyHex := mockOperatorSigningKeyHex("parker-mock-ark-signer")
		if publicKeyHex == operatorPubkeyHex {
			return privateKeyHex
		}
	}
	return ""
}

func (runtime *meshRuntime) buildCandidateIntentAck(table nativeTableState, bundle NativeTurnCandidateBundle, intentID string) (*tablecustody.CandidateIntentAck, error) {
	config, err := runtime.arkCustodyConfig()
	if err != nil {
		return nil, err
	}
	ack := &tablecustody.CandidateIntentAck{
		AcceptedAt:           nowISO(),
		CandidateHash:        bundle.CandidateHash,
		DecisionIndex:        custodyDecisionIndex(&table.ActiveHand.State),
		Epoch:                table.CurrentEpoch,
		HandID:               table.ActiveHand.State.HandID,
		IntentID:             intentID,
		OperatorPubkeyHex:    config.SignerPubkeyHex,
		PrevCustodyStateHash: latestCustodyStateHash(table),
		TableID:              table.Config.TableID,
		TurnAnchorHash:       bundle.TurnAnchorHash,
	}
	if privateKeyHex := runtime.candidateIntentAckSigningKeyHex(ack.OperatorPubkeyHex); strings.TrimSpace(privateKeyHex) != "" {
		signatureHex, err := settlementcore.SignStructuredData(privateKeyHex, candidateIntentAckPayload(*ack))
		if err != nil {
			return nil, err
		}
		ack.OperatorSignatureHex = signatureHex
		return ack, nil
	}
	if runtime.custodyBatchExecute != nil || runtime.config.UseMockSettlement {
		privateKeyHex, publicKeyHex := mockOperatorSigningKeyHex("parker-mock-ark-signer")
		ack.OperatorPubkeyHex = publicKeyHex
		signatureHex, err := settlementcore.SignStructuredData(privateKeyHex, candidateIntentAckPayload(*ack))
		if err != nil {
			return nil, err
		}
		ack.OperatorSignatureHex = signatureHex
	}
	return ack, nil
}

func (runtime *meshRuntime) registerCandidateIntent(table nativeTableState, bundle NativeTurnCandidateBundle) (*tablecustody.CandidateIntentAck, error) {
	if runtime.candidateIntentAckHook != nil {
		return runtime.candidateIntentAckHook(table, bundle)
	}
	if strings.TrimSpace(bundle.SignedProofPSBT) == "" || strings.TrimSpace(bundle.RegisterMessage) == "" {
		return nil, errors.New("candidate bundle is missing a signed proof intent")
	}
	if runtime.custodyBatchExecute != nil || runtime.config.UseMockSettlement {
		return runtime.buildCandidateIntentAck(table, bundle, "synthetic-candidate-intent-"+bundle.CandidateHash[:12])
	}
	config, err := runtime.arkCustodyConfig()
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(runtime.candidateIntentAckSigningKeyHex(config.SignerPubkeyHex)) == "" {
		return nil, nil
	}
	transport, err := runtime.newArkTransportClient()
	if err != nil {
		return nil, err
	}
	defer transport.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	intentID, err := transport.RegisterIntent(ctx, bundle.SignedProofPSBT, bundle.RegisterMessage)
	if err != nil {
		return nil, err
	}
	return runtime.buildCandidateIntentAck(table, bundle, intentID)
}

func validateCandidateIntentAckMetadata(table nativeTableState, bundle NativeTurnCandidateBundle, ack tablecustody.CandidateIntentAck) error {
	if ack.TableID != table.Config.TableID {
		return errors.New("candidate intent ack table mismatch")
	}
	if ack.Epoch != table.CurrentEpoch {
		return errors.New("candidate intent ack epoch mismatch")
	}
	if table.ActiveHand != nil && ack.HandID != table.ActiveHand.State.HandID {
		return errors.New("candidate intent ack hand mismatch")
	}
	if ack.DecisionIndex != custodyDecisionIndex(&table.ActiveHand.State) {
		return errors.New("candidate intent ack decision mismatch")
	}
	if ack.PrevCustodyStateHash != latestCustodyStateHash(table) {
		return errors.New("candidate intent ack prev custody mismatch")
	}
	if ack.CandidateHash != bundle.CandidateHash {
		return errors.New("candidate intent ack candidate mismatch")
	}
	if ack.TurnAnchorHash != bundle.TurnAnchorHash {
		return errors.New("candidate intent ack turn anchor mismatch")
	}
	if strings.TrimSpace(ack.IntentID) == "" || strings.TrimSpace(ack.AcceptedAt) == "" {
		return errors.New("candidate intent ack is incomplete")
	}
	return nil
}

func (runtime *meshRuntime) verifyCandidateIntentAck(table nativeTableState, bundle NativeTurnCandidateBundle, ack tablecustody.CandidateIntentAck) error {
	if err := validateCandidateIntentAckMetadata(table, bundle, ack); err != nil {
		return err
	}
	if strings.TrimSpace(ack.OperatorSignatureHex) == "" || strings.TrimSpace(ack.OperatorPubkeyHex) == "" {
		return errors.New("candidate intent ack signature is missing")
	}
	config, err := runtime.arkCustodyConfig()
	if err != nil {
		return err
	}
	if ack.OperatorPubkeyHex != config.SignerPubkeyHex {
		return errors.New("candidate intent ack operator key mismatch")
	}
	ok, err := settlementcore.VerifyStructuredData(ack.OperatorPubkeyHex, candidateIntentAckPayload(ack), ack.OperatorSignatureHex)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("candidate intent ack signature is invalid")
	}
	return nil
}

func acceptedBeforeOrAtDeadline(acceptedAt, deadline string) bool {
	if strings.TrimSpace(acceptedAt) == "" || strings.TrimSpace(deadline) == "" {
		return false
	}
	accepted, err := parseISOTimestamp(acceptedAt)
	if err != nil {
		return false
	}
	limit, err := parseISOTimestamp(deadline)
	if err != nil {
		return false
	}
	return !accepted.After(limit)
}

func selectionAuthPayload(auth tablecustody.SelectionAuth) map[string]any {
	return map[string]any{
		"actionDeadlineAt":     auth.ActionDeadlineAt,
		"candidateHash":        auth.CandidateHash,
		"decisionIndex":        auth.DecisionIndex,
		"epoch":                auth.Epoch,
		"handId":               auth.HandID,
		"playerId":             auth.PlayerID,
		"prevCustodyStateHash": auth.PrevCustodyStateHash,
		"signedAt":             auth.SignedAt,
		"tableId":              auth.TableID,
		"turnAnchorHash":       auth.TurnAnchorHash,
		"type":                 "selection-auth",
	}
}

func actionLockedAckPayload(ack tablecustody.ActionLockedAck) map[string]any {
	return map[string]any{
		"actionDeadlineAt":     ack.ActionDeadlineAt,
		"candidateHash":        ack.CandidateHash,
		"decisionIndex":        ack.DecisionIndex,
		"epoch":                ack.Epoch,
		"handId":               ack.HandID,
		"hostPeerId":           ack.HostPeerID,
		"lockedAt":             ack.LockedAt,
		"prevCustodyStateHash": ack.PrevCustodyStateHash,
		"tableId":              ack.TableID,
		"turnAnchorHash":       ack.TurnAnchorHash,
		"type":                 "action-locked-ack",
	}
}

func (runtime *meshRuntime) buildSelectionAuth(table nativeTableState, candidateHash string) (tablecustody.SelectionAuth, error) {
	if table.ActiveHand == nil || table.ActiveHand.State.ActingSeatIndex == nil {
		return tablecustody.SelectionAuth{}, errors.New("selection auth requires an actionable hand")
	}
	menuEpoch := pendingTurnEpoch(table, table.PendingTurnMenu)
	auth := tablecustody.SelectionAuth{
		ActionDeadlineAt:     table.PendingTurnMenu.ActionDeadlineAt,
		CandidateHash:        candidateHash,
		DecisionIndex:        custodyDecisionIndex(&table.ActiveHand.State),
		Epoch:                menuEpoch,
		HandID:               table.ActiveHand.State.HandID,
		PlayerID:             seatPlayerID(table, *table.ActiveHand.State.ActingSeatIndex),
		PrevCustodyStateHash: turnMenuSourceStateHash(table),
		SignedAt:             nowISO(),
		TableID:              table.Config.TableID,
		TurnAnchorHash:       table.PendingTurnMenu.TurnAnchorHash,
	}
	signatureHex, err := settlementcore.SignStructuredData(runtime.walletID.PrivateKeyHex, selectionAuthPayload(auth))
	if err != nil {
		return tablecustody.SelectionAuth{}, err
	}
	auth.SignatureHex = signatureHex
	return auth, nil
}

func validateSelectionAuthMetadata(table nativeTableState, auth tablecustody.SelectionAuth) error {
	if table.PendingTurnMenu == nil {
		return errors.New("selection auth requires a pending turn menu")
	}
	if auth.TableID != table.Config.TableID {
		return errors.New("selection auth table mismatch")
	}
	if table.PendingTurnMenu == nil {
		return errors.New("selection auth requires a pending turn menu")
	}
	if auth.Epoch != table.PendingTurnMenu.Epoch {
		return errors.New("selection auth epoch mismatch")
	}
	if table.ActiveHand != nil && auth.HandID != table.ActiveHand.State.HandID {
		return errors.New("selection auth hand mismatch")
	}
	if auth.DecisionIndex != custodyDecisionIndex(&table.ActiveHand.State) {
		return errors.New("selection auth decision mismatch")
	}
	if auth.PrevCustodyStateHash != turnMenuSourceStateHash(table) {
		return errors.New("selection auth prev custody mismatch")
	}
	if auth.TurnAnchorHash != table.PendingTurnMenu.TurnAnchorHash {
		return errors.New("selection auth turn anchor mismatch")
	}
	if auth.ActionDeadlineAt != table.PendingTurnMenu.ActionDeadlineAt {
		return errors.New("selection auth deadline mismatch")
	}
	if strings.TrimSpace(auth.CandidateHash) == "" {
		return errors.New("selection auth candidate hash is missing")
	}
	if strings.TrimSpace(auth.SignedAt) == "" {
		return errors.New("selection auth signed timestamp is missing")
	}
	if !acceptedBeforeOrAtDeadline(auth.SignedAt, auth.ActionDeadlineAt) {
		return errors.New("selection auth was signed after the action deadline")
	}
	if table.ActiveHand == nil || table.ActiveHand.State.ActingSeatIndex == nil {
		return errors.New("selection auth requires an actionable hand")
	}
	expectedPlayerID := seatPlayerID(table, *table.ActiveHand.State.ActingSeatIndex)
	if auth.PlayerID != expectedPlayerID {
		return errors.New("selection auth player mismatch")
	}
	if _, ok := findTurnMenuOptionByCandidateHash(table.PendingTurnMenu, auth.CandidateHash); !ok {
		return errors.New("selection auth candidate is not part of the deterministic turn menu")
	}
	return nil
}

func (runtime *meshRuntime) verifySelectionAuth(table nativeTableState, auth tablecustody.SelectionAuth) error {
	if err := validateSelectionAuthMetadata(table, auth); err != nil {
		return err
	}
	if strings.TrimSpace(auth.SignatureHex) == "" {
		return errors.New("selection auth signature is missing")
	}
	seat, ok := seatRecordForPlayer(table, auth.PlayerID)
	if !ok {
		return fmt.Errorf("missing seat for acting player %s", auth.PlayerID)
	}
	ok, err := settlementcore.VerifyStructuredData(seat.WalletPubkeyHex, selectionAuthPayload(auth), auth.SignatureHex)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("selection auth signature is invalid")
	}
	return nil
}

func (runtime *meshRuntime) buildActionLockedAck(table nativeTableState, auth tablecustody.SelectionAuth, lockedAt string) (tablecustody.ActionLockedAck, error) {
	ack := tablecustody.ActionLockedAck{
		ActionDeadlineAt:     auth.ActionDeadlineAt,
		CandidateHash:        auth.CandidateHash,
		DecisionIndex:        auth.DecisionIndex,
		Epoch:                auth.Epoch,
		HandID:               auth.HandID,
		HostPeerID:           runtime.selfPeerID(),
		LockedAt:             lockedAt,
		PrevCustodyStateHash: auth.PrevCustodyStateHash,
		TableID:              auth.TableID,
		TurnAnchorHash:       auth.TurnAnchorHash,
	}
	signatureHex, err := settlementcore.SignStructuredData(runtime.protocolIdentity.PrivateKeyHex, actionLockedAckPayload(ack))
	if err != nil {
		return tablecustody.ActionLockedAck{}, err
	}
	ack.SignatureHex = signatureHex
	return ack, nil
}

func (runtime *meshRuntime) verifyActionLockedAck(table nativeTableState, ack tablecustody.ActionLockedAck) error {
	if table.PendingTurnMenu == nil {
		return errors.New("action locked ack requires a pending turn menu")
	}
	if ack.TableID != table.Config.TableID {
		return errors.New("action locked ack table mismatch")
	}
	if ack.Epoch != table.PendingTurnMenu.Epoch {
		return errors.New("action locked ack epoch mismatch")
	}
	if ack.TurnAnchorHash != table.PendingTurnMenu.TurnAnchorHash {
		return errors.New("action locked ack turn anchor mismatch")
	}
	if ack.ActionDeadlineAt != table.PendingTurnMenu.ActionDeadlineAt {
		return errors.New("action locked ack deadline mismatch")
	}
	if ack.HostPeerID != table.CurrentHost.Peer.PeerID {
		return errors.New("action locked ack host mismatch")
	}
	ok, err := settlementcore.VerifyStructuredData(table.CurrentHost.Peer.ProtocolPubkeyHex, actionLockedAckPayload(ack), ack.SignatureHex)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("action locked ack signature is invalid")
	}
	return nil
}

func (runtime *meshRuntime) selectionSettlementTimeoutMSForTable(table nativeTableState) int {
	return maxInt(runtime.handProtocolTimeoutMSForTable(table), runtime.actionTimeoutMSForTable(table))
}

func (runtime *meshRuntime) hasTimelySelectedCandidate(table nativeTableState) bool {
	if !turnMenuMatchesTable(table, table.PendingTurnMenu) {
		return false
	}
	menu := table.PendingTurnMenu
	if strings.TrimSpace(menu.SelectedCandidateHash) == "" || menu.SelectionAuth == nil || menu.SelectedBundle == nil {
		return false
	}
	if menu.SelectedBundle.CandidateHash != menu.SelectedCandidateHash {
		return false
	}
	if err := runtime.verifySelectionAuth(table, *menu.SelectionAuth); err != nil {
		return false
	}
	return acceptedBeforeOrAtDeadline(menu.SelectionAuth.SignedAt, menu.ActionDeadlineAt)
}

func nextHandStateForCandidate(table nativeTableState, bundle NativeTurnCandidateBundle) (game.HoldemState, error) {
	if table.ActiveHand == nil || table.ActiveHand.State.ActingSeatIndex == nil {
		return game.HoldemState{}, errors.New("selected candidate requires an actionable hand")
	}
	actingSeatIndex := *table.ActiveHand.State.ActingSeatIndex
	switch {
	case bundle.ActionRequest != nil:
		return game.ApplyHoldemAction(table.ActiveHand.State, actingSeatIndex, bundle.ActionRequest.Action)
	case bundle.TimeoutResolution != nil:
		action := game.Action{Type: game.ActionFold}
		if bundle.TimeoutResolution.ActionType == string(game.ActionCheck) {
			action = game.Action{Type: game.ActionCheck}
		}
		return game.ApplyHoldemAction(table.ActiveHand.State, actingSeatIndex, action)
	default:
		return game.HoldemState{}, errors.New("selected candidate is missing its action authorizer")
	}
}

func authorizerForCandidate(bundle NativeTurnCandidateBundle) *nativeTransitionAuthorizer {
	if bundle.ActionRequest != nil {
		return authorizerForActionRequest(*bundle.ActionRequest)
	}
	if strings.TrimSpace(bundle.TurnDeadlineAt) != "" {
		return &nativeTransitionAuthorizer{TurnDeadlineAt: bundle.TurnDeadlineAt}
	}
	return nil
}

func (runtime *meshRuntime) executePreSignedCandidateBatch(table nativeTableState, bundle NativeTurnCandidateBundle) (*custodyBatchResult, error) {
	signingTransition, plan, err := runtime.normalizedCustodySigningTransition(table, bundle.Transition)
	if err != nil {
		return nil, err
	}
	if len(plan.Inputs) == 0 {
		return nil, nil
	}
	approvalTransition, _, err := runtime.normalizedCustodyApprovalTransition(table, bundle.Transition)
	if err != nil {
		return nil, err
	}
	requestHash := approvalTransition.Proof.RequestHash
	if runtime.custodyBatchExecute != nil || runtime.config.UseMockSettlement {
		return runtime.executeCustodyBatch(table, bundle.Transition.PrevStateHash, requestHash, signingTransition, authorizerForCandidate(bundle), plan.Inputs, plan.ProofSignerIDs, plan.TreeSignerIDs, plan.AuthorizedOutputs)
	}
	signerSessions, signerPubkeys, derivationPath, err := runtime.prepareCustodyBatchSigners(table, bundle.Transition.PrevStateHash, requestHash, signingTransition, authorizerForCandidate(bundle), plan.TreeSignerIDs, plan.AuthorizedOutputs)
	if err != nil {
		return nil, err
	}
	onchainOutputIndexes := custodyOnchainOutputIndexes(plan.AuthorizedOutputs)
	cosignerPubkeys := sortedSignerPubkeys(signerPubkeys)
	if bundle.RegisterMessage != "" {
		if err := validateCustodyRegisterMessage(bundle.RegisterMessage, onchainOutputIndexes, cosignerPubkeys); err != nil {
			return nil, err
		}
	}
	message, err := custodyRegisterMessage(onchainOutputIndexes, cosignerPubkeys)
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

	intentID, err := transport.RegisterIntent(ctx, bundle.SignedProofPSBT, firstNonEmptyString(bundle.RegisterMessage, message))
	if err != nil {
		return nil, err
	}
	topics := custodyBatchTopics(plan.Inputs, signerPubkeys)
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
		runtime:            runtime,
		table:              table,
		prevStateHash:      bundle.Transition.PrevStateHash,
		requestKey:         requestHash,
		transition:         signingTransition,
		authorizer:         authorizerForCandidate(bundle),
		plan:               &custodySettlementPlan{Inputs: append([]custodyInputSpec(nil), plan.Inputs...)},
		transport:          transport,
		arkConfig:          arkConfig,
		intentID:           intentID,
		derivationPath:     derivationPath,
		signerSessions:     signerSessions,
		signerPubkeys:      signerPubkeys,
		outputCount:        len(plan.AuthorizedOutputs),
		intentRegisteredAt: time.Now(),
	}
	options := []arksdk.BatchSessionOption{}
	if !custodyOutputsRequireTreeSigning(plan.AuthorizedOutputs) {
		options = append(options, arksdk.WithSkipVtxoTreeSigning())
	}
	arkTxID, err := arksdk.JoinBatchSession(ctx, eventsCh, handler, options...)
	if err != nil {
		return nil, err
	}
	finalizedAt := nowISO()
	outputRefs, err := matchCustodyBatchOutputRefs(intentID, arkTxID, finalizedAt, handler.batchExpiry, plan.AuthorizedOutputs, handler.finalVtxoTree)
	if err != nil {
		return nil, err
	}
	return &custodyBatchResult{
		ArkTxID:          arkTxID,
		BatchExpiryType:  custodyBatchExpiryType(handler.batchExpiry),
		BatchExpiryValue: handler.batchExpiry.Value,
		CommitmentTx:     handler.commitmentTx,
		ConnectorTree:    mustSerializeTxTree(handler.finalConnectorTree),
		FinalizedAt:      finalizedAt,
		IntentID:         intentID,
		OutputRefs:       outputRefs,
		ProofPSBT:        bundle.SignedProofPSBT,
		VtxoTree:         mustSerializeTxTree(handler.finalVtxoTree),
	}, nil
}

func (runtime *meshRuntime) settleTurnCandidateTransition(table nativeTableState, bundle NativeTurnCandidateBundle) (tablecustody.CustodyTransition, game.HoldemState, error) {
	nextHand, err := nextHandStateForCandidate(table, bundle)
	if err != nil {
		return tablecustody.CustodyTransition{}, game.HoldemState{}, err
	}
	transition := cloneJSON(bundle.Transition)
	if candidateRequiresIntentAck(bundle) {
		result, err := runtime.executePreSignedCandidateBatch(table, bundle)
		if err != nil {
			return tablecustody.CustodyTransition{}, game.HoldemState{}, err
		}
		if result != nil {
			_, plan, err := runtime.normalizedCustodySigningTransition(table, transition)
			if err != nil {
				return tablecustody.CustodyTransition{}, game.HoldemState{}, err
			}
			applyTransitionSettlementPlan(&transition, plan, result.OutputRefs)
			transition.ArkIntentID = result.IntentID
			transition.ArkTxID = result.ArkTxID
			transition.Proof.ArkIntentID = result.IntentID
			transition.Proof.ArkTxID = result.ArkTxID
			transition.Proof.FinalizedAt = result.FinalizedAt
			transition.Proof.SettlementWitness = custodySettlementWitnessFromResult(result)
		}
	}
	transition.Proof.ReplayValidated = true
	transition.Proof.TurnAnchorHash = bundle.TurnAnchorHash
	transition.Proof.TurnCandidateHash = bundle.CandidateHash
	transition.Proof.VTXORefs = stackProofRefs(transition.NextState)
	if transition.Proof.FinalizedAt == "" {
		transition.Proof.FinalizedAt = nowISO()
	}
	transition.NextState.StateHash = tablecustody.HashCustodyState(transition.NextState)
	transition.NextStateHash = transition.NextState.StateHash
	transition.Proof.StateHash = transition.NextStateHash
	transition.Proof.TransitionHash = tablecustody.HashCustodyTransition(transition)
	if err := tablecustody.ValidateTransition(table.LatestCustodyState, transition); err != nil {
		return tablecustody.CustodyTransition{}, game.HoldemState{}, err
	}
	return transition, nextHand, nil
}

func (runtime *meshRuntime) validateLockedActionSettlement(table nativeTableState, bundle NativeTurnCandidateBundle, transition tablecustody.CustodyTransition) error {
	if transition.Kind != tablecustody.TransitionKindAction {
		return errors.New("locked action settlement must finalize an action transition")
	}
	if transition.Proof.TurnCandidateHash != "" && transition.Proof.TurnCandidateHash != bundle.CandidateHash {
		return errors.New("locked action settlement candidate hash mismatch")
	}
	if transition.Proof.TurnAnchorHash != "" && transition.Proof.TurnAnchorHash != bundle.TurnAnchorHash {
		return errors.New("locked action settlement turn anchor mismatch")
	}
	if !reflect.DeepEqual(transition.Approvals, bundle.Transition.Approvals) {
		return errors.New("locked action settlement approvals do not match the prebuilt bundle")
	}
	if transition.Proof.RequestHash != bundle.Transition.Proof.RequestHash {
		return errors.New("locked action settlement request hash does not match the prebuilt bundle")
	}
	if !reflect.DeepEqual(transition.Proof.Signatures, bundle.Transition.Proof.Signatures) {
		return errors.New("locked action settlement signatures do not match the prebuilt bundle")
	}
	if err := runtime.validateCustodyTransitionSemantics(table, transition, authorizerForCandidate(bundle)); err != nil {
		return err
	}
	if err := tablecustody.ValidateTransition(table.LatestCustodyState, transition); err != nil {
		return err
	}
	if candidateRequiresIntentAck(bundle) {
		if transition.Proof.SettlementWitness == nil {
			return errors.New("locked action settlement is missing its settlement witness")
		}
		if runtime.config.UseMockSettlement {
			return nil
		}
		return runtime.validateAcceptedCustodySettlementWitness(table, table.LatestCustodyState, transition)
	}
	if transition.Proof.SettlementWitness != nil {
		return errors.New("locked action settlement unexpectedly includes a settlement witness")
	}
	return nil
}

func (runtime *meshRuntime) publishLockedActionTransitionLocked(table *nativeTableState, bundle NativeTurnCandidateBundle, transition tablecustody.CustodyTransition) (bool, error) {
	if table == nil {
		return false, nil
	}
	actingSeatIndex := -1
	if table.ActiveHand != nil && table.ActiveHand.State.ActingSeatIndex != nil {
		actingSeatIndex = *table.ActiveHand.State.ActingSeatIndex
	}
	nextHand, err := nextHandStateForCandidate(*table, bundle)
	if err != nil {
		return false, err
	}
	if err := runtime.attachDeterministicRecoveryBundles(*table, &transition, authorizerForCandidate(bundle), &nextHand); err != nil {
		return false, err
	}
	table.ActiveHand.State = nextHand
	runtime.applyCustodyTransition(table, transition)
	switch {
	case bundle.ActionRequest != nil:
		seat, _ := seatRecordForPlayer(*table, bundle.ActionRequest.PlayerID)
		if err := runtime.appendEvent(table, map[string]any{
			"actionRequest":  rawJSONMap(*bundle.ActionRequest),
			"custodySeq":     transition.CustodySeq,
			"playerId":       bundle.ActionRequest.PlayerID,
			"seatIndex":      seat.SeatIndex,
			"transitionHash": transition.Proof.TransitionHash,
			"type":           "PlayerAction",
		}); err != nil {
			return false, err
		}
	default:
		if err := runtime.appendEvent(table, map[string]any{
			"custodySeq":        transition.CustodySeq,
			"playerId":          bundle.TimeoutResolution.ActingPlayerID,
			"seatIndex":         actingSeatIndex,
			"timeoutResolution": rawJSONMap(*bundle.TimeoutResolution),
			"transitionHash":    transition.Proof.TransitionHash,
			"type":              "PlayerAction",
		}); err != nil {
			return false, err
		}
	}
	if err := runtime.advanceHandProtocolLocked(table); err != nil {
		return false, err
	}
	return true, nil
}

func (runtime *meshRuntime) handleLockedActionSettlementTimeoutLocked(table *nativeTableState) (bool, error) {
	if table == nil || !turnMenuMatchesTable(*table, table.PendingTurnMenu) || table.PendingTurnMenu == nil {
		return false, nil
	}
	menu := table.PendingTurnMenu
	if strings.TrimSpace(menu.SelectedCandidateHash) == "" || menu.SelectedBundle == nil {
		return false, nil
	}
	if strings.TrimSpace(menu.SettlementDeadlineAt) == "" || elapsedMillis(menu.SettlementDeadlineAt) < 0 {
		return false, nil
	}
	transition, _, err := runtime.settleTurnCandidateTransition(*table, *menu.SelectedBundle)
	if err != nil {
		return false, err
	}
	return runtime.publishLockedActionTransitionLocked(table, *menu.SelectedBundle, transition)
}

func (runtime *meshRuntime) buildActionSelectionRequest(table nativeTableState, action game.Action) (nativeActionChooseRequest, error) {
	if turnChallengeMatchesTable(table, table.PendingTurnChallenge) {
		return nativeActionChooseRequest{}, errors.New("turn challenge is open for this turn; ordinary SendAction is disabled")
	}
	if err := runtime.validatePendingTurnMenu(table, table.PendingTurnMenu); err != nil {
		return nativeActionChooseRequest{}, errors.New("turn menu is not available for the current action")
	}
	if !turnMenuMatchesTable(table, table.PendingTurnMenu) {
		return nativeActionChooseRequest{}, errors.New("turn menu is not available for the current action")
	}
	if strings.TrimSpace(table.PendingTurnMenu.SelectedCandidateHash) != "" {
		return nativeActionChooseRequest{}, errors.New("this turn already has a selected candidate")
	}
	option, ok := findTurnMenuOptionByAction(table.PendingTurnMenu, action)
	if !ok {
		return nativeActionChooseRequest{}, errors.New("action is not one of the deterministic menu options for this turn")
	}
	cache, err := runtime.loadLocalTurnBundleCache(table)
	if err != nil {
		return nativeActionChooseRequest{}, err
	}
	bundle, ok := findTurnCandidateByOptionFromCache(table.PendingTurnMenu, cache, option.OptionID)
	if !ok {
		return nativeActionChooseRequest{}, errors.New("selected action bundle is missing from the local turn bundle cache")
	}
	auth, err := runtime.buildSelectionAuth(table, bundle.CandidateHash)
	if err != nil {
		return nativeActionChooseRequest{}, err
	}
	return nativeActionChooseRequest{
		CandidateHash: bundle.CandidateHash,
		SelectionAuth: auth,
		TableID:       table.Config.TableID,
	}, nil
}

func (runtime *meshRuntime) handleActionSelectionFromPeer(request nativeActionChooseRequest) (updated nativeActionChooseResponse, err error) {
	if strings.TrimSpace(request.TableID) == "" {
		request.TableID = request.SelectionAuth.TableID
	}
	timingFields := meshTimingFields{
		Metric:  "action_transition_total",
		TableID: request.TableID,
		Purpose: "candidate-lock",
	}
	timing := startMeshTiming(timingFields)
	defer func() {
		timing.EndWith(timingFields, err)
	}()
	err = runtime.store.withTableLock(request.TableID, func() error {
		table, err := runtime.store.readTable(request.TableID)
		if err != nil || table == nil {
			return fmt.Errorf("table %s not found", request.TableID)
		}
		if table.CurrentHost.Peer.PeerID != runtime.selfPeerID() {
			return errors.New("action selection must be sent to the current host")
		}
		if turnChallengeMatchesTable(*table, table.PendingTurnChallenge) {
			return errors.New("turn challenge is open for this turn")
		}
		if err := runtime.ensurePendingTurnMenuLocked(table); err != nil {
			return err
		}
		if err := runtime.validatePendingTurnMenu(*table, table.PendingTurnMenu); err != nil {
			return errors.New("turn menu is unavailable for the current table state")
		}
		if err := runtime.verifySelectionAuth(*table, request.SelectionAuth); err != nil {
			return err
		}
		cache, err := runtime.loadLocalTurnBundleCache(*table)
		if err != nil {
			return err
		}
		expected, ok := findTurnCandidateByHashFromCache(table.PendingTurnMenu, cache, request.CandidateHash)
		if !ok {
			return errors.New("selected candidate is not part of the current turn menu")
		}
		if strings.TrimSpace(table.PendingTurnMenu.SelectedCandidateHash) != "" && table.PendingTurnMenu.SelectedCandidateHash != expected.CandidateHash {
			return errors.New("a sibling candidate for this turn is already selected")
		}
		lockedAt := nowISO()
		ack, err := runtime.buildActionLockedAck(*table, request.SelectionAuth, lockedAt)
		if err != nil {
			return err
		}
		table.PendingTurnMenu.SelectedCandidateHash = expected.CandidateHash
		table.PendingTurnMenu.SelectionAuth = cloneJSON(&request.SelectionAuth)
		table.PendingTurnMenu.LockedAt = lockedAt
		table.PendingTurnMenu.SettlementDeadlineAt = addMillis(lockedAt, runtime.selectionSettlementTimeoutMSForTable(*table))
		selectedBundle := cloneJSON(expected)
		table.PendingTurnMenu.SelectedBundle = &selectedBundle
		table.LocalTurnBundleCache = nil
		if err := runtime.storeLocalTurnBundleCache(request.TableID, nil); err != nil {
			return err
		}
		if err := runtime.persistAndReplicate(table, true); err != nil {
			return err
		}
		updated = nativeActionChooseResponse{
			LockedAck: ack,
			Table:     runtime.networkTableView(*table, request.SelectionAuth.PlayerID),
		}
		return nil
	})
	return updated, err
}

func (runtime *meshRuntime) buildActionSettlementRequest(table nativeTableState) (nativeActionSettlementRequest, error) {
	if !turnMenuMatchesTable(table, table.PendingTurnMenu) || table.PendingTurnMenu == nil {
		return nativeActionSettlementRequest{}, errors.New("turn menu is not available for settlement")
	}
	if strings.TrimSpace(table.PendingTurnMenu.SelectedCandidateHash) == "" || table.PendingTurnMenu.SelectedBundle == nil {
		return nativeActionSettlementRequest{}, errors.New("this turn does not have a locked action")
	}
	transition, _, err := runtime.settleTurnCandidateTransition(table, *table.PendingTurnMenu.SelectedBundle)
	if err != nil {
		return nativeActionSettlementRequest{}, err
	}
	return nativeActionSettlementRequest{
		CandidateHash:   table.PendingTurnMenu.SelectedCandidateHash,
		PlayerID:        runtime.walletID.PlayerID,
		ProtocolVersion: nativeProtocolVersion,
		TableID:         table.Config.TableID,
		Transition:      transition,
	}, nil
}

func (runtime *meshRuntime) handleActionSettlementFromPeer(request nativeActionSettlementRequest) (updated nativeActionSettlementResponse, err error) {
	err = runtime.store.withTableLock(request.TableID, func() error {
		table, err := runtime.store.readTable(request.TableID)
		if err != nil || table == nil {
			return fmt.Errorf("table %s not found", request.TableID)
		}
		if table.CurrentHost.Peer.PeerID != runtime.selfPeerID() {
			return errors.New("action settlement must be sent to the current host")
		}
		if !turnMenuMatchesTable(*table, table.PendingTurnMenu) || table.PendingTurnMenu == nil {
			return errors.New("turn menu is unavailable for the current table state")
		}
		if strings.TrimSpace(table.PendingTurnMenu.SelectedCandidateHash) == "" || table.PendingTurnMenu.SelectedBundle == nil {
			return errors.New("this turn does not have a locked action")
		}
		if request.CandidateHash != table.PendingTurnMenu.SelectedCandidateHash {
			return errors.New("action settlement candidate does not match the locked action")
		}
		if request.PlayerID != table.PendingTurnMenu.ActingPlayerID {
			return errors.New("action settlement player mismatch")
		}
		if err := validateSettlementRequestProtocolVersion(request.ProtocolVersion); err != nil {
			return err
		}
		if err := runtime.validateLockedActionSettlement(*table, *table.PendingTurnMenu.SelectedBundle, request.Transition); err != nil {
			return err
		}
		published, err := runtime.publishLockedActionTransitionLocked(table, *table.PendingTurnMenu.SelectedBundle, request.Transition)
		if err != nil {
			return err
		}
		if !published {
			return errors.New("locked action settlement did not publish")
		}
		if err := runtime.persistAndReplicate(table, true); err != nil {
			return err
		}
		updated = nativeActionSettlementResponse{Table: runtime.networkTableView(*table, request.PlayerID)}
		return nil
	})
	return updated, err
}
