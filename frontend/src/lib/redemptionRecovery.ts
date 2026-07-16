export interface AppliedRedemptionResult {
  keys: string[]
  count: number
}

interface MutationErrorEnvelope {
  error?: {
    code?: unknown
    details?: unknown
  }
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

// The backend only exposes plaintext codes in this one recovery envelope. Be
// deliberately strict before placing any value into transient React state.
export function extractAppliedRedemptionResult(value: unknown): AppliedRedemptionResult | null {
  if (!isRecord(value)) return null
  const envelope = value as MutationErrorEnvelope
  if (envelope.error?.code !== 'AUDIT_OUTCOME_PERSIST_FAILED') return null
  if (!isRecord(envelope.error.details)) return null

  const details = envelope.error.details
  if (details.operation_applied !== true || details.do_not_retry !== true || !isRecord(details.result)) {
    return null
  }

  const result = details.result
  if (!Number.isInteger(result.count) || (result.count as number) < 1 || (result.count as number) > 100) {
    return null
  }
  if (!Array.isArray(result.keys) || result.keys.length !== result.count) return null

  const keys = result.keys
  if (!keys.every((key) => typeof key === 'string' && key.length > 0 && key.length <= 512)) return null
  if (new Set(keys).size !== keys.length) return null
  return { keys: [...keys], count: result.count as number }
}
