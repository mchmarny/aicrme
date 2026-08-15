import { describe, expect, it } from 'vitest'
import { detectGap, mergeEvents, type AicrEvent } from './useEvents'

const ev = (id: number, message: string): AicrEvent => ({
  id, at: '2026-08-13T00:00:00Z', kind: 'log', level: 'info', message,
})

describe('mergeEvents', () => {
  it('appends new events in id order', () => {
    expect(mergeEvents([ev(1, 'a')], ev(2, 'b')).map(e => e.id)).toEqual([1, 2])
  })

  it('drops duplicates delivered by a reconnect replay', () => {
    expect(mergeEvents([ev(1, 'a'), ev(2, 'b')], ev(2, 'b')).map(e => e.id)).toEqual([1, 2])
  })

  it('reorders an out-of-order delivery', () => {
    expect(mergeEvents([ev(2, 'b')], ev(1, 'a')).map(e => e.id)).toEqual([1, 2])
  })
})

describe('detectGap', () => {
  it('is not a gap for the first event on a fresh connection', () => {
    expect(detectGap(0, 1)).toBe(false)
  })

  it('is not a gap when the replay ring already evicted earlier events', () => {
    // lastId still 0 (nothing seen yet); the first delivered id can be far
    // above 1 if the ring is full. That is not a hole this subscriber missed.
    expect(detectGap(0, 500)).toBe(false)
  })

  it('is not a gap for the next contiguous id', () => {
    expect(detectGap(5, 6)).toBe(false)
  })

  it('is a gap when an id is skipped', () => {
    expect(detectGap(5, 8)).toBe(true)
  })

  it('is not a gap for a duplicate or out-of-order id', () => {
    expect(detectGap(5, 3)).toBe(false)
    expect(detectGap(5, 5)).toBe(false)
  })
})
