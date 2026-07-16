import { useState, useEffect, useCallback, useRef, useMemo } from 'react'
import { useAuth } from '../contexts/AuthContext'
import { useToast } from './Toast'
import { cn } from '../lib/utils'
import { mergeFilteredModelOrder } from '../lib/modelStatusOrder'
import {
  chunkModelNames,
  mapWithConcurrency,
  MODEL_STATUS_BATCH_MAX_CONCURRENCY,
  normalizeModelStatusMaxBatch,
} from '../lib/modelStatusBatch'
import { RefreshCw, Loader2, Timer, ChevronDown, Settings2, Check, Clock, Palette, Moon, Sun, Minimize2, Maximize2, Zap, Terminal, Leaf, Droplets, HelpCircle, Copy, X, Command, LayoutGrid, Bot, MessageSquareQuote, Triangle, Sparkles, CreditCard, GitBranch, Gamepad2, Rocket, Brain, ArrowUpDown, GripVertical, Search, Filter, Layers, Plus, Pencil, Trash2, FolderPlus, Tag, KeyRound } from 'lucide-react'
import { DndContext, closestCenter, KeyboardSensor, PointerSensor, useSensor, useSensors, DragEndEvent } from '@dnd-kit/core'
import { SortableContext, sortableKeyboardCoordinates, rectSortingStrategy, useSortable } from '@dnd-kit/sortable'
import { CSS } from '@dnd-kit/utilities'
import { Card, CardContent } from './ui/card'
import { Button } from './ui/button'
import { Badge } from './ui/badge'
import { useClickOutside } from '../hooks/useClickOutside'
import {
  OpenAI, Gemini, DeepSeek, SiliconCloud, Groq, Ollama, Claude, Mistral,
  Minimax, Baichuan, Moonshot, Spark, Qwen, Yi, Hunyuan, Stepfun, ZeroOne,
  Zhipu, ChatGLM, Cohere, Perplexity, Together, OpenRouter, Fireworks,
  Ai360, Doubao, Wenxin, Meta, Coze, Cerebras, Kimi, NewAPI, ZAI, ModelScope
} from '@lobehub/icons'

type ModelHealthStatus = 'green' | 'yellow' | 'red' | 'unknown'

interface SlotStatus {
  slot: number
  start_time: number
  end_time: number
  total_requests: number
  success_count: number
  success_rate: number
  status: ModelHealthStatus
}

interface ModelStatus {
  model_name: string
  display_name: string
  time_window: string
  total_requests: number
  success_count: number
  success_rate: number
  current_status: ModelHealthStatus
  slot_data: SlotStatus[]
}

interface ModelStatusMonitorProps {
  isEmbed?: boolean
}

const STATUS_COLORS = {
  green: 'bg-green-500',
  yellow: 'bg-yellow-500',
  red: 'bg-red-500',
  unknown: 'bg-gray-200 dark:bg-gray-700',
  empty: 'bg-gray-200 dark:bg-gray-700',  // No requests - neutral gray
}

// eslint-disable-next-line @typescript-eslint/no-explicit-any
type IconComponent = React.ComponentType<any>

// Model logo mapping - maps model name patterns to logo components
const MODEL_LOGO_MAP: Record<string, IconComponent> = {
  // OpenAI models
  'gpt': OpenAI,
  'openai': OpenAI,
  'o1': OpenAI,
  'o3': OpenAI,
  'chatgpt': OpenAI,
  'dall-e': OpenAI,
  'whisper': OpenAI,
  'tts': OpenAI,

  // Google models
  'gemini': Gemini,
  'gemma': Gemini,
  'palm': Gemini,
  'bard': Gemini,

  // Anthropic models
  'claude': Claude,
  'anthropic': Claude,

  // DeepSeek models
  'deepseek': DeepSeek,

  // Meta models
  'llama': Meta,
  'meta': Meta,

  // Mistral models
  'mistral': Mistral,
  'mixtral': Mistral,
  'codestral': Mistral,
  'pixtral': Mistral,

  // Chinese models
  'qwen': Qwen,
  'tongyi': Qwen,
  'yi': Yi,
  '01-ai': Yi,
  'baichuan': Baichuan,
  'glm': ChatGLM,
  'chatglm': ChatGLM,
  'zhipu': Zhipu,
  'moonshot': Moonshot,
  'kimi': Kimi,
  'spark': Spark,
  'xunfei': Spark,
  'hunyuan': Hunyuan,
  'tencent': Hunyuan,
  'doubao': Doubao,
  'bytedance': Doubao,
  'wenxin': Wenxin,
  'ernie': Wenxin,
  'baidu': Wenxin,
  'minimax': Minimax,
  'abab': Minimax,
  'stepfun': Stepfun,
  'step': Stepfun,
  'zeroone': ZeroOne,
  '01': ZeroOne,
  '360': Ai360,
  'modelscope': ModelScope,

  // Other providers
  'groq': Groq,
  'ollama': Ollama,
  'cohere': Cohere,
  'command': Cohere,
  'perplexity': Perplexity,
  'pplx': Perplexity,
  'together': Together,
  'openrouter': OpenRouter,
  'fireworks': Fireworks,
  'siliconcloud': SiliconCloud,
  'silicon': SiliconCloud,
  'cerebras': Cerebras,
  'coze': Coze,
  'newapi': NewAPI,
  'zai': ZAI,
}

// Token group from abilities table (system-defined)
interface TokenGroup {
  group_name: string
  model_count: number
  models: string[]
}

interface TokenGroupSyncResult {
  groups: TokenGroup[]
  groupFilter: string
}

function isTokenGroup(value: unknown): value is TokenGroup {
  if (typeof value !== 'object' || value === null) return false
  const group = value as Record<string, unknown>
  return typeof group.group_name === 'string' && group.group_name.trim().length > 0
    && typeof group.model_count === 'number' && Number.isInteger(group.model_count) && group.model_count >= 0
    && Array.isArray(group.models) && group.models.every(model => typeof model === 'string')
}

// Custom user-defined model group
interface CustomModelGroup {
  id: string
  name: string
  icon?: string // key from GROUP_ICON_OPTIONS
  models: string[] // exact model names in this group
}

// 厂商关键字映射：当某个 vendor 分组配置了对应的 icon 时，
// 模型名包含这些关键字（不区分大小写）也会被自动算入该分组。
// 这样新增的模型（如 claude-opus-4-7）无需手动勾选就能出现在 Claude 分组下。
const VENDOR_KEYWORDS: Record<string, string[]> = {
  openai: ['gpt', 'openai', 'o1', 'o3', 'chatgpt', 'dall-e', 'whisper', 'tts'],
  claude: ['claude', 'anthropic'],
  gemini: ['gemini', 'gemma', 'bard'],
  deepseek: ['deepseek'],
  meta: ['llama', 'meta'],
  mistral: ['mistral', 'mixtral', 'codestral', 'pixtral'],
  qwen: ['qwen', 'tongyi'],
  zhipu: ['glm', 'chatglm', 'zhipu'],
  moonshot: ['moonshot', 'kimi'],
  kimi: ['kimi', 'moonshot'],
  doubao: ['doubao', 'bytedance'],
  minimax: ['minimax', 'abab'],
  baichuan: ['baichuan'],
  yi: ['yi-', '01-ai', 'zero-one'],
  spark: ['spark', 'xunfei'],
  hunyuan: ['hunyuan', 'tencent'],
  stepfun: ['stepfun', 'step-'],
  wenxin: ['wenxin', 'ernie', 'baidu'],
  cohere: ['cohere', 'command'],
  perplexity: ['perplexity', 'pplx', 'sonar'],
  groq: ['groq'],
  ollama: ['ollama'],
  together: ['together'],
  openrouter: ['openrouter'],
  siliconcloud: ['siliconcloud', 'silicon'],
  coze: ['coze'],
  cerebras: ['cerebras'],
}

// 判断某个模型是否属于某个自定义分组（精确名 + 厂商关键字模糊匹配）
function modelMatchesGroup(modelName: string, group: CustomModelGroup): boolean {
  if (group.models.includes(modelName)) return true
  if (!group.icon) return false
  const keywords = VENDOR_KEYWORDS[group.icon]
  if (!keywords) return false
  const lower = modelName.toLowerCase()
  return keywords.some(k => lower.includes(k))
}

// Available icons for groups (from @lobehub/icons)
const GROUP_ICON_OPTIONS: { key: string; label: string; component: IconComponent }[] = [
  { key: 'openai', label: 'OpenAI', component: OpenAI },
  { key: 'claude', label: 'Claude', component: Claude },
  { key: 'gemini', label: 'Gemini', component: Gemini },
  { key: 'deepseek', label: 'DeepSeek', component: DeepSeek },
  { key: 'meta', label: 'Meta', component: Meta },
  { key: 'mistral', label: 'Mistral', component: Mistral },
  { key: 'qwen', label: 'Qwen', component: Qwen },
  { key: 'zhipu', label: 'Zhipu', component: Zhipu },
  { key: 'moonshot', label: 'Moonshot', component: Moonshot },
  { key: 'kimi', label: 'Kimi', component: Kimi },
  { key: 'doubao', label: 'Doubao', component: Doubao },
  { key: 'minimax', label: 'Minimax', component: Minimax },
  { key: 'baichuan', label: 'Baichuan', component: Baichuan },
  { key: 'yi', label: 'Yi', component: Yi },
  { key: 'spark', label: 'Spark', component: Spark },
  { key: 'hunyuan', label: 'Hunyuan', component: Hunyuan },
  { key: 'stepfun', label: 'Stepfun', component: Stepfun },
  { key: 'wenxin', label: 'Wenxin', component: Wenxin },
  { key: 'cohere', label: 'Cohere', component: Cohere },
  { key: 'perplexity', label: 'Perplexity', component: Perplexity },
  { key: 'groq', label: 'Groq', component: Groq },
  { key: 'ollama', label: 'Ollama', component: Ollama },
  { key: 'together', label: 'Together', component: Together },
  { key: 'openrouter', label: 'OpenRouter', component: OpenRouter },
  { key: 'siliconcloud', label: 'SiliconCloud', component: SiliconCloud },
  { key: 'coze', label: 'Coze', component: Coze },
  { key: 'cerebras', label: 'Cerebras', component: Cerebras },
]

// Preset group templates for quick creation
const GROUP_PRESETS: { name: string; icon: string; keywords: string[] }[] = [
  { name: 'OpenAI', icon: 'openai', keywords: ['gpt', 'o1', 'o3', 'o4', 'chatgpt', 'openai', 'codex', 'dall-e', 'whisper', 'tts'] },
  { name: 'Claude', icon: 'claude', keywords: ['claude', 'anthropic'] },
  { name: 'Gemini', icon: 'gemini', keywords: ['gemini', 'gemma'] },
  { name: 'DeepSeek', icon: 'deepseek', keywords: ['deepseek'] },
  { name: 'Meta/Llama', icon: 'meta', keywords: ['llama', 'meta'] },
  { name: 'Mistral', icon: 'mistral', keywords: ['mistral', 'mixtral', 'codestral', 'pixtral'] },
  { name: '国产模型', icon: 'qwen', keywords: ['qwen', 'tongyi', 'yi', 'baichuan', 'glm', 'chatglm', 'zhipu', 'moonshot', 'kimi', 'spark', 'xunfei', 'hunyuan', 'tencent', 'doubao', 'bytedance', 'wenxin', 'ernie', 'baidu', 'minimax', 'abab', 'stepfun', 'step', 'zeroone', '360', 'modelscope'] },
]

// Color palette for groups
const GROUP_COLORS = [
  'from-emerald-500/20 to-teal-500/20 border-emerald-500/30 text-emerald-700 dark:text-emerald-400',
  'from-amber-500/20 to-orange-500/20 border-amber-500/30 text-amber-700 dark:text-amber-400',
  'from-blue-500/20 to-indigo-500/20 border-blue-500/30 text-blue-700 dark:text-blue-400',
  'from-cyan-500/20 to-sky-500/20 border-cyan-500/30 text-cyan-700 dark:text-cyan-400',
  'from-violet-500/20 to-purple-500/20 border-violet-500/30 text-violet-700 dark:text-violet-400',
  'from-rose-500/20 to-pink-500/20 border-rose-500/30 text-rose-700 dark:text-rose-400',
  'from-orange-500/20 to-red-500/20 border-orange-500/30 text-orange-700 dark:text-orange-400',
  'from-lime-500/20 to-green-500/20 border-lime-500/30 text-lime-700 dark:text-lime-400',
]

const MODEL_GROUP_KEY = 'model_status_group_filter'
const TOKEN_GROUP_SYNC_EVERY_STATUS_REFRESHES = 5

function tokenGroupModelsForFilter(groups: TokenGroup[], filter: string): string[] {
  if (!filter.startsWith('token:')) return []
  const groupName = filter.slice(6)
  return groups.find(group => group.group_name === groupName)?.models ?? []
}

// Get model logo component based on model name
function getModelLogo(modelName: string): IconComponent | null {
  const lowerName = modelName.toLowerCase()

  // Check each pattern in order of specificity
  for (const [pattern, Logo] of Object.entries(MODEL_LOGO_MAP)) {
    if (lowerName.includes(pattern)) {
      return Logo
    }
  }

  return null
}

// Model Logo component with fallback
interface ModelLogoProps {
  modelName: string
  size?: number
  className?: string
}

function ModelLogo({ modelName, size = 20, className }: ModelLogoProps) {
  const Logo = useMemo(() => getModelLogo(modelName), [modelName])

  if (Logo) {
    return <Logo size={size} className={className} />
  }

  // Fallback to generic AI icon
  return <Brain size={size} className={cn("text-muted-foreground", className)} />
}

const STATUS_LABELS = {
  green: '正常',
  yellow: '警告',
  red: '异常',
  unknown: '暂无数据',
}

// Time window options
const TIME_WINDOWS = [
  { value: '1h', label: '1小时', slots: 60 },
  { value: '6h', label: '6小时', slots: 24 },
  { value: '12h', label: '12小时', slots: 24 },
  { value: '24h', label: '24小时', slots: 24 },
]

// Theme options
const THEMES = [
  { id: 'daylight', name: '日光', nameEn: 'Daylight', icon: Sun, description: '明亮清新的浅色', preview: 'bg-slate-100' },
  { id: 'obsidian', name: '黑曜石', nameEn: 'Obsidian', icon: Moon, description: '经典深色，专业稳重', preview: 'bg-[#0d1117]' },
  { id: 'minimal', name: '极简', nameEn: 'Minimal', icon: Minimize2, description: '极度精简，适合嵌入', preview: 'bg-white' },
  { id: 'neon', name: '霓虹', nameEn: 'Neon', icon: Zap, description: '赛博朋克，科技感', preview: 'bg-black' },
  { id: 'forest', name: '森林', nameEn: 'Forest', icon: Leaf, description: '深邃自然的森林色调', preview: 'bg-[#022c22]' },
  { id: 'ocean', name: '海洋', nameEn: 'Ocean', icon: Droplets, description: '宁静深邃的海洋蓝', preview: 'bg-[#0b1121]' },
  { id: 'terminal', name: '终端', nameEn: 'Terminal', icon: Terminal, description: '复古极客风格', preview: 'bg-black border border-green-500' },
  { id: 'cupertino', name: 'Apple', nameEn: 'Apple', icon: Command, description: '致敬 Apple 设计风格', preview: 'bg-[#f5f5f7]' },
  { id: 'material', name: 'Google', nameEn: 'Google', icon: LayoutGrid, description: '致敬 Google Material', preview: 'bg-[#f0f4f8]' },
  { id: 'openai', name: 'OpenAI', nameEn: 'OpenAI', icon: Bot, description: '致敬 OpenAI 设计风格', preview: 'bg-[#343541]' },
  { id: 'anthropic', name: 'Claude', nameEn: 'Claude', icon: MessageSquareQuote, description: '致敬 Claude 设计风格', preview: 'bg-[#f4f1ea]' },
  { id: 'vercel', name: 'Vercel', nameEn: 'Vercel', icon: Triangle, description: 'Geist 风格，极致黑白', preview: 'bg-black radial-gradient(#333 1px, transparent 1px)' },
  { id: 'linear', name: 'Linear', nameEn: 'Linear', icon: Sparkles, description: '流光风格，深色质感', preview: 'bg-[#0f1015]' },
  { id: 'stripe', name: 'Stripe', nameEn: 'Stripe', icon: CreditCard, description: '现代支付美学', preview: 'bg-white' },
  { id: 'github', name: 'GitHub', nameEn: 'GitHub', icon: GitBranch, description: '开发者之魂', preview: 'bg-[#0d1117]' },
  { id: 'discord', name: 'Discord', nameEn: 'Discord', icon: Gamepad2, description: '游戏社区风格', preview: 'bg-[#313338]' },
  { id: 'tesla', name: 'Tesla', nameEn: 'Tesla', icon: Rocket, description: '工业未来风', preview: 'bg-black' },
]

function formatTime(timestamp: number): string {
  return new Date(timestamp * 1000).toLocaleTimeString('zh-CN', {
    hour: '2-digit',
    minute: '2-digit',
  })
}

function formatDateTime(timestamp: number): string {
  return new Date(timestamp * 1000).toLocaleString('zh-CN', {
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
  })
}

function formatCountdown(seconds: number): string {
  const mins = Math.floor(seconds / 60)
  const secs = seconds % 60
  return mins > 0 ? `${mins}:${secs.toString().padStart(2, '0')}` : `${secs}s`
}

const REFRESH_INTERVALS = [
  { value: 0, label: '关闭' },
  { value: 30, label: '30秒' },
  { value: 60, label: '1分钟' },
  { value: 120, label: '2分钟' },
  { value: 300, label: '5分钟' },
]

// Model with stats interface
interface ModelWithStats {
  model_name: string
  request_count_24h: number
}

// Storage keys
const SELECTED_MODELS_KEY = 'model_status_selected_models'
const REFRESH_INTERVAL_KEY = 'model_status_refresh_interval'
const TIME_WINDOW_KEY = 'model_status_time_window'
const THEME_KEY = 'model_status_theme'
const SORT_MODE_KEY = 'model_status_sort_mode'
const CUSTOM_ORDER_KEY = 'model_status_custom_order'
// Note: MODEL_GROUP_KEY is defined alongside MODEL_GROUPS above

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null
}

function apiErrorMessage(payload: unknown, status: number): string {
  if (isRecord(payload)) {
    if (isRecord(payload.error) && typeof payload.error.message === 'string') {
      return payload.error.message
    }
    if (typeof payload.error === 'string') return payload.error
    if (typeof payload.message === 'string') return payload.message
  }
  return `HTTP ${status}`
}

async function requireSuccessfulResponse(response: Response): Promise<Record<string, unknown>> {
  let payload: unknown
  try {
    payload = await response.json()
  } catch {
    throw new Error(`Invalid JSON response (HTTP ${response.status})`)
  }
  if (!response.ok || !isRecord(payload) || payload.success !== true) {
    throw new Error(apiErrorMessage(payload, response.status))
  }
  return payload
}

function isAbortError(error: unknown): boolean {
  return error instanceof DOMException && error.name === 'AbortError'
}

function getLocalStorage(): Storage | null {
  try {
    return typeof window === 'undefined' ? null : window.localStorage
  } catch {
    return null
  }
}

function readStoredValue(key: string): string | null {
  try {
    return getLocalStorage()?.getItem(key) ?? null
  } catch {
    return null
  }
}

function writeStoredValue(key: string, value: string): void {
  try {
    getLocalStorage()?.setItem(key, value)
  } catch {
    // Backend persistence remains authoritative when browser storage is blocked.
  }
}

function readStoredStringArray(key: string): string[] | null {
  const stored = readStoredValue(key)
  if (stored === null) return null
  try {
    const parsed: unknown = JSON.parse(stored)
    return Array.isArray(parsed) && parsed.every(value => typeof value === 'string') ? parsed : null
  } catch {
    return null
  }
}

function sameStringArray(left: string[], right: string[]): boolean {
  return left.length === right.length && left.every((value, index) => value === right[index])
}

// Sort mode type
type SortMode = 'default' | 'availability' | 'custom'

// Status filter type
type StatusFilter = 'all' | ModelHealthStatus
const AUTHENTICATED_MODEL_STATUS_DEFAULT_MAX_BATCH = 200

export function ModelStatusMonitor({ isEmbed = false }: ModelStatusMonitorProps) {
  const { token } = useAuth()
  const { showToast } = useToast()

  const [availableModels, setAvailableModels] = useState<ModelWithStats[]>([])
  const [selectedModels, setSelectedModels] = useState<string[]>([])
  const [maxBatch, setMaxBatch] = useState(AUTHENTICATED_MODEL_STATUS_DEFAULT_MAX_BATCH)
  const [modelStatuses, setModelStatuses] = useState<ModelStatus[]>([])
  const [loading, setLoading] = useState(true)
  const [initialLoading, setInitialLoading] = useState(true)
  const [refreshing, setRefreshing] = useState(false)

  const [timeWindow, setTimeWindow] = useState(() => {
    const saved = readStoredValue(TIME_WINDOW_KEY)
    return saved || '24h'
  })

  const [theme, setTheme] = useState(() => {
    const saved = readStoredValue(THEME_KEY)
    // Validate saved theme exists, fallback for legacy values (light/dark/system)
    if (saved && THEMES.find(t => t.id === saved)) return saved
    return 'daylight'
  })

  const [refreshInterval, setRefreshInterval] = useState(() => {
    const saved = readStoredValue(REFRESH_INTERVAL_KEY)
    return saved ? parseInt(saved, 10) : 60
  })
  const [countdown, setCountdown] = useState(refreshInterval)
  const refreshIntervalRef = useRef(refreshInterval)

  const [sortMode, setSortMode] = useState<SortMode>(() => {
    const saved = readStoredValue(SORT_MODE_KEY)
    return saved === 'availability' || saved === 'custom' ? saved : 'default'
  })
  const [customOrder, setCustomOrder] = useState<string[]>(() => {
    return readStoredStringArray(CUSTOM_ORDER_KEY) ?? []
  })
  const [sortReconciliationRequired, setSortReconciliationRequired] = useState(false)

  const [showModelSelector, setShowModelSelector] = useState(false)
  const [showIntervalDropdown, setShowIntervalDropdown] = useState(false)
  const [showWindowDropdown, setShowWindowDropdown] = useState(false)
  const [showThemeDropdown, setShowThemeDropdown] = useState(false)
  const [showEmbedHelp, setShowEmbedHelp] = useState(false)
  const [modelSearchQuery, setModelSearchQuery] = useState('')
  const [statusFilter, setStatusFilter] = useState<StatusFilter>('all')
  const [groupFilter, setGroupFilter] = useState(() => {
    const saved = readStoredValue(MODEL_GROUP_KEY)
    return saved || 'all'
  })
  const [customGroups, setCustomGroups] = useState<CustomModelGroup[]>([])
  const [tokenGroups, setTokenGroups] = useState<TokenGroup[]>([])
  const [showGroupManager, setShowGroupManager] = useState(false)
  const [siteTitle, setSiteTitle] = useState('')
  const [showSiteTitleInput, setShowSiteTitleInput] = useState(false)
  const [isFullscreen, setIsFullscreen] = useState(false)
  const modelSelectorRef = useRef<HTMLDivElement>(null)
  const intervalDropdownRef = useRef<HTMLDivElement>(null)
  const windowDropdownRef = useRef<HTMLDivElement>(null)
  const themeDropdownRef = useRef<HTMLDivElement>(null)
  const selectedModelsRef = useRef(selectedModels)
  const persistedSelectedModelsRef = useRef(selectedModels)
  const selectedModelsSaveQueueRef = useRef<Promise<void>>(Promise.resolve())
  const sortSaveQueueRef = useRef<Promise<void>>(Promise.resolve())
  const configSaveQueuesRef = useRef(new Map<string, Promise<void>>())
  const configMutationIdsRef = useRef({ timeWindow: 0, theme: 0, refreshInterval: 0, sort: 0, siteTitle: 0 })
  const statusRequestIdRef = useRef(0)
  const statusRequestControllerRef = useRef<AbortController | null>(null)
  const tokenGroupRequestIdRef = useRef(0)
  const tokenGroupRequestControllerRef = useRef<AbortController | null>(null)
  const groupFilterRef = useRef(groupFilter)
  const automaticStatusRefreshCountRef = useRef(0)
  const persistedTimeWindowRef = useRef(timeWindow)
  const persistedThemeRef = useRef(theme)
  const persistedRefreshIntervalRef = useRef(refreshInterval)
  const persistedSortModeRef = useRef(sortMode)
  const persistedCustomOrderRef = useRef(customOrder)
  const persistedSiteTitleRef = useRef('')

  const apiUrl = import.meta.env.VITE_API_URL || ''
  const activeTokenGroupModels = useMemo(() => {
    return tokenGroupModelsForFilter(tokenGroups, groupFilter)
  }, [groupFilter, tokenGroups])

  const getAuthHeaders = useCallback((): Record<string, string> => {
    if (isEmbed) {
      return { 'Content-Type': 'application/json' }
    }
    return {
      'Content-Type': 'application/json',
      'Authorization': `Bearer ${token}`,
    }
  }, [token, isEmbed])

  const getApiPrefix = useCallback(() => {
    return isEmbed ? '/api/model-status/embed' : '/api/model-status'
  }, [isEmbed])

  // Click outside handlers
  useClickOutside(modelSelectorRef, () => setShowModelSelector(false), showModelSelector)
  useClickOutside(intervalDropdownRef, () => setShowIntervalDropdown(false), showIntervalDropdown)
  useClickOutside(windowDropdownRef, () => setShowWindowDropdown(false), showWindowDropdown)
  useClickOutside(themeDropdownRef, () => setShowThemeDropdown(false), showThemeDropdown)

  useEffect(() => {
    selectedModelsRef.current = selectedModels
  }, [selectedModels])

  useEffect(() => {
    groupFilterRef.current = groupFilter
  }, [groupFilter])

  useEffect(() => () => {
    statusRequestIdRef.current += 1
    statusRequestControllerRef.current?.abort()
    tokenGroupRequestIdRef.current += 1
    tokenGroupRequestControllerRef.current?.abort()
  }, [])

  // Fullscreen change listener
  useEffect(() => {
    const handleFullscreenChange = () => {
      setIsFullscreen(!!document.fullscreenElement)
    }
    document.addEventListener('fullscreenchange', handleFullscreenChange)
    return () => document.removeEventListener('fullscreenchange', handleFullscreenChange)
  }, [])

  // Toggle fullscreen mode
  const toggleFullscreen = useCallback(() => {
    if (!document.fullscreenElement) {
      document.documentElement.requestFullscreen().catch(console.error)
    } else {
      document.exitFullscreen().catch(console.error)
    }
  }, [])

  const saveConfigMutation = useCallback((
    path: string,
    body: Record<string, unknown>,
    label: string,
    method: 'POST' | 'PUT' = 'POST',
  ): Promise<boolean> => {
    const previousSave = configSaveQueuesRef.current.get(path) ?? Promise.resolve()
    const saveAttempt = previousSave.then(async () => {
      try {
        const response = await fetch(`${apiUrl}${path}`, {
          method,
          headers: getAuthHeaders(),
          body: JSON.stringify(body),
        })
        await requireSuccessfulResponse(response)
        return true
      } catch (error) {
        console.error(`Failed to save ${label}:`, error)
        showToast('error', `${label}保存失败，已保留有效设置`)
        return false
      }
    })
    configSaveQueuesRef.current.set(path, saveAttempt.then(() => undefined))
    return saveAttempt
  }, [apiUrl, getAuthHeaders, showToast])

  // Save time window to backend cache
  const saveTimeWindowToBackend = useCallback(async (window: string): Promise<boolean> => {
    const saved = await saveConfigMutation(
      '/api/model-status/config/window',
      { time_window: window },
      '时间窗口',
    )
    if (saved) {
      persistedTimeWindowRef.current = window
      writeStoredValue(TIME_WINDOW_KEY, window)
    }
    return saved
  }, [saveConfigMutation])

  // Save theme to backend cache
  const saveThemeToBackend = useCallback(async (newTheme: string): Promise<boolean> => {
    const saved = await saveConfigMutation(
      '/api/model-status/config/theme',
      { theme: newTheme },
      '主题',
    )
    if (saved) {
      persistedThemeRef.current = newTheme
      writeStoredValue(THEME_KEY, newTheme)
      showToast('success', `主题已切换为 ${THEMES.find(t => t.id === newTheme)?.name || newTheme}`)
    }
    return saved
  }, [saveConfigMutation, showToast])

  // Save refresh interval to backend cache
  const saveRefreshIntervalToBackend = useCallback(async (interval: number): Promise<boolean> => {
    const saved = await saveConfigMutation(
      '/api/model-status/config/refresh',
      { refresh_interval: interval },
      '刷新间隔',
    )
    if (saved) {
      persistedRefreshIntervalRef.current = interval
      writeStoredValue(REFRESH_INTERVAL_KEY, interval.toString())
    }
    return saved
  }, [saveConfigMutation])

  // Save selected models to backend cache
  const saveSelectedModelsToBackend = useCallback((models: string[]): Promise<boolean> => {
    const saveAttempt = selectedModelsSaveQueueRef.current.then(async () => {
      const saved = await saveConfigMutation(
        '/api/model-status/config/selected',
        { models },
        '模型选择',
      )
      if (saved) {
        persistedSelectedModelsRef.current = [...models]
        writeStoredValue(SELECTED_MODELS_KEY, JSON.stringify(models))
      }
      return saved
    })
    selectedModelsSaveQueueRef.current = saveAttempt.then(() => undefined)
    return saveAttempt
  }, [saveConfigMutation])

  const reloadAuthoritativeSortConfig = useCallback(async (mutationId: number): Promise<boolean> => {
    try {
      const response = await fetch(`${apiUrl}/api/model-status/config/selected`, {
        headers: getAuthHeaders(),
      })
      const data = await requireSuccessfulResponse(response)
      const mode = data.sort_mode
      if (mode !== 'default' && mode !== 'availability' && mode !== 'custom') {
        throw new Error('Invalid authoritative sort mode')
      }
      if (!Array.isArray(data.custom_order) || !data.custom_order.every(value => typeof value === 'string')) {
        throw new Error('Invalid authoritative custom order')
      }

      const authoritativeOrder = [...data.custom_order] as string[]
      persistedSortModeRef.current = mode
      persistedCustomOrderRef.current = authoritativeOrder
      writeStoredValue(SORT_MODE_KEY, mode)
      writeStoredValue(CUSTOM_ORDER_KEY, JSON.stringify(authoritativeOrder))
      if (mutationId === configMutationIdsRef.current.sort) {
        setSortMode(mode)
        setCustomOrder(authoritativeOrder)
        setSortReconciliationRequired(false)
      }
      return true
    } catch (error) {
      console.error('Failed to reload authoritative sort configuration:', error)
      if (mutationId === configMutationIdsRef.current.sort) {
        setSortReconciliationRequired(true)
      }
      showToast('error', '排序回滚结果不确定，无法读取服务端配置；请刷新页面后再排序')
      return false
    }
  }, [apiUrl, getAuthHeaders, showToast])

  // Save sort config to backend cache
  const saveSortConfigToBackend = useCallback((mode: SortMode, order: string[] | undefined, mutationId: number): Promise<boolean> => {
    const saveAttempt = sortSaveQueueRef.current.then(async () => {
      const previousPersistedOrder = [...persistedCustomOrderRef.current]
      let customOrderChanged = false
      if (order !== undefined) {
        const orderSaved = await saveConfigMutation(
          '/api/model-status/config/custom-order',
          { custom_order: order },
          '自定义排序',
          'PUT',
        )
        if (!orderSaved) {
          await reloadAuthoritativeSortConfig(mutationId)
          return false
        }
        customOrderChanged = true
      }

      const modeSaved = await saveConfigMutation(
        '/api/model-status/config/sort',
        { sort_mode: mode },
        '排序方式',
      )
      if (!modeSaved) {
        if (customOrderChanged) {
          await saveConfigMutation(
            '/api/model-status/config/custom-order',
            { custom_order: previousPersistedOrder },
            '自定义排序回滚',
            'PUT',
          )
        }
        await reloadAuthoritativeSortConfig(mutationId)
        return false
      }

      persistedSortModeRef.current = mode
      if (order !== undefined) persistedCustomOrderRef.current = [...order]
      writeStoredValue(SORT_MODE_KEY, mode)
      if (order !== undefined) writeStoredValue(CUSTOM_ORDER_KEY, JSON.stringify(order))
      return true
    })
    sortSaveQueueRef.current = saveAttempt.then(() => undefined)
    return saveAttempt
  }, [reloadAuthoritativeSortConfig, saveConfigMutation])

  // Load config from backend on mount
  const loadConfigFromBackend = useCallback(async (signal?: AbortSignal): Promise<string[] | null> => {
    try {
      const response = await fetch(`${apiUrl}/api/model-status/config/selected`, {
        headers: getAuthHeaders(),
        signal,
      })
      const data = await requireSuccessfulResponse(response)
      setMaxBatch(normalizeModelStatusMaxBatch(data.max_batch, AUTHENTICATED_MODEL_STATUS_DEFAULT_MAX_BATCH))

      let selectedConfig: string[] | null = null
      if (Array.isArray(data.data) && data.data.every(value => typeof value === 'string')) {
        selectedConfig = [...data.data]
        selectedModelsRef.current = selectedConfig
        persistedSelectedModelsRef.current = selectedConfig
        setSelectedModels(selectedConfig)
        writeStoredValue(SELECTED_MODELS_KEY, JSON.stringify(selectedConfig))
      }
      if (typeof data.time_window === 'string' && TIME_WINDOWS.some(item => item.value === data.time_window)) {
        persistedTimeWindowRef.current = data.time_window
        setTimeWindow(data.time_window)
        writeStoredValue(TIME_WINDOW_KEY, data.time_window)
      }
      if (typeof data.theme === 'string') {
        // Validate theme exists, fallback to daylight for legacy values (light/dark/system)
        const validTheme = THEMES.find(t => t.id === data.theme) ? data.theme : 'daylight'
        persistedThemeRef.current = validTheme
        setTheme(validTheme)
        writeStoredValue(THEME_KEY, validTheme)
      }
      if (typeof data.refresh_interval === 'number') {
        persistedRefreshIntervalRef.current = data.refresh_interval
        setRefreshInterval(data.refresh_interval)
        setCountdown(data.refresh_interval)
        writeStoredValue(REFRESH_INTERVAL_KEY, data.refresh_interval.toString())
      }
      if (data.sort_mode === 'default' || data.sort_mode === 'availability' || data.sort_mode === 'custom') {
        persistedSortModeRef.current = data.sort_mode
        setSortMode(data.sort_mode)
        writeStoredValue(SORT_MODE_KEY, data.sort_mode)
      }
      if (Array.isArray(data.custom_order) && data.custom_order.every(value => typeof value === 'string')) {
        const order = [...data.custom_order]
        persistedCustomOrderRef.current = order
        setCustomOrder(order)
        writeStoredValue(CUSTOM_ORDER_KEY, JSON.stringify(order))
      }
      // Load custom groups from backend
      if (Array.isArray(data.custom_groups)) {
        setCustomGroups(data.custom_groups as CustomModelGroup[])
      }
      // Load site title
      if (typeof data.site_title === 'string') {
        persistedSiteTitleRef.current = data.site_title
        setSiteTitle(data.site_title)
      }
      if (selectedConfig !== null) return selectedConfig
    } catch (error) {
      if (signal?.aborted || isAbortError(error)) return null
      console.error('Failed to load config from backend:', error)
    }
    // Fallback to localStorage
    const savedModels = readStoredStringArray(SELECTED_MODELS_KEY)
    if (savedModels !== null) {
      selectedModelsRef.current = savedModels
      persistedSelectedModelsRef.current = savedModels
      setSelectedModels(savedModels)
      return savedModels
    }
    return null
  }, [apiUrl, getAuthHeaders])

  // 加载令牌分组列表
  const fetchTokenGroups = useCallback(async (): Promise<TokenGroupSyncResult | null | undefined> => {
    const requestId = ++tokenGroupRequestIdRef.current
    tokenGroupRequestControllerRef.current?.abort()
    const controller = new AbortController()
    tokenGroupRequestControllerRef.current = controller

    try {
      const response = await fetch(`${apiUrl}${getApiPrefix()}/token-groups`, {
        headers: getAuthHeaders(),
        signal: controller.signal,
      })
      const data = await requireSuccessfulResponse(response)
      if (!Array.isArray(data.data) || !data.data.every(isTokenGroup)) {
        throw new Error('Invalid token group response')
      }

      if (requestId !== tokenGroupRequestIdRef.current) return undefined
      const groups = data.data as TokenGroup[]
      setTokenGroups(groups)

      let nextGroupFilter = groupFilterRef.current
      if (nextGroupFilter.startsWith('token:') && !groups.some(group => `token:${group.group_name}` === nextGroupFilter)) {
        nextGroupFilter = 'all'
        groupFilterRef.current = nextGroupFilter
        setGroupFilter(nextGroupFilter)
        writeStoredValue(MODEL_GROUP_KEY, nextGroupFilter)
      }

      return { groups, groupFilter: nextGroupFilter }
    } catch (error) {
      if (controller.signal.aborted || requestId !== tokenGroupRequestIdRef.current || isAbortError(error)) {
        return undefined
      }
      console.error('Failed to fetch token groups:', error)
      return null
    } finally {
      if (requestId === tokenGroupRequestIdRef.current && tokenGroupRequestControllerRef.current === controller) {
        tokenGroupRequestControllerRef.current = null
      }
    }
  }, [apiUrl, getApiPrefix, getAuthHeaders])

  // Update refresh interval ref
  useEffect(() => {
    refreshIntervalRef.current = refreshInterval
  }, [refreshInterval])

  // Fetch available models and load config
  const fetchAvailableModels = useCallback(async (signal?: AbortSignal) => {
    try {
      const response = await fetch(`${apiUrl}${getApiPrefix()}/models`, {
        headers: getAuthHeaders(),
        signal,
      })
      const data = await requireSuccessfulResponse(response)
      // data.data is now an array of { model_name, request_count_24h }
      // 后端无数据时可能返回 null（Go nil 切片 → JSON null），统一兜底成数组，避免渲染崩溃
      const models: ModelWithStats[] = Array.isArray(data.data) ? data.data as ModelWithStats[] : []
      setAvailableModels(models)
      // Load config from backend. null means no usable config; [] is an explicit empty selection.
      const savedModels = await loadConfigFromBackend(signal)
      if (signal?.aborted) return
      if ((savedModels !== null && savedModels.length === 0) || (savedModels === null && models.length === 0)) {
        setModelStatuses([])
        setLoading(false)
        setInitialLoading(false)
      }
      // Auto-select active models only when neither backend nor browser storage has a selection.
      if (savedModels === null && models.length > 0) {
        const activeModels = models
          .filter((m: ModelWithStats) => m.request_count_24h > 0)
          .map((m: ModelWithStats) => m.model_name)
        const defaultModels = activeModels.length > 0
          ? activeModels
          : models.slice(0, 5).map((m: ModelWithStats) => m.model_name)
        selectedModelsRef.current = defaultModels
        setSelectedModels(defaultModels)
        const saved = await saveSelectedModelsToBackend(defaultModels)
        if (!saved && sameStringArray(selectedModelsRef.current, defaultModels)) {
          const persistedModels = persistedSelectedModelsRef.current
          selectedModelsRef.current = persistedModels
          setSelectedModels(persistedModels)
        }
      }
      // 同时加载令牌分组
      await fetchTokenGroups()
    } catch (error) {
      if (signal?.aborted || isAbortError(error)) return
      console.error('Failed to fetch available models:', error)
    }
  }, [apiUrl, getApiPrefix, getAuthHeaders, loadConfigFromBackend, saveSelectedModelsToBackend, fetchTokenGroups])

  // Fetch model statuses
  // forceRefresh: bypass cache to get fresh data (used for manual refresh)
  const fetchModelStatuses = useCallback(async (forceRefresh = false, tokenGroupModelsOverride?: readonly string[]) => {
    // 计算实际要请求状态的模型集合：
    //   - 用户手工选中的 selectedModels（基础）
    //   - 当前过滤器若为某个密钥分组（token:X），把该分组下的全部模型并入
    // 这样选中密钥分组时，分组下所有模型会自动出现在监控视图中，无需手动勾选。
    const tokenGroupModels = tokenGroupModelsOverride ?? activeTokenGroupModels
    const fetchSet = Array.from(new Set([...selectedModels, ...tokenGroupModels]))
    const requestId = ++statusRequestIdRef.current
    statusRequestControllerRef.current?.abort()

    if (fetchSet.length === 0) {
      statusRequestControllerRef.current = null
      setModelStatuses([])
      setLoading(false)
      setRefreshing(false)
      // Only clear initialLoading when we know models have been loaded
      if (availableModels.length > 0) {
        setInitialLoading(false)
      }
      return
    }

    const controller = new AbortController()
    statusRequestControllerRef.current = controller
    if (forceRefresh) {
      setRefreshing(true)
    }

    try {
      // Add no_cache=true when force refreshing to bypass backend cache
      const cacheParam = forceRefresh ? '&no_cache=true' : ''
      const chunkResults = await mapWithConcurrency(
        chunkModelNames(fetchSet, maxBatch),
        MODEL_STATUS_BATCH_MAX_CONCURRENCY,
        async modelNames => {
          const response = await fetch(`${apiUrl}${getApiPrefix()}/status/batch?window=${timeWindow}${cacheParam}`, {
            method: 'POST',
            headers: getAuthHeaders(),
            body: JSON.stringify(modelNames),
            signal: controller.signal,
          })
          const data = await requireSuccessfulResponse(response)
          if (!Array.isArray(data.data)) throw new Error('Invalid model status batch response')
          return data.data as ModelStatus[]
        },
      )
      if (requestId !== statusRequestIdRef.current) return
      setModelStatuses(chunkResults.flat())
      setInitialLoading(false)
    } catch (error) {
      if (controller.signal.aborted || requestId !== statusRequestIdRef.current || isAbortError(error)) return
      controller.abort()
      console.error('Failed to fetch model statuses:', error)
      if (!isEmbed) {
        showToast('error', '获取模型状态失败')
      }
    } finally {
      if (requestId === statusRequestIdRef.current) {
        statusRequestControllerRef.current = null
        setLoading(false)
        setRefreshing(false)
      }
    }
  }, [apiUrl, getApiPrefix, getAuthHeaders, selectedModels, activeTokenGroupModels, timeWindow, isEmbed, showToast, availableModels.length, maxBatch])

  const refreshStatusesWithLatestTokenGroups = useCallback(async (forceRefresh: boolean) => {
    const tokenGroupSync = await fetchTokenGroups()
    if (tokenGroupSync === undefined) return
    const tokenGroupModels = tokenGroupSync === null
      ? undefined
      : tokenGroupModelsForFilter(tokenGroupSync.groups, tokenGroupSync.groupFilter)
    await fetchModelStatuses(forceRefresh, tokenGroupModels)
  }, [fetchModelStatuses, fetchTokenGroups])

  const runAutomaticRefresh = useCallback(async () => {
    automaticStatusRefreshCountRef.current += 1
    if (automaticStatusRefreshCountRef.current % TOKEN_GROUP_SYNC_EVERY_STATUS_REFRESHES === 0) {
      await refreshStatusesWithLatestTokenGroups(true)
      return
    }
    await fetchModelStatuses(true)
  }, [fetchModelStatuses, refreshStatusesWithLatestTokenGroups])

  // Initial load
  useEffect(() => {
    const controller = new AbortController()
    void fetchAvailableModels(controller.signal)
    return () => controller.abort()
  }, [fetchAvailableModels])

  // Track if models/window changed (not initial load)
  const isInitialMount = useRef(true)
  const prevSelectedModels = useRef<string[]>([])
  const prevTimeWindow = useRef<string>(timeWindow)
  const prevGroupFilter = useRef<string>(groupFilter)
  const prevActiveTokenGroupModels = useRef<string[]>(activeTokenGroupModels)

  // Handle model selection and time window changes
  useEffect(() => {
    if (isInitialMount.current) {
      // Initial load - use cache for fast loading
      isInitialMount.current = false
      prevSelectedModels.current = selectedModels
      prevTimeWindow.current = timeWindow
      prevGroupFilter.current = groupFilter
      prevActiveTokenGroupModels.current = activeTokenGroupModels
      fetchModelStatuses(false)  // Use cache on initial load
      return
    }

    // Check what changed
    const modelsChanged =
      selectedModels.length !== prevSelectedModels.current.length ||
      selectedModels.some(m => !prevSelectedModels.current.includes(m))
    const windowChanged = timeWindow !== prevTimeWindow.current
    // 切到/切出某个密钥分组时也要重新拉取，因为请求集合包含分组成员
    const groupFilterChanged = groupFilter !== prevGroupFilter.current
    const tokenGroupSwitched =
      groupFilterChanged && (groupFilter.startsWith('token:') || prevGroupFilter.current.startsWith('token:'))
    const tokenGroupModelsChanged = !sameStringArray(activeTokenGroupModels, prevActiveTokenGroupModels.current)

    // Update refs
    prevSelectedModels.current = selectedModels
    prevTimeWindow.current = timeWindow
    prevGroupFilter.current = groupFilter
    prevActiveTokenGroupModels.current = activeTokenGroupModels

    if (modelsChanged || tokenGroupSwitched || tokenGroupModelsChanged) {
      // Models selection changed - fetch fresh data for new models
      fetchModelStatuses(true)
    } else if (windowChanged) {
      fetchModelStatuses(false)
    }
  }, [selectedModels, timeWindow, groupFilter, activeTokenGroupModels, fetchModelStatuses])

  // Auto refresh countdown
  useEffect(() => {
    if (refreshInterval === 0) return

    const timer = setInterval(() => {
      setCountdown(prev => {
        if (prev <= 1) {
          // Auto refresh should also get fresh data
          void runAutomaticRefresh()
          return refreshIntervalRef.current
        }
        return prev - 1
      })
    }, 1000)

    return () => clearInterval(timer)
  }, [refreshInterval, runAutomaticRefresh])

  // Reset countdown when interval changes
  useEffect(() => {
    setCountdown(refreshInterval)
  }, [refreshInterval])

  const handleRefresh = () => {
    setCountdown(refreshIntervalRef.current)
    void refreshStatusesWithLatestTokenGroups(true)
  }

  // DnD sensors
  const sensors = useSensors(
    useSensor(PointerSensor, {
      activationConstraint: {
        distance: 8,
      },
    }),
    useSensor(KeyboardSensor, {
      coordinateGetter: sortableKeyboardCoordinates,
    })
  )

  const visibleModelStatuses = useMemo(
    () => modelStatuses.filter(m => m.total_requests > 0 || m.current_status === 'unknown'),
    [modelStatuses]
  )

  const groupManagerModels = useMemo(() => Array.from(new Set([
    ...availableModels.map(model => model.model_name),
    ...tokenGroups.flatMap(group => group.models),
    ...selectedModels,
    ...customGroups.flatMap(group => group.models),
    ...modelStatuses.map(model => model.model_name),
  ])), [availableModels, tokenGroups, selectedModels, customGroups, modelStatuses])

  // Status counts for overview
  const statusCounts = useMemo(() => {
    const counts = { green: 0, yellow: 0, red: 0, unknown: 0 }
    visibleModelStatuses.forEach(m => {
      counts[m.current_status]++
    })
    return counts
  }, [visibleModelStatuses])

  // Group counts for custom group filter tabs
  const groupCounts = useMemo(() => {
    const counts: Record<string, number> = { all: 0 }
    customGroups.forEach(g => { counts[g.id] = 0 })
    tokenGroups.forEach(g => { counts[`token:${g.group_name}`] = 0 })
    counts.all = visibleModelStatuses.length
    visibleModelStatuses.forEach(m => {
      customGroups.forEach(g => {
        if (modelMatchesGroup(m.model_name, g)) {
          counts[g.id] = (counts[g.id] || 0) + 1
        }
      })
      tokenGroups.forEach(g => {
        if (g.models.includes(m.model_name)) {
          counts[`token:${g.group_name}`] = (counts[`token:${g.group_name}`] || 0) + 1
        }
      })
    })
    return counts
  }, [visibleModelStatuses, customGroups, tokenGroups])

  // Average success rate
  const avgSuccessRate = useMemo(() => {
    const measured = visibleModelStatuses.filter(m => m.total_requests > 0 && m.current_status !== 'unknown')
    if (measured.length === 0) return null
    return +(measured.reduce((sum, m) => sum + m.success_rate, 0) / measured.length).toFixed(1)
  }, [visibleModelStatuses])

  // Handle group filter change
  const handleGroupFilterChange = useCallback((gid: string) => {
    groupFilterRef.current = gid
    setGroupFilter(gid)
    writeStoredValue(MODEL_GROUP_KEY, gid)
  }, [])

  // Save custom groups to backend
  const saveCustomGroups = useCallback(async (groups: CustomModelGroup[]): Promise<boolean> => {
    const saved = await saveConfigMutation(
      '/api/model-status/config/groups',
      { groups },
      '自定义分组',
    )
    if (saved) setCustomGroups(groups)
    return saved
  }, [saveConfigMutation])

  // Save site title to backend
  const saveSiteTitleToBackend = useCallback(async (title: string): Promise<boolean> => {
    const mutationId = ++configMutationIdsRef.current.siteTitle
    const saved = await saveConfigMutation(
      '/api/model-status/config/site-title',
      { site_title: title },
      '站点标题',
    )
    if (saved) {
      persistedSiteTitleRef.current = title
    } else if (mutationId === configMutationIdsRef.current.siteTitle) {
      const persistedTitle = persistedSiteTitleRef.current
      setSiteTitle(current => current === title ? persistedTitle : current)
    }
    return saved
  }, [saveConfigMutation])

  const applySelectedModels = useCallback(async (models: string[]): Promise<boolean> => {
    const nextModels = [...models]
    selectedModelsRef.current = nextModels
    setSelectedModels(nextModels)
    const saved = await saveSelectedModelsToBackend(nextModels)
    if (!saved && sameStringArray(selectedModelsRef.current, nextModels)) {
      const persistedModels = persistedSelectedModelsRef.current
      selectedModelsRef.current = persistedModels
      setSelectedModels(persistedModels)
    }
    return saved
  }, [saveSelectedModelsToBackend])

  // Select all models in a group
  const selectGroupModels = useCallback((group: CustomModelGroup) => {
    const newModels = [...new Set([...selectedModels, ...group.models.filter(m => availableModels.some(a => a.model_name === m))])]
    void applySelectedModels(newModels)
  }, [selectedModels, availableModels, applySelectedModels])

  // Order the complete visible catalogue before applying view filters so drag
  // operations can preserve models that are temporarily hidden by a filter.
  const orderedModelStatuses = useMemo(() => {
    if (visibleModelStatuses.length === 0) return []

    switch (sortMode) {
      case 'availability':
        return [...visibleModelStatuses].sort((a, b) => b.success_rate - a.success_rate)
      case 'custom':
        if (customOrder.length === 0) {
          return visibleModelStatuses
        }
        return [...visibleModelStatuses].sort((a, b) => {
          const indexA = customOrder.indexOf(a.model_name)
          const indexB = customOrder.indexOf(b.model_name)
          if (indexA === -1 && indexB === -1) return 0
          if (indexA === -1) return 1
          if (indexB === -1) return -1
          return indexA - indexB
        })
      default:
        return visibleModelStatuses
    }
  }, [visibleModelStatuses, sortMode, customOrder])

  const sortedModelStatuses = useMemo(() => {
    let result = orderedModelStatuses

    // Apply group filter
    if (groupFilter !== 'all') {
      if (groupFilter.startsWith('token:')) {
        // 令牌分组过滤
        const tokenGroupName = groupFilter.slice(6)
        const tg = tokenGroups.find(g => g.group_name === tokenGroupName)
        if (tg) {
          result = result.filter(m => tg.models.includes(m.model_name))
        }
      } else {
        // 自定义分组过滤（精确名 + 厂商关键字模糊匹配）
        const group = customGroups.find(g => g.id === groupFilter)
        if (group) {
          result = result.filter(m => modelMatchesGroup(m.model_name, group))
        }
      }
    }

    // Apply status filter
    if (statusFilter !== 'all') {
      result = result.filter(m => m.current_status === statusFilter)
    }

    return result
  }, [orderedModelStatuses, statusFilter, groupFilter, customGroups, tokenGroups])

  const applySortConfig = useCallback(async (
    mode: SortMode,
    order: string[] | undefined,
    successMessage: string,
  ) => {
    if (sortReconciliationRequired) {
      showToast('error', '排序配置尚未完成对账，请刷新页面读取服务端权威配置')
      return
    }
    const mutationId = ++configMutationIdsRef.current.sort
    setSortMode(mode)
    if (order !== undefined) setCustomOrder(order)

    const saved = await saveSortConfigToBackend(mode, order, mutationId)
    if (mutationId !== configMutationIdsRef.current.sort) return
    if (!saved) {
      const persistedMode = persistedSortModeRef.current
      setSortMode(current => current === mode ? persistedMode : current)
      if (order !== undefined) {
        const persistedOrder = persistedCustomOrderRef.current
        setCustomOrder(current => sameStringArray(current, order) ? persistedOrder : current)
      }
      return
    }
    showToast('success', successMessage)
  }, [saveSortConfigToBackend, showToast, sortReconciliationRequired])

  // Handle drag end for reordering
  const handleDragEnd = (event: DragEndEvent) => {
    const { active, over } = event

    if (over && active.id !== over.id) {
      const oldIndex = sortedModelStatuses.findIndex(m => m.model_name === active.id)
      const newIndex = sortedModelStatuses.findIndex(m => m.model_name === over.id)

      if (oldIndex !== -1 && newIndex !== -1) {
        const newOrder = mergeFilteredModelOrder(
          orderedModelStatuses.map(model => model.model_name),
          sortedModelStatuses.map(model => model.model_name),
          String(active.id),
          String(over.id),
          customOrder,
        )

        void applySortConfig('custom', newOrder, '已切换为自定义排序')
      }
    }
  }

  // Handle availability sort button click
  const handleAvailabilitySort = () => {
    void applySortConfig('availability', undefined, '已按成功率排序')
  }

  const toggleModelSelection = (model: string) => {
    const newModels = selectedModels.includes(model)
      ? selectedModels.filter(m => m !== model)
      : [...selectedModels, model]
    void applySelectedModels(newModels)
  }

  const selectAllModels = () => {
    const allModelNames = availableModels.map(m => m.model_name)
    void applySelectedModels(allModelNames)
  }

  const clearAllModels = () => {
    void applySelectedModels([])
  }

  const handleTimeWindowChange = (value: string) => {
    const mutationId = ++configMutationIdsRef.current.timeWindow
    setTimeWindow(value)
    setShowWindowDropdown(false)
    void saveTimeWindowToBackend(value).then(saved => {
      if (!saved && mutationId === configMutationIdsRef.current.timeWindow) {
        const persistedValue = persistedTimeWindowRef.current
        setTimeWindow(current => current === value ? persistedValue : current)
      }
    })
  }

  const handleThemeChange = (value: string) => {
    const mutationId = ++configMutationIdsRef.current.theme
    setTheme(value)
    setShowThemeDropdown(false)
    void saveThemeToBackend(value).then(saved => {
      if (!saved && mutationId === configMutationIdsRef.current.theme) {
        const persistedValue = persistedThemeRef.current
        setTheme(current => current === value ? persistedValue : current)
      }
    })
  }

  const handleRefreshIntervalChange = (value: number) => {
    const mutationId = ++configMutationIdsRef.current.refreshInterval
    setRefreshInterval(value)
    setShowIntervalDropdown(false)
    void saveRefreshIntervalToBackend(value).then(saved => {
      if (!saved && mutationId === configMutationIdsRef.current.refreshInterval) {
        const persistedValue = persistedRefreshIntervalRef.current
        setRefreshInterval(current => current === value ? persistedValue : current)
      }
    })
  }

  if (loading && modelStatuses.length === 0) {
    return (
      <div className={cn("space-y-6", isEmbed && "p-4")}>
        {/* Skeleton Header */}
        <Card>
          <CardContent className="p-4">
            <div className="flex flex-col sm:flex-row sm:items-center sm:justify-between gap-4">
              <div>
                <div className="flex items-center gap-3">
                  <div className="h-6 w-32 bg-muted animate-pulse rounded-md" />
                  <div className="h-5 w-14 bg-muted animate-pulse rounded-full" />
                </div>
                <div className="h-4 w-48 bg-muted animate-pulse rounded-md mt-2" />
              </div>
              <div className="flex items-center gap-3">
                {[80, 64, 80, 96, 80, 72].map((w, i) => (
                  <div key={i} className="h-9 bg-muted animate-pulse rounded-md" style={{ width: w, animationDelay: `${i * 100}ms` }} />
                ))}
              </div>
            </div>
          </CardContent>
        </Card>

        {/* Skeleton Cards Grid */}
        <div className="grid grid-cols-1 lg:grid-cols-2 gap-3">
          {[0, 1, 2, 3].map(i => (
            <Card key={i} className="overflow-hidden" style={{ animationDelay: `${i * 150}ms` }}>
              <div className="px-4 pt-3 pb-3 animate-in fade-in-0 duration-500" style={{ animationDelay: `${i * 150}ms` }}>
                {/* Skeleton header row */}
                <div className="flex items-center gap-2 mb-2.5">
                  <div className="w-6 h-6 rounded-md bg-muted animate-pulse" />
                  <div className="h-4 bg-muted animate-pulse rounded-md" style={{ width: `${120 + i * 30}px` }} />
                  <div className="h-5 w-12 bg-muted animate-pulse rounded-full" />
                  <div className="ml-auto flex items-center gap-1">
                    <div className="h-4 w-10 bg-muted animate-pulse rounded-md" />
                    <div className="h-4 w-14 bg-muted animate-pulse rounded-md" />
                  </div>
                </div>
                {/* Skeleton status bar */}
                <div className="flex gap-[3px]">
                  {Array.from({ length: 24 }).map((_, j) => (
                    <div
                      key={j}
                      className={cn(
                        "flex-1 h-5 bg-muted animate-pulse",
                        j === 0 ? "rounded-l-md rounded-r-sm" :
                          j === 23 ? "rounded-r-md rounded-l-sm" : "rounded-sm"
                      )}
                      style={{ animationDelay: `${(i * 150) + (j * 20)}ms` }}
                    />
                  ))}
                </div>
                {/* Skeleton time labels */}
                <div className="flex justify-between mt-1.5">
                  <div className="h-3 w-10 bg-muted animate-pulse rounded" />
                  <div className="h-3 w-10 bg-muted animate-pulse rounded" />
                  <div className="h-3 w-8 bg-muted animate-pulse rounded" />
                </div>
              </div>
            </Card>
          ))}
        </div>
      </div>
    )
  }

  return (
    <div className={cn("space-y-5", isEmbed && "p-4")}>
      {/* Header */}
      <Card className="overflow-visible border-0 shadow-md">
        <div className="bg-gradient-to-r from-primary/5 via-primary/3 to-transparent">
          <CardContent className="p-5">
            <div className="flex flex-col sm:flex-row sm:items-center sm:justify-between gap-4">
              <div>
                <div className="flex items-center gap-3">
                  <div className="flex items-center gap-2.5">
                    <div className="w-8 h-8 rounded-lg bg-primary/10 flex items-center justify-center">
                      <Layers className="h-4.5 w-4.5 text-primary" />
                    </div>
                    <h2 className="text-xl font-semibold tracking-tight whitespace-nowrap">模型状态监控</h2>
                  </div>
                  <Badge variant="outline" className="font-normal whitespace-nowrap shrink-0">{TIME_WINDOWS.find(w => w.value === timeWindow)?.label || '24小时'} 滑动窗口</Badge>
                </div>
                <p className="text-sm text-muted-foreground mt-2 flex items-center flex-wrap gap-x-3 gap-y-1">
                  <span>监控 <span className="font-semibold text-foreground">{selectedModels.length}</span> 个模型</span>
                  {modelStatuses.length > 0 && (
                    <>
                      <span className="text-muted-foreground/40">·</span>
                      <span>总请求 <span className="font-semibold text-foreground tabular-nums">{modelStatuses.reduce((sum, m) => sum + m.total_requests, 0).toLocaleString()}</span></span>
                      <span className="text-muted-foreground/40">·</span>
                      <span>平均成功率 <span className={cn(
                        "font-semibold tabular-nums",
                        avgSuccessRate === null
                          ? 'text-muted-foreground'
                          : avgSuccessRate >= 95
                            ? 'text-green-600'
                            : avgSuccessRate >= 80
                              ? 'text-yellow-600'
                              : 'text-red-600'
                      )}>{avgSuccessRate === null ? '—' : `${avgSuccessRate}%`}</span></span>
                      <span className="text-muted-foreground/40">·</span>
                      <span className="flex items-center gap-1.5">
                        <span className="inline-flex items-center gap-1"><span className="w-2 h-2 rounded-full bg-green-500" /><span className="font-medium text-green-600 tabular-nums">{statusCounts.green}</span></span>
                        <span className="inline-flex items-center gap-1"><span className="w-2 h-2 rounded-full bg-yellow-500" /><span className="font-medium text-yellow-600 tabular-nums">{statusCounts.yellow}</span></span>
                        <span className="inline-flex items-center gap-1"><span className="w-2 h-2 rounded-full bg-red-500" /><span className="font-medium text-red-600 tabular-nums">{statusCounts.red}</span></span>
                        <span className="inline-flex items-center gap-1"><span className="w-2 h-2 rounded-full bg-gray-400" /><span className="font-medium text-muted-foreground tabular-nums">{statusCounts.unknown}</span></span>
                      </span>
                    </>
                  )}
                </p>
              </div>
            <div className="flex items-center gap-3">
              {/* Time Window Selector */}
              <div className="relative" ref={windowDropdownRef}>
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => setShowWindowDropdown(!showWindowDropdown)}
                  className="h-9"
                >
                  <Clock className="h-4 w-4 mr-2" />
                  {TIME_WINDOWS.find(w => w.value === timeWindow)?.label}
                  <ChevronDown className="h-3 w-3 ml-1" />
                </Button>

                {showWindowDropdown && (
                  <div className="absolute right-0 mt-1 w-36 bg-popover border rounded-md shadow-lg z-40">
                    <div className="p-2 border-b">
                      <p className="text-xs text-muted-foreground">时间窗口</p>
                    </div>
                    <div className="p-1">
                      {TIME_WINDOWS.map(({ value, label }) => (
                        <button
                          key={value}
                          onClick={() => handleTimeWindowChange(value)}
                          className={cn(
                            "w-full text-left px-3 py-2 text-sm rounded hover:bg-accent transition-colors",
                            timeWindow === value && "bg-accent text-accent-foreground"
                          )}
                        >
                          {label}
                        </button>
                      ))}
                    </div>
                  </div>
                )}
              </div>

              {/* Theme Selector */}
              <div className="relative" ref={themeDropdownRef}>
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => setShowThemeDropdown(!showThemeDropdown)}
                  className="h-9"
                >
                  <Palette className="h-4 w-4 mr-2" />
                  {THEMES.find(t => t.id === theme)?.name || '主题'}
                  <ChevronDown className="h-3 w-3 ml-1" />
                </Button>

                {showThemeDropdown && (
                  <div className="absolute right-0 mt-1 w-56 bg-popover border rounded-md shadow-lg z-40">
                    <div className="p-2 border-b">
                      <p className="text-xs text-muted-foreground">嵌入页面主题</p>
                    </div>
                    <div className="p-1">
                      {THEMES.map((t) => {
                        const ThemeIcon = t.icon
                        return (
                          <button
                            key={t.id}
                            onClick={() => handleThemeChange(t.id)}
                            className={cn(
                              "w-full text-left px-3 py-2 text-sm rounded hover:bg-accent transition-colors flex items-center gap-3",
                              theme === t.id && "bg-accent text-accent-foreground"
                            )}
                          >
                            <div className={cn("w-6 h-6 rounded flex items-center justify-center", t.preview)}>
                              <ThemeIcon className="h-3.5 w-3.5 text-white mix-blend-difference" />
                            </div>
                            <div className="flex-1 min-w-0">
                              <div className="font-medium">{t.name}</div>
                              <div className="text-xs text-muted-foreground truncate">{t.description}</div>
                            </div>
                            {theme === t.id && <Check className="h-4 w-4 text-primary flex-shrink-0" />}
                          </button>
                        )
                      })}
                    </div>
                  </div>
                )}
              </div>

              {/* Model Selector */}
              <div className="relative" ref={modelSelectorRef}>
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => setShowModelSelector(!showModelSelector)}
                  className="h-9"
                >
                  <Settings2 className="h-4 w-4 mr-2" />
                  选择模型
                  <ChevronDown className="h-3 w-3 ml-1" />
                </Button>

                {showModelSelector && (
                  <div className="absolute right-0 mt-1 w-80 bg-popover border rounded-md shadow-lg z-40 max-h-[520px] flex flex-col overflow-hidden">
                    <div className="p-2 border-b flex justify-between items-center">
                      <p className="text-xs text-muted-foreground">选择要监控的模型</p>
                      <div className="flex gap-1">
                        <Button variant="ghost" size="sm" className="h-6 text-xs" onClick={selectAllModels}>
                          全选
                        </Button>
                        <Button variant="ghost" size="sm" className="h-6 text-xs" onClick={() => {
                          const activeModels = availableModels
                            .filter(m => m.request_count_24h > 0)
                            .map(m => m.model_name)
                          void applySelectedModels(activeModels)
                        }}>
                          有记录
                        </Button>
                        <Button variant="ghost" size="sm" className="h-6 text-xs" onClick={clearAllModels}>
                          清空
                        </Button>
                      </div>
                    </div>
                    {/* Group Quick Select */}
                    {customGroups.length > 0 && (
                      <div className="p-2 border-b">
                        <p className="text-xs text-muted-foreground mb-1.5">按分组选择</p>
                        <div className="flex flex-wrap gap-1">
                          {customGroups.map((group) => {
                            const groupModelCount = group.models.filter(m => availableModels.some(a => a.model_name === m)).length
                            const selectedInGroup = group.models.filter(m => selectedModels.includes(m)).length
                            const allSelected = selectedInGroup === groupModelCount && groupModelCount > 0
                            return (
                              <button
                                key={group.id}
                                onClick={() => {
                                  if (allSelected) {
                                    const newModels = selectedModels.filter(m => !group.models.includes(m))
                                    void applySelectedModels(newModels)
                                  } else {
                                    selectGroupModels(group)
                                  }
                                }}
                                className={cn(
                                  "inline-flex items-center gap-1 px-2 py-1 text-xs rounded-md border transition-colors",
                                  allSelected
                                    ? "bg-primary/10 border-primary/30 text-primary font-medium"
                                    : selectedInGroup > 0
                                    ? "bg-accent/50 border-border text-foreground"
                                    : "bg-background border-border text-muted-foreground hover:bg-accent"
                                )}
                              >
                                <Layers className="h-3 w-3" />
                                {group.name}
                                <span className="opacity-60">{selectedInGroup}/{groupModelCount}</span>
                              </button>
                            )
                          })}
                        </div>
                      </div>
                    )}
                    {/* Search input */}
                    <div className="px-2 py-1.5 border-b">
                      <div className="relative">
                        <Search className="absolute left-2.5 top-1/2 -translate-y-1/2 h-3.5 w-3.5 text-muted-foreground" />
                        <input
                          type="text"
                          placeholder="搜索模型..."
                          value={modelSearchQuery}
                          onChange={(e) => setModelSearchQuery(e.target.value)}
                          className="w-full h-8 pl-8 pr-3 text-sm bg-muted/50 border rounded-md focus:outline-none focus:ring-1 focus:ring-primary"
                          autoFocus
                        />
                      </div>
                    </div>
                    <div className="p-1 flex-1 min-h-0 overflow-y-auto">
                      {availableModels
                        .filter(model => !modelSearchQuery || model.model_name.toLowerCase().includes(modelSearchQuery.toLowerCase()))
                        .map(model => (
                          <button
                            key={model.model_name}
                            onClick={() => toggleModelSelection(model.model_name)}
                            className={cn(
                              "w-full text-left px-3 py-2 text-sm rounded hover:bg-accent transition-colors flex items-center justify-between",
                              selectedModels.includes(model.model_name) && "bg-accent"
                            )}
                          >
                            <div className="flex items-center gap-2 min-w-0">
                              <div className="flex-shrink-0 w-5 h-5 flex items-center justify-center">
                                <ModelLogo modelName={model.model_name} size={16} />
                              </div>
                              <span className={cn(
                                "truncate",
                                model.request_count_24h === 0 && "text-muted-foreground"
                              )}>
                                {model.model_name}
                              </span>
                            </div>
                            <div className="flex items-center gap-2 flex-shrink-0">
                              {model.request_count_24h > 0 ? (
                                <span className="text-xs text-muted-foreground">
                                  {model.request_count_24h.toLocaleString()}
                                </span>
                              ) : (
                                <span className="text-xs text-orange-400">无请求</span>
                              )}
                              {selectedModels.includes(model.model_name) && (
                                <Check className="h-4 w-4 text-primary" />
                              )}
                            </div>
                          </button>
                        ))}
                      {availableModels.filter(m => !modelSearchQuery || m.model_name.toLowerCase().includes(modelSearchQuery.toLowerCase())).length === 0 && (
                        <p className="text-sm text-muted-foreground text-center py-4">
                          {modelSearchQuery ? '未找到匹配的模型' : '暂无可用模型'}
                        </p>
                      )}
                    </div>
                  </div>
                )}
              </div>

              {/* Refresh Interval */}
              <div className="relative" ref={intervalDropdownRef}>
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => setShowIntervalDropdown(!showIntervalDropdown)}
                  className="h-9 w-[120px] justify-between"
                >
                  <div className="flex items-center">
                    <Timer className="h-4 w-4 mr-2 flex-shrink-0" />
                    {refreshInterval > 0 && countdown > 0 ? (
                      <span className="text-primary font-medium tabular-nums">{formatCountdown(countdown)}</span>
                    ) : (
                      <span>自动刷新</span>
                    )}
                  </div>
                  <ChevronDown className="h-3 w-3 flex-shrink-0" />
                </Button>

                {showIntervalDropdown && (
                  <div className="absolute right-0 mt-1 w-36 bg-popover border rounded-md shadow-lg z-40">
                    <div className="p-2 border-b">
                      <p className="text-xs text-muted-foreground">刷新间隔</p>
                    </div>
                    <div className="p-1">
                      {REFRESH_INTERVALS.map(({ value, label }) => (
                        <button
                          key={value}
                          onClick={() => handleRefreshIntervalChange(value)}
                          className={cn(
                            "w-full text-left px-3 py-2 text-sm rounded hover:bg-accent transition-colors",
                            refreshInterval === value && "bg-accent text-accent-foreground"
                          )}
                        >
                          {label}
                        </button>
                      ))}
                    </div>
                  </div>
                )}
              </div>

              {/* Availability Sort Button */}
              <Button
                variant={sortMode === 'availability' ? 'default' : 'outline'}
                size="sm"
                onClick={handleAvailabilitySort}
                className="h-9"
                title="按成功率从高到低排序"
              >
                <ArrowUpDown className="h-4 w-4 mr-2" />
                高可用排序
              </Button>

              {/* Manual Refresh */}
              <Button onClick={handleRefresh} disabled={refreshing}>
                {refreshing ? (
                  <Loader2 className="h-4 w-4 mr-2 animate-spin" />
                ) : (
                  <RefreshCw className="h-4 w-4 mr-2" />
                )}
                刷新
              </Button>

              {/* Fullscreen Button */}
              <Button
                variant="ghost"
                size="icon"
                onClick={toggleFullscreen}
                title={isFullscreen ? '退出全屏' : '全屏模式'}
                className="h-9 w-9"
              >
                {isFullscreen ? (
                  <Minimize2 className="h-4 w-4" />
                ) : (
                  <Maximize2 className="h-4 w-4" />
                )}
              </Button>

              {/* Site Title Setting */}
              <div className="relative">
                <Button
                  variant={showSiteTitleInput ? 'default' : 'ghost'}
                  size="sm"
                  onClick={() => setShowSiteTitleInput(!showSiteTitleInput)}
                  title="设置站点标题"
                  className="h-9"
                >
                  <Pencil className="h-4 w-4 mr-2" />
                  站点标题
                </Button>
                {showSiteTitleInput && (
                  <div className="absolute right-0 mt-1 w-72 bg-popover border rounded-md shadow-lg z-50 p-3">
                    <p className="text-xs text-muted-foreground mb-2">嵌入页面显示的标题（留空使用默认）</p>
                    <input
                      type="text"
                      placeholder="例如：OpenAI-模型状态监控"
                      value={siteTitle}
                      onChange={(e) => setSiteTitle(e.target.value)}
                      onBlur={() => { void saveSiteTitleToBackend(siteTitle) }}
                      onKeyDown={(e) => {
                        if (e.key === 'Enter') {
                          e.currentTarget.blur()
                          setShowSiteTitleInput(false)
                        }
                        if (e.key === 'Escape') {
                          setShowSiteTitleInput(false)
                        }
                      }}
                      className="w-full h-8 px-3 text-sm bg-muted/50 border rounded-md focus:outline-none focus:ring-1 focus:ring-primary"
                      autoFocus
                    />
                    {siteTitle && (
                      <p className="text-xs text-muted-foreground mt-2">预览: <span className="font-medium text-foreground">{siteTitle}</span></p>
                    )}
                  </div>
                )}
              </div>

              {/* Embed Help Button */}
              <Button
                variant="ghost"
                size="icon"
                onClick={() => setShowEmbedHelp(true)}
                title="嵌入说明"
                className="h-9 w-9"
              >
                <HelpCircle className="h-4 w-4" />
              </Button>
            </div>
          </div>
        </CardContent>
        </div>
      </Card>

      {/* Embed Help Modal */}
      {showEmbedHelp && (
        <EmbedHelpModal onClose={() => setShowEmbedHelp(false)} />
      )}

      {/* Group Filter + Status Filter */}
      {modelStatuses.length > 0 && (
        <div className="space-y-3">
          {/* Model Group Filter */}
          <div className="flex items-center gap-2 overflow-x-auto pb-1 scrollbar-hide">
            <Tag className="h-4 w-4 text-muted-foreground flex-shrink-0" />
            <button
              onClick={() => handleGroupFilterChange('all')}
              className={cn(
                "inline-flex items-center gap-1.5 px-3 py-1.5 text-sm rounded-full border transition-all whitespace-nowrap flex-shrink-0",
                groupFilter === 'all'
                  ? "bg-primary text-primary-foreground border-primary shadow-sm"
                  : "bg-background hover:bg-muted border-border text-muted-foreground hover:text-foreground"
              )}
            >
              全部
              <span className={cn("text-xs tabular-nums", groupFilter === 'all' ? "opacity-80" : "text-muted-foreground")}>
                {groupCounts.all}
              </span>
            </button>
            {customGroups.map((group, index) => {
              const color = GROUP_COLORS[index % GROUP_COLORS.length]
              const isActive = groupFilter === group.id
              const count = groupCounts[group.id] || 0
              return (
                <button
                  key={group.id}
                  onClick={() => handleGroupFilterChange(group.id)}
                  className={cn(
                    "inline-flex items-center gap-2 px-3.5 py-2 text-sm font-medium rounded-full border transition-all whitespace-nowrap flex-shrink-0",
                    isActive
                      ? cn("bg-gradient-to-r shadow-sm border", color)
                      : "bg-background hover:bg-muted border-border text-muted-foreground hover:text-foreground"
                  )}
                >
                  {(() => {
                    if (group.icon) {
                      const iconOpt = GROUP_ICON_OPTIONS.find(o => o.key === group.icon)
                      if (iconOpt) {
                        const IconComp = iconOpt.component
                        return <IconComp size={16} className="flex-shrink-0" />
                      }
                    }
                    return <Layers size={14} className="flex-shrink-0" />
                  })()}
                  {group.name}
                  <span className={cn("text-xs tabular-nums", isActive ? "opacity-80" : "text-muted-foreground")}>
                    {count}
                  </span>
                </button>
              )
            })}
            {/* Token Groups Separator + Tabs */}
            {tokenGroups.length > 0 && (customGroups.length > 0 ? (
              <div className="w-px h-5 bg-border flex-shrink-0 mx-1" />
            ) : null)}
            {tokenGroups.map((tg) => {
              const filterId = `token:${tg.group_name}`
              const isActive = groupFilter === filterId
              const count = groupCounts[filterId] || 0
              return (
                <button
                  key={filterId}
                  onClick={() => handleGroupFilterChange(filterId)}
                  className={cn(
                    "inline-flex items-center gap-1.5 px-3 py-1.5 text-sm rounded-full border transition-all whitespace-nowrap flex-shrink-0",
                    isActive
                      ? "bg-blue-500/15 text-blue-700 dark:text-blue-400 border-blue-500/30 shadow-sm"
                      : "bg-background hover:bg-muted border-border text-muted-foreground hover:text-foreground"
                  )}
                >
                  <KeyRound size={13} className="flex-shrink-0" />
                  {tg.group_name}
                  <span className={cn("text-xs tabular-nums", isActive ? "opacity-80" : "text-muted-foreground")}>
                    {count}
                  </span>
                </button>
              )
            })}
            {/* Add Group Button */}
            <button
              onClick={() => setShowGroupManager(true)}
              className="inline-flex items-center gap-1 px-2.5 py-1.5 text-sm rounded-full border border-dashed border-border text-muted-foreground hover:text-foreground hover:border-primary/50 hover:bg-muted transition-all whitespace-nowrap flex-shrink-0"
            >
              <Settings2 size={13} />
              管理分组
            </button>
          </div>

          {/* Status Filter Tabs */}
          <div className="flex items-center gap-2">
            <Filter className="h-4 w-4 text-muted-foreground" />
            {[
              { value: 'all' as StatusFilter, label: '全部', count: visibleModelStatuses.length },
              { value: 'green' as StatusFilter, label: '正常', count: statusCounts.green, color: 'text-green-600' },
              { value: 'yellow' as StatusFilter, label: '警告', count: statusCounts.yellow, color: 'text-yellow-600' },
              { value: 'red' as StatusFilter, label: '异常', count: statusCounts.red, color: 'text-red-600' },
              { value: 'unknown' as StatusFilter, label: '暂无数据', count: statusCounts.unknown, color: 'text-muted-foreground' },
            ].map(tab => (
              <button
                key={tab.value}
                onClick={() => setStatusFilter(tab.value)}
                className={cn(
                  "px-3 py-1.5 text-sm rounded-md transition-all",
                  statusFilter === tab.value
                    ? "bg-primary text-primary-foreground shadow-sm"
                    : "hover:bg-muted text-muted-foreground hover:text-foreground"
                )}
              >
                {tab.label}
                <span className={cn(
                  "ml-1.5 text-xs tabular-nums",
                  statusFilter === tab.value ? "opacity-80" : (tab.color || "")
                )}>
                  {tab.count}
                </span>
              </button>
            ))}
          </div>
        </div>
      )}

      {/* Group Manager Modal */}
      {showGroupManager && (
        <GroupManagerModal
          groups={customGroups}
          allModels={groupManagerModels}
          onSave={async (groups) => {
            const saved = await saveCustomGroups(groups)
            // Reset filter if the active group was deleted
            if (saved && groupFilter !== 'all' && !groupFilter.startsWith('token:') && !groups.find(g => g.id === groupFilter)) {
              handleGroupFilterChange('all')
            }
            return saved
          }}
          onClose={() => setShowGroupManager(false)}
        />
      )}

      {/* Model Status Cards */}
      {sortedModelStatuses.length > 0 ? (
        <DndContext
          sensors={sensors}
          collisionDetection={closestCenter}
          onDragEnd={handleDragEnd}
        >
          <SortableContext
            items={sortedModelStatuses.map(m => m.model_name)}
            strategy={rectSortingStrategy}
          >
            <div key={statusFilter} className="grid grid-cols-1 lg:grid-cols-2 gap-3">
              {sortedModelStatuses.map(model => (
                <SortableModelCard key={model.model_name} model={model} />
              ))}
            </div>
          </SortableContext>
        </DndContext>
      ) : initialLoading ? (
        /* Skeleton cards during initial loading transition */
        <div className="grid grid-cols-1 lg:grid-cols-2 gap-3">
          {[0, 1, 2, 3].map(i => (
            <Card key={i} className="overflow-hidden">
              <div className="px-4 pt-3 pb-3 animate-in fade-in-0 duration-500" style={{ animationDelay: `${i * 150}ms` }}>
                <div className="flex items-center gap-2 mb-2.5">
                  <div className="w-6 h-6 rounded-md bg-muted animate-pulse" />
                  <div className="h-4 bg-muted animate-pulse rounded-md" style={{ width: `${120 + i * 30}px` }} />
                  <div className="h-5 w-12 bg-muted animate-pulse rounded-full" />
                  <div className="ml-auto flex items-center gap-1">
                    <div className="h-4 w-10 bg-muted animate-pulse rounded-md" />
                    <div className="h-4 w-14 bg-muted animate-pulse rounded-md" />
                  </div>
                </div>
                <div className="flex gap-[3px]">
                  {Array.from({ length: 24 }).map((_, j) => (
                    <div
                      key={j}
                      className={cn(
                        "flex-1 h-5 bg-muted animate-pulse",
                        j === 0 ? "rounded-l-md rounded-r-sm" :
                          j === 23 ? "rounded-r-md rounded-l-sm" : "rounded-sm"
                      )}
                      style={{ animationDelay: `${(i * 150) + (j * 20)}ms` }}
                    />
                  ))}
                </div>
                <div className="flex justify-between mt-1.5">
                  <div className="h-3 w-10 bg-muted animate-pulse rounded" />
                  <div className="h-3 w-10 bg-muted animate-pulse rounded" />
                  <div className="h-3 w-8 bg-muted animate-pulse rounded" />
                </div>
              </div>
            </Card>
          ))}
        </div>
      ) : (
        <Card>
          <CardContent className="py-12 text-center text-muted-foreground">
            {selectedModels.length === 0 ? (
              <p>请选择要监控的模型</p>
            ) : (
              <p>暂无模型状态数据</p>
            )}
          </CardContent>
        </Card>
      )}

      {/* Legend */}
      <Card className="bg-muted/30 border-dashed">
        <CardContent className="px-4 py-3">
          <div className="flex flex-wrap items-center gap-x-6 gap-y-2 text-xs text-muted-foreground">
            <span className="font-medium text-foreground/70">图例</span>
            <div className="flex items-center gap-1.5">
              <span className="w-2.5 h-2.5 rounded-sm bg-green-500" />
              <span>成功率 ≥ 95%</span>
            </div>
            <div className="flex items-center gap-1.5">
              <span className="w-2.5 h-2.5 rounded-sm bg-yellow-500" />
              <span>成功率 80-95%</span>
            </div>
            <div className="flex items-center gap-1.5">
              <span className="w-2.5 h-2.5 rounded-sm bg-red-500" />
              <span>成功率 &lt; 80%</span>
            </div>
            <div className="flex items-center gap-1.5">
              <span className="w-2.5 h-2.5 rounded-sm bg-gray-200 dark:bg-gray-700" />
              <span>无请求</span>
            </div>
            <div className="ml-auto text-[10px] text-muted-foreground/50">
              更新于 {new Date().toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit', second: '2-digit' })}
            </div>
          </div>
        </CardContent>
      </Card>
    </div>
  )
}

// Group Manager Modal Component
interface GroupManagerModalProps {
  groups: CustomModelGroup[]
  allModels: string[]
  onSave: (groups: CustomModelGroup[]) => Promise<boolean>
  onClose: () => void
}

function GroupManagerModal({ groups, allModels, onSave, onClose }: GroupManagerModalProps) {
  const [localGroups, setLocalGroups] = useState<CustomModelGroup[]>([...groups])
  const [editingGroup, setEditingGroup] = useState<CustomModelGroup | null>(null)
  const [editingName, setEditingName] = useState('')
  const [editingModels, setEditingModels] = useState<string[]>([])
  const [editingSearch, setEditingSearch] = useState('')
  const [editingIcon, setEditingIcon] = useState('')
  const [isCreating, setIsCreating] = useState(false)

  // Helper to render group icon
  const renderGroupIcon = (iconKey: string | undefined, size: number = 14) => {
    if (iconKey) {
      const iconOpt = GROUP_ICON_OPTIONS.find(o => o.key === iconKey)
      if (iconOpt) {
        const IconComp = iconOpt.component
        return <IconComp size={size} />
      }
    }
    return <Layers size={size} />
  }

  const handleCreateNew = () => {
    setEditingGroup(null)
    setEditingName('')
    setEditingIcon('')
    setEditingModels([])
    setEditingSearch('')
    setIsCreating(true)
  }

  const handleEditGroup = (group: CustomModelGroup) => {
    setEditingGroup(group)
    setEditingName(group.name)
    setEditingIcon(group.icon || '')
    setEditingModels([...group.models])
    setEditingSearch('')
    setIsCreating(true)
  }

  const handleDeleteGroup = async (groupId: string) => {
    const newGroups = localGroups.filter(g => g.id !== groupId)
    if (await onSave(newGroups)) setLocalGroups(newGroups)
  }

  const handleSaveEdit = async () => {
    if (!editingName.trim()) return

    let newGroups: CustomModelGroup[]
    if (editingGroup) {
      // Update existing
      newGroups = localGroups.map(g =>
        g.id === editingGroup.id
          ? { ...g, name: editingName.trim(), icon: editingIcon || undefined, models: editingModels }
          : g
      )
    } else {
      // Create new
      const newGroup: CustomModelGroup = {
        id: `group_${Date.now()}`,
        name: editingName.trim(),
        icon: editingIcon || undefined,
        models: editingModels,
      }
      newGroups = [...localGroups, newGroup]
    }

    if (!await onSave(newGroups)) return
    setLocalGroups(newGroups)
    setIsCreating(false)
    setEditingGroup(null)
  }

  const handlePresetCreate = (preset: { name: string; icon?: string; keywords: string[] }) => {
    // Match models by keywords
    const matchedModels = allModels.filter(m => {
      const lower = m.toLowerCase()
      return preset.keywords.some(k => lower.includes(k))
    })

    // Check if group name already exists
    const existingGroup = localGroups.find(g => g.name === preset.name)
    if (existingGroup) {
      // Update existing group with matched models
      handleEditGroup({ ...existingGroup, models: matchedModels })
      return
    }

    setEditingGroup(null)
    setEditingName(preset.name)
    setEditingIcon(preset.icon || '')
    setEditingModels(matchedModels)
    setEditingSearch('')
    setIsCreating(true)
  }

  const toggleModelInEdit = (modelName: string) => {
    setEditingModels(prev =>
      prev.includes(modelName)
        ? prev.filter(m => m !== modelName)
        : [...prev, modelName]
    )
  }

  const filteredModels = allModels.filter(m =>
    !editingSearch || m.toLowerCase().includes(editingSearch.toLowerCase())
  )

  return (
    <div className="fixed inset-0 bg-black/50 flex items-center justify-center z-50 p-4">
      <div className="bg-background border rounded-xl shadow-2xl max-w-2xl w-full max-h-[85vh] overflow-hidden flex flex-col">
        {/* Header */}
        <div className="flex items-center justify-between px-5 py-4 border-b bg-muted/30">
          <div className="flex items-center gap-2.5">
            <div className="w-8 h-8 rounded-lg bg-primary/10 flex items-center justify-center">
              <FolderPlus className="h-4 w-4 text-primary" />
            </div>
            <div>
              <h2 className="text-base font-semibold">管理模型分组</h2>
              <p className="text-xs text-muted-foreground">创建自定义分组，快速筛选模型</p>
            </div>
          </div>
          <Button variant="ghost" size="icon" onClick={onClose} className="h-8 w-8">
            <X className="h-4 w-4" />
          </Button>
        </div>

        <div className="flex-1 overflow-y-auto">
          {isCreating ? (
            /* Edit/Create Form */
            <div className="p-5 space-y-4">
              <div className="flex items-center gap-2 mb-1">
                <button
                  onClick={() => setIsCreating(false)}
                  className="text-sm text-muted-foreground hover:text-foreground transition-colors"
                >
                  ← 返回
                </button>
                <span className="text-sm font-medium">{editingGroup ? '编辑分组' : '创建分组'}</span>
              </div>

              {/* Group Name */}
              <div>
                <label className="text-sm font-medium mb-1.5 block">分组名称</label>
                <input
                  type="text"
                  value={editingName}
                  onChange={(e) => setEditingName(e.target.value)}
                  placeholder="例如: Claude 模型"
                  className="w-full h-9 px-3 text-sm bg-background border rounded-lg focus:outline-none focus:ring-2 focus:ring-primary/50"
                  autoFocus
                />
              </div>

              {/* Group Icon */}
              <div>
                <label className="text-sm font-medium mb-1.5 block">分组图标 <span className="font-normal text-muted-foreground">(可选)</span></label>
                <div className="flex flex-wrap gap-1.5">
                  <button
                    onClick={() => setEditingIcon('')}
                    className={cn(
                      "w-9 h-9 rounded-lg border-2 flex items-center justify-center transition-all",
                      !editingIcon ? "border-primary bg-primary/10" : "border-border hover:border-primary/50 hover:bg-muted"
                    )}
                    title="默认"
                  >
                    <Layers size={16} className="text-muted-foreground" />
                  </button>
                  {GROUP_ICON_OPTIONS.map(opt => {
                    const IconComp = opt.component
                    return (
                      <button
                        key={opt.key}
                        onClick={() => setEditingIcon(opt.key)}
                        className={cn(
                          "w-9 h-9 rounded-lg border-2 flex items-center justify-center transition-all",
                          editingIcon === opt.key ? "border-primary bg-primary/10" : "border-border hover:border-primary/50 hover:bg-muted"
                        )}
                        title={opt.label}
                      >
                        <IconComp size={18} />
                      </button>
                    )
                  })}
                </div>
              </div>

              {/* Model Selection */}
              <div>
                <div className="flex items-center justify-between mb-1.5">
                  <label className="text-sm font-medium">选择模型 <span className="font-normal text-muted-foreground">({editingModels.length} 个已选)</span></label>
                  <div className="flex gap-1">
                    <Button variant="ghost" size="sm" className="h-6 text-xs" onClick={() => setEditingModels([...allModels])}>全选</Button>
                    <Button variant="ghost" size="sm" className="h-6 text-xs" onClick={() => setEditingModels([])}>清空</Button>
                  </div>
                </div>
                {/* Search */}
                <div className="relative mb-2">
                  <Search className="absolute left-2.5 top-1/2 -translate-y-1/2 h-3.5 w-3.5 text-muted-foreground" />
                  <input
                    type="text"
                    placeholder="搜索模型..."
                    value={editingSearch}
                    onChange={(e) => setEditingSearch(e.target.value)}
                    className="w-full h-8 pl-8 pr-3 text-sm bg-muted/50 border rounded-lg focus:outline-none focus:ring-1 focus:ring-primary"
                  />
                </div>
                {/* Model List */}
                <div className="border rounded-lg max-h-60 overflow-y-auto">
                  {filteredModels.map(model => (
                    <button
                      key={model}
                      onClick={() => toggleModelInEdit(model)}
                      className={cn(
                        "w-full text-left px-3 py-2 text-sm flex items-center justify-between hover:bg-muted/50 transition-colors border-b last:border-b-0",
                        editingModels.includes(model) && "bg-primary/5"
                      )}
                    >
                      <div className="flex items-center gap-2 min-w-0">
                        <div className="flex-shrink-0 w-5 h-5 flex items-center justify-center">
                          <ModelLogo modelName={model} size={14} />
                        </div>
                        <span className="truncate">{model}</span>
                      </div>
                      {editingModels.includes(model) && (
                        <Check className="h-4 w-4 text-primary flex-shrink-0" />
                      )}
                    </button>
                  ))}
                  {filteredModels.length === 0 && (
                    <p className="text-sm text-muted-foreground text-center py-4">未找到匹配的模型</p>
                  )}
                </div>
              </div>

              {/* Save Button */}
              <div className="flex justify-end gap-2 pt-2">
                <Button variant="outline" size="sm" onClick={() => setIsCreating(false)}>取消</Button>
                <Button size="sm" onClick={handleSaveEdit} disabled={!editingName.trim()}>
                  {editingGroup ? '保存修改' : '创建分组'}
                </Button>
              </div>
            </div>
          ) : (
            /* Groups List */
            <div className="p-5 space-y-5">
              {/* Existing Groups */}
              {localGroups.length > 0 && (
                <div>
                  <h3 className="text-sm font-medium text-muted-foreground mb-2">已创建的分组</h3>
                  <div className="space-y-2">
                    {localGroups.map((group, index) => {
                      const color = GROUP_COLORS[index % GROUP_COLORS.length]
                      return (
                        <div
                          key={group.id}
                          className="flex items-center justify-between p-3 border rounded-lg hover:bg-muted/30 transition-colors"
                        >
                          <div className="flex items-center gap-3 min-w-0">
                            <div className={cn("w-8 h-8 rounded-lg bg-gradient-to-br flex items-center justify-center flex-shrink-0", color)}>
                              {renderGroupIcon(group.icon, 16)}
                            </div>
                            <div className="min-w-0">
                              <div className="font-medium text-sm">{group.name}</div>
                              <div className="text-xs text-muted-foreground truncate">
                                {group.models.length} 个模型
                                {group.models.length > 0 && (
                                  <span className="ml-1">· {group.models.slice(0, 3).join(', ')}{group.models.length > 3 ? ` 等` : ''}</span>
                                )}
                              </div>
                            </div>
                          </div>
                          <div className="flex items-center gap-1 flex-shrink-0">
                            <Button
                              variant="ghost"
                              size="icon"
                              className="h-7 w-7"
                              onClick={() => handleEditGroup(group)}
                              title="编辑"
                            >
                              <Pencil className="h-3.5 w-3.5" />
                            </Button>
                            <Button
                              variant="ghost"
                              size="icon"
                              className="h-7 w-7 text-destructive hover:text-destructive"
                              onClick={() => handleDeleteGroup(group.id)}
                              title="删除"
                            >
                              <Trash2 className="h-3.5 w-3.5" />
                            </Button>
                          </div>
                        </div>
                      )
                    })}
                  </div>
                </div>
              )}

              {/* Create New */}
              <div>
                <button
                  onClick={handleCreateNew}
                  className="w-full flex items-center justify-center gap-2 p-3 border-2 border-dashed rounded-lg text-sm text-muted-foreground hover:text-foreground hover:border-primary/50 hover:bg-muted/30 transition-all"
                >
                  <Plus size={16} />
                  创建新分组
                </button>
              </div>

              {/* Quick Presets */}
              <div>
                <h3 className="text-sm font-medium text-muted-foreground mb-2">快速创建（按模型名称自动匹配）</h3>
                <div className="flex flex-wrap gap-2">
                  {GROUP_PRESETS.map((preset) => {
                    const exists = localGroups.some(g => g.name === preset.name)
                    return (
                      <button
                        key={preset.name}
                        onClick={() => handlePresetCreate(preset)}
                        className={cn(
                          "inline-flex items-center gap-1.5 px-3 py-1.5 text-xs rounded-full border transition-all",
                          exists
                            ? "bg-muted text-muted-foreground border-border"
                            : "bg-background hover:bg-muted border-border hover:border-primary/50 text-foreground"
                        )}
                      >
                        <Tag size={12} />
                        {preset.name}
                        {exists && <Check size={12} className="text-green-500" />}
                      </button>
                    )
                  })}
                </div>
              </div>
            </div>
          )}
        </div>

        {/* Footer */}
        <div className="flex justify-end px-5 py-3 border-t bg-muted/20">
          <Button variant="outline" size="sm" onClick={onClose}>
            关闭
          </Button>
        </div>
      </div>
    </div>
  )
}

interface ModelStatusCardProps {
  model: ModelStatus
  dragHandleProps?: React.HTMLAttributes<HTMLDivElement>
}

// Sortable wrapper for ModelStatusCard
function SortableModelCard({ model }: { model: ModelStatus }) {
  const {
    attributes,
    listeners,
    setNodeRef,
    transform,
    transition,
    isDragging,
  } = useSortable({ id: model.model_name })

  const style = {
    transform: CSS.Transform.toString(transform),
    transition,
    opacity: isDragging ? 0.5 : 1,
    zIndex: isDragging ? 1000 : 'auto',
  }

  return (
    <div ref={setNodeRef} style={style} {...attributes}>
      <ModelStatusCard
        model={model}
        dragHandleProps={listeners}
      />
    </div>
  )
}

// Embed Help Modal Component
function EmbedHelpModal({ onClose }: { onClose: () => void }) {
  const [copied, setCopied] = useState<string | null>(null)

  // Get current origin for embed URL
  const currentOrigin = window.location.origin
  const embedPath = '/embed.html'
  const embedUrl = `${currentOrigin}${embedPath}`

  // Check if using IP address (recommend using domain with HTTPS)
  const isIpAddress = /^https?:\/\/(\d{1,3}\.){3}\d{1,3}/.test(currentOrigin)
  const isHttps = currentOrigin.startsWith('https://')

  const codeExamples = {
    basic: `<iframe 
  src="${embedUrl}" 
  width="100%" 
  height="600" 
  frameborder="0"
  style="border-radius: 8px;"
></iframe>`,
    responsive: `<div style="position: relative; width: 100%; padding-bottom: 56.25%;">
  <iframe 
    src="${embedUrl}" 
    style="position: absolute; top: 0; left: 0; width: 100%; height: 100%; border: none; border-radius: 8px;"
  ></iframe>
</div>`,
    fullpage: `<!DOCTYPE html>
<html>
<head>
  <title>模型状态监控</title>
  <style>
    body { margin: 0; padding: 0; }
    iframe { width: 100vw; height: 100vh; border: none; }
  </style>
</head>
<body>
  <iframe src="${embedUrl}"></iframe>
</body>
</html>`,
  }

  const copyToClipboard = async (code: string, key: string) => {
    try {
      // Try modern clipboard API first
      if (navigator.clipboard && window.isSecureContext) {
        await navigator.clipboard.writeText(code)
      } else {
        // Fallback for HTTP or older browsers
        const textArea = document.createElement('textarea')
        textArea.value = code
        textArea.style.position = 'fixed'
        textArea.style.left = '-999999px'
        textArea.style.top = '-999999px'
        document.body.appendChild(textArea)
        textArea.focus()
        textArea.select()
        document.execCommand('copy')
        document.body.removeChild(textArea)
      }
      setCopied(key)
      setTimeout(() => setCopied(null), 2000)
    } catch (err) {
      console.error('Failed to copy:', err)
    }
  }

  return (
    <div className="fixed inset-0 bg-black/50 flex items-center justify-center z-50 p-4">
      <div className="bg-background border rounded-lg shadow-xl max-w-2xl w-full max-h-[90vh] overflow-hidden">
        {/* Header */}
        <div className="flex items-center justify-between p-4 border-b">
          <h2 className="text-lg font-semibold">使用 iframe 嵌入模型状态监控</h2>
          <Button variant="ghost" size="icon" onClick={onClose}>
            <X className="h-4 w-4" />
          </Button>
        </div>

        {/* Content */}
        <div className="p-4 space-y-6 overflow-y-auto max-h-[calc(90vh-120px)]">
          {/* Security Warning for IP/HTTP */}
          {(isIpAddress || !isHttps) && (
            <div className="bg-amber-500/10 border border-amber-500/20 rounded-lg p-3">
              <h3 className="text-sm font-medium text-amber-600 dark:text-amber-400 mb-2">⚠️ 安全建议</h3>
              <ul className="text-sm text-muted-foreground space-y-1">
                {isIpAddress && (
                  <li>• 当前使用 IP 地址访问，建议配置域名以获得更好的兼容性</li>
                )}
                {!isHttps && (
                  <li>• 当前使用 HTTP 协议，<strong>强烈建议</strong>使用 HTTPS 以确保数据安全</li>
                )}
                <li>• 示例：<code className="bg-muted px-1 rounded">https://your-domain.com{embedPath}</code></li>
              </ul>
            </div>
          )}

          {/* Embed URL */}
          <div>
            <h3 className="text-sm font-medium mb-2">嵌入地址</h3>
            <div className="flex items-center gap-2">
              <code className="flex-1 bg-muted px-3 py-2 rounded text-sm break-all">
                {embedUrl}
              </code>
              <Button
                variant="outline"
                size="sm"
                onClick={() => copyToClipboard(embedUrl, 'url')}
              >
                {copied === 'url' ? <Check className="h-4 w-4" /> : <Copy className="h-4 w-4" />}
              </Button>
            </div>
          </div>

          {/* Features */}
          <div>
            <h3 className="text-sm font-medium mb-2">功能特点</h3>
            <ul className="text-sm text-muted-foreground space-y-1 list-disc list-inside">
              <li>无需登录即可访问（公开嵌入模式）</li>
              <li>支持多种主题风格（在主界面选择后自动同步）</li>
              <li>自动刷新数据，实时监控模型状态</li>
              <li>响应式设计，适配各种屏幕尺寸</li>
            </ul>
          </div>

          {/* Basic Example */}
          <div>
            <div className="flex items-center justify-between mb-2">
              <h3 className="text-sm font-medium">基础嵌入</h3>
              <Button
                variant="ghost"
                size="sm"
                onClick={() => copyToClipboard(codeExamples.basic, 'basic')}
              >
                {copied === 'basic' ? <Check className="h-4 w-4 mr-1" /> : <Copy className="h-4 w-4 mr-1" />}
                复制
              </Button>
            </div>
            <pre className="bg-muted p-3 rounded text-xs overflow-x-auto">
              <code>{codeExamples.basic}</code>
            </pre>
          </div>

          {/* Responsive Example */}
          <div>
            <div className="flex items-center justify-between mb-2">
              <h3 className="text-sm font-medium">响应式嵌入（16:9 比例）</h3>
              <Button
                variant="ghost"
                size="sm"
                onClick={() => copyToClipboard(codeExamples.responsive, 'responsive')}
              >
                {copied === 'responsive' ? <Check className="h-4 w-4 mr-1" /> : <Copy className="h-4 w-4 mr-1" />}
                复制
              </Button>
            </div>
            <pre className="bg-muted p-3 rounded text-xs overflow-x-auto">
              <code>{codeExamples.responsive}</code>
            </pre>
          </div>

          {/* Full Page Example */}
          <div>
            <div className="flex items-center justify-between mb-2">
              <h3 className="text-sm font-medium">全屏页面</h3>
              <Button
                variant="ghost"
                size="sm"
                onClick={() => copyToClipboard(codeExamples.fullpage, 'fullpage')}
              >
                {copied === 'fullpage' ? <Check className="h-4 w-4 mr-1" /> : <Copy className="h-4 w-4 mr-1" />}
                复制
              </Button>
            </div>
            <pre className="bg-muted p-3 rounded text-xs overflow-x-auto">
              <code>{codeExamples.fullpage}</code>
            </pre>
          </div>

          {/* Tips */}
          <div className="bg-blue-500/10 border border-blue-500/20 rounded-lg p-3">
            <h3 className="text-sm font-medium text-blue-600 dark:text-blue-400 mb-2">💡 提示</h3>
            <ul className="text-sm text-muted-foreground space-y-1">
              <li>• 在主界面选择的模型、主题、刷新间隔会自动同步到嵌入页面</li>
              <li>• 嵌入页面使用独立的公开 API，不需要认证</li>
              <li>• <strong>推荐使用域名 + HTTPS</strong> 方式部署，确保安全性和兼容性</li>
              <li>• 部分浏览器可能阻止 HTTP iframe 嵌入到 HTTPS 页面</li>
            </ul>
          </div>
        </div>

        {/* Footer */}
        <div className="flex justify-end gap-2 p-4 border-t">
          <Button variant="outline" onClick={onClose}>
            关闭
          </Button>
          <Button onClick={() => window.open(`${currentOrigin}${embedPath}`, '_blank')}>
            预览嵌入页面
          </Button>
        </div>
      </div>
    </div>
  )
}

function ModelStatusCard({ model, dragHandleProps }: ModelStatusCardProps) {
  const [hoveredSlot, setHoveredSlot] = useState<SlotStatus | null>(null)
  const [tooltipPosition, setTooltipPosition] = useState({ x: 0, y: 0 })
  const [tooltipFlipped, setTooltipFlipped] = useState(false)

  const handleMouseEnter = (slot: SlotStatus, event: React.MouseEvent) => {
    const rect = event.currentTarget.getBoundingClientRect()
    const viewportWidth = window.innerWidth
    // Boundary detection: flip tooltip below if too close to top
    const shouldFlip = rect.top < 100
    // Clamp X to prevent overflow at edges
    const clampedX = Math.max(120, Math.min(rect.left + rect.width / 2, viewportWidth - 120))
    setTooltipPosition({
      x: clampedX,
      y: shouldFlip ? rect.bottom + 10 : rect.top - 10,
    })
    setTooltipFlipped(shouldFlip)
    setHoveredSlot(slot)
  }

  const getTimeLabels = () => {
    switch (model.time_window) {
      case '1h': return ['60m前', '30m前', '现在']
      case '6h': return ['6h前', '3h前', '现在']
      case '12h': return ['12h前', '6h前', '现在']
      default: return ['24h前', '12h前', '现在']
    }
  }

  const timeLabels = getTimeLabels()

  // Success rate color based on status
  const rateColorClass = model.current_status === 'unknown' ? 'text-muted-foreground'
    : model.current_status === 'green' ? 'text-green-600 dark:text-green-400'
    : model.current_status === 'yellow' ? 'text-yellow-600 dark:text-yellow-400'
      : 'text-red-600 dark:text-red-400'

  // 状态颜色仅通过模型名右侧的徽章 + 成功率数字 + 时间槽点显示，
  // 卡片本身不再加左边色条/背景染色，保持视觉中性。
  const cardStatusClass = ''

  return (
    <Card className={cn(
      "overflow-hidden transition-all duration-200 hover:shadow-lg hover:border-primary/20",
      cardStatusClass
    )}>
      <div className="px-4 pt-3 pb-3">
        {/* Header row: drag handle + logo + name + badge + stats */}
        <div className="flex items-center gap-2 mb-2.5">
          {dragHandleProps && (
            <div
              {...dragHandleProps}
              className="flex items-center justify-center w-5 h-5 cursor-grab active:cursor-grabbing text-muted-foreground/50 hover:text-muted-foreground transition-colors flex-shrink-0"
              title="拖拽排序"
            >
              <GripVertical className="h-3.5 w-3.5" />
            </div>
          )}
          <div className="flex items-center justify-center w-6 h-6 rounded-md bg-muted/50 flex-shrink-0">
            <ModelLogo modelName={model.model_name} size={16} />
          </div>
          <span className="text-sm font-medium truncate" title={model.model_name}>
            {model.model_name}
          </span>
          <Badge
            variant={model.current_status === 'unknown' ? 'outline' : model.current_status === 'green' ? 'success' : model.current_status === 'yellow' ? 'warning' : 'destructive'}
            className="text-[10px] px-1.5 py-0 h-5 flex-shrink-0"
          >
            {STATUS_LABELS[model.current_status]}
          </Badge>
          <div className="ml-auto text-xs text-muted-foreground flex-shrink-0 tabular-nums">
            <span className={cn("font-semibold", rateColorClass)}>{model.current_status === 'unknown' ? '—' : `${model.success_rate}%`}</span>
            <span className="mx-1 text-muted-foreground/40">·</span>
            <span>{model.total_requests.toLocaleString()}</span>
          </div>
        </div>

        {/* Status grid - compact with rounded ends and staggered animation */}
        <div className="relative">
          <div className="flex gap-[3px]">
            {model.slot_data.map((slot, index) => (
              <div
                key={index}
                className={cn(
                  "flex-1 h-5 cursor-pointer transition-all hover:ring-1.5 hover:ring-primary hover:ring-offset-1 hover:scale-y-110",
                  // Rounded ends for pill shape
                  index === 0 ? "rounded-l-md rounded-r-sm" :
                    index === model.slot_data.length - 1 ? "rounded-r-md rounded-l-sm" :
                      "rounded-sm",
                  slot.total_requests === 0 ? STATUS_COLORS.empty : STATUS_COLORS[slot.status],
                  "animate-in fade-in-0 duration-300"
                )}
                style={{ animationDelay: `${index * 15}ms` }}
                onMouseEnter={(e) => handleMouseEnter(slot, e)}
                onMouseLeave={() => setHoveredSlot(null)}
              />
            ))}
          </div>

          {/* Time labels */}
          <div className="flex justify-between mt-1.5 text-[10px] text-muted-foreground/60">
            <span>{timeLabels[0]}</span>
            <span>{timeLabels[1]}</span>
            <span>{timeLabels[2]}</span>
          </div>

          {/* Tooltip with boundary detection and entrance animation */}
          {hoveredSlot && (
            <div
              className="fixed z-[9999] bg-popover border rounded-lg shadow-xl p-2.5 text-xs pointer-events-none animate-in fade-in-0 zoom-in-95 duration-150"
              style={{
                left: tooltipPosition.x,
                top: tooltipPosition.y,
                transform: tooltipFlipped ? 'translate(-50%, 0)' : 'translate(-50%, -100%)',
              }}
            >
              {/* Arrow indicator */}
              <div
                className={cn(
                  "absolute left-1/2 -translate-x-1/2 w-2 h-2 bg-popover border rotate-45",
                  tooltipFlipped
                    ? "-top-1 border-b-0 border-r-0"
                    : "-bottom-1 border-t-0 border-l-0"
                )}
              />
              <div className="font-medium mb-1.5">
                {formatDateTime(hoveredSlot.start_time)} - {formatTime(hoveredSlot.end_time)}
              </div>
              <div className="space-y-0.5 text-muted-foreground">
                <div className="flex justify-between gap-4">
                  <span>请求:</span>
                  <span className="font-medium text-foreground">{hoveredSlot.total_requests}</span>
                </div>
                <div className="flex justify-between gap-4">
                  <span>成功:</span>
                  <span className="font-medium text-green-600">{hoveredSlot.success_count}</span>
                </div>
                <div className="flex justify-between gap-4">
                  <span>成功率:</span>
                  <span className={cn(
                    "font-medium",
                    hoveredSlot.total_requests === 0 || hoveredSlot.status === 'unknown'
                      ? 'text-muted-foreground'
                      : hoveredSlot.status === 'green'
                        ? 'text-green-600'
                        : hoveredSlot.status === 'yellow'
                          ? 'text-yellow-600'
                          : 'text-red-600'
                  )}>
                    {hoveredSlot.total_requests === 0 || hoveredSlot.status === 'unknown'
                      ? '—'
                      : `${hoveredSlot.success_rate}%`}
                  </span>
                </div>
              </div>
            </div>
          )}
        </div>
      </div>
    </Card>
  )
}

export default ModelStatusMonitor
