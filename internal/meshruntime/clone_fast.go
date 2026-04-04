package meshruntime

import (
	"encoding/json"

	arktree "github.com/arkade-os/arkd/pkg/ark-lib/tree"
	"github.com/parkerpoker/parkerd/internal/game"
	"github.com/parkerpoker/parkerd/internal/tablecustody"
)

func cloneNativeTableStateFast(state nativeTableState) nativeTableState {
	cloned := state
	cloned.Advertisement = cloneAdvertisementPtrFast(state.Advertisement)
	cloned.ActiveHand = cloneActiveHandPtrFast(state.ActiveHand)
	cloned.Config = cloneMeshTableConfigFast(state.Config)
	cloned.CurrentHost = cloneKnownParticipantFast(state.CurrentHost)
	cloned.CustodyTransitions = cloneCustodyTransitionsFast(state.CustodyTransitions)
	cloned.Events = cloneSignedTableEventsFast(state.Events)
	cloned.LatestCustodyState = cloneCustodyStatePtrFast(state.LatestCustodyState)
	cloned.LatestFullySignedSnapshot = cloneSnapshotPtrFast(state.LatestFullySignedSnapshot)
	cloned.LatestSnapshot = cloneSnapshotPtrFast(state.LatestSnapshot)
	cloned.PendingTurnChallenge = clonePendingTurnChallengePtrFast(state.PendingTurnChallenge)
	cloned.PendingTurnMenu = clonePendingTurnMenuPtrFast(state.PendingTurnMenu)
	cloned.PublicState = clonePublicStatePtrFast(state.PublicState)
	cloned.Seats = cloneSeatRecordsFast(state.Seats)
	cloned.Snapshots = cloneSnapshotsFast(state.Snapshots)
	cloned.Witnesses = cloneKnownParticipantsFast(state.Witnesses)
	return cloned
}

func cloneAdvertisementPtrFast(value *NativeAdvertisement) *NativeAdvertisement {
	if value == nil {
		return nil
	}
	cloned := *value
	cloned.HostModeCapabilities = cloneStringSliceFast(value.HostModeCapabilities)
	cloned.Stakes = cloneStringIntMapFast(value.Stakes)
	return &cloned
}

func cloneMeshTableConfigFast(config NativeMeshTableConfig) NativeMeshTableConfig {
	cloned := config
	cloned.ActionMenuPolicy = ActionMenuPolicy{
		PotFractionBps: cloneIntSliceFast(config.ActionMenuPolicy.PotFractionBps),
	}
	return cloned
}

func cloneKnownParticipantFast(value nativeKnownParticipant) nativeKnownParticipant {
	cloned := value
	cloned.Peer = clonePeerAddressFast(value.Peer)
	return cloned
}

func cloneKnownParticipantsFast(values []nativeKnownParticipant) []nativeKnownParticipant {
	if values == nil {
		return nil
	}
	cloned := make([]nativeKnownParticipant, len(values))
	for index, value := range values {
		cloned[index] = cloneKnownParticipantFast(value)
	}
	return cloned
}

func clonePeerAddressFast(value NativePeerAddress) NativePeerAddress {
	cloned := value
	cloned.Roles = cloneStringSliceFast(value.Roles)
	return cloned
}

func cloneActiveHandFast(value nativeActiveHand) nativeActiveHand {
	cloned := value
	cloned.Cards = cloneHandCardStateFast(value.Cards)
	cloned.State = cloneHoldemStateFast(value.State)
	return cloned
}

func cloneActiveHandPtrFast(value *nativeActiveHand) *nativeActiveHand {
	if value == nil {
		return nil
	}
	cloned := cloneActiveHandFast(*value)
	return &cloned
}

func cloneHandCardStateFast(value nativeHandCardState) nativeHandCardState {
	cloned := value
	cloned.FinalDeck = cloneStringSliceFast(value.FinalDeck)
	cloned.Transcript = cloneHandTranscriptFast(value.Transcript)
	return cloned
}

func cloneHandTranscriptFast(value game.HandTranscript) game.HandTranscript {
	cloned := value
	cloned.Records = cloneTranscriptRecordsFast(value.Records)
	return cloned
}

func cloneTranscriptRecordsFast(values []game.HandTranscriptRecord) []game.HandTranscriptRecord {
	if values == nil {
		return nil
	}
	cloned := make([]game.HandTranscriptRecord, len(values))
	for index, value := range values {
		cloned[index] = cloneTranscriptRecordFast(value)
	}
	return cloned
}

func cloneTranscriptRecordFast(value game.HandTranscriptRecord) game.HandTranscriptRecord {
	cloned := value
	cloned.SeatIndex = cloneIntPtrFast(value.SeatIndex)
	cloned.DeckStage = cloneStringSliceFast(value.DeckStage)
	cloned.RecipientSeatIndex = cloneIntPtrFast(value.RecipientSeatIndex)
	cloned.CardPositions = cloneIntSliceFast(value.CardPositions)
	cloned.PartialCiphertexts = cloneStringSliceFast(value.PartialCiphertexts)
	cloned.Cards = cloneCardCodesFast(value.Cards)
	return cloned
}

func cloneHoldemStateFast(value game.HoldemState) game.HoldemState {
	cloned := value
	cloned.ActingSeatIndex = cloneIntPtrFast(value.ActingSeatIndex)
	cloned.RaiseLockedSeatIndex = cloneIntPtrFast(value.RaiseLockedSeatIndex)
	cloned.Board = cloneCardCodesFast(value.Board)
	cloned.Players = cloneHoldemPlayersFast(value.Players)
	cloned.Winners = cloneHoldemWinnersFast(value.Winners)
	cloned.ShowdownScores = cloneShowdownScoresFast(value.ShowdownScores)
	cloned.ActionLog = cloneHoldemActionLogFast(value.ActionLog)
	return cloned
}

func cloneHoldemPlayersFast(values []game.HoldemPlayerState) []game.HoldemPlayerState {
	if values == nil {
		return nil
	}
	cloned := make([]game.HoldemPlayerState, len(values))
	copy(cloned, values)
	return cloned
}

func cloneHoldemWinnersFast(values []game.HoldemWinner) []game.HoldemWinner {
	if values == nil {
		return nil
	}
	cloned := make([]game.HoldemWinner, len(values))
	for index, value := range values {
		cloned[index] = value
		cloned[index].HandScore = cloneHandScorePtrFast(value.HandScore)
	}
	return cloned
}

func cloneHandScorePtrFast(value *game.HandScore) *game.HandScore {
	if value == nil {
		return nil
	}
	cloned := *value
	cloned.RankValues = cloneIntSliceFast(value.RankValues)
	cloned.BestFive = cloneCardCodesFast(value.BestFive)
	return &cloned
}

func cloneShowdownScoresFast(values map[string]game.HandScore) map[string]game.HandScore {
	if values == nil {
		return nil
	}
	cloned := make(map[string]game.HandScore, len(values))
	for key, value := range values {
		copied := value
		copied.RankValues = cloneIntSliceFast(value.RankValues)
		copied.BestFive = cloneCardCodesFast(value.BestFive)
		cloned[key] = copied
	}
	return cloned
}

func cloneHoldemActionLogFast(values []game.HoldemActionRecord) []game.HoldemActionRecord {
	if values == nil {
		return nil
	}
	cloned := make([]game.HoldemActionRecord, len(values))
	copy(cloned, values)
	return cloned
}

func cloneSeatRecordsFast(values []nativeSeatRecord) []nativeSeatRecord {
	if values == nil {
		return nil
	}
	cloned := make([]nativeSeatRecord, len(values))
	for index, value := range values {
		cloned[index] = value
		cloned[index].NativeSeatedPlayer = cloneSeatedPlayerFast(value.NativeSeatedPlayer)
	}
	return cloned
}

func cloneSeatedPlayersFast(values []NativeSeatedPlayer) []NativeSeatedPlayer {
	if values == nil {
		return nil
	}
	cloned := make([]NativeSeatedPlayer, len(values))
	for index, value := range values {
		cloned[index] = cloneSeatedPlayerFast(value)
	}
	return cloned
}

func cloneSeatedPlayerFast(value NativeSeatedPlayer) NativeSeatedPlayer {
	cloned := value
	cloned.FundingRefs = cloneVTXORefsFast(value.FundingRefs)
	return cloned
}

func cloneSignedTableEventsFast(values []NativeSignedTableEvent) []NativeSignedTableEvent {
	if values == nil {
		return nil
	}
	cloned := make([]NativeSignedTableEvent, len(values))
	for index, value := range values {
		cloned[index] = cloneSignedTableEventFast(value)
	}
	return cloned
}

func cloneSignedTableEventFast(value NativeSignedTableEvent) NativeSignedTableEvent {
	cloned := value
	cloned.Body = cloneAnyMapFast(value.Body)
	return cloned
}

func clonePendingTurnChallengePtrFast(value *NativePendingTurnChallenge) *NativePendingTurnChallenge {
	if value == nil {
		return nil
	}
	cloned := clonePendingTurnChallengeFast(*value)
	return &cloned
}

func clonePendingTurnChallengeFast(value NativePendingTurnChallenge) NativePendingTurnChallenge {
	cloned := value
	cloned.ChallengeRef = cloneVTXORefFast(value.ChallengeRef)
	cloned.OptionIDs = cloneOptionalStringSliceFast(value.OptionIDs)
	cloned.TimeoutResolution = cloneTimeoutResolutionPtrFast(value.TimeoutResolution)
	return cloned
}

func clonePendingTurnMenuPtrFast(value *NativePendingTurnMenu) *NativePendingTurnMenu {
	if value == nil {
		return nil
	}
	cloned := clonePendingTurnMenuFast(*value)
	return &cloned
}

func clonePendingTurnMenuFast(value NativePendingTurnMenu) NativePendingTurnMenu {
	cloned := value
	cloned.AcceptedIntentAck = cloneCandidateIntentAckPtrFast(value.AcceptedIntentAck)
	cloned.Candidates = cloneOptionalTurnCandidateBundlesFast(value.Candidates)
	cloned.ChallengeEnvelope = cloneChallengeEnvelopePtrFast(value.ChallengeEnvelope)
	cloned.Options = cloneOptionalActionMenuOptionsFast(value.Options)
	cloned.TimeoutCandidate = cloneTurnCandidateBundleFast(value.TimeoutCandidate)
	return cloned
}

func cloneActionMenuOptionsFast(values []NativeActionMenuOption) []NativeActionMenuOption {
	if values == nil {
		return nil
	}
	cloned := make([]NativeActionMenuOption, len(values))
	copy(cloned, values)
	return cloned
}

func cloneOptionalActionMenuOptionsFast(values []NativeActionMenuOption) []NativeActionMenuOption {
	if len(values) == 0 {
		return nil
	}
	return cloneActionMenuOptionsFast(values)
}

func cloneChallengeEnvelopePtrFast(value *NativeChallengeEnvelope) *NativeChallengeEnvelope {
	if value == nil {
		return nil
	}
	cloned := cloneChallengeEnvelopeFast(*value)
	return &cloned
}

func cloneChallengeEnvelopeFast(value NativeChallengeEnvelope) NativeChallengeEnvelope {
	cloned := value
	cloned.OpenBundle = cloneCustodyChallengeBundleFast(value.OpenBundle)
	cloned.OpenTransition = cloneCustodyTransitionFast(value.OpenTransition)
	cloned.OptionResolutionBundles = cloneCustodyChallengeBundlesFast(value.OptionResolutionBundles)
	cloned.EscapeBundle = cloneCustodyChallengeBundleFast(value.EscapeBundle)
	cloned.TimeoutResolutionBundle = cloneCustodyChallengeBundleFast(value.TimeoutResolutionBundle)
	return cloned
}

func cloneTurnCandidateBundlesFast(values []NativeTurnCandidateBundle) []NativeTurnCandidateBundle {
	if values == nil {
		return nil
	}
	cloned := make([]NativeTurnCandidateBundle, len(values))
	for index, value := range values {
		cloned[index] = cloneTurnCandidateBundleFast(value)
	}
	return cloned
}

func cloneOptionalTurnCandidateBundlesFast(values []NativeTurnCandidateBundle) []NativeTurnCandidateBundle {
	if len(values) == 0 {
		return nil
	}
	return cloneTurnCandidateBundlesFast(values)
}

func cloneTurnCandidateBundleFast(value NativeTurnCandidateBundle) NativeTurnCandidateBundle {
	cloned := value
	cloned.ActionRequest = cloneActionRequestPtrFast(value.ActionRequest)
	cloned.AuthorizedOutputs = cloneOptionalCustodyBatchOutputsFast(value.AuthorizedOutputs)
	cloned.ProofSignerIDs = cloneOptionalStringSliceFast(value.ProofSignerIDs)
	cloned.SignerPubkeys = cloneOptionalStringStringMapFast(value.SignerPubkeys)
	cloned.TimeoutResolution = cloneTimeoutResolutionPtrFast(value.TimeoutResolution)
	cloned.Transition = cloneCustodyTransitionFast(value.Transition)
	cloned.TreeSignerIDs = cloneOptionalStringSliceFast(value.TreeSignerIDs)
	return cloned
}

func cloneActionRequestPtrFast(value *nativeActionRequest) *nativeActionRequest {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneCustodyBatchOutputsFast(values []custodyBatchOutput) []custodyBatchOutput {
	if values == nil {
		return nil
	}
	cloned := make([]custodyBatchOutput, len(values))
	for index, value := range values {
		cloned[index] = value
		cloned[index].Tapscripts = cloneStringSliceFast(value.Tapscripts)
	}
	return cloned
}

func cloneOptionalCustodyBatchOutputsFast(values []custodyBatchOutput) []custodyBatchOutput {
	if len(values) == 0 {
		return nil
	}
	return cloneCustodyBatchOutputsFast(values)
}

func clonePublicStatePtrFast(value *NativePublicTableState) *NativePublicTableState {
	if value == nil {
		return nil
	}
	cloned := clonePublicStateFast(*value)
	return &cloned
}

func clonePublicStateFast(value NativePublicTableState) NativePublicTableState {
	cloned := value
	cloned.Board = cloneStringSliceFast(value.Board)
	cloned.ChipBalances = cloneStringIntMapFast(value.ChipBalances)
	cloned.DealerCommitment = cloneDealerCommitmentPtrFast(value.DealerCommitment)
	cloned.FoldedPlayerIDs = cloneStringSliceFast(value.FoldedPlayerIDs)
	cloned.LivePlayerIDs = cloneStringSliceFast(value.LivePlayerIDs)
	cloned.RoundContributions = cloneStringIntMapFast(value.RoundContributions)
	cloned.SeatedPlayers = cloneSeatedPlayersFast(value.SeatedPlayers)
	cloned.TotalContributions = cloneStringIntMapFast(value.TotalContributions)
	return cloned
}

func cloneDealerCommitmentPtrFast(value *NativeDealerCommitment) *NativeDealerCommitment {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneSnapshotPtrFast(value *NativeCooperativeTableSnapshot) *NativeCooperativeTableSnapshot {
	if value == nil {
		return nil
	}
	cloned := cloneSnapshotFast(*value)
	return &cloned
}

func cloneSnapshotsFast(values []NativeCooperativeTableSnapshot) []NativeCooperativeTableSnapshot {
	if values == nil {
		return nil
	}
	cloned := make([]NativeCooperativeTableSnapshot, len(values))
	for index, value := range values {
		cloned[index] = cloneSnapshotFast(value)
	}
	return cloned
}

func cloneSnapshotFast(value NativeCooperativeTableSnapshot) NativeCooperativeTableSnapshot {
	cloned := value
	cloned.ChipBalances = cloneStringIntMapFast(value.ChipBalances)
	cloned.FoldedPlayerIDs = cloneStringSliceFast(value.FoldedPlayerIDs)
	cloned.LivePlayerIDs = cloneStringSliceFast(value.LivePlayerIDs)
	cloned.SeatedPlayers = cloneSeatedPlayersFast(value.SeatedPlayers)
	cloned.SidePots = cloneIntSliceFast(value.SidePots)
	cloned.Signatures = cloneSnapshotSignaturesFast(value.Signatures)
	return cloned
}

func cloneSnapshotSignaturesFast(values []NativeTableSnapshotSignature) []NativeTableSnapshotSignature {
	if values == nil {
		return nil
	}
	cloned := make([]NativeTableSnapshotSignature, len(values))
	copy(cloned, values)
	return cloned
}

func cloneCustodyTransitionsFast(values []tablecustody.CustodyTransition) []tablecustody.CustodyTransition {
	if len(values) == 0 {
		return nil
	}
	cloned := make([]tablecustody.CustodyTransition, len(values))
	for index, value := range values {
		cloned[index] = cloneCustodyTransitionFast(value)
	}
	return cloned
}

func cloneCustodyTransitionFast(value tablecustody.CustodyTransition) tablecustody.CustodyTransition {
	cloned := value
	cloned.Action = cloneActionDescriptorPtrFast(value.Action)
	cloned.Approvals = cloneCustodySignaturesFast(value.Approvals)
	cloned.NextState = cloneCustodyStateFast(value.NextState)
	cloned.Proof = cloneCustodyProofFast(value.Proof)
	cloned.TimeoutResolution = cloneTimeoutResolutionPtrFast(value.TimeoutResolution)
	return cloned
}

func cloneActionDescriptorPtrFast(value *tablecustody.ActionDescriptor) *tablecustody.ActionDescriptor {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneCustodyStatePtrFast(value *tablecustody.CustodyState) *tablecustody.CustodyState {
	if value == nil {
		return nil
	}
	cloned := cloneCustodyStateFast(*value)
	return &cloned
}

func cloneCustodyStateFast(value tablecustody.CustodyState) tablecustody.CustodyState {
	cloned := value
	cloned.PotSlices = clonePotSlicesFast(value.PotSlices)
	cloned.StackClaims = cloneStackClaimsFast(value.StackClaims)
	return cloned
}

func cloneStackClaimsFast(values []tablecustody.StackClaim) []tablecustody.StackClaim {
	if len(values) == 0 {
		return nil
	}
	cloned := make([]tablecustody.StackClaim, len(values))
	for index, value := range values {
		cloned[index] = value
		cloned[index].VTXORefs = cloneVTXORefsFast(value.VTXORefs)
	}
	return cloned
}

func clonePotSlicesFast(values []tablecustody.PotSlice) []tablecustody.PotSlice {
	if len(values) == 0 {
		return nil
	}
	cloned := make([]tablecustody.PotSlice, len(values))
	for index, value := range values {
		cloned[index] = value
		cloned[index].ContributedPlayerIDs = cloneOptionalStringSliceFast(value.ContributedPlayerIDs)
		cloned[index].Contributions = cloneOptionalStringIntMapFast(value.Contributions)
		cloned[index].EligiblePlayerIDs = cloneOptionalStringSliceFast(value.EligiblePlayerIDs)
		cloned[index].OddChipPlayerIDs = cloneOptionalStringSliceFast(value.OddChipPlayerIDs)
		cloned[index].VTXORefs = cloneVTXORefsFast(value.VTXORefs)
		cloned[index].WinnerPlayerIDs = cloneOptionalStringSliceFast(value.WinnerPlayerIDs)
	}
	return cloned
}

func cloneCustodyProofFast(value tablecustody.CustodyProof) tablecustody.CustodyProof {
	cloned := value
	cloned.CandidateIntentAck = cloneCandidateIntentAckPtrFast(value.CandidateIntentAck)
	cloned.ChallengeBundle = cloneCustodyChallengeBundlePtrFast(value.ChallengeBundle)
	cloned.ChallengeWitness = cloneCustodyChallengeWitnessPtrFast(value.ChallengeWitness)
	cloned.RecoveryBundles = cloneCustodyRecoveryBundlesFast(value.RecoveryBundles)
	cloned.RecoveryWitness = cloneCustodyRecoveryWitnessPtrFast(value.RecoveryWitness)
	cloned.SettlementWitness = cloneCustodySettlementWitnessPtrFast(value.SettlementWitness)
	cloned.Signatures = cloneCustodySignaturesFast(value.Signatures)
	cloned.VTXORefs = cloneVTXORefsFast(value.VTXORefs)
	return cloned
}

func cloneCandidateIntentAckPtrFast(value *tablecustody.CandidateIntentAck) *tablecustody.CandidateIntentAck {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneCustodyChallengeBundlePtrFast(value *tablecustody.CustodyChallengeBundle) *tablecustody.CustodyChallengeBundle {
	if value == nil {
		return nil
	}
	cloned := cloneCustodyChallengeBundleFast(*value)
	return &cloned
}

func cloneCustodyChallengeBundlesFast(values []tablecustody.CustodyChallengeBundle) []tablecustody.CustodyChallengeBundle {
	if len(values) == 0 {
		return nil
	}
	cloned := make([]tablecustody.CustodyChallengeBundle, len(values))
	for index, value := range values {
		cloned[index] = cloneCustodyChallengeBundleFast(value)
	}
	return cloned
}

func cloneCustodyChallengeBundleFast(value tablecustody.CustodyChallengeBundle) tablecustody.CustodyChallengeBundle {
	cloned := value
	cloned.AuthorizedOutputs = cloneCustodyChallengeOutputsFast(value.AuthorizedOutputs)
	cloned.SourceRefs = cloneVTXORefsFast(value.SourceRefs)
	cloned.TimeoutResolution = cloneTimeoutResolutionPtrFast(value.TimeoutResolution)
	return cloned
}

func cloneCustodyChallengeOutputsFast(values []tablecustody.CustodyChallengeOutput) []tablecustody.CustodyChallengeOutput {
	if len(values) == 0 {
		return nil
	}
	cloned := make([]tablecustody.CustodyChallengeOutput, len(values))
	for index, value := range values {
		cloned[index] = value
		cloned[index].Tapscripts = cloneStringSliceFast(value.Tapscripts)
	}
	return cloned
}

func cloneCustodyChallengeWitnessPtrFast(value *tablecustody.CustodyChallengeWitness) *tablecustody.CustodyChallengeWitness {
	if value == nil {
		return nil
	}
	cloned := *value
	cloned.BroadcastTxIDs = cloneOptionalStringSliceFast(value.BroadcastTxIDs)
	return &cloned
}

func cloneCustodyRecoveryBundlesFast(values []tablecustody.CustodyRecoveryBundle) []tablecustody.CustodyRecoveryBundle {
	if len(values) == 0 {
		return nil
	}
	cloned := make([]tablecustody.CustodyRecoveryBundle, len(values))
	for index, value := range values {
		cloned[index] = cloneCustodyRecoveryBundleFast(value)
	}
	return cloned
}

func cloneCustodyRecoveryBundleFast(value tablecustody.CustodyRecoveryBundle) tablecustody.CustodyRecoveryBundle {
	cloned := value
	cloned.AuthorizedOutputs = cloneCustodyRecoveryOutputsFast(value.AuthorizedOutputs)
	cloned.SourcePotRefs = cloneVTXORefsFast(value.SourcePotRefs)
	cloned.TimeoutResolution = cloneTimeoutResolutionPtrFast(value.TimeoutResolution)
	return cloned
}

func cloneCustodyRecoveryOutputsFast(values []tablecustody.CustodyRecoveryOutput) []tablecustody.CustodyRecoveryOutput {
	if len(values) == 0 {
		return nil
	}
	cloned := make([]tablecustody.CustodyRecoveryOutput, len(values))
	for index, value := range values {
		cloned[index] = value
		cloned[index].Tapscripts = cloneStringSliceFast(value.Tapscripts)
	}
	return cloned
}

func cloneCustodyRecoveryWitnessPtrFast(value *tablecustody.CustodyRecoveryWitness) *tablecustody.CustodyRecoveryWitness {
	if value == nil {
		return nil
	}
	cloned := *value
	cloned.BroadcastTxIDs = cloneOptionalStringSliceFast(value.BroadcastTxIDs)
	return &cloned
}

func cloneCustodySettlementWitnessPtrFast(value *tablecustody.CustodySettlementWitness) *tablecustody.CustodySettlementWitness {
	if value == nil {
		return nil
	}
	cloned := *value
	cloned.VtxoTree = cloneFlatTxTreeFast(value.VtxoTree)
	cloned.ConnectorTree = cloneFlatTxTreeFast(value.ConnectorTree)
	return &cloned
}

func cloneFlatTxTreeFast(value arktree.FlatTxTree) arktree.FlatTxTree {
	if len(value) == 0 {
		return nil
	}
	cloned := make(arktree.FlatTxTree, len(value))
	for index, node := range value {
		cloned[index] = node
		cloned[index].Children = cloneUint32StringMapFast(node.Children)
	}
	return cloned
}

func cloneCustodySignaturesFast(values []tablecustody.CustodySignature) []tablecustody.CustodySignature {
	if len(values) == 0 {
		return nil
	}
	cloned := make([]tablecustody.CustodySignature, len(values))
	copy(cloned, values)
	return cloned
}

func cloneTimeoutResolutionPtrFast(value *tablecustody.TimeoutResolution) *tablecustody.TimeoutResolution {
	if value == nil {
		return nil
	}
	cloned := *value
	cloned.DeadPlayerIDs = cloneOptionalStringSliceFast(value.DeadPlayerIDs)
	cloned.LostEligibilityPlayerIDs = cloneOptionalStringSliceFast(value.LostEligibilityPlayerIDs)
	return &cloned
}

func cloneVTXORefsFast(values []tablecustody.VTXORef) []tablecustody.VTXORef {
	if len(values) == 0 {
		return nil
	}
	cloned := make([]tablecustody.VTXORef, len(values))
	for index, value := range values {
		cloned[index] = cloneVTXORefFast(value)
	}
	return cloned
}

func cloneVTXORefFast(value tablecustody.VTXORef) tablecustody.VTXORef {
	cloned := value
	cloned.Tapscripts = cloneOptionalStringSliceFast(value.Tapscripts)
	return cloned
}

func cloneAnyMapFast(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	var cloned map[string]any
	if err := json.Unmarshal(data, &cloned); err != nil {
		panic(err)
	}
	return cloned
}

func cloneStringSliceFast(values []string) []string {
	if values == nil {
		return nil
	}
	cloned := make([]string, len(values))
	copy(cloned, values)
	return cloned
}

func cloneOptionalStringSliceFast(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	return cloneStringSliceFast(values)
}

func cloneCardCodesFast(values []game.CardCode) []game.CardCode {
	if values == nil {
		return nil
	}
	cloned := make([]game.CardCode, len(values))
	copy(cloned, values)
	return cloned
}

func cloneIntSliceFast(values []int) []int {
	if values == nil {
		return nil
	}
	cloned := make([]int, len(values))
	copy(cloned, values)
	return cloned
}

func cloneStringIntMapFast(values map[string]int) map[string]int {
	if values == nil {
		return nil
	}
	cloned := make(map[string]int, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func cloneOptionalStringIntMapFast(values map[string]int) map[string]int {
	if len(values) == 0 {
		return nil
	}
	return cloneStringIntMapFast(values)
}

func cloneStringStringMapFast(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func cloneOptionalStringStringMapFast(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	return cloneStringStringMapFast(values)
}

func cloneUint32StringMapFast(values map[uint32]string) map[uint32]string {
	if values == nil {
		return nil
	}
	cloned := make(map[uint32]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func cloneIntPtrFast(value *int) *int {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}
