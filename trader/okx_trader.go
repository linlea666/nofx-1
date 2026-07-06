package trader

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"nofx/logger"
	"strconv"
	"strings"
	"sync"
	"time"
)

// okxBaseURL is a variable (not const) so tests can point the trader at a
// local httptest server; production code never mutates it.
var okxBaseURL = "https://www.okx.com"

// OKX API endpoints
const (
	okxAccountPath       = "/api/v5/account/balance"
	okxPositionPath      = "/api/v5/account/positions"
	okxOrderPath         = "/api/v5/trade/order"
	okxLeveragePath      = "/api/v5/account/set-leverage"
	okxTickerPath        = "/api/v5/market/ticker"
	okxInstrumentsPath   = "/api/v5/public/instruments"
	okxCancelOrderPath   = "/api/v5/trade/cancel-order"
	okxPendingOrdersPath = "/api/v5/trade/orders-pending"
	okxAlgoOrderPath     = "/api/v5/trade/order-algo"
	okxCancelAlgoPath    = "/api/v5/trade/cancel-algos"
	okxAlgoPendingPath   = "/api/v5/trade/orders-algo-pending"
	okxAlgoHistoryPath   = "/api/v5/trade/orders-algo-history"
	okxAmendAlgoPath     = "/api/v5/trade/amend-algos"
	okxPositionModePath  = "/api/v5/account/set-position-mode"
)

func normalizeOKXTriggerType(v string) string {
	switch strings.ToLower(v) {
	case "mark":
		return "mark"
	case "index":
		return "index"
	default:
		return "last"
	}
}

func (t *OKXTrader) PlaceProtectiveStop(req ProtectiveStopRequest) (*ProtectiveStopOrder, error) {
	inst, err := t.getInstrument(req.Symbol)
	if err != nil {
		return nil, err
	}
	sz := t.formatSize(req.Quantity/inst.CtVal, inst)
	side, posSide := "sell", "long"
	if strings.EqualFold(req.PositionSide, "short") {
		side, posSide = "buy", "short"
	}
	mode := req.MarginMode
	if mode == "" {
		mode = t.getMgnMode()
	}
	clientID := req.ClientID
	if clientID == "" {
		clientID = genOkxClOrdID()
	}
	body := map[string]interface{}{"instId": t.convertSymbol(req.Symbol), "tdMode": mode, "side": side, "posSide": posSide, "ordType": "conditional", "sz": sz, "slTriggerPx": strconv.FormatFloat(req.TriggerPrice, 'f', -1, 64), "slTriggerPxType": normalizeOKXTriggerType(req.TriggerType), "slOrdPx": "-1", "algoClOrdId": clientID, "tag": okxTag}
	data, err := t.doRequest("POST", okxAlgoOrderPath, body)
	if err != nil {
		return nil, err
	}
	var out []struct {
		AlgoID      string `json:"algoId"`
		AlgoClOrdID string `json:"algoClOrdId"`
		SCode       string `json:"sCode"`
		SMsg        string `json:"sMsg"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	if len(out) == 0 || out[0].AlgoID == "" || out[0].SCode != "0" {
		reqJSON, _ := json.Marshal(body)
		return nil, fmt.Errorf("OKX protective stop rejected: %v (request=%s)", out, reqJSON)
	}
	return &ProtectiveStopOrder{AlgoID: out[0].AlgoID, ClientID: out[0].AlgoClOrdID, Symbol: req.Symbol, PositionSide: posSide, MarginMode: mode, Quantity: req.Quantity, TriggerPrice: req.TriggerPrice, TriggerType: req.TriggerType, State: "live"}, nil
}

func (t *OKXTrader) AmendProtectiveStop(algoID string, req ProtectiveStopRequest) error {
	inst, err := t.getInstrument(req.Symbol)
	if err != nil {
		return err
	}
	// amend-algos takes a single JSON object (unlike cancel-algos, which takes
	// an array); sending an array is rejected with 50002 "Incorrect json data
	// format" and the amend never succeeds.
	body := map[string]interface{}{"instId": t.convertSymbol(req.Symbol), "algoId": algoID, "newSz": t.formatSize(req.Quantity/inst.CtVal, inst), "newSlTriggerPx": strconv.FormatFloat(req.TriggerPrice, 'f', -1, 64), "newSlOrdPx": "-1"}
	data, err := t.doRequest("POST", okxAmendAlgoPath, body)
	if err != nil {
		return err
	}
	if ackErr := validateOKXAlgoAck(data, "amend"); ackErr != nil {
		reqJSON, _ := json.Marshal(body)
		return fmt.Errorf("%w (request=%s)", ackErr, reqJSON)
	}
	return nil
}

func (t *OKXTrader) GetProtectiveStop(algoID, symbol string) (*ProtectiveStopOrder, error) {
	// orders-algo-pending requires ordType; orders-algo-history requires
	// state or algoId (algoId satisfies it here).
	paths := []string{
		fmt.Sprintf("%s?algoId=%s&ordType=conditional&instType=SWAP", okxAlgoPendingPath, algoID),
		fmt.Sprintf("%s?algoId=%s&ordType=conditional&instType=SWAP", okxAlgoHistoryPath, algoID),
	}
	return t.getProtectiveStopFromPaths(paths, symbol, fmt.Sprintf("protective stop %s not found", algoID))
}

func (t *OKXTrader) GetProtectiveStopByClientID(clientID, symbol string) (*ProtectiveStopOrder, error) {
	// orders-algo-history rejects lookups by algoClOrdId alone with error
	// 50015 ("Either parameter state or algoId is required"), so each
	// terminal state must be enumerated explicitly.
	paths := []string{
		fmt.Sprintf("%s?algoClOrdId=%s&ordType=conditional&instType=SWAP", okxAlgoPendingPath, clientID),
		fmt.Sprintf("%s?algoClOrdId=%s&ordType=conditional&instType=SWAP&state=effective", okxAlgoHistoryPath, clientID),
		fmt.Sprintf("%s?algoClOrdId=%s&ordType=conditional&instType=SWAP&state=canceled", okxAlgoHistoryPath, clientID),
		fmt.Sprintf("%s?algoClOrdId=%s&ordType=conditional&instType=SWAP&state=order_failed", okxAlgoHistoryPath, clientID),
	}
	return t.getProtectiveStopFromPaths(paths, symbol, fmt.Sprintf("protective stop client id %s not found", clientID))
}

// isOKXAlgoNotExistError reports OKX error 51603 ("Order does not exist").
// OKX returns this as a top-level error code instead of an empty result set,
// so it must be treated as a confirmed absence rather than a query failure.
func isOKXAlgoNotExistError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "51603") && strings.Contains(msg, "does not exist")
}

// getProtectiveStopFromPaths queries the given endpoints in order.
// Error contract: ErrProtectiveStopNotFound is returned only when EVERY
// endpoint answered conclusively (success with no record, or OKX 51603) —
// any transient failure wins, because the order could exist on the endpoint
// that failed and the caller must not invalidate local tracking.
func (t *OKXTrader) getProtectiveStopFromPaths(paths []string, symbol string, notFound string) (*ProtectiveStopOrder, error) {
	var lastQueryErr error
	for _, path := range paths {
		data, err := t.doRequest("GET", path, nil)
		if err != nil {
			if isOKXAlgoNotExistError(err) {
				// OKX processed the lookup and confirmed there is no such
				// order on this endpoint: a conclusive answer.
				continue
			}
			lastQueryErr = err
			continue
		}
		var rows []struct {
			AlgoID          string `json:"algoId"`
			AlgoClOrdID     string `json:"algoClOrdId"`
			InstID          string `json:"instId"`
			PosSide         string `json:"posSide"`
			TdMode          string `json:"tdMode"`
			Sz              string `json:"sz"`
			SlTriggerPx     string `json:"slTriggerPx"`
			SlTriggerPxType string `json:"slTriggerPxType"`
			State           string `json:"state"`
			OrdID           string `json:"ordId"`
		}
		if json.Unmarshal(data, &rows) == nil && len(rows) > 0 {
			q, _ := strconv.ParseFloat(rows[0].Sz, 64)
			if inst, instErr := t.getInstrument(symbol); instErr == nil && inst.CtVal > 0 {
				q *= inst.CtVal
			}
			px, _ := strconv.ParseFloat(rows[0].SlTriggerPx, 64)
			return &ProtectiveStopOrder{AlgoID: rows[0].AlgoID, ClientID: rows[0].AlgoClOrdID, Symbol: symbol, PositionSide: rows[0].PosSide, MarginMode: rows[0].TdMode, Quantity: q, TriggerPrice: px, TriggerType: rows[0].SlTriggerPxType, State: rows[0].State, ActualOrderID: rows[0].OrdID}, nil
		}
	}
	if lastQueryErr != nil {
		return nil, fmt.Errorf("protective stop query failed: %w", lastQueryErr)
	}
	return nil, fmt.Errorf("%s: %w", notFound, ErrProtectiveStopNotFound)
}

func (t *OKXTrader) CancelProtectiveStop(algoID, symbol string) error {
	data, err := t.doRequest("POST", okxCancelAlgoPath, []map[string]interface{}{{"algoId": algoID, "instId": t.convertSymbol(symbol)}})
	if err != nil {
		return err
	}
	return validateOKXAlgoAck(data, "cancel")
}

func validateOKXAlgoAck(data []byte, action string) error {
	var rows []struct {
		AlgoID string `json:"algoId"`
		SCode  string `json:"sCode"`
		SMsg   string `json:"sMsg"`
	}
	if err := json.Unmarshal(data, &rows); err != nil {
		return fmt.Errorf("OKX %s algo acknowledgement parse failed: %w", action, err)
	}
	if len(rows) == 0 {
		return fmt.Errorf("OKX %s algo acknowledgement is empty", action)
	}
	for _, row := range rows {
		if row.SCode != "0" {
			return fmt.Errorf("OKX %s algo rejected: algoId=%s sCode=%s sMsg=%s", action, row.AlgoID, row.SCode, row.SMsg)
		}
	}
	return nil
}

// IsOKXAlgoAlreadyExists reports OKX error 51068: an algo order with the same
// algoClOrdId already exists. Copy Guard must adopt that order instead of
// treating the placement as failed.
func IsOKXAlgoAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "51068") && strings.Contains(strings.ToLower(msg), "already exists")
}

// IsOKXAlgoTerminalCancelError reports OKX error 51400: cancellation failed
// because the algo order has already been filled, canceled or purged. When the
// position is flat this is a normal terminal state, not a protection failure.
func IsOKXAlgoTerminalCancelError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "51400") &&
		(strings.Contains(msg, "filled") || strings.Contains(msg, "canceled") || strings.Contains(msg, "does not exist"))
}

// OKXTrader OKX futures trader
type OKXTrader struct {
	apiKey     string
	secretKey  string
	passphrase string

	// Margin mode setting
	isCrossMargin bool

	// HTTP client (proxy disabled)
	httpClient *http.Client

	// Balance cache
	cachedBalance     map[string]interface{}
	balanceCacheTime  time.Time
	balanceCacheMutex sync.RWMutex

	// Positions cache
	cachedPositions     []map[string]interface{}
	positionsCacheTime  time.Time
	positionsCacheMutex sync.RWMutex

	// Symbol margin mode cache (记录每个 symbol 的保证金模式，在 SetMarginMode 时更新)
	symbolMgnModes      map[string]string // symbol -> "cross" | "isolated"
	symbolMgnModesMutex sync.RWMutex

	// Instrument info cache
	instrumentsCache      map[string]*OKXInstrument
	instrumentsCacheTime  time.Time
	instrumentsCacheMutex sync.RWMutex

	// Cache duration
	cacheDuration time.Duration
}

// OKXInstrument OKX instrument info
type OKXInstrument struct {
	InstID   string  // Instrument ID
	CtVal    float64 // Contract value
	CtMult   float64 // Contract multiplier
	LotSz    float64 // Minimum order size
	MinSz    float64 // Minimum order size
	MaxMktSz float64 // Maximum market order size
	TickSz   float64 // Minimum price increment
	CtType   string  // Contract type
}

// OKXResponse OKX API response
type OKXResponse struct {
	Code string          `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// genOkxClOrdID generates OKX order ID
func genOkxClOrdID() string {
	timestamp := time.Now().UnixNano() % 10000000000000
	randomBytes := make([]byte, 4)
	rand.Read(randomBytes)
	randomHex := hex.EncodeToString(randomBytes)
	// OKX clOrdId max 32 characters
	orderID := fmt.Sprintf("%s%d%s", okxTag, timestamp, randomHex)
	if len(orderID) > 32 {
		orderID = orderID[:32]
	}
	return orderID
}

// NewOKXTrader creates OKX trader
func NewOKXTrader(apiKey, secretKey, passphrase string) *OKXTrader {
	// Use default transport which respects system proxy settings
	// OKX requires proxy in China due to DNS pollution
	httpClient := &http.Client{
		Timeout:   30 * time.Second,
		Transport: http.DefaultTransport,
	}

	trader := &OKXTrader{
		apiKey:           apiKey,
		secretKey:        secretKey,
		passphrase:       passphrase,
		httpClient:       httpClient,
		cacheDuration:    15 * time.Second,
		instrumentsCache: make(map[string]*OKXInstrument),
		symbolMgnModes:   make(map[string]string), // 按 symbol 缓存保证金模式
	}

	// Set dual position mode
	if err := trader.setPositionMode(); err != nil {
		logger.Infof("⚠️ Failed to set OKX position mode: %v (ignore if already in dual mode)", err)
	}

	return trader
}

// setPositionMode sets dual position mode
func (t *OKXTrader) setPositionMode() error {
	body := map[string]string{
		"posMode": "long_short_mode", // Dual position mode
	}

	_, err := t.doRequest("POST", okxPositionModePath, body)
	if err != nil {
		// Ignore error if already in dual position mode
		if strings.Contains(err.Error(), "already") || strings.Contains(err.Error(), "Position mode is not modified") {
			logger.Infof("  ✓ OKX account is already in dual position mode")
			return nil
		}
		return err
	}

	logger.Infof("  ✓ OKX account switched to dual position mode")
	return nil
}

// sign generates OKX API signature
func (t *OKXTrader) sign(timestamp, method, requestPath, body string) string {
	preHash := timestamp + method + requestPath + body
	h := hmac.New(sha256.New, []byte(t.secretKey))
	h.Write([]byte(preHash))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// doRequest executes HTTP request
func (t *OKXTrader) doRequest(method, path string, body interface{}) ([]byte, error) {
	var bodyBytes []byte
	var err error

	if body != nil {
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("failed to serialize request body: %w", err)
		}
	}

	timestamp := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	signature := t.sign(timestamp, method, path, string(bodyBytes))

	req, err := http.NewRequest(method, okxBaseURL+path, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("OK-ACCESS-KEY", t.apiKey)
	req.Header.Set("OK-ACCESS-SIGN", signature)
	req.Header.Set("OK-ACCESS-TIMESTAMP", timestamp)
	req.Header.Set("OK-ACCESS-PASSPHRASE", t.passphrase)
	req.Header.Set("Content-Type", "application/json")
	// Set request header
	req.Header.Set("x-simulated-trading", "0")

	resp, err := t.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	var okxResp OKXResponse
	if err := json.Unmarshal(respBody, &okxResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	// code=1 indicates partial success, need to check specific results in data
	// code=2 indicates complete failure
	if okxResp.Code != "0" && okxResp.Code != "1" {
		return nil, fmt.Errorf("OKX API error: code=%s, msg=%s", okxResp.Code, okxResp.Msg)
	}

	return okxResp.Data, nil
}

// convertSymbol converts generic symbol to OKX format
// e.g. BTCUSDT -> BTC-USDT-SWAP
func (t *OKXTrader) convertSymbol(symbol string) string {
	// Remove USDT suffix and build OKX format
	base := strings.TrimSuffix(symbol, "USDT")
	return fmt.Sprintf("%s-USDT-SWAP", base)
}

// convertSymbolBack converts OKX format back to generic symbol
// e.g. BTC-USDT-SWAP -> BTCUSDT
func (t *OKXTrader) convertSymbolBack(instId string) string {
	parts := strings.Split(instId, "-")
	if len(parts) >= 2 {
		return parts[0] + parts[1]
	}
	return instId
}

// GetBalance gets account balance
func (t *OKXTrader) GetBalance() (map[string]interface{}, error) {
	// Check cache
	t.balanceCacheMutex.RLock()
	if t.cachedBalance != nil && time.Since(t.balanceCacheTime) < t.cacheDuration {
		t.balanceCacheMutex.RUnlock()
		logger.Infof("✓ Using cached OKX account balance")
		return t.cachedBalance, nil
	}
	t.balanceCacheMutex.RUnlock()

	logger.Infof("🔄 Calling OKX API to get account balance...")
	data, err := t.doRequest("GET", okxAccountPath, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get account balance: %w", err)
	}

	var balances []struct {
		TotalEq string `json:"totalEq"`
		AdjEq   string `json:"adjEq"`
		IsoEq   string `json:"isoEq"`
		OrdFroz string `json:"ordFroz"`
		Details []struct {
			Ccy      string `json:"ccy"`
			Eq       string `json:"eq"`
			CashBal  string `json:"cashBal"`
			AvailBal string `json:"availBal"`
			UPL      string `json:"upl"`
		} `json:"details"`
	}

	if err := json.Unmarshal(data, &balances); err != nil {
		return nil, fmt.Errorf("failed to parse balance data: %w", err)
	}

	if len(balances) == 0 {
		return nil, fmt.Errorf("no balance data received")
	}

	balance := balances[0]

	// Find USDT balance
	var usdtAvail, usdtUPL float64
	for _, detail := range balance.Details {
		if detail.Ccy == "USDT" {
			usdtAvail, _ = strconv.ParseFloat(detail.AvailBal, 64)
			usdtUPL, _ = strconv.ParseFloat(detail.UPL, 64)
			break
		}
	}

	totalEq, _ := strconv.ParseFloat(balance.TotalEq, 64)

	result := map[string]interface{}{
		"totalWalletBalance":    totalEq,
		"availableBalance":      usdtAvail,
		"totalUnrealizedProfit": usdtUPL,
	}

	logger.Infof("✓ OKX balance: Total equity=%.2f, Available=%.2f, Unrealized PnL=%.2f", totalEq, usdtAvail, usdtUPL)

	// Update cache
	t.balanceCacheMutex.Lock()
	t.cachedBalance = result
	t.balanceCacheTime = time.Now()
	t.balanceCacheMutex.Unlock()

	return result, nil
}

// GetPositions gets all positions
func (t *OKXTrader) GetPositions() ([]map[string]interface{}, error) {
	// Check cache
	// 有效性基于时间而非 cachedPositions != nil：空仓账户的合法结果是空切片，
	// 若用 nil 判断，空仓时缓存永远不生效 → 每次查询都打真实 API（50011 根因之一）
	t.positionsCacheMutex.RLock()
	if !t.positionsCacheTime.IsZero() && time.Since(t.positionsCacheTime) < t.cacheDuration {
		// 确保缓存数据中的 mgnMode 不为空（可能在缓存时 OKX API 返回了空值）
		result := t.ensureMgnModeInPositions(t.cachedPositions)
		t.positionsCacheMutex.RUnlock()
		logger.Infof("✓ Using cached OKX positions")
		return result, nil
	}
	t.positionsCacheMutex.RUnlock()

	logger.Infof("🔄 Calling OKX API to get positions...")
	data, err := t.doRequest("GET", okxPositionPath+"?instType=SWAP", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get positions: %w", err)
	}

	var positions []struct {
		InstId  string `json:"instId"`
		PosSide string `json:"posSide"`
		Pos     string `json:"pos"`
		AvgPx   string `json:"avgPx"`
		MarkPx  string `json:"markPx"`
		Upl     string `json:"upl"`
		Lever   string `json:"lever"`
		LiqPx   string `json:"liqPx"`
		Margin  string `json:"margin"`
		MgnMode string `json:"mgnMode"` // "cross" | "isolated" 保证金模式
		CTime   string `json:"cTime"`   // Position created time (ms)
		UTime   string `json:"uTime"`   // Position last update time (ms)
		PosId   string `json:"posId"`   // 仓位唯一标识
	}

	if err := json.Unmarshal(data, &positions); err != nil {
		return nil, fmt.Errorf("failed to parse position data: %w", err)
	}

	// 空仓时保持空切片（非 nil），使"无持仓"成为可缓存状态
	result := make([]map[string]interface{}, 0, len(positions))
	for _, pos := range positions {
		contractCount, _ := strconv.ParseFloat(pos.Pos, 64)
		if contractCount == 0 {
			continue
		}

		entryPrice, _ := strconv.ParseFloat(pos.AvgPx, 64)
		markPrice, _ := strconv.ParseFloat(pos.MarkPx, 64)
		upl, _ := strconv.ParseFloat(pos.Upl, 64)
		leverage, _ := strconv.ParseFloat(pos.Lever, 64)
		liqPrice, _ := strconv.ParseFloat(pos.LiqPx, 64)

		// Convert symbol format
		symbol := t.convertSymbolBack(pos.InstId)

		// Determine direction and ensure contractCount is positive
		side := "long"
		if pos.PosSide == "short" {
			side = "short"
		}
		// OKX short position's pos is negative, need to take absolute value
		if contractCount < 0 {
			contractCount = -contractCount
		}

		// Convert contract count to actual position amount (in base asset)
		// positionAmt = contractCount * ctVal
		inst, err := t.getInstrument(symbol)
		posAmt := contractCount
		if err == nil && inst.CtVal > 0 {
			posAmt = contractCount * inst.CtVal
			logger.Debugf("  📊 OKX position %s: contracts=%.4f, ctVal=%.6f, posAmt=%.6f", symbol, contractCount, inst.CtVal, posAmt)
		}

		// Parse timestamps
		cTime, _ := strconv.ParseInt(pos.CTime, 10, 64)
		uTime, _ := strconv.ParseInt(pos.UTime, 10, 64)

		// 保证金模式：优先使用 API 返回值，其次使用缓存，最后默认 cross
		mgnMode := pos.MgnMode
		if mgnMode == "" {
			// 尝试从缓存获取（优先级：symbol_side_pending > symbol > 默认 cross）
			t.symbolMgnModesMutex.RLock()
			pendingKey := symbol + "_" + side + "_pending"
			if cached, ok := t.symbolMgnModes[pendingKey]; ok {
				mgnMode = cached
				logger.Debugf("  📝 OKX position %s %s mgnMode 使用 pending 缓存: %s", symbol, side, mgnMode)
			} else if cached, ok := t.symbolMgnModes[symbol]; ok {
				mgnMode = cached
				logger.Debugf("  📝 OKX position %s %s mgnMode 使用 symbol 缓存: %s", symbol, side, mgnMode)
			} else {
				mgnMode = "cross" // 默认全仓
				logger.Debugf("  ⚠️ OKX position %s %s mgnMode 为空，使用默认值: cross", symbol, side)
			}
			t.symbolMgnModesMutex.RUnlock()
		}

		// 记录已确认的仓位 mgnMode（用 symbol_side_mgnMode 作为精确 key）
		// 这样后续查询时可以精确获取每个仓位的 mgnMode
		positionKey := symbol + "_" + side + "_" + mgnMode
		t.symbolMgnModesMutex.Lock()
		t.symbolMgnModes[positionKey] = mgnMode
		t.symbolMgnModesMutex.Unlock()

		posMap := map[string]interface{}{
			"symbol":           symbol,
			"positionAmt":      posAmt,
			"entryPrice":       entryPrice,
			"markPrice":        markPrice,
			"unRealizedProfit": upl,
			"leverage":         leverage,
			"liquidationPrice": liqPrice,
			"side":             side,
			"mgnMode":          mgnMode,   // 保证金模式：cross/isolated
			"createdTime":      cTime,     // Position open time (ms)
			"updatedTime":      uTime,     // Position last update time (ms)
			"posId":            pos.PosId, // 仓位唯一标识（OKX 独有）
		}
		logger.Debugf("  📊 OKX position: %s %s mgnMode=%s posId=%s size=%.4f", symbol, side, mgnMode, pos.PosId, posAmt)
		result = append(result, posMap)
	}

	// Update cache
	t.positionsCacheMutex.Lock()
	t.cachedPositions = result
	t.positionsCacheTime = time.Now()
	t.positionsCacheMutex.Unlock()

	return result, nil
}

func (t *OKXTrader) invalidatePositionsCache() {
	t.positionsCacheMutex.Lock()
	t.cachedPositions = nil
	t.positionsCacheTime = time.Time{}
	t.positionsCacheMutex.Unlock()
}

// GetPositionsFresh is used by Copy Guard after an order mutation.
func (t *OKXTrader) GetPositionsFresh() ([]map[string]interface{}, error) {
	t.invalidatePositionsCache()
	return t.GetPositions()
}

// ensureMgnModeInPositions 确保持仓数据中的 mgnMode 不为空
// 用于处理从缓存返回数据时，可能存在的空 mgnMode 情况
func (t *OKXTrader) ensureMgnModeInPositions(positions []map[string]interface{}) []map[string]interface{} {
	result := make([]map[string]interface{}, len(positions))
	for i, pos := range positions {
		// 复制 map（避免修改原缓存数据）
		newPos := make(map[string]interface{})
		for k, v := range pos {
			newPos[k] = v
		}

		// 检查并修复空的 mgnMode
		mgnMode, _ := newPos["mgnMode"].(string)
		if mgnMode == "" {
			symbol, _ := newPos["symbol"].(string)
			side, _ := newPos["side"].(string)

			// 尝试从缓存获取（优先级：已确认的 > pending > symbol > 默认 cross）
			t.symbolMgnModesMutex.RLock()

			// 1. 尝试已确认的仓位 mgnMode（symbol_side_cross 或 symbol_side_isolated）
			crossKey := symbol + "_" + side + "_cross"
			isolatedKey := symbol + "_" + side + "_isolated"
			pendingKey := symbol + "_" + side + "_pending"

			if _, ok := t.symbolMgnModes[isolatedKey]; ok {
				mgnMode = "isolated"
				logger.Debugf("  📝 修复缓存 %s %s mgnMode 使用已确认 key: isolated", symbol, side)
			} else if _, ok := t.symbolMgnModes[crossKey]; ok {
				mgnMode = "cross"
				logger.Debugf("  📝 修复缓存 %s %s mgnMode 使用已确认 key: cross", symbol, side)
			} else if cached, ok := t.symbolMgnModes[pendingKey]; ok {
				// 2. 尝试 pending 缓存
				mgnMode = cached
				logger.Debugf("  📝 修复缓存 %s %s mgnMode 使用 pending: %s", symbol, side, mgnMode)
			} else if cached, ok := t.symbolMgnModes[symbol]; ok {
				// 3. 尝试 symbol 缓存
				mgnMode = cached
				logger.Debugf("  📝 修复缓存 %s %s mgnMode 使用 symbol: %s", symbol, side, mgnMode)
			} else {
				// 4. 默认 cross
				mgnMode = "cross"
				logger.Debugf("  ⚠️ 修复缓存 %s %s mgnMode 使用默认: cross", symbol, side)
			}
			t.symbolMgnModesMutex.RUnlock()

			newPos["mgnMode"] = mgnMode
		}

		result[i] = newPos
	}
	return result
}

// getInstrument gets instrument info
func (t *OKXTrader) getInstrument(symbol string) (*OKXInstrument, error) {
	instId := t.convertSymbol(symbol)

	// Check cache
	t.instrumentsCacheMutex.RLock()
	if inst, ok := t.instrumentsCache[instId]; ok && time.Since(t.instrumentsCacheTime) < 5*time.Minute {
		t.instrumentsCacheMutex.RUnlock()
		return inst, nil
	}
	t.instrumentsCacheMutex.RUnlock()

	// Get instrument info
	path := fmt.Sprintf("%s?instType=SWAP&instId=%s", okxInstrumentsPath, instId)
	data, err := t.doRequest("GET", path, nil)
	if err != nil {
		return nil, err
	}

	var instruments []struct {
		InstId   string `json:"instId"`
		CtVal    string `json:"ctVal"`
		CtMult   string `json:"ctMult"`
		LotSz    string `json:"lotSz"`
		MinSz    string `json:"minSz"`
		MaxMktSz string `json:"maxMktSz"` // Maximum market order size
		TickSz   string `json:"tickSz"`
		CtType   string `json:"ctType"`
	}

	if err := json.Unmarshal(data, &instruments); err != nil {
		return nil, err
	}

	if len(instruments) == 0 {
		return nil, fmt.Errorf("instrument info not found: %s", instId)
	}

	inst := instruments[0]
	ctVal, _ := strconv.ParseFloat(inst.CtVal, 64)
	ctMult, _ := strconv.ParseFloat(inst.CtMult, 64)
	lotSz, _ := strconv.ParseFloat(inst.LotSz, 64)
	minSz, _ := strconv.ParseFloat(inst.MinSz, 64)
	maxMktSz, _ := strconv.ParseFloat(inst.MaxMktSz, 64)
	tickSz, _ := strconv.ParseFloat(inst.TickSz, 64)

	instrument := &OKXInstrument{
		InstID:   inst.InstId,
		CtVal:    ctVal,
		CtMult:   ctMult,
		LotSz:    lotSz,
		MinSz:    minSz,
		MaxMktSz: maxMktSz,
		TickSz:   tickSz,
		CtType:   inst.CtType,
	}

	// Update cache
	t.instrumentsCacheMutex.Lock()
	t.instrumentsCache[instId] = instrument
	t.instrumentsCacheTime = time.Now()
	t.instrumentsCacheMutex.Unlock()

	return instrument, nil
}

// SetMarginMode sets margin mode for a symbol.
//
// 重要说明（PR-5 / 修复 H）：OKX 的"逐仓 / 全仓"在开仓 API 中通过 `tdMode` 参数
// 直接指定，不依赖账户级或合约级的全局设置。OKX 统一账户（Unified Account，
// 当前线上跟单账户的主流形态）下也没有暴露逐合约切换保证金模式的官方 API；
// 原本调用的 `/api/v5/account/set-isolated-mode` 端点在统一账户下会返回
// `code=51000, msg=Parameter type error`，旧实现虽吞掉了错误，但每 3s 跟单
// 轮询都会触发，产生大量噪声日志、误导排查。
//
// 实际行为：
//   - 维护本地缓存（isCrossMargin、symbolMgnModes、pending 标记、cachedPositions 清空）
//   - 不再发起任何 HTTP 请求，由开仓 API 的 tdMode 在订单上精确生效
//
// 副作用对比（修复前 vs 修复后）：
//   - 缓存语义：完全一致
//   - 跟单准确性：完全一致（依然由 tdMode 在订单上生效）
//   - 日志噪声：消失（OKX 51000 错误不再每 3s 出现）
//   - 网络成本：每次开仓少一次失败 HTTP 调用
func (t *OKXTrader) SetMarginMode(symbol string, isCrossMargin bool) error {
	mgnMode := "isolated"
	if isCrossMargin {
		mgnMode = "cross"
	}

	t.isCrossMargin = isCrossMargin

	t.symbolMgnModesMutex.Lock()
	t.symbolMgnModes[symbol] = mgnMode
	t.symbolMgnModes[symbol+"_long_pending"] = mgnMode
	t.symbolMgnModes[symbol+"_short_pending"] = mgnMode
	t.symbolMgnModesMutex.Unlock()

	// 缓存有效性判断基于 positionsCacheTime，必须走统一失效入口（同时清时间戳）
	t.invalidatePositionsCache()

	logger.Debugf("  📝 [OKX] 缓存 %s 待开仓保证金模式: %s（不调用 set-isolated-mode，依赖订单 tdMode）",
		symbol, mgnMode)
	return nil
}

// getMgnMode 获取当前保证金模式字符串
func (t *OKXTrader) getMgnMode() string {
	if t.isCrossMargin {
		return "cross"
	}
	return "isolated"
}

// SetLeverage sets leverage for both directions (used for general cases)
func (t *OKXTrader) SetLeverage(symbol string, leverage int) error {
	instId := t.convertSymbol(symbol)

	// Set leverage for both long and short
	for _, posSide := range []string{"long", "short"} {
		body := map[string]interface{}{
			"instId":  instId,
			"lever":   strconv.Itoa(leverage),
			"mgnMode": t.getMgnMode(), // 使用当前设置的保证金模式
			"posSide": posSide,
		}

		_, err := t.doRequest("POST", okxLeveragePath, body)
		if err != nil {
			// Ignore if already at target leverage
			if strings.Contains(err.Error(), "same") {
				continue
			}
			logger.Infof("  ⚠️ Failed to set %s %s leverage: %v", symbol, posSide, err)
		}
	}

	logger.Infof("  ✓ %s leverage set to %dx", symbol, leverage)
	return nil
}

// setLeverageForSide sets leverage for a specific direction only
// This prevents overwriting leverage of existing positions in the opposite direction
func (t *OKXTrader) setLeverageForSide(symbol string, leverage int, posSide string) error {
	instId := t.convertSymbol(symbol)

	body := map[string]interface{}{
		"instId":  instId,
		"lever":   strconv.Itoa(leverage),
		"mgnMode": t.getMgnMode(), // 使用当前设置的保证金模式
		"posSide": posSide,
	}

	_, err := t.doRequest("POST", okxLeveragePath, body)
	if err != nil {
		// Ignore if already at target leverage
		if strings.Contains(err.Error(), "same") {
			logger.Infof("  ✓ %s %s leverage already at %dx", symbol, posSide, leverage)
			return nil
		}
		return fmt.Errorf("failed to set %s %s leverage: %w", symbol, posSide, err)
	}

	logger.Infof("  ✓ %s %s leverage set to %dx", symbol, posSide, leverage)
	return nil
}

// OpenLong opens long position
func (t *OKXTrader) OpenLong(symbol string, quantity float64, leverage int) (map[string]interface{}, error) {
	return t.openLong(symbol, quantity, leverage, true, "")
}

func (t *OKXTrader) OpenLongPreservingOrders(symbol string, quantity float64, leverage int) (map[string]interface{}, error) {
	return t.openLong(symbol, quantity, leverage, false, "")
}

func (t *OKXTrader) OpenLongPreservingOrdersWithClientID(symbol string, quantity float64, leverage int, clientOrderID string) (map[string]interface{}, error) {
	return t.openLong(symbol, quantity, leverage, false, clientOrderID)
}

func (t *OKXTrader) openLong(symbol string, quantity float64, leverage int, cancelExisting bool, clientOrderID string) (map[string]interface{}, error) {
	// Cancel old orders
	if cancelExisting {
		t.CancelAllOrders(symbol)
	}

	// Set leverage for long direction only (don't affect existing short positions)
	if err := t.setLeverageForSide(symbol, leverage, "long"); err != nil {
		logger.Infof("  ⚠️ Failed to set leverage: %v", err)
	}

	instId := t.convertSymbol(symbol)

	// Get instrument info and calculate contract size
	inst, err := t.getInstrument(symbol)
	if err != nil {
		return nil, fmt.Errorf("failed to get instrument info: %w", err)
	}

	// OKX uses contract count, need to convert quantity (in base asset) to contract count
	// sz = quantity / ctVal (number of contracts = asset amount / asset per contract)
	sz := quantity / inst.CtVal
	szStr := t.formatSize(sz, inst)

	logger.Infof("  📊 OKX OpenLong: quantity=%.6f, ctVal=%.6f, contracts=%.2f", quantity, inst.CtVal, sz)

	// Check max market order size limit
	if inst.MaxMktSz > 0 && sz > inst.MaxMktSz {
		logger.Infof("  ⚠️ OKX market order size %.2f exceeds max %.2f, reducing to max", sz, inst.MaxMktSz)
		sz = inst.MaxMktSz
		szStr = t.formatSize(sz, inst)
	}

	if clientOrderID == "" {
		clientOrderID = genOkxClOrdID()
	}
	body := map[string]interface{}{
		"instId":  instId,
		"tdMode":  t.getMgnMode(), // 使用当前设置的保证金模式
		"side":    "buy",
		"posSide": "long",
		"ordType": "market",
		"sz":      szStr,
		"clOrdId": clientOrderID,
		"tag":     okxTag,
	}

	data, err := t.doRequest("POST", okxOrderPath, body)
	if err != nil {
		return nil, fmt.Errorf("failed to open long position: %w", err)
	}

	var orders []struct {
		OrdId   string `json:"ordId"`
		ClOrdId string `json:"clOrdId"`
		SCode   string `json:"sCode"`
		SMsg    string `json:"sMsg"`
	}

	if err := json.Unmarshal(data, &orders); err != nil {
		return nil, fmt.Errorf("failed to parse order response: %w", err)
	}

	if len(orders) == 0 || orders[0].SCode != "0" {
		msg := "unknown error"
		if len(orders) > 0 {
			msg = orders[0].SMsg
		}
		return nil, fmt.Errorf("failed to open long position: %s", msg)
	}

	logger.Infof("✓ OKX opened long position successfully: %s size: %s", symbol, szStr)
	logger.Infof("  Order ID: %s", orders[0].OrdId)
	t.invalidatePositionsCache()

	return map[string]interface{}{
		"orderId": orders[0].OrdId,
		"clOrdId": orders[0].ClOrdId,
		"symbol":  symbol,
		"status":  "FILLED",
	}, nil
}

// OpenShort opens short position
func (t *OKXTrader) OpenShort(symbol string, quantity float64, leverage int) (map[string]interface{}, error) {
	return t.openShort(symbol, quantity, leverage, true, "")
}

func (t *OKXTrader) OpenShortPreservingOrders(symbol string, quantity float64, leverage int) (map[string]interface{}, error) {
	return t.openShort(symbol, quantity, leverage, false, "")
}

func (t *OKXTrader) OpenShortPreservingOrdersWithClientID(symbol string, quantity float64, leverage int, clientOrderID string) (map[string]interface{}, error) {
	return t.openShort(symbol, quantity, leverage, false, clientOrderID)
}

func (t *OKXTrader) openShort(symbol string, quantity float64, leverage int, cancelExisting bool, clientOrderID string) (map[string]interface{}, error) {
	// Cancel old orders
	if cancelExisting {
		t.CancelAllOrders(symbol)
	}

	// Set leverage for short direction only (don't affect existing long positions)
	if err := t.setLeverageForSide(symbol, leverage, "short"); err != nil {
		logger.Infof("  ⚠️ Failed to set leverage: %v", err)
	}

	instId := t.convertSymbol(symbol)

	// Get instrument info and calculate contract size
	inst, err := t.getInstrument(symbol)
	if err != nil {
		return nil, fmt.Errorf("failed to get instrument info: %w", err)
	}

	// OKX uses contract count, need to convert quantity (in base asset) to contract count
	// sz = quantity / ctVal (number of contracts = asset amount / asset per contract)
	sz := quantity / inst.CtVal
	szStr := t.formatSize(sz, inst)

	logger.Infof("  📊 OKX OpenShort: quantity=%.6f, ctVal=%.6f, contracts=%.2f", quantity, inst.CtVal, sz)

	// Check max market order size limit
	if inst.MaxMktSz > 0 && sz > inst.MaxMktSz {
		logger.Infof("  ⚠️ OKX market order size %.2f exceeds max %.2f, reducing to max", sz, inst.MaxMktSz)
		sz = inst.MaxMktSz
		szStr = t.formatSize(sz, inst)
	}

	if clientOrderID == "" {
		clientOrderID = genOkxClOrdID()
	}
	body := map[string]interface{}{
		"instId":  instId,
		"tdMode":  t.getMgnMode(), // 使用当前设置的保证金模式
		"side":    "sell",
		"posSide": "short",
		"ordType": "market",
		"sz":      szStr,
		"clOrdId": clientOrderID,
		"tag":     okxTag,
	}

	data, err := t.doRequest("POST", okxOrderPath, body)
	if err != nil {
		return nil, fmt.Errorf("failed to open short position: %w", err)
	}

	var orders []struct {
		OrdId   string `json:"ordId"`
		ClOrdId string `json:"clOrdId"`
		SCode   string `json:"sCode"`
		SMsg    string `json:"sMsg"`
	}

	if err := json.Unmarshal(data, &orders); err != nil {
		return nil, fmt.Errorf("failed to parse order response: %w", err)
	}

	if len(orders) == 0 || orders[0].SCode != "0" {
		msg := "unknown error"
		if len(orders) > 0 {
			msg = orders[0].SMsg
		}
		return nil, fmt.Errorf("failed to open short position: %s", msg)
	}

	logger.Infof("✓ OKX opened short position successfully: %s size: %s", symbol, szStr)
	logger.Infof("  Order ID: %s", orders[0].OrdId)

	t.invalidatePositionsCache()
	return map[string]interface{}{
		"orderId": orders[0].OrdId,
		"clOrdId": orders[0].ClOrdId,
		"symbol":  symbol,
		"status":  "FILLED",
	}, nil
}

// CloseLong closes long position
func (t *OKXTrader) CloseLong(symbol string, quantity float64) (map[string]interface{}, error) {
	return t.closeLong(symbol, quantity, true)
}

func (t *OKXTrader) CloseLongPreservingOrders(symbol string, quantity float64) (map[string]interface{}, error) {
	return t.closeLong(symbol, quantity, false)
}

func (t *OKXTrader) closeLong(symbol string, quantity float64, cancelExisting bool) (map[string]interface{}, error) {
	instId := t.convertSymbol(symbol)

	// Get instrument info for contract conversion
	inst, err := t.getInstrument(symbol)
	if err != nil {
		return nil, fmt.Errorf("failed to get instrument info: %w", err)
	}

	// 🔑 posId 方案：使用已设置的 marginMode 筛选仓位
	tdMode := t.getMgnMode()
	positions, err := t.GetPositions()
	if err != nil {
		return nil, err
	}
	logger.Infof("🔍 OKX CloseLong searching positions: symbol=%s, targetMgnMode=%s, count=%d", symbol, tdMode, len(positions))
	for _, pos := range positions {
		if pos["symbol"] == symbol && pos["side"] == "long" {
			posMgnMode, _ := pos["mgnMode"].(string)
			// 精确匹配 marginMode（posId 方案核心）
			if posMgnMode != tdMode {
				logger.Infof("🔍 Skip long position: mgnMode=%s (want %s)", posMgnMode, tdMode)
				continue
			}
			if quantity == 0 {
				quantity = pos["positionAmt"].(float64)
			}
			logger.Infof("📊 Found matching long position: mgnMode=%s, quantity=%.4f", posMgnMode, quantity)
			break
		}
	}
	if quantity == 0 {
		return nil, fmt.Errorf("long position not found for %s (mgnMode=%s)", symbol, tdMode)
	}

	// Convert quantity (base asset) to contract count
	// contracts = quantity / ctVal
	contracts := quantity / inst.CtVal
	szStr := t.formatSize(contracts, inst)

	logger.Infof("🔻 OKX close long: symbol=%s, quantity=%.6f, ctVal=%.6f, contracts=%.2f, szStr=%s",
		symbol, quantity, inst.CtVal, contracts, szStr)

	body := map[string]interface{}{
		"instId":  instId,
		"tdMode":  tdMode, // 使用仓位实际的保证金模式
		"side":    "sell",
		"posSide": "long",
		"ordType": "market",
		"sz":      szStr,
		"clOrdId": genOkxClOrdID(),
		"tag":     okxTag,
	}

	data, err := t.doRequest("POST", okxOrderPath, body)
	if err != nil {
		return nil, fmt.Errorf("failed to close long position: %w", err)
	}

	var orders []struct {
		OrdId string `json:"ordId"`
		SCode string `json:"sCode"`
		SMsg  string `json:"sMsg"`
	}

	if err := json.Unmarshal(data, &orders); err != nil {
		return nil, err
	}

	if len(orders) == 0 || orders[0].SCode != "0" {
		msg := "unknown error"
		if len(orders) > 0 {
			msg = orders[0].SMsg
		}
		return nil, fmt.Errorf("failed to close long position: %s", msg)
	}

	logger.Infof("✓ OKX closed long position successfully: %s", symbol)

	t.invalidatePositionsCache()
	if cancelExisting {
		t.CancelAllOrders(symbol)
	}

	return map[string]interface{}{
		"orderId": orders[0].OrdId,
		"symbol":  symbol,
		"status":  "FILLED",
	}, nil
}

// CloseShort closes short position
func (t *OKXTrader) CloseShort(symbol string, quantity float64) (map[string]interface{}, error) {
	return t.closeShort(symbol, quantity, true)
}

func (t *OKXTrader) CloseShortPreservingOrders(symbol string, quantity float64) (map[string]interface{}, error) {
	return t.closeShort(symbol, quantity, false)
}

func (t *OKXTrader) closeShort(symbol string, quantity float64, cancelExisting bool) (map[string]interface{}, error) {
	instId := t.convertSymbol(symbol)

	// Get instrument info for contract conversion
	inst, err := t.getInstrument(symbol)
	if err != nil {
		return nil, fmt.Errorf("failed to get instrument info: %w", err)
	}

	// 🔑 posId 方案：使用已设置的 marginMode 筛选仓位
	tdMode := t.getMgnMode()
	positions, err := t.GetPositions()
	if err != nil {
		return nil, err
	}
	logger.Infof("🔍 OKX CloseShort searching positions: symbol=%s, targetMgnMode=%s, count=%d", symbol, tdMode, len(positions))
	for _, pos := range positions {
		if pos["symbol"] == symbol && pos["side"] == "short" {
			posMgnMode, _ := pos["mgnMode"].(string)
			// 精确匹配 marginMode（posId 方案核心）
			if posMgnMode != tdMode {
				logger.Infof("🔍 Skip short position: mgnMode=%s (want %s)", posMgnMode, tdMode)
				continue
			}
			if quantity == 0 {
				quantity = pos["positionAmt"].(float64)
			}
			logger.Infof("📊 Found matching short position: mgnMode=%s, quantity=%.4f", posMgnMode, quantity)
			break
		}
	}
	if quantity == 0 {
		return nil, fmt.Errorf("short position not found for %s (mgnMode=%s)", symbol, tdMode)
	}

	// Ensure quantity is positive (OKX sz parameter must be positive)
	if quantity < 0 {
		quantity = -quantity
	}

	// Convert quantity (base asset) to contract count
	// contracts = quantity / ctVal
	contracts := quantity / inst.CtVal
	szStr := t.formatSize(contracts, inst)

	logger.Infof("🔻 OKX close short: symbol=%s, quantity=%.6f, ctVal=%.6f, contracts=%.2f, szStr=%s",
		symbol, quantity, inst.CtVal, contracts, szStr)

	body := map[string]interface{}{
		"instId":  instId,
		"tdMode":  tdMode, // 使用仓位实际的保证金模式
		"side":    "buy",
		"posSide": "short",
		"ordType": "market",
		"sz":      szStr,
		"clOrdId": genOkxClOrdID(),
		"tag":     okxTag,
	}

	logger.Infof("🔻 OKX close short request body: %+v", body)

	data, err := t.doRequest("POST", okxOrderPath, body)
	if err != nil {
		return nil, fmt.Errorf("failed to close short position: %w", err)
	}

	var orders []struct {
		OrdId string `json:"ordId"`
		SCode string `json:"sCode"`
		SMsg  string `json:"sMsg"`
	}

	if err := json.Unmarshal(data, &orders); err != nil {
		return nil, err
	}

	if len(orders) == 0 || orders[0].SCode != "0" {
		msg := "unknown error"
		if len(orders) > 0 {
			msg = fmt.Sprintf("sCode=%s, sMsg=%s", orders[0].SCode, orders[0].SMsg)
		}
		logger.Infof("❌ OKX failed to close short position: %s, response: %s", msg, string(data))
		return nil, fmt.Errorf("failed to close short position: %s", msg)
	}

	logger.Infof("✓ OKX closed short position successfully: %s, ordId=%s", symbol, orders[0].OrdId)

	t.invalidatePositionsCache()
	if cancelExisting {
		t.CancelAllOrders(symbol)
	}

	return map[string]interface{}{
		"orderId": orders[0].OrdId,
		"symbol":  symbol,
		"status":  "FILLED",
	}, nil
}

// GetMarketPrice gets market price
func (t *OKXTrader) GetMarketPrice(symbol string) (float64, error) {
	instId := t.convertSymbol(symbol)
	path := fmt.Sprintf("%s?instId=%s", okxTickerPath, instId)

	data, err := t.doRequest("GET", path, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to get price: %w", err)
	}

	var tickers []struct {
		Last string `json:"last"`
	}

	if err := json.Unmarshal(data, &tickers); err != nil {
		return 0, err
	}

	if len(tickers) == 0 {
		return 0, fmt.Errorf("no price data received")
	}

	price, err := strconv.ParseFloat(tickers[0].Last, 64)
	if err != nil {
		return 0, err
	}

	return price, nil
}

// SetStopLoss sets stop loss order
func (t *OKXTrader) SetStopLoss(symbol string, positionSide string, quantity, stopPrice float64) error {
	instId := t.convertSymbol(symbol)

	// Get instrument info
	inst, err := t.getInstrument(symbol)
	if err != nil {
		return fmt.Errorf("failed to get instrument info: %w", err)
	}

	// Calculate contract size: quantity (in base asset) / ctVal (asset per contract)
	sz := quantity / inst.CtVal
	szStr := t.formatSize(sz, inst)

	// Determine direction
	side := "sell"
	posSide := "long"
	if strings.ToUpper(positionSide) == "SHORT" {
		side = "buy"
		posSide = "short"
	}

	body := map[string]interface{}{
		"instId":      instId,
		"tdMode":      t.getMgnMode(), // 使用当前设置的保证金模式
		"side":        side,
		"posSide":     posSide,
		"ordType":     "conditional",
		"sz":          szStr,
		"slTriggerPx": fmt.Sprintf("%.8f", stopPrice),
		"slOrdPx":     "-1", // Market price
		"tag":         okxTag,
	}

	_, err = t.doRequest("POST", okxAlgoOrderPath, body)
	if err != nil {
		return fmt.Errorf("failed to set stop loss: %w", err)
	}

	logger.Infof("  Stop loss price set: %.4f", stopPrice)
	return nil
}

// SetTakeProfit sets take profit order
func (t *OKXTrader) SetTakeProfit(symbol string, positionSide string, quantity, takeProfitPrice float64) error {
	instId := t.convertSymbol(symbol)

	// Get instrument info
	inst, err := t.getInstrument(symbol)
	if err != nil {
		return fmt.Errorf("failed to get instrument info: %w", err)
	}

	// Calculate contract size: quantity (in base asset) / ctVal (asset per contract)
	sz := quantity / inst.CtVal
	szStr := t.formatSize(sz, inst)

	// Determine direction
	side := "sell"
	posSide := "long"
	if strings.ToUpper(positionSide) == "SHORT" {
		side = "buy"
		posSide = "short"
	}

	body := map[string]interface{}{
		"instId":      instId,
		"tdMode":      t.getMgnMode(), // 使用当前设置的保证金模式
		"side":        side,
		"posSide":     posSide,
		"ordType":     "conditional",
		"sz":          szStr,
		"tpTriggerPx": fmt.Sprintf("%.8f", takeProfitPrice),
		"tpOrdPx":     "-1", // Market price
		"tag":         okxTag,
	}

	_, err = t.doRequest("POST", okxAlgoOrderPath, body)
	if err != nil {
		return fmt.Errorf("failed to set take profit: %w", err)
	}

	logger.Infof("  Take profit price set: %.4f", takeProfitPrice)
	return nil
}

// CancelStopLossOrders cancels stop loss orders
func (t *OKXTrader) CancelStopLossOrders(symbol string) error {
	return t.cancelAlgoOrders(symbol, "sl")
}

// CancelTakeProfitOrders cancels take profit orders
func (t *OKXTrader) CancelTakeProfitOrders(symbol string) error {
	return t.cancelAlgoOrders(symbol, "tp")
}

// cancelAlgoOrders cancels algo orders
func (t *OKXTrader) cancelAlgoOrders(symbol string, orderType string) error {
	instId := t.convertSymbol(symbol)

	// Get pending algo orders
	path := fmt.Sprintf("%s?instType=SWAP&instId=%s&ordType=conditional", okxAlgoPendingPath, instId)
	data, err := t.doRequest("GET", path, nil)
	if err != nil {
		return err
	}

	var orders []struct {
		AlgoId      string `json:"algoId"`
		InstId      string `json:"instId"`
		SlTriggerPx string `json:"slTriggerPx"`
		TpTriggerPx string `json:"tpTriggerPx"`
	}

	if err := json.Unmarshal(data, &orders); err != nil {
		return err
	}

	canceledCount := 0
	for _, order := range orders {
		if orderType == "sl" && order.SlTriggerPx == "" {
			continue
		}
		if orderType == "tp" && order.TpTriggerPx == "" {
			continue
		}
		body := []map[string]interface{}{
			{
				"algoId": order.AlgoId,
				"instId": order.InstId,
			},
		}

		_, err := t.doRequest("POST", okxCancelAlgoPath, body)
		if err != nil {
			logger.Infof("  ⚠️ Failed to cancel algo order: %v", err)
			continue
		}
		canceledCount++
	}

	if canceledCount > 0 {
		logger.Infof("  ✓ Canceled %d algo orders for %s", canceledCount, symbol)
	}

	return nil
}

// CancelAllOrders cancels all pending orders
func (t *OKXTrader) CancelAllOrders(symbol string) error {
	instId := t.convertSymbol(symbol)

	// Get pending orders
	path := fmt.Sprintf("%s?instType=SWAP&instId=%s", okxPendingOrdersPath, instId)
	data, err := t.doRequest("GET", path, nil)
	if err != nil {
		return err
	}

	var orders []struct {
		OrdId  string `json:"ordId"`
		InstId string `json:"instId"`
	}

	if err := json.Unmarshal(data, &orders); err != nil {
		return err
	}

	// Batch cancel
	for _, order := range orders {
		body := map[string]interface{}{
			"instId": order.InstId,
			"ordId":  order.OrdId,
		}
		t.doRequest("POST", okxCancelOrderPath, body)
	}

	// Also cancel algo orders
	t.cancelAlgoOrders(symbol, "")

	if len(orders) > 0 {
		logger.Infof("  ✓ Canceled all pending orders for %s", symbol)
	}

	return nil
}

// CancelStopOrders cancels stop loss and take profit orders
func (t *OKXTrader) CancelStopOrders(symbol string) error {
	return t.cancelAlgoOrders(symbol, "")
}

// FormatQuantity formats quantity (converts base asset quantity to contract count)
func (t *OKXTrader) FormatQuantity(symbol string, quantity float64) (string, error) {
	inst, err := t.getInstrument(symbol)
	if err != nil {
		return fmt.Sprintf("%.3f", quantity), nil
	}

	// OKX uses contract count: quantity (in base asset) / ctVal (asset per contract)
	sz := quantity / inst.CtVal
	return t.formatSize(sz, inst), nil
}

// formatSize formats contract size
func (t *OKXTrader) formatSize(sz float64, inst *OKXInstrument) string {
	// Determine precision based on lotSz
	if inst.LotSz >= 1 {
		// 整数张合约要求：当 sz 在 (0, 1) 区间时，向上取整到 1（最小单位）
		// 这确保减仓操作能执行，避免 "Parameter sz error"
		if sz > 0 && sz < 1 {
			logger.Infof("  ⚠️ 合约数 %.2f 不足 1 张，向上取整到 1（最小单位）", sz)
			return "1"
		}
		return fmt.Sprintf("%.0f", sz)
	}

	// Calculate decimal places
	lotSzStr := fmt.Sprintf("%f", inst.LotSz)
	dotIndex := strings.Index(lotSzStr, ".")
	if dotIndex == -1 {
		return fmt.Sprintf("%.0f", sz)
	}

	// Remove trailing zeros
	lotSzStr = strings.TrimRight(lotSzStr, "0")
	precision := len(lotSzStr) - dotIndex - 1

	// 小数精度时：如果 sz > 0 但格式化后可能为 "0"，也需要处理
	formatted := fmt.Sprintf(fmt.Sprintf("%%.%df", precision), sz)
	if formatted == "0" && sz > 0 {
		// 向上取整到最小单位 lotSz
		logger.Infof("  ⚠️ 合约数 %.6f 不足最小单位 %.6f，向上取整", sz, inst.LotSz)
		return fmt.Sprintf(fmt.Sprintf("%%.%df", precision), inst.LotSz)
	}

	return formatted
}

// GetOrderStatus gets order status
func (t *OKXTrader) GetOrderStatus(symbol string, orderID string) (map[string]interface{}, error) {
	instId := t.convertSymbol(symbol)
	path := fmt.Sprintf("/api/v5/trade/order?instId=%s&ordId=%s", instId, orderID)
	return t.getOrderStatus(symbol, path)
}

func (t *OKXTrader) GetOrderStatusByClientID(symbol, clientOrderID string) (map[string]interface{}, error) {
	instID := t.convertSymbol(symbol)
	path := fmt.Sprintf("/api/v5/trade/order?instId=%s&clOrdId=%s", instID, clientOrderID)
	return t.getOrderStatus(symbol, path)
}

func (t *OKXTrader) getOrderStatus(symbol, path string) (map[string]interface{}, error) {
	data, err := t.doRequest("GET", path, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get order status: %w", err)
	}

	var orders []struct {
		OrdId     string `json:"ordId"`
		State     string `json:"state"`
		AvgPx     string `json:"avgPx"`
		AccFillSz string `json:"accFillSz"`
		Fee       string `json:"fee"`
		Side      string `json:"side"`
		OrdType   string `json:"ordType"`
		CTime     string `json:"cTime"`
		UTime     string `json:"uTime"`
	}

	if err := json.Unmarshal(data, &orders); err != nil {
		return nil, err
	}

	if len(orders) == 0 {
		return nil, fmt.Errorf("order not found")
	}

	order := orders[0]
	avgPrice, _ := strconv.ParseFloat(order.AvgPx, 64)
	fillSz, _ := strconv.ParseFloat(order.AccFillSz, 64) // This is in contracts
	fee, _ := strconv.ParseFloat(order.Fee, 64)
	cTime, _ := strconv.ParseInt(order.CTime, 10, 64)
	uTime, _ := strconv.ParseInt(order.UTime, 10, 64)

	// Convert contract count to base asset quantity
	// executedQty = contracts * ctVal
	executedQty := fillSz
	inst, err := t.getInstrument(symbol)
	if err == nil && inst.CtVal > 0 {
		executedQty = fillSz * inst.CtVal
		logger.Debugf("  📊 OKX order %s: fillSz(contracts)=%.4f, ctVal=%.6f, executedQty=%.6f", order.OrdId, fillSz, inst.CtVal, executedQty)
	}

	// Status mapping
	statusMap := map[string]string{
		"filled":           "FILLED",
		"live":             "NEW",
		"partially_filled": "PARTIALLY_FILLED",
		"canceled":         "CANCELED",
	}

	status := statusMap[order.State]
	if status == "" {
		status = order.State
	}

	return map[string]interface{}{
		"orderId":     order.OrdId,
		"symbol":      symbol,
		"status":      status,
		"avgPrice":    avgPrice,
		"executedQty": executedQty,
		"side":        order.Side,
		"type":        order.OrdType,
		"time":        cTime,
		"updateTime":  uTime,
		"commission":  -fee, // OKX returns negative value
	}, nil
}

// OKX order tag
var okxTag = func() string {
	b, _ := base64.StdEncoding.DecodeString("NGMzNjNjODFlZGM1QkNERQ==")
	return string(b)
}()

// GetClosedPnL retrieves closed position PnL records from OKX
// OKX API: /api/v5/account/positions-history
func (t *OKXTrader) GetClosedPnL(startTime time.Time, limit int) ([]ClosedPnLRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 100 {
		limit = 100
	}

	// Build query path with parameters
	path := fmt.Sprintf("/api/v5/account/positions-history?instType=SWAP&limit=%d", limit)
	if !startTime.IsZero() {
		// OKX positions-history uses "before" for records newer than the
		// supplied uTime and "after" for older records. Copy Guard needs
		// freshly closed positions after openedAt, so "after" would exclude
		// exactly the records we are trying to reconcile.
		path += fmt.Sprintf("&before=%d", startTime.UnixMilli())
	}
	return t.getClosedPnLFromPath(path)
}

func (t *OKXTrader) GetClosedPnLByPositionID(symbol, posID string, limit int) ([]ClosedPnLRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 100 {
		limit = 100
	}
	if strings.TrimSpace(posID) == "" {
		return nil, fmt.Errorf("position id is required")
	}
	path := fmt.Sprintf("/api/v5/account/positions-history?instType=SWAP&posId=%s&limit=%d", posID, limit)
	if strings.TrimSpace(symbol) != "" {
		path += fmt.Sprintf("&instId=%s", t.convertSymbol(symbol))
	}
	return t.getClosedPnLFromPath(path)
}

func (t *OKXTrader) getClosedPnLFromPath(path string) ([]ClosedPnLRecord, error) {
	data, err := t.doRequest("GET", path, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get positions history: %w", err)
	}

	// doRequest 已解包外层 {code, msg, data}，这里直接解析 data 数组
	var positions []struct {
		InstID        string `json:"instId"`        // Instrument ID (e.g., "BTC-USDT-SWAP")
		Direction     string `json:"direction"`     // Position direction: "long" or "short"
		OpenAvgPx     string `json:"openAvgPx"`     // Average open price
		CloseAvgPx    string `json:"closeAvgPx"`    // Average close price
		CloseTotalPos string `json:"closeTotalPos"` // Closed position quantity
		RealizedPnl   string `json:"realizedPnl"`   // Realized PnL
		Pnl           string `json:"pnl"`
		Fee           string `json:"fee"`        // Total fee
		FundingFee    string `json:"fundingFee"` // Funding fee
		LiqPenalty    string `json:"liqPenalty"`
		Lever         string `json:"lever"` // Leverage
		MgnMode       string `json:"mgnMode"`
		CTime         string `json:"cTime"` // Position open time
		UTime         string `json:"uTime"` // Position close time
		Type          string `json:"type"`  // Close type: 1=close position, 2=partial close, 3=liquidation, 4=partial liquidation
		PosId         string `json:"posId"` // Position ID
	}

	if err := json.Unmarshal(data, &positions); err != nil {
		return nil, fmt.Errorf("failed to parse positions history: %w", err)
	}

	records := make([]ClosedPnLRecord, 0, len(positions))

	for _, pos := range positions {
		record := ClosedPnLRecord{}

		// Convert instrument ID to standard format (BTC-USDT-SWAP -> BTCUSDT)
		parts := strings.Split(pos.InstID, "-")
		if len(parts) >= 2 {
			record.Symbol = parts[0] + parts[1]
		} else {
			record.Symbol = pos.InstID
		}

		// Side
		record.Side = pos.Direction // OKX already returns "long" or "short"

		// Prices
		record.EntryPrice, _ = strconv.ParseFloat(pos.OpenAvgPx, 64)
		record.ExitPrice, _ = strconv.ParseFloat(pos.CloseAvgPx, 64)

		// Quantity: closeTotalPos is in contracts; also expose the coin
		// amount so callers comparing against coin-denominated sizes
		// (e.g. Copy Guard attempts) do not mismatch by a ctVal factor.
		record.Quantity, _ = strconv.ParseFloat(pos.CloseTotalPos, 64)
		if inst, instErr := t.getInstrument(record.Symbol); instErr == nil && inst.CtVal > 0 {
			record.QuantityCoins = record.Quantity * inst.CtVal
		}

		// PnL
		record.RealizedPnL, _ = strconv.ParseFloat(pos.RealizedPnl, 64)

		// Fee
		fee, _ := strconv.ParseFloat(pos.Fee, 64)
		fundingFee, _ := strconv.ParseFloat(pos.FundingFee, 64)
		record.Fee = math.Abs(fee) // display as a positive cost; RealizedPnL already includes it
		record.FundingFee = fundingFee
		penalty, _ := strconv.ParseFloat(pos.LiqPenalty, 64)
		record.LiquidationPenalty = math.Abs(penalty)
		record.GrossPnL, _ = strconv.ParseFloat(pos.Pnl, 64)

		// Leverage
		lev, _ := strconv.ParseFloat(pos.Lever, 64)
		record.Leverage = int(lev)
		record.MarginMode = pos.MgnMode

		// Times
		cTime, _ := strconv.ParseInt(pos.CTime, 10, 64)
		uTime, _ := strconv.ParseInt(pos.UTime, 10, 64)
		record.EntryTime = time.UnixMilli(cTime)
		record.ExitTime = time.UnixMilli(uTime)

		// Close type
		switch pos.Type {
		case "1", "2":
			record.CloseType = "unknown" // Could be manual or AI, need to cross-reference
		case "3", "4":
			record.CloseType = "liquidation"
		default:
			record.CloseType = "unknown"
		}

		// Exchange ID
		record.ExchangeID = pos.PosId

		records = append(records, record)
	}

	return records, nil
}
