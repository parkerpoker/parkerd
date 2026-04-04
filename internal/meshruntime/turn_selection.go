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

func (runtime *meshRuntime) hasTimelySelectedCandidate(table nativeTableState) bool {
	if !turnMenuMatchesTable(table, table.PendingTurnMenu) {
		return false
	}
	menu := table.PendingTurnMenu
	if strings.TrimSpace(menu.SelectedCandidateHash) == "" {
		return false
	}
	bundle, ok := findTurnCandidateByHash(menu, menu.SelectedCandidateHash)
	if !ok {
		return false
	}
	if !candidateRequiresIntentAck(bundle) {
		return true
	}
	if menu.AcceptedIntentAck == nil {
		return false
	}
	if err := runtime.verifyCandidateIntentAck(table, bundle, *menu.AcceptedIntentAck); err != nil {
		return false
	}
	return acceptedBeforeOrAtDeadline(menu.AcceptedIntentAck.AcceptedAt, menu.ActionDeadlineAt)
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

func (runtime *meshRuntime) executePreSignedCandidateBatch(table nativeTableState, bundle NativeTurnCandidateBundle, ack *tablecustody.CandidateIntentAck) (*custodyBatchResult, error) {
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

	intentID := ""
	if ack != nil {
		intentID = ack.IntentID
	}
	if strings.TrimSpace(intentID) == "" {
		intentID, err = transport.RegisterIntent(ctx, bundle.SignedProofPSBT, firstNonEmptyString(bundle.RegisterMessage, message))
		if err != nil {
			return nil, err
		}
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

func (runtime *meshRuntime) finalizeSelectedTurnCandidateLocked(table *nativeTableState) (bool, error) {
	if table == nil || !turnMenuMatchesTable(*table, table.PendingTurnMenu) || strings.TrimSpace(table.PendingTurnMenu.SelectedCandidateHash) == "" {
		return false, nil
	}
	menu := table.PendingTurnMenu
	bundle, ok := findTurnCandidateByHash(menu, menu.SelectedCandidateHash)
	if !ok {
		return false, errors.New("selected turn candidate is missing from the pending menu")
	}
	actingSeatIndex := -1
	if table.ActiveHand != nil && table.ActiveHand.State.ActingSeatIndex != nil {
		actingSeatIndex = *table.ActiveHand.State.ActingSeatIndex
	}
	if candidateRequiresIntentAck(bundle) {
		if menu.AcceptedIntentAck != nil {
			if err := runtime.verifyCandidateIntentAck(*table, bundle, *menu.AcceptedIntentAck); err != nil {
				return false, err
			}
		}
	}
	nextHand, err := nextHandStateForCandidate(*table, bundle)
	if err != nil {
		return false, err
	}
	transition := cloneJSON(bundle.Transition)
	if candidateRequiresIntentAck(bundle) {
		var intentAck *tablecustody.CandidateIntentAck
		if menu.AcceptedIntentAck != nil {
			intentAck = menu.AcceptedIntentAck
		}
		result, err := runtime.executePreSignedCandidateBatch(*table, bundle, intentAck)
		if err != nil {
			return false, err
		}
		if result != nil {
			signingTransition, plan, err := runtime.normalizedCustodySigningTransition(*table, transition)
			if err != nil {
				return false, err
			}
			_ = signingTransition
			applyTransitionSettlementPlan(&transition, plan, result.OutputRefs)
			transition.ArkIntentID = result.IntentID
			transition.ArkTxID = result.ArkTxID
			transition.Proof.ArkIntentID = result.IntentID
			transition.Proof.ArkTxID = result.ArkTxID
			transition.Proof.FinalizedAt = result.FinalizedAt
			transition.Proof.SettlementWitness = custodySettlementWitnessFromResult(result)
		}
	}
	if menu.AcceptedIntentAck != nil {
		transition.Proof.CandidateIntentAck = menu.AcceptedIntentAck
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

func (runtime *meshRuntime) buildActionSelectionRequest(table nativeTableState, action game.Action) (nativeActionSelectionRequest, error) {
	if turnChallengeMatchesTable(table, table.PendingTurnChallenge) {
		return nativeActionSelectionRequest{}, errors.New("turn challenge is open for this turn; ordinary SendAction is disabled")
	}
	if err := runtime.validatePendingTurnMenu(table, table.PendingTurnMenu); err != nil {
		return nativeActionSelectionRequest{}, errors.New("turn menu is not available for the current action")
	}
	if !turnMenuMatchesTable(table, table.PendingTurnMenu) {
		return nativeActionSelectionRequest{}, errors.New("turn menu is not available for the current action")
	}
	if strings.TrimSpace(table.PendingTurnMenu.SelectedCandidateHash) != "" {
		return nativeActionSelectionRequest{}, errors.New("this turn already has a selected candidate")
	}
	option, ok := findTurnMenuOptionByAction(table.PendingTurnMenu, action)
	if !ok {
		return nativeActionSelectionRequest{}, errors.New("action is not one of the deterministic menu options for this turn")
	}
	bundle, ok := findTurnCandidateByOption(table.PendingTurnMenu, option.OptionID)
	if !ok {
		return nativeActionSelectionRequest{}, errors.New("selected action bundle is missing from the pending turn menu")
	}
	return nativeActionSelectionRequest{
		Candidate: bundle,
		TableID:   table.Config.TableID,
	}, nil
}

func (runtime *meshRuntime) handleActionSelectionFromPeer(request nativeActionSelectionRequest) (updated nativeTableState, err error) {
	if strings.TrimSpace(request.TableID) == "" {
		request.TableID = request.Candidate.Transition.TableID
	}
	timingFields := meshTimingFields{
		Metric:  "action_transition_total",
		TableID: request.TableID,
		Purpose: "candidate-select",
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
		expected, ok := findTurnCandidateByHash(table.PendingTurnMenu, request.Candidate.CandidateHash)
		if !ok {
			return errors.New("selected candidate is not part of the current turn menu")
		}
		if !reflect.DeepEqual(cloneJSON(expected), cloneJSON(request.Candidate)) {
			return errors.New("selected candidate does not match the current prebuilt turn bundle")
		}
		if strings.TrimSpace(table.PendingTurnMenu.SelectedCandidateHash) != "" && table.PendingTurnMenu.SelectedCandidateHash != expected.CandidateHash {
			return errors.New("a sibling candidate for this turn is already selected")
		}
		table.PendingTurnMenu.SelectedCandidateHash = expected.CandidateHash
		if candidateRequiresIntentAck(expected) && table.PendingTurnMenu.AcceptedIntentAck == nil {
			ack, err := runtime.registerCandidateIntent(*table, expected)
			if err != nil {
				return err
			}
			if ack != nil {
				table.PendingTurnMenu.AcceptedIntentAck = ack
			}
		}
		completed, finalizeErr := runtime.finalizeSelectedTurnCandidateLocked(table)
		if finalizeErr != nil {
			if !runtime.hasTimelySelectedCandidate(*table) {
				return finalizeErr
			}
			debugMeshf("selected turn candidate deferred table=%s candidate=%s err=%v", table.Config.TableID, expected.CandidateHash, finalizeErr)
		}
		if completed {
			timingFields.CustodySeq = table.LatestCustodyState.CustodySeq
		}
		if err := runtime.persistAndReplicate(table, true); err != nil {
			return err
		}
		updated = *table
		return nil
	})
	return updated, err
}

func (runtime *meshRuntime) persistDirectActionSelection(tableID string, selection nativeActionSelectionRequest) (updated nativeTableState, err error) {
	err = runtime.store.withTableLock(tableID, func() error {
		table, err := runtime.store.readTable(tableID)
		if err != nil || table == nil {
			return fmt.Errorf("table %s not found", tableID)
		}
		if turnChallengeMatchesTable(*table, table.PendingTurnChallenge) {
			return errors.New("turn challenge is open for this turn")
		}
		if err := runtime.validatePendingTurnMenu(*table, table.PendingTurnMenu); err != nil {
			return errors.New("turn menu is unavailable for direct action fallback")
		}
		expected, ok := findTurnCandidateByHash(table.PendingTurnMenu, selection.Candidate.CandidateHash)
		if !ok {
			return errors.New("selected candidate is not part of the current turn menu")
		}
		if !reflect.DeepEqual(cloneJSON(expected), cloneJSON(selection.Candidate)) {
			return errors.New("direct action fallback candidate does not match the prebuilt turn bundle")
		}
		if strings.TrimSpace(table.PendingTurnMenu.SelectedCandidateHash) != "" && table.PendingTurnMenu.SelectedCandidateHash != expected.CandidateHash {
			return errors.New("a sibling candidate for this turn is already selected")
		}
		table.PendingTurnMenu.SelectedCandidateHash = expected.CandidateHash
		if candidateRequiresIntentAck(expected) && table.PendingTurnMenu.AcceptedIntentAck == nil {
			ack, err := runtime.registerCandidateIntent(*table, expected)
			if err != nil {
				return err
			}
			if ack != nil {
				table.PendingTurnMenu.AcceptedIntentAck = ack
			}
		}
		if _, err := runtime.finalizeSelectedTurnCandidateLocked(table); err != nil {
			return err
		}
		if err := runtime.persistLocalTable(table, false); err != nil {
			return err
		}
		updated = *table
		return nil
	})
	if err == nil {
		runtime.replicateTable(updated)
	}
	return updated, err
}
