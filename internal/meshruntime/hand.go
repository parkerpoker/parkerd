package meshruntime

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/parkerpoker/parkerd/internal/game"
	"github.com/parkerpoker/parkerd/internal/settlementcore"
	"github.com/parkerpoker/parkerd/internal/tablecustody"
)

const (
	nativeHandMessageFairnessCommit  = "fairness-commit"
	nativeHandMessageFairnessReveal  = "fairness-reveal"
	nativeHandMessageFinalization    = "finalization"
	nativeHandMessagePrivateDelivery = "private-delivery-share"
	nativeHandMessageBoardShare      = "board-share"
	nativeHandMessageBoardOpen       = "board-open"
	nativeHandMessageShowdownReveal  = "showdown-reveal"
)

type nativeLocalHandSecrets struct {
	HandID                 string `json:"handId"`
	HandNumber             int    `json:"handNumber"`
	LockPrivateExponentHex string `json:"lockPrivateExponentHex"`
	LockPublicExponentHex  string `json:"lockPublicExponentHex"`
	PlayerID               string `json:"playerId"`
	SeatIndex              int    `json:"seatIndex"`
	ShuffleSeedHex         string `json:"shuffleSeedHex"`
}

type nativeTablePrivateState struct {
	AuditBundlesByHandID map[string]map[string]any         `json:"auditBundlesByHandId"`
	MyHoleCardsByHandID  map[string][]string               `json:"myHoleCardsByHandId"`
	SecretsByHandID      map[string]nativeLocalHandSecrets `json:"secretsByHandId"`
	TurnBundleCaches     map[string]LocalTurnBundleCache   `json:"turnBundleCaches,omitempty"`
}

func copyOptionalInt(value *int) *int {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func (runtime *meshRuntime) readTablePrivateState(tableID string) (nativeTablePrivateState, error) {
	raw, err := runtime.store.repository.LoadPrivateState(tableID)
	if err != nil {
		return nativeTablePrivateState{}, err
	}
	state := nativeTablePrivateState{
		AuditBundlesByHandID: map[string]map[string]any{},
		MyHoleCardsByHandID:  map[string][]string{},
		SecretsByHandID:      map[string]nativeLocalHandSecrets{},
		TurnBundleCaches:     map[string]LocalTurnBundleCache{},
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return state, nil
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		return nativeTablePrivateState{}, err
	}
	if state.AuditBundlesByHandID == nil {
		state.AuditBundlesByHandID = map[string]map[string]any{}
	}
	if state.MyHoleCardsByHandID == nil {
		state.MyHoleCardsByHandID = map[string][]string{}
	}
	if state.SecretsByHandID == nil {
		state.SecretsByHandID = map[string]nativeLocalHandSecrets{}
	}
	if state.TurnBundleCaches == nil {
		state.TurnBundleCaches = map[string]LocalTurnBundleCache{}
	}
	return state, nil
}

func (runtime *meshRuntime) writeTablePrivateState(tableID string, state nativeTablePrivateState) error {
	if state.AuditBundlesByHandID == nil {
		state.AuditBundlesByHandID = map[string]map[string]any{}
	}
	if state.MyHoleCardsByHandID == nil {
		state.MyHoleCardsByHandID = map[string][]string{}
	}
	if state.SecretsByHandID == nil {
		state.SecretsByHandID = map[string]nativeLocalHandSecrets{}
	}
	if state.TurnBundleCaches == nil {
		state.TurnBundleCaches = map[string]LocalTurnBundleCache{}
	}
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return runtime.store.repository.SavePrivateState(tableID, raw)
}

func otherSeatIndex(seatIndex int) int {
	if seatIndex == 0 {
		return 1
	}
	return 0
}

func seatRecordByIndex(table nativeTableState, seatIndex int) (nativeSeatRecord, bool) {
	for _, seat := range table.Seats {
		if seat.SeatIndex == seatIndex {
			return seat, true
		}
	}
	return nativeSeatRecord{}, false
}

func activeHandID(table nativeTableState) string {
	if table.ActiveHand == nil {
		return ""
	}
	return table.ActiveHand.State.HandID
}

func (runtime *meshRuntime) ensureLocalHandSecrets(table nativeTableState) (nativeLocalHandSecrets, bool, error) {
	if table.ActiveHand == nil {
		return nativeLocalHandSecrets{}, false, nil
	}
	seat, ok := seatRecordForPlayer(table, runtime.walletID.PlayerID)
	if !ok {
		return nativeLocalHandSecrets{}, false, nil
	}
	privateState, err := runtime.readTablePrivateState(table.Config.TableID)
	if err != nil {
		return nativeLocalHandSecrets{}, false, err
	}
	if secrets, ok := privateState.SecretsByHandID[table.ActiveHand.State.HandID]; ok {
		return secrets, true, nil
	}

	shuffleSeedHex, err := settlementcore.RandomHex(32)
	if err != nil {
		return nativeLocalHandSecrets{}, false, err
	}
	keyPair, err := game.GenerateMentalKeyPair()
	if err != nil {
		return nativeLocalHandSecrets{}, false, err
	}
	secrets := nativeLocalHandSecrets{
		HandID:                 table.ActiveHand.State.HandID,
		HandNumber:             table.ActiveHand.State.HandNumber,
		LockPrivateExponentHex: keyPair.PrivateExponentHex,
		LockPublicExponentHex:  keyPair.PublicExponentHex,
		PlayerID:               seat.PlayerID,
		SeatIndex:              seat.SeatIndex,
		ShuffleSeedHex:         shuffleSeedHex,
	}
	privateState.SecretsByHandID[secrets.HandID] = secrets
	if err := runtime.writeTablePrivateState(table.Config.TableID, privateState); err != nil {
		return nativeLocalHandSecrets{}, false, err
	}
	return secrets, true, nil
}

func findTranscriptRecord(transcript game.HandTranscript, kind string, seatIndex *int, phase string, recipientSeatIndex *int) (game.HandTranscriptRecord, bool) {
	for _, record := range transcript.Records {
		if record.Kind != kind {
			continue
		}
		if phase != "" && record.Phase != phase {
			continue
		}
		if seatIndex != nil {
			if record.SeatIndex == nil || *record.SeatIndex != *seatIndex {
				continue
			}
		}
		if recipientSeatIndex != nil {
			if record.RecipientSeatIndex == nil || *record.RecipientSeatIndex != *recipientSeatIndex {
				continue
			}
		}
		return record, true
	}
	return game.HandTranscriptRecord{}, false
}

func transcriptRecordsByKind(transcript game.HandTranscript, kind string, phase string) []game.HandTranscriptRecord {
	records := []game.HandTranscriptRecord{}
	for _, record := range transcript.Records {
		if record.Kind != kind {
			continue
		}
		if phase != "" && record.Phase != phase {
			continue
		}
		records = append(records, record)
	}
	return records
}

func handTranscriptRoot(table nativeTableState) string {
	if table.ActiveHand == nil {
		return ""
	}
	return table.ActiveHand.Cards.Transcript.RootHash
}

func (runtime *meshRuntime) appendHandTranscriptRecord(table *nativeTableState, record game.HandTranscriptRecord) error {
	if table == nil || table.ActiveHand == nil {
		return fmt.Errorf("hand is not active")
	}
	next, _, err := game.AppendTranscriptRecord(table.ActiveHand.Cards.Transcript, record)
	if err != nil {
		return err
	}
	table.ActiveHand.Cards.Transcript = next
	return nil
}

func (runtime *meshRuntime) protocolSeatIndexes(table nativeTableState) []int {
	indexes := make([]int, 0, len(table.Seats))
	for _, seat := range table.Seats {
		indexes = append(indexes, seat.SeatIndex)
	}
	return indexes
}

func liveShowdownSeatIndexes(table nativeTableState) []int {
	indexes := []int{}
	if table.ActiveHand == nil {
		return indexes
	}
	for _, player := range table.ActiveHand.State.Players {
		if player.Status != game.PlayerStatusFolded {
			indexes = append(indexes, player.SeatIndex)
		}
	}
	return indexes
}

func fairnessRevealForSeat(active *nativeActiveHand, seatIndex int) (game.HandTranscriptRecord, bool) {
	if active == nil {
		return game.HandTranscriptRecord{}, false
	}
	return findTranscriptRecord(active.Cards.Transcript, nativeHandMessageFairnessReveal, &seatIndex, string(game.StreetReveal), nil)
}

func publicExponentForSeat(active *nativeActiveHand, seatIndex int) (string, bool) {
	record, ok := fairnessRevealForSeat(active, seatIndex)
	if !ok {
		return "", false
	}
	return record.LockPublicExponentHex, true
}

func holdEmPlanFromTable(table nativeTableState) (game.HoldemDealPlan, error) {
	if table.ActiveHand == nil {
		return game.HoldemDealPlan{}, fmt.Errorf("hand is not active")
	}
	return game.HoldemDealPositions(table.ActiveHand.State.DealerSeatIndex)
}

func verifyPartialCiphertexts(finalDeck []string, positions []int, partialCiphertexts []string, senderSeatIndex int, active *nativeActiveHand) error {
	if len(positions) != len(partialCiphertexts) {
		return fmt.Errorf("partial ciphertext count mismatch")
	}
	publicExponentHex, ok := publicExponentForSeat(active, senderSeatIndex)
	if !ok {
		return fmt.Errorf("missing public exponent for seat %d", senderSeatIndex)
	}
	for index, position := range positions {
		if position < 0 || position >= len(finalDeck) {
			return fmt.Errorf("card position %d is out of range", position)
		}
		reencrypted, err := game.EncryptMentalValueHex(partialCiphertexts[index], publicExponentHex)
		if err != nil {
			return err
		}
		if reencrypted != finalDeck[position] {
			return fmt.Errorf("partial ciphertext does not match final deck at position %d", position)
		}
	}
	return nil
}

func verifyShowdownReveal(active *nativeActiveHand, seatIndex int, cards []string) error {
	if active == nil {
		return fmt.Errorf("hand is not active")
	}
	recipientSeatIndex := seatIndex
	opponentSeatIndex := otherSeatIndex(seatIndex)
	opponentShare, ok := findTranscriptRecord(active.Cards.Transcript, nativeHandMessagePrivateDelivery, &opponentSeatIndex, string(game.StreetPrivateDelivery), &recipientSeatIndex)
	if !ok {
		return fmt.Errorf("missing private delivery share for seat %d", seatIndex)
	}
	if len(cards) != len(opponentShare.PartialCiphertexts) {
		return fmt.Errorf("showdown reveal card count mismatch")
	}
	publicExponentHex, ok := publicExponentForSeat(active, seatIndex)
	if !ok {
		return fmt.Errorf("missing public exponent for seat %d", seatIndex)
	}
	for index, card := range cards {
		encoded, err := game.EncodeMentalCard(game.CardCode(card))
		if err != nil {
			return err
		}
		encrypted, err := game.EncryptMentalValueHex(encoded, publicExponentHex)
		if err != nil {
			return err
		}
		if encrypted != opponentShare.PartialCiphertexts[index] {
			return fmt.Errorf("showdown reveal does not match private delivery share")
		}
	}
	return nil
}

func verifyBoardOpen(active *nativeActiveHand, phase string, cards []string) error {
	if active == nil {
		return fmt.Errorf("hand is not active")
	}
	shares := transcriptRecordsByKind(active.Cards.Transcript, nativeHandMessageBoardShare, phase)
	if len(shares) != 2 {
		return fmt.Errorf("board open requires two board shares")
	}
	for _, share := range shares {
		if len(share.PartialCiphertexts) != len(cards) {
			return fmt.Errorf("board open card count mismatch")
		}
		opponentExponentHex, ok := publicExponentForSeat(active, otherSeatIndex(*share.SeatIndex))
		if !ok {
			return fmt.Errorf("missing public exponent for opposing seat")
		}
		for index, card := range cards {
			encoded, err := game.EncodeMentalCard(game.CardCode(card))
			if err != nil {
				return err
			}
			encrypted, err := game.EncryptMentalValueHex(encoded, opponentExponentHex)
			if err != nil {
				return err
			}
			if encrypted != share.PartialCiphertexts[index] {
				return fmt.Errorf("board open does not match share from seat %d", *share.SeatIndex)
			}
		}
	}
	return nil
}

func requiredCardPositionsForPhase(table nativeTableState, phase game.Street, seatIndex int) ([]int, error) {
	plan, err := holdEmPlanFromTable(table)
	if err != nil {
		return nil, err
	}
	switch phase {
	case game.StreetPrivateDelivery:
		positions := append([]int(nil), plan.HoleCardPositionsBySeat[seatIndex]...)
		return positions, nil
	case game.StreetFlopReveal, game.StreetTurnReveal, game.StreetRiverReveal:
		return append([]int(nil), plan.BoardPositionsByPhase[phase]...), nil
	default:
		return nil, fmt.Errorf("phase %s does not have card positions", phase)
	}
}

func seatPlayerID(table nativeTableState, seatIndex int) string {
	for _, seat := range table.Seats {
		if seat.SeatIndex == seatIndex {
			return seat.PlayerID
		}
	}
	return ""
}

type protocolDriveSnapshot struct {
	custodyHash    string
	epoch          int
	handID         string
	hostPeerID     string
	phase          game.Street
	transcriptRoot string
}

type guestContributionSendSnapshot struct {
	acked      bool
	handID     string
	handNumber int
	kind       string
	phase      string
	recordKey  string
}

func snapshotProtocolDrive(table nativeTableState) protocolDriveSnapshot {
	snapshot := protocolDriveSnapshot{
		custodyHash: latestCustodyStateHash(table),
		epoch:       table.CurrentEpoch,
		hostPeerID:  table.CurrentHost.Peer.PeerID,
	}
	if table.ActiveHand != nil {
		snapshot.handID = table.ActiveHand.State.HandID
		snapshot.phase = table.ActiveHand.State.Phase
		snapshot.transcriptRoot = handTranscriptRoot(table)
	}
	return snapshot
}

func sameProtocolDriveSnapshot(table nativeTableState, snapshot protocolDriveSnapshot) bool {
	current := snapshotProtocolDrive(table)
	return current == snapshot
}

func tablePhaseForTiming(table nativeTableState) string {
	if table.ActiveHand == nil {
		return ""
	}
	return string(table.ActiveHand.State.Phase)
}

func custodyTimingFields(table nativeTableState, transition tablecustody.CustodyTransition, metric string) meshTimingFields {
	return meshTimingFields{
		Metric:         metric,
		TableID:        table.Config.TableID,
		CustodySeq:     transition.CustodySeq,
		TransitionKind: string(transition.Kind),
		Phase:          tablePhaseForTiming(table),
		RequestHash:    custodyApprovalTargetHash(transition),
	}
}

func handMessageRecordPayload(tableID, handID string, handNumber int, record game.HandTranscriptRecord) map[string]any {
	payload := map[string]any{
		"handId":     handID,
		"handNumber": handNumber,
		"kind":       record.Kind,
		"phase":      record.Phase,
		"tableId":    tableID,
		"type":       "dealerless-hand-record-key",
	}
	if record.PlayerID != "" {
		payload["playerId"] = record.PlayerID
	}
	if record.SeatIndex != nil {
		payload["seatIndex"] = *record.SeatIndex
	}
	if record.CommitmentHash != "" {
		payload["commitmentHash"] = record.CommitmentHash
	}
	if record.ShuffleSeedHex != "" {
		payload["shuffleSeedHex"] = record.ShuffleSeedHex
	}
	if record.LockPublicExponentHex != "" {
		payload["lockPublicExponentHex"] = record.LockPublicExponentHex
	}
	if len(record.DeckStage) > 0 {
		payload["deckStage"] = append([]string(nil), record.DeckStage...)
	}
	if record.DeckStageRoot != "" {
		payload["deckStageRoot"] = record.DeckStageRoot
	}
	if record.RecipientSeatIndex != nil {
		payload["recipientSeatIndex"] = *record.RecipientSeatIndex
	}
	if len(record.CardPositions) > 0 {
		payload["cardPositions"] = append([]int(nil), record.CardPositions...)
	}
	if len(record.PartialCiphertexts) > 0 {
		payload["partialCiphertexts"] = append([]string(nil), record.PartialCiphertexts...)
	}
	if len(record.Cards) > 0 {
		payload["cards"] = handMessageCards(record.Cards)
	}
	return payload
}

func handMessageRecordKey(tableID, handID string, handNumber int, record game.HandTranscriptRecord) (string, error) {
	return settlementcore.HashStructuredDataHex(handMessageRecordPayload(tableID, handID, handNumber, record))
}

func handMessageRequestRecordKey(request nativeHandMessageRequest) (string, error) {
	record, err := transcriptRecordFromHandMessage(request)
	if err != nil {
		return "", err
	}
	return handMessageRecordKey(request.TableID, request.HandID, request.HandNumber, record)
}

func handMessageSlotLabel(record game.HandTranscriptRecord) string {
	switch record.Kind {
	case nativeHandMessageFairnessCommit, nativeHandMessageFairnessReveal, nativeHandMessageBoardShare, nativeHandMessageShowdownReveal:
		if record.SeatIndex != nil {
			return fmt.Sprintf("%s seat=%d phase=%s", record.Kind, *record.SeatIndex, record.Phase)
		}
	case nativeHandMessagePrivateDelivery:
		if record.SeatIndex != nil && record.RecipientSeatIndex != nil {
			return fmt.Sprintf("%s seat=%d recipient=%d phase=%s", record.Kind, *record.SeatIndex, *record.RecipientSeatIndex, record.Phase)
		}
	case nativeHandMessageBoardOpen:
		return fmt.Sprintf("%s phase=%s", record.Kind, record.Phase)
	}
	return record.Kind
}

func findParticipantHandMessageSlotRecord(transcript game.HandTranscript, record game.HandTranscriptRecord) (game.HandTranscriptRecord, bool, error) {
	switch record.Kind {
	case nativeHandMessageFairnessCommit:
		if record.SeatIndex == nil {
			return game.HandTranscriptRecord{}, false, fmt.Errorf("%s is missing seat index", record.Kind)
		}
		existing, ok := findTranscriptRecord(transcript, record.Kind, record.SeatIndex, string(game.StreetCommitment), nil)
		return existing, ok, nil
	case nativeHandMessageFairnessReveal:
		if record.SeatIndex == nil {
			return game.HandTranscriptRecord{}, false, fmt.Errorf("%s is missing seat index", record.Kind)
		}
		existing, ok := findTranscriptRecord(transcript, record.Kind, record.SeatIndex, string(game.StreetReveal), nil)
		return existing, ok, nil
	case nativeHandMessagePrivateDelivery:
		if record.SeatIndex == nil || record.RecipientSeatIndex == nil {
			return game.HandTranscriptRecord{}, false, fmt.Errorf("%s is missing seat indexes", record.Kind)
		}
		existing, ok := findTranscriptRecord(transcript, record.Kind, record.SeatIndex, string(game.StreetPrivateDelivery), record.RecipientSeatIndex)
		return existing, ok, nil
	case nativeHandMessageBoardShare:
		if record.SeatIndex == nil {
			return game.HandTranscriptRecord{}, false, fmt.Errorf("%s is missing seat index", record.Kind)
		}
		existing, ok := findTranscriptRecord(transcript, record.Kind, record.SeatIndex, record.Phase, nil)
		return existing, ok, nil
	case nativeHandMessageBoardOpen:
		existing, ok := findTranscriptRecord(transcript, record.Kind, nil, record.Phase, nil)
		return existing, ok, nil
	case nativeHandMessageShowdownReveal:
		if record.SeatIndex == nil {
			return game.HandTranscriptRecord{}, false, fmt.Errorf("%s is missing seat index", record.Kind)
		}
		existing, ok := findTranscriptRecord(transcript, record.Kind, record.SeatIndex, string(game.StreetShowdownReveal), nil)
		return existing, ok, nil
	default:
		return game.HandTranscriptRecord{}, false, nil
	}
}

func handTranscriptHasRecordKey(transcript game.HandTranscript, recordKey string) bool {
	if strings.TrimSpace(recordKey) == "" {
		return false
	}
	for _, record := range transcript.Records {
		currentKey, err := handMessageRecordKey(transcript.TableID, transcript.HandID, transcript.HandNumber, record)
		if err == nil && currentKey == recordKey {
			return true
		}
	}
	return false
}

func guestContributionSnapshot(table nativeTableState, record game.HandTranscriptRecord, recordKey string) guestContributionSendSnapshot {
	snapshot := guestContributionSendSnapshot{
		kind:      record.Kind,
		phase:     record.Phase,
		recordKey: recordKey,
	}
	if table.ActiveHand != nil {
		snapshot.handID = table.ActiveHand.State.HandID
		snapshot.handNumber = table.ActiveHand.State.HandNumber
	}
	return snapshot
}

func (runtime *meshRuntime) reconcileGuestContribution(tableID string, table nativeTableState) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()

	previous, ok := runtime.lastGuestContribution[tableID]
	if !ok {
		return
	}
	if table.ActiveHand == nil ||
		table.ActiveHand.State.HandID != previous.handID ||
		table.ActiveHand.State.HandNumber != previous.handNumber {
		delete(runtime.lastGuestContribution, tableID)
		return
	}
	if handTranscriptHasRecordKey(table.ActiveHand.Cards.Transcript, previous.recordKey) {
		delete(runtime.lastGuestContribution, tableID)
	}
}

func (runtime *meshRuntime) shouldSendGuestContribution(tableID string, pending guestContributionSendSnapshot) bool {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()

	if runtime.lastGuestContribution == nil {
		runtime.lastGuestContribution = map[string]guestContributionSendSnapshot{}
	}
	previous, ok := runtime.lastGuestContribution[tableID]
	if !ok || previous.recordKey != pending.recordKey {
		runtime.lastGuestContribution[tableID] = pending
		return true
	}
	return !previous.acked
}

func (runtime *meshRuntime) markGuestContributionAcked(tableID, recordKey string) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()

	previous, ok := runtime.lastGuestContribution[tableID]
	if !ok || previous.recordKey != recordKey {
		return
	}
	previous.acked = true
	runtime.lastGuestContribution[tableID] = previous
}

func sameInts(left, right []int) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func publicTranscriptRoot(table nativeTableState) string {
	if table.PublicState == nil || table.PublicState.DealerCommitment == nil {
		return ""
	}
	return strings.TrimSpace(table.PublicState.DealerCommitment.RootHash)
}

func normalizePublicTranscriptRoot(table *nativeTableState) {
	if table == nil || table.PublicState == nil || table.ActiveHand == nil {
		return
	}
	rootHash := strings.TrimSpace(handTranscriptRoot(*table))
	if rootHash == "" {
		table.PublicState.DealerCommitment = nil
		return
	}
	commitment := NativeDealerCommitment{
		Mode:     nativeDealerMode,
		RootHash: rootHash,
	}
	if table.PublicState.DealerCommitment != nil {
		commitment.CommittedAt = table.PublicState.DealerCommitment.CommittedAt
	}
	table.PublicState.DealerCommitment = &commitment
}

func protocolDeadlineExpired(table nativeTableState) bool {
	if table.ActiveHand == nil || !shouldTrackProtocolDeadline(table.ActiveHand.State.Phase) {
		return false
	}
	if table.ActiveHand.Cards.PhaseDeadlineAt == "" {
		return false
	}
	return elapsedMillis(table.ActiveHand.Cards.PhaseDeadlineAt) >= 0
}

func validateSnapshotTranscriptRoot(snapshot *NativeCooperativeTableSnapshot, handID, rootHash string) error {
	if snapshot == nil || stringValue(snapshot.HandID) != handID {
		return nil
	}
	snapshotRoot := strings.TrimSpace(stringValue(snapshot.DealerCommitmentRoot))
	if rootHash == "" {
		if snapshotRoot != "" {
			return fmt.Errorf("snapshot transcript root unexpectedly set for hand %s", handID)
		}
		return nil
	}
	if snapshotRoot != "" && snapshotRoot != rootHash {
		return fmt.Errorf("snapshot transcript root mismatch for hand %s", handID)
	}
	return nil
}

func compareCardCodes(recordCards []game.CardCode, expectedCards []string) bool {
	if len(recordCards) != len(expectedCards) {
		return false
	}
	for index := range recordCards {
		if string(recordCards[index]) != expectedCards[index] {
			return false
		}
	}
	return true
}

func validateTranscriptRecordPlaintext(record game.HandTranscriptRecord) error {
	if len(record.Cards) == 0 {
		return nil
	}
	switch record.Kind {
	case nativeHandMessageBoardOpen, nativeHandMessageShowdownReveal:
		return nil
	default:
		return fmt.Errorf("hand transcript %s must not include plaintext cards", record.Kind)
	}
}

func (runtime *meshRuntime) validateAcceptedHandTranscript(table nativeTableState) error {
	if table.ActiveHand == nil {
		return nil
	}
	transcript := table.ActiveHand.Cards.Transcript
	if transcript.TableID != table.Config.TableID {
		return fmt.Errorf("hand transcript table mismatch")
	}
	if transcript.HandID != table.ActiveHand.State.HandID {
		return fmt.Errorf("hand transcript hand mismatch")
	}
	if transcript.HandNumber != table.ActiveHand.State.HandNumber {
		return fmt.Errorf("hand transcript hand number mismatch")
	}

	replayedRoot, err := game.ReplayTranscriptRoot(transcript)
	if err != nil {
		return fmt.Errorf("invalid hand transcript: %w", err)
	}
	if replayedRoot != transcript.RootHash {
		return fmt.Errorf("hand transcript root mismatch")
	}
	if publicRoot := publicTranscriptRoot(table); publicRoot != "" && publicRoot != replayedRoot {
		return fmt.Errorf("public transcript root mismatch")
	}
	if err := validateSnapshotTranscriptRoot(table.LatestSnapshot, transcript.HandID, replayedRoot); err != nil {
		return err
	}
	if err := validateSnapshotTranscriptRoot(table.LatestFullySignedSnapshot, transcript.HandID, replayedRoot); err != nil {
		return err
	}

	seenKeys := map[string]struct{}{}
	for _, record := range transcript.Records {
		if err := validateTranscriptRecordPlaintext(record); err != nil {
			return err
		}
		if record.SeatIndex != nil {
			seat, ok := seatRecordByIndex(table, *record.SeatIndex)
			if !ok {
				return fmt.Errorf("hand transcript references unknown seat %d", *record.SeatIndex)
			}
			if record.PlayerID != "" && record.PlayerID != seat.PlayerID {
				return fmt.Errorf("hand transcript player mismatch for seat %d", *record.SeatIndex)
			}
		}
		switch record.Kind {
		case nativeHandMessageFairnessCommit, nativeHandMessageFairnessReveal, nativeHandMessageBoardShare, nativeHandMessageShowdownReveal:
			if record.SeatIndex == nil {
				return fmt.Errorf("hand transcript %s is missing seat index", record.Kind)
			}
		case nativeHandMessagePrivateDelivery:
			if record.SeatIndex == nil || record.RecipientSeatIndex == nil {
				return fmt.Errorf("hand transcript private delivery share is missing seat indexes")
			}
		case nativeHandMessageBoardOpen:
			if len(record.Cards) == 0 {
				return fmt.Errorf("hand transcript board open is missing cards")
			}
		}

		switch record.Kind {
		case nativeHandMessageFairnessCommit:
			key := fmt.Sprintf("%s:%d", record.Kind, *record.SeatIndex)
			if _, exists := seenKeys[key]; exists {
				return fmt.Errorf("duplicate fairness commit for seat %d", *record.SeatIndex)
			}
			seenKeys[key] = struct{}{}
			if strings.TrimSpace(record.CommitmentHash) == "" {
				return fmt.Errorf("fairness commit for seat %d is missing commitment hash", *record.SeatIndex)
			}
		case nativeHandMessageFairnessReveal:
			key := fmt.Sprintf("%s:%d", record.Kind, *record.SeatIndex)
			if _, exists := seenKeys[key]; exists {
				return fmt.Errorf("duplicate fairness reveal for seat %d", *record.SeatIndex)
			}
			seenKeys[key] = struct{}{}
		case nativeHandMessagePrivateDelivery:
			key := fmt.Sprintf("%s:%d:%d", record.Kind, *record.SeatIndex, *record.RecipientSeatIndex)
			if _, exists := seenKeys[key]; exists {
				return fmt.Errorf("duplicate private delivery share for seat %d", *record.SeatIndex)
			}
			seenKeys[key] = struct{}{}
		case nativeHandMessageBoardShare:
			key := fmt.Sprintf("%s:%s:%d", record.Kind, record.Phase, *record.SeatIndex)
			if _, exists := seenKeys[key]; exists {
				return fmt.Errorf("duplicate board share for seat %d during %s", *record.SeatIndex, record.Phase)
			}
			seenKeys[key] = struct{}{}
		case nativeHandMessageBoardOpen:
			key := fmt.Sprintf("%s:%s", record.Kind, record.Phase)
			if _, exists := seenKeys[key]; exists {
				return fmt.Errorf("duplicate board open during %s", record.Phase)
			}
			seenKeys[key] = struct{}{}
		case nativeHandMessageShowdownReveal:
			key := fmt.Sprintf("%s:%d", record.Kind, *record.SeatIndex)
			if _, exists := seenKeys[key]; exists {
				return fmt.Errorf("duplicate showdown reveal for seat %d", *record.SeatIndex)
			}
			seenKeys[key] = struct{}{}
		case nativeHandMessageFinalization:
			key := fmt.Sprintf("%s:%s", record.Kind, record.Phase)
			if _, exists := seenKeys[key]; exists {
				return fmt.Errorf("duplicate finalization record")
			}
			seenKeys[key] = struct{}{}
		}
	}

	for _, seat := range table.Seats {
		commit, hasCommit := findTranscriptRecord(transcript, nativeHandMessageFairnessCommit, &seat.SeatIndex, string(game.StreetCommitment), nil)
		reveal, hasReveal := fairnessRevealForSeat(table.ActiveHand, seat.SeatIndex)
		if hasCommit && strings.TrimSpace(commit.CommitmentHash) == "" {
			return fmt.Errorf("fairness commit for seat %d is missing commitment hash", seat.SeatIndex)
		}
		if !hasReveal {
			continue
		}
		if !hasCommit {
			return fmt.Errorf("missing fairness commit for reveal seat %d", seat.SeatIndex)
		}
		if err := game.VerifyFairnessReveal(table.Config.TableID, table.ActiveHand.State.HandNumber, seat.SeatIndex, seat.PlayerID, string(game.StreetCommitment), commit.CommitmentHash, reveal.ShuffleSeedHex, reveal.LockPublicExponentHex); err != nil {
			return fmt.Errorf("invalid fairness reveal for seat %d: %w", seat.SeatIndex, err)
		}

		reveals := []game.MentalDeckReveal{}
		for _, otherSeat := range table.Seats {
			if otherSeat.SeatIndex > seat.SeatIndex {
				continue
			}
			if otherSeat.SeatIndex == seat.SeatIndex {
				reveals = append(reveals, game.MentalDeckReveal{
					PlayerID:              seat.PlayerID,
					SeatIndex:             seat.SeatIndex,
					ShuffleSeedHex:        reveal.ShuffleSeedHex,
					LockPublicExponentHex: reveal.LockPublicExponentHex,
				})
				continue
			}
			otherReveal, ok := fairnessRevealForSeat(table.ActiveHand, otherSeat.SeatIndex)
			if !ok {
				return fmt.Errorf("missing lower-seat reveal for seat %d", seat.SeatIndex)
			}
			reveals = append(reveals, game.MentalDeckReveal{
				PlayerID:              otherSeat.PlayerID,
				SeatIndex:             otherSeat.SeatIndex,
				ShuffleSeedHex:        otherReveal.ShuffleSeedHex,
				LockPublicExponentHex: otherReveal.LockPublicExponentHex,
			})
		}
		replay, err := game.ReplayMentalDeck(reveals)
		if err != nil {
			return err
		}
		stage := replay.RevealStagesBySeat[seat.SeatIndex]
		if !sameStrings(stage, reveal.DeckStage) || replay.RevealStageRootBySeat[seat.SeatIndex] != reveal.DeckStageRoot {
			return fmt.Errorf("reveal stage mismatch for seat %d", seat.SeatIndex)
		}
	}

	allRevealsPresent := len(transcriptRecordsByKind(transcript, nativeHandMessageFairnessReveal, string(game.StreetReveal))) == len(table.Seats)
	_, abortedHand := acceptedAbortSeatIndex(table)
	if table.ActiveHand.State.Phase != game.StreetCommitment && table.ActiveHand.State.Phase != game.StreetReveal && !allRevealsPresent {
		if abortedHand && table.ActiveHand.State.Phase == game.StreetSettled {
			return nil
		}
		return fmt.Errorf("missing fairness reveals for active hand")
	}
	if allRevealsPresent {
		replay, err := runtime.buildMentalReplay(table)
		if err != nil {
			return err
		}
		finalization, ok := findTranscriptRecord(transcript, nativeHandMessageFinalization, nil, string(game.StreetFinalization), nil)
		if !ok {
			return fmt.Errorf("missing finalization transcript record")
		}
		finalDeckRoot, err := game.MentalDeckStageRoot(replay.FinalDeck)
		if err != nil {
			return err
		}
		if !sameStrings(finalization.DeckStage, replay.FinalDeck) || finalization.DeckStageRoot != finalDeckRoot {
			return fmt.Errorf("finalization record does not match replayed deck")
		}
		if len(table.ActiveHand.Cards.FinalDeck) == 0 {
			table.ActiveHand.Cards.FinalDeck = append([]string(nil), replay.FinalDeck...)
		} else if !sameStrings(table.ActiveHand.Cards.FinalDeck, replay.FinalDeck) {
			return fmt.Errorf("active hand final deck does not match replayed transcript deck")
		}
	}
	if abortedHand && table.ActiveHand.State.Phase == game.StreetSettled {
		return nil
	}
	if requiresCompletedPrivateDelivery(table.ActiveHand.State.Phase) {
		if missing := missingProtocolSeatIndexesForPhase(table, game.StreetPrivateDelivery); len(missing) > 0 {
			return fmt.Errorf("missing private delivery shares for active hand")
		}
	}

	for _, record := range transcript.Records {
		switch record.Kind {
		case nativeHandMessagePrivateDelivery:
			expectedPositions, err := requiredCardPositionsForPhase(table, game.StreetPrivateDelivery, *record.RecipientSeatIndex)
			if err != nil {
				return err
			}
			if !sameInts(record.CardPositions, expectedPositions) {
				return fmt.Errorf("private delivery positions do not match hand plan")
			}
			if err := verifyPartialCiphertexts(table.ActiveHand.Cards.FinalDeck, record.CardPositions, record.PartialCiphertexts, *record.SeatIndex, table.ActiveHand); err != nil {
				return fmt.Errorf("invalid private delivery share for seat %d: %w", *record.SeatIndex, err)
			}
		case nativeHandMessageBoardShare:
			expectedPositions, err := requiredCardPositionsForPhase(table, game.Street(record.Phase), *record.SeatIndex)
			if err != nil {
				return err
			}
			if !sameInts(record.CardPositions, expectedPositions) {
				return fmt.Errorf("board share positions do not match hand plan")
			}
			if err := verifyPartialCiphertexts(table.ActiveHand.Cards.FinalDeck, record.CardPositions, record.PartialCiphertexts, *record.SeatIndex, table.ActiveHand); err != nil {
				return fmt.Errorf("invalid board share for seat %d: %w", *record.SeatIndex, err)
			}
		case nativeHandMessageBoardOpen:
			if err := verifyBoardOpen(table.ActiveHand, record.Phase, handMessageCards(record.Cards)); err != nil {
				return fmt.Errorf("invalid board open for %s: %w", record.Phase, err)
			}
		case nativeHandMessageShowdownReveal:
			if err := verifyShowdownReveal(table.ActiveHand, *record.SeatIndex, handMessageCards(record.Cards)); err != nil {
				return fmt.Errorf("invalid showdown reveal for seat %d: %w", *record.SeatIndex, err)
			}
		}
	}

	return nil
}

func (runtime *meshRuntime) storeLocalHoleCards(table nativeTableState) error {
	if table.ActiveHand == nil {
		return nil
	}
	privateState, err := runtime.readTablePrivateState(table.Config.TableID)
	if err != nil {
		return err
	}
	secrets, ok := privateState.SecretsByHandID[table.ActiveHand.State.HandID]
	if !ok {
		return runtime.writeTablePrivateState(table.Config.TableID, privateState)
	}

	recipientSeatIndex := secrets.SeatIndex
	opponentSeatIndex := otherSeatIndex(recipientSeatIndex)
	opponentShare, ok := findTranscriptRecord(table.ActiveHand.Cards.Transcript, nativeHandMessagePrivateDelivery, &opponentSeatIndex, string(game.StreetPrivateDelivery), &recipientSeatIndex)
	if !ok {
		return runtime.writeTablePrivateState(table.Config.TableID, privateState)
	}
	cards := make([]string, 0, len(opponentShare.PartialCiphertexts))
	for _, partialCiphertext := range opponentShare.PartialCiphertexts {
		plaintextHex, err := game.DecryptMentalValueHex(partialCiphertext, secrets.LockPrivateExponentHex)
		if err != nil {
			return err
		}
		card, err := game.DecodeMentalCardHex(plaintextHex)
		if err != nil {
			return err
		}
		cards = append(cards, string(card))
	}
	privateState.MyHoleCardsByHandID[table.ActiveHand.State.HandID] = cards
	privateState.AuditBundlesByHandID[table.ActiveHand.State.HandID] = map[string]any{
		"transcriptRoot": handTranscriptRoot(table),
	}
	return runtime.writeTablePrivateState(table.Config.TableID, privateState)
}

func missingProtocolSeatIndexesForPhase(table nativeTableState, phase game.Street) []int {
	if table.ActiveHand == nil {
		return nil
	}
	transcript := table.ActiveHand.Cards.Transcript
	switch phase {
	case game.StreetCommitment:
		missing := []int{}
		for _, seatIndex := range runtimeSeatIndexes(table) {
			if _, ok := findTranscriptRecord(transcript, nativeHandMessageFairnessCommit, &seatIndex, string(game.StreetCommitment), nil); !ok {
				missing = append(missing, seatIndex)
			}
		}
		return missing
	case game.StreetReveal:
		missing := []int{}
		for _, seatIndex := range runtimeSeatIndexes(table) {
			if _, ok := findTranscriptRecord(transcript, nativeHandMessageFairnessReveal, &seatIndex, string(game.StreetReveal), nil); !ok {
				missing = append(missing, seatIndex)
			}
		}
		return missing
	case game.StreetPrivateDelivery:
		missing := []int{}
		for _, seatIndex := range runtimeSeatIndexes(table) {
			recipientSeatIndex := otherSeatIndex(seatIndex)
			if _, ok := findTranscriptRecord(transcript, nativeHandMessagePrivateDelivery, &seatIndex, string(game.StreetPrivateDelivery), &recipientSeatIndex); !ok {
				missing = append(missing, seatIndex)
			}
		}
		return missing
	case game.StreetFlopReveal, game.StreetTurnReveal, game.StreetRiverReveal:
		missing := []int{}
		for _, seatIndex := range runtimeSeatIndexes(table) {
			if _, ok := findTranscriptRecord(transcript, nativeHandMessageBoardShare, &seatIndex, string(table.ActiveHand.State.Phase), nil); !ok {
				missing = append(missing, seatIndex)
			}
		}
		return missing
	case game.StreetShowdownReveal:
		missing := []int{}
		for _, seatIndex := range liveShowdownSeatIndexes(table) {
			if _, ok := findTranscriptRecord(transcript, nativeHandMessageShowdownReveal, &seatIndex, string(game.StreetShowdownReveal), nil); !ok {
				missing = append(missing, seatIndex)
			}
		}
		return missing
	default:
		return nil
	}
}

func missingProtocolSeatIndexes(table nativeTableState) []int {
	if table.ActiveHand == nil {
		return nil
	}
	return missingProtocolSeatIndexesForPhase(table, table.ActiveHand.State.Phase)
}

func requiresCompletedPrivateDelivery(phase game.Street) bool {
	switch phase {
	case game.StreetPreflop, game.StreetFlopReveal, game.StreetFlop, game.StreetTurnReveal, game.StreetTurn, game.StreetRiverReveal, game.StreetRiver, game.StreetShowdownReveal, game.StreetSettled:
		return true
	default:
		return false
	}
}

func runtimeSeatIndexes(table nativeTableState) []int {
	indexes := make([]int, 0, len(table.Seats))
	for _, seat := range table.Seats {
		indexes = append(indexes, seat.SeatIndex)
	}
	return indexes
}

func shouldTrackProtocolDeadline(phase game.Street) bool {
	switch phase {
	case game.StreetCommitment, game.StreetReveal, game.StreetPrivateDelivery, game.StreetFlopReveal, game.StreetTurnReveal, game.StreetRiverReveal, game.StreetShowdownReveal:
		return true
	default:
		return false
	}
}

func allFairnessRevealsPresent(table nativeTableState) bool {
	if table.ActiveHand == nil {
		return false
	}
	return len(transcriptRecordsByKind(table.ActiveHand.Cards.Transcript, nativeHandMessageFairnessReveal, string(game.StreetReveal))) == len(table.Seats)
}

func protocolPhaseTranscriptProgressed(existing *nativeTableState, incoming nativeTableState) bool {
	if existing == nil || existing.ActiveHand == nil || incoming.ActiveHand == nil {
		return false
	}
	if existing.CurrentHost.Peer.PeerID != incoming.CurrentHost.Peer.PeerID ||
		existing.ActiveHand.State.HandID != incoming.ActiveHand.State.HandID ||
		existing.ActiveHand.State.HandNumber != incoming.ActiveHand.State.HandNumber ||
		existing.ActiveHand.State.Phase != incoming.ActiveHand.State.Phase ||
		!shouldTrackProtocolDeadline(incoming.ActiveHand.State.Phase) {
		return false
	}
	if len(incoming.ActiveHand.Cards.Transcript.Records) <= len(existing.ActiveHand.Cards.Transcript.Records) {
		return false
	}
	return transcriptExtends(existing.ActiveHand.Cards.Transcript, incoming.ActiveHand.Cards.Transcript)
}

func (runtime *meshRuntime) deriveLocalProtocolDeadline(existing *nativeTableState, incoming nativeTableState) string {
	if incoming.ActiveHand == nil || !shouldTrackProtocolDeadline(incoming.ActiveHand.State.Phase) {
		return ""
	}
	if existing != nil && existing.ActiveHand != nil &&
		existing.CurrentHost.Peer.PeerID == incoming.CurrentHost.Peer.PeerID &&
		existing.ActiveHand.State.HandID == incoming.ActiveHand.State.HandID &&
		existing.ActiveHand.State.Phase == incoming.ActiveHand.State.Phase &&
		existing.ActiveHand.Cards.PhaseDeadlineAt != "" &&
		shouldTrackProtocolDeadline(existing.ActiveHand.State.Phase) {
		if protocolPhaseTranscriptProgressed(existing, incoming) {
			return addMillis(nowISO(), runtime.handProtocolTimeoutMSForTable(incoming))
		}
		return existing.ActiveHand.Cards.PhaseDeadlineAt
	}
	return addMillis(nowISO(), runtime.handProtocolTimeoutMSForTable(incoming))
}

func (runtime *meshRuntime) normalizeAcceptedActiveHand(existing *nativeTableState, incoming *nativeTableState) error {
	if incoming == nil {
		return nil
	}
	if incoming.ActiveHand == nil {
		incoming.ActiveHandStartAt = ""
		return nil
	}
	if allFairnessRevealsPresent(*incoming) {
		replay, err := runtime.buildMentalReplay(*incoming)
		if err != nil {
			return err
		}
		incoming.ActiveHand.Cards.FinalDeck = append([]string(nil), replay.FinalDeck...)
	} else {
		incoming.ActiveHand.Cards.FinalDeck = nil
	}
	incoming.ActiveHand.Cards.PhaseDeadlineAt = runtime.deriveLocalProtocolDeadline(existing, *incoming)
	normalizePublicTranscriptRoot(incoming)
	return nil
}

func (runtime *meshRuntime) setProtocolDeadline(table *nativeTableState) {
	if table == nil || table.ActiveHand == nil {
		return
	}
	if shouldTrackProtocolDeadline(table.ActiveHand.State.Phase) {
		table.ActiveHand.Cards.PhaseDeadlineAt = addMillis(nowISO(), runtime.handProtocolTimeoutMSForTable(*table))
		return
	}
	table.ActiveHand.Cards.PhaseDeadlineAt = ""
}

func transcriptHasBoardOpen(active *nativeActiveHand, phase game.Street) bool {
	if active == nil {
		return false
	}
	_, ok := findTranscriptRecord(active.Cards.Transcript, nativeHandMessageBoardOpen, nil, string(phase), nil)
	return ok
}

func decodeCardCodes(cards []string) ([]game.CardCode, error) {
	values := make([]game.CardCode, 0, len(cards))
	for _, card := range cards {
		values = append(values, game.CardCode(card))
	}
	return values, nil
}

func decryptCardCodes(partialCiphertexts []string, privateExponentHex string) ([]game.CardCode, error) {
	cards := make([]game.CardCode, 0, len(partialCiphertexts))
	for _, partialCiphertext := range partialCiphertexts {
		plaintextHex, err := game.DecryptMentalValueHex(partialCiphertext, privateExponentHex)
		if err != nil {
			return nil, err
		}
		card, err := game.DecodeMentalCardHex(plaintextHex)
		if err != nil {
			return nil, err
		}
		cards = append(cards, card)
	}
	return cards, nil
}

func (runtime *meshRuntime) buildMentalReplay(table nativeTableState) (game.MentalDeckReplay, error) {
	reveals := make([]game.MentalDeckReveal, 0, len(table.Seats))
	for _, seat := range table.Seats {
		record, ok := findTranscriptRecord(table.ActiveHand.Cards.Transcript, nativeHandMessageFairnessReveal, &seat.SeatIndex, string(game.StreetReveal), nil)
		if !ok {
			return game.MentalDeckReplay{}, fmt.Errorf("missing fairness reveal for seat %d", seat.SeatIndex)
		}
		reveals = append(reveals, game.MentalDeckReveal{
			PlayerID:              seat.PlayerID,
			SeatIndex:             seat.SeatIndex,
			ShuffleSeedHex:        record.ShuffleSeedHex,
			LockPublicExponentHex: record.LockPublicExponentHex,
		})
	}
	replay, err := game.ReplayMentalDeck(reveals)
	if err != nil {
		return game.MentalDeckReplay{}, err
	}
	for _, seat := range table.Seats {
		record, ok := findTranscriptRecord(table.ActiveHand.Cards.Transcript, nativeHandMessageFairnessReveal, &seat.SeatIndex, string(game.StreetReveal), nil)
		if !ok {
			return game.MentalDeckReplay{}, fmt.Errorf("missing fairness reveal for seat %d", seat.SeatIndex)
		}
		if record.DeckStageRoot != replay.RevealStageRootBySeat[seat.SeatIndex] {
			return game.MentalDeckReplay{}, fmt.Errorf("reveal stage root mismatch for seat %d", seat.SeatIndex)
		}
		stage := replay.RevealStagesBySeat[seat.SeatIndex]
		if len(record.DeckStage) != len(stage) {
			return game.MentalDeckReplay{}, fmt.Errorf("reveal stage length mismatch for seat %d", seat.SeatIndex)
		}
		for index := range stage {
			if record.DeckStage[index] != stage[index] {
				return game.MentalDeckReplay{}, fmt.Errorf("reveal stage mismatch for seat %d", seat.SeatIndex)
			}
		}
	}
	return replay, nil
}

func startingBalancesForHand(table nativeTableState, handNumber int) map[string]int {
	for index := len(table.CustodyTransitions) - 1; index >= 0; index-- {
		state := table.CustodyTransitions[index].NextState
		if state.HandNumber >= handNumber {
			continue
		}
		balances := map[string]int{}
		for _, claim := range state.StackClaims {
			balances[claim.PlayerID] = claim.AmountSats
		}
		if len(balances) > 0 {
			return balances
		}
	}
	if table.LatestCustodyState != nil && table.LatestCustodyState.HandNumber < handNumber {
		balances := map[string]int{}
		for _, claim := range table.LatestCustodyState.StackClaims {
			balances[claim.PlayerID] = claim.AmountSats
		}
		if len(balances) > 0 {
			return balances
		}
	}
	bestHandNumber := -1
	balances := map[string]int{}
	for _, snapshot := range table.Snapshots {
		if snapshot.HandNumber >= handNumber || snapshot.HandNumber < bestHandNumber {
			continue
		}
		bestHandNumber = snapshot.HandNumber
		balances = cloneJSON(snapshot.ChipBalances)
	}
	if bestHandNumber >= 0 {
		return balances
	}
	for _, seat := range table.Seats {
		balances[seat.PlayerID] = seat.BuyInSats
	}
	return balances
}

func acceptedAbortSeatIndex(table nativeTableState) (*int, bool) {
	if table.ActiveHand == nil {
		return nil, false
	}
	handID := table.ActiveHand.State.HandID
	for index := len(table.Events) - 1; index >= 0; index-- {
		event := table.Events[index]
		if event.MessageType != "HandAbort" || stringValue(event.HandID) != handID {
			continue
		}
		if event.Body == nil || event.Body["seatIndex"] == nil {
			return nil, false
		}
		seatIndex := intFromMap(event.Body, "seatIndex", -1)
		if seatIndex < 0 {
			return nil, false
		}
		return &seatIndex, true
	}
	return nil, false
}

func replayAcceptedAbortIfPresent(table nativeTableState, hand game.HoldemState) (game.HoldemState, bool, error) {
	if table.ActiveHand == nil || table.ActiveHand.State.Phase != game.StreetSettled {
		return hand, false, nil
	}
	seatIndex, ok := acceptedAbortSeatIndex(table)
	if !ok {
		return hand, false, nil
	}
	next, err := game.ForceFoldSeat(hand, *seatIndex)
	if err != nil {
		return hand, false, err
	}
	return next, true, nil
}

func (runtime *meshRuntime) replayAcceptedHandState(table nativeTableState) (game.HoldemState, error) {
	if table.ActiveHand == nil {
		return game.HoldemState{}, fmt.Errorf("hand is not active")
	}
	startingBalances := startingBalancesForHand(table, table.ActiveHand.State.HandNumber)
	hand, err := game.CreateHoldemHand(game.HoldemHandConfig{
		BigBlindSats:    table.Config.BigBlindSats,
		DealerSeatIndex: table.ActiveHand.State.DealerSeatIndex,
		HandID:          table.ActiveHand.State.HandID,
		HandNumber:      table.ActiveHand.State.HandNumber,
		Seats: []game.HoldemSeatConfig{
			{PlayerID: table.Seats[0].PlayerID, StackSats: startingBalances[table.Seats[0].PlayerID]},
			{PlayerID: table.Seats[1].PlayerID, StackSats: startingBalances[table.Seats[1].PlayerID]},
		},
		SmallBlindSats: table.Config.SmallBlindSats,
	})
	if err != nil {
		return game.HoldemState{}, err
	}
	blindTransition, previousBlindState, previousEventHash, haveBlindTransition, err := runtime.handStartTransitionForHand(table, hand.HandID)
	if err != nil {
		return game.HoldemState{}, err
	}
	if haveBlindTransition {
		blindTable := cloneJSON(table)
		blindTable.CurrentEpoch = blindTransition.NextState.Epoch
		blindTable.CustodyTransitions = nil
		blindTable.ActiveHand = &nativeActiveHand{
			Cards: nativeHandCardState{
				FinalDeck:       nil,
				PhaseDeadlineAt: "",
				Transcript: game.HandTranscript{
					HandID:     hand.HandID,
					HandNumber: hand.HandNumber,
					Records:    []game.HandTranscriptRecord{},
					RootHash:   "",
					TableID:    table.Config.TableID,
				},
			},
			State: cloneJSON(hand),
		}
		blindTable.LastEventHash = previousEventHash
		blindTable.LatestCustodyState = previousBlindState
		if err := runtime.validateCustodyTransitionSemantics(blindTable, blindTransition, nil); err != nil {
			return game.HoldemState{}, err
		}
	}

	switch table.ActiveHand.State.Phase {
	case game.StreetCommitment, game.StreetReveal, game.StreetFinalization, game.StreetPrivateDelivery:
		hand.Phase = table.ActiveHand.State.Phase
		return hand, nil
	}

	hand, err = game.ActivateHoldemHand(hand)
	if err != nil {
		return game.HoldemState{}, err
	}
	actionEvents, err := runtime.acceptedReplayActionEvents(table, hand.HandID)
	if err != nil {
		return game.HoldemState{}, err
	}

	actionIndex := 0
	for {
		if game.PhaseAllowsActions(hand.Phase) {
			if actionIndex >= len(actionEvents) {
				if next, applied, err := replayAcceptedAbortIfPresent(table, hand); err != nil {
					return game.HoldemState{}, err
				} else if applied {
					hand = next
					continue
				}
				return hand, nil
			}
			next, _, err := runtime.applyAcceptedReplayActionEvent(table, hand, actionEvents[actionIndex])
			if err != nil {
				return game.HoldemState{}, err
			}
			hand = next
			actionIndex++
			continue
		}

		switch hand.Phase {
		case game.StreetFlopReveal, game.StreetTurnReveal, game.StreetRiverReveal:
			boardOpen, ok := findTranscriptRecord(table.ActiveHand.Cards.Transcript, nativeHandMessageBoardOpen, nil, string(hand.Phase), nil)
			if !ok {
				if next, applied, err := replayAcceptedAbortIfPresent(table, hand); err != nil {
					return game.HoldemState{}, err
				} else if applied {
					hand = next
					continue
				}
				return hand, nil
			}
			next, err := game.ApplyBoardCards(hand, boardOpen.Cards)
			if err != nil {
				return game.HoldemState{}, err
			}
			hand = next
		case game.StreetShowdownReveal:
			holeCardsByPlayerID := map[string][2]game.CardCode{}
			replayedAbort := false
			for _, player := range hand.Players {
				if player.Status == game.PlayerStatusFolded {
					continue
				}
				seatIndex := player.SeatIndex
				reveal, ok := findTranscriptRecord(table.ActiveHand.Cards.Transcript, nativeHandMessageShowdownReveal, &seatIndex, string(game.StreetShowdownReveal), nil)
				if !ok {
					if next, applied, err := replayAcceptedAbortIfPresent(table, hand); err != nil {
						return game.HoldemState{}, err
					} else if applied {
						hand = next
						replayedAbort = true
						break
					}
					return hand, nil
				}
				if len(reveal.Cards) != 2 {
					return game.HoldemState{}, fmt.Errorf("showdown reveal for seat %d is incomplete", seatIndex)
				}
				holeCardsByPlayerID[player.PlayerID] = [2]game.CardCode{reveal.Cards[0], reveal.Cards[1]}
			}
			if replayedAbort {
				continue
			}
			next, err := game.SettleHoldemShowdown(hand, holeCardsByPlayerID)
			if err != nil {
				return game.HoldemState{}, err
			}
			hand = next
		case game.StreetSettled, game.StreetAborted:
			return hand, nil
		default:
			return hand, nil
		}
	}
}

func (runtime *meshRuntime) buildLocalContributionRecord(table nativeTableState) (*game.HandTranscriptRecord, error) {
	if table.ActiveHand == nil {
		return nil, nil
	}
	seat, ok := seatRecordForPlayer(table, runtime.walletID.PlayerID)
	if !ok {
		return nil, nil
	}
	secrets, ok, err := runtime.ensureLocalHandSecrets(table)
	if err != nil || !ok {
		return nil, err
	}
	transcript := table.ActiveHand.Cards.Transcript

	switch table.ActiveHand.State.Phase {
	case game.StreetCommitment:
		if _, ok := findTranscriptRecord(transcript, nativeHandMessageFairnessCommit, &seat.SeatIndex, string(game.StreetCommitment), nil); ok {
			return nil, nil
		}
		commitmentHash, err := game.BuildFairnessCommitment(table.Config.TableID, table.ActiveHand.State.HandNumber, seat.SeatIndex, seat.PlayerID, string(game.StreetCommitment), secrets.ShuffleSeedHex, secrets.LockPublicExponentHex)
		if err != nil {
			return nil, err
		}
		return &game.HandTranscriptRecord{
			CommitmentHash: commitmentHash,
			Kind:           nativeHandMessageFairnessCommit,
			Phase:          string(game.StreetCommitment),
			PlayerID:       seat.PlayerID,
			SeatIndex:      &seat.SeatIndex,
		}, nil
	case game.StreetReveal:
		if _, ok := findTranscriptRecord(transcript, nativeHandMessageFairnessReveal, &seat.SeatIndex, string(game.StreetReveal), nil); ok {
			return nil, nil
		}
		for _, other := range table.Seats {
			if other.SeatIndex >= seat.SeatIndex {
				continue
			}
			if _, ok := findTranscriptRecord(transcript, nativeHandMessageFairnessReveal, &other.SeatIndex, string(game.StreetReveal), nil); !ok {
				return nil, nil
			}
		}
		reveals := []game.MentalDeckReveal{}
		for _, other := range table.Seats {
			if other.SeatIndex > seat.SeatIndex {
				continue
			}
			record, hasReveal := findTranscriptRecord(transcript, nativeHandMessageFairnessReveal, &other.SeatIndex, string(game.StreetReveal), nil)
			if !hasReveal && other.SeatIndex != seat.SeatIndex {
				return nil, nil
			}
			if other.SeatIndex == seat.SeatIndex {
				reveals = append(reveals, game.MentalDeckReveal{
					PlayerID:              seat.PlayerID,
					SeatIndex:             seat.SeatIndex,
					ShuffleSeedHex:        secrets.ShuffleSeedHex,
					LockPublicExponentHex: secrets.LockPublicExponentHex,
				})
				continue
			}
			reveals = append(reveals, game.MentalDeckReveal{
				PlayerID:              other.PlayerID,
				SeatIndex:             other.SeatIndex,
				ShuffleSeedHex:        record.ShuffleSeedHex,
				LockPublicExponentHex: record.LockPublicExponentHex,
			})
		}
		replay, err := game.ReplayMentalDeck(reveals)
		if err != nil {
			return nil, err
		}
		return &game.HandTranscriptRecord{
			DeckStage:             replay.RevealStagesBySeat[seat.SeatIndex],
			DeckStageRoot:         replay.RevealStageRootBySeat[seat.SeatIndex],
			Kind:                  nativeHandMessageFairnessReveal,
			LockPublicExponentHex: secrets.LockPublicExponentHex,
			Phase:                 string(game.StreetReveal),
			PlayerID:              seat.PlayerID,
			SeatIndex:             &seat.SeatIndex,
			ShuffleSeedHex:        secrets.ShuffleSeedHex,
		}, nil
	case game.StreetPrivateDelivery:
		recipientSeatIndex := otherSeatIndex(seat.SeatIndex)
		if _, ok := findTranscriptRecord(transcript, nativeHandMessagePrivateDelivery, &seat.SeatIndex, string(game.StreetPrivateDelivery), &recipientSeatIndex); ok {
			return nil, nil
		}
		positions, err := requiredCardPositionsForPhase(table, game.StreetPrivateDelivery, recipientSeatIndex)
		if err != nil {
			return nil, err
		}
		partials := make([]string, 0, len(positions))
		for _, position := range positions {
			partial, err := game.DecryptMentalValueHex(table.ActiveHand.Cards.FinalDeck[position], secrets.LockPrivateExponentHex)
			if err != nil {
				return nil, err
			}
			partials = append(partials, partial)
		}
		return &game.HandTranscriptRecord{
			CardPositions:      positions,
			Kind:               nativeHandMessagePrivateDelivery,
			PartialCiphertexts: partials,
			Phase:              string(game.StreetPrivateDelivery),
			PlayerID:           seat.PlayerID,
			RecipientSeatIndex: &recipientSeatIndex,
			SeatIndex:          &seat.SeatIndex,
		}, nil
	case game.StreetFlopReveal, game.StreetTurnReveal, game.StreetRiverReveal:
		phase := table.ActiveHand.State.Phase
		if _, ok := findTranscriptRecord(transcript, nativeHandMessageBoardShare, &seat.SeatIndex, string(phase), nil); !ok {
			positions, err := requiredCardPositionsForPhase(table, phase, seat.SeatIndex)
			if err != nil {
				return nil, err
			}
			partials := make([]string, 0, len(positions))
			for _, position := range positions {
				partial, err := game.DecryptMentalValueHex(table.ActiveHand.Cards.FinalDeck[position], secrets.LockPrivateExponentHex)
				if err != nil {
					return nil, err
				}
				partials = append(partials, partial)
			}
			return &game.HandTranscriptRecord{
				CardPositions:      positions,
				Kind:               nativeHandMessageBoardShare,
				PartialCiphertexts: partials,
				Phase:              string(phase),
				PlayerID:           seat.PlayerID,
				SeatIndex:          &seat.SeatIndex,
			}, nil
		}
		if transcriptHasBoardOpen(table.ActiveHand, phase) {
			return nil, nil
		}
		opponentSeatIndex := otherSeatIndex(seat.SeatIndex)
		opponentShare, ok := findTranscriptRecord(transcript, nativeHandMessageBoardShare, &opponentSeatIndex, string(phase), nil)
		if !ok {
			return nil, nil
		}
		cards, err := decryptCardCodes(opponentShare.PartialCiphertexts, secrets.LockPrivateExponentHex)
		if err != nil {
			return nil, err
		}
		return &game.HandTranscriptRecord{
			CardPositions: opponentShare.CardPositions,
			Cards:         cards,
			Kind:          nativeHandMessageBoardOpen,
			Phase:         string(phase),
			PlayerID:      seat.PlayerID,
			SeatIndex:     &seat.SeatIndex,
		}, nil
	case game.StreetShowdownReveal:
		if table.ActiveHand.State.Players[seat.SeatIndex].Status == game.PlayerStatusFolded {
			return nil, nil
		}
		if _, ok := findTranscriptRecord(transcript, nativeHandMessageShowdownReveal, &seat.SeatIndex, string(game.StreetShowdownReveal), nil); ok {
			return nil, nil
		}
		opponentSeatIndex := otherSeatIndex(seat.SeatIndex)
		opponentShare, ok := findTranscriptRecord(transcript, nativeHandMessagePrivateDelivery, &opponentSeatIndex, string(game.StreetPrivateDelivery), &seat.SeatIndex)
		if !ok {
			return nil, nil
		}
		cards, err := decryptCardCodes(opponentShare.PartialCiphertexts, secrets.LockPrivateExponentHex)
		if err != nil {
			return nil, err
		}
		return &game.HandTranscriptRecord{
			Cards:     cards,
			Kind:      nativeHandMessageShowdownReveal,
			Phase:     string(game.StreetShowdownReveal),
			PlayerID:  seat.PlayerID,
			SeatIndex: &seat.SeatIndex,
		}, nil
	default:
		return nil, nil
	}
}

func (runtime *meshRuntime) handleProtocolTimeoutLocked(table *nativeTableState) error {
	if table == nil || table.ActiveHand == nil {
		return nil
	}
	if table.ActiveHand.Cards.PhaseDeadlineAt == "" || elapsedMillis(table.ActiveHand.Cards.PhaseDeadlineAt) < 0 {
		return nil
	}
	missing := missingProtocolSeatIndexes(*table)
	if len(missing) == 1 {
		debugMeshf("protocol timeout table=%s phase=%s missingSeat=%d", table.Config.TableID, table.ActiveHand.State.Phase, missing[0])
		return runtime.abortActiveHandLocked(table, fmt.Sprintf("protocol timeout during %s", table.ActiveHand.State.Phase), &missing[0])
	}
	if len(missing) > 1 {
		debugMeshf("protocol timeout table=%s phase=%s missingSeats=%v", table.Config.TableID, table.ActiveHand.State.Phase, missing)
		return runtime.abortActiveHandLocked(table, fmt.Sprintf("protocol timeout during %s", table.ActiveHand.State.Phase), nil)
	}
	if table.ActiveHand.State.Phase == game.StreetFlopReveal || table.ActiveHand.State.Phase == game.StreetTurnReveal || table.ActiveHand.State.Phase == game.StreetRiverReveal {
		if !transcriptHasBoardOpen(table.ActiveHand, table.ActiveHand.State.Phase) {
			debugMeshf("board open timeout table=%s phase=%s", table.Config.TableID, table.ActiveHand.State.Phase)
			return runtime.abortActiveHandLocked(table, fmt.Sprintf("board open timeout during %s", table.ActiveHand.State.Phase), nil)
		}
	}
	return nil
}

func (runtime *meshRuntime) handleActionTimeoutLocked(table *nativeTableState) (handled bool, err error) {
	if table == nil || table.ActiveHand == nil || table.LatestCustodyState == nil {
		return false, nil
	}
	timingFields := meshTimingFields{
		Metric:         "action_transition_total",
		TableID:        table.Config.TableID,
		TransitionKind: string(tablecustody.TransitionKindTimeout),
		Phase:          tablePhaseForTiming(*table),
		Purpose:        "timeout",
	}
	timing := startMeshTiming(timingFields)
	defer func() {
		timing.EndWith(timingFields, err)
	}()
	if !game.PhaseAllowsActions(table.ActiveHand.State.Phase) || table.ActiveHand.State.ActingSeatIndex == nil {
		return false, nil
	}
	if pendingTurnHasLockedCandidate(*table) {
		return false, nil
	}
	if turnTimeoutModeForTable(*table) == turnTimeoutModeChainChallenge || table.PendingTurnChallenge != nil {
		return false, nil
	}
	if turnMenuMatchesTable(*table, table.PendingTurnMenu) {
		if err := runtime.validatePendingTurnMenu(*table, table.PendingTurnMenu); err != nil {
			return false, nil
		}
	}
	deadline := runtime.currentCustodyActionDeadline(*table)
	if deadline == "" || elapsedMillis(deadline) < 0 {
		return false, nil
	}
	actingSeatIndex := *table.ActiveHand.State.ActingSeatIndex
	actingPlayerID := seatPlayerID(*table, actingSeatIndex)
	legalActions := game.GetLegalActions(table.ActiveHand.State, table.ActiveHand.State.ActingSeatIndex)
	actionTypes := make([]string, 0, len(legalActions))
	for _, action := range legalActions {
		actionTypes = append(actionTypes, string(action.Type))
	}
	resolution := tablecustody.BuildTimeoutResolution(defaultCustodyTimeoutPolicy, actingPlayerID, actionTypes, []string{actingPlayerID})
	var action game.Action
	switch resolution.ActionType {
	case string(game.ActionCheck):
		action = game.Action{Type: game.ActionCheck}
	default:
		action = game.Action{Type: game.ActionFold}
	}
	requiredSigners := runtime.requiredCustodySigners(*table, tablecustody.CustodyTransition{
		Kind:              tablecustody.TransitionKindTimeout,
		TableID:           table.Config.TableID,
		TimeoutResolution: &resolution,
	})
	nextState, err := game.ApplyHoldemAction(table.ActiveHand.State, actingSeatIndex, action)
	if err != nil {
		return false, err
	}
	custodyTransition, err := runtime.buildCustodyTransition(*table, tablecustody.TransitionKindTimeout, &nextState, &action, &resolution)
	if err != nil {
		return false, err
	}
	if err := runtime.syncTableToCustodySigners(*table, requiredSigners); err != nil {
		if recovered, recoveryErr := runtime.finalizeCustodyRecoveryTransition(table, &custodyTransition, nil); recoveryErr != nil {
			return false, recoveryErr
		} else if !recovered {
			debugMeshf("action timeout recovery deferred table=%s err=%v", table.Config.TableID, err)
			return false, nil
		}
	} else if err := runtime.finalizeCustodyTransition(table, &custodyTransition, nil); err != nil {
		if recovered, recoveryErr := runtime.finalizeCustodyRecoveryTransition(table, &custodyTransition, nil); recoveryErr != nil {
			return false, recoveryErr
		} else if !recovered {
			debugMeshf("action timeout recovery deferred table=%s err=%v", table.Config.TableID, err)
			return false, nil
		}
	}
	timingFields.CustodySeq = custodyTransition.CustodySeq
	timingFields.PlayerID = actingPlayerID
	timingFields.RequestHash = custodyApprovalTargetHash(custodyTransition)
	if err := runtime.attachDeterministicRecoveryBundles(*table, &custodyTransition, nil, &nextState); err != nil {
		return false, err
	}
	table.ActiveHand.State = nextState
	runtime.applyCustodyTransition(table, custodyTransition)
	if err := runtime.appendEvent(table, map[string]any{
		"custodySeq":        custodyTransition.CustodySeq,
		"playerId":          actingPlayerID,
		"seatIndex":         actingSeatIndex,
		"timeoutResolution": rawJSONMap(resolution),
		"transitionHash":    custodyTransition.Proof.TransitionHash,
		"type":              "PlayerAction",
	}); err != nil {
		return false, err
	}
	return true, nil
}

func (runtime *meshRuntime) abortActiveHandLocked(table *nativeTableState, reason string, offendingSeatIndex *int) error {
	if table == nil || table.ActiveHand == nil {
		return nil
	}
	debugMeshf("abort hand table=%s reason=%s offendingSeat=%v", table.Config.TableID, reason, offendingSeatIndex)
	table.PendingTurnChallenge = nil
	table.PendingTurnMenu = nil
	if err := runtime.appendEvent(table, map[string]any{
		"handId":         table.ActiveHand.State.HandID,
		"reason":         reason,
		"seatIndex":      offendingSeatIndex,
		"transcriptRoot": handTranscriptRoot(*table),
		"type":           "HandAbort",
	}); err != nil {
		return err
	}
	if offendingSeatIndex != nil {
		offendingPlayerID := seatPlayerID(*table, *offendingSeatIndex)
		resolution := tablecustody.TimeoutResolution{
			ActionType:               string(game.ActionFold),
			ActingPlayerID:           offendingPlayerID,
			DeadPlayerIDs:            []string{offendingPlayerID},
			LostEligibilityPlayerIDs: []string{offendingPlayerID},
			Policy:                   defaultCustodyTimeoutPolicy,
			Reason:                   reason,
		}
		nextState, err := game.ForceFoldSeat(table.ActiveHand.State, *offendingSeatIndex)
		if err != nil {
			return err
		}
		table.ActiveHand.State = nextState
		publicState := runtime.publicStateFromHand(*table, nextState)
		table.PublicState = &publicState
		table.ActiveHand.Cards.PhaseDeadlineAt = ""
		return runtime.finalizeSettledHandLocked(table, &resolution)
	}
	if table.LatestFullySignedSnapshot == nil {
		table.ActiveHand = nil
		table.ActiveHandStartAt = ""
		table.Config.Status = "ready"
		table.NextHandAt = addMillis(nowISO(), runtime.nextHandDelayMSForTable(*table))
		return nil
	}
	restored := runtime.publicStateFromSnapshot(*table, *table.LatestFullySignedSnapshot)
	restored.LatestEventHash = table.LastEventHash
	table.PublicState = &restored
	table.ActiveHand = nil
	table.ActiveHandStartAt = ""
	table.Config.Status = "ready"
	table.NextHandAt = addMillis(nowISO(), runtime.nextHandDelayMSForTable(*table))
	return nil
}

func (runtime *meshRuntime) finalizeSettledHandLocked(table *nativeTableState, timeoutResolution *tablecustody.TimeoutResolution) (err error) {
	if table == nil || table.ActiveHand == nil || table.ActiveHand.State.Phase != game.StreetSettled {
		return nil
	}
	timingFields := meshTimingFields{
		Metric:         "settled_hand_finalize_total",
		TableID:        table.Config.TableID,
		TransitionKind: string(tablecustody.TransitionKindShowdownPayout),
		Phase:          tablePhaseForTiming(*table),
	}
	timing := startMeshTiming(timingFields)
	defer func() {
		timing.EndWith(timingFields, err)
	}()
	if table.NextHandAt != "" {
		return nil
	}
	table.Config.Status = "active"
	publicState := runtime.publicStateFromHand(*table, table.ActiveHand.State)
	table.PublicState = &publicState
	if timeoutResolution == nil {
		timeoutResolution = latestTimeoutResolutionForHand(*table)
	}
	derivedTimeoutResolution := runtime.showdownPayoutTimeoutResolution(*table, timeoutResolution)
	showdownResolution := timeoutResolution
	showdownOverrides := (*custodyBindingOverrides)(nil)
	if derivedTimeoutResolution != nil {
		showdownResolution = derivedTimeoutResolution
		showdownOverrides = &custodyBindingOverrides{
			ActionDeadlineAt: table.LatestCustodyState.ActionDeadlineAt,
		}
	}
	custodySeq := 0
	latestCustodyStateHash := ""
	transitionHash := ""
	if table.LatestCustodyState == nil ||
		table.LatestCustodyState.HandID != table.ActiveHand.State.HandID ||
		table.LatestCustodyState.HandNumber != table.ActiveHand.State.HandNumber ||
		table.LatestCustodyState.PublicStateHash != runtime.publicMoneyStateHash(*table, &table.ActiveHand.State) {
		custodyTransition, err := runtime.buildCustodyTransitionWithOverrides(*table, tablecustody.TransitionKindShowdownPayout, &table.ActiveHand.State, nil, showdownResolution, showdownOverrides)
		if err != nil {
			return err
		}
		timingFields.CustodySeq = custodyTransition.CustodySeq
		timingFields.RequestHash = custodyApprovalTargetHash(custodyTransition)
		if err := runtime.syncTableToCustodySigners(*table, runtime.requiredCustodySigners(*table, custodyTransition)); err != nil {
			if recovered, recoveryErr := runtime.finalizeCustodyRecoveryTransition(table, &custodyTransition, nil); recoveryErr != nil {
				return recoveryErr
			} else if !recovered {
				debugMeshf("settled hand payout sync deferred table=%s err=%v", table.Config.TableID, err)
				return nil
			}
		} else if err := runtime.finalizeCustodyTransition(table, &custodyTransition, nil); err != nil {
			if recovered, recoveryErr := runtime.finalizeCustodyRecoveryTransition(table, &custodyTransition, nil); recoveryErr != nil {
				return recoveryErr
			} else if !recovered {
				debugMeshf("settled hand payout finalize deferred table=%s err=%v", table.Config.TableID, err)
				return nil
			}
		}
		runtime.applyCustodyTransition(table, custodyTransition)
		custodySeq = custodyTransition.CustodySeq
		latestCustodyStateHash = custodyTransition.NextStateHash
		transitionHash = custodyTransition.Proof.TransitionHash
	} else if table.LatestCustodyState != nil {
		custodySeq = table.LatestCustodyState.CustodySeq
		latestCustodyStateHash = table.LatestCustodyState.StateHash
		if len(table.CustodyTransitions) > 0 {
			transitionHash = table.CustodyTransitions[len(table.CustodyTransitions)-1].Proof.TransitionHash
		}
	}
	snapshot, err := runtime.buildSnapshot(*table, publicState)
	if err != nil {
		return err
	}
	table.LatestSnapshot = &snapshot
	table.LatestFullySignedSnapshot = &snapshot
	table.Snapshots = append(table.Snapshots, snapshot)
	if err := runtime.appendEvent(table, map[string]any{
		"balances":               publicState.ChipBalances,
		"checkpointHash":         runtime.snapshotHash(snapshot),
		"custodySeq":             custodySeq,
		"handId":                 table.ActiveHand.State.HandID,
		"latestCustodyStateHash": latestCustodyStateHash,
		"publicState":            rawJSONMap(publicState),
		"transcriptRoot":         handTranscriptRoot(*table),
		"type":                   "HandResult",
		"transitionHash":         transitionHash,
		"winners":                rawJSONMap(table.ActiveHand.State.Winners),
	}); err != nil {
		return err
	}
	table.ActiveHandStartAt = ""
	table.NextHandAt = addMillis(nowISO(), runtime.nextHandDelayMSForTable(*table))
	return nil
}

func settledPayoutAmountByPlayer(hand game.HoldemState) map[string]int {
	payouts := make(map[string]int, len(hand.Players))
	for _, player := range hand.Players {
		payouts[player.PlayerID] = 0
	}
	for _, winner := range hand.Winners {
		payouts[winner.PlayerID] += winner.AmountSats
	}
	return payouts
}

func (runtime *meshRuntime) showdownPayoutTimeoutResolution(table nativeTableState, baseline *tablecustody.TimeoutResolution) *tablecustody.TimeoutResolution {
	if table.ActiveHand == nil || table.ActiveHand.State.Phase != game.StreetSettled || table.LatestCustodyState == nil {
		return nil
	}
	if strings.TrimSpace(table.LatestCustodyState.ActionDeadlineAt) == "" || elapsedMillis(table.LatestCustodyState.ActionDeadlineAt) < 0 {
		return nil
	}
	baselineTransition, err := runtime.buildCustodyTransition(table, tablecustody.TransitionKindShowdownPayout, &table.ActiveHand.State, nil, baseline)
	if err != nil {
		return nil
	}
	payouts := settledPayoutAmountByPlayer(table.ActiveHand.State)
	missingPlayers := make([]string, 0)
	for _, playerID := range runtime.requiredCustodySigners(table, baselineTransition) {
		if payouts[playerID] != 0 {
			continue
		}
		missingPlayers = append(missingPlayers, playerID)
	}
	if len(missingPlayers) == 0 {
		return nil
	}
	resolution := cloneTimeoutResolution(baseline)
	if resolution == nil {
		resolution = &tablecustody.TimeoutResolution{
			ActionType:     string(game.ActionFold),
			ActingPlayerID: missingPlayers[0],
			Policy:         timeoutPolicyFromState(table.LatestCustodyState),
			Reason:         "settlement deadline expired",
		}
	}
	if strings.TrimSpace(resolution.ActingPlayerID) == "" {
		resolution.ActingPlayerID = missingPlayers[0]
	}
	resolution.ActionType = string(game.ActionFold)
	resolution.Policy = timeoutPolicyFromState(table.LatestCustodyState)
	resolution.Reason = "settlement deadline expired"
	resolution.DeadPlayerIDs = uniqueSortedPlayerIDs(append(append([]string(nil), resolution.DeadPlayerIDs...), missingPlayers...))
	resolution.LostEligibilityPlayerIDs = uniqueSortedPlayerIDs(append(append([]string(nil), resolution.LostEligibilityPlayerIDs...), missingPlayers...))
	return resolution
}

func (runtime *meshRuntime) advanceHandProtocolLocked(table *nativeTableState) error {
	if table == nil || table.ActiveHand == nil {
		return nil
	}
	for iteration := 0; iteration < 8; iteration++ {
		changed := false
		runtime.observeLockedActionState(*table)
		if tableHasActionableTurn(*table) &&
			table.CurrentHost.Peer.PeerID == runtime.selfPeerID() &&
			!turnMenuMatchesTable(*table, table.PendingTurnMenu) {
			if err := runtime.ensurePendingTurnMenuLocked(table); err != nil {
				return err
			}
			changed = true
		}
		// Protocol invariant: a successor host always consumes any locked-action
		// state before it considers challenge-open or ordinary timeout
		// substitution. Locked ordinary turns only continue through the persisted
		// settled request or the selected bundle after SettlementDeadlineAt.
		if completed, err := runtime.handlePersistedLockedActionSettlementLocked(table); err != nil {
			return err
		} else if completed {
			changed = true
			continue
		}
		if completed, err := runtime.handleLockedActionSettlementTimeoutLocked(table); err != nil {
			return err
		} else if completed {
			changed = true
			continue
		}
		if shouldTrackProtocolDeadline(table.ActiveHand.State.Phase) && table.ActiveHand.Cards.PhaseDeadlineAt == "" {
			runtime.setProtocolDeadline(table)
			changed = true
		}
		if pendingTurnAllowsUnlockedResolution(*table) {
			if handled, err := runtime.handlePendingTurnChallengeLocked(table); err != nil {
				return err
			} else if handled {
				changed = true
				continue
			}
			if opened, err := runtime.openTurnChallengeLocked(table); err != nil {
				return err
			} else if opened {
				changed = true
				continue
			}
			if handled, err := runtime.handleActionTimeoutLocked(table); err != nil {
				return err
			} else if handled {
				changed = true
			}
		}
		if record, err := runtime.buildLocalContributionRecord(*table); err != nil {
			return err
		} else if record != nil && table.CurrentHost.Peer.PeerID == runtime.selfPeerID() {
			if err := runtime.appendHandTranscriptRecord(table, *record); err != nil {
				return err
			}
			changed = true
		}
		if err := runtime.handleProtocolTimeoutLocked(table); err != nil {
			return err
		}
		if table.ActiveHand == nil {
			return nil
		}

		switch table.ActiveHand.State.Phase {
		case game.StreetCommitment:
			if len(missingProtocolSeatIndexes(*table)) == 0 {
				table.ActiveHand.State.Phase = game.StreetReveal
				runtime.setProtocolDeadline(table)
				changed = true
			}
		case game.StreetReveal:
			if len(missingProtocolSeatIndexes(*table)) == 0 {
				replay, err := runtime.buildMentalReplay(*table)
				if err != nil {
					return err
				}
				table.ActiveHand.Cards.FinalDeck = append([]string(nil), replay.FinalDeck...)
				finalDeckRoot, err := game.MentalDeckStageRoot(replay.FinalDeck)
				if err != nil {
					return err
				}
				if err := runtime.appendHandTranscriptRecord(table, game.HandTranscriptRecord{
					DeckStage:     append([]string(nil), replay.FinalDeck...),
					DeckStageRoot: finalDeckRoot,
					Kind:          "finalization",
					Phase:         string(game.StreetFinalization),
				}); err != nil {
					return err
				}
				table.ActiveHand.State.Phase = game.StreetPrivateDelivery
				runtime.setProtocolDeadline(table)
				changed = true
			}
		case game.StreetPrivateDelivery:
			if len(missingProtocolSeatIndexes(*table)) == 0 {
				nextState, err := game.ActivateHoldemHand(table.ActiveHand.State)
				if err != nil {
					return err
				}
				table.ActiveHand.State = nextState
				runtime.setProtocolDeadline(table)
				changed = true
			}
		case game.StreetFlopReveal, game.StreetTurnReveal, game.StreetRiverReveal:
			if len(missingProtocolSeatIndexes(*table)) == 0 {
				if boardOpen, ok := findTranscriptRecord(table.ActiveHand.Cards.Transcript, nativeHandMessageBoardOpen, nil, string(table.ActiveHand.State.Phase), nil); ok {
					nextState, err := game.ApplyBoardCards(table.ActiveHand.State, boardOpen.Cards)
					if err != nil {
						return err
					}
					table.ActiveHand.State = nextState
					runtime.setProtocolDeadline(table)
					changed = true
				}
			}
		case game.StreetShowdownReveal:
			if len(missingProtocolSeatIndexes(*table)) == 0 {
				holeCardsByPlayerID := map[string][2]game.CardCode{}
				for _, seatIndex := range liveShowdownSeatIndexes(*table) {
					record, ok := findTranscriptRecord(table.ActiveHand.Cards.Transcript, nativeHandMessageShowdownReveal, &seatIndex, string(game.StreetShowdownReveal), nil)
					if !ok || len(record.Cards) != 2 {
						return fmt.Errorf("missing showdown reveal for seat %d", seatIndex)
					}
					holeCardsByPlayerID[seatPlayerID(*table, seatIndex)] = [2]game.CardCode{record.Cards[0], record.Cards[1]}
				}
				nextState, err := game.SettleHoldemShowdown(table.ActiveHand.State, holeCardsByPlayerID)
				if err != nil {
					return err
				}
				table.ActiveHand.State = nextState
				runtime.setProtocolDeadline(table)
				return runtime.finalizeSettledHandLocked(table, nil)
			}
		case game.StreetSettled:
			return runtime.finalizeSettledHandLocked(table, nil)
		}

		if !changed {
			break
		}
	}
	publicState := runtime.publicStateFromHand(*table, table.ActiveHand.State)
	table.PublicState = &publicState
	return runtime.storeLocalHoleCards(*table)
}

func (runtime *meshRuntime) observeLockedActionState(table nativeTableState) {
	if table.CurrentHost.Peer.PeerID != runtime.selfPeerID() ||
		!turnMenuMatchesTable(table, table.PendingTurnMenu) ||
		table.PendingTurnMenu == nil {
		return
	}
	menu := table.PendingTurnMenu
	switch pendingTurnStageForTable(table) {
	case pendingTurnStageLocked:
		if lockedAt, err := parseISOTimestamp(menu.LockedAt); err == nil {
			emitMeshTiming(
				actionMetricFields("action_locked_unsettled_age_ms", sendActionStageSettlement, table, menu.SelectedCandidateHash, ""),
				time.Since(lockedAt),
				nil,
			)
		}
	case pendingTurnStageSettled:
		signedAt := menu.LockedAt
		if menu.SettledRequest != nil && strings.TrimSpace(menu.SettledRequest.SignedAt) != "" {
			signedAt = menu.SettledRequest.SignedAt
		}
		if settledAt, err := parseISOTimestamp(signedAt); err == nil {
			emitMeshTiming(
				actionMetricFields("action_settled_unpublished_age_ms", sendActionStageSettlement, table, menu.SelectedCandidateHash, ""),
				time.Since(settledAt),
				nil,
			)
		}
	}
}

func handMessageCards(cards []game.CardCode) []string {
	values := make([]string, 0, len(cards))
	for _, card := range cards {
		values = append(values, string(card))
	}
	return values
}

func transcriptRecordFromHandMessage(request nativeHandMessageRequest) (game.HandTranscriptRecord, error) {
	record := game.HandTranscriptRecord{
		CardPositions:         append([]int(nil), request.CardPositions...),
		CommitmentHash:        request.CommitmentHash,
		DeckStage:             append([]string(nil), request.DeckStage...),
		DeckStageRoot:         request.DeckStageRoot,
		Kind:                  request.Kind,
		LockPublicExponentHex: request.LockPublicExponentHex,
		PartialCiphertexts:    append([]string(nil), request.PartialCiphertexts...),
		Phase:                 request.Phase,
		PlayerID:              request.PlayerID,
		RecipientSeatIndex:    copyOptionalInt(request.RecipientSeatIndex),
		SeatIndex:             &request.SeatIndex,
		ShuffleSeedHex:        request.ShuffleSeedHex,
	}
	if len(request.Cards) > 0 {
		cards, err := decodeCardCodes(request.Cards)
		if err != nil {
			return game.HandTranscriptRecord{}, err
		}
		record.Cards = cards
	}
	return record, nil
}

func nativeHandMessageAuthPayload(request nativeHandMessageRequest) map[string]any {
	payload := map[string]any{
		"epoch":           request.Epoch,
		"handId":          request.HandID,
		"handNumber":      request.HandNumber,
		"kind":            request.Kind,
		"playerId":        request.PlayerID,
		"phase":           request.Phase,
		"protocolVersion": request.ProtocolVersion,
		"seatIndex":       request.SeatIndex,
		"signedAt":        request.SignedAt,
		"tableId":         request.TableID,
		"type":            "table-hand-message",
	}
	if request.CommitmentHash != "" {
		payload["commitmentHash"] = request.CommitmentHash
	}
	if len(request.DeckStage) > 0 {
		payload["deckStage"] = append([]string(nil), request.DeckStage...)
	}
	if request.DeckStageRoot != "" {
		payload["deckStageRoot"] = request.DeckStageRoot
	}
	if request.LockPublicExponentHex != "" {
		payload["lockPublicExponentHex"] = request.LockPublicExponentHex
	}
	if len(request.CardPositions) > 0 {
		payload["cardPositions"] = append([]int(nil), request.CardPositions...)
	}
	if len(request.PartialCiphertexts) > 0 {
		payload["partialCiphertexts"] = append([]string(nil), request.PartialCiphertexts...)
	}
	if len(request.Cards) > 0 {
		payload["cards"] = append([]string(nil), request.Cards...)
	}
	if request.RecipientSeatIndex != nil {
		payload["recipientSeatIndex"] = *request.RecipientSeatIndex
	}
	if request.ShuffleSeedHex != "" {
		payload["shuffleSeedHex"] = request.ShuffleSeedHex
	}
	return payload
}

func (runtime *meshRuntime) buildSignedHandMessageRequest(table nativeTableState, record game.HandTranscriptRecord) (nativeHandMessageRequest, error) {
	seat, ok := seatRecordForPlayer(table, runtime.walletID.PlayerID)
	if !ok {
		return nativeHandMessageRequest{}, fmt.Errorf("player is not seated")
	}
	request := nativeHandMessageRequest{
		CardPositions:         append([]int(nil), record.CardPositions...),
		Cards:                 handMessageCards(record.Cards),
		CommitmentHash:        record.CommitmentHash,
		DeckStage:             append([]string(nil), record.DeckStage...),
		DeckStageRoot:         record.DeckStageRoot,
		Epoch:                 table.CurrentEpoch,
		HandID:                table.ActiveHand.State.HandID,
		HandNumber:            table.ActiveHand.State.HandNumber,
		Kind:                  record.Kind,
		LockPublicExponentHex: record.LockPublicExponentHex,
		PartialCiphertexts:    append([]string(nil), record.PartialCiphertexts...),
		Phase:                 record.Phase,
		PlayerID:              runtime.walletID.PlayerID,
		ProfileName:           runtime.profileName,
		ProtocolVersion:       nativeProtocolVersion,
		RecipientSeatIndex:    copyOptionalInt(record.RecipientSeatIndex),
		SeatIndex:             seat.SeatIndex,
		ShuffleSeedHex:        record.ShuffleSeedHex,
		SignedAt:              nowISO(),
		TableID:               table.Config.TableID,
	}
	signatureHex, err := settlementcore.SignStructuredData(runtime.walletID.PrivateKeyHex, nativeHandMessageAuthPayload(request))
	if err != nil {
		return nativeHandMessageRequest{}, err
	}
	request.SignatureHex = signatureHex
	return request, nil
}

func (runtime *meshRuntime) validateHandMessageRequestIdentity(table nativeTableState, seat nativeSeatRecord, request nativeHandMessageRequest) (game.HandTranscriptRecord, error) {
	if strings.TrimSpace(request.ProtocolVersion) != nativeProtocolVersion {
		return game.HandTranscriptRecord{}, fmt.Errorf("hand message protocol version mismatch")
	}
	if request.TableID != table.Config.TableID {
		return game.HandTranscriptRecord{}, fmt.Errorf("hand message table mismatch")
	}
	if request.PlayerID != seat.PlayerID || request.SeatIndex != seat.SeatIndex {
		return game.HandTranscriptRecord{}, fmt.Errorf("hand message seat mismatch")
	}
	if table.ActiveHand == nil || request.HandID == "" || request.HandID != table.ActiveHand.State.HandID {
		return game.HandTranscriptRecord{}, fmt.Errorf("hand message hand mismatch")
	}
	if request.HandNumber != table.ActiveHand.State.HandNumber {
		return game.HandTranscriptRecord{}, fmt.Errorf("hand message hand number mismatch")
	}
	if request.SignedAt == "" || request.SignatureHex == "" {
		return game.HandTranscriptRecord{}, fmt.Errorf("hand message is missing signature")
	}
	ok, err := settlementcore.VerifyStructuredData(seat.WalletPubkeyHex, nativeHandMessageAuthPayload(request), request.SignatureHex)
	if err != nil {
		return game.HandTranscriptRecord{}, err
	}
	if !ok {
		return game.HandTranscriptRecord{}, fmt.Errorf("hand message signature is invalid")
	}
	record, err := transcriptRecordFromHandMessage(request)
	if err != nil {
		return game.HandTranscriptRecord{}, err
	}
	if err := validateTranscriptRecordPlaintext(record); err != nil {
		return game.HandTranscriptRecord{}, err
	}
	return record, nil
}

func (runtime *meshRuntime) validateNewHandMessageRequest(table nativeTableState, seat nativeSeatRecord, request nativeHandMessageRequest, record game.HandTranscriptRecord) error {
	if request.Epoch != table.CurrentEpoch {
		return fmt.Errorf("hand message epoch mismatch")
	}
	switch request.Kind {
	case nativeHandMessageFairnessCommit:
		if table.ActiveHand.State.Phase != game.StreetCommitment {
			return fmt.Errorf("hand is not accepting commitments")
		}
		if request.Phase != string(game.StreetCommitment) {
			return fmt.Errorf("commitment phase mismatch")
		}
		if strings.TrimSpace(request.CommitmentHash) == "" {
			return fmt.Errorf("commitment hash is required")
		}
	case nativeHandMessageFairnessReveal:
		if table.ActiveHand.State.Phase != game.StreetReveal {
			return fmt.Errorf("hand is not accepting reveals")
		}
		if request.Phase != string(game.StreetReveal) {
			return fmt.Errorf("reveal phase mismatch")
		}
		for _, otherSeat := range table.Seats {
			if otherSeat.SeatIndex >= seat.SeatIndex {
				continue
			}
			if _, ok := findTranscriptRecord(table.ActiveHand.Cards.Transcript, nativeHandMessageFairnessReveal, &otherSeat.SeatIndex, string(game.StreetReveal), nil); !ok {
				return fmt.Errorf("waiting for lower seat reveal before seat %d", seat.SeatIndex)
			}
		}
		commit, ok := findTranscriptRecord(table.ActiveHand.Cards.Transcript, nativeHandMessageFairnessCommit, &seat.SeatIndex, string(game.StreetCommitment), nil)
		if !ok {
			return fmt.Errorf("missing prior commitment for seat %d", seat.SeatIndex)
		}
		if err := game.VerifyFairnessReveal(table.Config.TableID, table.ActiveHand.State.HandNumber, seat.SeatIndex, seat.PlayerID, string(game.StreetCommitment), commit.CommitmentHash, request.ShuffleSeedHex, request.LockPublicExponentHex); err != nil {
			return err
		}
		reveals := []game.MentalDeckReveal{}
		for _, otherSeat := range table.Seats {
			if otherSeat.SeatIndex > seat.SeatIndex {
				continue
			}
			if otherSeat.SeatIndex == seat.SeatIndex {
				reveals = append(reveals, game.MentalDeckReveal{
					PlayerID:              seat.PlayerID,
					SeatIndex:             seat.SeatIndex,
					ShuffleSeedHex:        request.ShuffleSeedHex,
					LockPublicExponentHex: request.LockPublicExponentHex,
				})
				continue
			}
			otherReveal, ok := findTranscriptRecord(table.ActiveHand.Cards.Transcript, nativeHandMessageFairnessReveal, &otherSeat.SeatIndex, string(game.StreetReveal), nil)
			if !ok {
				return fmt.Errorf("missing lower reveal for seat %d", otherSeat.SeatIndex)
			}
			reveals = append(reveals, game.MentalDeckReveal{
				PlayerID:              otherSeat.PlayerID,
				SeatIndex:             otherSeat.SeatIndex,
				ShuffleSeedHex:        otherReveal.ShuffleSeedHex,
				LockPublicExponentHex: otherReveal.LockPublicExponentHex,
			})
		}
		replay, err := game.ReplayMentalDeck(reveals)
		if err != nil {
			return err
		}
		stage := replay.RevealStagesBySeat[seat.SeatIndex]
		if len(stage) != len(record.DeckStage) {
			return fmt.Errorf("reveal stage length mismatch")
		}
		for index := range stage {
			if stage[index] != record.DeckStage[index] {
				return fmt.Errorf("reveal stage does not match replay")
			}
		}
		if replay.RevealStageRootBySeat[seat.SeatIndex] != record.DeckStageRoot {
			return fmt.Errorf("reveal stage root does not match replay")
		}
	case nativeHandMessagePrivateDelivery:
		if table.ActiveHand.State.Phase != game.StreetPrivateDelivery {
			return fmt.Errorf("hand is not accepting private delivery shares")
		}
		if request.Phase != string(game.StreetPrivateDelivery) {
			return fmt.Errorf("private delivery phase mismatch")
		}
		expectedRecipient := otherSeatIndex(seat.SeatIndex)
		if request.RecipientSeatIndex == nil || *request.RecipientSeatIndex != expectedRecipient {
			return fmt.Errorf("private delivery recipient mismatch")
		}
		expectedPositions, err := requiredCardPositionsForPhase(table, game.StreetPrivateDelivery, *request.RecipientSeatIndex)
		if err != nil {
			return err
		}
		if len(expectedPositions) != len(request.CardPositions) {
			return fmt.Errorf("private delivery position mismatch")
		}
		for index := range expectedPositions {
			if expectedPositions[index] != request.CardPositions[index] {
				return fmt.Errorf("private delivery position mismatch")
			}
		}
		if err := verifyPartialCiphertexts(table.ActiveHand.Cards.FinalDeck, request.CardPositions, request.PartialCiphertexts, seat.SeatIndex, table.ActiveHand); err != nil {
			return err
		}
	case nativeHandMessageBoardShare:
		if table.ActiveHand.State.Phase != game.StreetFlopReveal && table.ActiveHand.State.Phase != game.StreetTurnReveal && table.ActiveHand.State.Phase != game.StreetRiverReveal {
			return fmt.Errorf("hand is not accepting board shares")
		}
		if request.Phase != string(table.ActiveHand.State.Phase) {
			return fmt.Errorf("board share phase mismatch")
		}
		expectedPositions, err := requiredCardPositionsForPhase(table, table.ActiveHand.State.Phase, seat.SeatIndex)
		if err != nil {
			return err
		}
		if len(expectedPositions) != len(request.CardPositions) {
			return fmt.Errorf("board share position mismatch")
		}
		for index := range expectedPositions {
			if expectedPositions[index] != request.CardPositions[index] {
				return fmt.Errorf("board share position mismatch")
			}
		}
		if err := verifyPartialCiphertexts(table.ActiveHand.Cards.FinalDeck, request.CardPositions, request.PartialCiphertexts, seat.SeatIndex, table.ActiveHand); err != nil {
			return err
		}
	case nativeHandMessageBoardOpen:
		if table.ActiveHand.State.Phase != game.StreetFlopReveal && table.ActiveHand.State.Phase != game.StreetTurnReveal && table.ActiveHand.State.Phase != game.StreetRiverReveal {
			return fmt.Errorf("hand is not accepting board openings")
		}
		if request.Phase != string(table.ActiveHand.State.Phase) {
			return fmt.Errorf("board open phase mismatch")
		}
		if err := verifyBoardOpen(table.ActiveHand, string(table.ActiveHand.State.Phase), request.Cards); err != nil {
			return err
		}
	case nativeHandMessageShowdownReveal:
		if table.ActiveHand.State.Phase != game.StreetShowdownReveal {
			return fmt.Errorf("hand is not accepting showdown reveals")
		}
		if request.Phase != string(game.StreetShowdownReveal) {
			return fmt.Errorf("showdown reveal phase mismatch")
		}
		if err := verifyShowdownReveal(table.ActiveHand, seat.SeatIndex, request.Cards); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported hand message kind %q", request.Kind)
	}
	return nil
}

func (runtime *meshRuntime) handleHandMessageFromPeer(request nativeHandMessageRequest) (response nativeHandMessageResponse, err error) {
	var (
		replicateView *nativeTableState
	)
	timing := startMeshTiming(meshTimingFields{
		Metric:   "hand_message_handle_total",
		TableID:  request.TableID,
		Phase:    request.Phase,
		Purpose:  request.Kind,
		PlayerID: request.PlayerID,
	})
	defer func() {
		timing.End(err)
	}()
	err = runtime.store.withTableLock(request.TableID, func() error {
		table, err := runtime.store.readTable(request.TableID)
		if err != nil || table == nil {
			return fmt.Errorf("table %s not found", request.TableID)
		}
		if table.CurrentHost.Peer.PeerID != runtime.selfPeerID() {
			return fmt.Errorf("hand message request must be sent to the current host")
		}
		if table.ActiveHand == nil {
			return fmt.Errorf("hand is not active")
		}
		seat, ok := seatRecordForPlayer(*table, request.PlayerID)
		if !ok {
			return fmt.Errorf("player is not seated")
		}
		record, err := runtime.validateHandMessageRequestIdentity(*table, seat, request)
		if err != nil {
			return err
		}
		recordKey, err := handMessageRecordKey(request.TableID, request.HandID, request.HandNumber, record)
		if err != nil {
			return err
		}
		response.RecordKey = recordKey
		existing, ok, err := findParticipantHandMessageSlotRecord(table.ActiveHand.Cards.Transcript, record)
		if err != nil {
			return err
		}
		if ok {
			existingKey, err := handMessageRecordKey(table.Config.TableID, table.ActiveHand.State.HandID, table.ActiveHand.State.HandNumber, existing)
			if err != nil {
				return err
			}
			runtime.markHostActiveLocked(table)
			response.AcceptedTranscriptRoot = existing.RootHash
			snapshot := cloneJSON(*table)
			response.Table = snapshot
			if existingKey == recordKey {
				response.Duplicate = true
				return nil
			}
			return fmt.Errorf("hand message replay conflicts with existing transcript slot %s", handMessageSlotLabel(record))
		}
		if err := runtime.validateNewHandMessageRequest(*table, seat, request, record); err != nil {
			return err
		}
		if err := runtime.appendHandTranscriptRecord(table, record); err != nil {
			return err
		}
		response.AcceptedTranscriptRoot = handTranscriptRoot(*table)
		if err := runtime.advanceHandProtocolLocked(table); err != nil {
			return err
		}
		runtime.markHostActiveLocked(table)
		if err := runtime.persistLocalTable(table, true); err != nil {
			return err
		}
		snapshot := cloneJSON(*table)
		response.Table = snapshot
		replicateView = &snapshot
		return nil
	})
	if err == nil && replicateView != nil {
		if runtime.beginBackgroundTask() {
			go func(table nativeTableState) {
				defer runtime.endBackgroundTask()
				runtime.replicateTable(table)
			}(*replicateView)
		}
	}
	return response, err
}

func (runtime *meshRuntime) driveLocalHandProtocol(tableID string) {
	if !runtime.beginProtocolDrive(tableID) {
		return
	}
	defer runtime.endProtocolDrive(tableID)
	if !runtime.isRunning() {
		return
	}
	table, err := runtime.requireLocalTable(tableID)
	if err != nil || table == nil || table.ActiveHand == nil {
		return
	}
	runtime.reconcileGuestContribution(tableID, *table)
	snapshot := snapshotProtocolDrive(*table)
	if !runtime.isRunning() {
		return
	}
	if table.CurrentHost.Peer.PeerID == runtime.selfPeerID() {
		var replicateView *nativeTableState
		_ = runtime.store.withTableLock(tableID, func() error {
			if !runtime.isRunning() {
				return nil
			}
			latest, err := runtime.store.readTable(tableID)
			if err != nil || latest == nil || latest.CurrentHost.Peer.PeerID != runtime.selfPeerID() {
				return err
			}
			if err := runtime.advanceHandProtocolLocked(latest); err != nil {
				return err
			}
			if err := runtime.persistLocalTable(latest, true); err != nil {
				return err
			}
			snapshot := cloneJSON(*latest)
			replicateView = &snapshot
			return nil
		})
		if replicateView != nil {
			runtime.replicateTable(*replicateView)
		}
		return
	}
	debugMeshf("drive local hand protocol table=%s phase=%s self=%s host=%s", tableID, table.ActiveHand.State.Phase, runtime.walletID.PlayerID, table.CurrentHost.Peer.PeerID)
	record, err := runtime.buildLocalContributionRecord(*table)
	if err != nil || record == nil {
		if err != nil {
			debugMeshf("build local contribution record failed table=%s err=%v", tableID, err)
		} else {
			debugMeshf("build local contribution record skipped table=%s phase=%s self=%s", tableID, table.ActiveHand.State.Phase, runtime.walletID.PlayerID)
		}
		return
	}
	recordKey, err := handMessageRecordKey(table.Config.TableID, table.ActiveHand.State.HandID, table.ActiveHand.State.HandNumber, *record)
	if err != nil {
		debugMeshf("compute hand message record key failed table=%s err=%v", tableID, err)
		return
	}
	if !runtime.shouldSendGuestContribution(tableID, guestContributionSnapshot(*table, *record, recordKey)) {
		emitMeshTiming(meshTimingFields{
			Metric:   "guest_protocol_dedupe_skip",
			TableID:  tableID,
			Phase:    record.Phase,
			Purpose:  record.Kind,
			PlayerID: runtime.walletID.PlayerID,
		}, 0, nil)
		debugMeshf("skip acked guest contribution table=%s phase=%s kind=%s self=%s", tableID, record.Phase, record.Kind, runtime.walletID.PlayerID)
		return
	}
	driveTiming := startMeshTiming(meshTimingFields{
		Metric:   "guest_protocol_drive_total",
		TableID:  tableID,
		Phase:    record.Phase,
		Purpose:  record.Kind,
		PlayerID: runtime.walletID.PlayerID,
	})
	var driveErr error
	defer func() {
		driveTiming.End(driveErr)
	}()
	debugMeshf("build local contribution record ready table=%s phase=%s kind=%s self=%s", tableID, table.ActiveHand.State.Phase, record.Kind, runtime.walletID.PlayerID)
	request, err := runtime.buildSignedHandMessageRequest(*table, *record)
	if err != nil {
		driveErr = err
		debugMeshf("build signed hand message failed table=%s err=%v", tableID, err)
		return
	}
	latest, latestErr := runtime.requireLocalTable(tableID)
	if latestErr != nil || latest == nil || !sameProtocolDriveSnapshot(*latest, snapshot) {
		driveErr = latestErr
		return
	}
	if !runtime.isRunning() {
		return
	}
	transportTiming := startMeshTiming(meshTimingFields{
		Metric:   "guest_transport_roundtrip_total",
		TableID:  tableID,
		Phase:    request.Phase,
		Purpose:  request.Kind,
		PlayerID: request.PlayerID,
	})
	updated, err := runtime.remoteHandMessage(table.CurrentHost.Peer.PeerURL, request)
	transportTiming.End(err)
	if err != nil {
		driveErr = err
		debugMeshf("remote hand message failed table=%s kind=%s err=%v", tableID, record.Kind, err)
		return
	}
	if updated.RecordKey != recordKey {
		driveErr = fmt.Errorf("hand message receipt record key mismatch")
		debugMeshf("remote hand message receipt mismatch table=%s kind=%s want=%s got=%s", tableID, record.Kind, recordKey, updated.RecordKey)
		return
	}
	runtime.markGuestContributionAcked(tableID, updated.RecordKey)
	debugMeshf("remote hand message accepted table=%s kind=%s phase=%s duplicate=%t self=%s", tableID, record.Kind, table.ActiveHand.State.Phase, updated.Duplicate, runtime.walletID.PlayerID)
	latest, latestErr = runtime.requireLocalTable(tableID)
	if latestErr != nil || latest == nil || !sameProtocolDriveSnapshot(*latest, snapshot) {
		driveErr = latestErr
		if latest != nil {
			runtime.reconcileGuestContribution(tableID, *latest)
		}
		return
	}
	if !runtime.isRunning() {
		return
	}
	if err := runtime.acceptRemoteTable(updated.Table); err != nil {
		driveErr = err
		debugMeshf("accept remote table after hand message failed table=%s err=%v", tableID, err)
		return
	}
	runtime.reconcileGuestContribution(tableID, updated.Table)
}
