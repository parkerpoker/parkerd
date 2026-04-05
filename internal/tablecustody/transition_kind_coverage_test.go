package tablecustody

import "testing"

func TestTransitionKindCoverageMatrix(t *testing.T) {
	t.Parallel()

	coverage := map[TransitionKind][]string{
		TransitionKindBuyInLock: {
			"TestJoinTableSyncsExistingSeatsBeforeRemoteSignerPrepare",
			"TestRegtestRoundUsesRealArkCustodyStandardScenario",
		},
		TransitionKindBlindPost: {
			"TestRecoveryBundlesOnlyAttachOnDeterministicMoneyResolvingStates",
			"TestRegtestRoundUsesRealArkCustodyStandardScenario",
		},
		TransitionKindAction: {
			"TestHoldemHandTransitions",
			"TestRegtestRoundUsesRealArkCustodyStandardScenario",
			"TestRegtestRoundUsesRealArkCustodyTurnChallengeScenario",
		},
		TransitionKindTimeout: {
			"TestActionTimeoutRecoveryMatchesCooperativeSuccessor",
			"TestRegtestRoundUsesRealArkCustodyRecoveryTimeoutScenario",
			"TestRegtestRoundUsesRealArkCustodyCashoutAfterChallengeScenario",
		},
		TransitionKindTurnChallengeOpen: {
			"TestTurnChallengeOpenBlocksOrdinarySendAction",
			"TestRegtestRoundUsesRealArkCustodyTurnChallengeScenario",
		},
		TransitionKindTurnChallengeEscape: {
			"TestTurnChallengeEscapeRestoresSplitAndAbortsHand",
			"TestRegtestRoundUsesRealArkCustodyChallengeEscapeScenario",
		},
		TransitionKindShowdownPayout: {
			"TestShowdownRevealRecoveryMatchesCooperativePayoutSuccessor",
			"TestRegtestRoundUsesRealArkCustodyStandardScenario",
			"TestRegtestRoundUsesRealArkCustodyRecoveryShowdownScenario",
		},
		TransitionKindCashOut: {
			"TestRegtestRoundUsesRealArkCustodyStandardScenario",
			"TestRegtestRoundUsesRealArkCustodyCashoutAfterChallengeScenario",
		},
		TransitionKindEmergencyExit: {
			"TestEmergencyExitAppendsCustodyTransition",
			"TestRegtestRoundUsesRealArkCustodyEmergencyExitScenario",
		},
		TransitionKindCarryForward: {
			"TestRegtestRoundUsesRealArkCustodyMultiHandScenario",
		},
	}

	for _, kind := range AllTransitionKinds() {
		entries := coverage[kind]
		if len(entries) == 0 {
			t.Fatalf("missing coverage matrix entry for transition kind %q", kind)
		}
	}
	if len(coverage) != len(AllTransitionKinds()) {
		t.Fatalf("coverage matrix has %d entries for %d transition kinds", len(coverage), len(AllTransitionKinds()))
	}
}
