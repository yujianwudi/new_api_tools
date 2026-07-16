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
const PENDING_MUTATION_STORAGE_KEY = 'new_api_tools.pending_mutations.v1'
const DEFAULT_OPERATION_IDENTIFIER = 'legacy'
const OPERATION_IDENTIFIER_PATTERN = /^[a-zA-Z0-9._:-]{1,128}$/
const FINGERPRINT_PATTERN = /^[a-f0-9]{32}$/
const UUID_PATTERN = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i

export type OperationReconciliationStatus =
  | 'not_found'
  | 'pending'
  | 'succeeded'
  | 'failed'
  | 'denied'
  | 'cancelled'

export interface PendingMutationRecord {
  operationIdentifier: string
  fingerprint: string
  key: string
  action: string
  targetType: string
  targetId: string
  reconciliationRequired: true
  createdAt: number
}

export interface PendingMutationSnapshot<Payload extends Record<string, unknown>> extends PendingMutationRecord {
  payload: Readonly<Payload>
}

export interface OperationReconciliation {
  status: OperationReconciliationStatus
  action: string
  target_type: string
  target_id: string
  audit_id: number
}

export interface OperationReleaseCandidate extends OperationReconciliation {
  pendingKey: string
}

export type OperationReconciliationDecision = 'applied' | 'released' | 'locked'
export type OperationReconciliationAction = 'clear' | 'confirm_release' | 'keep_locked'

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

function sameStoredOperations(
  left: readonly StoredIdempotencyOperation[],
  right: readonly StoredIdempotencyOperation[],
): boolean {
  return left.length === right.length && left.every((operation, index) => {
    const other = right[index]
    return operation.operationIdentifier === other.operationIdentifier
      && operation.fingerprint === other.fingerprint
      && operation.key === other.key
  })
}

function writeStoredOperations(operations: StoredIdempotencyOperation[]): boolean {
  const storage = getSessionStorage()
  if (!storage) return false
  try {
    if (operations.length === 0) {
      storage.removeItem(STORAGE_KEY)
    } else {
      storage.setItem(STORAGE_KEY, JSON.stringify(operations))
    }
    return sameStoredOperations(readStoredOperations(), operations)
  } catch {
    // Idempotency still works for the current mount when storage is blocked.
    return false
  }
}

function isPendingMutationRecord(value: unknown): value is PendingMutationRecord {
  if (!value || typeof value !== 'object') return false
  const pending = value as Partial<PendingMutationRecord>
  return typeof pending.operationIdentifier === 'string'
    && OPERATION_IDENTIFIER_PATTERN.test(pending.operationIdentifier)
    && typeof pending.fingerprint === 'string'
    && FINGERPRINT_PATTERN.test(pending.fingerprint)
    && typeof pending.key === 'string'
    && UUID_PATTERN.test(pending.key)
    && typeof pending.action === 'string'
    && OPERATION_IDENTIFIER_PATTERN.test(pending.action)
    && typeof pending.targetType === 'string'
    && OPERATION_IDENTIFIER_PATTERN.test(pending.targetType)
    && typeof pending.targetId === 'string'
    && pending.targetId.length > 0
    && pending.targetId.length <= 128
    && pending.reconciliationRequired === true
    && typeof pending.createdAt === 'number'
    && Number.isFinite(pending.createdAt)
    && pending.createdAt > 0
}

function readPendingMutations(): PendingMutationRecord[] {
  const storage = getSessionStorage()
  if (!storage) return []
  try {
    const parsed: unknown = JSON.parse(storage.getItem(PENDING_MUTATION_STORAGE_KEY) || '[]')
    return Array.isArray(parsed) ? parsed.filter(isPendingMutationRecord) : []
  } catch {
    return []
  }
}

function samePendingMutations(
  left: readonly PendingMutationRecord[],
  right: readonly PendingMutationRecord[],
): boolean {
  return left.length === right.length && left.every((pending, index) => {
    const other = right[index]
    return pending.operationIdentifier === other.operationIdentifier
      && pending.fingerprint === other.fingerprint
      && pending.key === other.key
      && pending.action === other.action
      && pending.targetType === other.targetType
      && pending.targetId === other.targetId
      && pending.reconciliationRequired === other.reconciliationRequired
      && pending.createdAt === other.createdAt
  })
}

function writePendingMutations(pendingMutations: PendingMutationRecord[]): boolean {
  const storage = getSessionStorage()
  if (!storage) return false
  try {
    if (pendingMutations.length === 0) {
      storage.removeItem(PENDING_MUTATION_STORAGE_KEY)
    } else {
      storage.setItem(PENDING_MUTATION_STORAGE_KEY, JSON.stringify(pendingMutations))
    }
    return samePendingMutations(readPendingMutations(), pendingMutations)
  } catch {
    return false
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

function cloneAndFreezePayload<Payload extends Record<string, unknown>>(payload: Payload): Readonly<Payload> {
  const seen = new WeakMap<object, object>()
  const cloneValue = (value: unknown): unknown => {
    if (value === null || typeof value !== 'object') return value
    const existing = seen.get(value)
    if (existing) return existing
    if (Array.isArray(value)) {
      const clone: unknown[] = []
      seen.set(value, clone)
      for (const item of value) clone.push(cloneValue(item))
      return Object.freeze(clone)
    }
    const clone: Record<string, unknown> = {}
    seen.set(value, clone)
    for (const [key, item] of Object.entries(value)) clone[key] = cloneValue(item)
    return Object.freeze(clone)
  }
  return cloneValue(payload) as Readonly<Payload>
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

export function clearAllIdempotencyKeys(): void {
  const storage = getSessionStorage()
  if (!storage) return
  try {
    storage.removeItem(STORAGE_KEY)
    storage.removeItem(PENDING_MUTATION_STORAGE_KEY)
  } catch {
    // Storage can be blocked in hardened browser contexts.
  }
}

// Authentication changes must discard reusable key ownership without erasing
// an uncertain mutation's durable lock. The pending marker contains no request
// payload or reason and is required so a later login cannot submit the same
// target again before reconciling the original audit chain.
export function clearReusableIdempotencyKeys(): void {
  const storage = getSessionStorage()
  if (!storage) return
  try {
    storage.removeItem(STORAGE_KEY)
  } catch {
    // Storage can be blocked in hardened browser contexts.
  }
}

export function parseUserMutationTargetId(targetId: string): number {
  if (typeof targetId !== 'string' || !/^[1-9]\d*$/.test(targetId)) {
    throw new Error('User mutation target must be a positive safe integer')
  }
  const userId = Number(targetId)
  if (!Number.isSafeInteger(userId) || userId <= 0) {
    throw new Error('User mutation target must be a positive safe integer')
  }
  return userId
}

export function parseUserMutationOperationTargetId(operationIdentifier: string, targetId: string): number {
  const userId = parseUserMutationTargetId(targetId)
  if (operationIdentifier !== userMutationOperationIdentifier(userId)) {
    throw new Error('User mutation operation identifier does not match its target')
  }
  return userId
}

export function userMutationOperationIdentifier(userId: number): string {
  if (!Number.isSafeInteger(userId) || userId <= 0) {
    throw new Error('User mutation target must be a positive safe integer')
  }
  return `control-plane.user:${userId}`
}

export function listPendingMutations(): PendingMutationRecord[] {
  return readPendingMutations().map(pending => ({ ...pending }))
}

export function indexPendingMutationsByOperation<Pending extends PendingMutationRecord>(
  pendingMutations: readonly Pending[],
  targetType?: string,
): Map<string, Pending> {
  const pendingByOperation = new Map<string, Pending>()
  for (const pending of pendingMutations) {
    if (targetType === undefined || pending.targetType === targetType) {
      pendingByOperation.set(pending.operationIdentifier, pending)
    }
  }
  return pendingByOperation
}

export function getPendingMutation(operationIdentifier: string): PendingMutationRecord | null {
  const identifier = normalizeOperationIdentifier(operationIdentifier)
  const pending = readPendingMutations().find(item => item.operationIdentifier === identifier)
  return pending ? { ...pending } : null
}

export function beginPendingMutation<Payload extends Record<string, unknown>>(input: {
  operationIdentifier: string
  fingerprint: string
  action: string
  targetType: string
  targetId: string
  payload: Payload
}): PendingMutationSnapshot<Payload> {
  const operationIdentifier = normalizeOperationIdentifier(input.operationIdentifier)
  const existing = getPendingMutation(operationIdentifier)
  if (existing) {
    throw new Error(`Pending mutation already exists for ${operationIdentifier}`)
  }
  if (!OPERATION_IDENTIFIER_PATTERN.test(input.action)) {
    throw new Error('Invalid pending mutation action')
  }
  if (!OPERATION_IDENTIFIER_PATTERN.test(input.targetType)) {
    throw new Error('Invalid pending mutation target type')
  }
  const targetId = input.targetId.trim()
  if (!targetId || targetId.length > 128) {
    throw new Error('Invalid pending mutation target id')
  }
  const payload = cloneAndFreezePayload(input.payload)

  const fingerprintDigest = digestFingerprint(input.fingerprint)
  const ownershipAlreadyExisted = readStoredOperations().some(operation => (
    operation.operationIdentifier === operationIdentifier
    && operation.fingerprint === fingerprintDigest
  ))
  const operationRef: IdempotencyOperationRef = { current: null }
  const key = getOrCreateIdempotencyKey(operationRef, input.fingerprint, operationIdentifier)
  if (!operationRef.current) {
    throw new Error('Failed to create pending mutation ownership')
  }
  const record: PendingMutationRecord = {
    operationIdentifier,
    fingerprint: operationRef.current.fingerprint,
    key,
    action: input.action,
    targetType: input.targetType,
    targetId,
    reconciliationRequired: true,
    createdAt: Date.now(),
  }
  const remaining = readPendingMutations().filter(item => item.operationIdentifier !== operationIdentifier)
  if (!writePendingMutations([...remaining, record])) {
    if (!ownershipAlreadyExisted) {
      clearIdempotencyKey(operationRef, operationIdentifier)
    }
    throw new Error('无法在浏览器会话中持久化操作锁，已阻止提交')
  }
  return Object.freeze({ ...record, payload })
}

export function clearPendingMutation(pending: PendingMutationRecord): boolean {
  const operationMatches = (operation: StoredIdempotencyOperation) => (
    operation.operationIdentifier === pending.operationIdentifier
    && operation.fingerprint === pending.fingerprint
    && operation.key === pending.key
  )
  const remainingOperations = readStoredOperations().filter(operation => !operationMatches(operation))
  if (!writeStoredOperations(remainingOperations) || readStoredOperations().some(operationMatches)) {
    return false
  }

  // Delete the durable lock only after the reusable ownership has been
  // removed and verified. A crash between the two writes therefore leaves a
  // conservative pending marker rather than an unlocked stale key.
  const pendingMatches = (item: PendingMutationRecord) => (
    item.operationIdentifier === pending.operationIdentifier && item.key === pending.key
  )
  const remainingPending = readPendingMutations().filter(item => !pendingMatches(item))
  return writePendingMutations(remainingPending) && !readPendingMutations().some(pendingMatches)
}

export function pendingMutationHasPayload<Payload extends Record<string, unknown>>(
  pending: PendingMutationRecord | PendingMutationSnapshot<Payload>,
): pending is PendingMutationSnapshot<Payload> {
  return 'payload' in pending
}

export function operationReconciliationDecision(
  status: OperationReconciliationStatus,
): OperationReconciliationDecision {
  switch (status) {
    case 'succeeded':
      return 'applied'
    case 'failed':
    case 'denied':
      return 'released'
    case 'not_found':
    case 'pending':
    case 'cancelled':
      return 'locked'
  }
}

export function operationReconciliationAction(
  status: OperationReconciliationStatus,
  releaseConfirmed = false,
): OperationReconciliationAction {
  const decision = operationReconciliationDecision(status)
  if (decision === 'applied') return 'clear'
  if (decision === 'released') return releaseConfirmed ? 'clear' : 'confirm_release'
  return 'keep_locked'
}

export function bindOperationReleaseCandidate(
  pending: PendingMutationRecord,
  reconciliation: OperationReconciliation,
): OperationReleaseCandidate {
  if (reconciliation.action !== pending.action
    || reconciliation.target_type !== pending.targetType
    || reconciliation.target_id !== pending.targetId) {
    throw new Error('Operation release candidate target mismatch')
  }
  return Object.freeze({ ...reconciliation, pendingKey: pending.key })
}

export function operationReleaseCandidateMatches(
  candidate: OperationReleaseCandidate | null | undefined,
  pending: PendingMutationRecord | null | undefined,
): candidate is OperationReleaseCandidate {
  if (!candidate || !pending) return false
  return candidate.pendingKey === pending.key
    && candidate.action === pending.action
    && candidate.target_type === pending.targetType
    && candidate.target_id === pending.targetId
}

export async function fetchOperationReconciliation(
  apiUrl: string,
  headers: Record<string, string>,
  pending: PendingMutationRecord,
  signal?: AbortSignal,
): Promise<OperationReconciliation> {
  const response = await fetch(
    `${apiUrl}/api/control-plane/operations/${encodeURIComponent(pending.key)}`,
    { headers, signal, cache: 'no-store' },
  )
  if (response.status === 404) {
    return {
      status: 'not_found',
      action: pending.action,
      target_type: pending.targetType,
      target_id: pending.targetId,
      audit_id: 0,
    }
  }
  const body = await response.json()
  if (!response.ok || !body.success || !body.data) {
    throw new Error(body.error?.message || body.message || `HTTP ${response.status}`)
  }
  const data = body.data as Partial<OperationReconciliation>
  const validStatus = data.status === 'pending' || data.status === 'succeeded' || data.status === 'failed'
    || data.status === 'denied' || data.status === 'cancelled'
  if (!validStatus || typeof data.action !== 'string' || typeof data.target_type !== 'string'
    || typeof data.target_id !== 'string' || !Number.isSafeInteger(data.audit_id) || (data.audit_id ?? 0) <= 0) {
    throw new Error('Invalid operation reconciliation response')
  }
  if (data.action !== pending.action || data.target_type !== pending.targetType || data.target_id !== pending.targetId) {
    throw new Error('Operation reconciliation target mismatch')
  }
  return data as OperationReconciliation
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
