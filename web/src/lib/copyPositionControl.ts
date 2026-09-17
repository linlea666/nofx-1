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
  return { mapping, unknown }
}
