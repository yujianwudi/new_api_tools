import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'

const read = (path) => readFileSync(new URL(`../${path}`, import.meta.url), 'utf8')

const authContext = read('frontend/src/contexts/AuthContext.tsx')
assert.match(authContext, /sessionStorage\.setItem\(TOKEN_KEY/)
assert.doesNotMatch(authContext, /localStorage\.setItem\(TOKEN_KEY/)
assert.match(authContext, /clearLegacyLocalAuthStorage\(\)/)

const generator = read('frontend/src/components/Generator.tsx')
assert.match(generator, /saveToHistory\(formData, data\.data\.count\)/)
assert.doesNotMatch(generator, /saveToHistory\(formData, data\.data\)/)

const history = read('frontend/src/components/History.tsx')
assert.doesNotMatch(history, /\b(?:item|record)\.keys\b/)
assert.doesNotMatch(history, /keys:\s*item\.keys/)

const indexedDB = read('frontend/src/services/indexedDB.ts')
assert.match(indexedDB, /const DB_VERSION = 2\b/)
assert.match(indexedDB, /database\.deleteObjectStore\(STORES\.HISTORY\)/)
assert.match(indexedDB, /\['generation_history', 'redemption_history'\]/)
assert.doesNotMatch(indexedDB, /\bkeys:\s*string\[\]/)
assert.doesNotMatch(indexedDB, /migrateFromLocalStorage/)

const resultModal = read('frontend/src/components/ResultModal.tsx')
assert.match(resultModal, /result\.keys/)
assert.match(resultModal, /关闭后不保留兑换码明文/)

console.log('frontend secret storage guards passed')
