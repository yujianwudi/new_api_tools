import { act, render, screen } from '@testing-library/react'
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

describe('ModelStatusEmbed request states', () => {
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
    expect(await screen.findByText('请在管理界面选择要监控的模型')).toBeVisible()

    act(() => { releaseConfig() })
    const requestUrl = new URL(await statusStarted)
    expect(requestUrl.searchParams.get('window')).toBe('7d')
    expect(container.querySelector('.animate-spin')).toBeInTheDocument()
    expect(screen.queryByText('暂无模型状态数据')).not.toBeInTheDocument()

    act(() => { releaseStatus() })
    expect(await screen.findByText('暂无模型状态数据')).toBeVisible()
  })
})
