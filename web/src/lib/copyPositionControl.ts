import type { CopyTradePositionMapping, Position } from '../types'

// A symbol/side match cannot distinguish a reopened manual position. Only a
// current identity verified by the backend may expose following controls.
export function copyPositionControl(
  position: Pick<Position, 'symbol' | 'side' | 'position_key'>,
  mappings: CopyTradePositionMapping[] | undefined
) {
  const matches = position.position_key
    ? (mappings?.filter(
        (m) =>
          m.current_position_key === position.position_key &&
          m.custody_state === 'MANAGED'
      ) ?? [])
    : []
  const mapping = matches.length === 1 ? matches[0] : undefined
  const unknown =
    !mappings ||
    matches.length > 1 ||
    mappings.some(
      (m) =>
        (m.custody_state === 'UNKNOWN' ||
          (m.custody_state === 'MANAGED' && !m.current_position_key)) &&
        (m.execution_symbol || m.symbol) === position.symbol &&
        m.side.toLowerCase() === position.side.toLowerCase()
    )
  const related =
    mappings?.filter(
      (m) =>
        (m.execution_symbol || m.symbol) === position.symbol &&
        m.side.toLowerCase() === position.side.toLowerCase()
    ) ?? []
  const exits = related.filter((m) => m.leader_exit_enabled)
  const exitMapping = exits.length === 1 ? exits[0] : undefined
  const followReason = related
    .map((m) => m.follow_reason)
    .filter(Boolean)
    .join('；')
  const followLabel = related.some((m) => m.status === 'manual_stopped')
    ? '已暂停'
    : related.some((m) => m.status === 'ignored')
      ? '本轮跳过'
      : '跟随状态待核实'
  return { mapping, unknown, exitMapping, followReason, followLabel }
}
