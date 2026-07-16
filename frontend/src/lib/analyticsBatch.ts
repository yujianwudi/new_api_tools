export interface OwnedAnalyticsBatchRun {
  controller: AbortController
  requestController: AbortController | null
  timeout: number | null
}

export interface AnalyticsBatchRunRef<Run extends OwnedAnalyticsBatchRun> {
  current: Run | null
}

export async function refreshAnalyticsBatchState(
  fetchSyncStatus: () => Promise<unknown>,
  fetchAnalytics: () => Promise<unknown>,
): Promise<void> {
  const results = await Promise.allSettled([
    Promise.resolve().then(fetchSyncStatus),
    Promise.resolve().then(fetchAnalytics),
  ])
  const rejected = results.find((result): result is PromiseRejectedResult => result.status === 'rejected')
  if (rejected) throw rejected.reason
}

export function replaceAnalyticsBatchRun<Run extends OwnedAnalyticsBatchRun>(
  runRef: AnalyticsBatchRunRef<Run>,
  nextRun: Run,
  clearTimeoutHandle: (timeout: number) => void,
): void {
  const previousRun = runRef.current
  previousRun?.controller.abort()
  previousRun?.requestController?.abort()
  if (previousRun?.timeout !== null && previousRun?.timeout !== undefined) {
    clearTimeoutHandle(previousRun.timeout)
    previousRun.timeout = null
  }
  runRef.current = nextRun
}

export function cleanupAnalyticsBatchRun<Run extends OwnedAnalyticsBatchRun>(
  runRef: AnalyticsBatchRunRef<Run>,
  run: Run,
  clearTimeoutHandle: (timeout: number) => void,
): boolean {
  if (run.timeout !== null) {
    clearTimeoutHandle(run.timeout)
    run.timeout = null
  }
  run.requestController?.abort()
  run.requestController = null

  if (runRef.current !== run) return false
  runRef.current = null
  return true
}
