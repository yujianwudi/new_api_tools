import { describe, expect, it } from 'vitest'
import {
  authoritativeTrafficSourceState,
  combineFreshSourceHealth,
  effectiveEmbedRate,
  effectiveEmbedStatus,
  settleStatusSource,
  shouldCommitStatusSnapshot,
  type SourceSnapshot,
} from './modelStatusTruth'

function deferred<T>() {
  let resolve!: (value: T) => void
  const promise = new Promise<T>(next => { resolve = next })
  return { promise, resolve }
}

describe('model status truth table', () => {
  it('does not infer fresh traffic evidence from a legacy response', () => {
    expect(authoritativeTrafficSourceState()).toBe('unavailable')
    expect(authoritativeTrafficSourceState('stale')).toBe('stale')
    expect(authoritativeTrafficSourceState('fresh')).toBe('fresh')
  })

  it('only reports dual-source healthy when both sources are fresh and healthy', () => {
    expect(combineFreshSourceHealth('healthy', 'fresh', 'healthy', 'fresh')).toBe('healthy')
    expect(combineFreshSourceHealth('healthy', 'stale', 'healthy', 'fresh')).toBe('observing')
    expect(combineFreshSourceHealth('healthy', 'fresh', 'healthy', 'unavailable')).toBe('observing')
    expect(combineFreshSourceHealth('healthy', 'stale', 'healthy', 'unavailable')).toBe('unknown')
  })

  it('does not let stale legacy green make the public embed green', () => {
    expect(effectiveEmbedStatus({ current_status: 'green', source_state: 'stale' })).toBe('unknown')
    expect(effectiveEmbedStatus({ current_status: 'green' })).toBe('unknown')
    expect(effectiveEmbedStatus({ current_status: 'green', source_state: 'fresh' })).toBe('green')
  })

  it('renders empty or unavailable success rates as N/A instead of numeric zero', () => {
    expect(effectiveEmbedRate({ current_status: 'green', source_state: 'empty', success_rate: 0 })).toBeNull()
    expect(effectiveEmbedRate({ current_status: 'unknown', source_state: 'fresh', success_rate: null })).toBeNull()
    expect(effectiveEmbedRate({ current_status: 'green', source_state: 'fresh', success_rate: 99.5 })).toBe(99.5)
  })

  it('settles traffic and probe independently so one failed source is invalidated immediately', async () => {
    const slowTraffic = deferred<string[]>()
    let trafficSnapshot: SourceSnapshot<string[]> = {
      data: ['old-traffic'], state: 'fresh', window: '24h', fetchedAt: 'old', error: '',
    }
    let probeSnapshot: SourceSnapshot<string[] | null> = {
      data: ['old-probe'], state: 'fresh', window: '24h', fetchedAt: 'old', error: '',
    }
    const trafficSettlement = settleStatusSource({
      promise: slowTraffic.promise,
      unavailableData: [],
      window: '24h',
      fallbackError: 'traffic failed',
      canCommit: () => true,
      commit: snapshot => { trafficSnapshot = snapshot },
    })
    const probeSettlement = settleStatusSource<string[] | null>({
      promise: Promise.reject(new Error('probe failed')),
      unavailableData: null,
      window: '24h',
      fallbackError: 'probe failed',
      canCommit: () => true,
      commit: snapshot => { probeSnapshot = snapshot },
    })

    await probeSettlement
    expect(probeSnapshot.state).toBe('unavailable')
    expect(probeSnapshot.data).toBeNull()
    expect(trafficSnapshot.data).toEqual(['old-traffic'])

    slowTraffic.resolve(['new-traffic'])
    await trafficSettlement
    expect(trafficSnapshot.state).toBe('fresh')
    expect(trafficSnapshot.data).toEqual(['new-traffic'])
  })

  it('rejects late responses from an old request or old window', async () => {
    let activeRequestId = 1
    let activeWindow = '24h'
    const slow = deferred<string[]>()
    let snapshot: SourceSnapshot<string[]> = {
      data: ['window-b'], state: 'fresh', window: '1h', fetchedAt: 'new', error: '',
    }
    const settlement = settleStatusSource({
      promise: slow.promise,
      unavailableData: [],
      window: '24h',
      fallbackError: 'failed',
      canCommit: () => shouldCommitStatusSnapshot(activeRequestId, 1, activeWindow, '24h', false),
      commit: next => { snapshot = next },
    })
    activeRequestId = 2
    activeWindow = '1h'
    slow.resolve(['late-window-a'])
    await settlement

    expect(snapshot.window).toBe('1h')
    expect(snapshot.data).toEqual(['window-b'])
    expect(shouldCommitStatusSnapshot(2, 1, '1h', '24h', false)).toBe(false)
    expect(shouldCommitStatusSnapshot(2, 2, '1h', '1h', false)).toBe(true)
  })
})
