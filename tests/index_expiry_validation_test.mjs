import assert from 'node:assert/strict'
import { webcrypto } from 'node:crypto'
import { readFileSync } from 'node:fs'
import vm from 'node:vm'

const html = readFileSync(new URL('../index.html', import.meta.url), 'utf8')
const inlineScript = [...html.matchAll(/<script(?:\s[^>]*)?>([\s\S]*?)<\/script>/gi)]
  .map((match) => match[1])
  .find((script) => script.includes('function parseExpirationDays'))
assert.ok(inlineScript, 'missing redemption generator inline script')
new vm.Script(inlineScript, { filename: 'index.html:inline-script' })

assert.doesNotMatch(html, /fonts\.googleapis\.com|fonts\.gstatic\.com/i)
assert.doesNotMatch(html, /cloudflareinsights\.com|data-cf-beacon/i)
assert.doesNotMatch(html, /\b(?:src|href)\s*=\s*["']https?:\/\//i)

function extractFunction(name) {
  const start = html.indexOf(`function ${name}(`)
  assert.notEqual(start, -1, `missing ${name} in index.html`)

  const bodyStart = html.indexOf('{', start)
  let depth = 0
  for (let index = bodyStart; index < html.length; index += 1) {
    if (html[index] === '{') depth += 1
    if (html[index] !== '}') continue

    depth -= 1
    if (depth === 0) return html.slice(start, index + 1)
  }

  assert.fail(`unterminated ${name} in index.html`)
}

function extractConstant(name) {
  const match = html.match(new RegExp(`^\\s*const ${name} = [^;]+;`, 'm'))
  assert.ok(match, `missing ${name} in index.html`)
  return match[0]
}

const validatorNames = [
  'isSafeUnixTimestamp',
  'parseExpirationDays',
  'parseExpirationDate',
]
const context = vm.createContext({ Date, Number, String, Math })
vm.runInContext(
  `${validatorNames.map(extractFunction).join('\n')}\nthis.validators = { ${validatorNames.join(', ')} };`,
  context,
)

const { isSafeUnixTimestamp, parseExpirationDays, parseExpirationDate } = context.validators
const nowMilliseconds = 1_700_000_000_123
const nowSeconds = Math.floor(nowMilliseconds / 1000)

assert.equal(parseExpirationDays('1', nowMilliseconds), nowSeconds + (24 * 60 * 60))
for (const invalidDays of ['', '0', '-1', '1.5', '1day', 'Infinity', '100000000', String(Number.MAX_SAFE_INTEGER)]) {
  assert.equal(parseExpirationDays(invalidDays, nowMilliseconds), null, `accepted invalid days: ${invalidDays}`)
}
assert.equal(parseExpirationDays('1', Number.POSITIVE_INFINITY), null)

const validDate = '2026-01-02T03:04'
assert.equal(parseExpirationDate(validDate), Math.floor(new Date(validDate).getTime() / 1000))
const validLeapDate = '2024-02-29T23:59'
assert.equal(parseExpirationDate(validLeapDate), Math.floor(new Date(validLeapDate).getTime() / 1000))
for (const invalidDate of [
  '',
  'not-a-date',
  '1960-01-01T00:00',
  '2026-02-29T12:00',
  '2024-02-30T12:00',
  '2026-00-10T12:00',
  '2026-13-10T12:00',
  '2026-01-00T12:00',
  '2026-01-02T24:00',
  '2026-01-02T03:60',
  '2026-1-02T03:04',
  '2026-01-2T03:04',
  '2026-01-02 03:04',
  '2026-01-02T03:04:00',
  '2026-01-02T03:04Z',
  ' 2026-01-02T03:04',
  '2026-01-02T03:04 ',
]) {
  assert.equal(parseExpirationDate(invalidDate), null, `accepted invalid date: ${invalidDate}`)
}

assert.equal(isSafeUnixTimestamp(1_700_000_000), true)
assert.equal(isSafeUnixTimestamp(8_640_000_000_000), true)
for (const invalidTimestamp of [0, -1, 1.5, Number.POSITIVE_INFINITY, 8_640_000_000_001, Number.MAX_SAFE_INTEGER]) {
  assert.equal(isSafeUnixTimestamp(invalidTimestamp), false, `accepted unsafe timestamp: ${invalidTimestamp}`)
}

assert.match(html, /parseExpirationDays\(document\.getElementById\('expireDays'\)\.value\)/)
assert.match(html, /parseExpirationDate\(dateStr\)/)

const keyConstantNames = [
  'GENERATED_KEY_LENGTH',
  'MIN_RANDOM_KEY_LENGTH',
  'MAX_KEY_PREFIX_LENGTH',
  'KEY_RANDOM_ALPHABET',
  'KEY_REJECTION_LIMIT',
]
const keyContext = vm.createContext({ crypto: webcrypto, Uint8Array, Math })
vm.runInContext(
  `${keyConstantNames.map(extractConstant).join('\n')}\n${extractFunction('generateRandomKey')}\n`
    + `this.keyTools = { generateRandomKey, ${keyConstantNames.join(', ')} };`,
  keyContext,
)

const {
  generateRandomKey,
  GENERATED_KEY_LENGTH,
  MIN_RANDOM_KEY_LENGTH,
  MAX_KEY_PREFIX_LENGTH,
  KEY_RANDOM_ALPHABET,
  KEY_REJECTION_LIMIT,
} = keyContext.keyTools

assert.equal(GENERATED_KEY_LENGTH, 32)
assert.equal(MIN_RANDOM_KEY_LENGTH, 25)
assert.equal(MAX_KEY_PREFIX_LENGTH, 7)
assert.match(html, /id="keyPrefix"[^>]*maxlength="7"/)
assert.equal(KEY_RANDOM_ALPHABET, '0123456789abcdefghijklmnopqrstuvwxyz')
assert.equal(KEY_REJECTION_LIMIT, Math.floor(256 / KEY_RANDOM_ALPHABET.length) * KEY_RANDOM_ALPHABET.length)
assert.ok(MIN_RANDOM_KEY_LENGTH * Math.log2(KEY_RANDOM_ALPHABET.length) >= 128)

const rejectedBytes = [252, 253, 254, 255]
const acceptedBytes = Array.from({ length: MIN_RANDOM_KEY_LENGTH }, (_, index) => index)
let deterministicRandomCalls = 0
const deterministicCrypto = {
  getRandomValues(randomBytes) {
    deterministicRandomCalls += 1
    randomBytes.fill(255)
    randomBytes.set([...rejectedBytes, ...acceptedBytes])
    return randomBytes
  },
}
const deterministicKeyContext = vm.createContext({ crypto: deterministicCrypto, Uint8Array, Math })
vm.runInContext(
  `${keyConstantNames.map(extractConstant).join('\n')}\n${extractFunction('generateRandomKey')}\n`
    + 'this.generateRandomKey = generateRandomKey;',
  deterministicKeyContext,
)
const rejectionSampledKey = deterministicKeyContext.generateRandomKey('api-v2_')
assert.equal(KEY_REJECTION_LIMIT, rejectedBytes[0])
assert.equal(rejectionSampledKey, `api-v2_${KEY_RANDOM_ALPHABET.slice(0, MIN_RANDOM_KEY_LENGTH)}`)
assert.equal(deterministicRandomCalls, 1)

const maxPrefix = 'api-v2_'
const prefixedKey = generateRandomKey(maxPrefix)
assert.equal(prefixedKey.length, GENERATED_KEY_LENGTH)
assert.ok(prefixedKey.startsWith(maxPrefix))
assert.match(prefixedKey.slice(maxPrefix.length), /^[a-z0-9]{25}$/)
assert.throws(() => generateRandomKey('12345678'), /最多 7 个字符/)
assert.throws(() => generateRandomKey('API'), /只能包含小写字母/)
assert.throws(() => generateRandomKey('api!'), /只能包含小写字母/)

const generatedKeys = Array.from({ length: 1000 }, () => generateRandomKey('api-v2_'))
assert.equal(new Set(generatedKeys).size, generatedKeys.length)
assert.equal(new Set(generatedKeys.map((key) => key.toLowerCase())).size, generatedKeys.length)
for (const key of generatedKeys) {
  assert.equal(key.length, GENERATED_KEY_LENGTH)
  assert.match(key, /^api-v2_[a-z0-9]{25}$/)
}

console.log('index key and expiration validation checks passed')
