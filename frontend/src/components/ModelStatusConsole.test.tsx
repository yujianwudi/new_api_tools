import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http } from 'msw'
import { describe, expect, it, vi } from 'vitest'

import { server } from '../test/server'
import { ModelStatusConsole } from './ModelStatusConsole'

const { showToast } = vi.hoisted(() => ({ showToast: vi.fn() }))

vi.mock('../contexts/AuthContext', () => ({
  useAuth: () => ({ token: 'test-token', apiKey: '' }),
}))

vi.mock('./Toast', () => ({
  useToast: () => ({ showToast }),
}))

function response(data: unknown) {
  return Response.json({ success: true, data })
}

function installModelStatusHandlers() {
  server.use(
    http.get('*/api/model-status/models', () => response([{ model_name: 'model-a', request_count_24h: 12 }])),
    http.get('*/api/model-status/config/selected', () => Response.json({
      success: true,
      data: ['model-a'],
      max_batch: 50,
      time_window: '24h',
    })),
    http.get('*/api/model-status/token-groups', () => response([])),
    http.post('*/api/model-status/status/batch', () => response([{
      model_name: 'model-a',
      time_window: '24h',
      total_requests: 12,
      success_count: 12,
      failure_count: 0,
      success_rate: 100,
      current_status: 'green',
      traffic_health: 'healthy',
      source_state: 'fresh',
      slot_data: [],
    }])),
    http.post('*/api/model-status/probes/summary', () => response({
      fetched_at: new Date().toISOString(),
      config: {
        enabled: false,
        configured: false,
        running: false,
        state: 'disabled',
        message: '主动探测已关闭',
        allowed_models: [],
        daily_request_budget: 10,
        requests_used_today: 0,
        requests_remaining_today: 10,
        interval_seconds: 3600,
        max_models_per_run: 10,
        max_concurrency: 1,
        retention_days: 7,
      },
      items: [{
        model_name: 'model-a',
        capability: 'chat',
        probe_health: 'unavailable',
        source_state: 'unavailable',
        availability_24h: null,
        attempt_count_24h: 0,
        success_count_24h: 0,
        failure_count_24h: 0,
        skipped_count_24h: 0,
        average_header_latency_ms: null,
        average_first_token_latency_ms: null,
        average_total_latency_ms: null,
        reason: '主动探测已关闭',
      }],
    })),
    http.get('*/api/model-status/probes/history', () => response([])),
  )
}

describe('ModelStatusConsole keyboard and dialog lifecycle', () => {
  it('opens the desktop detail from a native button and restores focus after Escape', async () => {
    installModelStatusHandlers()
    const actor = userEvent.setup()
    render(<ModelStatusConsole />)

    const detailTrigger = await screen.findByRole('button', { name: '打开模型 model-a 诊断详情' })
    detailTrigger.focus()
    await actor.keyboard('{Enter}')
    expect(await screen.findByRole('dialog', { name: 'model-a' })).toBeVisible()
    await actor.keyboard('{Escape}')
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'model-a' })).not.toBeInTheDocument())
    expect(detailTrigger).toHaveFocus()
  })

  it('focuses the monitor-range search and restores the trigger when closed', async () => {
    installModelStatusHandlers()
    const actor = userEvent.setup()
    render(<ModelStatusConsole />)

    const rangeTrigger = await screen.findByRole('button', { name: '监控范围' })
    await actor.click(rangeTrigger)
    expect(await screen.findByRole('dialog', { name: '监控范围' })).toBeVisible()
    expect(screen.getByRole('textbox', { name: '搜索可监控模型' })).toHaveFocus()
    await actor.keyboard('{Escape}')
    await waitFor(() => expect(screen.queryByRole('dialog', { name: '监控范围' })).not.toBeInTheDocument())
    expect(rangeTrigger).toHaveFocus()
  })
})
