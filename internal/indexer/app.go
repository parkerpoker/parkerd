package indexer

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	cfg "github.com/parkerpoker/parkerd/internal/config"
	storepkg "github.com/parkerpoker/parkerd/internal/storage"
)

type Stakes struct {
	BigBlindSats   int `json:"bigBlindSats"`
	SmallBlindSats int `json:"smallBlindSats"`
}

type SignedTableAdvertisement struct {
	AdExpiresAt           string   `json:"adExpiresAt"`
	BuyInMaxSats          int      `json:"buyInMaxSats"`
	BuyInMinSats          int      `json:"buyInMinSats"`
	Currency              string   `json:"currency"`
	GeographicHint        string   `json:"geographicHint,omitempty"`
	HostModeCapabilities  []string `json:"hostModeCapabilities"`
	HostPeerID            string   `json:"hostPeerId"`
	HostPeerURL           string   `json:"hostPeerUrl,omitempty"`
	HostProtocolPubkeyHex string   `json:"hostProtocolPubkeyHex"`
	HostSignatureHex      string   `json:"hostSignatureHex"`
	LatencyHintMS         int      `json:"latencyHintMs,omitempty"`
	NetworkID             string   `json:"networkId"`
	OccupiedSeats         int      `json:"occupiedSeats"`
	ProtocolVersion       string   `json:"protocolVersion"`
	SeatCount             int      `json:"seatCount"`
	SpectatorsAllowed     bool     `json:"spectatorsAllowed"`
	Stakes                Stakes   `json:"stakes"`
	TableID               string   `json:"tableId"`
	TableName             string   `json:"tableName"`
	Visibility            string   `json:"visibility"`
	WitnessCount          int      `json:"witnessCount"`
}

type DealerCommitment struct {
	CommittedAt string `json:"committedAt"`
	Mode        string `json:"mode"`
	RootHash    string `json:"rootHash"`
}

type SeatedPlayer struct {
	ArkAddress        string `json:"arkAddress"`
	BuyInSats         int    `json:"buyInSats"`
	Nickname          string `json:"nickname"`
	PeerID            string `json:"peerId"`
	PlayerID          string `json:"playerId"`
	ProtocolPubkeyHex string `json:"protocolPubkeyHex"`
	SeatIndex         int    `json:"seatIndex"`
	Status            string `json:"status"`
	WalletPubkeyHex   string `json:"walletPubkeyHex"`
}

type PublicTableState struct {
	ActingSeatIndex      any               `json:"actingSeatIndex"`
	Board                []string          `json:"board"`
	ChipBalances         map[string]int    `json:"chipBalances"`
	CurrentBetSats       int               `json:"currentBetSats"`
	DealerCommitment     *DealerCommitment `json:"dealerCommitment"`
	DealerSeatIndex      any               `json:"dealerSeatIndex"`
	Epoch                int               `json:"epoch"`
	FoldedPlayerIDs      []string          `json:"foldedPlayerIds"`
	HandID               any               `json:"handId"`
	HandNumber           int               `json:"handNumber"`
	LatestEventHash      any               `json:"latestEventHash"`
	LivePlayerIDs        []string          `json:"livePlayerIds"`
	MinRaiseToSats       int               `json:"minRaiseToSats"`
	Phase                any               `json:"phase"`
	PotSats              int               `json:"potSats"`
	PreviousSnapshotHash any               `json:"previousSnapshotHash"`
	RoundContributions   map[string]int    `json:"roundContributions"`
	SeatedPlayers        []SeatedPlayer    `json:"seatedPlayers"`
	SnapshotID           string            `json:"snapshotId"`
	Status               string            `json:"status"`
	TableID              string            `json:"tableId"`
	TotalContributions   map[string]int    `json:"totalContributions"`
	UpdatedAt            string            `json:"updatedAt"`
}

type PublicTableUpdate struct {
	Advertisement *SignedTableAdvertisement `json:"advertisement,omitempty"`
	PublishedAt   string                    `json:"publishedAt,omitempty"`
	PublicState   *PublicTableState         `json:"publicState,omitempty"`
	TableID       string                    `json:"tableId"`
	Type          string                    `json:"type"`
}

type PublicTableView struct {
	Advertisement SignedTableAdvertisement `json:"advertisement"`
	LatestState   *PublicTableState        `json:"latestState"`
	RecentUpdates []PublicTableUpdate      `json:"recentUpdates"`
}

type App struct {
	mux        *http.ServeMux
	repository *storepkg.IndexerRepository
}

func NewApp(runtimeConfig cfg.RuntimeConfig) (*App, error) {
	repository, err := storepkg.OpenIndexerRepository(runtimeConfig)
	if err != nil {
		return nil, err
	}
	app := &App{
		mux:        http.NewServeMux(),
		repository: repository,
	}
	app.registerRoutes()
	return app, nil
}

func (app *App) Close() error {
	return app.repository.Close()
}

func (app *App) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	app.mux.ServeHTTP(writer, request)
}

func (app *App) registerRoutes() {
	app.mux.HandleFunc("GET /health", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, map[string]bool{"ok": true})
	})

	app.mux.HandleFunc("POST /api/indexer/table-ads", func(writer http.ResponseWriter, request *http.Request) {
		raw, err := io.ReadAll(request.Body)
		if err != nil {
			writeJSON(writer, http.StatusBadRequest, map[string]string{"message": err.Error()})
			return
		}
		var advertisement SignedTableAdvertisement
		if err := json.Unmarshal(raw, &advertisement); err != nil {
			writeJSON(writer, http.StatusBadRequest, map[string]string{"message": err.Error()})
			return
		}
		if err := validateAdvertisement(advertisement); err != nil {
			writeJSON(writer, http.StatusBadRequest, map[string]string{"message": err.Error()})
			return
		}
		if err := app.repository.SavePublicTableAd(advertisement.TableID, raw); err != nil {
			writeJSON(writer, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		writeJSON(writer, http.StatusOK, map[string]bool{"ok": true})
	})

	app.mux.HandleFunc("POST /api/indexer/table-updates", func(writer http.ResponseWriter, request *http.Request) {
		raw, err := io.ReadAll(request.Body)
		if err != nil {
			writeJSON(writer, http.StatusBadRequest, map[string]string{"message": err.Error()})
			return
		}
		var update PublicTableUpdate
		if err := json.Unmarshal(raw, &update); err != nil {
			writeJSON(writer, http.StatusBadRequest, map[string]string{"message": err.Error()})
			return
		}
		if err := validateUpdate(update); err != nil {
			writeJSON(writer, http.StatusBadRequest, map[string]string{"message": err.Error()})
			return
		}
		var latestState []byte
		if update.PublicState != nil {
			latestState, err = json.Marshal(update.PublicState)
			if err != nil {
				writeJSON(writer, http.StatusInternalServerError, map[string]string{"message": err.Error()})
				return
			}
		}
		if err := app.repository.SavePublicUpdate(update.TableID, raw, latestState); err != nil {
			writeJSON(writer, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		writeJSON(writer, http.StatusOK, map[string]bool{"ok": true})
	})

	app.mux.HandleFunc("GET /api/public/tables", func(writer http.ResponseWriter, _ *http.Request) {
		views, err := app.repository.ListPublicTables()
		if err != nil {
			writeJSON(writer, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		decoded, err := decodeViews(views)
		if err != nil {
			writeJSON(writer, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		writeJSON(writer, http.StatusOK, decoded)
	})

	app.mux.HandleFunc("GET /api/public/tables/{tableId}", func(writer http.ResponseWriter, request *http.Request) {
		view, err := app.repository.LoadPublicTable(request.PathValue("tableId"))
		if err != nil {
			writeJSON(writer, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		if view == nil {
			writeJSON(writer, http.StatusNotFound, map[string]string{"message": "public table not found"})
			return
		}
		decoded, err := decodeView(*view)
		if err != nil {
			writeJSON(writer, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		writeJSON(writer, http.StatusOK, decoded)
	})
}

func decodeViews(rawViews []storepkg.RawPublicTableView) ([]PublicTableView, error) {
	views := make([]PublicTableView, 0, len(rawViews))
	for _, rawView := range rawViews {
		view, err := decodeView(rawView)
		if err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	return views, nil
}

func decodeView(rawView storepkg.RawPublicTableView) (PublicTableView, error) {
	var advertisement SignedTableAdvertisement
	if err := json.Unmarshal(rawView.Advertisement, &advertisement); err != nil {
		return PublicTableView{}, err
	}

	var latestState *PublicTableState
	if len(rawView.LatestState) > 0 {
		var decoded PublicTableState
		if err := json.Unmarshal(rawView.LatestState, &decoded); err != nil {
			return PublicTableView{}, err
		}
		latestState = &decoded
	}

	updates := make([]PublicTableUpdate, 0, len(rawView.RecentUpdates))
	for _, rawUpdate := range rawView.RecentUpdates {
		var update PublicTableUpdate
		if err := json.Unmarshal(rawUpdate, &update); err != nil {
			return PublicTableView{}, err
		}
		updates = append(updates, update)
	}

	return PublicTableView{
		Advertisement: advertisement,
		LatestState:   latestState,
		RecentUpdates: updates,
	}, nil
}

func validateAdvertisement(advertisement SignedTableAdvertisement) error {
	switch {
	case strings.TrimSpace(advertisement.TableID) == "":
		return fmt.Errorf("tableId is required")
	case strings.TrimSpace(advertisement.ProtocolVersion) == "":
		return fmt.Errorf("protocolVersion is required")
	case strings.TrimSpace(advertisement.NetworkID) == "":
		return fmt.Errorf("networkId is required")
	case strings.TrimSpace(advertisement.HostPeerID) == "":
		return fmt.Errorf("hostPeerId is required")
	case strings.TrimSpace(advertisement.TableName) == "":
		return fmt.Errorf("tableName is required")
	default:
		return nil
	}
}

func validateUpdate(update PublicTableUpdate) error {
	if strings.TrimSpace(update.Type) == "" {
		return fmt.Errorf("type is required")
	}
	if strings.TrimSpace(update.TableID) == "" {
		return fmt.Errorf("tableId is required")
	}
	return nil
}

func writeJSON(writer http.ResponseWriter, statusCode int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(statusCode)
	encoder := json.NewEncoder(writer)
	_ = encoder.Encode(value)
}
