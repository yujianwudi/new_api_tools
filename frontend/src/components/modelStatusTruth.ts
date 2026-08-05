export type ModelHealthStatus = 'green' | 'yellow' | 'red' | 'unknown'
export type ModelSourceState = 'fresh' | 'stale' | 'empty' | 'unavailable' | 'pending' | 'unsupported'
export type CombinedHealth = 'healthy' | 'degraded' | 'unhealthy' | 'observing' | 'unknown'
export type SnapshotState = 'fresh' | 'unavailable'

export interface SourceSnapshot<T> {
  data: T
  state: SnapshotState
  window: string
  fetchedAt: string | null
  error: string
}

// source_state is the authoritative freshness contract. Older or malformed
// responses that omit it must fail closed; request counts and legacy colours
// cannot prove that the evidence is current.
export function authoritativeTrafficSourceState(source?: ModelSourceState): ModelSourceState {
  return source ?? 'unavailable'
}

type EvidenceHealth = 'healthy' | 'degraded' | 'unhealthy' | 'unknown' | string

function authoritativeHealth(health: EvidenceHealth, source: ModelSourceState): EvidenceHealth {
  return source === 'fresh' ? health : 'unknown'
}

export function combineFreshSourceHealth(
  trafficHealth: EvidenceHealth,
  trafficSource: ModelSourceState,
  probeHealth: EvidenceHealth,
  probeSource: ModelSourceState,
): CombinedHealth {
  const traffic = authoritativeHealth(trafficHealth, trafficSource)
  const probe = authoritativeHealth(probeHealth, probeSource)
  if (traffic === 'unhealthy' || probe === 'unhealthy') return 'unhealthy'
  if (traffic === 'degraded' || probe === 'degraded') return 'degraded'
  if (trafficSource === 'fresh' && probeSource === 'fresh' && traffic === 'healthy' && probe === 'healthy') return 'healthy'
  if (traffic === 'healthy' || probe === 'healthy') return 'observing'
  return 'unknown'
}

export function effectiveEmbedStatus(model: { current_status: ModelHealthStatus; source_state?: ModelSourceState }): ModelHealthStatus {
  return model.source_state === 'fresh' ? model.current_status : 'unknown'
}

export function effectiveEmbedRate(model: {
  success_rate: number | null
  current_status: ModelHealthStatus
  source_state?: ModelSourceState
}): number | null {
  if (effectiveEmbedStatus(model) === 'unknown') return null
  return typeof model.success_rate === 'number' && Number.isFinite(model.success_rate) ? model.success_rate : null
}

export function shouldCommitStatusSnapshot(
  activeRequestId: number,
  requestId: number,
  activeWindow: string,
  requestedWindow: string,
  aborted: boolean,
): boolean {
  return !aborted && activeRequestId === requestId && activeWindow === requestedWindow
}

interface SettleStatusSourceOptions<T> {
  promise: Promise<T>
  unavailableData: T
  window: string
  fallbackError: string
  canCommit: () => boolean
  fetchedAt?: (data: T) => string | null | undefined
  commit: (snapshot: SourceSnapshot<T>) => void
}

export async function settleStatusSource<T>(options: SettleStatusSourceOptions<T>): Promise<string> {
  try {
    const data = await options.promise
    if (options.canCommit()) {
      options.commit({
        data,
        state: 'fresh',
        window: options.window,
        fetchedAt: options.fetchedAt?.(data) ?? new Date().toISOString(),
        error: '',
      })
    }
    return ''
  } catch (error) {
    const message = error instanceof Error && error.message ? error.message : options.fallbackError
    if (options.canCommit()) {
      options.commit({
        data: options.unavailableData,
        state: 'unavailable',
        window: options.window,
        fetchedAt: new Date().toISOString(),
        error: message,
      })
    }
    return message
  }
}
