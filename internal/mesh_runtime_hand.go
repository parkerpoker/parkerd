package parker

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/danieldresner/arkade_fun/internal/game"
	"github.com/danieldresner/arkade_fun/internal/settlementcore"
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
	if table.ActiveHand.State.Phase != game.StreetCommitment && table.ActiveHand.State.Phase != game.StreetReveal && !allRevealsPresent {
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

func deriveLocalProtocolDeadline(existing *nativeTableState, incoming nativeTableState) string {
	if incoming.ActiveHand == nil || !shouldTrackProtocolDeadline(incoming.ActiveHand.State.Phase) {
		return ""
	}
	if existing != nil && existing.ActiveHand != nil &&
		existing.CurrentHost.Peer.PeerID == incoming.CurrentHost.Peer.PeerID &&
		existing.ActiveHand.State.HandID == incoming.ActiveHand.State.HandID &&
		existing.ActiveHand.State.Phase == incoming.ActiveHand.State.Phase &&
		existing.ActiveHand.Cards.PhaseDeadlineAt != "" &&
		shouldTrackProtocolDeadline(existing.ActiveHand.State.Phase) {
		return existing.ActiveHand.Cards.PhaseDeadlineAt
	}
	return addMillis(nowISO(), nativeHandProtocolTimeoutMS)
}

func (runtime *meshRuntime) normalizeAcceptedActiveHand(existing *nativeTableState, incoming *nativeTableState) error {
	if incoming == nil || incoming.ActiveHand == nil {
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
	incoming.ActiveHand.Cards.PhaseDeadlineAt = deriveLocalProtocolDeadline(existing, *incoming)
	return nil
}

func setProtocolDeadline(table *nativeTableState) {
	if table == nil || table.ActiveHand == nil {
		return
	}
	if shouldTrackProtocolDeadline(table.ActiveHand.State.Phase) {
		table.ActiveHand.Cards.PhaseDeadlineAt = addMillis(nowISO(), nativeHandProtocolTimeoutMS)
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
		Seats: [2]game.HoldemSeatConfig{
			{PlayerID: table.Seats[0].PlayerID, StackSats: startingBalances[table.Seats[0].PlayerID]},
			{PlayerID: table.Seats[1].PlayerID, StackSats: startingBalances[table.Seats[1].PlayerID]},
		},
		SmallBlindSats: table.Config.SmallBlindSats,
	})
	if err != nil {
		return game.HoldemState{}, err
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

	actionIndex := 0
	for {
		if game.PhaseAllowsActions(hand.Phase) {
			if actionIndex >= len(table.ActiveHand.State.ActionLog) {
				return hand, nil
			}
			record := table.ActiveHand.State.ActionLog[actionIndex]
			seat, ok := seatRecordForPlayer(table, record.ActorPlayerID)
			if !ok {
				return game.HoldemState{}, fmt.Errorf("missing seat for action actor %s", record.ActorPlayerID)
			}
			next, err := game.ApplyHoldemAction(hand, seat.SeatIndex, record.Action)
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
				return hand, nil
			}
			next, err := game.ApplyBoardCards(hand, boardOpen.Cards)
			if err != nil {
				return game.HoldemState{}, err
			}
			hand = next
		case game.StreetShowdownReveal:
			holeCardsByPlayerID := map[string][2]game.CardCode{}
			for _, player := range hand.Players {
				if player.Status == game.PlayerStatusFolded {
					continue
				}
				seatIndex := player.SeatIndex
				reveal, ok := findTranscriptRecord(table.ActiveHand.Cards.Transcript, nativeHandMessageShowdownReveal, &seatIndex, string(game.StreetShowdownReveal), nil)
				if !ok {
					return hand, nil
				}
				if len(reveal.Cards) != 2 {
					return game.HoldemState{}, fmt.Errorf("showdown reveal for seat %d is incomplete", seatIndex)
				}
				holeCardsByPlayerID[player.PlayerID] = [2]game.CardCode{reveal.Cards[0], reveal.Cards[1]}
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
		return runtime.abortActiveHandLocked(table, fmt.Sprintf("protocol timeout during %s", table.ActiveHand.State.Phase), &missing[0])
	}
	if len(missing) > 1 {
		return runtime.abortActiveHandLocked(table, fmt.Sprintf("protocol timeout during %s", table.ActiveHand.State.Phase), nil)
	}
	if table.ActiveHand.State.Phase == game.StreetFlopReveal || table.ActiveHand.State.Phase == game.StreetTurnReveal || table.ActiveHand.State.Phase == game.StreetRiverReveal {
		if !transcriptHasBoardOpen(table.ActiveHand, table.ActiveHand.State.Phase) {
			return runtime.abortActiveHandLocked(table, fmt.Sprintf("board open timeout during %s", table.ActiveHand.State.Phase), nil)
		}
	}
	return nil
}

func (runtime *meshRuntime) abortActiveHandLocked(table *nativeTableState, reason string, offendingSeatIndex *int) error {
	if table == nil || table.ActiveHand == nil {
		return nil
	}
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
		nextState, err := game.ForceFoldSeat(table.ActiveHand.State, *offendingSeatIndex)
		if err != nil {
			return err
		}
		table.ActiveHand.State = nextState
		publicState := runtime.publicStateFromHand(*table, nextState)
		table.PublicState = &publicState
		table.ActiveHand.Cards.PhaseDeadlineAt = ""
		return runtime.finalizeSettledHandLocked(table)
	}
	if table.LatestFullySignedSnapshot == nil {
		table.ActiveHand = nil
		table.Config.Status = "ready"
		table.NextHandAt = addMillis(nowISO(), nativeNextHandDelayMS)
		return nil
	}
	restored := runtime.publicStateFromSnapshot(*table, *table.LatestFullySignedSnapshot)
	table.PublicState = &restored
	table.ActiveHand = nil
	table.Config.Status = "ready"
	table.NextHandAt = addMillis(nowISO(), nativeNextHandDelayMS)
	snapshot, err := runtime.buildSnapshot(*table, restored)
	if err != nil {
		return err
	}
	table.LatestSnapshot = &snapshot
	table.LatestFullySignedSnapshot = &snapshot
	table.Snapshots = append(table.Snapshots, snapshot)
	return nil
}

func (runtime *meshRuntime) finalizeSettledHandLocked(table *nativeTableState) error {
	if table == nil || table.ActiveHand == nil || table.ActiveHand.State.Phase != game.StreetSettled {
		return nil
	}
	if table.NextHandAt != "" {
		return nil
	}
	table.Config.Status = "active"
	publicState := runtime.publicStateFromHand(*table, table.ActiveHand.State)
	table.PublicState = &publicState
	snapshot, err := runtime.buildSnapshot(*table, publicState)
	if err != nil {
		return err
	}
	table.LatestSnapshot = &snapshot
	table.LatestFullySignedSnapshot = &snapshot
	table.Snapshots = append(table.Snapshots, snapshot)
	if err := runtime.appendEvent(table, map[string]any{
		"balances":       publicState.ChipBalances,
		"checkpointHash": runtime.snapshotHash(snapshot),
		"handId":         table.ActiveHand.State.HandID,
		"publicState":    rawJSONMap(publicState),
		"transcriptRoot": handTranscriptRoot(*table),
		"type":           "HandResult",
		"winners":        rawJSONMap(table.ActiveHand.State.Winners),
	}); err != nil {
		return err
	}
	table.NextHandAt = addMillis(nowISO(), nativeNextHandDelayMS)
	return nil
}

func (runtime *meshRuntime) advanceHandProtocolLocked(table *nativeTableState) error {
	if table == nil || table.ActiveHand == nil {
		return nil
	}
	for iteration := 0; iteration < 8; iteration++ {
		changed := false
		if err := runtime.handleProtocolTimeoutLocked(table); err != nil {
			return err
		}
		if table.ActiveHand == nil {
			return nil
		}

		if record, err := runtime.buildLocalContributionRecord(*table); err != nil {
			return err
		} else if record != nil && table.CurrentHost.Peer.PeerID == runtime.selfPeerID() {
			if err := runtime.appendHandTranscriptRecord(table, *record); err != nil {
				return err
			}
			changed = true
		}

		switch table.ActiveHand.State.Phase {
		case game.StreetCommitment:
			if len(missingProtocolSeatIndexes(*table)) == 0 {
				table.ActiveHand.State.Phase = game.StreetReveal
				setProtocolDeadline(table)
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
				setProtocolDeadline(table)
				changed = true
			}
		case game.StreetPrivateDelivery:
			if len(missingProtocolSeatIndexes(*table)) == 0 {
				nextState, err := game.ActivateHoldemHand(table.ActiveHand.State)
				if err != nil {
					return err
				}
				table.ActiveHand.State = nextState
				setProtocolDeadline(table)
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
					setProtocolDeadline(table)
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
				setProtocolDeadline(table)
				return runtime.finalizeSettledHandLocked(table)
			}
		case game.StreetSettled:
			return runtime.finalizeSettledHandLocked(table)
		}

		if !changed {
			break
		}
	}
	publicState := runtime.publicStateFromHand(*table, table.ActiveHand.State)
	table.PublicState = &publicState
	return runtime.storeLocalHoleCards(*table)
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
		"epoch":      request.Epoch,
		"handId":     request.HandID,
		"handNumber": request.HandNumber,
		"kind":       request.Kind,
		"playerId":   request.PlayerID,
		"phase":      request.Phase,
		"seatIndex":  request.SeatIndex,
		"signedAt":   request.SignedAt,
		"tableId":    request.TableID,
		"type":       "table-hand-message",
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

func (runtime *meshRuntime) validateHandMessageRequest(table nativeTableState, seat nativeSeatRecord, request nativeHandMessageRequest) error {
	if request.TableID != table.Config.TableID {
		return fmt.Errorf("hand message table mismatch")
	}
	if request.PlayerID != seat.PlayerID || request.SeatIndex != seat.SeatIndex {
		return fmt.Errorf("hand message seat mismatch")
	}
	if table.ActiveHand == nil || request.HandID == "" || request.HandID != table.ActiveHand.State.HandID {
		return fmt.Errorf("hand message hand mismatch")
	}
	if request.HandNumber != table.ActiveHand.State.HandNumber {
		return fmt.Errorf("hand message hand number mismatch")
	}
	if request.Epoch != table.CurrentEpoch {
		return fmt.Errorf("hand message epoch mismatch")
	}
	if request.SignedAt == "" || request.SignatureHex == "" {
		return fmt.Errorf("hand message is missing signature")
	}
	ok, err := settlementcore.VerifyStructuredData(seat.WalletPubkeyHex, nativeHandMessageAuthPayload(request), request.SignatureHex)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("hand message signature is invalid")
	}

	switch request.Kind {
	case nativeHandMessageFairnessCommit:
		if table.ActiveHand.State.Phase != game.StreetCommitment {
			return fmt.Errorf("hand is not accepting commitments")
		}
		if _, ok := findTranscriptRecord(table.ActiveHand.Cards.Transcript, nativeHandMessageFairnessCommit, &seat.SeatIndex, string(game.StreetCommitment), nil); ok {
			return fmt.Errorf("commit already recorded for seat %d", seat.SeatIndex)
		}
		if strings.TrimSpace(request.CommitmentHash) == "" {
			return fmt.Errorf("commitment hash is required")
		}
	case nativeHandMessageFairnessReveal:
		if table.ActiveHand.State.Phase != game.StreetReveal {
			return fmt.Errorf("hand is not accepting reveals")
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
		if _, ok := findTranscriptRecord(table.ActiveHand.Cards.Transcript, nativeHandMessageFairnessReveal, &seat.SeatIndex, string(game.StreetReveal), nil); ok {
			return fmt.Errorf("reveal already recorded for seat %d", seat.SeatIndex)
		}
		if err := game.VerifyFairnessReveal(table.Config.TableID, table.ActiveHand.State.HandNumber, seat.SeatIndex, seat.PlayerID, string(game.StreetCommitment), commit.CommitmentHash, request.ShuffleSeedHex, request.LockPublicExponentHex); err != nil {
			return err
		}
		record, err := transcriptRecordFromHandMessage(request)
		if err != nil {
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
		expectedRecipient := otherSeatIndex(seat.SeatIndex)
		if request.RecipientSeatIndex == nil || *request.RecipientSeatIndex != expectedRecipient {
			return fmt.Errorf("private delivery recipient mismatch")
		}
		if _, ok := findTranscriptRecord(table.ActiveHand.Cards.Transcript, nativeHandMessagePrivateDelivery, &seat.SeatIndex, string(game.StreetPrivateDelivery), request.RecipientSeatIndex); ok {
			return fmt.Errorf("private delivery share already recorded for seat %d", seat.SeatIndex)
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
		if _, ok := findTranscriptRecord(table.ActiveHand.Cards.Transcript, nativeHandMessageBoardShare, &seat.SeatIndex, string(table.ActiveHand.State.Phase), nil); ok {
			return fmt.Errorf("board share already recorded for seat %d", seat.SeatIndex)
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
		if transcriptHasBoardOpen(table.ActiveHand, table.ActiveHand.State.Phase) {
			return fmt.Errorf("board already opened for %s", table.ActiveHand.State.Phase)
		}
		if err := verifyBoardOpen(table.ActiveHand, string(table.ActiveHand.State.Phase), request.Cards); err != nil {
			return err
		}
	case nativeHandMessageShowdownReveal:
		if table.ActiveHand.State.Phase != game.StreetShowdownReveal {
			return fmt.Errorf("hand is not accepting showdown reveals")
		}
		if _, ok := findTranscriptRecord(table.ActiveHand.Cards.Transcript, nativeHandMessageShowdownReveal, &seat.SeatIndex, string(game.StreetShowdownReveal), nil); ok {
			return fmt.Errorf("showdown reveal already recorded for seat %d", seat.SeatIndex)
		}
		if err := verifyShowdownReveal(table.ActiveHand, seat.SeatIndex, request.Cards); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported hand message kind %q", request.Kind)
	}
	return nil
}

func (runtime *meshRuntime) handleHandMessageFromPeer(request nativeHandMessageRequest) (nativeTableState, error) {
	var updated nativeTableState
	err := runtime.store.withTableLock(request.TableID, func() error {
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
		if err := runtime.validateHandMessageRequest(*table, seat, request); err != nil {
			return err
		}
		record, err := transcriptRecordFromHandMessage(request)
		if err != nil {
			return err
		}
		if err := runtime.appendHandTranscriptRecord(table, record); err != nil {
			return err
		}
		if err := runtime.advanceHandProtocolLocked(table); err != nil {
			return err
		}
		if err := runtime.persistAndReplicate(table, true); err != nil {
			return err
		}
		updated = *table
		return nil
	})
	return updated, err
}

func (runtime *meshRuntime) driveLocalHandProtocol(tableID string) {
	table, err := runtime.requireLocalTable(tableID)
	if err != nil || table == nil || table.ActiveHand == nil {
		return
	}
	if table.CurrentHost.Peer.PeerID == runtime.selfPeerID() {
		_ = runtime.store.withTableLock(tableID, func() error {
			latest, err := runtime.store.readTable(tableID)
			if err != nil || latest == nil || latest.CurrentHost.Peer.PeerID != runtime.selfPeerID() {
				return err
			}
			if err := runtime.advanceHandProtocolLocked(latest); err != nil {
				return err
			}
			return runtime.persistAndReplicate(latest, true)
		})
		return
	}
	record, err := runtime.buildLocalContributionRecord(*table)
	if err != nil || record == nil {
		return
	}
	request, err := runtime.buildSignedHandMessageRequest(*table, *record)
	if err != nil {
		return
	}
	updated, err := runtime.remoteHandMessage(table.CurrentHost.Peer.PeerURL, request)
	if err != nil {
		return
	}
	_ = runtime.acceptRemoteTable(updated)
}
