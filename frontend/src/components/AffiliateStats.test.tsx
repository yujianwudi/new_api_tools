import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createHash } from 'node:crypto'
import { http } from 'msw'
import { describe, expect, it, vi } from 'vitest'

import { AffiliateStats } from './AffiliateStats'
import { server } from '../test/server'
import { canonicalInviteTopUpQuery } from '../lib/affiliateFingerprint'

const { showToast } = vi.hoisted(() => ({ showToast: vi.fn() }))

vi.mock('../contexts/AuthContext', () => ({
  useAuth: () => ({ token: 'test-token' }),
}))

vi.mock('./Toast', () => ({
  useToast: () => ({ showToast }),
}))

interface DeferredResponse {
  promise: Promise<Response>
  resolve: (response: Response) => void
}

interface ParentRow {
  inviter_id: number
  inviter_username: string
  inviter_display_name: string | null
  current_invitee_count: number
  window_paying_invitee_count: number
  rewarded_invite_count: number
  success_topup_count: number
  success_amount: null
  success_money: null
  last_topup_at: number
  detail_evidence_hash: string
}

function deferredResponse(): DeferredResponse {
  let resolve!: (response: Response) => void
  const promise = new Promise<Response>((done) => { resolve = done })
  return { promise, resolve }
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

function parentFingerprint(url: URL): string {
  const query = url.searchParams
  return createHash('sha256').update(canonicalInviteTopUpQuery({
    search: query.get('search') || '',
    startDate: query.get('start_date') || '',
    endDate: query.get('end_date') || '',
    sortBy: query.get('sort_by') || '',
    sortDir: query.get('sort_dir') || '',
    asOf: Number(query.get('as_of')),
  })).digest('hex')
}

function parentRow(id: number, username: string, successTopUps = id): ParentRow {
  return {
    inviter_id: id,
    inviter_username: username,
    inviter_display_name: null,
    current_invitee_count: id + 2,
    window_paying_invitee_count: id + 1,
    rewarded_invite_count: id,
    success_topup_count: successTopUps,
    success_amount: null,
    success_money: null,
    last_topup_at: 1_700_000_000 + id,
    detail_evidence_hash: id.toString(16).padStart(64, '0'),
  }
}

function summaryData(url: URL, items: ParentRow[]) {
  return {
    currency: 'source',
    unit: 'raw',
    source_state: 'unreconciled',
    as_of: Number(url.searchParams.get('as_of')),
    query_fingerprint: parentFingerprint(url),
    window_active_inviter_count: items.length,
    current_invitee_count: items.reduce((sum, item) => sum + item.current_invitee_count, 0),
    window_paying_invitee_count: items.reduce((sum, item) => sum + item.window_paying_invitee_count, 0),
    rewarded_invite_count: items.reduce((sum, item) => sum + item.rewarded_invite_count, 0),
    success_topup_count: items.reduce((sum, item) => sum + item.success_topup_count, 0),
    total_amount: null,
    total_money: null,
  }
}

function listBody(url: URL, items: ParentRow[]) {
  const page = Number(url.searchParams.get('page'))
  const pageSize = Number(url.searchParams.get('page_size'))
  return {
    success: true,
    data: {
      currency: 'source',
      unit: 'raw',
      source_state: 'unreconciled',
      as_of: Number(url.searchParams.get('as_of')),
      query_fingerprint: parentFingerprint(url),
      summary: summaryData(url, items),
      items,
      total: items.length,
      page,
      page_size: pageSize,
      total_pages: 1,
    },
  }
}

function detailBody(url: URL, inviterID: number, username: string, evidenceHash: string) {
  return {
    success: true,
    data: {
      currency: 'source',
      unit: 'raw',
      source_state: 'unreconciled',
      as_of: Number(url.searchParams.get('as_of')),
      query_fingerprint: url.searchParams.get('query_fingerprint'),
      inviter_id: inviterID,
      items: [{
        id: inviterID * 10,
        user_id: inviterID * 100,
        username,
        amount: null,
        money: null,
        complete_time: 1_700_000_000 + inviterID,
        status: 'success',
      }],
      total: Number(url.searchParams.get('expected_total')),
      page: Number(url.searchParams.get('page')),
      page_size: Number(url.searchParams.get('page_size')),
      total_pages: 1,
      total_amount: null,
      total_money: null,
      detail_evidence_hash: evidenceHash,
    },
  }
}

function useImmediateParentHandlers(items = [parentRow(1, 'initial-inviter')]) {
  server.use(
    http.get('*/api/users/invite-topup-analysis', ({ request }) => {
      const url = new URL(request.url)
      return jsonResponse(listBody(url, items))
    }),
  )
}

describe('AffiliateStats request ownership and accessibility', () => {
  it('loads list and summary from one query identity and exposes labelled removable filters', async () => {
    const listRequests: URL[] = []
    const summaryRequests: URL[] = []
    server.use(
      http.get('*/api/users/invite-topup-analysis/summary', ({ request }) => {
        summaryRequests.push(new URL(request.url))
        return jsonResponse({ success: false }, 500)
      }),
      http.get('*/api/users/invite-topup-analysis', ({ request }) => {
        const url = new URL(request.url)
        listRequests.push(url)
        return jsonResponse(listBody(url, [parentRow(4, 'identity-inviter', 4)]))
      }),
    )

    const actor = userEvent.setup()
    render(<AffiliateStats />)
    expect(await screen.findByText('identity-inviter')).toBeVisible()

    const search = screen.getByRole('textbox', { name: '搜索邀请人' })
    const startDate = screen.getByLabelText('完成日期起始')
    const endDate = screen.getByLabelText('完成日期结束（含当日）')
    await actor.type(search, 'alice')
    await actor.click(screen.getByRole('button', { name: '应用搜索' }))
    await waitFor(() => expect(listRequests.some(url => url.searchParams.get('search') === 'alice')).toBe(true))

    fireEvent.change(startDate, { target: { value: '2026-08-01' } })
    await waitFor(() => expect(listRequests.some(url => url.searchParams.get('start_date') === '2026-08-01')).toBe(true))
    fireEvent.change(endDate, { target: { value: '2026-08-03' } })

    const finalList = await waitFor(() => {
      const match = [...listRequests].reverse().find(url =>
        url.searchParams.get('search') === 'alice'
        && url.searchParams.get('start_date') === '2026-08-01'
        && url.searchParams.get('end_date') === '2026-08-03')
      expect(match).toBeDefined()
      return match as URL
    })
    expect(finalList.searchParams.get('page')).toBe('1')
    expect(finalList.searchParams.get('page_size')).toBe('20')
    expect(summaryRequests).toHaveLength(0)
    expect(screen.queryByText(/列表与汇总查询身份不一致/)).not.toBeInTheDocument()

    expect(screen.getByRole('group', { name: '已应用的邀请充值筛选' })).toBeVisible()
    await actor.click(screen.getByRole('button', { name: '移除搜索筛选：alice' }))
    expect(screen.queryByRole('button', { name: '移除搜索筛选：alice' })).not.toBeInTheDocument()
    expect(search).toHaveValue('')
    await actor.click(screen.getByRole('button', { name: '清除全部筛选' }))
    expect(screen.queryByRole('group', { name: '已应用的邀请充值筛选' })).not.toBeInTheDocument()
  })

  it('fails closed when the parent returns a well-formed but incorrect SHA-256 fingerprint', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => {})
    server.use(
      http.get('*/api/users/invite-topup-analysis', ({ request }) => {
        const body = listBody(new URL(request.url), [parentRow(8, 'wrong-fingerprint-inviter')])
        body.data.query_fingerprint = 'f'.repeat(64)
        body.data.summary.query_fingerprint = 'f'.repeat(64)
        return jsonResponse(body)
      }),
    )

    render(<AffiliateStats />)

    expect(await screen.findByRole('alert')).toHaveTextContent('列表暂不可用')
    expect(screen.queryByText('wrong-fingerprint-inviter')).not.toBeInTheDocument()
  })

  it.each([
    ['unavailable', undefined],
    ['digest failure', { subtle: { digest: vi.fn().mockRejectedValue(new Error('digest failed')) } }],
  ])('fails closed before requesting data when Web Crypto is %s', async (_case, cryptoProvider) => {
    vi.spyOn(console, 'error').mockImplementation(() => {})
    let requests = 0
    server.use(http.get('*/api/users/invite-topup-analysis', () => {
      requests += 1
      return jsonResponse({ success: false }, 500)
    }))
    vi.stubGlobal('crypto', cryptoProvider)
    try {
      const view = render(<AffiliateStats />)
      expect(await screen.findByRole('alert')).toHaveTextContent('列表暂不可用')
      expect(requests).toBe(0)
      view.unmount()
    } finally {
      vi.unstubAllGlobals()
    }
  })

  it('ignores an older parent bundle completion after a newer query wins', async () => {
    const alphaList = deferredResponse()
    const betaList = deferredResponse()
    let alphaListURL: URL | undefined
    let betaListURL: URL | undefined

    server.use(
      http.get('*/api/users/invite-topup-analysis', ({ request }) => {
        const url = new URL(request.url)
        const search = url.searchParams.get('search')
        if (search === 'alpha') {
          alphaListURL = url
          return alphaList.promise.then(response => response.clone())
        }
        if (search === 'beta') {
          betaListURL = url
          return betaList.promise.then(response => response.clone())
        }
        return jsonResponse(listBody(url, [parentRow(1, 'initial-inviter')]))
      }),
    )

    const actor = userEvent.setup()
    render(<AffiliateStats />)
    expect(await screen.findByText('initial-inviter')).toBeVisible()
    const search = screen.getByRole('textbox', { name: '搜索邀请人' })

    await actor.type(search, 'alpha')
    await actor.click(screen.getByRole('button', { name: '应用搜索' }))
    await waitFor(() => expect(alphaListURL).toBeDefined())
    await actor.clear(search)
    await actor.type(search, 'beta')
    await actor.click(screen.getByRole('button', { name: '应用搜索' }))
    await waitFor(() => expect(betaListURL).toBeDefined())

    betaList.resolve(jsonResponse(listBody(betaListURL!, [parentRow(77, 'newer-beta-inviter', 77)])))
    expect(await screen.findByText('newer-beta-inviter')).toBeVisible()
    expect(await screen.findByText('77 人')).toBeVisible()

    alphaList.resolve(jsonResponse(listBody(alphaListURL!, [parentRow(11, 'older-alpha-inviter', 11)])))
    await waitFor(() => expect(screen.queryByText('older-alpha-inviter')).not.toBeInTheDocument())
    expect(screen.getByText('newer-beta-inviter')).toBeVisible()
    expect(screen.getByText('77 人')).toBeVisible()
  })

  it('binds detail responses to the currently expanded inviter', async () => {
    const parentRows = [parentRow(1, 'inviter-a'), parentRow(2, 'inviter-b')]
    useImmediateParentHandlers(parentRows)
    const inviterA = deferredResponse()
    const inviterB = deferredResponse()
    let inviterAURL: URL | undefined
    let inviterBURL: URL | undefined
    server.use(
      http.get('*/api/users/invite-topup-analysis/:inviterID/details', ({ params, request }) => {
        const url = new URL(request.url)
        if (params.inviterID === '1') {
          inviterAURL = url
          return inviterA.promise.then(response => response.clone())
        }
        inviterBURL = url
        return inviterB.promise.then(response => response.clone())
      }),
    )

    const actor = userEvent.setup()
    render(<AffiliateStats />)
    await screen.findByText('inviter-a')
    await actor.click(screen.getByRole('button', { name: '展开邀请人 inviter-a 的逐笔充值记录' }))
    await waitFor(() => expect(inviterAURL).toBeDefined())
    expect(inviterAURL!.searchParams.get('expected_evidence_hash')).toBe(parentRows[0].detail_evidence_hash)
    expect(inviterAURL!.searchParams.get('expected_total')).toBe(String(parentRows[0].success_topup_count))
    await actor.click(screen.getByRole('button', { name: '展开邀请人 inviter-b 的逐笔充值记录' }))
    await waitFor(() => expect(inviterBURL).toBeDefined())
    expect(inviterBURL!.searchParams.get('expected_evidence_hash')).toBe(parentRows[1].detail_evidence_hash)

    inviterB.resolve(jsonResponse(detailBody(inviterBURL!, 2, 'detail-for-b', parentRows[1].detail_evidence_hash)))
    expect(await screen.findByText('detail-for-b')).toBeVisible()
    inviterA.resolve(jsonResponse(detailBody(inviterAURL!, 1, 'detail-for-a', parentRows[0].detail_evidence_hash)))
    await waitFor(() => expect(screen.queryByText('detail-for-a')).not.toBeInTheDocument())
    expect(screen.getByText('detail-for-b')).toBeVisible()
  })

  it('fails closed when detail evidence does not match the parent row', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => {})
    const parent = parentRow(5, 'evidence-inviter', 1)
    useImmediateParentHandlers([parent])
    server.use(
      http.get('*/api/users/invite-topup-analysis/:inviterID/details', ({ params, request }) => {
        const url = new URL(request.url)
        const body = detailBody(url, Number(params.inviterID), 'untrusted-detail', parent.detail_evidence_hash)
        body.data.detail_evidence_hash = 'f'.repeat(64)
        return jsonResponse(body)
      }),
    )

    const actor = userEvent.setup()
    render(<AffiliateStats />)
    await screen.findByText('evidence-inviter')
    await actor.click(screen.getByRole('button', { name: /evidence-inviter/ }))

    expect(await screen.findByRole('alert')).toBeVisible()
    expect(screen.queryByText('untrusted-detail')).not.toBeInTheDocument()
  })

  it.each([
    ['missing', ''],
    ['malformed', 'not-a-sha256'],
  ])('fails closed when a parent row has a %s evidence hash', async (_case, evidenceHash) => {
    vi.spyOn(console, 'error').mockImplementation(() => {})
    const invalidParent = { ...parentRow(9, 'invalid-evidence-inviter', 1), detail_evidence_hash: evidenceHash }
    useImmediateParentHandlers([invalidParent])

    render(<AffiliateStats />)

    expect(await screen.findByRole('alert')).toBeVisible()
    expect(screen.queryByText('invalid-evidence-inviter')).not.toBeInTheDocument()
  })

  it('invalidates an in-flight detail when a new parent query changes the evidence identity', async () => {
    const oldDetail = deferredResponse()
    const oldParent = parentRow(6, 'old-parent-inviter', 1)
    const newParent = { ...parentRow(6, 'new-parent-inviter', 1), detail_evidence_hash: 'e'.repeat(64) }
    let oldDetailURL: URL | undefined
    let newDetailURL: URL | undefined
    let detailCalls = 0
    server.use(
      http.get('*/api/users/invite-topup-analysis', ({ request }) => {
        const url = new URL(request.url)
        return jsonResponse(listBody(url, url.searchParams.get('search') === 'new' ? [newParent] : [oldParent]))
      }),
      http.get('*/api/users/invite-topup-analysis/:inviterID/details', ({ request }) => {
        detailCalls += 1
        const url = new URL(request.url)
        if (detailCalls === 1) {
          oldDetailURL = url
          return oldDetail.promise.then(response => response.clone())
        }
        newDetailURL = url
        return jsonResponse(detailBody(url, 6, 'new-parent-detail', newParent.detail_evidence_hash))
      }),
    )

    const actor = userEvent.setup()
    render(<AffiliateStats />)
    await screen.findByText('old-parent-inviter')
    await actor.click(screen.getByRole('button', { name: /old-parent-inviter/ }))
    await waitFor(() => expect(oldDetailURL).toBeDefined())

    const search = screen.getByRole('textbox', { name: '搜索邀请人' })
    await actor.type(search, 'new')
    await actor.keyboard('{Enter}')
    expect(await screen.findByText('new-parent-inviter')).toBeVisible()

    oldDetail.resolve(jsonResponse(detailBody(oldDetailURL!, 6, 'stale-old-detail', oldParent.detail_evidence_hash)))
    await waitFor(() => expect(screen.queryByText('stale-old-detail')).not.toBeInTheDocument())

    await actor.click(screen.getByRole('button', { name: /new-parent-inviter/ }))
    expect(await screen.findByText('new-parent-detail')).toBeVisible()
    expect(newDetailURL?.searchParams.get('expected_evidence_hash')).toBe(newParent.detail_evidence_hash)
    expect(newDetailURL?.searchParams.get('query_fingerprint')).not.toBe(oldDetailURL?.searchParams.get('query_fingerprint'))
  })

  it('supports native Tab, Enter, and Space expansion and maintains aria-sort', async () => {
    const keyboardParent = parentRow(3, 'keyboard-inviter', 3)
    useImmediateParentHandlers([keyboardParent])
    server.use(
      http.get('*/api/users/invite-topup-analysis/:inviterID/details', ({ params, request }) => {
        const url = new URL(request.url)
        return jsonResponse(detailBody(url, Number(params.inviterID), 'keyboard-detail', keyboardParent.detail_evidence_hash))
      }),
    )

    const actor = userEvent.setup()
    render(<AffiliateStats />)
    const expand = await screen.findByRole('button', { name: '展开邀请人 keyboard-inviter 的逐笔充值记录' })
    expect(expand).toHaveAttribute('type', 'button')

    for (let index = 0; index < 25 && document.activeElement !== expand; index += 1) {
      await actor.tab()
    }
    expect(expand).toHaveFocus()
    await actor.keyboard('{Enter}')
    expect(await screen.findByText('keyboard-detail')).toBeVisible()
    const collapse = screen.getByRole('button', { name: '收起邀请人 keyboard-inviter 的逐笔充值记录' })
    expect(collapse).toHaveFocus()
    await actor.keyboard(' ')
    await waitFor(() => expect(screen.queryByText('keyboard-detail')).not.toBeInTheDocument())

    const activeHeader = screen.getByRole('columnheader', { name: /窗口充值笔数/ })
    expect(activeHeader).toHaveAttribute('aria-sort', 'descending')
    const sortButton = within(activeHeader).getByRole('button')
    sortButton.focus()
    await actor.keyboard('{Enter}')
    const ascendingHeader = await screen.findByRole('columnheader', { name: /窗口充值笔数/ })
    expect(ascendingHeader).toHaveAttribute('aria-sort', 'ascending')
    expect(screen.getByRole('columnheader', { name: /当前邀请用户/ })).toHaveAttribute('aria-sort', 'none')
  })

  it('distinguishes empty and unavailable parent states', async () => {
    useImmediateParentHandlers([])
    const { unmount } = render(<AffiliateStats />)
    expect(await screen.findByText('当前查询条件下没有完成的邀请充值记录')).toBeVisible()
    expect(screen.queryByText(/列表不可用；这不是/)).not.toBeInTheDocument()
    unmount()

    vi.spyOn(console, 'error').mockImplementation(() => {})
    server.resetHandlers()
    server.use(
      http.get('*/api/users/invite-topup-analysis', () => jsonResponse({ success: false, error: { message: 'list down' } }, 503)),
    )
    render(<AffiliateStats />)
    expect(await screen.findByText('列表不可用；这不是“暂无数据”。')).toBeVisible()
    expect(screen.getByRole('alert')).toHaveTextContent('列表暂不可用，未显示为 0 或空数据。')
  })

  it('keeps prior rows and labels them stale when refresh fails', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => {})
    let listCalls = 0
    server.use(
      http.get('*/api/users/invite-topup-analysis', ({ request }) => {
        listCalls += 1
        if (listCalls > 1) return jsonResponse({ success: false, error: { message: 'list refresh failed' } }, 503)
        const url = new URL(request.url)
        return jsonResponse(listBody(url, [parentRow(8, 'stale-inviter', 8)]))
      }),
    )

    const actor = userEvent.setup()
    render(<AffiliateStats />)
    expect(await screen.findByText('stale-inviter')).toBeVisible()
    const refresh = screen.getByRole('button', { name: '刷新' })
    await waitFor(() => expect(refresh).toBeEnabled())
    await actor.click(refresh)

    expect(await screen.findByText(/列表刷新失败，当前保留的是上次成功数据/)).toBeVisible()
    expect(screen.getByText('stale-inviter')).toBeVisible()
    expect(screen.queryByText(/列表不可用；这不是/)).not.toBeInTheDocument()
  })
})
