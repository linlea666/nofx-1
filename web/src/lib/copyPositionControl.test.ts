import { describe, expect, it } from 'vitest'
import { copyPositionControl } from './copyPositionControl'
import type { CopyTradePositionMapping } from '../types'

const position = {
  symbol: 'BTCUSDT',
  side: 'short',
  position_key: 'BTCUSDT|short|cross|same-pos-id',
}
const paused = {
  leader_pos_id: 'original',
  symbol: 'BTCUSDT',
  side: 'short',
  status: 'manual_stopped',
  custody_state: 'MANAGED',
  current_position_key: position.position_key,
  can_resume: true,
} as CopyTradePositionMapping

describe('position following controls', () => {
  it('offers resume only for the original verified position', () => {
    expect(copyPositionControl(position, [paused]).mapping?.can_resume).toBe(
      true
    )
  })
  it('does not label a new manual position paused just because symbol and side match', () => {
    expect(
      copyPositionControl(position, [
        {
          ...paused,
          custody_state: 'RELEASED',
          current_position_key: undefined,
        },
      ])
    ).toMatchObject({ mapping: undefined, unknown: false })
  })
  it('waits for ownership evidence instead of claiming a position is manual', () => {
    expect(copyPositionControl(position, undefined).unknown).toBe(true)
    expect(
      copyPositionControl(position, [
        { ...paused, current_position_key: undefined },
      ]).unknown
    ).toBe(true)
    expect(
      copyPositionControl(position, [{ ...paused, custody_state: 'UNKNOWN' }])
        .mapping
    ).toBeUndefined()
  })
  it('does not choose between conflicting custody claims', () => {
    expect(
      copyPositionControl(position, [
        paused,
        { ...paused, leader_pos_id: 'another' },
      ])
    ).toMatchObject({ mapping: undefined, unknown: true })
  })
})

it('exposes leader exit without claiming protection custody of a later manual position', () => {
  const c = copyPositionControl(position, [
    {
      ...paused,
      status: 'stopped_by_risk',
      custody_state: 'RELEASED',
      current_position_key: undefined,
      can_resume: false,
      leader_exit_enabled: true,
    },
  ])
  expect(c.mapping).toBeUndefined()
  expect(c.exitMapping?.leader_pos_id).toBe('original')
})
it('does not authorize exits for a whole-cycle skipped independent manual position', () => {
  const c = copyPositionControl(position, [
    {
      ...paused,
      status: 'ignored',
      custody_state: 'RELEASED',
      current_position_key: undefined,
      can_resume: false,
      leader_exit_enabled: false,
      follow_reason: '已有独立仓位，本轮跳过',
    },
  ])
  expect(c.exitMapping).toBeUndefined()
  expect(c.followLabel).toBe('本轮跳过')
})
