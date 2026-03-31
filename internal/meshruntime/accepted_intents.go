package meshruntime

import (
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/parkerpoker/parkerd/internal/game"
	"github.com/parkerpoker/parkerd/internal/settlementcore"
	"github.com/parkerpoker/parkerd/internal/tablecustody"
)

func verifyNativeActionRequestSignature(seat nativeSeatRecord, request nativeActionRequest) error {
	if request.SignedAt == "" || request.SignatureHex == "" {
		return errors.New("action request is missing player signature")
	}
	ok, err := settlementcore.VerifyStructuredData(seat.WalletPubkeyHex, nativeActionAuthPayload(request.TableID, request.PlayerID, request.HandID, request.PrevCustodyStateHash, request.ChallengeAnchor, request.TranscriptRoot, request.Epoch, request.DecisionIndex, request.Action, request.SignedAt), request.SignatureHex)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("action request signature is invalid")
	}
	return nil
}

func verifyNativeFundsRequestSignature(seat nativeSeatRecord, request nativeFundsRequest) error {
	if request.SignedAt == "" || request.SignatureHex == "" {
		return errors.New("funds request is missing player signature")
	}
	ok, err := settlementcore.VerifyStructuredData(seat.WalletPubkeyHex, nativeFundsAuthPayload(request.TableID, request.PlayerID, request.PrevCustodyStateHash, request.Kind, request.ArkAddress, request.Epoch, request.SignedAt), request.SignatureHex)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("funds request signature is invalid")
	}
	return nil
}

func eventCustodySeq(event NativeSignedTableEvent) (int, error) {
	value, err := decodeJSONValue[int](event.Body["custodySeq"])
	if err != nil {
		return 0, err
	}
	return value, nil
}

func eventTransitionHash(event NativeSignedTableEvent) string {
	return strings.TrimSpace(stringValue(event.Body["transitionHash"]))
}

func actionRequestFromEvent(event NativeSignedTableEvent) (*nativeActionRequest, bool, error) {
	raw, ok := event.Body["actionRequest"]
	if !ok || raw == nil {
		return nil, false, nil
	}
	request, err := decodeJSONValue[nativeActionRequest](raw)
	if err != nil {
		return nil, true, err
	}
	return &request, true, nil
}

func fundsRequestFromEvent(event NativeSignedTableEvent) (*nativeFundsRequest, bool, error) {
	raw, ok := event.Body["fundsRequest"]
	if !ok || raw == nil {
		return nil, false, nil
	}
	request, err := decodeJSONValue[nativeFundsRequest](raw)
	if err != nil {
		return nil, true, err
	}
	return &request, true, nil
}

func timeoutResolutionFromEvent(event NativeSignedTableEvent) (*tablecustody.TimeoutResolution, bool, error) {
	raw, ok := event.Body["timeoutResolution"]
	if !ok || raw == nil {
		return nil, false, nil
	}
	resolution, err := decodeJSONValue[tablecustody.TimeoutResolution](raw)
	if err != nil {
		return nil, true, err
	}
	return &resolution, true, nil
}

func actionEventHandID(event NativeSignedTableEvent) (string, error) {
	request, ok, err := actionRequestFromEvent(event)
	if err != nil {
		return "", err
	}
	if ok {
		return request.HandID, nil
	}
	return stringValue(event.HandID), nil
}

func linkedCustodyTransitionByHashSeq(table nativeTableState, transitionHash string, custodySeq int) (tablecustody.CustodyTransition, int, bool) {
	for index, transition := range table.CustodyTransitions {
		if transition.CustodySeq != custodySeq {
			continue
		}
		if transition.Proof.TransitionHash != transitionHash {
			continue
		}
		return transition, index, true
	}
	return tablecustody.CustodyTransition{}, -1, false
}

func linkedCustodyTransitionForEvent(table nativeTableState, event NativeSignedTableEvent) (tablecustody.CustodyTransition, int, error) {
	transitionHash := eventTransitionHash(event)
	if transitionHash == "" {
		return tablecustody.CustodyTransition{}, -1, fmt.Errorf("%s event is missing transition hash", stringValue(event.Body["type"]))
	}
	custodySeq, err := eventCustodySeq(event)
	if err != nil {
		return tablecustody.CustodyTransition{}, -1, fmt.Errorf("%s event custody seq is invalid: %w", stringValue(event.Body["type"]), err)
	}
	transition, index, ok := linkedCustodyTransitionByHashSeq(table, transitionHash, custodySeq)
	if !ok {
		return tablecustody.CustodyTransition{}, -1, fmt.Errorf("%s event does not match accepted custody history", stringValue(event.Body["type"]))
	}
	return transition, index, nil
}

func previousCustodyStateForTransition(table nativeTableState, index int) *tablecustody.CustodyState {
	if index <= 0 || index > len(table.CustodyTransitions)-1 {
		return nil
	}
	state := table.CustodyTransitions[index-1].NextState
	return &state
}

func (runtime *meshRuntime) handStartTransitionForHand(table nativeTableState, handID string) (tablecustody.CustodyTransition, *tablecustody.CustodyState, string, bool, error) {
	for _, event := range table.Events {
		if stringValue(event.Body["type"]) != "HandStart" {
			continue
		}
		if strings.TrimSpace(stringValue(event.Body["handId"])) != handID {
			continue
		}
		transition, index, err := linkedCustodyTransitionForEvent(table, event)
		if err != nil {
			return tablecustody.CustodyTransition{}, nil, "", false, err
		}
		if transition.Kind != tablecustody.TransitionKindBlindPost {
			return tablecustody.CustodyTransition{}, nil, "", false, fmt.Errorf("hand %s start event links %s instead of blind-post", handID, transition.Kind)
		}
		return transition, previousCustodyStateForTransition(table, index), strings.TrimSpace(stringValue(event.PrevEventHash)), true, nil
	}
	for _, transition := range table.CustodyTransitions {
		if transition.Kind == tablecustody.TransitionKindBlindPost && transition.NextState.HandID == handID {
			return tablecustody.CustodyTransition{}, nil, "", false, fmt.Errorf("hand %s is missing HandStart event", handID)
		}
	}
	return tablecustody.CustodyTransition{}, nil, "", false, nil
}

func handResultPublicStateForStateHash(table nativeTableState, stateHash string) (*NativePublicTableState, bool, error) {
	trimmedStateHash := strings.TrimSpace(stateHash)
	if trimmedStateHash == "" {
		return nil, false, nil
	}
	for index, event := range table.Events {
		if stringValue(event.Body["type"]) != "HandResult" {
			continue
		}
		if strings.TrimSpace(stringValue(event.Body["latestCustodyStateHash"])) != trimmedStateHash {
			continue
		}
		rawPublicState, ok := event.Body["publicState"]
		if !ok || rawPublicState == nil {
			return nil, true, fmt.Errorf("hand result event %d is missing public state", index)
		}
		publicState, err := decodeJSONValue[NativePublicTableState](rawPublicState)
		if err != nil {
			return nil, true, err
		}
		return &publicState, true, nil
	}
	return nil, false, nil
}

func restoreAcceptedSeatStatusesFromCustodyState(table *nativeTableState, state *tablecustody.CustodyState) {
	if table == nil || state == nil {
		return
	}
	statusByPlayerID := make(map[string]string, len(state.StackClaims))
	for _, claim := range state.StackClaims {
		status := strings.TrimSpace(claim.Status)
		if status == "" {
			continue
		}
		statusByPlayerID[claim.PlayerID] = status
	}
	for index := range table.Seats {
		status, ok := statusByPlayerID[table.Seats[index].PlayerID]
		if !ok {
			continue
		}
		table.Seats[index].Status = status
		table.Seats[index].NativeSeatedPlayer.Status = status
	}
}

func (runtime *meshRuntime) acceptedTableBeforeFundsTransition(table nativeTableState, event NativeSignedTableEvent, transition tablecustody.CustodyTransition, transitionIndex int) (nativeTableState, error) {
	previousState := previousCustodyStateForTransition(table, transitionIndex)
	if previousState == nil {
		return nativeTableState{}, errors.New("funds event is missing its previous custody state")
	}

	baseTable := cloneJSON(table)
	baseTable.ActiveHand = nil
	baseTable.ActiveHandStartAt = ""
	baseTable.CurrentEpoch = transition.NextState.Epoch
	baseTable.LastEventHash = firstNonEmptyString(strings.TrimSpace(stringValue(event.PrevEventHash)), previousState.ChallengeAnchor)
	baseTable.LatestCustodyState = previousState
	restoreAcceptedSeatStatusesFromCustodyState(&baseTable, previousState)

	if strings.TrimSpace(previousState.TranscriptRoot) != "" || strings.TrimSpace(previousState.HandID) != "" {
		baseTable.ActiveHand = &nativeActiveHand{
			Cards: nativeHandCardState{
				FinalDeck:       nil,
				PhaseDeadlineAt: previousState.ActionDeadlineAt,
				Transcript: game.HandTranscript{
					HandID:     previousState.HandID,
					HandNumber: previousState.HandNumber,
					Records:    []game.HandTranscriptRecord{},
					RootHash:   previousState.TranscriptRoot,
					TableID:    table.Config.TableID,
				},
			},
		}
	}

	if publicState, ok, err := handResultPublicStateForStateHash(table, previousState.StateHash); err != nil {
		return nativeTableState{}, err
	} else if ok {
		baseTable.PublicState = publicState
		baseTable.Config.Status = firstNonEmptyString(stringValue(publicState.Status), baseTable.Config.Status)
		return baseTable, nil
	}

	status := "seating"
	if activeCustodySeatCount(baseTable) >= 2 {
		status = "ready"
	}
	baseTable.Config.Status = status
	if status == "ready" && strings.TrimSpace(previousState.HandID) == "" && previousState.HandNumber == 0 {
		readyState := runtime.buildReadyPublicState(baseTable)
		baseTable.PublicState = &readyState
		return baseTable, nil
	}
	baseTable.PublicState = runtime.publicStateFromLatestCustody(baseTable, status)
	return baseTable, nil
}

func (runtime *meshRuntime) validateAcceptedInitiatorHistory(table nativeTableState) error {
	for index, event := range table.Events {
		switch stringValue(event.Body["type"]) {
		case "PlayerAction":
			transition, transitionIndex, err := linkedCustodyTransitionForEvent(table, event)
			if err != nil {
				return fmt.Errorf("event %d player action link is invalid: %w", index, err)
			}
			request, hasRequest, err := actionRequestFromEvent(event)
			if err != nil {
				return fmt.Errorf("event %d action request is invalid: %w", index, err)
			}
			if hasRequest {
				seat, ok := seatRecordForPlayer(table, request.PlayerID)
				if !ok {
					return fmt.Errorf("event %d action request player %s is not seated", index, request.PlayerID)
				}
				if err := verifyNativeActionRequestSignature(seat, *request); err != nil {
					return fmt.Errorf("event %d action request verification failed: %w", index, err)
				}
				if transition.Kind != tablecustody.TransitionKindAction {
					return fmt.Errorf("event %d action request links %s instead of action", index, transition.Kind)
				}
				previousState := previousCustodyStateForTransition(table, transitionIndex)
				if previousState == nil {
					return fmt.Errorf("event %d action request is missing its previous custody state", index)
				}
				if request.PrevCustodyStateHash != transition.PrevStateHash {
					return fmt.Errorf("event %d action request prev custody hash mismatch", index)
				}
				if request.Epoch != transition.NextState.Epoch {
					return fmt.Errorf("event %d action request epoch mismatch", index)
				}
				if request.DecisionIndex != previousState.DecisionIndex {
					return fmt.Errorf("event %d action request decision index mismatch", index)
				}
				if request.HandID != previousState.HandID {
					return fmt.Errorf("event %d action request hand mismatch", index)
				}
				if transition.Action == nil {
					return fmt.Errorf("event %d action transition is missing its action descriptor", index)
				}
				if transition.Action.Type != string(request.Action.Type) || transition.Action.TotalSats != request.Action.TotalSats {
					return fmt.Errorf("event %d action request does not match custody transition", index)
				}
				continue
			}
			if transition.Kind != tablecustody.TransitionKindTimeout {
				return fmt.Errorf("event %d player action is missing its signed action request", index)
			}
			resolution, hasResolution, err := timeoutResolutionFromEvent(event)
			if err != nil {
				return fmt.Errorf("event %d timeout resolution is invalid: %w", index, err)
			}
			if hasResolution {
				normalizedEvent := cloneTimeoutResolution(resolution)
				normalizedTransition := cloneTimeoutResolution(transition.TimeoutResolution)
				sortTimeoutResolution(normalizedEvent)
				sortTimeoutResolution(normalizedTransition)
				if !reflect.DeepEqual(normalizedEvent, normalizedTransition) {
					return fmt.Errorf("event %d timeout resolution does not match custody transition", index)
				}
			}
		case "CashOut", "EmergencyExit":
			transition, transitionIndex, err := linkedCustodyTransitionForEvent(table, event)
			if err != nil {
				return fmt.Errorf("event %d funds link is invalid: %w", index, err)
			}
			request, hasRequest, err := fundsRequestFromEvent(event)
			if err != nil {
				return fmt.Errorf("event %d funds request is invalid: %w", index, err)
			}
			if !hasRequest {
				return fmt.Errorf("event %d is missing its signed funds request", index)
			}
			seat, ok := seatRecordForPlayer(table, request.PlayerID)
			if !ok {
				return fmt.Errorf("event %d funds request player %s is not seated", index, request.PlayerID)
			}
			if err := verifyNativeFundsRequestSignature(seat, *request); err != nil {
				return fmt.Errorf("event %d funds request verification failed: %w", index, err)
			}
			expectedKind, _, err := fundsTransitionKindAndStatus(request.Kind)
			if err != nil {
				return fmt.Errorf("event %d funds request kind is invalid: %w", index, err)
			}
			if transition.Kind != expectedKind {
				return fmt.Errorf("event %d funds request links %s instead of %s", index, transition.Kind, expectedKind)
			}
			if request.PrevCustodyStateHash != transition.PrevStateHash {
				return fmt.Errorf("event %d funds request prev custody hash mismatch", index)
			}
			if request.Epoch != transition.NextState.Epoch {
				return fmt.Errorf("event %d funds request epoch mismatch", index)
			}
			if request.PlayerID != transition.ActingPlayerID {
				return fmt.Errorf("event %d funds request player mismatch", index)
			}
			baseTable, err := runtime.acceptedTableBeforeFundsTransition(table, event, transition, transitionIndex)
			if err != nil {
				return fmt.Errorf("event %d funds replay setup failed: %w", index, err)
			}
			if err := runtime.validateCustodyTransitionSemantics(baseTable, transition, authorizerForFundsRequest(*request)); err != nil {
				return fmt.Errorf("event %d funds transition does not match the locally derived successor: %w", index, err)
			}
		}
	}
	return nil
}

func (runtime *meshRuntime) acceptedReplayActionEvents(table nativeTableState, handID string) ([]NativeSignedTableEvent, error) {
	events := make([]NativeSignedTableEvent, 0)
	for index, event := range table.Events {
		if stringValue(event.Body["type"]) != "PlayerAction" {
			continue
		}
		eventHandID, err := actionEventHandID(event)
		if err != nil {
			return nil, fmt.Errorf("event %d hand id is invalid: %w", index, err)
		}
		if eventHandID != handID {
			continue
		}
		events = append(events, event)
	}
	return events, nil
}

func (runtime *meshRuntime) applyAcceptedReplayActionEvent(table nativeTableState, hand game.HoldemState, event NativeSignedTableEvent) (game.HoldemState, *tablecustody.CustodyState, error) {
	transition, index, err := linkedCustodyTransitionForEvent(table, event)
	if err != nil {
		return game.HoldemState{}, nil, err
	}
	previousState := previousCustodyStateForTransition(table, index)
	if previousState == nil {
		return game.HoldemState{}, nil, errors.New("player action event is missing its previous custody state")
	}
	baseTable := cloneJSON(table)
	challengeAnchor := previousState.ChallengeAnchor
	transcriptRoot := previousState.TranscriptRoot
	baseTable.CurrentEpoch = transition.NextState.Epoch
	baseTable.ActiveHand = &nativeActiveHand{
		Cards: nativeHandCardState{
			FinalDeck:       nil,
			PhaseDeadlineAt: previousState.ActionDeadlineAt,
			Transcript: game.HandTranscript{
				HandID:     hand.HandID,
				HandNumber: hand.HandNumber,
				Records:    []game.HandTranscriptRecord{},
				RootHash:   transcriptRoot,
				TableID:    table.Config.TableID,
			},
		},
		State: cloneJSON(hand),
	}
	request, hasRequest, err := actionRequestFromEvent(event)
	if err != nil {
		return game.HoldemState{}, nil, err
	}
	if hasRequest {
		challengeAnchor = request.ChallengeAnchor
		transcriptRoot = request.TranscriptRoot
		baseTable.ActiveHand.Cards.Transcript.RootHash = transcriptRoot
	}
	baseTable.LastEventHash = challengeAnchor
	baseTable.LatestCustodyState = previousState
	if hasRequest {
		authorizer := authorizerForActionRequest(*request)
		if err := runtime.validateCustodyTransitionSemantics(baseTable, transition, authorizer); err != nil {
			return game.HoldemState{}, nil, err
		}
		nextState, err := game.ApplyHoldemAction(hand, seatIndexForPlayerID(table, request.PlayerID), request.Action)
		if err != nil {
			return game.HoldemState{}, nil, err
		}
		return nextState, &transition.NextState, nil
	}
	if transition.Kind != tablecustody.TransitionKindTimeout {
		return game.HoldemState{}, nil, errors.New("player action event is missing its signed action request")
	}
	if transition.TimeoutResolution == nil {
		return game.HoldemState{}, nil, errors.New("timeout transition is missing its timeout resolution")
	}
	if err := runtime.validateCustodyTransitionSemantics(baseTable, transition, nil); err != nil {
		return game.HoldemState{}, nil, err
	}
	var action game.Action
	switch transition.TimeoutResolution.ActionType {
	case string(game.ActionCheck):
		action = game.Action{Type: game.ActionCheck}
	default:
		action = game.Action{Type: game.ActionFold}
	}
	nextState, err := game.ApplyHoldemAction(hand, seatIndexForPlayerID(table, transition.TimeoutResolution.ActingPlayerID), action)
	if err != nil {
		return game.HoldemState{}, nil, err
	}
	return nextState, &transition.NextState, nil
}

func seatIndexForPlayerID(table nativeTableState, playerID string) int {
	seat, ok := seatRecordForPlayer(table, playerID)
	if !ok {
		return -1
	}
	return seat.SeatIndex
}
