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

const { chunkModelNames, normalizeModelStatusMaxBatch } = loadTypeScriptModule('frontend/src/lib/modelStatusBatch.ts')
assert.equal(normalizeModelStatusMaxBatch(undefined, 50), 50)
assert.equal(normalizeModelStatusMaxBatch(200, 50), 200)
assert.throws(() => chunkModelNames(['a'], 0), /positive integer/)
assert.deepEqual(
  Array.from(chunkModelNames(['a', 'b', 'c', 'd', 'e'], 2), chunk => Array.from(chunk)),
  [['a', 'b'], ['c', 'd'], ['e']],
  'model-status requests must be partitioned by the server-advertised limit',
)

const monitor = read('frontend/src/components/ModelStatusMonitor.tsx')
assert.match(monitor, /savedModels === null/)
assert.doesNotMatch(monitor, /Array\.isArray\(data\.data\) && data\.data\.length > 0/)
assert.match(monitor, /const groupManagerModels = useMemo/)
assert.match(monitor, /allModels=\{groupManagerModels\}/)
assert.match(monitor, /requireSuccessfulResponse\(response\)/)
assert.match(monitor, /statusRequestControllerRef\.current\?\.abort\(\)/)
assert.match(monitor, /\/api\/model-status\/config\/custom-order/)
assert.match(monitor, /custom_order: previousPersistedOrder/)
assert.match(monitor, /AUTHENTICATED_MODEL_STATUS_DEFAULT_MAX_BATCH = 200/)
assert.match(monitor, /Promise\.all\(chunkModelNames\(fetchSet, maxBatch\)/)
assert.match(monitor, /setModelStatuses\(chunkResults\.flat\(\)\)/)

const embed = read('frontend/src/components/ModelStatusEmbed.tsx')
assert.match(embed, /if \(Array\.isArray\(data\.data\)\) \{\s*setSelectedModels\(data\.data\)/)
assert.doesNotMatch(embed, /Array\.isArray\(data\.data\) && data\.data\.length > 0/)
assert.match(embed, /statusRequestControllerRef\.current\?\.abort\(\)/)
assert.match(embed, /PUBLIC_MODEL_STATUS_DEFAULT_MAX_BATCH = 50/)
assert.match(embed, /Promise\.all\(chunkModelNames\(fetchSet, maxBatch\)/)
assert.match(embed, /setModelStatuses\(chunkResults\.flat\(\)\)/)

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
const userAnalysis = read('frontend/src/components/UserAnalysisDialog.tsx')
assert.doesNotMatch(userAnalysis, /const closeBanConfirmDialog = \(\) => \{\s*clearIdempotencyKey/)
const userManagement = read('frontend/src/components/UserManagement.tsx')
assert.doesNotMatch(userManagement, /const closeDeleteDialog = \(\) => \{[\s\S]*?clearIdempotencyKey/)
assert.doesNotMatch(userManagement, /onChange=\{\(\) => \{[^}]*clearIdempotencyKey/)

console.log('frontend control-plane reliability guards passed')
