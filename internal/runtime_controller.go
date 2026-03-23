package parker

import "github.com/danieldresner/arkade_fun/internal/game"

type daemonRuntime interface {
	Start() error
	Close() error
	Bootstrap(nickname string) (map[string]any, error)
	Tick()
	CurrentState() (map[string]any, error)
	QuickState() (map[string]any, error)
	WalletSummary() (any, error)
	WalletFaucet(amountSats int) (any, error)
	WalletOnboard() (any, error)
	WalletOffboard(address string, amountSats *int) (any, error)
	WalletDeposit(amountSats int) (any, error)
	WalletWithdraw(amountSats int, invoice string) (any, error)
	NetworkPeers() (any, error)
	BootstrapPeer(endpoint, alias string, roles []string) (any, error)
	CreateTable(input map[string]any) (any, error)
	AnnounceTable(tableID string) (any, error)
	JoinTable(inviteCode string, buyInSats int) (any, error)
	GetTable(tableID string) (any, error)
	SendAction(tableID string, action game.Action) (any, error)
	RotateHost(tableID string) (any, error)
	PublicTables() (any, error)
	CashOut(tableID string) (any, error)
	Renew(tableID string) (any, error)
	Exit(tableID string) (any, error)
	currentTableID() string
}

func newDaemonRuntime(profileName string, config RuntimeConfig, mode string) (daemonRuntime, error) {
	if config.TransportMode == "v2" {
		runtime, err := newTransportV2Runtime(profileName, config, mode)
		if err != nil {
			return nil, err
		}
		return runtime, nil
	}

	runtime, err := newNativeRuntime(profileName, config, mode)
	if err != nil {
		return nil, err
	}
	return legacyRuntimeAdapter{inner: runtime}, nil
}

type legacyRuntimeAdapter struct {
	inner *nativeRuntime
}

func (adapter legacyRuntimeAdapter) Start() error { return adapter.inner.Start() }

func (adapter legacyRuntimeAdapter) Close() error { return adapter.inner.Close() }

func (adapter legacyRuntimeAdapter) Bootstrap(nickname string) (map[string]any, error) {
	return adapter.inner.Bootstrap(nickname)
}

func (adapter legacyRuntimeAdapter) Tick() { adapter.inner.Tick() }

func (adapter legacyRuntimeAdapter) CurrentState() (map[string]any, error) {
	return adapter.inner.CurrentState()
}

func (adapter legacyRuntimeAdapter) QuickState() (map[string]any, error) {
	return adapter.inner.QuickState()
}

func (adapter legacyRuntimeAdapter) WalletSummary() (any, error) {
	return adapter.inner.walletSummary()
}

func (adapter legacyRuntimeAdapter) WalletFaucet(amountSats int) (any, error) {
	if err := adapter.inner.walletRuntime.Faucet(adapter.inner.profileName, amountSats); err != nil {
		return nil, err
	}
	return adapter.inner.walletSummary()
}

func (adapter legacyRuntimeAdapter) WalletOnboard() (any, error) {
	return adapter.inner.walletRuntime.Onboard(adapter.inner.profileName)
}

func (adapter legacyRuntimeAdapter) WalletOffboard(address string, amountSats *int) (any, error) {
	return adapter.inner.walletRuntime.Offboard(adapter.inner.profileName, address, amountSats)
}

func (adapter legacyRuntimeAdapter) WalletDeposit(amountSats int) (any, error) {
	return adapter.inner.walletRuntime.CreateDepositQuote(adapter.inner.profileName, amountSats)
}

func (adapter legacyRuntimeAdapter) WalletWithdraw(amountSats int, invoice string) (any, error) {
	return adapter.inner.walletRuntime.SubmitWithdrawal(adapter.inner.profileName, amountSats, invoice)
}

func (adapter legacyRuntimeAdapter) NetworkPeers() (any, error) {
	return adapter.inner.NetworkPeers()
}

func (adapter legacyRuntimeAdapter) BootstrapPeer(endpoint, alias string, roles []string) (any, error) {
	return adapter.inner.BootstrapPeer(endpoint, alias, roles)
}

func (adapter legacyRuntimeAdapter) CreateTable(input map[string]any) (any, error) {
	return adapter.inner.CreateTable(input)
}

func (adapter legacyRuntimeAdapter) AnnounceTable(tableID string) (any, error) {
	return adapter.inner.AnnounceTable(tableID)
}

func (adapter legacyRuntimeAdapter) JoinTable(inviteCode string, buyInSats int) (any, error) {
	return adapter.inner.JoinTable(inviteCode, buyInSats)
}

func (adapter legacyRuntimeAdapter) GetTable(tableID string) (any, error) {
	return adapter.inner.GetTable(tableID)
}

func (adapter legacyRuntimeAdapter) SendAction(tableID string, action game.Action) (any, error) {
	return adapter.inner.SendAction(tableID, action)
}

func (adapter legacyRuntimeAdapter) RotateHost(tableID string) (any, error) {
	return adapter.inner.RotateHost(tableID)
}

func (adapter legacyRuntimeAdapter) PublicTables() (any, error) {
	return adapter.inner.PublicTables()
}

func (adapter legacyRuntimeAdapter) CashOut(tableID string) (any, error) {
	return adapter.inner.CashOut(tableID)
}

func (adapter legacyRuntimeAdapter) Renew(tableID string) (any, error) {
	return adapter.inner.Renew(tableID)
}

func (adapter legacyRuntimeAdapter) Exit(tableID string) (any, error) {
	return adapter.inner.Exit(tableID)
}

func (adapter legacyRuntimeAdapter) currentTableID() string {
	return adapter.inner.currentTableID()
}
