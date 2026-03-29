package mesh

import parker "github.com/parkerpoker/parkerd/internal"

type RuntimeMode string

const (
	RuntimeModePlayer  RuntimeMode = "player"
	RuntimeModeHost    RuntimeMode = "host"
	RuntimeModeWitness RuntimeMode = "witness"
	RuntimeModeIndexer RuntimeMode = "indexer"
)

type PeerAddress = parker.NativePeerAddress
type RuntimeState = parker.NativeMeshRuntimeState
type TableConfig = parker.NativeMeshTableConfig
type TableSummary = parker.NativeTableSummary
type SeatedPlayer = parker.NativeSeatedPlayer
type DealerCommitment = parker.NativeDealerCommitment
type PublicTableState = parker.NativePublicTableState
type TableSnapshotSignature = parker.NativeTableSnapshotSignature
type CooperativeTableSnapshot = parker.NativeCooperativeTableSnapshot
type SignedTableEvent = parker.NativeSignedTableEvent
type TableFundsOperation = parker.NativeTableFundsOperation
type TableLocalView = parker.NativeTableLocalView
type TableView = parker.NativeMeshTableView
