export const INVITE_TOP_UP_FINGERPRINT_VERSION = 'invite-topup-query-v2'
export const INVITE_TOP_UP_TIMEZONE = 'Asia/Shanghai'

export interface InviteTopUpFingerprintInput {
  search: string
  startDate: string
  endDate: string
  sortBy: string
  sortDir: string
  asOf: number
}

const SHA256_HEX_PATTERN = /^[0-9a-f]{64}$/

function appendLengthPrefixed(target: number[], value: string) {
  const encoded = new TextEncoder().encode(value)
  if (encoded.byteLength > 0xffff_ffff) throw new Error('邀请充值查询字段过长')
  target.push(
    (encoded.byteLength >>> 24) & 0xff,
    (encoded.byteLength >>> 16) & 0xff,
    (encoded.byteLength >>> 8) & 0xff,
    encoded.byteLength & 0xff,
    ...encoded,
  )
}

// Cross-language v2 canonical form. Every label and value is UTF-8 encoded
// behind an unsigned 32-bit big-endian byte length, so delimiters and Unicode
// content cannot create ambiguous fingerprints.
export function canonicalInviteTopUpQuery(input: InviteTopUpFingerprintInput): Uint8Array {
  if (!Number.isSafeInteger(input.asOf) || input.asOf < 1) throw new Error('邀请充值查询 as_of 无效')
  const fields: Array<[string, string]> = [
    ['version', INVITE_TOP_UP_FINGERPRINT_VERSION],
    ['search', input.search.trim()],
    ['start_date', input.startDate.trim()],
    ['end_date', input.endDate.trim()],
    ['sort_by', input.sortBy.trim().toLowerCase()],
    ['sort_dir', input.sortDir.trim().toLowerCase()],
    ['as_of', String(input.asOf)],
    ['timezone', INVITE_TOP_UP_TIMEZONE],
  ]
  const bytes: number[] = []
  for (const [label, value] of fields) {
    appendLengthPrefixed(bytes, label)
    appendLengthPrefixed(bytes, value)
  }
  return new Uint8Array(bytes)
}

export async function inviteTopUpQueryFingerprint(
  input: InviteTopUpFingerprintInput,
  cryptoProvider: Pick<Crypto, 'subtle'> | null | undefined = globalThis.crypto,
): Promise<string> {
  if (!cryptoProvider?.subtle || typeof cryptoProvider.subtle.digest !== 'function') {
    throw new Error('浏览器 SHA-256 能力不可用')
  }
  const canonical = canonicalInviteTopUpQuery(input)
  const digestInput = new ArrayBuffer(canonical.byteLength)
  new Uint8Array(digestInput).set(canonical)
  const digest = await cryptoProvider.subtle.digest('SHA-256', digestInput)
  return Array.from(new Uint8Array(digest), byte => byte.toString(16).padStart(2, '0')).join('')
}

export function equalSha256Hex(left: unknown, right: unknown): boolean {
  if (typeof left !== 'string' || typeof right !== 'string' ||
    !SHA256_HEX_PATTERN.test(left) || !SHA256_HEX_PATTERN.test(right)) return false
  let difference = 0
  for (let index = 0; index < 64; index += 1) {
    difference |= left.charCodeAt(index) ^ right.charCodeAt(index)
  }
  return difference === 0
}

export function isSha256Hex(value: unknown): value is string {
  return typeof value === 'string' && SHA256_HEX_PATTERN.test(value)
}
