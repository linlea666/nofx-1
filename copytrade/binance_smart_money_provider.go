package copytrade

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	BinanceSmartMoneyProfileAPI         = "https://www.binance.com/bapi/asset/v1/friendly/future/smart-money/profile"
	BinanceSmartMoneyPositionsAPI       = "https://www.binance.com/bapi/asset/v1/private/future/smart-money/profile/query-positions"
	BinanceSmartMoneyPositionHistoryAPI = "https://www.binance.com/bapi/asset/v2/private/future/smart-money/profile/query-um-position-history"
	BinanceSmartMoneyOrderHistoryAPI    = "https://www.binance.com/bapi/asset/v1/private/future/smart-money/profile/query-order-history"
	BinanceUSDExchangeInfoAPI           = "https://fapi.binance.com/fapi/v1/exchangeInfo"
	BinanceSpotPriceAPI                 = "https://api.binance.com/api/v3/ticker/price"

	smartMoneyRows       = 9
	smartMoneyMaxPages   = 100
	smartMoneyProfileTTL = 15 * time.Second
	smartMoneyCatalogTTL = 5 * time.Minute
	smartMoneyFXTTL      = 2 * time.Minute
	// A sudden all-empty response is held long enough for two normal polling
	// rounds and is paired with an uncached profile check. This prevents the
	// visibility endpoint and positions endpoint from racing into a false
	// full-close signal while keeping genuine full closes reasonably prompt.
	smartMoneyEmptyConfirmDelay = 6 * time.Second
)

var (
	ErrBinanceSmartMoneyPrivate          = errors.New("binance smart money leader no longer shares current positions")
	ErrBinanceSmartMoneyDisabled         = errors.New("binance smart money leader profile is disabled")
	ErrBinanceSmartMoneyEmptyUnconfirmed = errors.New("binance smart money empty position snapshot is awaiting visibility confirmation")
	ErrBinanceSmartMoneyHistoryPrivate   = errors.New("binance smart money leader no longer shares position history")
	ErrBinanceSmartMoneyLatestPrivate    = errors.New("binance smart money leader no longer shares latest operations")
)

type SourceHealthObservation struct {
	Status           string
	TraderName       string
	Error            string
	CompleteSnapshot bool
	CheckedAt        time.Time
}

type SourceHealthObservationProvider interface {
	LastSourceHealthObservation() SourceHealthObservation
}

type BinanceSmartMoneyProvider struct {
	client *http.Client
	auth   *BinanceProvider

	mu                        sync.Mutex
	profile                   *binanceSmartProfile
	profileTopTraderID        string
	profileFetchedAt          time.Time
	lastObservation           SourceHealthObservation
	catalog                   map[string]InstrumentRef
	catalogFetchedAt          time.Time
	fxRates                   map[string]cachedSmartFXRate
	lastCompletePositionCount int
	emptySnapshotCandidateAt  time.Time
}

type cachedSmartFXRate struct {
	Rate      float64
	FetchedAt time.Time
}

func NewBinanceSmartMoneyProvider(p20t, csrf string) *BinanceSmartMoneyProvider {
	return &BinanceSmartMoneyProvider{
		client:  &http.Client{Timeout: 15 * time.Second},
		auth:    NewBinanceProvider(p20t, csrf),
		catalog: make(map[string]InstrumentRef),
		fxRates: make(map[string]cachedSmartFXRate),
	}
}

func NewBinanceSmartMoneyProviderWithLoader(loader BinanceCredentialsLoader, label, fallbackP20T, fallbackCSRF string) *BinanceSmartMoneyProvider {
	return &BinanceSmartMoneyProvider{
		client:  &http.Client{Timeout: 15 * time.Second},
		auth:    NewBinanceProviderWithLoader(loader, label, fallbackP20T, fallbackCSRF),
		catalog: make(map[string]InstrumentRef),
		fxRates: make(map[string]cachedSmartFXRate),
	}
}

func (p *BinanceSmartMoneyProvider) Type() ProviderType { return ProviderBinance }

// GetFills deliberately returns no order-history events. Smart Money's latest
// operation feed is delayed; Engine's existing Binance snapshot reconciler is
// the single authoritative signal path.
func (p *BinanceSmartMoneyProvider) GetFills(_ string, _ time.Time) ([]Fill, error) {
	return nil, nil
}

// BinanceSmartMoneyPositionHistoryRecord is a normalized, read-only history
// record. Raw is retained for diagnostics when Binance adds fields; it must not
// be used as an execution signal.
type BinanceSmartMoneyPositionHistoryRecord struct {
	ID            string
	Symbol        string
	Side          string
	PositionSide  string
	Status        string
	Isolated      bool
	Leverage      float64
	Amount        float64
	EntryPrice    float64
	AvgCost       float64
	AvgClosePrice float64
	MarkPrice     float64
	PnL           float64
	ROE           float64
	OpenedAt      int64
	ClosedAt      int64
	UpdatedAt     int64
	Raw           json.RawMessage
}

// BinanceSmartMoneyOrderHistoryRecord is a normalized, read-only latest
// operation record. Binance publishes this feed with delay, so GetFills never
// consumes it and trading continues to reconcile complete position snapshots.
type BinanceSmartMoneyOrderHistoryRecord struct {
	ID               string
	OrderID          string
	TradeID          string
	Symbol           string
	Side             string
	PositionSide     string
	Operation        string
	OrderType        string
	Status           string
	Price            float64
	AveragePrice     float64
	Quantity         float64
	ExecutedQuantity float64
	RealizedPnL      float64
	CreatedAt        int64
	UpdatedAt        int64
	Raw              json.RawMessage
}

// GetSmartMoneyPositionHistory retrieves the complete UM position-history
// diagnostic range. It is intentionally not named GetPositionHistory so this
// provider cannot accidentally satisfy the legacy execution-signal interface.
func (p *BinanceSmartMoneyProvider) GetSmartMoneyPositionHistory(leaderID string, startTime, endTime time.Time) ([]BinanceSmartMoneyPositionHistoryRecord, error) {
	topTraderID, err := NormalizeTopTraderID(leaderID)
	if err != nil {
		return nil, fmt.Errorf("binance smart money history: %w", err)
	}
	if err := validateSmartDiagnosticRange(startTime, endTime); err != nil {
		return nil, fmt.Errorf("binance smart money history: %w", err)
	}
	if err := p.requireSmartDiagnosticSharing(topTraderID, true); err != nil {
		return nil, err
	}
	rows, err := p.fetchAllSmartDiagnosticRows(topTraderID, BinanceSmartMoneyPositionHistoryAPI, startTime, endTime, smartMoneyRows, false)
	if err != nil {
		return nil, fmt.Errorf("binance smart money history: %w", err)
	}
	records := make([]BinanceSmartMoneyPositionHistoryRecord, 0, len(rows))
	for index, raw := range rows {
		record, decodeErr := decodeSmartPositionHistoryRecord(raw)
		if decodeErr != nil {
			return nil, fmt.Errorf("binance smart money history row %d: %w", index+1, decodeErr)
		}
		records = append(records, record)
	}
	return records, nil
}

// GetSmartMoneyLatestOperations retrieves the complete delayed UM operation
// history range for audit and diagnosis only.
func (p *BinanceSmartMoneyProvider) GetSmartMoneyLatestOperations(leaderID string, startTime, endTime time.Time) ([]BinanceSmartMoneyOrderHistoryRecord, error) {
	topTraderID, err := NormalizeTopTraderID(leaderID)
	if err != nil {
		return nil, fmt.Errorf("binance smart money latest operations: %w", err)
	}
	if err := validateSmartDiagnosticRange(startTime, endTime); err != nil {
		return nil, fmt.Errorf("binance smart money latest operations: %w", err)
	}
	if err := p.requireSmartDiagnosticSharing(topTraderID, false); err != nil {
		return nil, err
	}
	rows, err := p.fetchAllSmartDiagnosticRows(topTraderID, BinanceSmartMoneyOrderHistoryAPI, startTime, endTime, 10, true)
	if err != nil {
		return nil, fmt.Errorf("binance smart money latest operations: %w", err)
	}
	records := make([]BinanceSmartMoneyOrderHistoryRecord, 0, len(rows))
	for index, raw := range rows {
		record, decodeErr := decodeSmartOrderHistoryRecord(raw)
		if decodeErr != nil {
			return nil, fmt.Errorf("binance smart money latest operations row %d: %w", index+1, decodeErr)
		}
		records = append(records, record)
	}
	return records, nil
}

func (p *BinanceSmartMoneyProvider) LastSourceHealthObservation() SourceHealthObservation {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastObservation
}

func (p *BinanceSmartMoneyProvider) setObservation(obs SourceHealthObservation) {
	if obs.CheckedAt.IsZero() {
		obs.CheckedAt = time.Now()
	}
	p.mu.Lock()
	p.lastObservation = obs
	p.mu.Unlock()
}

func NormalizeTopTraderID(value string) (string, error) {
	value = strings.TrimSpace(value)
	if parsed, err := url.Parse(value); err == nil && parsed.Host != "" {
		host := strings.ToLower(parsed.Hostname())
		if host != "binance.com" && !strings.HasSuffix(host, ".binance.com") {
			return "", fmt.Errorf("Smart Money URL must use a binance.com host")
		}
		parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
		profileID := ""
		for i := 0; i+2 < len(parts); i++ {
			if parts[i] == "smart-money" && parts[i+1] == "profile" {
				if i+3 != len(parts) {
					return "", fmt.Errorf("Smart Money URL must end at /smart-money/profile/<topTraderId>")
				}
				profileID = parts[i+2]
				break
			}
		}
		if profileID == "" {
			return "", fmt.Errorf("Smart Money URL path must contain /smart-money/profile/<topTraderId>")
		}
		value = profileID
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("topTraderId is required")
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return "", fmt.Errorf("topTraderId must contain digits only")
		}
	}
	if len(value) != 19 {
		return "", fmt.Errorf("topTraderId must contain exactly 19 digits")
	}
	return value, nil
}

func (p *BinanceSmartMoneyProvider) GetAccountState(leaderID string) (*AccountState, error) {
	topTraderID, err := NormalizeTopTraderID(leaderID)
	if err != nil {
		return nil, fmt.Errorf("binance smart money: %w", err)
	}
	checkedAt := time.Now()
	// Visibility flags authorize trading and must be refreshed for every
	// authoritative snapshot. Equity may fall back to a zeroed stale profile
	// below, but sharingPosition must never remain trusted for the full cache
	// TTL after the leader turns private.
	profile, err := p.getProfile(topTraderID, checkedAt, true)
	if err != nil {
		if !errors.Is(err, ErrBinanceCredentialsExpired) {
			// A stale profile is sufficient to retain the last confirmed
			// visibility flags, but its equity must never size new risk.
			// Positions are still fetched below so reductions and closes can
			// reconcile through a transient profile outage.
			profile = p.staleProfile(topTraderID)
			if profile != nil {
				profile.UMMarginBalance = 0
			}
		}
		if profile == nil {
			status := "ERROR"
			if errors.Is(err, ErrBinanceCredentialsExpired) {
				status = "AUTH_FAILED"
			}
			p.setObservation(SourceHealthObservation{Status: status, Error: err.Error(), CheckedAt: checkedAt})
			return nil, err
		}
	}
	if profile.Enable == nil {
		err = errors.New("binance smart money profile missing enable")
		p.setObservation(SourceHealthObservation{Status: "ERROR", TraderName: profile.TraderName, Error: err.Error(), CheckedAt: checkedAt})
		return nil, err
	}
	if !*profile.Enable {
		p.setObservation(SourceHealthObservation{Status: "DISABLED", TraderName: profile.TraderName, Error: ErrBinanceSmartMoneyDisabled.Error(), CheckedAt: checkedAt})
		return nil, ErrBinanceSmartMoneyDisabled
	}
	if profile.SharingPosition == nil {
		err = errors.New("binance smart money profile missing sharingPosition")
		p.setObservation(SourceHealthObservation{Status: "ERROR", TraderName: profile.TraderName, Error: err.Error(), CheckedAt: checkedAt})
		return nil, err
	}
	if !*profile.SharingPosition {
		p.setObservation(SourceHealthObservation{Status: "PRIVATE", TraderName: profile.TraderName, Error: ErrBinanceSmartMoneyPrivate.Error(), CheckedAt: checkedAt})
		return nil, ErrBinanceSmartMoneyPrivate
	}
	positions, err := p.fetchAllPositions(topTraderID)
	if err != nil {
		status := "ERROR"
		if errors.Is(err, ErrBinanceCredentialsExpired) {
			status = "AUTH_FAILED"
		}
		p.setObservation(SourceHealthObservation{Status: status, TraderName: profile.TraderName, Error: err.Error(), CheckedAt: checkedAt})
		return nil, err
	}
	emptySnapshotConfirmed := false
	if countSmartOpenPositions(positions) == 0 {
		// Bypass the 15-second cache: a just-hidden profile can otherwise make an
		// empty positions response look like a genuine full close.
		freshProfile, profileErr := p.getProfile(topTraderID, time.Now(), true)
		if profileErr != nil {
			status := "ERROR"
			if errors.Is(profileErr, ErrBinanceCredentialsExpired) {
				status = "AUTH_FAILED"
			}
			p.setObservation(SourceHealthObservation{Status: status, TraderName: profile.TraderName, Error: profileErr.Error(), CheckedAt: checkedAt})
			return nil, profileErr
		}
		profile = freshProfile
		if profile.Enable == nil {
			err = errors.New("binance smart money profile missing enable")
			p.setObservation(SourceHealthObservation{Status: "ERROR", TraderName: profile.TraderName, Error: err.Error(), CheckedAt: checkedAt})
			return nil, err
		}
		if !*profile.Enable {
			p.setObservation(SourceHealthObservation{Status: "DISABLED", TraderName: profile.TraderName, Error: ErrBinanceSmartMoneyDisabled.Error(), CheckedAt: checkedAt})
			return nil, ErrBinanceSmartMoneyDisabled
		}
		if profile.SharingPosition == nil {
			err = errors.New("binance smart money profile missing sharingPosition")
			p.setObservation(SourceHealthObservation{Status: "ERROR", TraderName: profile.TraderName, Error: err.Error(), CheckedAt: checkedAt})
			return nil, err
		}
		if !*profile.SharingPosition {
			p.setObservation(SourceHealthObservation{Status: "PRIVATE", TraderName: profile.TraderName, Error: ErrBinanceSmartMoneyPrivate.Error(), CheckedAt: checkedAt})
			return nil, ErrBinanceSmartMoneyPrivate
		}
		if p.requiresEmptySnapshotConfirmation() {
			if !p.confirmEmptySnapshot(time.Now()) {
				p.setObservation(SourceHealthObservation{Status: "ERROR", TraderName: profile.TraderName, Error: ErrBinanceSmartMoneyEmptyUnconfirmed.Error(), CheckedAt: checkedAt})
				return nil, ErrBinanceSmartMoneyEmptyUnconfirmed
			}
			emptySnapshotConfirmed = true
		}
	}

	catalog, catalogErr := p.getCatalog()
	state := &AccountState{
		TotalEquity:            float64(profile.UMMarginBalance),
		AvailableBalance:       float64(profile.UMMarginBalance),
		Positions:              make(map[string]*Position),
		Timestamp:              checkedAt,
		EmptySnapshotConfirmed: emptySnapshotConfirmed,
	}
	for _, raw := range positions {
		symbol := strings.ToUpper(strings.TrimSpace(raw.Symbol))
		if symbol == "" {
			err = errors.New("binance smart money position missing symbol")
			p.setObservation(SourceHealthObservation{Status: "ERROR", TraderName: profile.TraderName, Error: err.Error(), CheckedAt: checkedAt})
			return nil, err
		}
		amount := float64(raw.Amount)
		var side SideType
		if amount > 0 {
			side = SideLong
		} else if amount < 0 {
			side = SideShort
		} else {
			continue
		}
		// M7：amount 符号是方向权威，但若接口显式声明的 side 与符号矛盾，
		// 说明字段语义可能已变更（例如改为 amount 恒正 + side 定向）。
		// 此时静默信任 amount 会把所有空头方向判反，必须 fail-closed，
		// 拒绝整个快照，等待人工确认接口语义。
		if declared := strings.ToUpper(strings.TrimSpace(raw.Side)); declared == "LONG" || declared == "SHORT" {
			if (declared == "LONG") != (side == SideLong) {
				err = fmt.Errorf("binance smart money position %s: declared side %q conflicts with amount %v sign", symbol, raw.Side, amount)
				p.setObservation(SourceHealthObservation{Status: "ERROR", TraderName: profile.TraderName, Error: err.Error(), CheckedAt: checkedAt})
				return nil, err
			}
		}
		size := math.Abs(amount)
		if size == 0 {
			continue
		}
		markPrice := float64(raw.MarkPrice)
		entryPrice := float64(raw.EntryPrice)
		rawNotional := size * markPrice
		marginMode := "cross"
		if raw.Isolated {
			marginMode = "isolated"
		}
		inst, found := catalog[symbol]
		valueCurrency := ""
		valueUSD, valueValid, valueErr := 0.0, false, ""
		if catalogErr != nil {
			valueErr = catalogErr.Error()
		} else if !found {
			valueErr = "instrument not found in Binance USD-M catalog"
		} else if inst.Status != "TRADING" {
			valueErr = "instrument is not TRADING"
		} else if inst.ContractType != "PERPETUAL" && inst.ContractType != "TRADIFI_PERPETUAL" {
			valueErr = "unsupported contract type: " + inst.ContractType
		} else {
			valueCurrency = inst.QuoteAsset
			rate, rateErr := p.quoteToUSD(inst.QuoteAsset)
			if rateErr != nil {
				valueErr = rateErr.Error()
			} else {
				valueUSD = rawNotional * rate
				valueValid = true
			}
		}
		posID := fmt.Sprintf("smart_%s_%s_%s", topTraderID, symbol, side)
		instCopy := inst
		state.Positions[posID] = &Position{
			Symbol: symbol, Side: side, Size: size, EntryPrice: entryPrice,
			MarkPrice: markPrice, Leverage: int(math.Round(float64(raw.Leverage))),
			MarginMode: marginMode, UnrealizedPnL: float64(raw.PnL),
			PositionValue: valueUSD, RawPositionValue: rawNotional,
			ValueCurrency: valueCurrency, ValueUSDValid: valueValid, ValueError: valueErr,
			Instrument: &instCopy, PosID: posID,
		}
		if valueErr != "" {
			// Keep the complete position snapshot for risk-reducing reconciliation;
			// open/add decisions are blocked later by ValueUSDValid.
			state.Positions[posID].Instrument = &instCopy
		}
	}
	p.setObservation(SourceHealthObservation{Status: "HEALTHY", TraderName: profile.TraderName, CompleteSnapshot: true, CheckedAt: checkedAt})
	// M5：只在快照非空时更新基线。空快照确认后若立即把基线归零并清空
	// candidate，下一轮空快照将不再携带 EmptySnapshotConfirmed 标记；
	// 引擎层（confirmSmartMoneyEmptySnapshot）会因此再跑一轮完整确认
	// 窗口（双层窗口串联，双倍延迟）。保留基线与 candidate：后续空快照
	// 由 confirmEmptySnapshot 立即复确认，直到仓位重新出现才重置基线。
	if len(state.Positions) > 0 {
		p.rememberCompletePositionCount(len(state.Positions))
	}
	return state, nil
}

func countSmartOpenPositions(positions []binanceSmartPositionRaw) int {
	count := 0
	for _, position := range positions {
		if math.Abs(float64(position.Amount)) > 0 {
			count++
		}
	}
	return count
}

func (p *BinanceSmartMoneyProvider) requiresEmptySnapshotConfirmation() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastCompletePositionCount > 0
}

func (p *BinanceSmartMoneyProvider) confirmEmptySnapshot(now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.emptySnapshotCandidateAt.IsZero() {
		p.emptySnapshotCandidateAt = now
		return false
	}
	return now.Sub(p.emptySnapshotCandidateAt) >= smartMoneyEmptyConfirmDelay
}

func (p *BinanceSmartMoneyProvider) rememberCompletePositionCount(count int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastCompletePositionCount = count
	p.emptySnapshotCandidateAt = time.Time{}
}

type binanceSmartEnvelope struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Msg     string          `json:"msg"`
	Success *bool           `json:"success"`
	Data    json.RawMessage `json:"data"`
}

func decodeSmartEnvelope(raw []byte, target interface{}) error {
	var env binanceSmartEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("decode Binance Smart Money envelope: %w", err)
	}
	if isBinanceAuthError(env.Code) {
		return ErrBinanceCredentialsExpired
	}
	success := env.Code == "" || env.Code == BinanceCodeSuccess || env.Code == "0" || env.Code == "200"
	if env.Success != nil {
		success = success && *env.Success
	}
	if !success {
		message := env.Message
		if message == "" {
			message = env.Msg
		}
		return fmt.Errorf("Binance Smart Money API error: code=%s msg=%s", env.Code, message)
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return errors.New("Binance Smart Money API returned empty data")
	}
	if err := json.Unmarshal(env.Data, target); err != nil {
		return fmt.Errorf("decode Binance Smart Money data: %w", err)
	}
	return nil
}

type binanceSmartProfile struct {
	TopTraderID            string    `json:"topTraderId"`
	TraderName             string    `json:"traderName"`
	AccountName            string    `json:"accountName"`
	AvatarURL              string    `json:"avatarUrl"`
	Enable                 *bool     `json:"enable"`
	SharingPosition        *bool     `json:"sharingPosition"`
	SharingPositionHistory *bool     `json:"sharingPositionHistory"`
	SharingLatestRecord    *bool     `json:"sharingLatestRecord"`
	UMMarginBalance        flexFloat `json:"umMarginBalance"`
}

func (p *BinanceSmartMoneyProvider) getProfile(topTraderID string, now time.Time, force bool) (*binanceSmartProfile, error) {
	p.mu.Lock()
	if !force && p.profile != nil && p.profileTopTraderID == topTraderID && now.Sub(p.profileFetchedAt) < smartMoneyProfileTTL {
		copy := *p.profile
		p.mu.Unlock()
		return &copy, nil
	}
	p.mu.Unlock()
	p20t, csrf, err := p.auth.credentials()
	if err != nil {
		return nil, err
	}
	endpoint := BinanceSmartMoneyProfileAPI + "?topTraderId=" + url.QueryEscape(topTraderID)
	raw, _, err := binanceWebRequest(p.client, p20t, csrf, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	var profile binanceSmartProfile
	if err := decodeSmartEnvelope(raw, &profile); err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.profile = &profile
	p.profileTopTraderID = topTraderID
	p.profileFetchedAt = now
	p.mu.Unlock()
	return &profile, nil
}

func (p *BinanceSmartMoneyProvider) staleProfile(topTraderID string) *binanceSmartProfile {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.profile == nil || p.profileTopTraderID != topTraderID {
		return nil
	}
	copy := *p.profile
	return &copy
}

func validateSmartDiagnosticRange(startTime, endTime time.Time) error {
	if startTime.IsZero() || endTime.IsZero() {
		return errors.New("startTime and endTime are required")
	}
	if endTime.Before(startTime) {
		return errors.New("endTime must not be before startTime")
	}
	return nil
}

func (p *BinanceSmartMoneyProvider) requireSmartDiagnosticSharing(topTraderID string, positionHistory bool) error {
	// Diagnostic sharing flags are privacy controls, not sizing data. Always
	// refresh them before touching a private history endpoint.
	profile, err := p.getProfile(topTraderID, time.Now(), true)
	if err != nil {
		return err
	}
	if profile.Enable == nil {
		return errors.New("binance smart money profile missing enable")
	}
	if !*profile.Enable {
		return ErrBinanceSmartMoneyDisabled
	}
	if positionHistory {
		if profile.SharingPositionHistory == nil {
			return errors.New("binance smart money profile missing sharingPositionHistory")
		}
		if !*profile.SharingPositionHistory {
			return ErrBinanceSmartMoneyHistoryPrivate
		}
		return nil
	}
	if profile.SharingLatestRecord == nil {
		return errors.New("binance smart money profile missing sharingLatestRecord")
	}
	if !*profile.SharingLatestRecord {
		return ErrBinanceSmartMoneyLatestPrivate
	}
	return nil
}

func (p *BinanceSmartMoneyProvider) fetchAllSmartDiagnosticRows(topTraderID, endpoint string, startTime, endTime time.Time, rowsPerPage int, includeMarketType bool) ([]json.RawMessage, error) {
	p20t, csrf, err := p.auth.credentials()
	if err != nil {
		return nil, err
	}
	var all []json.RawMessage
	expectedTotal := -1
	seenPages := make(map[string]bool)
	for page := 1; page <= smartMoneyMaxPages; page++ {
		q := url.Values{
			"topTraderId": {topTraderID},
			"startTime":   {strconv.FormatInt(startTime.UnixMilli(), 10)},
			"endTime":     {strconv.FormatInt(endTime.UnixMilli(), 10)},
			"page":        {strconv.Itoa(page)},
			"rows":        {strconv.Itoa(rowsPerPage)},
		}
		if includeMarketType {
			q.Set("marketType", "UM")
		}
		raw, _, requestErr := binanceWebRequest(p.client, p20t, csrf, http.MethodGet, endpoint+"?"+q.Encode(), nil)
		if requestErr != nil {
			return nil, fmt.Errorf("query page %d: %w", page, requestErr)
		}
		var data json.RawMessage
		if err := decodeSmartEnvelope(raw, &data); err != nil {
			return nil, fmt.Errorf("query page %d: %w", page, err)
		}
		pageRows, total, hasTotal, err := parseSmartDiagnosticPage(data)
		if err != nil {
			return nil, fmt.Errorf("query page %d: %w", page, err)
		}
		if hasTotal {
			if total < 0 {
				return nil, fmt.Errorf("query page %d: negative total %d", page, total)
			}
			if expectedTotal >= 0 && total != expectedTotal {
				return nil, fmt.Errorf("pagination total changed from %d to %d", expectedTotal, total)
			}
			expectedTotal = total
		}
		fingerprint := smartDiagnosticPageFingerprint(pageRows)
		if len(pageRows) > 0 && seenPages[fingerprint] {
			return nil, fmt.Errorf("pagination repeated page at page %d", page)
		}
		if len(pageRows) > 0 {
			seenPages[fingerprint] = true
		}
		all = append(all, pageRows...)
		if expectedTotal >= 0 {
			if len(all) >= expectedTotal {
				if len(all) != expectedTotal {
					return nil, fmt.Errorf("pagination count %d exceeds total %d", len(all), expectedTotal)
				}
				return all, nil
			}
			if len(pageRows) == 0 {
				return nil, fmt.Errorf("pagination ended at %d of %d", len(all), expectedTotal)
			}
		} else if len(pageRows) < rowsPerPage {
			return all, nil
		}
	}
	return nil, fmt.Errorf("pagination exceeded %d pages", smartMoneyMaxPages)
}

func parseSmartDiagnosticPage(data json.RawMessage) ([]json.RawMessage, int, bool, error) {
	var direct []json.RawMessage
	if err := json.Unmarshal(data, &direct); err == nil {
		return direct, 0, false, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, 0, false, err
	}
	total, hasTotal := parseSmartTotal(obj)
	for _, key := range []string{"positions", "orders", "rows", "list", "items", "records"} {
		raw, ok := obj[key]
		if !ok {
			continue
		}
		if err := json.Unmarshal(raw, &direct); err == nil {
			return direct, total, hasTotal, nil
		}
	}
	// Some Binance envelopes introduce one additional data/result container.
	// Parse exactly one nested object while retaining an outer total when one is
	// present; unknown deeper schemas fail closed.
	for _, key := range []string{"data", "result"} {
		raw, ok := obj[key]
		if !ok {
			continue
		}
		var nested map[string]json.RawMessage
		if err := json.Unmarshal(raw, &nested); err != nil {
			continue
		}
		nestedTotal, nestedHasTotal := parseSmartTotal(nested)
		for _, rowsKey := range []string{"positions", "orders", "rows", "list", "items", "records"} {
			rowsRaw, rowsOK := nested[rowsKey]
			if !rowsOK || json.Unmarshal(rowsRaw, &direct) != nil {
				continue
			}
			if !hasTotal && nestedHasTotal {
				total, hasTotal = nestedTotal, true
			}
			return direct, total, hasTotal, nil
		}
	}
	return nil, total, hasTotal, errors.New("diagnostic page does not contain a recognized rows array")
}

func smartDiagnosticPageFingerprint(rows []json.RawMessage) string {
	if len(rows) == 0 {
		return "empty"
	}
	return fmt.Sprintf("%d|%s|%s", len(rows), rows[0], rows[len(rows)-1])
}

func decodeSmartPositionHistoryRecord(raw json.RawMessage) (BinanceSmartMoneyPositionHistoryRecord, error) {
	obj, err := decodeSmartDiagnosticObject(raw)
	if err != nil {
		return BinanceSmartMoneyPositionHistoryRecord{}, err
	}
	record := BinanceSmartMoneyPositionHistoryRecord{
		ID:            smartDiagnosticString(obj, "id", "positionId", "positionID"),
		Symbol:        strings.ToUpper(strings.TrimSpace(smartDiagnosticString(obj, "symbol", "pair"))),
		Side:          smartDiagnosticString(obj, "side"),
		PositionSide:  smartDiagnosticString(obj, "positionSide", "direction"),
		Status:        smartDiagnosticString(obj, "status"),
		Isolated:      smartDiagnosticBool(obj, "isolated", "isIsolated"),
		Leverage:      smartDiagnosticFloat(obj, "leverage"),
		Amount:        smartDiagnosticFloat(obj, "amount", "positionAmount", "quantity", "maxOpenInterest"),
		EntryPrice:    smartDiagnosticFloat(obj, "entryPrice", "openPrice", "avgOpenPrice"),
		AvgCost:       smartDiagnosticFloat(obj, "avgCost", "averageCost"),
		AvgClosePrice: smartDiagnosticFloat(obj, "avgClosePrice", "closePrice", "averageClosePrice"),
		MarkPrice:     smartDiagnosticFloat(obj, "markPrice"),
		PnL:           smartDiagnosticFloat(obj, "pnl", "closingPnl", "realizedPnl", "realizedProfit"),
		ROE:           smartDiagnosticFloat(obj, "roe", "roi", "returnRate"),
		OpenedAt:      smartDiagnosticInt64(obj, "opened", "openTime", "openedAt", "createTime"),
		ClosedAt:      smartDiagnosticInt64(obj, "closed", "closeTime", "closedAt"),
		UpdatedAt:     smartDiagnosticInt64(obj, "updateTime", "updatedAt"),
		Raw:           append(json.RawMessage(nil), raw...),
	}
	if record.Symbol == "" {
		return BinanceSmartMoneyPositionHistoryRecord{}, errors.New("record missing symbol")
	}
	return record, nil
}

func decodeSmartOrderHistoryRecord(raw json.RawMessage) (BinanceSmartMoneyOrderHistoryRecord, error) {
	obj, err := decodeSmartDiagnosticObject(raw)
	if err != nil {
		return BinanceSmartMoneyOrderHistoryRecord{}, err
	}
	record := BinanceSmartMoneyOrderHistoryRecord{
		ID:               smartDiagnosticString(obj, "id", "recordId"),
		OrderID:          smartDiagnosticString(obj, "orderId", "orderID"),
		TradeID:          smartDiagnosticString(obj, "tradeId", "tradeID"),
		Symbol:           strings.ToUpper(strings.TrimSpace(smartDiagnosticString(obj, "symbol", "pair"))),
		Side:             smartDiagnosticString(obj, "side"),
		PositionSide:     smartDiagnosticString(obj, "positionSide", "direction"),
		Operation:        smartDiagnosticString(obj, "operation", "action", "eventType"),
		OrderType:        smartDiagnosticString(obj, "type", "orderType"),
		Status:           smartDiagnosticString(obj, "status", "orderStatus"),
		Price:            smartDiagnosticFloat(obj, "price", "orderPrice"),
		AveragePrice:     smartDiagnosticFloat(obj, "avgPrice", "averagePrice"),
		Quantity:         smartDiagnosticFloat(obj, "quantity", "qty", "amount", "origQty"),
		ExecutedQuantity: smartDiagnosticFloat(obj, "executedQuantity", "executedQty", "cumQty"),
		RealizedPnL:      smartDiagnosticFloat(obj, "realizedPnl", "realizedProfit", "pnl"),
		CreatedAt:        smartDiagnosticInt64(obj, "time", "createTime", "createdAt", "orderTime"),
		UpdatedAt:        smartDiagnosticInt64(obj, "updateTime", "updatedAt"),
		Raw:              append(json.RawMessage(nil), raw...),
	}
	if record.Symbol == "" {
		return BinanceSmartMoneyOrderHistoryRecord{}, errors.New("record missing symbol")
	}
	return record, nil
}

func decodeSmartDiagnosticObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, errors.New("record is not an object")
	}
	return obj, nil
}

func smartDiagnosticString(obj map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		raw, ok := obj[key]
		if !ok || len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var text string
		if json.Unmarshal(raw, &text) == nil {
			return text
		}
		return strings.Trim(string(raw), "\"")
	}
	return ""
}

func smartDiagnosticFloat(obj map[string]json.RawMessage, keys ...string) float64 {
	for _, key := range keys {
		raw, ok := obj[key]
		if !ok {
			continue
		}
		var value flexFloat
		if json.Unmarshal(raw, &value) == nil {
			return float64(value)
		}
	}
	return 0
}

func smartDiagnosticInt64(obj map[string]json.RawMessage, keys ...string) int64 {
	return int64(smartDiagnosticFloat(obj, keys...))
}

func smartDiagnosticBool(obj map[string]json.RawMessage, keys ...string) bool {
	for _, key := range keys {
		raw, ok := obj[key]
		if !ok {
			continue
		}
		var value bool
		if json.Unmarshal(raw, &value) == nil {
			return value
		}
		var text string
		if json.Unmarshal(raw, &text) == nil {
			value, _ = strconv.ParseBool(text)
			return value
		}
	}
	return false
}

type binanceSmartPositionRaw struct {
	Symbol     string    `json:"symbol"`
	Isolated   bool      `json:"isolated"`
	Side       string    `json:"side"`
	EntryPrice flexFloat `json:"entryPrice"`
	MarkPrice  flexFloat `json:"markPrice"`
	LiqPrice   flexFloat `json:"liqPrice"`
	Margin     flexFloat `json:"margin"`
	PnL        flexFloat `json:"pnl"`
	Amount     flexFloat `json:"amount"`
	Leverage   flexFloat `json:"leverage"`
}

func (p *BinanceSmartMoneyProvider) fetchAllPositions(topTraderID string) ([]binanceSmartPositionRaw, error) {
	p20t, csrf, err := p.auth.credentials()
	if err != nil {
		return nil, err
	}
	var all []binanceSmartPositionRaw
	expectedTotal := -1
	seenPages := make(map[string]bool)
	seenPositions := make(map[string]bool)
	for page := 1; page <= smartMoneyMaxPages; page++ {
		q := url.Values{"topTraderId": {topTraderID}, "marketType": {"UM"}, "page": {strconv.Itoa(page)}, "rows": {strconv.Itoa(smartMoneyRows)}}
		raw, _, reqErr := binanceWebRequest(p.client, p20t, csrf, http.MethodGet, BinanceSmartMoneyPositionsAPI+"?"+q.Encode(), nil)
		if reqErr != nil {
			return nil, fmt.Errorf("query Smart Money positions page %d: %w", page, reqErr)
		}
		var data json.RawMessage
		if err := decodeSmartEnvelope(raw, &data); err != nil {
			return nil, fmt.Errorf("query Smart Money positions page %d: %w", page, err)
		}
		rows, total, hasTotal, err := parseSmartPositionPage(data)
		if err != nil {
			return nil, fmt.Errorf("query Smart Money positions page %d: %w", page, err)
		}
		if hasTotal {
			if expectedTotal >= 0 && total != expectedTotal {
				return nil, fmt.Errorf("position pagination total changed from %d to %d", expectedTotal, total)
			}
			expectedTotal = total
		}
		fingerprint := smartPageFingerprint(rows)
		if len(rows) > 0 && seenPages[fingerprint] {
			return nil, fmt.Errorf("position pagination repeated page fingerprint at page %d", page)
		}
		if len(rows) > 0 {
			seenPages[fingerprint] = true
		}
		for _, row := range rows {
			symbol := strings.ToUpper(strings.TrimSpace(row.Symbol))
			if symbol == "" {
				return nil, fmt.Errorf("position pagination page %d contains an empty symbol", page)
			}
			amount := float64(row.Amount)
			if amount == 0 || math.IsNaN(amount) || math.IsInf(amount, 0) {
				return nil, fmt.Errorf("position pagination page %d contains invalid amount for %s", page, symbol)
			}
			side := ""
			if amount > 0 {
				side = string(SideLong)
			} else {
				side = string(SideShort)
			}
			key := symbol + "|" + side
			if seenPositions[key] {
				return nil, fmt.Errorf("position pagination contains duplicate contract identity %s", key)
			}
			seenPositions[key] = true
		}
		all = append(all, rows...)
		if expectedTotal >= 0 {
			if len(all) >= expectedTotal {
				if len(all) != expectedTotal {
					return nil, fmt.Errorf("position pagination count %d exceeds total %d", len(all), expectedTotal)
				}
				return all, nil
			}
			if len(rows) == 0 {
				return nil, fmt.Errorf("position pagination ended at %d of %d", len(all), expectedTotal)
			}
		} else if len(rows) < smartMoneyRows {
			return all, nil
		}
	}
	return nil, fmt.Errorf("position pagination exceeded %d pages", smartMoneyMaxPages)
}

func parseSmartPositionPage(data json.RawMessage) ([]binanceSmartPositionRaw, int, bool, error) {
	var direct []binanceSmartPositionRaw
	if json.Unmarshal(data, &direct) == nil {
		return direct, 0, false, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, 0, false, err
	}
	total, hasTotal := parseSmartTotal(obj)
	for _, key := range []string{"positions", "rows", "list", "items", "data"} {
		raw, ok := obj[key]
		if !ok {
			continue
		}
		if json.Unmarshal(raw, &direct) == nil {
			return direct, total, hasTotal, nil
		}
	}
	return nil, total, hasTotal, errors.New("position page does not contain a recognized rows array")
}

func parseSmartTotal(obj map[string]json.RawMessage) (int, bool) {
	for _, key := range []string{"total", "totalCount", "count"} {
		raw, ok := obj[key]
		if !ok {
			continue
		}
		var value flexFloat
		if json.Unmarshal(raw, &value) == nil {
			return int(value), true
		}
	}
	return 0, false
}

func smartPageFingerprint(rows []binanceSmartPositionRaw) string {
	if len(rows) == 0 {
		return "empty"
	}
	first, last := rows[0], rows[len(rows)-1]
	return fmt.Sprintf("%s|%s|%.12f|%s|%s|%.12f|%d", first.Symbol, first.Side, first.Amount, last.Symbol, last.Side, last.Amount, len(rows))
}

func (p *BinanceSmartMoneyProvider) getCatalog() (map[string]InstrumentRef, error) {
	p.mu.Lock()
	if len(p.catalog) > 0 && time.Since(p.catalogFetchedAt) < smartMoneyCatalogTTL {
		out := cloneInstrumentCatalog(p.catalog)
		p.mu.Unlock()
		return out, nil
	}
	p.mu.Unlock()
	resp, err := p.client.Get(BinanceUSDExchangeInfoAPI)
	if err != nil {
		return nil, fmt.Errorf("load Binance USD-M catalog: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("load Binance USD-M catalog: http %d", resp.StatusCode)
	}
	var info struct {
		Symbols []struct {
			Symbol, BaseAsset, QuoteAsset, MarginAsset, ContractType, Status string
		} `json:"symbols"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, fmt.Errorf("decode Binance USD-M catalog: %w", err)
	}
	catalog := make(map[string]InstrumentRef, len(info.Symbols))
	for _, item := range info.Symbols {
		catalog[item.Symbol] = InstrumentRef{SourceSymbol: item.Symbol, BaseAsset: item.BaseAsset, QuoteAsset: item.QuoteAsset, MarginAsset: item.MarginAsset, MarketType: "UM", ContractType: item.ContractType, Status: item.Status}
	}
	p.mu.Lock()
	p.catalog = catalog
	p.catalogFetchedAt = time.Now()
	out := cloneInstrumentCatalog(catalog)
	p.mu.Unlock()
	return out, nil
}

func cloneInstrumentCatalog(in map[string]InstrumentRef) map[string]InstrumentRef {
	out := make(map[string]InstrumentRef, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func (p *BinanceSmartMoneyProvider) quoteToUSD(quote string) (float64, error) {
	quote = strings.ToUpper(strings.TrimSpace(quote))
	if quote == "USDT" {
		return 1, nil
	}
	if quote != "USDC" && quote != "USD1" {
		return 0, fmt.Errorf("unsupported quote asset %s", quote)
	}
	p.mu.Lock()
	if cached, ok := p.fxRates[quote]; ok && time.Since(cached.FetchedAt) < smartMoneyFXTTL {
		p.mu.Unlock()
		return cached.Rate, nil
	}
	p.mu.Unlock()
	resp, err := p.client.Get(BinanceSpotPriceAPI + "?symbol=" + url.QueryEscape(quote+"USDT"))
	if err != nil {
		return 0, fmt.Errorf("load %s/USD conversion: %w", quote, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("load %s/USD conversion: http %d", quote, resp.StatusCode)
	}
	var ticker struct {
		Price flexFloat `json:"price"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ticker); err != nil || ticker.Price <= 0 {
		return 0, fmt.Errorf("invalid %s/USD conversion response", quote)
	}
	rate := float64(ticker.Price)
	p.mu.Lock()
	p.fxRates[quote] = cachedSmartFXRate{Rate: rate, FetchedAt: time.Now()}
	p.mu.Unlock()
	return rate, nil
}

type flexFloat float64

func (f *flexFloat) UnmarshalJSON(data []byte) error {
	if string(data) == "null" || len(data) == 0 {
		*f = 0
		return nil
	}
	var number json.Number
	if data[0] == '"' {
		var text string
		if err := json.Unmarshal(data, &text); err != nil {
			return err
		}
		number = json.Number(text)
	} else {
		number = json.Number(string(data))
	}
	value, err := strconv.ParseFloat(number.String(), 64)
	if err != nil {
		return err
	}
	*f = flexFloat(value)
	return nil
}
