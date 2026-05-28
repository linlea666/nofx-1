package notifier

// 本文件中的所有符号仅供测试使用（命名带 ForTesting 后缀以示警示）。
// 它们必须存在于生产构建中，因为 Go 的 _test.go helper 仅对同包测试可见，
// 跨包测试（如 copytrade 测试需要替换 notifier 全局状态）必须通过生产 API 接入。
//
// 请勿在任何业务代码中调用 SetGlobalForTesting / 实例化 CaptureNotifier。
// 业务代码应使用 Init(cfg) 配置真实 notifier。

// CaptureNotifier 收集所有 Notify(a) 调用的 Alert，仅用于测试断言。
type CaptureNotifier struct {
	Alerts []Alert
}

// Notify 实现 Notifier 接口；线程不安全（测试单线程使用即可）。
func (c *CaptureNotifier) Notify(a Alert) {
	c.Alerts = append(c.Alerts, a)
}

// Shutdown 实现 Notifier 接口；无副作用。
func (c *CaptureNotifier) Shutdown() {}

// SetGlobalForTesting 临时替换全局 notifier 与跟单动作开关；返回还原函数。
//
// 调用方应在 t.Cleanup(restore) 中调用还原函数，避免污染同 process 内其他测试。
// 仅在测试中使用——生产代码请走 Init(cfg) 路径。
//
// 参数：
//   - n: 临时全局 notifier 实例（通常是 *CaptureNotifier）
//   - copyActionEnabled: 用于驱动 CopyTradeActionEnabled() 返回值
func SetGlobalForTesting(n Notifier, copyActionEnabled bool) func() {
	globalMu.Lock()
	prevGlobal := global
	prevCfg := globalCfg
	prevInited := globalInited

	global = n
	globalCfg = Config{NotifyBinanceCopyActionEnabled: copyActionEnabled}
	globalInited = true
	globalMu.Unlock()

	return func() {
		globalMu.Lock()
		global = prevGlobal
		globalCfg = prevCfg
		globalInited = prevInited
		globalMu.Unlock()
	}
}
