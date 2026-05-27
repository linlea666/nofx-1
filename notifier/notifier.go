package notifier

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"nofx/logger"
)

// ============================================================================
// 配置
// ============================================================================

// Config 邮件通知配置
type Config struct {
	Enabled       bool          // 是否启用邮件通知
	SMTPHost      string        // SMTP 服务器（smtp.163.com / smtp.qq.com / ...）
	SMTPPort      int           // 端口（465 SSL / 25 STARTTLS / 587 STARTTLS）
	SMTPUser      string        // 发件邮箱
	SMTPPass      string        // 授权码 / 应用密码
	From          string        // 发件人邮箱地址（默认同 SMTPUser）
	FromName      string        // 发件人显示名
	To            []string      // 收件人列表
	MinInterval   time.Duration // 同 key 告警最小间隔，默认 60s
	QueueSize     int           // 异步队列大小，默认 100
	SendOnStartup bool          // 启动时发送测试邮件，默认 true
}

// LoadFromEnv 从环境变量加载配置
//
// 环境变量：
//
//	NOTIFY_EMAIL_ENABLED   = true|false           (默认 false，全模块禁用)
//	SMTP_HOST              = smtp.163.com         (必填)
//	SMTP_PORT              = 465                  (默认 465)
//	SMTP_USER              = sender@163.com       (必填)
//	SMTP_PASS              = <auth-code>          (必填，163 授权码)
//	SMTP_FROM              = sender@163.com       (默认同 SMTP_USER)
//	SMTP_FROM_NAME         = NOFX Notifier        (默认 NOFX Notifier)
//	NOTIFY_TO              = a@x.com,b@y.com      (必填，逗号分隔)
//	NOTIFY_MIN_INTERVAL    = 60                   (秒，默认 60)
//	NOTIFY_QUEUE_SIZE      = 100                  (默认 100)
//	NOTIFY_SEND_ON_STARTUP = true                 (默认 true)
func LoadFromEnv() Config {
	cfg := Config{
		SMTPPort:      465,
		MinInterval:   60 * time.Second,
		QueueSize:     100,
		SendOnStartup: true,
		FromName:      "NOFX Notifier",
	}

	cfg.Enabled = parseBool(os.Getenv("NOTIFY_EMAIL_ENABLED"), false)
	cfg.SMTPHost = strings.TrimSpace(os.Getenv("SMTP_HOST"))
	if v := os.Getenv("SMTP_PORT"); v != "" {
		if p, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && p > 0 {
			cfg.SMTPPort = p
		}
	}
	cfg.SMTPUser = strings.TrimSpace(os.Getenv("SMTP_USER"))
	cfg.SMTPPass = strings.TrimSpace(os.Getenv("SMTP_PASS"))
	cfg.From = strings.TrimSpace(os.Getenv("SMTP_FROM"))
	if cfg.From == "" {
		cfg.From = cfg.SMTPUser
	}
	if v := strings.TrimSpace(os.Getenv("SMTP_FROM_NAME")); v != "" {
		cfg.FromName = v
	}

	if v := os.Getenv("NOTIFY_TO"); v != "" {
		for _, addr := range strings.Split(v, ",") {
			if a := strings.TrimSpace(addr); a != "" {
				cfg.To = append(cfg.To, a)
			}
		}
	}

	if v := os.Getenv("NOTIFY_MIN_INTERVAL"); v != "" {
		if sec, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && sec >= 0 {
			cfg.MinInterval = time.Duration(sec) * time.Second
		}
	}
	if v := os.Getenv("NOTIFY_QUEUE_SIZE"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			cfg.QueueSize = n
		}
	}
	if v := os.Getenv("NOTIFY_SEND_ON_STARTUP"); v != "" {
		cfg.SendOnStartup = parseBool(v, true)
	}

	return cfg
}

// ============================================================================
// 告警事件
// ============================================================================

// Alert 一条告警事件
type Alert struct {
	Time     time.Time
	Category string            // copy_trade | trader | system | startup ...
	TraderID string            // 关联的 trader（可空，例如系统级告警）
	Title    string            // 主题片段
	Body     string            // 详情正文
	Fields   map[string]string // 可选附加字段（会附在正文）
	RateKey  string            // 限流键，留空时按 Category+TraderID+Title 自动生成
	DedupKey string            // 一次性去重键：同 key 告警进程生命周期内只发送一次
}

// ============================================================================
// Notifier 接口与全局单例
// ============================================================================

// Notifier 通知器接口（便于未来扩展 Telegram / 企业微信等）
type Notifier interface {
	Notify(a Alert)
	Shutdown()
}

// noopNotifier 不发送任何通知（未启用时使用）
type noopNotifier struct{}

func (n noopNotifier) Notify(Alert) {}
func (n noopNotifier) Shutdown()    {}

// 全局单例（默认 no-op，未初始化时调用零副作用）
var (
	global       Notifier = noopNotifier{}
	globalCfg    Config
	globalInited bool
	globalMu     sync.RWMutex
)

// Init 初始化全局通知器
// 未启用或必填配置缺失时退化为 no-op 实现
func Init(cfg Config) error {
	globalMu.Lock()
	defer globalMu.Unlock()

	if !cfg.Enabled {
		global = noopNotifier{}
		globalCfg = cfg
		globalInited = true
		logger.Info("📭 邮件通知未启用 (NOTIFY_EMAIL_ENABLED=false)")
		return nil
	}

	if err := validateConfig(cfg); err != nil {
		// 配置不全 → 降级 no-op，仅 warn，不影响主流程
		global = noopNotifier{}
		globalCfg = cfg
		globalInited = true
		logger.Warnf("⚠️ 邮件通知配置不完整: %v，已禁用", err)
		return nil
	}

	n := newEmailNotifier(cfg)
	global = n
	globalCfg = cfg
	globalInited = true

	logger.Infof("📧 邮件通知已启用 | %s:%d → %s",
		cfg.SMTPHost, cfg.SMTPPort, strings.Join(cfg.To, ", "))

	// 启动测试邮件（可关闭）
	if cfg.SendOnStartup {
		go func() {
			testAlert := Alert{
				Time:     time.Now(),
				Category: "startup",
				Title:    "NOFX 邮件通知启动测试",
				Body: fmt.Sprintf(
					"NOFX 邮件告警模块已成功启动。\n\n"+
						"配置信息:\n"+
						"  SMTP:  %s:%d\n"+
						"  发件人: %s\n"+
						"  收件人: %s\n"+
						"  限流:   %s\n\n"+
						"今后跟单失败、引擎异常等事件将通过本邮箱告警。",
					cfg.SMTPHost, cfg.SMTPPort, cfg.From,
					strings.Join(cfg.To, ", "), cfg.MinInterval),
				RateKey: "__startup_test__",
			}
			n.Notify(testAlert)
		}()
	}

	return nil
}

// Notify 全局便捷调用（线程安全 + 非阻塞）
func Notify(a Alert) {
	globalMu.RLock()
	n := global
	globalMu.RUnlock()
	if a.Time.IsZero() {
		a.Time = time.Now()
	}
	n.Notify(a)
}

// NotifyError 便捷封装：构造 Alert 并发送
// category: 告警分类（例如 "copy_trade" / "trader"）
// traderID: 关联 trader（可为空）
// title:    主题片段
// body:     详情正文
func NotifyError(category, traderID, title, body string) {
	Notify(Alert{
		Time:     time.Now(),
		Category: category,
		TraderID: traderID,
		Title:    title,
		Body:     body,
	})
}

// Shutdown 关闭全局通知器（等待队列清空）
func Shutdown() {
	globalMu.Lock()
	n := global
	global = noopNotifier{}
	globalMu.Unlock()
	if n != nil {
		n.Shutdown()
	}
}

// Enabled 全局通知器是否真的处于启用状态（非 noop）
func Enabled() bool {
	globalMu.RLock()
	defer globalMu.RUnlock()
	_, isEmail := global.(*emailNotifier)
	return isEmail
}

// ============================================================================
// 邮件通知器实现
// ============================================================================

type emailNotifier struct {
	cfg      Config
	client   *emailClient
	queue    chan Alert
	wg       sync.WaitGroup
	stopOnce sync.Once
	stopCh   chan struct{}

	// 限流：rateKey -> 上次发送时间
	lastSent sync.Map

	// 一次性去重：dedupKey -> 首次入队时间
	deduped sync.Map
}

func newEmailNotifier(cfg Config) *emailNotifier {
	n := &emailNotifier{
		cfg:    cfg,
		client: newEmailClient(cfg),
		queue:  make(chan Alert, cfg.QueueSize),
		stopCh: make(chan struct{}),
	}
	n.wg.Add(1)
	go n.worker()
	return n
}

// Notify 入队，非阻塞
func (n *emailNotifier) Notify(a Alert) {
	dedupReserved := false
	if a.DedupKey != "" {
		if _, loaded := n.deduped.LoadOrStore(a.DedupKey, time.Now()); loaded {
			logger.Debugf("📭 通知去重跳过 | dedupKey=%s", a.DedupKey)
			return
		}
		dedupReserved = true
	}

	// 1. 限流键
	key := a.RateKey
	if key == "" {
		key = a.Category + "|" + a.TraderID + "|" + a.Title
	}

	// 2. 限流判断（在入队前过滤，节省队列容量）
	if n.cfg.MinInterval > 0 {
		if v, ok := n.lastSent.Load(key); ok {
			if last, ok := v.(time.Time); ok && time.Since(last) < n.cfg.MinInterval {
				// 命中限流，静默丢弃（debug 级日志，避免日志刷屏）
				logger.Debugf("📭 通知限流跳过 | key=%s elapsed=%s < interval=%s",
					key, time.Since(last).Truncate(time.Second), n.cfg.MinInterval)
				if dedupReserved {
					n.deduped.Delete(a.DedupKey)
				}
				return
			}
		}
		n.lastSent.Store(key, time.Now())
	}

	// 3. 非阻塞入队
	select {
	case n.queue <- a:
	case <-n.stopCh:
		// 已关闭，丢弃
		if dedupReserved {
			n.deduped.Delete(a.DedupKey)
		}
	default:
		// 队列满，丢弃 + warn
		if dedupReserved {
			n.deduped.Delete(a.DedupKey)
		}
		logger.Warnf("⚠️ 邮件通知队列已满（容量=%d），丢弃: %s", n.cfg.QueueSize, a.Title)
	}
}

// worker 后台协程，串行消费队列发送邮件
func (n *emailNotifier) worker() {
	defer n.wg.Done()
	for {
		select {
		case <-n.stopCh:
			// drain 剩余告警再退出（最多等 5 秒）
			drainDeadline := time.After(5 * time.Second)
			for {
				select {
				case a := <-n.queue:
					n.send(a)
				case <-drainDeadline:
					return
				default:
					return
				}
			}
		case a := <-n.queue:
			n.send(a)
		}
	}
}

// send 真实发送一封邮件
func (n *emailNotifier) send(a Alert) {
	subject := buildSubject(a)
	body := buildBody(a)

	if err := n.client.Send(n.cfg.To, subject, body); err != nil {
		logger.Warnf("⚠️ 邮件发送失败: %v | subject=%s", err, subject)
		return
	}
	logger.Infof("📧 邮件已发送 | %s → %s", subject, strings.Join(n.cfg.To, ","))
}

// Shutdown 等待队列消费完毕（最多 5 秒）
func (n *emailNotifier) Shutdown() {
	n.stopOnce.Do(func() {
		close(n.stopCh)
		n.wg.Wait()
	})
}

// ============================================================================
// 内部工具
// ============================================================================

func validateConfig(cfg Config) error {
	if cfg.SMTPHost == "" {
		return fmt.Errorf("SMTP_HOST is required")
	}
	if cfg.SMTPPort == 0 {
		return fmt.Errorf("SMTP_PORT is required")
	}
	if cfg.SMTPUser == "" {
		return fmt.Errorf("SMTP_USER is required")
	}
	if cfg.SMTPPass == "" {
		return fmt.Errorf("SMTP_PASS is required")
	}
	if len(cfg.To) == 0 {
		return fmt.Errorf("NOTIFY_TO is required (at least one recipient)")
	}
	return nil
}

func parseBool(s string, def bool) bool {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return def
	}
	switch s {
	case "1", "true", "yes", "y", "on":
		return true
	case "0", "false", "no", "n", "off":
		return false
	}
	return def
}

// buildSubject 构造邮件主题
func buildSubject(a Alert) string {
	tag := strings.ToUpper(a.Category)
	if tag == "" {
		tag = "ALERT"
	}
	if a.Title == "" {
		return fmt.Sprintf("[NOFX %s] 异常告警", tag)
	}
	return fmt.Sprintf("[NOFX %s] %s", tag, a.Title)
}

// buildBody 构造邮件正文
func buildBody(a Alert) string {
	var b strings.Builder
	b.WriteString("=========================================\n")
	b.WriteString("NOFX 系统告警 / NOFX System Alert\n")
	b.WriteString("=========================================\n\n")

	b.WriteString("时间 (Time):     ")
	b.WriteString(a.Time.Format("2006-01-02 15:04:05 MST"))
	b.WriteString("\n")

	b.WriteString("分类 (Category): ")
	b.WriteString(a.Category)
	b.WriteString("\n")

	if a.TraderID != "" {
		b.WriteString("账号 (TraderID): ")
		b.WriteString(a.TraderID)
		b.WriteString("\n")
	}

	if a.Title != "" {
		b.WriteString("标题 (Title):    ")
		b.WriteString(a.Title)
		b.WriteString("\n")
	}

	b.WriteString("\n--- 详情 / Details ---\n")
	if a.Body != "" {
		b.WriteString(a.Body)
		b.WriteString("\n")
	}

	if len(a.Fields) > 0 {
		b.WriteString("\n--- 附加字段 / Fields ---\n")
		// 稳定排序输出
		keys := make([]string, 0, len(a.Fields))
		for k := range a.Fields {
			keys = append(keys, k)
		}
		// 简单字典序，不引入新依赖
		for i := 0; i < len(keys); i++ {
			for j := i + 1; j < len(keys); j++ {
				if keys[i] > keys[j] {
					keys[i], keys[j] = keys[j], keys[i]
				}
			}
		}
		for _, k := range keys {
			b.WriteString("  ")
			b.WriteString(k)
			b.WriteString(": ")
			b.WriteString(a.Fields[k])
			b.WriteString("\n")
		}
	}

	b.WriteString("\n— NOFX Notifier\n")
	return b.String()
}
