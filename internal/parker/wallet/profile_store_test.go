package wallet

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestProfileStoreSaveConcurrent(t *testing.T) {
	t.Parallel()

	store := NewProfileStore(t.TempDir())

	const writers = 16
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- store.Save(PlayerProfileState{
				ProfileName:           "alice",
				Nickname:              fmt.Sprintf("Alice-%d", i),
				PrivateKeyHex:         fmt.Sprintf("priv-%d", i),
				PeerPrivateKeyHex:     fmt.Sprintf("peer-%d", i),
				ProtocolPrivateKeyHex: fmt.Sprintf("protocol-%d", i),
				WalletPrivateKeyHex:   fmt.Sprintf("wallet-%d", i),
				HandSeeds:             map[string]string{},
			})
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("expected concurrent saves to succeed: %v", err)
		}
	}

	state, err := store.Load("alice")
	if err != nil {
		t.Fatalf("expected saved profile to load: %v", err)
	}
	if state == nil {
		t.Fatal("expected saved profile state")
	}
	if !strings.HasPrefix(state.Nickname, "Alice-") {
		t.Fatalf("expected a concurrent writer result, received %q", state.Nickname)
	}
}
