import { webcrypto } from 'node:crypto'
import { describe, expect, it, vi } from 'vitest'

import {
  canonicalInviteTopUpQuery,
  equalSha256Hex,
  inviteTopUpQueryFingerprint,
  type InviteTopUpFingerprintInput,
} from './affiliateFingerprint'

const fixture: InviteTopUpFingerprintInput = {
  search: ' Alice|Bob\nx=1 ',
  startDate: ' 2026-01-02 ',
  endDate: '2026-01-03',
  sortBy: 'LAST_TOPUP_AT',
  sortDir: 'ASC',
  asOf: 1_767_225_600,
}

const fixtureDigest = '7bfec800b7080f1b3ebdcab2414fdb0969f69822ae4ca99791182cd8c8707208'

describe('invite top-up query fingerprint v2', () => {
  it('matches the cross-language length-prefixed SHA-256 fixture', async () => {
    expect(canonicalInviteTopUpQuery(fixture).byteLength).toBeGreaterThan(0)
    await expect(inviteTopUpQueryFingerprint(fixture, webcrypto as unknown as Crypto)).resolves.toBe(fixtureDigest)
    expect(equalSha256Hex(fixtureDigest, fixtureDigest)).toBe(true)
    expect(equalSha256Hex(fixtureDigest, 'f'.repeat(64))).toBe(false)
  })

  it('canonicalizes large search fields without spreading bytes into function arguments', () => {
    const largeSearch = '测'.repeat(100_000)
    const canonical = canonicalInviteTopUpQuery({ ...fixture, search: largeSearch })

    expect(canonical.byteLength).toBeGreaterThan(new TextEncoder().encode(largeSearch).byteLength)
  })

  it('fails closed when Web Crypto is unavailable or digesting fails', async () => {
    await expect(inviteTopUpQueryFingerprint(fixture, null)).rejects.toThrow('SHA-256')
    const digest = vi.fn().mockRejectedValue(new Error('crypto failed'))
    const failingCrypto = { subtle: { digest } } as unknown as Crypto
    await expect(inviteTopUpQueryFingerprint(fixture, failingCrypto)).rejects.toThrow('crypto failed')
  })
})
