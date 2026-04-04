package wallet

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const maxCachedChainTipAge = 30 * time.Second

func (runtime *Runtime) ChainTip(profileName string) (ChainTipStatus, error) {
	state, err := runtime.ensureBootstrap(profileName, "", "")
	if err != nil {
		return ChainTipStatus{}, err
	}

	config, err := runtime.explorerConfig(profileName)
	if err != nil {
		return ChainTipStatus{}, err
	}

	height, liveErr := fetchExplorerTipHeight(config.ExplorerURL)
	if liveErr == nil {
		status := ChainTipStatus{
			Height:     height,
			ObservedAt: walletNowISO(),
		}
		state.CachedChainTip = &status
		_ = runtime.saveProfileState(*state)
		return status, nil
	}

	if cached, ok := freshCachedChainTip(*state); ok {
		return cached, nil
	}
	return ChainTipStatus{}, liveErr
}

func (runtime *Runtime) TransactionChainStatus(profileName, txid string) (ChainTransactionStatus, error) {
	config, err := runtime.explorerConfig(profileName)
	if err != nil {
		return ChainTransactionStatus{}, err
	}
	return fetchExplorerTxStatus(config.ExplorerURL, txid)
}

func (runtime *Runtime) explorerConfig(profileName string) (CustodyArkConfig, error) {
	state, err := runtime.ensureBootstrap(profileName, "", "")
	if err != nil {
		return CustodyArkConfig{}, err
	}
	if cached, ok := cachedArkConfig(*state); ok && strings.TrimSpace(cached.ExplorerURL) != "" {
		return cached, nil
	}

	config, err := runtime.ArkConfig(profileName)
	if err != nil {
		return CustodyArkConfig{}, err
	}
	if strings.TrimSpace(config.ExplorerURL) == "" {
		return CustodyArkConfig{}, errors.New("ark explorer url is unavailable")
	}
	return config, nil
}

func fetchExplorerTipHeight(baseURL string) (int64, error) {
	response, err := fetchExplorer(baseURL, "/blocks/tip/height")
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		return 0, err
	}
	height, err := strconv.ParseInt(strings.TrimSpace(string(body)), 10, 64)
	if err != nil {
		return 0, err
	}
	return height, nil
}

func fetchExplorerTxStatus(baseURL, txid string) (ChainTransactionStatus, error) {
	trimmedTxID := strings.TrimSpace(txid)
	if trimmedTxID == "" {
		return ChainTransactionStatus{}, errors.New("missing txid")
	}

	response, err := fetchExplorer(baseURL, "/tx/"+url.PathEscape(trimmedTxID)+"/status")
	if err != nil {
		return ChainTransactionStatus{}, err
	}
	defer response.Body.Close()

	var decoded struct {
		BlockHeight int64 `json:"block_height"`
		BlockTime   int64 `json:"block_time"`
		Confirmed   bool  `json:"confirmed"`
	}
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		return ChainTransactionStatus{}, err
	}

	return ChainTransactionStatus{
		BlockHeight: decoded.BlockHeight,
		BlockTime:   decoded.BlockTime,
		Confirmed:   decoded.Confirmed,
		ObservedAt:  walletNowISO(),
		TxID:        trimmedTxID,
	}, nil
}

func fetchExplorer(baseURL, path string) (*http.Response, error) {
	trimmedBaseURL := strings.TrimSpace(baseURL)
	if trimmedBaseURL == "" {
		return nil, errors.New("missing explorer url")
	}

	request, err := http.NewRequest(http.MethodGet, strings.TrimSuffix(trimmedBaseURL, "/")+path, nil)
	if err != nil {
		return nil, err
	}
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		defer response.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(response.Body, 256))
		return nil, fmt.Errorf("explorer %s returned %s: %s", path, response.Status, strings.TrimSpace(string(body)))
	}
	return response, nil
}

func freshCachedChainTip(state PlayerProfileState) (ChainTipStatus, bool) {
	if state.CachedChainTip == nil {
		return ChainTipStatus{}, false
	}
	cached := *state.CachedChainTip
	if strings.TrimSpace(cached.ObservedAt) == "" {
		return ChainTipStatus{}, false
	}
	observedAt, err := parseWalletTimestamp(cached.ObservedAt)
	if err != nil {
		return ChainTipStatus{}, false
	}
	if time.Since(observedAt) > maxCachedChainTipAge {
		return ChainTipStatus{}, false
	}
	return cached, true
}

func walletNowISO() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func parseWalletTimestamp(value string) (time.Time, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return time.Time{}, errors.New("empty timestamp")
	}
	if parsed, err := time.Parse(time.RFC3339Nano, trimmed); err == nil {
		return parsed, nil
	}
	return time.Parse(time.RFC3339, trimmed)
}
