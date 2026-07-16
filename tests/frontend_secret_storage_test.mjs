import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'

const read = (path) => readFileSync(new URL(`../${path}`, import.meta.url), 'utf8')

const authContext = read('frontend/src/contexts/AuthContext.tsx')
assert.match(authContext, /sessionStorage\.setItem\(TOKEN_KEY/)
assert.doesNotMatch(authContext, /localStorage\.setItem\(TOKEN_KEY/)
assert.match(authContext, /clearLegacyLocalAuthStorage\(\)/)
assert.match(authContext, /try\s*\{[\s\S]*?data = await response\.json\(\)[\s\S]*?catch/)
assert.match(authContext, /Login response was not valid JSON/)
assert.match(authContext, /!isLoginResponse\(data\)/)
assert.match(authContext, /typeof data\.token !== 'string'/)
assert.match(authContext, /!Number\.isFinite\(parsedExpiry\)/)
assert.match(authContext, /logout\(\)\s+return false/)
assert.doesNotMatch(authContext, /Number\.isFinite\(parsedExpiry\) \? parsedExpiry : fallbackExpiry/)

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

const legacyGenerator = read('index.html')
assert.match(legacyGenerator, /localStorage\.removeItem\('redemption_history'\)/)
assert.doesNotMatch(legacyGenerator, /copyHistory(?:SQL|Keys)|downloadHistoryKeys/)
assert.doesNotMatch(legacyGenerator, /HistoryManager\.save\(\{[\s\S]*?\b(?:keys|sql)\s*:/)
assert.match(legacyGenerator, /role="radiogroup" aria-label="数据库类型"/)
assert.doesNotMatch(legacyGenerator, /\.segmented-control input\s*\{\s*display:\s*none/)
assert.match(legacyGenerator, /id="toast"[^>]*role="status"[^>]*aria-live="polite"/)

console.log('frontend secret storage guards passed')
