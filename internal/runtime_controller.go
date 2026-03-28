package parker

import "github.com/danieldresner/arkade_fun/internal/game"

type daemonRuntime interface {
	Start() error
	Close() error
	Bootstrap(nickname, walletNsec string) (map[string]any, error)
	Tick()
	CurrentState() (map[string]any, error)
	QuickState() (map[string]any, error)
	WalletNsec() (any, error)
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
	return newDaemonRuntimeAdapter(profileName, config, mode)
}
