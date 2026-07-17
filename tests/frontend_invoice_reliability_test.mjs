import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import vm from 'node:vm'
import ts from '../frontend/node_modules/typescript/lib/typescript.js'

const read = (path) => readFileSync(new URL(`../${path}`, import.meta.url), 'utf8')

function loadTypeScriptModule(path) {
  const output = ts.transpileModule(read(path), {
    compilerOptions: {
      module: ts.ModuleKind.CommonJS,
      target: ts.ScriptTarget.ES2020,
    },
    fileName: path,
  }).outputText
  const module = { exports: {} }
  const context = vm.createContext({ module, exports: module.exports, URLSearchParams, Date, BigInt })
  new vm.Script(output, { filename: path }).runInContext(context)
  return module.exports
}

const invoicesLib = loadTypeScriptModule('frontend/src/lib/invoices.ts')
assert.equal(
  invoicesLib.formatMinorAmount('900719925474099312345', 'CNY', 2),
  'CNY 9,007,199,254,740,993,123.45',
  'invoice amounts must remain exact beyond the JavaScript safe-integer range',
)
const params = invoicesLib.invoiceFilterParams({
  currency: 'cny',
  status: 'issued',
  issuedFrom: '2026-07-01',
  issuedTo: '2026-07-31',
})
assert.equal(params.get('issued_from'), '2026-07-01T00:00:00+08:00')
assert.equal(params.get('issued_to'), '2026-08-01T00:00:00+08:00')
assert.equal(params.get('status'), 'issued')

const invoices = read('frontend/src/components/Invoices.tsx')
assert.match(invoices, /beginPendingMutation\(/)
assert.match(invoices, /fetchOperationReconciliation\(/)
assert.match(invoices, /const fingerprint = `sha256:\$\{await sha256Utf8\(bodyJson\)\}`/)
assert.match(invoices, /payload:\s*\{\}/)
assert.match(invoices, /body:\s*bodyJson/)
assert.doesNotMatch(invoices, /pending\.payload\.body/)
assert.match(invoices, /setInterval\(\(\) => setFreshnessNow\(Date\.now\(\)\),\s*60_000\)/)
assert.match(invoices, /sourceFreshness\(summary\?\.generated_at,\s*freshnessNow\)/)
assert.match(invoices, /targetId:\s*payload\.invoice_number/)
assert.match(invoices, /targetId:\s*'batch'/)
assert.match(invoices, /targetId:\s*String\(invoiceID\)/)
assert.match(invoices, /capabilities\?\.\[capability\]\s*===\s*true/)
assert.match(read('frontend/src/lib/invoices.ts'), /new TextDecoder\('utf-8',\s*\{\s*fatal:\s*true\s*\}\)/)
assert.match(invoices, /detailSequence/)
assert.match(invoices, /detailAbortController/)
assert.match(invoices, /previewHash/)
assert.match(invoices, /previewVersion\s*!==\s*csvVersion\.current/)
assert.doesNotMatch(invoices, /file\.text\(/)
assert.doesNotMatch(invoices, /voided_at\s*:/)
assert.doesNotMatch(invoices, /Intl\.NumberFormat/)

const app = read('frontend/src/App.tsx')
const layout = read('frontend/src/components/Layout.tsx')
assert.match(app, /invoices/)
assert.match(layout, /发票统计/)

console.log('frontend invoice reliability checks passed')
