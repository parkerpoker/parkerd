package meshruntime

import (
	"testing"

	"github.com/parkerpoker/parkerd/internal/game"
)

func TestAcceptedCustodyHistoryReplaysHistoricalActionRefsWithoutCurrentActiveHand(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTableInSyntheticRealMode(t, host, guest)

	waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)
	if _, err := host.SendAction(tableID, game.Action{Type: game.ActionCall}); err != nil {
		t.Fatalf("host send preflop call: %v", err)
	}
	waitForLocalCanAct(t, []*meshRuntime{host, guest}, guest, tableID)
	if _, err := guest.SendAction(tableID, aggressiveActionForTable(t, mustReadNativeTable(t, guest, tableID))); err != nil {
		t.Fatalf("guest send preflop bet: %v", err)
	}
	waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)
	if _, err := host.SendAction(tableID, passiveActionForTable(t, mustReadNativeTable(t, host, tableID))); err != nil {
		t.Fatalf("host send preflop call to showdown line: %v", err)
	}
	for _, actor := range []*meshRuntime{guest, host, guest, host, guest, host} {
		waitForLocalCanAct(t, []*meshRuntime{host, guest}, actor, tableID)
		if _, err := actor.SendAction(tableID, game.Action{Type: game.ActionCheck}); err != nil {
			t.Fatalf("send showdown-line check: %v", err)
		}
	}
	waitForHandPhase(t, []*meshRuntime{host, guest}, host, tableID, game.StreetSettled)
	waitForHandPhase(t, []*meshRuntime{host, guest}, guest, tableID, game.StreetSettled)
	waitForCustodySync(t, []*meshRuntime{host, guest}, host, guest, tableID)

	table := mustReadNativeTable(t, host, tableID)
	if err := guest.validateAcceptedCustodyHistory(nil, table); err != nil {
		t.Fatalf("validate accepted custody history after settled hand: %v", err)
	}

	cashoutShaped := cloneJSON(table)
	cashoutShaped.ActiveHand = nil
	cashoutShaped.ActiveHandStartAt = ""
	if err := guest.validateAcceptedCustodyHistory(nil, cashoutShaped); err != nil {
		t.Fatalf("validate accepted custody history after clearing active hand: %v", err)
	}
}
