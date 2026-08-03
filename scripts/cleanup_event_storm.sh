#!/usr/bin/env bash
#
# 一次性清理 Copy Guard 事件风暴残留。
#
# 为什么需要手工执行而不是交给 store.RetentionService：
# 自动留存按"周期关闭 + 超过 RETENTION_COPYGUARD_EVENT_DAYS(30 天)"删除，
# 而当前库里 11 万条事件全部产生于最近 31 天（约 3500 条/天），一条都还没到期。
# 这些行不是过期数据，是高频写入本身——需要一次性压掉存量，代码侧的去重与
# 事件抑制才能在新窗口里体现出来。
#
# 保留原则：只删「无分析价值的重复态」，保留一切能用于复盘止损策略的证据。
#   删除：CYCLE_SUMMARY_EMAIL_DEDUPED —— 纯投递去重噪音，代码已不再写入
#         （integration.go saveCycleSummaryEmailStatus 直接 return）。
#         PROTECTION_ACTIVE / PROTECTION_RETRY / REENTRY_GATE_CHANGED —— 心跳与
#         中间态，其最终结论已固化在 copy_guard_cycles 的 protection_status /
#         protection_retries 和候选终态里，仅对已关闭周期删除。
#   保留：PROTECTION_PLAN / PROTECTION_CLAMPED / STOP_RISK_THRESHOLD_EXCEEDED /
#         AI_CANDIDATE_UNACTIONABLE —— 这些是止损重构上线后做「改动前 vs 改动后」
#         对比的唯一基线（distance/ATR 分布、governed_by、钳位频次），删掉就再也
#         无法验证保证金上限降级是否真的把亏损转正。
#
# 用法：
#   ./cleanup_event_storm.sh <db_path>            # 预演，只统计不删除
#   ./cleanup_event_storm.sh <db_path> --apply    # 实际执行
#
# 执行前请先停服（systemctl stop nofx 或 docker stop），SQLite 单连接模型下
# 与交易写入并发会长时间持锁。脚本会自动备份到 <db_path>.bak.<时间戳>。

set -euo pipefail

DB="${1:-}"
MODE="${2:-dry-run}"

if [[ -z "$DB" || ! -f "$DB" ]]; then
	echo "用法: $0 <db_path> [--apply]" >&2
	exit 1
fi

SQLITE=(sqlite3 -cmd ".timeout 60000" "$DB")

# 已关闭周期上的心跳/中间态事件；开放周期一律不动，它们的事件仍在被读取。
CLOSED_CYCLE_NOISE="'PROTECTION_ACTIVE','PROTECTION_RETRY','REENTRY_GATE_CHANGED'"

echo "=== 清理前 ==="
"${SQLITE[@]}" <<SQL
.mode column
.headers on
SELECT '总事件数' AS 指标, COUNT(*) AS 值 FROM copy_guard_events
UNION ALL SELECT '投递去重噪音(全删)', COUNT(*) FROM copy_guard_events
	WHERE type='CYCLE_SUMMARY_EMAIL_DEDUPED'
UNION ALL SELECT '已关闭周期心跳(删)', COUNT(*) FROM copy_guard_events e
	WHERE e.type IN ($CLOSED_CYCLE_NOISE)
	  AND EXISTS (SELECT 1 FROM copy_guard_cycles c WHERE c.id=e.cycle_id AND c.closed_at IS NOT NULL)
UNION ALL SELECT '止损基线证据(保留)', COUNT(*) FROM copy_guard_events
	WHERE type IN ('PROTECTION_PLAN','PROTECTION_CLAMPED','STOP_RISK_THRESHOLD_EXCEEDED','AI_CANDIDATE_UNACTIONABLE')
UNION ALL SELECT '镜像事件表总数', COUNT(*) FROM copy_trade_events;
SQL

echo
echo "数据库当前大小: $(du -h "$DB" | cut -f1)"

if [[ "$MODE" != "--apply" ]]; then
	echo
	echo "预演模式，未做任何修改。确认无误后追加 --apply 执行。"
	exit 0
fi

BACKUP="${DB}.bak.$(date +%Y%m%d%H%M%S)"
echo
echo "备份到 $BACKUP ..."
"${SQLITE[@]}" ".backup '$BACKUP'"

echo "删除中 ..."
# 分批提交：单条 DELETE 覆盖数万行会长时间独占写锁，即使停服也会拖长 VACUUM 前的
# 事务；500 行一批与 store.RetentionService 的 retentionBatchSize 保持一致。
delete_batched() {
	local label="$1" table="$2" where="$3" removed=0 n
	while :; do
		n=$("${SQLITE[@]}" "DELETE FROM $table WHERE rowid IN (SELECT rowid FROM $table WHERE $where LIMIT 500); SELECT changes();")
		removed=$((removed + n))
		[[ "$n" -eq 0 ]] && break
	done
	echo "  $label: $removed 行"
}

delete_batched "投递去重噪音" copy_guard_events "type='CYCLE_SUMMARY_EMAIL_DEDUPED'"
delete_batched "已关闭周期心跳" copy_guard_events \
	"type IN ($CLOSED_CYCLE_NOISE) AND EXISTS (SELECT 1 FROM copy_guard_cycles c WHERE c.id=cycle_id AND c.closed_at IS NOT NULL)"
# 镜像表按同口径同步，否则前端事件日志仍会展示已在主表清掉的心跳。
delete_batched "镜像表心跳" copy_trade_events \
	"event_type IN ($CLOSED_CYCLE_NOISE) AND EXISTS (SELECT 1 FROM copy_guard_cycles c WHERE c.id=cycle_id AND c.closed_at IS NOT NULL)"

echo "VACUUM 回收空间（按库大小可能需要数分钟）..."
"${SQLITE[@]}" "VACUUM;"

echo
echo "=== 清理后 ==="
"${SQLITE[@]}" <<SQL
.mode column
.headers on
SELECT '总事件数' AS 指标, COUNT(*) AS 值 FROM copy_guard_events
UNION ALL SELECT '止损基线证据(应不变)', COUNT(*) FROM copy_guard_events
	WHERE type IN ('PROTECTION_PLAN','PROTECTION_CLAMPED','STOP_RISK_THRESHOLD_EXCEEDED','AI_CANDIDATE_UNACTIONABLE')
UNION ALL SELECT '镜像事件表总数', COUNT(*) FROM copy_trade_events;
SQL
echo "数据库大小: $(du -h "$DB" | cut -f1)（备份保留在 $BACKUP）"
