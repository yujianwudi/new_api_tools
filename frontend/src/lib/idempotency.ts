export interface IdempotencyOperation {
  fingerprint: string
  key: string
  operationIdentifier?: string
}

export type IdempotencyOperationRef = {
  current: IdempotencyOperation | null
}

interface StoredIdempotencyOperation {
  operationIdentifier: string
  fingerprint: string
  key: string
}

const STORAGE_KEY = 'new_api_tools.pending_idempotency.v1'
const DEFAULT_OPERATION_IDENTIFIER = 'legacy'
const OPERATION_IDENTIFIER_PATTERN = /^[a-zA-Z0-9._:-]{1,128}$/
const FINGERPRINT_PATTERN = /^[a-f0-9]{32}$/
const UUID_PATTERN = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i

function getSessionStorage(): Storage | null {
  try {
    return typeof window === 'undefined' ? null : window.sessionStorage
  } catch {
    return null
  }
}

function isStoredOperation(value: unknown): value is StoredIdempotencyOperation {
  if (!value || typeof value !== 'object') return false
  const operation = value as Partial<StoredIdempotencyOperation>
  return typeof operation.operationIdentifier === 'string'
    && OPERATION_IDENTIFIER_PATTERN.test(operation.operationIdentifier)
    && typeof operation.fingerprint === 'string'
    && FINGERPRINT_PATTERN.test(operation.fingerprint)
    && typeof operation.key === 'string'
    && UUID_PATTERN.test(operation.key)
}

function readStoredOperations(): StoredIdempotencyOperation[] {
  const storage = getSessionStorage()
  if (!storage) return []
  try {
    const parsed: unknown = JSON.parse(storage.getItem(STORAGE_KEY) || '[]')
    return Array.isArray(parsed) ? parsed.filter(isStoredOperation) : []
  } catch {
    return []
  }
}

function writeStoredOperations(operations: StoredIdempotencyOperation[]): void {
  const storage = getSessionStorage()
  if (!storage) return
  try {
    if (operations.length === 0) {
      storage.removeItem(STORAGE_KEY)
      return
    }
    storage.setItem(STORAGE_KEY, JSON.stringify(operations))
  } catch {
    // Idempotency still works for the current mount when storage is blocked.
  }
}

// Store only an opaque digest. Mutation payloads and reasons must never be
// persisted in browser storage merely to recover an idempotency key.
function digestFingerprint(value: string): string {
  const seeds = [0x811c9dc5, 0x9e3779b9, 0x85ebca6b, 0xc2b2ae35]
  const hashes = seeds.map((seed, index) => {
    let hash = seed >>> 0
    const multiplier = [0x01000193, 0x27d4eb2d, 0x85ebca6b, 0x165667b1][index]
    for (let offset = 0; offset < value.length; offset++) {
      hash = Math.imul(hash ^ value.charCodeAt(offset), multiplier)
      hash ^= hash >>> 13
    }
    hash = Math.imul(hash ^ value.length, multiplier)
    return (hash >>> 0).toString(16).padStart(8, '0')
  })
  return hashes.join('')
}

function normalizeOperationIdentifier(operationIdentifier?: string): string {
  if (operationIdentifier === undefined) return DEFAULT_OPERATION_IDENTIFIER
  if (!OPERATION_IDENTIFIER_PATTERN.test(operationIdentifier)) {
    throw new Error('Invalid idempotency operation identifier')
  }
  return operationIdentifier
}

export function createIdempotencyKey(): string {
  if (!globalThis.crypto?.randomUUID) {
    throw new Error('Secure UUID generation is unavailable in this browser')
  }
  return globalThis.crypto.randomUUID()
}

// A retry of the same logical request reuses its key. If the request payload
// changes, it is a new operation and receives a new key automatically.
export function getOrCreateIdempotencyKey(
  operationRef: IdempotencyOperationRef,
  fingerprint: string,
  operationIdentifier?: string,
): string {
  const identifier = normalizeOperationIdentifier(operationIdentifier)
  const fingerprintDigest = digestFingerprint(fingerprint)
  if (
    operationRef.current?.fingerprint === fingerprintDigest
    && (operationRef.current.operationIdentifier || DEFAULT_OPERATION_IDENTIFIER) === identifier
  ) {
    return operationRef.current.key
  }

  const storedOperations = readStoredOperations()
  const pendingOperation = storedOperations.find((operation) => (
    operation.operationIdentifier === identifier
    && operation.fingerprint === fingerprintDigest
  ))
  if (pendingOperation) {
    operationRef.current = pendingOperation
    return pendingOperation.key
  }

  const key = createIdempotencyKey()
  const operation: StoredIdempotencyOperation = {
    operationIdentifier: identifier,
    fingerprint: fingerprintDigest,
    key,
  }
  operationRef.current = operation
  writeStoredOperations([...storedOperations, operation])
  return key
}

export function clearIdempotencyKey(
  operationRef: IdempotencyOperationRef,
  operationIdentifier?: string,
): void {
  const currentOperation = operationRef.current
  if (!currentOperation) return

  const currentIdentifier = currentOperation.operationIdentifier || DEFAULT_OPERATION_IDENTIFIER
  const identifier = operationIdentifier !== undefined
    ? normalizeOperationIdentifier(operationIdentifier)
    : currentIdentifier
  if (identifier !== currentIdentifier) return

  operationRef.current = null
  const remainingOperations = readStoredOperations().filter((operation) => {
    if (operation.operationIdentifier !== identifier) return true
    return operation.fingerprint !== currentOperation.fingerprint || operation.key !== currentOperation.key
  })
  writeStoredOperations(remainingOperations)
}

export function idempotencyHeader(key: string): Record<'Idempotency-Key', string> {
  return { 'Idempotency-Key': key }
}

export function mutationResponseRequiresReconciliation(value: unknown): boolean {
  if (!value || typeof value !== 'object') return false
  const error = (value as { error?: unknown }).error
  if (!error || typeof error !== 'object') return false
  const details = (error as { details?: unknown }).details
  return Boolean(
    details
    && typeof details === 'object'
    && (details as { do_not_retry?: unknown }).do_not_retry === true,
  )
}
