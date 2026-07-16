import { useState } from 'react'
import { AlertTriangle, Loader2 } from 'lucide-react'
import { Button } from './ui/button'

export type QuotaMode = 'fixed'
export type ExpireMode = 'never' | 'days' | 'date'

export interface GenerateFormData {
  name: string
  reason: string
  count: number
  quota_mode: QuotaMode
  fixed_amount: number
  expire_mode: ExpireMode
  expire_days: number
  expire_date: string
}

interface GeneratorFormProps {
  onSubmit: (data: GenerateFormData) => void
  isLoading: boolean
}

const defaultFormData: GenerateFormData = {
  name: '',
  reason: '',
  count: 1,
  quota_mode: 'fixed',
  fixed_amount: 1,
  expire_mode: 'never',
  expire_days: 30,
  expire_date: '',
}

export function GeneratorForm({ onSubmit, isLoading }: GeneratorFormProps) {
  const [formData, setFormData] = useState<GenerateFormData>(defaultFormData)
  const [errors, setErrors] = useState<Partial<Record<keyof GenerateFormData, string>>>({})

  const validateForm = (): boolean => {
    const newErrors: Partial<Record<keyof GenerateFormData, string>> = {}
    const reason = formData.reason.trim()

    if (!formData.name.trim()) newErrors.name = '请输入兑换码名称'
    if (reason.length < 3) newErrors.reason = '请输入至少 3 个字符的具体操作原因'
    if (reason.length > 1000) newErrors.reason = '操作原因不能超过 1000 个字符'
    if (formData.count < 1 || formData.count > 100) newErrors.count = '数量必须在 1-100 之间'
    if (formData.fixed_amount <= 0) newErrors.fixed_amount = '固定额度必须大于 0'

    if (formData.expire_mode === 'days' && formData.expire_days < 1) {
      newErrors.expire_days = '过期天数必须大于 0'
    } else if (formData.expire_mode === 'date') {
      if (!formData.expire_date) newErrors.expire_date = '请选择过期日期'
      else if (new Date(formData.expire_date) <= new Date()) newErrors.expire_date = '过期日期必须在未来'
    }

    setErrors(newErrors)
    return Object.keys(newErrors).length === 0
  }

  const handleSubmit = (event: React.FormEvent) => {
    event.preventDefault()
    if (validateForm()) onSubmit({ ...formData, name: formData.name.trim(), reason: formData.reason.trim() })
  }

  const updateField = <K extends keyof GenerateFormData>(field: K, value: GenerateFormData[K]) => {
    setFormData((previous) => ({ ...previous, [field]: value }))
    if (errors[field]) setErrors((previous) => ({ ...previous, [field]: undefined }))
  }

  const inputClass = (hasError: boolean) =>
    `w-full px-3 py-2 border rounded-lg bg-background focus:ring-2 focus:ring-primary focus:border-primary transition-colors ${
      hasError ? 'border-destructive' : 'border-input'
    }`

  return (
    <form onSubmit={handleSubmit} className="space-y-6">
      <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
        <div>
          <label htmlFor="redemption-name" className="block text-sm font-medium mb-1">
            兑换码名称 <span className="text-destructive">*</span>
          </label>
          <input
            id="redemption-name"
            type="text"
            value={formData.name}
            onChange={(event) => updateField('name', event.target.value)}
            placeholder="例如：客户补偿额度"
            className={inputClass(Boolean(errors.name))}
          />
          {errors.name ? <p className="mt-1 text-sm text-destructive">{errors.name}</p> : null}
        </div>

        <div>
          <label htmlFor="redemption-count" className="block text-sm font-medium mb-1">
            生成数量 <span className="text-destructive">*</span>
          </label>
          <input
            id="redemption-count"
            type="number"
            min={1}
            max={100}
            value={formData.count}
            onChange={(event) => updateField('count', Number.parseInt(event.target.value, 10) || 1)}
            className={inputClass(Boolean(errors.count))}
          />
          <p className="mt-1 text-xs text-muted-foreground">单次最多 100 个；为避免多批次部分成功，超出请拆分并逐批复核。</p>
          {errors.count ? <p className="mt-1 text-sm text-destructive">{errors.count}</p> : null}
        </div>
      </div>

      <div>
        <label htmlFor="redemption-reason" className="block text-sm font-medium mb-1">
          操作原因 <span className="text-destructive">*</span>
        </label>
        <textarea
          id="redemption-reason"
          value={formData.reason}
          onChange={(event) => updateField('reason', event.target.value)}
          placeholder="例如：工单 #1234，经复核向客户补偿额度"
          maxLength={1000}
          rows={3}
          className={inputClass(Boolean(errors.reason))}
        />
        <div className="mt-1 flex justify-between gap-3 text-xs text-muted-foreground">
          <span>原因会写入不可变操作审计。</span>
          <span>{formData.reason.length}/1000</span>
        </div>
        {errors.reason ? <p className="mt-1 text-sm text-destructive">{errors.reason}</p> : null}
      </div>

      <div className="rounded-lg border border-amber-500/30 bg-amber-500/10 p-3 text-sm text-muted-foreground">
        <div className="flex items-start gap-2">
          <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0 text-amber-600" />
          <div>
            <p className="font-medium text-foreground">NewAPI Admin API 安全模式</p>
            <p className="mt-1">仅支持固定金额，兑换码由 NewAPI 生成，不允许自定义 Key 前缀。</p>
          </div>
        </div>
      </div>

      <div>
        <label htmlFor="redemption-fixed-amount" className="block text-sm font-medium mb-1">
          固定额度 (USD) <span className="text-destructive">*</span>
        </label>
        <input
          id="redemption-fixed-amount"
          type="number"
          min={0.01}
          step={0.01}
          value={formData.fixed_amount}
          onChange={(event) => updateField('fixed_amount', Number.parseFloat(event.target.value) || 0)}
          className={inputClass(Boolean(errors.fixed_amount))}
        />
        <p className="mt-1 text-xs text-muted-foreground">1 USD = 500,000 Token</p>
        {errors.fixed_amount ? <p className="mt-1 text-sm text-destructive">{errors.fixed_amount}</p> : null}
      </div>

      <div>
        <label className="block text-sm font-medium mb-2">过期模式</label>
        <div className="flex flex-wrap gap-4 mb-3">
          {([['never', '永不过期'], ['days', '指定天数'], ['date', '指定日期']] as const).map(([mode, label]) => (
            <label key={mode} className="flex items-center gap-2 cursor-pointer">
              <input
                type="radio"
                name="expire_mode"
                value={mode}
                checked={formData.expire_mode === mode}
                onChange={() => updateField('expire_mode', mode)}
                className="text-primary focus:ring-primary"
              />
              <span className="text-sm">{label}</span>
            </label>
          ))}
        </div>

        {formData.expire_mode === 'days' ? (
          <div>
            <label htmlFor="redemption-expire-days" className="block text-sm font-medium mb-1">过期天数</label>
            <input
              id="redemption-expire-days"
              type="number"
              min={1}
              value={formData.expire_days}
              onChange={(event) => updateField('expire_days', Number.parseInt(event.target.value, 10) || 1)}
              className={inputClass(Boolean(errors.expire_days))}
            />
            {errors.expire_days ? <p className="mt-1 text-sm text-destructive">{errors.expire_days}</p> : null}
          </div>
        ) : null}

        {formData.expire_mode === 'date' ? (
          <div>
            <label htmlFor="redemption-expire-date" className="block text-sm font-medium mb-1">过期日期</label>
            <input
              id="redemption-expire-date"
              type="datetime-local"
              value={formData.expire_date}
              onChange={(event) => updateField('expire_date', event.target.value)}
              className={inputClass(Boolean(errors.expire_date))}
            />
            {errors.expire_date ? <p className="mt-1 text-sm text-destructive">{errors.expire_date}</p> : null}
          </div>
        ) : null}
      </div>

      <div className="pt-4">
        <Button type="submit" disabled={isLoading} className="w-full" size="lg">
          {isLoading ? (
            <>
              <Loader2 className="h-5 w-5 mr-2 animate-spin" />
              正在通过 NewAPI 创建...
            </>
          ) : (
            '创建兑换码'
          )}
        </Button>
      </div>
    </form>
  )
}
