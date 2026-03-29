package game

import (
	"fmt"

	"github.com/parkerpoker/parkerd/internal/settlementcore"
)

func copySeatPointer(value *int) *int {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func cloneTranscriptRecord(record HandTranscriptRecord) HandTranscriptRecord {
	clone := record
	clone.SeatIndex = copySeatPointer(record.SeatIndex)
	clone.RecipientSeatIndex = copySeatPointer(record.RecipientSeatIndex)
	clone.DeckStage = append([]string(nil), record.DeckStage...)
	clone.CardPositions = append([]int(nil), record.CardPositions...)
	clone.PartialCiphertexts = append([]string(nil), record.PartialCiphertexts...)
	clone.Cards = append([]CardCode(nil), record.Cards...)
	return clone
}

func transcriptUnsignedRecord(tableID, handID string, handNumber int, record HandTranscriptRecord) map[string]any {
	unsigned := map[string]any{
		"handId":     handID,
		"handNumber": handNumber,
		"index":      record.Index,
		"kind":       record.Kind,
		"phase":      record.Phase,
		"tableId":    tableID,
		"type":       "dealerless-hand-transcript-record",
	}
	if record.PlayerID != "" {
		unsigned["playerId"] = record.PlayerID
	}
	if record.SeatIndex != nil {
		unsigned["seatIndex"] = *record.SeatIndex
	}
	if record.CommitmentHash != "" {
		unsigned["commitmentHash"] = record.CommitmentHash
	}
	if record.ShuffleSeedHex != "" {
		unsigned["shuffleSeedHex"] = record.ShuffleSeedHex
	}
	if record.LockPublicExponentHex != "" {
		unsigned["lockPublicExponentHex"] = record.LockPublicExponentHex
	}
	if len(record.DeckStage) > 0 {
		unsigned["deckStage"] = append([]string(nil), record.DeckStage...)
	}
	if record.DeckStageRoot != "" {
		unsigned["deckStageRoot"] = record.DeckStageRoot
	}
	if record.RecipientSeatIndex != nil {
		unsigned["recipientSeatIndex"] = *record.RecipientSeatIndex
	}
	if len(record.CardPositions) > 0 {
		unsigned["cardPositions"] = append([]int(nil), record.CardPositions...)
	}
	if len(record.PartialCiphertexts) > 0 {
		unsigned["partialCiphertexts"] = append([]string(nil), record.PartialCiphertexts...)
	}
	if len(record.Cards) > 0 {
		unsigned["cards"] = append([]CardCode(nil), record.Cards...)
	}
	if record.Reason != "" {
		unsigned["reason"] = record.Reason
	}
	return unsigned
}

func AppendTranscriptRecord(transcript HandTranscript, record HandTranscriptRecord) (HandTranscript, HandTranscriptRecord, error) {
	record.Index = len(transcript.Records)
	unsigned := transcriptUnsignedRecord(transcript.TableID, transcript.HandID, transcript.HandNumber, record)
	stepHash, err := settlementcore.HashStructuredDataHex(unsigned)
	if err != nil {
		return HandTranscript{}, HandTranscriptRecord{}, err
	}

	rootHash, err := settlementcore.HashStructuredDataHex(map[string]any{
		"handId":     transcript.HandID,
		"handNumber": transcript.HandNumber,
		"index":      record.Index,
		"prevRoot":   transcript.RootHash,
		"stepHash":   stepHash,
		"tableId":    transcript.TableID,
		"type":       "dealerless-hand-transcript-root",
	})
	if err != nil {
		return HandTranscript{}, HandTranscriptRecord{}, err
	}

	record.StepHash = stepHash
	record.RootHash = rootHash

	next := transcript
	next.Records = append(append([]HandTranscriptRecord(nil), transcript.Records...), cloneTranscriptRecord(record))
	next.RootHash = rootHash
	return next, record, nil
}

func ReplayTranscriptRoot(transcript HandTranscript) (string, error) {
	root := ""
	for index, record := range transcript.Records {
		clone := cloneTranscriptRecord(record)
		clone.StepHash = ""
		clone.RootHash = ""
		clone.Index = index

		unsigned := transcriptUnsignedRecord(transcript.TableID, transcript.HandID, transcript.HandNumber, clone)
		stepHash, err := settlementcore.HashStructuredDataHex(unsigned)
		if err != nil {
			return "", err
		}
		if record.StepHash != "" && record.StepHash != stepHash {
			return "", fmt.Errorf("transcript step hash mismatch at index %d", index)
		}

		nextRoot, err := settlementcore.HashStructuredDataHex(map[string]any{
			"handId":     transcript.HandID,
			"handNumber": transcript.HandNumber,
			"index":      index,
			"prevRoot":   root,
			"stepHash":   stepHash,
			"tableId":    transcript.TableID,
			"type":       "dealerless-hand-transcript-root",
		})
		if err != nil {
			return "", err
		}
		if record.RootHash != "" && record.RootHash != nextRoot {
			return "", fmt.Errorf("transcript root mismatch at index %d", index)
		}
		root = nextRoot
	}
	return root, nil
}
