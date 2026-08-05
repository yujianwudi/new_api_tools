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
  it.each(['model catalog', 'selected config', 'token groups'] as const)(
    'fails closed on a malformed successful %s payload',
    async malformedSource => {
      let statusRequests = 0
      const modelsData = malformedSource === 'model catalog'
        ? {}
        : [{ model_name: 'model-a', request_count_24h: 12 }]
      const selectedData = malformedSource === 'selected config' ? {} : ['model-a']
      const groupsData = malformedSource === 'token groups'
        ? [{ group_name: 'broken-group', model_count: 1, models: [null] }]
        : []

      server.use(
        http.get('*/api/model-status/models', () => response(modelsData)),
        http.get('*/api/model-status/config/selected', () => Response.json({
          success: true,
          data: selectedData,
          max_batch: 50,
          time_window: '24h',
        })),
        http.get('*/api/model-status/token-groups', () => response(groupsData)),
        http.post('*/api/model-status/status/batch', () => {
          statusRequests += 1
          return response([])
        }),
        http.post('*/api/model-status/probes/summary', () => {
          statusRequests += 1
          return response({ items: [] })
        }),
      )

      render(<ModelStatusConsole />)

      expect(await screen.findByRole('alert')).toHaveTextContent('模型监测基础配置暂不可用')
      expect(screen.queryByText('没有符合当前筛选条件的模型')).not.toBeInTheDocument()
      expect(statusRequests).toBe(0)
    },
  )

  it('keeps a failed foundation unavailable and retries it before requesting statuses', async () => {
    let foundationUnavailable = true
    let statusRequests = 0
    server.use(
      http.get('*/api/model-status/models', () => foundationUnavailable
        ? Response.json({ success: false, message: 'foundation unavailable' }, { status: 503 })
        : response([{ model_name: 'model-a', request_count_24h: 12 }])),
      http.get('*/api/model-status/config/selected', () => Response.json({
        success: true,
        data: ['model-a'],
        max_batch: 50,
        time_window: '24h',
      })),
      http.get('*/api/model-status/token-groups', () => response([])),
      http.post('*/api/model-status/status/batch', () => {
        statusRequests += 1
        return response([{
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
        }])
      }),
      http.post('*/api/model-status/probes/summary', () => {
        statusRequests += 1
        return response({
          config: {
            enabled: false,
            configured: false,
            running: false,
            state: 'disabled',
            allowed_models: [],
          },
          items: [],
        })
      }),
    )

    const actor = userEvent.setup()
    render(<ModelStatusConsole />)

    expect(await screen.findByRole('alert')).toHaveTextContent('模型监测基础配置暂不可用')
    expect(screen.queryByText('没有符合当前筛选条件的模型')).not.toBeInTheDocument()
    expect(statusRequests).toBe(0)

    foundationUnavailable = false
    await actor.click(screen.getByRole('button', { name: '重试加载模型监测基础配置' }))

    expect(await screen.findByRole('button', { name: '打开模型 model-a 诊断详情' })).toBeVisible()
    await waitFor(() => expect(statusRequests).toBe(2))
  })

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
    await waitFor(() => expect(screen.getByRole('textbox', { name: '搜索可监控模型' })).toHaveFocus())
    await actor.keyboard('{Escape}')
    await waitFor(() => expect(screen.queryByRole('dialog', { name: '监控范围' })).not.toBeInTheDocument())
    expect(rangeTrigger).toHaveFocus()
  })

  it('does not try to focus a detached detail trigger when the dialog closes', async () => {
    installModelStatusHandlers()
    const actor = userEvent.setup()
    render(<ModelStatusConsole />)

    const detailTrigger = await screen.findByRole('button', { name: '打开模型 model-a 诊断详情' })
    await actor.click(detailTrigger)
    expect(await screen.findByRole('dialog', { name: 'model-a' })).toBeVisible()

    const focus = vi.spyOn(detailTrigger, 'focus')
    const dispatchEvent = EventTarget.prototype.dispatchEvent
    let unmountAutoFocusDispatched = false
    vi.spyOn(EventTarget.prototype, 'dispatchEvent').mockImplementation(function (this: EventTarget, event) {
      const dispatched = dispatchEvent.call(this, event)
      if (event.type === 'focusScope.autoFocusOnUnmount') unmountAutoFocusDispatched = true
      return dispatched
    })
    detailTrigger.remove()

    await actor.keyboard('{Escape}')
    await waitFor(() => expect(unmountAutoFocusDispatched).toBe(true))
    expect(focus).not.toHaveBeenCalled()
  })

  it('does not try to focus a detached monitor-range trigger when the dialog closes', async () => {
    installModelStatusHandlers()
    const actor = userEvent.setup()
    render(<ModelStatusConsole />)

    const rangeTrigger = await screen.findByRole('button', { name: '监控范围' })
    await actor.click(rangeTrigger)
    expect(await screen.findByRole('dialog', { name: '监控范围' })).toBeVisible()

    const focus = vi.spyOn(rangeTrigger, 'focus')
    const dispatchEvent = EventTarget.prototype.dispatchEvent
    let unmountAutoFocusDispatched = false
    vi.spyOn(EventTarget.prototype, 'dispatchEvent').mockImplementation(function (this: EventTarget, event) {
      const dispatched = dispatchEvent.call(this, event)
      if (event.type === 'focusScope.autoFocusOnUnmount') unmountAutoFocusDispatched = true
      return dispatched
    })
    rangeTrigger.remove()

    await actor.keyboard('{Escape}')
    await waitFor(() => expect(unmountAutoFocusDispatched).toBe(true))
    expect(focus).not.toHaveBeenCalled()
  })

  it('does not describe a stale traffic snapshot as a degraded numeric rate', async () => {
    installModelStatusHandlers()
    const actor = userEvent.setup()
    server.use(http.post('*/api/model-status/status/batch', () => response([{
      model_name: 'model-a',
      time_window: '24h',
      total_requests: 12,
      success_count: 10,
      failure_count: 2,
      success_rate: 87.5,
      current_status: 'yellow',
      traffic_health: 'degraded',
      source_state: 'stale',
      slot_data: [],
    }])))

    render(<ModelStatusConsole />)

    expect((await screen.findAllByText('真实流量证据已过期')).length).toBeGreaterThan(0)
    expect(screen.queryByText(/真实流量成功率下降至/)).not.toBeInTheDocument()
    expect(screen.queryByText('87.5%')).not.toBeInTheDocument()

    await actor.click(screen.getByRole('button', { name: '打开模型 model-a 诊断详情' }))
    const trafficRateLabel = await screen.findByText('真实流量可用率')
    expect(trafficRateLabel.nextElementSibling).toHaveTextContent('—')
  })

  it('does not render an empty traffic snapshot as a numeric zero rate', async () => {
    installModelStatusHandlers()
    server.use(http.post('*/api/model-status/status/batch', () => response([{
      model_name: 'model-a',
      time_window: '24h',
      total_requests: 0,
      success_count: 0,
      failure_count: 0,
      success_rate: 0,
      current_status: 'unknown',
      traffic_health: 'unknown',
      source_state: 'empty',
      slot_data: [],
    }])))

    render(<ModelStatusConsole />)

    expect((await screen.findAllByText('当前窗口没有真实调用')).length).toBeGreaterThan(0)
    expect(screen.queryByText('0.0%')).not.toBeInTheDocument()
  })
})
