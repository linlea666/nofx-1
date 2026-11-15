package api

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"nofx/copytrade"
	"nofx/logger"
	"nofx/store"
)

// ============================================================================
// FollowCoin 全局 token 管理 + 实盘账户查询
// ============================================================================

type FollowCoinCredentialsView struct {
	Label           string    `json:"label"`
	MaskedToken     string    `json:"masked_token"`
	ExpiresAt       time.Time `json:"expires_at"`
	LastValidatedAt time.Time `json:"last_validated_at"`
	LastStatus      string    `json:"last_status"`
	LastError       string    `json:"last_error"`
	UpdatedAt       time.Time `json:"updated_at"`
}

func toFollowCoinCredentialsView(c *store.FollowCoinCredentials) *FollowCoinCredentialsView {
	if c == nil {
		return nil
	}
	return &FollowCoinCredentialsView{
		Label: c.Label, MaskedToken: c.MaskedToken(), ExpiresAt: c.ExpiresAt,
		LastValidatedAt: c.LastValidatedAt, LastStatus: c.LastStatus, LastError: c.LastError,
		UpdatedAt: c.UpdatedAt,
	}
}

// newFollowCoinProbe 便于测试替换网络探测。
var newFollowCoinProbe = func(token string) followCoinProbe {
	return copytrade.NewFollowCoinProviderWithToken(token)
}

type followCoinProbe interface {
	ValidateToken() (*copytrade.FollowCoinUser, error)
	ListAccounts(userID int64) ([]copytrade.FollowCoinAccount, error)
	GetPositionSnapshot(apiID string) (*copytrade.AccountState, error)
}

var followCoinTokenHeaderRe = regexp.MustCompile(`(?i)(?:^|['"\s])token\s*:\s*([A-Za-z0-9_\-.=]+)`)

// extractFollowCoinToken 接受纯 token、"token: xxx" 或完整 cURL。
func extractFollowCoinToken(raw string) string {
	raw = strings.TrimSpace(raw)
	if m := followCoinTokenHeaderRe.FindStringSubmatch(raw); m != nil {
		return strings.TrimSpace(m[1])
	}
	if strings.ContainsAny(raw, " \t\n'\"") {
		return ""
	}
	return raw
}

func followCoinErrorMessage(err error) string {
	switch {
	case errors.Is(err, copytrade.ErrFollowCoinTokenInvalid):
		return "FollowCoin token 无效或已过期，请重新登录 App 获取新的 token"
	case errors.Is(err, copytrade.ErrFollowCoinAccountUnavailable):
		return "FollowCoin 实盘账户不存在或已停用"
	default:
		return "FollowCoin 接口暂时不可用: " + err.Error()
	}
}

// GetFollowCoinCredentials @Router /api/copytrade/followcoin-credentials [get]
func (h *CopyTradeHandler) GetFollowCoinCredentials(c *gin.Context) {
	creds, err := h.store.FollowCoinCreds().Get(store.FollowCoinCredsLabelDefault)
	if err != nil {
		logger.Errorf("Failed to load FollowCoin credentials: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load credentials"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"credentials": toFollowCoinCredentialsView(creds)})
}

type FollowCoinCredentialsSetRequest struct {
	Token string `json:"token"`
}

// SetFollowCoinCredentials 先校验再保存：明确无效的 token 不会覆盖现有 token。
// @Router /api/copytrade/followcoin-credentials [post]
func (h *CopyTradeHandler) SetFollowCoinCredentials(c *gin.Context) {
	var req FollowCoinCredentialsSetRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	token := extractFollowCoinToken(req.Token)
	if token == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请粘贴 FollowCoin token（也可粘贴包含 token 请求头的完整 cURL）"})
		return
	}
	user, err := newFollowCoinProbe(token).ValidateToken()
	if errors.Is(err, copytrade.ErrFollowCoinTokenInvalid) {
		c.JSON(http.StatusBadRequest, gin.H{"error": followCoinErrorMessage(err)})
		return
	}
	cs := h.store.FollowCoinCreds()
	if saveErr := cs.Set(store.FollowCoinCredsLabelDefault, token); saveErr != nil {
		logger.Errorf("Failed to save FollowCoin credentials: %v", saveErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save credentials"})
		return
	}
	status, errMsg, nick := store.FollowCoinCredsStatusValid, "", ""
	if err != nil {
		// 网络等暂时性故障：保存但标记 error，运行时第一次成功读取会自动转为 valid。
		status, errMsg = store.FollowCoinCredsStatusError, err.Error()
	} else if user != nil {
		nick = user.NickName
	}
	if updErr := cs.UpdateStatus(store.FollowCoinCredsLabelDefault, status, errMsg); updErr != nil {
		logger.Warnf("Failed to update FollowCoin credential status: %v", updErr)
	}
	creds, _ := cs.Get(store.FollowCoinCredsLabelDefault)
	c.JSON(http.StatusOK, gin.H{"message": "credentials saved", "nick_name": nick, "credentials": toFollowCoinCredentialsView(creds)})
}

// TestFollowCoinCredentials 校验当前保存的 token 并回写状态。
// @Router /api/copytrade/followcoin-credentials/test [post]
func (h *CopyTradeHandler) TestFollowCoinCredentials(c *gin.Context) {
	cs := h.store.FollowCoinCreds()
	creds, err := cs.Get(store.FollowCoinCredsLabelDefault)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load credentials"})
		return
	}
	if creds == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "尚未配置 FollowCoin token"})
		return
	}
	user, err := newFollowCoinProbe(creds.Token).ValidateToken()
	status, errMsg, nick := store.FollowCoinCredsStatusValid, "", ""
	switch {
	case errors.Is(err, copytrade.ErrFollowCoinTokenInvalid):
		status, errMsg = store.FollowCoinCredsStatusExpired, err.Error()
	case err != nil:
		status, errMsg = store.FollowCoinCredsStatusError, err.Error()
	case user != nil:
		nick = user.NickName
	}
	if updErr := cs.UpdateStatus(store.FollowCoinCredsLabelDefault, status, errMsg); updErr != nil {
		logger.Warnf("Failed to update FollowCoin credential status: %v", updErr)
	}
	c.JSON(http.StatusOK, gin.H{"status": status, "error": errMsg, "nick_name": nick})
}

// DeleteFollowCoinCredentials @Router /api/copytrade/followcoin-credentials [delete]
func (h *CopyTradeHandler) DeleteFollowCoinCredentials(c *gin.Context) {
	if err := h.store.FollowCoinCreds().Delete(store.FollowCoinCredsLabelDefault); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete credentials"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "credentials deleted"})
}

// ListFollowCoinAccounts 交易员主页 ID → 实盘账户列表（供选择要跟的账户）。
// @Router /api/copytrade/followcoin/accounts [get]
func (h *CopyTradeHandler) ListFollowCoinAccounts(c *gin.Context) {
	userID, err := strconv.ParseInt(strings.TrimSpace(c.Query("userId")), 10, 64)
	if err != nil || userID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请输入正确的交易员主页 ID（数字）"})
		return
	}
	token, err := h.store.FollowCoinCreds().LoadFollowCoinToken(store.FollowCoinCredsLabelDefault)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load credentials"})
		return
	}
	if token == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请先在「FollowCoin 凭证」中配置 token"})
		return
	}
	accounts, err := newFollowCoinProbe(token).ListAccounts(userID)
	if err != nil {
		code := http.StatusBadGateway
		if errors.Is(err, copytrade.ErrFollowCoinTokenInvalid) {
			code = http.StatusBadRequest
		}
		c.JSON(code, gin.H{"error": followCoinErrorMessage(err)})
		return
	}
	c.JSON(http.StatusOK, gin.H{"accounts": accounts, "count": len(accounts)})
}

// validateFollowCoinSource 保存跟单配置前探测所选实盘账户。只有"明确无效"
// （token 失效、账户不存在）才拒绝保存；接口暂时故障不阻止保存，运行时
// 由快照读取 fail-closed 兜底。
var validateFollowCoinSource = func(st *store.Store, apiID string) error {
	if st == nil {
		return nil
	}
	token, err := st.FollowCoinCreds().LoadFollowCoinToken(store.FollowCoinCredsLabelDefault)
	if err != nil {
		return err
	}
	if token == "" {
		return errors.New("请先在「FollowCoin 凭证」中配置 token，再保存 FollowCoin 跟单")
	}
	if _, err := newFollowCoinProbe(token).GetPositionSnapshot(apiID); err != nil {
		if errors.Is(err, copytrade.ErrFollowCoinTokenInvalid) || errors.Is(err, copytrade.ErrFollowCoinAccountUnavailable) {
			return errors.New(followCoinErrorMessage(err))
		}
		logger.Warnf("⚠️ FollowCoin 实盘账户 %s 保存前探测失败（不阻止保存）: %v", apiID, err)
	}
	return nil
}
