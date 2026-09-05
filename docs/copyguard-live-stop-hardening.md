# Copy Guard 实盘兜底止损修复交付记录

## 范围与执行边界

本次从 `main@2000a247` 及未提交的断点恢复改动继续。保留首仓固化公式、普通跟单金额算法与既有 ATR 周期合同；不分叉跟单引擎。不执行服务器部署、数据库清理、交易员自动启用或实盘下单。

固定模式首次确认成交后固化首仓价格、数量与杠杆证据，比例默认 0.8。后续加减仓不重算策略价格；仅强平安全边界冲突时单向收紧。80% 不是后续整个仓位的最大损失保证，费用、资金费、跳空和滑点不进入触发公式。

## 根因与修复

1. **首次保护失败后无法可靠恢复**：新增成交证据及杠杆确认回执表。原始成交证据与周期、attempt、事件、intent 在同一事务落库。回执在未提交订单前可跟随成功设置的杠杆更新，一旦提交或首次成交则冻结。重试、重启只读取证据，不使用新均价和新杠杆冒充首仓。
2. **触发与退出缺少统一边界**：所有可信触发先建立内存门禁，再持久化；交易所提交边界再次检查门禁。挂单核验发现触发立即返回，不等待正常换单收敛；接管、撤单确认过程中发现触发同样先建立门禁。退出门禁保留后来补全的成交订单身份，审计元数据按调用复制，避免数据库规范化与并发触发共用可变 map。新增有完整币种、方向、保证金模式、仓位 ID 和稳定订单身份的退出请求。超时先查原订单，终态后重新查询残仓才生成下一笔。撤单后的查询失败不得当作订单已消失。
3. **晚到成交及提前收尾**：已提交的领航员订单成交仍原子记账，但不重新激活映射、不补齐仓位。仍有未完成在途订单时禁止风险退出结算；`STOP_PENDING_FLAT/STOP_PARTIAL/STOP_TRIGGERED` 不得被观察、次数用尽、领航员平仓或普通结算覆盖。退出确认完成后才能结束领航员生命周期。
4. **无触发证据的空仓误归因**：仅新鲜、同范围空仓且无在途交易时记为 `POSITION_ABSENT` 并脱离跟随，保留周期等待领航员结束。不伪造止损，不循环补不存在仓位的保护，不自动重开。
5. **交易所语义差异**：OKX 退出使用请求的保证金范围与仓位身份，并限制到新鲜残仓数量；普通下单接口行为不变。Binance Hedge CLOSE_ALL 按币种与方向全仓覆盖，其查询没有 marginMode 字段；不套用 OKX 的显式数量/保证金校验。触发类型必须匹配，一档价格差不能当作浮点误差。
6. **风险审计与实盘保护混用**：页面区分首仓策略价、要求价和最近核验的交易所价格。风险计算复用后端纯数学模块；挂单失败展示旧单实际价格而不是尚未落实的新价。记录权威标记价来源和交易所时间；缺失或过期不得用 last price 主动退出。
7. **配置往返与兼容**：恢复持久 ATR profile 后只合并显式 ATR patch；前端保存未提交草稿。固定模式继续在 UI/API/运行时禁用重入。总开关只影响新周期。能力查询和保存回读避免旧后端静默丢失固定模式。修复配置弹窗缺省列表引用不稳定造成的重复初始化。
8. **快照安全与日志**：空或无法识别快照明确失败；已识别旧协议仍通过白名单转成 v2。测试夹具改用显式版本快照，不放宽安全校验。动作日志由原始执行身份分类，区分首仓、加仓、部分成交和待对账。
9. **停用影子运行**：停止替代策略初始化、采集、补洞、结算和晋级计算；UI 隐藏替代策略与晋级区。历史读取和表保留。现有 ATR 重入合同所需的领航员观察与实盘归因账本未作无关清理。

## 复用决策

- 直接复用：固定公式、安全取整、单向强平钳紧、托管保护、精度与现有退出状态。
- 扩展复用：成交事务、执行 intent/order attempt、OKX/Binance 下单接口、周期监控。
- 提取复用：标记价观测、保护身份校验、退出结算屏障、ATR 显式 patch、风险审计公式。
- 独立新增：不可变首仓证据表与窄接口；不引入新引擎、影子策略或账户级动态缩量。

## 验证命令与结果记录

使用临时测试数据库和本地模拟交易所，不使用实盘凭证。

```sh
GOCACHE=/private/tmp/nofx-go-cache GOTMPDIR=/private/tmp/nofx-go-tmp go test ./... -count=1
GOCACHE=/private/tmp/nofx-go-cache GOTMPDIR=/private/tmp/nofx-go-tmp go test ./copytrade ./store ./api ./trader ./copyguardmetrics -count=3
GOCACHE=/private/tmp/nofx-go-cache GOTMPDIR=/private/tmp/nofx-go-tmp go test -race ./copytrade ./store ./api ./trader ./copyguardmetrics -count=1 -timeout=20m
GOCACHE=/private/tmp/nofx-go-cache GOTMPDIR=/private/tmp/nofx-go-tmp go vet ./...
cd web
npm run lint
npm test
npm run build
```

生产构建脚本包含 TypeScript 检查。提交前还核对 `git diff --check`、所有新增文件与最终差异。

2026-09-05 最终核验结果：

- 全量 Go 测试通过：`/private/tmp/copyguard-final-go.log`。
- 相关五个包三轮回归通过：`/private/tmp/copyguard-handoff-multirun.log`。最后补充的触发时序、退出状态屏障和并发证据隔离另有三轮 race 验证通过：`/private/tmp/copyguard-handoff-trigger-race.log`、`/private/tmp/copyguard-evidence-race-verified.log`。
- 最终源码完整相关包 race 通过，无数据竞争报告：`/private/tmp/copyguard-final-race.log`（copytrade 344.675s、store 332.672s、api 61.902s、trader 63.112s、copyguardmetrics 23.399s）。
- `go vet ./...` 通过，日志为空：`/private/tmp/copyguard-final-vet.log`。
- 前端 lint、124 项测试（10 个测试文件）、TypeScript 检查和生产构建全部通过：`/private/tmp/copyguard-release-web-lint.log`、`/private/tmp/copyguard-release-web-test.log`、`/private/tmp/copyguard-release-web-build.log`。之后未再修改前端源码。
- `git diff --check` 通过；本次共 68 个修改/新增文件，无删除文件。临时测试数据库、测试日志和本地构建输出不纳入提交。

断点恢复时未假定先前命令成功：实际核对分支、差异、未完成进程、日志及上游。恢复前未提交改动中的功能主干予以保留；剩余触发/结算边界继续补齐；不确定的测试以实际退出结果和新回归确认。服务器运行版本、生产数据库迁移和实际托管单属于部署后验证，本次未执行。

已新增测试覆盖：首仓查询/精度/锚点写入失败恢复；杠杆回执冻结；一档止损收紧；mark/last 分离与过期；强平价越过均价；可信触发数据库故障门禁；核验/接管/撤单触发边界；并发元数据隔离及成交身份补全；退出超时与重启；晚到加减平仓、重复回调及结算屏障；无证据空仓；OKX 跨保证金范围；配置往返、1/80/99% 保存与边界拒绝、总开关关闭后固定合同；动作日志；页面实盘审计与快照脱敏；影子历史只读。

## 发布与已知边界

- 部署由用户手动完成，先备份数据库。空/无法识别历史快照将阻止启动并报告周期 ID，不能使用当前模板静默替代。
- 核验实际服务进程的运行版本，而非只检查磁盘文件；检查能力响应 `live_hardening_version >= 1`、`fixed_initial_margin=true`、`shadow_runtime_enabled=false`，并验证配置回读和实际托管单。
- 历史无可信首仓证据的固定周期不会用当前仓位补造锚点；告警待处理，不自动切换策略。
- 权威行情/数据库/交易所故障时仍有不可避免的保护延迟；挂单失败保留旧保护并重试，不因失败强制平仓。交易所成交损失不保证等于理论损失。
- 已知构建警告：前端大包提示；macOS race 链接器可能提示 LC_DYSYMTAB。以命令退出码和测试结论为准，不据此声称零风险。
- 首批仅用户手动启用少量交易员。密码轮换、SSH 安全加固和日志保留是独立运维任务，不含在本次提交。

## 修改与新增文件

本报告亦为新增文件。以下为当前功能差异清单（包括因严格快照协议更新的回归夹具）：

- `api/copyguard_attribution_test.go`
- `api/copyguard_capabilities.go`
- `api/copyguard_export_test.go`
- `api/copyguard_live_hardening_test.go`
- `api/copytrade_handler.go`
- `api/server.go`
- `copyguardmetrics/ai_evaluation_test.go`
- `copyguardmetrics/shadow_evaluation.go`
- `copyguardmetrics/shadow_evaluation_test.go`
- `copyguardmetrics/shadow_retirement_test.go`
- `copytrade/ai_reentry_ttl_test.go`
- `copytrade/batch2_idempotency_test.go`
- `copytrade/copyguard_binance_test.go`
- `copytrade/copyguard_convergence_test.go`
- `copytrade/copyguard_fixes_test.go`
- `copytrade/copyguard_governance_test.go`
- `copytrade/copyguard_reentry_test.go`
- `copytrade/engine.go`
- `copytrade/engine_risk.go`
- `copytrade/fixed_mark.go`
- `copytrade/integration.go`
- `copytrade/integration_test.go`
- `copytrade/live_hardening_test.go`
- `copytrade/ownership_recovery_test.go`
- `copytrade/position_absence.go`
- `copytrade/position_margin_integration_test.go`
- `copytrade/protection_identity.go`
- `copytrade/protection_v4_test.go`
- `copytrade/risk_exit_gate_test.go`
- `copytrade/risk_exit_order.go`
- `copytrade/risk_policy.go`
- `copytrade/risk_trigger.go`
- `copytrade/stop_consistency_test.go`
- `decision/engine.go`
- `store/copy_events_test.go`
- `store/copyguard.go`
- `store/copyguard_atr_patch.go`
- `store/copyguard_exit_barrier.go`
- `store/copyguard_initial_fill.go`
- `store/copyguard_manual_test.go`
- `store/copyguard_position_absent.go`
- `store/copyguard_replacement_test.go`
- `store/copyguard_runtime.go`
- `store/copyguard_snapshot_test.go`
- `store/copyguard_test.go`
- `store/copytrade_execution_intent.go`
- `store/copytrade_execution_intent_test.go`
- `store/copytrade_ownership_recovery_test.go`
- `store/follower_notional_invariant_test.go`
- `store/position_margin_shadow_test.go`
- `store/reentry_candidate_test.go`
- `store/trader_lifecycle_test.go`
- `trader/auto_trader.go`
- `trader/binance_futures.go`
- `trader/copyguard_exit.go`
- `trader/copyguard_leverage.go`
- `trader/copyguard_live_exit_test.go`
- `trader/copyguard_mark.go`
- `trader/interface.go`
- `trader/okx_trader.go`
- `trader/position_sync_test.go`
- `web/src/components/TraderConfigModal.copyguard.test.tsx`
- `web/src/components/TraderConfigModal.tsx`
- `web/src/lib/copyGuardPolicy.ts`
- `web/src/pages/CopyGuardPage.live.test.tsx`
- `web/src/pages/CopyGuardPage.tsx`
- `web/src/types.ts`
