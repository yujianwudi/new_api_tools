import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http } from 'msw'
import type { ReactNode } from 'react'
import { describe, expect, it, vi } from 'vitest'

import { UserManagement } from './UserManagement'
import { server } from '../test/server'

const { showToast } = vi.hoisted(() => ({ showToast: vi.fn() }))

vi.mock('../contexts/AuthContext', () => ({
  useAuth: () => ({ token: 'test-token' }),
}))

vi.mock('./Toast', () => ({
  useToast: () => ({ showToast }),
}))

vi.mock('./AffiliateStats', () => ({ AffiliateStats: () => null }))
vi.mock('./UserAnalysisDialog', () => ({
  UserAnalysisDialog: ({ open, renderExtra }: { open: boolean; renderExtra?: () => ReactNode }) => (
    open ? <div data-testid="analysis-dialog">{renderExtra?.()}</div> : null
  ),
}))
vi.mock('../lib/controlPlane', () => ({
  canSafelyHardDelete: () => false,
  fetchNewAPICapabilities: vi.fn(async () => ({
    status: {}, capabilities: {}, admin_credentials_configured: false, write_mode: 'read_only', checked_at: '',
  })),
}))

interface DeferredResponse {
  promise: Promise<Response>
  resolve: (response: Response) => void
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

function userPayload(id: number, username: string, activity = 'active') {
  return {
    success: true,
    data: {
      items: [{
        id, username, display_name: null, email: null, role: 1, status: 1,
        quota: 0, used_quota: 0, request_count: activity === 'never' ? 0 : 1,
        group: 'default', last_request_time: 1_700_000_000,
        activity_level: activity, linux_do_id: null, source: 'password',
      }],
      total: 1, page: 1, page_size: 20, total_pages: 1,
    },
  }
}

function invitedPayload(
  inviterID: number,
  userID: number,
  username: string,
  overrides: Record<string, unknown> = {},
) {
  return {
    success: true,
    data: {
      inviter: { user_id: inviterID, username: `inviter-${inviterID}`, aff_count: 1 },
      items: [{
        user_id: userID, username, status: 1, request_count: 1, group: 'default', used_quota: 0,
      }],
      total: 1, page: 1, page_size: 10, total_pages: 1, as_of: 1_700_000_000,
      query_fingerprint: 'a'.repeat(64),
      stats: { total_invited: 1, requested_count: 1, banned_count: 0, total_used_quota: 0, total_requests: 1 },
      ...overrides,
    },
  }
}

function invitedPagePayload(
  inviterID: number,
  page: number,
  asOf = 1_700_000_000,
  fingerprint = 'a'.repeat(64),
) {
  const total = 11
  const count = page === 1 ? 10 : 1
  const items = Array.from({ length: count }, (_, index) => ({
    user_id: page * 100 + index,
    username: `snapshot-page-${page}-user-${index}`,
    status: 1,
    request_count: 1,
    group: 'default',
    used_quota: 0,
  }))
  return invitedPayload(inviterID, items[0].user_id, items[0].username, {
    items,
    total,
    page,
    page_size: 10,
    total_pages: 2,
    as_of: asOf,
    query_fingerprint: fingerprint,
    stats: { total_invited: total, requested_count: total, banned_count: 0, total_used_quota: 0, total_requests: total },
  })
}

function useSingleUserList(userID = 1, username = 'snapshot-inviter') {
  server.use(
    http.get('*/api/users/stats', () => jsonResponse({
      success: true,
      data: { total_users: 1, active_users: 1, inactive_users: 0, very_inactive_users: 0, never_requested: 0 },
    })),
    http.get('*/api/users/soft-deleted/count', () => jsonResponse({ success: true, data: { count: 0 } })),
    http.get('*/api/auto-group/groups', () => jsonResponse({ success: true, data: { items: [], total: 0 } })),
    http.get('*/api/users', () => jsonResponse(userPayload(userID, username))),
  )
}

describe('UserManagement request ownership', () => {
  it('keeps trustworthy never-requested evidence when full activity statistics are partial', async () => {
    server.use(
      http.get('*/api/users/stats', () => jsonResponse({
          success: true,
          data: {
            total_users: 1,
            active_users: null,
            inactive_users: null,
            very_inactive_users: null,
            never_requested: 1,
            source_state: 'partial',
          },
        })),
      http.get('*/api/users/soft-deleted/count', () => jsonResponse({ success: true, data: { count: 0 } })),
      http.get('*/api/auto-group/groups', () => jsonResponse({ success: true, data: { items: [], total: 0 } })),
      http.get('*/api/users', () => jsonResponse(userPayload(1, 'stats-user', 'never'))),
    )

    render(<UserManagement />)
    expect(await screen.findByText('活跃度证据不完整；当前仅“从未请求”可作为可信数字，其余指标不会显示为 0。')).toBeVisible()
    expect(screen.getAllByText('N/A')).toHaveLength(3)
    expect(screen.getByRole('button', { name: /^从未请求 1/ })).toBeVisible()
    expect(screen.getByRole('button', { name: '预览非常不活跃 (N/A)' })).toBeDisabled()
    expect(screen.getByRole('button', { name: '预览从未请求 (1)' })).toBeEnabled()
  })

  it('clamps deletion counters at zero after deleting a stale-classified user', async () => {
    server.use(
      http.get('*/api/users/stats', () => jsonResponse({
        success: true,
        data: { total_users: 0, active_users: 0, inactive_users: 0, very_inactive_users: 0, never_requested: 0, source_state: 'fresh' },
      })),
      http.get('*/api/users/soft-deleted/count', () => jsonResponse({ success: true, data: { count: 0 } })),
      http.get('*/api/auto-group/groups', () => jsonResponse({ success: true, data: { items: [], total: 0 } })),
      http.get('*/api/users', () => jsonResponse(userPayload(1, 'stale-never-user', 'never'))),
      http.delete('*/api/users/:userID', () => jsonResponse({ success: true, message: '用户已注销' })),
    )

    const actor = userEvent.setup()
    render(<UserManagement />)
    await screen.findByText('stale-never-user')
    expect(screen.getByRole('button', { name: /^从未请求 0/ })).toBeVisible()

    await actor.click(screen.getByRole('button', { name: '删除用户 stale-never-user' }))
    await actor.type(screen.getByPlaceholderText('例如：用户主动申请注销'), '测试注销原因')
    await actor.type(screen.getByPlaceholderText('请输入 注销用户'), '注销用户')
    await actor.click(screen.getByRole('button', { name: '确认注销' }))

    await waitFor(() => expect(screen.queryByText('stale-never-user')).not.toBeInTheDocument())
    expect(screen.getByRole('button', { name: /^从未请求 0/ })).toBeVisible()
    expect(screen.queryByText('-1')).not.toBeInTheDocument()
  })

  it('prevents an aborted older statistics response from overwriting a newer refresh', async () => {
    const olderFull = deferredResponse()
    let fullCalls = 0
    let markOlderStarted!: () => void
    const olderStarted = new Promise<void>(resolve => { markOlderStarted = resolve })
    server.use(
      http.get('*/api/users/stats', ({ request }) => {
        const quick = new URL(request.url).searchParams.get('quick') === 'true'
        if (quick) {
          return jsonResponse({
            success: true,
            data: {
              total_users: 1,
              active_users: null,
              inactive_users: null,
              very_inactive_users: null,
              never_requested: 1,
              source_state: 'partial',
            },
          })
        }
        fullCalls += 1
        if (fullCalls === 1) {
          markOlderStarted()
          return olderFull.promise.then(response => response.clone())
        }
        return jsonResponse({
          success: true,
          data: {
            total_users: 7,
            active_users: 7,
            inactive_users: 0,
            very_inactive_users: 0,
            never_requested: 0,
            source_state: 'fresh',
          },
        })
      }),
      http.get('*/api/users/soft-deleted/count', () => jsonResponse({ success: true, data: { count: 0 } })),
      http.get('*/api/auto-group/groups', () => jsonResponse({ success: true, data: { items: [], total: 0 } })),
      http.get('*/api/users', () => jsonResponse(userPayload(1, 'stats-owner-user'))),
    )

    const actor = userEvent.setup()
    render(<UserManagement />)
    expect(await screen.findByText('stats-owner-user')).toBeVisible()
    await olderStarted

    await actor.click(screen.getByRole('button', { name: '刷新' }))
    expect(await screen.findByRole('button', { name: /^活跃用户 7/ })).toBeVisible()

    olderFull.resolve(jsonResponse({
      success: true,
      data: {
        total_users: 99,
        active_users: 99,
        inactive_users: 0,
        very_inactive_users: 0,
        never_requested: 0,
        source_state: 'fresh',
      },
    }))
    await waitFor(() => expect(screen.queryByRole('button', { name: /^活跃用户 99/ })).not.toBeInTheDocument())
    expect(screen.getByRole('button', { name: /^活跃用户 7/ })).toBeVisible()
  })

  it('keeps rows locked while a newer request is pending and ignores the older completion', async () => {
    const active = deferredResponse()
    const inactive = deferredResponse()
    server.use(
      http.get('*/api/users/stats', () => jsonResponse({
          success: true,
          data: { total_users: 1, active_users: 1, inactive_users: 0, very_inactive_users: 0, never_requested: 0 },
      })),
      http.get('*/api/users/soft-deleted/count', () => jsonResponse({ success: true, data: { count: 0 } })),
      http.get('*/api/auto-group/groups', () => jsonResponse({ success: true, data: { items: [], total: 0 } })),
      http.get('*/api/users', ({ request }) => {
        const query = new URL(request.url).searchParams
        if (query.get('activity') === 'active') return active.promise.then(response => response.clone())
        if (query.get('activity') === 'inactive') return inactive.promise.then(response => response.clone())
        return jsonResponse(userPayload(1, 'initial-user'))
      }),
    )

    const actor = userEvent.setup()
    render(<UserManagement />)
    expect(await screen.findByText('initial-user')).toBeVisible()

    const activitySelect = screen.getByRole('combobox', { name: '按活跃度筛选用户' })
    await actor.selectOptions(activitySelect, 'active')
    await actor.selectOptions(activitySelect, 'inactive')

    active.resolve(jsonResponse(userPayload(2, 'older-active-user', 'active')))
    await waitFor(() => {
      expect(screen.getByText(/当前旧行已锁定/)).toBeVisible()
      expect(screen.getByRole('checkbox', { name: '选择用户 initial-user' })).toBeDisabled()
    })

    inactive.resolve(jsonResponse(userPayload(3, 'latest-inactive-user', 'inactive')))
    expect(await screen.findByText('latest-inactive-user')).toBeVisible()
    expect(screen.queryByText('older-active-user')).not.toBeInTheDocument()
    expect(screen.queryByText('initial-user')).not.toBeInTheDocument()
  })

  it('binds invited-user responses to the selected user and page', async () => {
    const inviterA = deferredResponse()
    const inviterB = deferredResponse()
    server.use(
      http.get('*/api/users/stats', () => jsonResponse({
        success: true,
        data: { total_users: 2, active_users: 2, inactive_users: 0, very_inactive_users: 0, never_requested: 0 },
      })),
      http.get('*/api/users/soft-deleted/count', () => jsonResponse({ success: true, data: { count: 0 } })),
      http.get('*/api/auto-group/groups', () => jsonResponse({ success: true, data: { items: [], total: 0 } })),
      http.get('*/api/users', () => {
        const first = userPayload(1, 'user-a').data.items[0]
        const second = userPayload(2, 'user-b').data.items[0]
        return jsonResponse({ success: true, data: { items: [first, second], total: 2, page: 1, page_size: 20, total_pages: 1 } })
      }),
      http.get('*/api/users/:userID/invited', ({ params }) => {
        if (params.userID === '1') return inviterA.promise.then(response => response.clone())
        return inviterB.promise.then(response => response.clone())
      }),
    )

    const actor = userEvent.setup()
    render(<UserManagement />)
    await screen.findByText('user-a')

    await actor.click(screen.getByText('user-a'))
    await actor.click(screen.getByText('user-b'))
    inviterB.resolve(jsonResponse(invitedPayload(2, 202, 'invite-for-b')))
    expect(await screen.findByText('invite-for-b')).toBeVisible()

    inviterA.resolve(jsonResponse(invitedPayload(1, 101, 'invite-for-a')))
    await waitFor(() => expect(screen.queryByText('invite-for-a')).not.toBeInTheDocument())
    expect(screen.getByText('invite-for-b')).toBeVisible()
  })

  it('replays the first-page as_of and SHA-256 fingerprint for normal invited-user pagination', async () => {
    useSingleUserList()
    const requests: URL[] = []
    server.use(
      http.get('*/api/users/:userID/invited', ({ request }) => {
        const url = new URL(request.url)
        requests.push(url)
        const page = Number(url.searchParams.get('page'))
        return jsonResponse(invitedPagePayload(1, page))
      }),
    )

    const actor = userEvent.setup()
    render(<UserManagement />)
    await actor.click(await screen.findByText('snapshot-inviter'))
    expect(await screen.findByText('snapshot-page-1-user-0')).toBeVisible()

    await actor.click(screen.getByRole('button', { name: '邀请用户下一页' }))
    expect(await screen.findByText('snapshot-page-2-user-0')).toBeVisible()
    expect(requests).toHaveLength(2)
    expect(requests[0].searchParams.has('as_of')).toBe(false)
    expect(requests[0].searchParams.has('query_fingerprint')).toBe(false)
    expect(requests[1].searchParams.get('as_of')).toBe('1700000000')
    expect(requests[1].searchParams.get('query_fingerprint')).toBe('a'.repeat(64))
  })

  it.each([
    ['as_of drift', 1_700_000_001, 'a'.repeat(64)],
    ['well-formed but wrong fingerprint', 1_700_000_000, 'b'.repeat(64)],
  ])('clears invited-user data when a later page has %s', async (_case, asOf, fingerprint) => {
    vi.spyOn(console, 'error').mockImplementation(() => {})
    useSingleUserList()
    server.use(
      http.get('*/api/users/:userID/invited', ({ request }) => {
        const page = Number(new URL(request.url).searchParams.get('page'))
        return jsonResponse(invitedPagePayload(1, page, page === 1 ? 1_700_000_000 : asOf, page === 1 ? 'a'.repeat(64) : fingerprint))
      }),
    )

    const actor = userEvent.setup()
    render(<UserManagement />)
    await actor.click(await screen.findByText('snapshot-inviter'))
    expect(await screen.findByText('snapshot-page-1-user-0')).toBeVisible()
    await actor.click(screen.getByRole('button', { name: '邀请用户下一页' }))

    expect(await screen.findByText(/邀请详情当前不可用/)).toBeVisible()
    expect(screen.queryByText('snapshot-page-1-user-0')).not.toBeInTheDocument()
    expect(screen.queryByText('snapshot-page-2-user-0')).not.toBeInTheDocument()
  })

  it.each([
    ['missing', ''],
    ['non-SHA-256', 'not-a-sha256'],
  ])('fails closed when the first invited-user page has a %s fingerprint', async (_case, fingerprint) => {
    vi.spyOn(console, 'error').mockImplementation(() => {})
    useSingleUserList()
    server.use(
      http.get('*/api/users/:userID/invited', () => jsonResponse(invitedPagePayload(1, 1, 1_700_000_000, fingerprint))),
    )

    const actor = userEvent.setup()
    render(<UserManagement />)
    await actor.click(await screen.findByText('snapshot-inviter'))
    expect(await screen.findByText(/邀请详情当前不可用/)).toBeVisible()
    expect(screen.queryByText('snapshot-page-1-user-0')).not.toBeInTheDocument()
  })

  it('uses labelled native controls and opens user details with Tab, Enter, and Space', async () => {
    server.use(
      http.get('*/api/users/stats', () => jsonResponse({
        success: true,
        data: { total_users: 2, active_users: 2, inactive_users: 0, very_inactive_users: 0, never_requested: 0 },
      })),
      http.get('*/api/users/soft-deleted/count', () => jsonResponse({ success: true, data: { count: 0 } })),
      http.get('*/api/auto-group/groups', () => jsonResponse({ success: true, data: { items: [], total: 0 } })),
      http.get('*/api/users', () => {
        const first = userPayload(1, 'keyboard-a').data.items[0]
        const second = userPayload(2, 'keyboard-b').data.items[0]
        return jsonResponse({ success: true, data: { items: [first, second], total: 2, page: 1, page_size: 20, total_pages: 1 } })
      }),
      http.get('*/api/users/:userID/invited', ({ params }) => {
        const inviterID = Number(params.userID)
        return jsonResponse(invitedPayload(inviterID, inviterID * 100, `invite-for-keyboard-${inviterID}`))
      }),
    )

    const actor = userEvent.setup()
    render(<UserManagement />)

    expect(await screen.findByRole('textbox', { name: '搜索用户' })).toBeVisible()
    const firstTrigger = screen.getByRole('button', { name: '打开用户 keyboard-a 详情' })
    expect(firstTrigger).toHaveAttribute('type', 'button')
    for (let index = 0; index < 40 && document.activeElement !== firstTrigger; index += 1) {
      await actor.tab()
    }
    expect(firstTrigger).toHaveFocus()
    await actor.keyboard('{Enter}')
    expect(await screen.findByText('invite-for-keyboard-1')).toBeVisible()

    const secondTrigger = screen.getByRole('button', { name: '打开用户 keyboard-b 详情' })
    secondTrigger.focus()
    expect(secondTrigger).toHaveFocus()
    await actor.keyboard(' ')
    expect(await screen.findByText('invite-for-keyboard-2')).toBeVisible()

    expect(screen.getByRole('button', { name: '查看用户 keyboard-a 的分析' })).toBeVisible()
    expect(screen.getByRole('button', { name: '删除用户 keyboard-a' })).toBeVisible()
  })
})
