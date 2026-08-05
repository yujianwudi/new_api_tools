import { act, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http } from 'msw'
import { describe, expect, it, vi } from 'vitest'

import { server } from '../test/server'
import { ModelStatusEmbed } from './ModelStatusEmbed'

vi.mock('@lobehub/icons', () => {
  const Icon = () => null
  return {
    OpenAI: Icon, Gemini: Icon, DeepSeek: Icon, SiliconCloud: Icon, Groq: Icon,
    Ollama: Icon, Claude: Icon, Mistral: Icon, Minimax: Icon, Baichuan: Icon,
    Moonshot: Icon, Spark: Icon, Qwen: Icon, Yi: Icon, Hunyuan: Icon,
    Stepfun: Icon, ZeroOne: Icon, Zhipu: Icon, ChatGLM: Icon, Cohere: Icon,
    Perplexity: Icon, Together: Icon, OpenRouter: Icon, Fireworks: Icon, Ai360: Icon,
    Doubao: Icon, Wenxin: Icon, Meta: Icon, Coze: Icon, Cerebras: Icon, Kimi: Icon,
    NewAPI: Icon, ZAI: Icon, ModelScope: Icon,
  }
})

function response(data: unknown) {
  return Response.json({ success: true, data })
}

function mockFetchResponse(body: unknown, status = 200): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: async () => body,
  } as Response
}

function embedConfig(timeWindow = '24h') {
  return Response.json({
    success: true,
    data: ['model-a'],
    max_batch: 50,
    time_window: timeWindow,
    refresh_interval: 0,
    theme: 'daylight',
    custom_groups: [],
  })
}

function installEmbedFoundation(timeWindow = '24h') {
  server.use(
    http.get('*/api/model-status/embed/config/selected', () => embedConfig(timeWindow)),
    http.get('*/api/model-status/embed/token-groups', () => response([])),
  )
}

function modelStatus(modelName: string) {
  return {
    model_name: modelName,
    display_name: modelName,
    time_window: '24h',
    total_requests: 1,
    success_count: 1,
    failure_count: 0,
    success_rate: 100,
    current_status: 'green',
    traffic_health: 'healthy',
    source_state: 'fresh',
    slot_data: [],
  }
}

describe('ModelStatusEmbed request states', () => {
  it.each([
    ['selected model list', { data: {}, custom_groups: [] }],
    ['custom groups', { data: ['model-a'], custom_groups: [{ id: 'broken-group' }] }],
  ])('fails closed on a malformed successful %s payload', async (_label, malformedConfig) => {
    vi.spyOn(console, 'error').mockImplementation(() => undefined)
    let statusRequests = 0
    server.use(
      http.get('*/api/model-status/embed/config/selected', () => Response.json({
        success: true,
        max_batch: 50,
        time_window: '24h',
        refresh_interval: 0,
        theme: 'daylight',
        ...malformedConfig,
      })),
      http.get('*/api/model-status/embed/token-groups', () => response([])),
      http.post('*/api/model-status/embed/status/batch', () => {
        statusRequests += 1
        return response([])
      }),
    )

    render(<ModelStatusEmbed refreshInterval={0} />)

    expect(await screen.findByText('模型监测配置暂不可用')).toBeVisible()
    expect(screen.queryByText('请在管理界面选择要监控的模型')).not.toBeInTheDocument()
    expect(statusRequests).toBe(0)
  })

  it('treats a config failure as unavailable and never requests status for an unknown scope', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => undefined)
    let statusRequests = 0
    server.use(
      http.get('*/api/model-status/embed/config/selected', () => Response.json({
        success: false,
        message: 'config unavailable',
      }, { status: 503 })),
      http.get('*/api/model-status/embed/token-groups', () => response([])),
      http.post('*/api/model-status/embed/status/batch', () => {
        statusRequests += 1
        return response([])
      }),
    )

    render(<ModelStatusEmbed refreshInterval={0} />)

    expect(await screen.findByText('模型监测配置暂不可用')).toBeVisible()
    expect(screen.queryByText('请在管理界面选择要监控的模型')).not.toBeInTheDocument()
    expect(statusRequests).toBe(0)
  })

  it('shows a source-unavailable state instead of a successful empty state after fetch failure', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => undefined)
    installEmbedFoundation()
    server.use(http.post('*/api/model-status/embed/status/batch', () => Response.json({
      success: false,
      message: 'status backend unavailable',
    }, { status: 503 })))

    render(<ModelStatusEmbed refreshInterval={0} />)

    expect(await screen.findByText('模型状态数据源暂不可用，请稍后重试')).toBeVisible()
    expect(screen.queryByText('暂无模型状态数据')).not.toBeInTheDocument()
  })

  it('clears an active token-group scope when group sync fails and does not reuse it', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => undefined)
    let tokenGroupRequests = 0
    let statusRequests = 0
    vi.spyOn(globalThis, 'fetch').mockImplementation(async (input, init) => {
      const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url
      if (url.includes('/api/model-status/embed/config/selected')) {
        return mockFetchResponse({
          success: true,
          data: ['seed-model'],
          max_batch: 50,
          time_window: '24h',
          refresh_interval: 0,
          theme: 'daylight',
          custom_groups: [],
        })
      }
      if (url.includes('/api/model-status/embed/token-groups')) {
        tokenGroupRequests += 1
        if (tokenGroupRequests > 1) {
          return mockFetchResponse({ success: false, message: 'token groups unavailable' }, 503)
        }
        return mockFetchResponse({
          success: true,
          data: [{ group_name: 'group-a', model_count: 1, models: ['group-model'] }],
        })
      }
      if (url.includes('/api/model-status/embed/status/batch')) {
        statusRequests += 1
        const models = JSON.parse(String(init?.body)) as string[]
        return mockFetchResponse({ success: true, data: models.map(modelStatus) })
      }
      throw new Error(`Unexpected fetch in test: ${url}`)
    })

    const actor = userEvent.setup()
    render(<ModelStatusEmbed refreshInterval={0} />)
    expect(await screen.findByText('seed-model')).toBeVisible()

    await actor.click(screen.getByRole('button', { name: '密钥分组筛选：1 个分组' }))
    await actor.click(await screen.findByRole('option', { name: /group-a/ }))

    expect(await screen.findByText('group-model')).toBeVisible()
    expect(screen.queryByText('seed-model')).not.toBeInTheDocument()
    expect(screen.getByText(/滑动窗口/)).toHaveTextContent('1 个模型')
    const requestsBeforeFailedSync = statusRequests

    await actor.click(screen.getByRole('button', { name: '立即刷新模型状态和密钥分组' }))

    expect(await screen.findByText('密钥分组范围暂不可用')).toBeVisible()
    expect(screen.queryByText('group-model')).not.toBeInTheDocument()
    await waitFor(() => expect(statusRequests).toBe(requestsBeforeFailedSync))
  })

  it('preserves the empty-data message after a successful empty response', async () => {
    installEmbedFoundation()
    server.use(http.post('*/api/model-status/embed/status/batch', () => response([])))

    render(<ModelStatusEmbed refreshInterval={0} />)

    expect(await screen.findByText('暂无模型状态数据')).toBeVisible()
    expect(screen.queryByText('模型状态数据源暂不可用，请稍后重试')).not.toBeInTheDocument()
  })

  it('shows the loading state while switching from the default to the configured window', async () => {
    let releaseConfig!: () => void
    const configPending = new Promise<void>(resolve => { releaseConfig = resolve })
    let releaseStatus!: () => void
    const statusPending = new Promise<void>(resolve => { releaseStatus = resolve })
    let markStatusStarted!: (url: string) => void
    const statusStarted = new Promise<string>(resolve => { markStatusStarted = resolve })

    server.use(
      http.get('*/api/model-status/embed/config/selected', async () => {
        await configPending
        return embedConfig('7d')
      }),
      http.get('*/api/model-status/embed/token-groups', () => response([])),
      http.post('*/api/model-status/embed/status/batch', async ({ request }) => {
        markStatusStarted(request.url)
        await statusPending
        return response([])
      }),
    )

    const { container } = render(<ModelStatusEmbed refreshInterval={0} />)
    expect(await screen.findByRole('status', { name: '正在加载模型状态' })).toHaveAttribute('aria-live', 'polite')

    act(() => { releaseConfig() })
    const requestUrl = new URL(await statusStarted)
    expect(requestUrl.searchParams.get('window')).toBe('7d')
    expect(screen.getByRole('status', { name: '正在加载模型状态' })).toBeVisible()
    expect(container.querySelector('.animate-spin')).toBeInTheDocument()
    expect(screen.queryByText('暂无模型状态数据')).not.toBeInTheDocument()

    act(() => { releaseStatus() })
    expect(await screen.findByText('暂无模型状态数据')).toBeVisible()
  })

  it('keeps the shell mounted and hides the previous snapshot while a token-group scope loads', async () => {
    let releaseGroupStatus!: () => void
    const groupStatusPending = new Promise<void>(resolve => { releaseGroupStatus = resolve })
    let markGroupStatusStarted!: () => void
    const groupStatusStarted = new Promise<void>(resolve => { markGroupStatusStarted = resolve })

    server.use(
      http.get('*/api/model-status/embed/config/selected', () => Response.json({
        success: true,
        data: ['seed-model'],
        max_batch: 50,
        time_window: '24h',
        refresh_interval: 0,
        theme: 'daylight',
        custom_groups: [],
      })),
      http.get('*/api/model-status/embed/token-groups', () => response([
        { group_name: 'group-a', model_count: 1, models: ['group-model'] },
      ])),
      http.post('*/api/model-status/embed/status/batch', async ({ request }) => {
        const models = await request.json() as string[]
        if (models.includes('group-model')) {
          markGroupStatusStarted()
          await groupStatusPending
        }
        return response(models.map(modelStatus))
      }),
    )

    const actor = userEvent.setup()
    render(<ModelStatusEmbed refreshInterval={0} />)
    expect(await screen.findByText('seed-model')).toBeVisible()

    await actor.click(screen.getByRole('button', { name: '密钥分组筛选：1 个分组' }))
    await actor.click(await screen.findByRole('option', { name: /group-a/ }))
    await groupStatusStarted

    const activeTrigger = screen.getByRole('button', { name: '密钥分组筛选：group-a' })
    await waitFor(() => expect(activeTrigger).toHaveFocus())
    expect(screen.getByRole('status', { name: '正在加载模型状态' })).toBeVisible()
    expect(screen.queryByText('seed-model')).not.toBeInTheDocument()
    expect(screen.queryByText('暂无模型状态数据')).not.toBeInTheDocument()

    act(() => { releaseGroupStatus() })
    expect(await screen.findByText('group-model')).toBeVisible()
    expect(screen.queryByText('seed-model')).not.toBeInTheDocument()
    expect(screen.getByText(/滑动窗口/)).toHaveTextContent('1 个模型')
  })

  it('restores token-group trigger focus after Escape without selecting an option', async () => {
    server.use(
      http.get('*/api/model-status/embed/config/selected', () => embedConfig()),
      http.get('*/api/model-status/embed/token-groups', () => response([
        { group_name: 'group-a', model_count: 1, models: ['model-a'] },
      ])),
      http.post('*/api/model-status/embed/status/batch', () => response([modelStatus('model-a')])),
    )

    const actor = userEvent.setup()
    render(<ModelStatusEmbed refreshInterval={0} />)
    const trigger = await screen.findByRole('button', { name: '密钥分组筛选：1 个分组' })
    await actor.click(trigger)
    const option = await screen.findByRole('option', { name: /group-a/ })
    option.focus()
    expect(option).toHaveFocus()

    await actor.keyboard('{Escape}')

    await waitFor(() => expect(trigger).toHaveFocus())
    expect(screen.queryByRole('option', { name: /group-a/ })).not.toBeInTheDocument()
  })
})
