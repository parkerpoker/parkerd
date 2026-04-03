package meshruntime

import (
	"fmt"
	"sort"
	"strings"

	"github.com/parkerpoker/parkerd/internal/game"
	"github.com/parkerpoker/parkerd/internal/tablecustody"
)

const (
	actionMenuDefaultHalfPotBps = 5000
	actionMenuDefaultPotBps     = 10000
)

func defaultActionMenuPolicy() ActionMenuPolicy {
	return ActionMenuPolicy{PotFractionBps: []int{actionMenuDefaultHalfPotBps, actionMenuDefaultPotBps}}
}

func normalizeActionMenuPolicy(policy ActionMenuPolicy) ActionMenuPolicy {
	normalized := ActionMenuPolicy{PotFractionBps: []int{}}
	if len(policy.PotFractionBps) == 0 {
		policy = defaultActionMenuPolicy()
	}
	seen := map[int]struct{}{}
	for _, raw := range policy.PotFractionBps {
		if raw <= 0 {
			continue
		}
		if raw > actionMenuDefaultPotBps {
			raw = actionMenuDefaultPotBps
		}
		if _, ok := seen[raw]; ok {
			continue
		}
		seen[raw] = struct{}{}
		normalized.PotFractionBps = append(normalized.PotFractionBps, raw)
	}
	if len(normalized.PotFractionBps) == 0 {
		return defaultActionMenuPolicy()
	}
	return normalized
}

func actionMenuPolicyForTable(table nativeTableState) ActionMenuPolicy {
	return normalizeActionMenuPolicy(table.Config.ActionMenuPolicy)
}

func actionMenuPolicyFromCreateInput(input map[string]any) ActionMenuPolicy {
	raw, ok := input["actionMenuPolicy"]
	if !ok || raw == nil {
		return defaultActionMenuPolicy()
	}
	policy, err := decodeJSONValue[ActionMenuPolicy](raw)
	if err != nil {
		return defaultActionMenuPolicy()
	}
	return normalizeActionMenuPolicy(policy)
}

func exactMenuLegalActions(menu *NativePendingTurnMenu) []game.LegalAction {
	if menu == nil {
		return nil
	}
	legal := make([]game.LegalAction, 0, len(menu.Options))
	for _, option := range menu.Options {
		entry := game.LegalAction{Type: option.Action.Type}
		switch option.Action.Type {
		case game.ActionBet, game.ActionRaise:
			total := option.Action.TotalSats
			entry.MinTotalSats = &total
			entry.MaxTotalSats = &total
		}
		legal = append(legal, entry)
	}
	return legal
}

func clampActionMenuTotal(total, minTotal, maxTotal int) int {
	if total < minTotal {
		return minTotal
	}
	if total > maxTotal {
		return maxTotal
	}
	return total
}

func actionMenuBucketTotal(hand game.HoldemState, seatIndex int, fractionBps int) int {
	player := hand.Players[seatIndex]
	toCall := maxInt(0, hand.CurrentBetSats-player.RoundContributionSats)
	baseTotal := player.RoundContributionSats + toCall
	potBase := hand.PotSats + toCall
	return baseTotal + (potBase*fractionBps)/10_000
}

func actionMenuOptionID(actionType game.ActionType, label string) string {
	return string(actionType) + "-" + label
}

func deriveFiniteMenuOptions(hand game.HoldemState, table nativeTableState) ([]NativeActionMenuOption, error) {
	if hand.ActingSeatIndex == nil {
		return nil, nil
	}
	actingSeatIndex := *hand.ActingSeatIndex
	legalActions := game.GetLegalActions(hand, hand.ActingSeatIndex)
	if len(legalActions) == 0 {
		return nil, nil
	}
	policy := actionMenuPolicyForTable(table)
	options := make([]NativeActionMenuOption, 0, len(legalActions)+len(policy.PotFractionBps)+2)
	for _, legal := range legalActions {
		switch legal.Type {
		case game.ActionFold, game.ActionCheck, game.ActionCall:
			options = append(options, NativeActionMenuOption{
				Action:   game.Action{Type: legal.Type},
				OptionID: string(legal.Type),
			})
		case game.ActionBet, game.ActionRaise:
			if legal.MinTotalSats == nil || legal.MaxTotalSats == nil {
				return nil, fmt.Errorf("action %s is missing its total range", legal.Type)
			}
			minTotal := *legal.MinTotalSats
			maxTotal := *legal.MaxTotalSats
			seenTotals := map[int]struct{}{}
			appendOption := func(label string, total int) {
				total = clampActionMenuTotal(total, minTotal, maxTotal)
				if _, ok := seenTotals[total]; ok {
					return
				}
				seenTotals[total] = struct{}{}
				options = append(options, NativeActionMenuOption{
					Action: game.Action{
						Type:      legal.Type,
						TotalSats: total,
					},
					OptionID: actionMenuOptionID(legal.Type, label),
				})
			}
			appendOption("min", minTotal)
			for _, fractionBps := range policy.PotFractionBps {
				appendOption(fmt.Sprintf("pot-%d", fractionBps), actionMenuBucketTotal(hand, actingSeatIndex, fractionBps))
			}
			appendOption("all-in", maxTotal)
		}
	}
	return options, nil
}

func candidateRequiresIntentAck(bundle NativeTurnCandidateBundle) bool {
	return strings.TrimSpace(bundle.SignedProofPSBT) != ""
}

func canonicalActionMenuOptions(options []NativeActionMenuOption) []NativeActionMenuOption {
	canonical := make([]NativeActionMenuOption, 0, len(options))
	for _, option := range options {
		canonical = append(canonical, NativeActionMenuOption{
			Action:   option.Action,
			OptionID: option.OptionID,
		})
	}
	return canonical
}

func actionMenuOptionsHash(options []NativeActionMenuOption) string {
	return tablecustody.HashValue(canonicalActionMenuOptions(options))
}

func actionMenuOptionsHashForTable(table nativeTableState) string {
	if table.ActiveHand == nil || table.ActiveHand.State.ActingSeatIndex == nil {
		return ""
	}
	options, err := deriveFiniteMenuOptions(table.ActiveHand.State, table)
	if err != nil {
		return ""
	}
	return actionMenuOptionsHash(options)
}

func turnAnchorPayload(table nativeTableState) map[string]any {
	payload := map[string]any{
		"actionMenuPolicy":     normalizeActionMenuPolicy(table.Config.ActionMenuPolicy),
		"decisionIndex":        0,
		"epoch":                table.CurrentEpoch,
		"handId":               "",
		"legalActionsHash":     "",
		"prevCustodyStateHash": latestCustodyStateHash(table),
		"tableId":              table.Config.TableID,
	}
	if table.ActiveHand != nil {
		payload["decisionIndex"] = custodyDecisionIndex(&table.ActiveHand.State)
		payload["handId"] = table.ActiveHand.State.HandID
		payload["legalActionsHash"] = tablecustody.HashLegalActions(game.GetLegalActions(table.ActiveHand.State, table.ActiveHand.State.ActingSeatIndex))
		payload["menuOptionsHash"] = actionMenuOptionsHashForTable(table)
		if table.ActiveHand.State.ActingSeatIndex != nil {
			payload["actingPlayerId"] = seatPlayerID(table, *table.ActiveHand.State.ActingSeatIndex)
		}
	}
	return payload
}

func turnAnchorHash(table nativeTableState) string {
	return tablecustody.HashValue(turnAnchorPayload(table))
}

func findTurnMenuOptionByID(menu *NativePendingTurnMenu, optionID string) (NativeActionMenuOption, bool) {
	if menu == nil {
		return NativeActionMenuOption{}, false
	}
	for _, option := range menu.Options {
		if option.OptionID == optionID {
			return option, true
		}
	}
	return NativeActionMenuOption{}, false
}

func findTurnMenuOptionByAction(menu *NativePendingTurnMenu, action game.Action) (NativeActionMenuOption, bool) {
	if menu == nil {
		return NativeActionMenuOption{}, false
	}
	for _, option := range menu.Options {
		if option.Action.Type != action.Type {
			continue
		}
		if option.Action.Type == game.ActionBet || option.Action.Type == game.ActionRaise {
			if option.Action.TotalSats != action.TotalSats {
				continue
			}
		}
		return option, true
	}
	return NativeActionMenuOption{}, false
}

func findTurnCandidateByHash(menu *NativePendingTurnMenu, candidateHash string) (NativeTurnCandidateBundle, bool) {
	if menu == nil {
		return NativeTurnCandidateBundle{}, false
	}
	for _, candidate := range menu.Candidates {
		if candidate.CandidateHash == candidateHash {
			return cloneJSON(candidate), true
		}
	}
	if menu.TimeoutCandidate.CandidateHash == candidateHash {
		return cloneJSON(menu.TimeoutCandidate), true
	}
	return NativeTurnCandidateBundle{}, false
}

func findTurnCandidateByOption(menu *NativePendingTurnMenu, optionID string) (NativeTurnCandidateBundle, bool) {
	if menu == nil {
		return NativeTurnCandidateBundle{}, false
	}
	for _, candidate := range menu.Candidates {
		if candidate.OptionID == optionID {
			return cloneJSON(candidate), true
		}
	}
	return NativeTurnCandidateBundle{}, false
}

func canonicalCandidateOutputs(outputs []custodyBatchOutput) []custodyBatchOutput {
	cloned := append([]custodyBatchOutput(nil), outputs...)
	sort.SliceStable(cloned, func(left, right int) bool {
		return custodyBatchOutputSemanticKey(cloned[left]) < custodyBatchOutputSemanticKey(cloned[right])
	})
	return cloned
}

func canonicalCandidateTransition(transition tablecustody.CustodyTransition) tablecustody.CustodyTransition {
	canonical := cloneJSON(transition)
	canonical.Approvals = nil
	canonical.ArkIntentID = ""
	canonical.ArkTxID = ""
	canonical.NextState.CreatedAt = ""
	canonical.NextState.StateHash = ""
	canonical.NextStateHash = ""
	canonical.Proof = tablecustody.CustodyProof{}
	canonical.ProposedAt = ""
	canonical.ProposedBy = ""
	canonical.TransitionID = ""
	canonical.TimeoutResolution = cloneTimeoutResolution(canonical.TimeoutResolution)
	sortTimeoutResolution(canonical.TimeoutResolution)
	return canonical
}

func turnCandidateHash(turnAnchorHash string, transition tablecustody.CustodyTransition, outputs []custodyBatchOutput) string {
	return tablecustody.HashValue(map[string]any{
		"authorizedOutputs": canonicalCandidateOutputs(outputs),
		"transition":        canonicalCandidateTransition(transition),
		"turnAnchorHash":    turnAnchorHash,
	})
}
