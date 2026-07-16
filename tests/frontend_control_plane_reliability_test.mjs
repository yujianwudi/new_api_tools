import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import vm from 'node:vm'
import { webcrypto } from 'node:crypto'
import ts from '../frontend/node_modules/typescript/lib/typescript.js'

const read = (path) => readFileSync(new URL(`../${path}`, import.meta.url), 'utf8')

function loadTypeScriptModule(path, globals = {}) {
  const source = read(path)
  const output = ts.transpileModule(source, {
    compilerOptions: {
      module: ts.ModuleKind.CommonJS,
      target: ts.ScriptTarget.ES2020,
    },
    fileName: path,
  }).outputText
  const module = { exports: {} }
  const context = vm.createContext({
    ...globals,
    module,
    exports: module.exports,
  })
  new vm.Script(output, { filename: path }).runInContext(context)
  return module.exports
}

class MemoryStorage {
  #values = new Map()

  getItem(key) {
    return this.#values.has(key) ? this.#values.get(key) : null
  }

  setItem(key, value) {
    this.#values.set(key, String(value))
  }

  removeItem(key) {
    this.#values.delete(key)
  }
}

class FaultInjectingStorage extends MemoryStorage {
  ignoredSetKeys = new Set()
  ignoredRemoveKeys = new Set()
  operations = []

  setItem(key, value) {
    this.operations.push(`set:${key}`)
    if (this.ignoredSetKeys.has(key)) return
    super.setItem(key, value)
  }

  removeItem(key) {
    this.operations.push(`remove:${key}`)
    if (this.ignoredRemoveKeys.has(key)) return
    super.removeItem(key)
  }
}

const sessionStorage = new MemoryStorage()
const idempotency = loadTypeScriptModule('frontend/src/lib/idempotency.ts', {
  crypto: webcrypto,
  window: { sessionStorage },
})

const firstRef = { current: null }
const secondRef = { current: null }
const firstKey = idempotency.getOrCreateIdempotencyKey(firstRef, 'payload-a', 'shared.operation')
const secondKey = idempotency.getOrCreateIdempotencyKey(secondRef, 'payload-b', 'shared.operation')
idempotency.clearIdempotencyKey(firstRef, 'shared.operation')

const reloadedSecondRef = { current: null }
assert.equal(
  idempotency.getOrCreateIdempotencyKey(reloadedSecondRef, 'payload-b', 'shared.operation'),
  secondKey,
  'clearing one fingerprint must preserve other pending fingerprints in the same operation namespace',
)
const reloadedFirstRef = { current: null }
assert.notEqual(
  idempotency.getOrCreateIdempotencyKey(reloadedFirstRef, 'payload-a', 'shared.operation'),
  firstKey,
  'the completed fingerprint must receive a fresh key',
)

const emptyRef = { current: null }
idempotency.clearIdempotencyKey(emptyRef, 'shared.operation')
const stillPendingSecondRef = { current: null }
assert.equal(
  idempotency.getOrCreateIdempotencyKey(stillPendingSecondRef, 'payload-b', 'shared.operation'),
  secondKey,
  'an empty ref must not clear an entire operation namespace',
)
assert.equal(
  idempotency.mutationResponseRequiresReconciliation({ error: { details: { do_not_retry: true } } }),
  true,
  'do_not_retry mutation responses must keep their idempotency key for reconciliation',
)
assert.equal(idempotency.mutationResponseRequiresReconciliation({ error: { details: {} } }), false)

const IDEMPOTENCY_STORAGE_KEY = 'new_api_tools.pending_idempotency.v1'
const PENDING_MUTATION_STORAGE_KEY = 'new_api_tools.pending_mutations.v1'
const pendingStorage = new MemoryStorage()
const pendingIdempotency = loadTypeScriptModule('frontend/src/lib/idempotency.ts', {
  crypto: webcrypto,
  window: { sessionStorage: pendingStorage },
})
const sourcePayload = {
  userId: 42,
  reason: 'operator secret reason',
  nested: { confirmText: 'confirm-delete' },
}
const pendingSnapshot = pendingIdempotency.beginPendingMutation({
  operationIdentifier: pendingIdempotency.userMutationOperationIdentifier(42),
  fingerprint: JSON.stringify({ action: 'user.delete', target: 42, reason: sourcePayload.reason }),
  action: 'user.delete',
  targetType: 'user',
  targetId: '42',
  payload: sourcePayload,
})
assert.equal(Object.isFrozen(pendingSnapshot), true, 'pending snapshot must be immutable')
assert.equal(Object.isFrozen(pendingSnapshot.payload), true, 'pending payload must be immutable')
assert.equal(Object.isFrozen(pendingSnapshot.payload.nested), true, 'nested pending payload must be immutable')
sourcePayload.reason = 'mutated after begin'
sourcePayload.nested.confirmText = 'mutated after begin'
assert.equal(pendingSnapshot.payload.reason, 'operator secret reason')
assert.equal(pendingSnapshot.payload.nested.confirmText, 'confirm-delete')
const persistedPendingJSON = pendingStorage.getItem(PENDING_MUTATION_STORAGE_KEY)
assert.ok(persistedPendingJSON, 'pending marker must be persisted before submission')
assert.doesNotMatch(persistedPendingJSON, /operator secret reason|confirm-delete/)
assert.match(persistedPendingJSON, /"action":"user\.delete"/)
pendingIdempotency.clearReusableIdempotencyKeys()
assert.equal(
  pendingStorage.getItem(IDEMPOTENCY_STORAGE_KEY),
  null,
  'an authentication change must discard reusable key ownership',
)
assert.equal(
  pendingIdempotency.getPendingMutation('control-plane.user:42')?.key,
  pendingSnapshot.key,
  'an authentication change must preserve the durable uncertain-operation lock',
)

const reloadedPendingIdempotency = loadTypeScriptModule('frontend/src/lib/idempotency.ts', {
  crypto: webcrypto,
  window: { sessionStorage: pendingStorage },
})
const restoredPending = reloadedPendingIdempotency.getPendingMutation('control-plane.user:42')
assert.equal(restoredPending?.key, pendingSnapshot.key, 'page reload must recover the durable pending owner')
assert.equal('payload' in restoredPending, false, 'browser storage must not recover or persist request payloads')
assert.throws(() => reloadedPendingIdempotency.beginPendingMutation({
  operationIdentifier: 'control-plane.user:42',
  fingerprint: 'different-action',
  action: 'user.disable',
  targetType: 'user',
  targetId: '42',
  payload: { userId: 42, reason: 'different request' },
}), /Pending mutation already exists/, 'all components must share one target-specific owner')

for (const [status, decision] of [
  ['succeeded', 'applied'],
  ['failed', 'released'],
  ['denied', 'released'],
  ['pending', 'locked'],
  ['cancelled', 'locked'],
  ['not_found', 'locked'],
]) {
  assert.equal(
    pendingIdempotency.operationReconciliationDecision(status),
    decision,
    `${status} reconciliation decision`,
  )
}
assert.equal(pendingIdempotency.operationReconciliationAction('succeeded'), 'clear')
assert.equal(pendingIdempotency.operationReconciliationAction('failed'), 'confirm_release')
assert.equal(pendingIdempotency.operationReconciliationAction('denied'), 'confirm_release')
assert.equal(pendingIdempotency.operationReconciliationAction('failed', true), 'clear')
assert.equal(pendingIdempotency.operationReconciliationAction('denied', true), 'clear')
for (const status of ['pending', 'cancelled', 'not_found']) {
  assert.equal(pendingIdempotency.operationReconciliationAction(status), 'keep_locked')
}
const missingAuditReconciliation = loadTypeScriptModule('frontend/src/lib/idempotency.ts', {
  crypto: webcrypto,
  window: { sessionStorage: new MemoryStorage() },
  fetch: async () => ({ status: 404 }),
})
const missingAudit = await missingAuditReconciliation.fetchOperationReconciliation(
  '',
  {},
  pendingSnapshot,
)
assert.equal(missingAudit.status, 'not_found')
assert.equal(
  missingAuditReconciliation.operationReconciliationDecision(missingAudit.status),
  'locked',
  'an audit miss is not durable proof that the original request never arrived',
)
const mismatchedAuditReconciliation = loadTypeScriptModule('frontend/src/lib/idempotency.ts', {
  crypto: webcrypto,
  window: { sessionStorage: new MemoryStorage() },
  fetch: async () => ({
    status: 200,
    ok: true,
    json: async () => ({
      success: true,
      data: { status: 'succeeded', action: 'user.disable', target_type: 'user', target_id: '99', audit_id: 12 },
    }),
  }),
})
await assert.rejects(
  mismatchedAuditReconciliation.fetchOperationReconciliation('', {}, pendingSnapshot),
  /target mismatch/,
  'a valid audit for another action or target must never unlock this owner',
)
assert.equal(pendingIdempotency.clearPendingMutation(pendingSnapshot), true)
assert.equal(pendingIdempotency.getPendingMutation('control-plane.user:42'), null)
const staleReleaseCandidate = pendingIdempotency.bindOperationReleaseCandidate(pendingSnapshot, {
  status: 'failed',
  action: 'user.delete',
  target_type: 'user',
  target_id: '42',
  audit_id: 41,
})
assert.equal(
  pendingIdempotency.operationReleaseCandidateMatches(staleReleaseCandidate, pendingSnapshot),
  true,
  'a release confirmation must be bound to the exact pending owner',
)
const replacementSnapshot = pendingIdempotency.beginPendingMutation({
  operationIdentifier: 'control-plane.user:42',
  fingerprint: 'replacement-after-terminal-outcome',
  action: 'user.delete',
  targetType: 'user',
  targetId: '42',
  payload: { userId: 42, reason: 'new reviewed operation' },
})
assert.notEqual(replacementSnapshot.key, pendingSnapshot.key, 'released ownership must not reuse a stale key')
assert.equal(replacementSnapshot.action, staleReleaseCandidate.action)
assert.equal(replacementSnapshot.targetId, staleReleaseCandidate.target_id)
assert.equal(
  pendingIdempotency.operationReleaseCandidateMatches(staleReleaseCandidate, replacementSnapshot),
  false,
  'a failed/denied confirmation for K1 must not release a same-action same-target K2 owner',
)
assert.equal(
  pendingIdempotency.operationReconciliationAction(
    staleReleaseCandidate.status,
    pendingIdempotency.operationReleaseCandidateMatches(staleReleaseCandidate, replacementSnapshot),
  ),
  'confirm_release',
  'a stale K1 candidate must never advance K2 directly to clear',
)

const pendingUserA = { ...replacementSnapshot, operationIdentifier: 'control-plane.user:42', targetId: '42' }
const pendingUserB = {
  ...replacementSnapshot,
  operationIdentifier: 'control-plane.user:43',
  key: '00000000-0000-4000-8000-000000000043',
  targetId: '43',
}
const onlyUserA = pendingIdempotency.indexPendingMutationsByOperation([pendingUserA], 'user')
assert.equal(
  onlyUserA.get(pendingIdempotency.userMutationOperationIdentifier(43)),
  undefined,
  'a durable pending mutation for user A must not lock user B',
)
const pendingUsersByOperation = pendingIdempotency.indexPendingMutationsByOperation(
  [pendingUserA, pendingUserB],
  'user',
)
assert.equal(
  pendingUsersByOperation.get(pendingIdempotency.userMutationOperationIdentifier(42))?.key,
  pendingUserA.key,
)
assert.equal(
  pendingUsersByOperation.get(pendingIdempotency.userMutationOperationIdentifier(43))?.key,
  pendingUserB.key,
)
assert.equal(pendingIdempotency.parseUserMutationTargetId('42'), 42)
assert.equal(
  pendingIdempotency.parseUserMutationOperationTargetId('control-plane.user:42', '42'),
  42,
)
assert.throws(
  () => pendingIdempotency.parseUserMutationOperationTargetId('control-plane.user:43', '42'),
  /does not match its target/,
)
for (const invalidTargetId of ['', '0', '-1', '1.5', '1e2', '+42', ' 42 ', '0042', '9007199254740992']) {
  assert.throws(
    () => pendingIdempotency.parseUserMutationTargetId(invalidTargetId),
    /positive safe integer/,
    `user mutation target ${JSON.stringify(invalidTargetId)} must be rejected`,
  )
}

const failedBeginStorage = new FaultInjectingStorage()
failedBeginStorage.ignoredSetKeys.add(PENDING_MUTATION_STORAGE_KEY)
const failedBeginIdempotency = loadTypeScriptModule('frontend/src/lib/idempotency.ts', {
  crypto: webcrypto,
  window: { sessionStorage: failedBeginStorage },
})
assert.throws(() => failedBeginIdempotency.beginPendingMutation({
  operationIdentifier: 'control-plane.user:7',
  fingerprint: 'storage-failure-before-submit',
  action: 'user.disable',
  targetType: 'user',
  targetId: '7',
  payload: { userId: 7, reason: 'must never be submitted' },
}), /持久化操作锁/, 'submission must fail closed when the pending marker cannot be recovered')
assert.equal(failedBeginStorage.getItem(PENDING_MUTATION_STORAGE_KEY), null)
assert.equal(failedBeginStorage.getItem(IDEMPOTENCY_STORAGE_KEY), null, 'failed begin must roll back new key ownership')

const failedOwnershipClearStorage = new FaultInjectingStorage()
const failedOwnershipClear = loadTypeScriptModule('frontend/src/lib/idempotency.ts', {
  crypto: webcrypto,
  window: { sessionStorage: failedOwnershipClearStorage },
})
const ownershipLocked = failedOwnershipClear.beginPendingMutation({
  operationIdentifier: 'control-plane.user:8',
  fingerprint: 'clear-order-ownership-first',
  action: 'user.enable',
  targetType: 'user',
  targetId: '8',
  payload: { userId: 8, reason: 'reviewed appeal' },
})
failedOwnershipClearStorage.operations = []
failedOwnershipClearStorage.ignoredRemoveKeys.add(IDEMPOTENCY_STORAGE_KEY)
assert.equal(failedOwnershipClear.clearPendingMutation(ownershipLocked), false)
assert.equal(failedOwnershipClear.getPendingMutation('control-plane.user:8')?.key, ownershipLocked.key)
assert.deepEqual(
  failedOwnershipClearStorage.operations,
  [`remove:${IDEMPOTENCY_STORAGE_KEY}`],
  'pending marker must not be removed when key ownership removal is unverified',
)

const failedPendingClearStorage = new FaultInjectingStorage()
const failedPendingClear = loadTypeScriptModule('frontend/src/lib/idempotency.ts', {
  crypto: webcrypto,
  window: { sessionStorage: failedPendingClearStorage },
})
const conservativelyLocked = failedPendingClear.beginPendingMutation({
  operationIdentifier: 'control-plane.user:9',
  fingerprint: 'clear-order-pending-second',
  action: 'user.delete',
  targetType: 'user',
  targetId: '9',
  payload: { userId: 9, reason: 'approved deletion' },
})
failedPendingClearStorage.operations = []
failedPendingClearStorage.ignoredRemoveKeys.add(PENDING_MUTATION_STORAGE_KEY)
assert.equal(failedPendingClear.clearPendingMutation(conservativelyLocked), false)
assert.equal(failedPendingClearStorage.getItem(IDEMPOTENCY_STORAGE_KEY), null)
assert.equal(failedPendingClear.getPendingMutation('control-plane.user:9')?.key, conservativelyLocked.key)
assert.deepEqual(
  failedPendingClearStorage.operations,
  [`remove:${IDEMPOTENCY_STORAGE_KEY}`, `remove:${PENDING_MUTATION_STORAGE_KEY}`],
  'clear must remove reusable ownership before the durable lock marker',
)

const redemptionRecovery = loadTypeScriptModule('frontend/src/lib/redemptionRecovery.ts')
const recovered = redemptionRecovery.extractAppliedRedemptionResult({
  error: {
    code: 'AUDIT_OUTCOME_PERSIST_FAILED',
    details: {
      operation_applied: true,
      do_not_retry: true,
      result: { keys: ['code-a', 'code-b'], count: 2 },
    },
  },
})
assert.deepEqual(Array.from(recovered.keys), ['code-a', 'code-b'])
assert.equal(recovered.count, 2)
assert.equal(
  redemptionRecovery.extractAppliedRedemptionResult({
    error: {
      code: 'AUDIT_OUTCOME_PERSIST_FAILED',
      details: { operation_applied: true, do_not_retry: true, result: { keys: ['duplicate', 'duplicate'], count: 2 } },
    },
  }),
  null,
  'recovery plaintext must be bounded, unique, and count-consistent',
)

const { mergeFilteredModelOrder } = loadTypeScriptModule('frontend/src/lib/modelStatusOrder.ts')
assert.deepEqual(
  Array.from(mergeFilteredModelOrder(
    ['alpha', 'bravo', 'charlie', 'delta'],
    ['bravo', 'delta'],
    'delta',
    'bravo',
    ['alpha', 'bravo', 'charlie', 'delta', 'archived'],
  )),
  ['alpha', 'delta', 'charlie', 'bravo', 'archived'],
  'dragging a filtered subset must preserve hidden and temporarily absent models',
)

const {
  chunkModelNames,
  mapWithConcurrency,
  MODEL_STATUS_BATCH_MAX_CONCURRENCY,
  normalizeModelStatusMaxBatch,
} = loadTypeScriptModule('frontend/src/lib/modelStatusBatch.ts')
assert.equal(normalizeModelStatusMaxBatch(undefined, 50), 50)
assert.equal(normalizeModelStatusMaxBatch(200, 50), 200)
assert.throws(() => chunkModelNames(['a'], 0), /positive integer/)
assert.deepEqual(
  Array.from(chunkModelNames(['a', 'b', 'c', 'd', 'e'], 2), chunk => Array.from(chunk)),
  [['a', 'b'], ['c', 'd'], ['e']],
  'model-status requests must be partitioned by the server-advertised limit',
)

let inFlightModelStatusBatches = 0
let maxInFlightModelStatusBatches = 0
const processedModelStatusBatches = []
const boundedBatchResults = await mapWithConcurrency(
  [0, 1, 2, 3, 4, 5, 6],
  MODEL_STATUS_BATCH_MAX_CONCURRENCY,
  async batch => {
    inFlightModelStatusBatches += 1
    maxInFlightModelStatusBatches = Math.max(maxInFlightModelStatusBatches, inFlightModelStatusBatches)
    await new Promise(resolve => setTimeout(resolve, 2))
    processedModelStatusBatches.push(batch)
    inFlightModelStatusBatches -= 1
    return `batch-${batch}`
  },
)
assert.ok(
  maxInFlightModelStatusBatches <= MODEL_STATUS_BATCH_MAX_CONCURRENCY,
  `model-status in-flight batches = ${maxInFlightModelStatusBatches}, want <= ${MODEL_STATUS_BATCH_MAX_CONCURRENCY}`,
)
assert.equal(maxInFlightModelStatusBatches, MODEL_STATUS_BATCH_MAX_CONCURRENCY)
assert.deepEqual(Array.from(processedModelStatusBatches).sort((a, b) => a - b), [0, 1, 2, 3, 4, 5, 6])
assert.deepEqual(Array.from(boundedBatchResults), [
  'batch-0', 'batch-1', 'batch-2', 'batch-3', 'batch-4', 'batch-5', 'batch-6',
])

const {
  cleanupAnalyticsBatchRun,
  refreshAnalyticsBatchState,
  replaceAnalyticsBatchRun,
} = loadTypeScriptModule('frontend/src/lib/analyticsBatch.ts')
const abortable = () => ({ aborted: false, abort() { this.aborted = true } })
const oldBatch = { controller: abortable(), requestController: abortable(), timeout: 11 }
const replacementBatch = { controller: abortable(), requestController: abortable(), timeout: 22 }
const batchRunRef = { current: oldBatch }
const clearedBatchTimeouts = []
replaceAnalyticsBatchRun(batchRunRef, replacementBatch, timeout => clearedBatchTimeouts.push(timeout))
assert.equal(oldBatch.controller.aborted, true)
assert.equal(oldBatch.requestController.aborted, true)
assert.deepEqual(clearedBatchTimeouts, [11])
assert.equal(
  cleanupAnalyticsBatchRun(batchRunRef, oldBatch, timeout => clearedBatchTimeouts.push(timeout)),
  false,
  'an old batch cleanup must not release ownership of its replacement',
)
assert.equal(batchRunRef.current, replacementBatch)
assert.equal(replacementBatch.requestController.aborted, false)
assert.deepEqual(clearedBatchTimeouts, [11], 'old cleanup must not clear the replacement timeout')
assert.equal(cleanupAnalyticsBatchRun(batchRunRef, replacementBatch, timeout => clearedBatchTimeouts.push(timeout)), true)
assert.equal(batchRunRef.current, null)
assert.equal(replacementBatch.requestController, null)
assert.deepEqual(clearedBatchTimeouts, [11, 22])

const refreshingBatch = { controller: abortable(), requestController: null, timeout: 33 }
const refreshingBatchRef = { current: refreshingBatch }
const refreshOrder = []
let resolveSyncRefresh
let resolveAnalyticsRefresh
const finalRefreshLifecycle = (async () => {
  try {
    await refreshAnalyticsBatchState(
      () => new Promise(resolve => {
        refreshOrder.push('sync-started')
        resolveSyncRefresh = () => {
          refreshOrder.push('sync-finished')
          resolve()
        }
      }),
      () => new Promise(resolve => {
        refreshOrder.push('analytics-started')
        resolveAnalyticsRefresh = () => {
          refreshOrder.push('analytics-finished')
          resolve()
        }
      }),
    )
  } finally {
    cleanupAnalyticsBatchRun(refreshingBatchRef, refreshingBatch, timeout => refreshOrder.push(`cleared-${timeout}`))
  }
})()
await Promise.resolve()
assert.deepEqual(refreshOrder, ['sync-started', 'analytics-started'])
assert.equal(refreshingBatchRef.current, refreshingBatch, 'batch ownership must remain while final refreshes are pending')
resolveSyncRefresh()
await Promise.resolve()
assert.equal(refreshingBatchRef.current, refreshingBatch, 'one completed refresh must not release batch ownership')
resolveAnalyticsRefresh()
await finalRefreshLifecycle
assert.equal(refreshingBatchRef.current, null)
assert.deepEqual(refreshOrder, [
  'sync-started',
  'analytics-started',
  'sync-finished',
  'analytics-finished',
  'cleared-33',
])

const partiallyFailedBatch = { controller: abortable(), requestController: null, timeout: 44 }
const partiallyFailedBatchRef = { current: partiallyFailedBatch }
const partialFailureOrder = []
const refreshFailure = new Error('sync refresh failed')
let resolvePendingAnalyticsRefresh
let observedRefreshFailure
const partialFailureLifecycle = (async () => {
  try {
    await refreshAnalyticsBatchState(
      async () => {
        partialFailureOrder.push('sync-rejected')
        throw refreshFailure
      },
      () => new Promise(resolve => {
        partialFailureOrder.push('analytics-started')
        resolvePendingAnalyticsRefresh = () => {
          partialFailureOrder.push('analytics-finished')
          resolve()
        }
      }),
    )
  } catch (error) {
    observedRefreshFailure = error
    partialFailureOrder.push('failure-observed')
  } finally {
    cleanupAnalyticsBatchRun(
      partiallyFailedBatchRef,
      partiallyFailedBatch,
      timeout => partialFailureOrder.push(`cleared-${timeout}`),
    )
  }
})()
await Promise.resolve()
await Promise.resolve()
assert.equal(
  partiallyFailedBatchRef.current,
  partiallyFailedBatch,
  'a rejected refresh must not release ownership while the other refresh is pending',
)
assert.deepEqual(partialFailureOrder, ['sync-rejected', 'analytics-started'])
resolvePendingAnalyticsRefresh()
await partialFailureLifecycle
assert.equal(observedRefreshFailure, refreshFailure)
assert.equal(partiallyFailedBatchRef.current, null)
assert.deepEqual(partialFailureOrder, [
  'sync-rejected',
  'analytics-started',
  'analytics-finished',
  'failure-observed',
  'cleared-44',
])

const monitor = read('frontend/src/components/ModelStatusMonitor.tsx')
assert.match(monitor, /savedModels === null/)
assert.doesNotMatch(monitor, /Array\.isArray\(data\.data\) && data\.data\.length > 0/)
assert.match(monitor, /const groupManagerModels = useMemo/)
assert.match(monitor, /allModels=\{groupManagerModels\}/)
assert.match(monitor, /requireSuccessfulResponse\(response\)/)
assert.match(monitor, /statusRequestControllerRef\.current\?\.abort\(\)/)
assert.match(monitor, /\/api\/model-status\/config\/custom-order/)
assert.match(monitor, /custom_order: previousPersistedOrder/)
assert.match(monitor, /if \(!orderSaved\) \{\s*await reloadAuthoritativeSortConfig\(mutationId\)\s*return false/)
assert.match(monitor, /if \(!modeSaved\) \{[\s\S]*?await reloadAuthoritativeSortConfig\(mutationId\)\s*return false/)
assert.match(monitor, /setSortMode\(mode\)\s*setCustomOrder\(authoritativeOrder\)/)
assert.match(monitor, /setSortReconciliationRequired\(true\)/)
assert.match(monitor, /if \(sortReconciliationRequired\) \{/)
assert.match(monitor, /AUTHENTICATED_MODEL_STATUS_DEFAULT_MAX_BATCH = 200/)
assert.match(monitor, /mapWithConcurrency\(\s*chunkModelNames\(fetchSet, maxBatch\),\s*MODEL_STATUS_BATCH_MAX_CONCURRENCY/)
assert.doesNotMatch(monitor, /Promise\.all\(chunkModelNames\(fetchSet, maxBatch\)/)
assert.match(monitor, /setModelStatuses\(chunkResults\.flat\(\)\)/)
assert.match(monitor, /const activeTokenGroupModels = useMemo/)
assert.match(monitor, /const tokenGroupModelsChanged = !sameStringArray\(activeTokenGroupModels, prevActiveTokenGroupModels\.current\)/)
assert.match(monitor, /modelsChanged \|\| tokenGroupSwitched \|\| tokenGroupModelsChanged/)
assert.match(monitor, /const tokenGroupRequestIdRef = useRef\(0\)/)
assert.match(monitor, /tokenGroupRequestControllerRef\.current\?\.abort\(\)/)
assert.match(monitor, /requestId !== tokenGroupRequestIdRef\.current/)
assert.match(monitor, /data\.data\.every\(isTokenGroup\)/)
assert.match(monitor, /Array\.isArray\(group\.models\) && group\.models\.every\(model => typeof model === 'string'\)/)
assert.match(monitor, /TOKEN_GROUP_SYNC_EVERY_STATUS_REFRESHES = 5/)
assert.match(monitor, /refreshStatusesWithLatestTokenGroups\(true\)/)
assert.match(monitor, /nextGroupFilter = 'all'/)

const embed = read('frontend/src/components/ModelStatusEmbed.tsx')
assert.match(embed, /Array\.isArray\(data\.data\) && data\.data\.every/)
assert.doesNotMatch(embed, /Array\.isArray\(data\.data\) && data\.data\.length > 0/)
assert.match(embed, /statusRequestControllerRef\.current\?\.abort\(\)/)
assert.match(embed, /PUBLIC_MODEL_STATUS_DEFAULT_MAX_BATCH = 50/)
assert.match(embed, /mapWithConcurrency\(\s*chunkModelNames\(fetchSet, maxBatch\),\s*MODEL_STATUS_BATCH_MAX_CONCURRENCY/)
assert.doesNotMatch(embed, /Promise\.all\(chunkModelNames\(fetchSet, maxBatch\)/)
assert.match(embed, /setModelStatuses\(chunkResults\.flat\(\)\)/)
assert.match(embed, /const tokenGroupRequestIdRef = useRef\(0\)/)
assert.match(embed, /tokenGroupRequestControllerRef\.current\?\.abort\(\)/)
assert.match(embed, /requestId !== tokenGroupRequestIdRef\.current/)
assert.match(embed, /data\.data\.every\(isEmbedTokenGroup\)/)
assert.match(embed, /Array\.isArray\(group\.models\) && group\.models\.every\(model => typeof model === 'string'\)/)
assert.match(embed, /TOKEN_GROUP_SYNC_EVERY_STATUS_REFRESHES = 5/)
assert.match(embed, /refreshStatusesWithLatestTokenGroups/)
assert.match(embed, /nextGroupFilter = 'all'/)

const analytics = read('frontend/src/components/Analytics.tsx')
assert.match(analytics, /replaceAnalyticsBatchRun\(batchRunRef, batchRun/)
assert.match(analytics, /cleanupAnalyticsBatchRun\(batchRunRef, batchRun/)
assert.match(analytics, /if \(batchRunRef\.current !== batchRun\) return/)
assert.match(analytics, /await refreshAnalyticsBatchState\(fetchSyncStatus, fetchAnalytics\)\s*\} finally/)
assert.doesNotMatch(analytics, /void Promise\.all\(\[fetchSyncStatus\(\), fetchAnalytics\(\)\]\)/)

const authContext = read('frontend/src/contexts/AuthContext.tsx')
assert.match(authContext, /clearReusableIdempotencyKeys\(\)/)
assert.doesNotMatch(authContext, /clearAllIdempotencyKeys\(\)/)

const generator = read('frontend/src/components/Generator.tsx')
assert.match(generator, /extractAppliedRedemptionResult\(data\)/)
assert.match(generator, /deliveryStatus: 'applied_audit_uncertain'/)
const resultModal = read('frontend/src/components/ResultModal.tsx')
assert.match(resultModal, /已创建，审计待对账/)
assert.match(resultModal, /切勿重试/)
assert.match(resultModal, /关闭后立即清空明文/)

const redemptions = read('frontend/src/components/Redemptions.tsx')
assert.doesNotMatch(redemptions, /const closeDeleteDialog = \(\) => \{\s*clearIdempotencyKey/)
const realtimeRanking = read('frontend/src/components/RealtimeRanking.tsx')
assert.doesNotMatch(realtimeRanking, /const closeBanConfirmDialog = \(\) => \{\s*clearIdempotencyKey/)
assert.match(realtimeRanking, /beginPendingMutation\(\{/)
assert.match(realtimeRanking, /userMutationOperationIdentifier\(dialog\.userId\)/)
assert.match(realtimeRanking, /fetchOperationReconciliation\(apiUrl, getAuthHeaders\(\), pending\)/)
assert.match(realtimeRanking, /disabled=\{banPayloadLocked\}/)
assert.match(realtimeRanking, /const reconcileBanMutation = async/)
assert.match(realtimeRanking, /nextAction === 'confirm_release'/)
assert.match(realtimeRanking, /确认解除本地锁/)
assert.match(realtimeRanking, /operationReleaseCandidateMatches\(banReleaseCandidate, pending\)/)
assert.match(realtimeRanking, /bindOperationReleaseCandidate\(pending, reconciliation\)/)
assert.match(realtimeRanking, /setBanReleaseCandidate\(current => operationReleaseCandidateMatches\(current, existing\)/)
assert.doesNotMatch(realtimeRanking, /getOrCreateIdempotencyKey|mutationResponseRequiresReconciliation|使用原内容重试/)
const userAnalysis = read('frontend/src/components/UserAnalysisDialog.tsx')
assert.doesNotMatch(userAnalysis, /const closeBanConfirmDialog = \(\) => \{\s*clearIdempotencyKey/)
assert.match(userAnalysis, /beginPendingMutation\(\{/)
assert.match(userAnalysis, /getPendingMutation\(userMutationOperationIdentifier\(userId\)\)/)
assert.match(userAnalysis, /fetchOperationReconciliation\(apiUrl, getAuthHeaders\(\), pending\)/)
assert.match(userAnalysis, /disabled=\{banPayloadLocked\}/)
assert.match(userAnalysis, /const reconcileBanMutation = async/)
assert.match(userAnalysis, /nextAction === 'confirm_release'/)
assert.match(userAnalysis, /确认解除本地锁/)
assert.match(userAnalysis, /operationReleaseCandidateMatches\(banReleaseCandidate, pending\)/)
assert.match(userAnalysis, /bindOperationReleaseCandidate\(pending, reconciliation\)/)
assert.match(userAnalysis, /setBanReleaseCandidate\(current => operationReleaseCandidateMatches\(current, existing\)/)
assert.match(userAnalysis, /operationReleaseCandidateMatches\(current, nextPending\)/)
assert.doesNotMatch(userAnalysis, /getOrCreateIdempotencyKey|mutationResponseRequiresReconciliation|使用原内容重试/)
const userManagement = read('frontend/src/components/UserManagement.tsx')
const openPendingMutationDialogSource = userManagement.slice(
  userManagement.indexOf('const openPendingMutationDialog'),
  userManagement.indexOf('const deleteUser'),
)
const pendingTargetValidationIndex = openPendingMutationDialogSource.indexOf(
  'parseUserMutationOperationTargetId(pending.operationIdentifier, pending.targetId)',
)
assert.ok(pendingTargetValidationIndex >= 0, 'pending user target validation must be present')
const closeDeleteDialogSource = userManagement.slice(
  userManagement.indexOf('const closeDeleteDialog'),
  userManagement.indexOf('const reconcileDeleteUser'),
)
assert.doesNotMatch(closeDeleteDialogSource, /clearIdempotencyKey/)
assert.doesNotMatch(userManagement, /onChange=\{\(\) => \{[^}]*clearIdempotencyKey/)
assert.match(userManagement, /beginPendingMutation\(\{/)
assert.match(userManagement, /fetchOperationReconciliation\(apiUrl, getAuthHeaders\(\), pending\)/)
assert.match(userManagement, /用户操作待审计对账/)
assert.match(userManagement, /disabled=\{deletePayloadLocked\}/)
assert.match(userManagement, /const reconcileDeleteUser = async/)
assert.match(userManagement, /nextAction === 'confirm_release'/)
assert.match(userManagement, /确认解除本地锁/)
assert.match(userManagement, /operationReleaseCandidateMatches\(deleteReleaseCandidate, pending\)/)
assert.match(userManagement, /bindOperationReleaseCandidate\(pending, reconciliation\)/)
assert.match(userManagement, /setDeleteReleaseCandidate\(current => operationReleaseCandidateMatches\(current, pending\)/)
assert.match(userManagement, /indexPendingMutationsByOperation<DeleteUserPendingMutation>\(listPendingMutations\(\), 'user'\)/)
assert.match(userManagement, /pendingDeleteMutations\.get\(userMutationOperationIdentifier\(deleteUserTarget\.userId\)\) \?\? null/)
assert.ok(
  pendingTargetValidationIndex < openPendingMutationDialogSource.indexOf('rememberPendingDeleteMutation(pending)'),
  'pending user target validation must run before the pending mutation enters component state',
)
assert.ok(
  pendingTargetValidationIndex < openPendingMutationDialogSource.indexOf('setDeleteUserTarget'),
  'pending user target validation must run before the dialog target is set',
)
assert.match(openPendingMutationDialogSource, /catch \(error\) \{[\s\S]*?showToast\(\s*'error',[\s\S]*?return\s*\}/)
assert.doesNotMatch(openPendingMutationDialogSource, /clearPendingMutation/)
assert.doesNotMatch(userManagement, /listPendingMutations\(\)\.find\(item => item\.targetType === 'user'\) \?\? null/)
assert.doesNotMatch(userManagement, /使用原内容重试|Tool Store 未发现该操作意图，已安全释放/)

const topUps = read('frontend/src/components/TopUps.tsx')
assert.match(topUps, /const fetchStatistics = useCallback\(async \(\): Promise<boolean>/)
assert.match(topUps, /const fetchRecords = useCallback\(async \(showErrorToast = true\): Promise<boolean>/)
assert.match(topUps, /const successCount = results\.filter\(Boolean\)\.length/)
assert.match(topUps, /2\/2 成功/)
assert.match(topUps, /1\/2 成功/)
assert.match(topUps, /0\/2 成功/)

console.log('frontend control-plane reliability guards passed')
