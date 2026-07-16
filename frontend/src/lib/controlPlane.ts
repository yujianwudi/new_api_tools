import { apiFetch, createAuthHeaders } from './api'

export interface NewAPIStatus {
  version: string
  start_time: number
  system_name: string
  setup: boolean
  enable_data_export: boolean
}

export interface NewAPICapabilities {
  version: string
  known: boolean
  admin_user_manage: boolean
  redemption_api: boolean
  upstream_request_id: boolean
  subscription_billing: boolean
  clickhouse_logs: boolean
  hard_delete_safe: boolean
  unknown_version_read_only: boolean
  minimum_known_release: string
  hard_delete_minimum_known_safe: string
}

export interface NewAPICapabilityData {
  status: NewAPIStatus
  capabilities: NewAPICapabilities
  admin_credentials_configured: boolean
  write_mode: string
  checked_at: string
}

interface CapabilityResponse {
  success: boolean
  data?: NewAPICapabilityData
  message?: string
  error?: string | { message?: string }
}

function responseErrorMessage(body: CapabilityResponse, status: number): string {
  if (typeof body.error === 'string') return body.error
  return body.error?.message || body.message || `HTTP ${status}`
}

export async function fetchNewAPICapabilities({
  apiUrl,
  token,
  signal,
}: {
  apiUrl: string
  token: string | null
  signal?: AbortSignal
}): Promise<NewAPICapabilityData> {
  const response = await apiFetch(`${apiUrl}/api/control-plane/newapi/capabilities`, {
    headers: createAuthHeaders(token),
    signal,
  })
  const body = await response.json() as CapabilityResponse
  if (!response.ok || !body.success || !body.data) {
    throw new Error(responseErrorMessage(body, response.status))
  }
  return body.data
}

export function canSafelyHardDelete(capabilityData: NewAPICapabilityData | null): boolean {
  return capabilityData?.capabilities.known === true
    && capabilityData.capabilities.admin_user_manage === true
    && capabilityData.capabilities.hard_delete_safe === true
    && capabilityData.admin_credentials_configured === true
    && capabilityData.write_mode === 'admin_api'
}
