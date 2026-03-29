package mesh

import "github.com/parkerpoker/parkerd/internal/meshruntime"

type RuntimeMode string

const (
	RuntimeModePlayer  RuntimeMode = "player"
	RuntimeModeHost    RuntimeMode = "host"
	RuntimeModeWitness RuntimeMode = "witness"
	RuntimeModeIndexer RuntimeMode = "indexer"
)

type PeerAddress = meshruntime.NativePeerAddress
type RuntimeState = meshruntime.NativeMeshRuntimeState
type TableConfig = meshruntime.NativeMeshTableConfig
type TableSummary = meshruntime.NativeTableSummary
type SeatedPlayer = meshruntime.NativeSeatedPlayer
type DealerCommitment = meshruntime.NativeDealerCommitment
type PublicTableState = meshruntime.NativePublicTableState
type TableSnapshotSignature = meshruntime.NativeTableSnapshotSignature
type CooperativeTableSnapshot = meshruntime.NativeCooperativeTableSnapshot
type SignedTableEvent = meshruntime.NativeSignedTableEvent
type TableFundsOperation = meshruntime.NativeTableFundsOperation
type TableLocalView = meshruntime.NativeTableLocalView
type TableView = meshruntime.NativeMeshTableView
