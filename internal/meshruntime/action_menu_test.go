package meshruntime

import (
	"reflect"
	"strings"
	"testing"

	"github.com/parkerpoker/parkerd/internal/game"
	"github.com/parkerpoker/parkerd/internal/settlementcore"
	"github.com/parkerpoker/parkerd/internal/tablecustody"
)

func comparableMenuOptions(options []NativeActionMenuOption) []NativeActionMenuOption {
	comparable := append([]NativeActionMenuOption(nil), options...)
	for index := range comparable {
		comparable[index].CandidateHash = ""
	}
	return comparable
}

func TestDeriveFiniteMenuOptionsDeterministicAcrossPeers(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)

	hostTable := waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)
	guestTable := mustReadNativeTable(t, guest, tableID)

	hostOptions, err := deriveFiniteMenuOptions(hostTable.ActiveHand.State, hostTable)
	if err != nil {
		t.Fatalf("derive host menu options: %v", err)
	}
	guestOptions, err := deriveFiniteMenuOptions(guestTable.ActiveHand.State, guestTable)
	if err != nil {
		t.Fatalf("derive guest menu options: %v", err)
	}
	if !reflect.DeepEqual(hostOptions, guestOptions) {
		t.Fatalf("expected peers to derive the same exact menu options, host=%+v guest=%+v", hostOptions, guestOptions)
	}
	if !turnMenuMatchesTable(hostTable, hostTable.PendingTurnMenu) {
		t.Fatal("expected host pending turn menu to match the current turn")
	}
	if !reflect.DeepEqual(comparableMenuOptions(hostTable.PendingTurnMenu.Options), hostOptions) {
		t.Fatalf("expected persisted host menu options to match derived options, stored=%+v derived=%+v", hostTable.PendingTurnMenu.Options, hostOptions)
	}
}

func TestAcceptRemoteTableRejectsTamperedPendingTurnMenuOptions(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	table := waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)

	if !turnMenuMatchesTable(table, table.PendingTurnMenu) {
		t.Fatal("expected actionable turn menu")
	}

	tampered := cloneJSON(table)
	if len(tampered.PendingTurnMenu.Options) == 0 {
		t.Fatal("expected pending turn menu options")
	}
	tampered.PendingTurnMenu.Options = tampered.PendingTurnMenu.Options[1:]

	if err := guest.acceptRemoteTable(tampered); err == nil || !strings.Contains(err.Error(), "pending turn menu options do not match") {
		t.Fatalf("expected tampered pending turn menu to be rejected, got %v", err)
	}
}

func TestDeriveFiniteMenuOptionsClampsAndDedupesBuckets(t *testing.T) {
	actingSeat := 0
	table := nativeTableState{
		Config: NativeMeshTableConfig{
			ActionMenuPolicy: ActionMenuPolicy{PotFractionBps: []int{5000, 10000}},
		},
	}
	hand := game.HoldemState{
		Phase:           game.StreetFlop,
		ActingSeatIndex: &actingSeat,
		BigBlindSats:    100,
		CurrentBetSats:  100,
		MinRaiseToSats:  175,
		PotSats:         100,
		Players: []game.HoldemPlayerState{
			{
				PlayerID:              "alice",
				SeatIndex:             0,
				StackSats:             150,
				Status:                game.PlayerStatusActive,
				RoundContributionSats: 50,
				TotalContributionSats: 50,
			},
			{
				PlayerID:              "bob",
				SeatIndex:             1,
				StackSats:             500,
				Status:                game.PlayerStatusActive,
				RoundContributionSats: 100,
				TotalContributionSats: 100,
			},
		},
	}

	options, err := deriveFiniteMenuOptions(hand, table)
	if err != nil {
		t.Fatalf("derive clamped finite menu options: %v", err)
	}

	want := []NativeActionMenuOption{
		{Action: game.Action{Type: game.ActionFold}, OptionID: "fold"},
		{Action: game.Action{Type: game.ActionCall}, OptionID: "call"},
		{Action: game.Action{Type: game.ActionRaise, TotalSats: 175}, OptionID: "raise-min"},
		{Action: game.Action{Type: game.ActionRaise, TotalSats: 200}, OptionID: "raise-pot-10000"},
	}
	if !reflect.DeepEqual(options, want) {
		t.Fatalf("expected clamped and deduped finite menu options %+v, got %+v", want, options)
	}
}

func TestDeriveTimeoutCustodyTransitionRejectsTimelySelectedCandidate(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTableInSyntheticRealMode(t, host, guest)
	table := waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)

	if !turnMenuMatchesTable(table, table.PendingTurnMenu) {
		t.Fatal("expected a pending turn menu for the actionable turn")
	}
	bundle, ok := findTurnCandidateByOption(table.PendingTurnMenu, "call")
	if !ok {
		t.Fatalf("expected call candidate in pending turn menu, got %+v", table.PendingTurnMenu.Options)
	}
	if !candidateRequiresIntentAck(bundle) {
		t.Fatal("expected call candidate to carry a signed proof bundle")
	}
	ack, err := host.buildCandidateIntentAck(table, bundle, "intent-test")
	if err != nil {
		t.Fatalf("build candidate intent ack: %v", err)
	}

	menu := cloneJSON(*table.PendingTurnMenu)
	menu.SelectedCandidateHash = bundle.CandidateHash
	menu.AcceptedIntentAck = ack
	table.PendingTurnMenu = &menu

	if _, err := host.deriveTimeoutCustodyTransition(table); err == nil || !strings.Contains(err.Error(), "stale after a valid selected turn candidate") {
		t.Fatalf("expected timely selected candidate to block timeout transition, got %v", err)
	}
}

func TestPendingTurnMenuDeadlineStartsAtDelivery(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	table := waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)

	if !turnMenuMatchesTable(table, table.PendingTurnMenu) {
		t.Fatal("expected actionable turn menu")
	}
	menu := table.PendingTurnMenu
	expectedDeadline := host.turnMenuActionDeadlineAt(table, menu.DeliveredAt)
	if menu.ActionDeadlineAt != expectedDeadline {
		t.Fatalf("expected turn deadline %q from delivery %q, got %q", expectedDeadline, menu.DeliveredAt, menu.ActionDeadlineAt)
	}

	tampered := cloneJSON(*menu)
	tampered.DeliveredAt = addMillis(menu.DeliveredAt, 1_000)
	if err := host.validatePendingTurnMenu(table, &tampered); err == nil || !strings.Contains(err.Error(), "action deadline does not start at menu delivery") {
		t.Fatalf("expected delivery/deadline mismatch to be rejected, got %v", err)
	}
}

func TestBuildPendingTurnMenuSetsDeliveryAfterBuildCompletion(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)

	if err := host.store.withTableLock(tableID, func() error {
		table, err := host.store.readTable(tableID)
		if err != nil || table == nil {
			return err
		}
		table.PendingTurnMenu = nil
		if err := host.ensurePendingTurnMenuLocked(table); err != nil {
			return err
		}
		return host.persistLocalTable(table, false)
	}); err != nil {
		t.Fatalf("rebuild pending turn menu: %v", err)
	}
	table := mustReadNativeTable(t, host, tableID)
	menu := table.PendingTurnMenu
	if !turnMenuMatchesTable(table, menu) {
		t.Fatal("expected rebuilt pending turn menu")
	}
	if elapsedMillis(menu.DeliveredAt) >= 0 {
		t.Fatalf("expected deliveredAt %q to still be in the future when build returns", menu.DeliveredAt)
	}
	if err := host.validatePendingTurnMenu(table, menu); err != nil {
		t.Fatalf("expected rebuilt menu to validate, got %v", err)
	}
}

func TestPendingTurnMenuAllowsUnsignedSelectedCandidateButNotTimelyProof(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTableInSyntheticRealMode(t, host, guest)
	table := waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)

	if !turnMenuMatchesTable(table, table.PendingTurnMenu) {
		t.Fatal("expected a pending turn menu for the actionable turn")
	}
	bundle, ok := findTurnCandidateByOption(table.PendingTurnMenu, "call")
	if !ok {
		t.Fatalf("expected call candidate in pending turn menu, got %+v", table.PendingTurnMenu.Options)
	}
	if !candidateRequiresIntentAck(bundle) {
		t.Fatal("expected call candidate to carry a signed proof bundle")
	}

	menu := cloneJSON(*table.PendingTurnMenu)
	menu.SelectedCandidateHash = bundle.CandidateHash
	menu.AcceptedIntentAck = nil
	table.PendingTurnMenu = &menu

	if err := host.validatePendingTurnMenu(table, table.PendingTurnMenu); err != nil {
		t.Fatalf("expected selected candidate without ack to remain a valid pending menu state, got %v", err)
	}
	if host.hasTimelySelectedCandidate(table) {
		t.Fatal("expected selected candidate without operator-signed ack to fail timely-proof validation")
	}
}

func TestVerifyCandidateIntentAckRejectsTampering(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTableInSyntheticRealMode(t, host, guest)
	table := waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)

	if !turnMenuMatchesTable(table, table.PendingTurnMenu) {
		t.Fatal("expected a pending turn menu for the actionable turn")
	}
	bundle, ok := findTurnCandidateByOption(table.PendingTurnMenu, "call")
	if !ok {
		t.Fatalf("expected call candidate in pending turn menu, got %+v", table.PendingTurnMenu.Options)
	}
	ack, err := host.buildCandidateIntentAck(table, bundle, "intent-test")
	if err != nil {
		t.Fatalf("build candidate intent ack: %v", err)
	}
	if err := host.verifyCandidateIntentAck(table, bundle, *ack); err != nil {
		t.Fatalf("expected untampered ack to verify, got %v", err)
	}

	unexpectedKey := cloneJSON(*ack)
	unexpectedKey.OperatorPubkeyHex = host.protocolIdentity.PublicKeyHex
	signatureHex, err := settlementcore.SignStructuredData(host.protocolIdentity.PrivateKeyHex, candidateIntentAckPayload(unexpectedKey))
	if err != nil {
		t.Fatalf("sign candidate intent ack: %v", err)
	}
	unexpectedKey.OperatorSignatureHex = signatureHex
	if err := host.verifyCandidateIntentAck(table, bundle, unexpectedKey); err == nil || !strings.Contains(err.Error(), "operator key mismatch") {
		t.Fatalf("expected non-signer operator key to fail verification, got %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*tablecustody.CandidateIntentAck)
		want   string
	}{
		{
			name: "candidate-hash",
			mutate: func(ack *tablecustody.CandidateIntentAck) {
				ack.CandidateHash = strings.Repeat("a", 64)
			},
			want: "candidate mismatch",
		},
		{
			name: "turn-anchor",
			mutate: func(ack *tablecustody.CandidateIntentAck) {
				ack.TurnAnchorHash = strings.Repeat("b", 64)
			},
			want: "turn anchor mismatch",
		},
		{
			name: "signature",
			mutate: func(ack *tablecustody.CandidateIntentAck) {
				ack.AcceptedAt = addMillis(ack.AcceptedAt, 1)
			},
			want: "signature",
		},
		{
			name: "missing-signature",
			mutate: func(ack *tablecustody.CandidateIntentAck) {
				ack.OperatorSignatureHex = ""
			},
			want: "signature is missing",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tampered := cloneJSON(*ack)
			tc.mutate(&tampered)
			if err := host.verifyCandidateIntentAck(table, bundle, tampered); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected tampered ack to fail with %q, got %v", tc.want, err)
			}
		})
	}
}
