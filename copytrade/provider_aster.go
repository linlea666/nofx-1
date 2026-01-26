package copytrade

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"nofx/logger"
)

// ============================================================================
// AsterDex Provider
// ============================================================================

const (
	// AsterDexExplorerAPI AsterDex 浏览器 API 地址
	// 测试网: https://explorer.asterdex-testnet.com/explorer
	// 主网: https://explorer.asterdex.com/explorer (待确认)
	AsterDexExplorerAPI = "https://explorer.asterdex-testnet.com/explorer"
)

// AsterDexProvider AsterDex 数据提供者
type AsterDexProvider struct {
	client       *http.Client
	apiEndpoint  string
	lastTxTime   int64  // 上次处理的交易时间戳（避免重复处理）
	lastTxHash   string // 上次处理的交易哈希（双重校验）
}

// NewAsterDexProvider 创建 AsterDex Provider
func NewAsterDexProvider() *AsterDexProvider {
	return &AsterDexProvider{
		client: &http.Client{Timeout: 15 * time.Second},
		apiEndpoint: AsterDexExplorerAPI,
	}
}

// NewAsterDexProviderWithEndpoint 创建自定义端点的 AsterDex Provider
// 用于测试或主网切换
func NewAsterDexProviderWithEndpoint(endpoint string) *AsterDexProvider {
	return &AsterDexProvider{
		client:      &http.Client{Timeout: 15 * time.Second},
		apiEndpoint: endpoint,
	}
}

func (p *AsterDexProvider) Type() ProviderType {
	return ProviderAsterDex
}

// GetFills 获取成交记录
// leaderID: 领航员钱包地址 (0x开头)
// since: 获取此时间之后的成交记录
func (p *AsterDexProvider) GetFills(leaderID string, since time.Time) ([]Fill, error) {
	// 获取用户详情（包含交易记录）
	userDetails, err := p.fetchUserDetails(leaderID)
	if err != nil {
		return nil, fmt.Errorf("fetch user details failed: %w", err)
	}

	if userDetails == nil || len(userDetails.Txs) == 0 {
		return nil, nil
	}

	var fills []Fill
	sinceMs := since.UnixMilli()

	for _, tx := range userDetails.Txs {
		// 1. 只处理已成交的订单
		if tx.Status != "FILLED" {
			continue
		}

		// 2. 只处理新交易（时间过滤）
		if tx.CreateTime <= sinceMs {
			continue
		}

		// 3. 避免重复处理（时间戳 + 哈希双重校验）
		if tx.CreateTime < p.lastTxTime {
			continue
		}
		if tx.CreateTime == p.lastTxTime && tx.TxHash == p.lastTxHash {
			continue
		}

		// 4. 解析交易记录
		fill := p.parseTxToFill(tx)
		fills = append(fills, fill)

		// 5. 更新最后处理标记
		if tx.CreateTime > p.lastTxTime || (tx.CreateTime == p.lastTxTime && tx.TxHash != p.lastTxHash) {
			p.lastTxTime = tx.CreateTime
			p.lastTxHash = tx.TxHash
		}
	}

	if len(fills) > 0 {
		logger.Infof("📡 [AsterDex] 获取到 %d 笔新成交 | leader=%s", len(fills), truncateAddr(leaderID))
	}

	return fills, nil
}

// GetAccountState 获取账户状态
// leaderID: 领航员钱包地址 (0x开头)
func (p *AsterDexProvider) GetAccountState(leaderID string) (*AccountState, error) {
	userDetails, err := p.fetchUserDetails(leaderID)
	if err != nil {
		return nil, fmt.Errorf("fetch user details failed: %w", err)
	}

	if userDetails == nil {
		return nil, fmt.Errorf("user details is nil")
	}

	state := &AccountState{
		TotalEquity:      userDetails.Balance.MarginBalance,
		AvailableBalance: userDetails.Balance.MarginBalance, // AsterDex 暂无可用余额字段，使用总余额
		Positions:        make(map[string]*Position),
		Timestamp:        time.Now(),
	}

	// 注意：AsterDex API 目前不返回持仓列表
	// 仓位通过交易记录推断，由 Engine 维护虚拟持仓状态
	// 如果后续 API 支持持仓查询，可在此处添加

	logger.Infof("📡 [AsterDex] 账户状态 | leader=%s equity=%.2f",
		truncateAddr(leaderID), state.TotalEquity)

	return state, nil
}

// ============================================================================
// 内部方法
// ============================================================================

// fetchUserDetails 获取用户详情
func (p *AsterDexProvider) fetchUserDetails(leaderID string) (*AsterDexUserDetails, error) {
	// 构造 POST 请求体
	reqBody := map[string]string{
		"address": leaderID,
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("JSON marshal failed: %w", err)
	}

	// 发送 POST 请求
	resp, err := p.client.Post(p.apiEndpoint, "application/json", bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBytes))
	}

	var apiResp AsterDexUserDetailsResp
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, fmt.Errorf("JSON decode failed: %w", err)
	}

	// 检查 API 返回状态
	if apiResp.Status == "ERROR" {
		return nil, fmt.Errorf("API error: %s (code=%s)", apiResp.ErrorData, apiResp.Code)
	}

	return apiResp.Data, nil
}

// parseTxToFill 将 AsterDex 交易记录转换为标准 Fill
func (p *AsterDexProvider) parseTxToFill(tx AsterDexTx) Fill {
	// 判断 Action
	action := DetermineAsterAction(tx)

	// 判断持仓方向
	positionSide := DetermineAsterPositionSide(tx)

	// 判断交易方向
	tradeSide := DetermineAsterTradeSide(tx)

	// 解析数量和价格
	quantity := parseFloat(tx.Quantity)
	price := parseFloat(tx.Price)

	// 如果有成交均价，优先使用
	if tx.AvgPrice != "" {
		avgPrice := parseFloat(tx.AvgPrice)
		if avgPrice > 0 {
			price = avgPrice
		}
	}

	// 标准化 symbol（确保大写，带 USDT 后缀）
	symbol := normalizeAsterSymbol(tx.Symbol)

	// 生成虚拟 posId
	posID := GenerateAsterPosID(symbol, positionSide)

	fill := Fill{
		ID:           tx.TxHash,
		Symbol:       symbol,
		Side:         tradeSide,
		PositionSide: positionSide,
		Action:       action,
		Price:        price,
		Size:         quantity,
		Value:        price * quantity,
		Timestamp:    time.UnixMilli(tx.CreateTime),
		Raw:          tx,
	}

	logger.Infof("📡 [AsterDex] 解析成交 | hash=%s symbol=%s action=%s side=%s posId=%s size=%.4f price=%.2f",
		truncateHash(tx.TxHash), symbol, action, positionSide, posID, quantity, price)

	return fill
}

// ============================================================================
// 工具函数
// ============================================================================

// normalizeAsterSymbol 标准化 AsterDex 符号
// 确保大写，带 USDT 后缀
func normalizeAsterSymbol(symbol string) string {
	symbol = strings.ToUpper(symbol)
	if !strings.HasSuffix(symbol, "USDT") {
		symbol = symbol + "USDT"
	}
	return symbol
}

// truncateAddr 截断地址显示（保护隐私）
func truncateAddr(addr string) string {
	if len(addr) <= 10 {
		return addr
	}
	return addr[:6] + "..." + addr[len(addr)-4:]
}

// truncateHash 截断哈希显示
func truncateHash(hash string) string {
	if len(hash) <= 14 {
		return hash
	}
	return hash[:10] + "..."
}
