/**
 * Admin Proxies API endpoints
 * Handles proxy server management for administrators
 */

import { apiClient } from '../client'
import type {
  Proxy,
  ProxyAccountSummary,
  ProxyQualityCheckResult,
  CreateProxyRequest,
  UpdateProxyRequest,
  PaginatedResponse,
  AdminDataPayload,
  AdminDataImportResult
} from '@/types'

function assertProxyArray(value: unknown): asserts value is Proxy[] {
  if (!Array.isArray(value)) {
    throw new Error('Invalid proxy list response')
  }
}

/**
 * List all proxies with pagination
 * @param page - Page number (default: 1)
 * @param pageSize - Items per page (default: 20)
 * @param filters - Optional filters
 * @returns Paginated list of proxies
 */
export async function list(
  page: number = 1,
  pageSize: number = 20,
  filters?: {
    protocol?: string
    status?: 'active' | 'inactive' | 'expired'
    search?: string
    sort_by?: string
    sort_order?: 'asc' | 'desc'
  },
  options?: {
    signal?: AbortSignal
  }
): Promise<PaginatedResponse<Proxy>> {
  const { data } = await apiClient.get<PaginatedResponse<Proxy>>('/admin/proxies', {
    params: {
      page,
      page_size: pageSize,
      ...filters
    },
    signal: options?.signal
  })
  assertProxyArray(data?.items)
  return data
}

/**
 * Get all active proxies (without pagination)
 * @returns List of all active proxies
 */
export async function getAll(): Promise<Proxy[]> {
  const { data } = await apiClient.get<Proxy[]>('/admin/proxies/all')
  assertProxyArray(data)
  return data
}

/**
 * Get all active proxies with account count (sorted by creation time desc)
 * @returns List of all active proxies with account count
 */
export async function getAllWithCount(): Promise<Proxy[]> {
  const { data } = await apiClient.get<Proxy[]>('/admin/proxies/all', {
    params: { with_count: 'true' }
  })
  assertProxyArray(data)
  return data
}

/**
 * Get proxy by ID
 * @param id - Proxy ID
 * @returns Proxy details
 */
export async function getById(id: number): Promise<Proxy> {
  const { data } = await apiClient.get<Proxy>(`/admin/proxies/${id}`)
  return data
}

/**
 * Create new proxy
 * @param proxyData - Proxy data
 * @returns Created proxy
 */
export async function create(proxyData: CreateProxyRequest): Promise<Proxy> {
  const { data } = await apiClient.post<Proxy>('/admin/proxies', proxyData)
  return data
}

/**
 * Update proxy
 * @param id - Proxy ID
 * @param updates - Fields to update
 * @returns Updated proxy
 */
export async function update(id: number, updates: UpdateProxyRequest): Promise<Proxy> {
  const { data } = await apiClient.put<Proxy>(`/admin/proxies/${id}`, updates)
  return data
}

/**
 * Delete proxy
 * @param id - Proxy ID
 * @returns Success confirmation
 */
export async function deleteProxy(id: number): Promise<{ message: string }> {
  const { data } = await apiClient.delete<{ message: string }>(`/admin/proxies/${id}`)
  return data
}

/**
 * Toggle proxy status
 * @param id - Proxy ID
 * @param status - New status
 * @returns Updated proxy
 */
export async function toggleStatus(id: number, status: 'active' | 'inactive'): Promise<Proxy> {
  return update(id, { status })
}

/**
 * Test proxy connectivity
 * @param id - Proxy ID
 * @returns Test result with IP info
 */
export async function testProxy(id: number): Promise<{
  success: boolean
  message: string
  latency_ms?: number
  ip_address?: string
  city?: string
  region?: string
  country?: string
  country_code?: string
}> {
  const { data } = await apiClient.post<{
    success: boolean
    message: string
    latency_ms?: number
    ip_address?: string
    city?: string
    region?: string
    country?: string
    country_code?: string
  }>(`/admin/proxies/${id}/test`)
  return data
}

/**
 * Check proxy quality across common AI targets
 * @param id - Proxy ID
 * @returns Quality check result
 */
export async function checkProxyQuality(id: number): Promise<ProxyQualityCheckResult> {
  const { data } = await apiClient.post<ProxyQualityCheckResult>(`/admin/proxies/${id}/quality-check`)
  return data
}

/**
 * Get proxy usage statistics
 * @param id - Proxy ID
 * @returns Proxy usage statistics
 */
export async function getStats(id: number): Promise<{
  total_accounts: number
  active_accounts: number
  total_requests: number
  success_rate: number
  average_latency: number
}> {
  const { data } = await apiClient.get<{
    total_accounts: number
    active_accounts: number
    total_requests: number
    success_rate: number
    average_latency: number
  }>(`/admin/proxies/${id}/stats`)
  return data
}

/**
 * Get accounts using a proxy
 * @param id - Proxy ID
 * @returns List of accounts using the proxy
 */
export async function getProxyAccounts(id: number): Promise<ProxyAccountSummary[]> {
  const { data } = await apiClient.get<ProxyAccountSummary[]>(`/admin/proxies/${id}/accounts`)
  return data
}

/**
 * Batch create proxies
 * @param proxies - Array of proxy data to create
 * @returns Creation result with count of created and skipped
 */
export async function batchCreate(
  proxies: Array<{
    protocol: string
    host: string
    port: number
    username?: string
    password?: string
  }>
): Promise<{
  created: number
  skipped: number
}> {
  const { data } = await apiClient.post<{
    created: number
    skipped: number
  }>('/admin/proxies/batch', { proxies })
  return data
}

export async function batchDelete(ids: number[]): Promise<{
  deleted_ids: number[]
  skipped: Array<{ id: number; reason: string }>
}> {
  const { data } = await apiClient.post<{
    deleted_ids: number[]
    skipped: Array<{ id: number; reason: string }>
  }>('/admin/proxies/batch-delete', { ids })
  return data
}

export async function exportData(options?: {
  ids?: number[]
  filters?: {
    protocol?: string
    status?: 'active' | 'inactive' | 'expired'
    search?: string
    sort_by?: string
    sort_order?: 'asc' | 'desc'
  }
}): Promise<AdminDataPayload> {
  const params: Record<string, string> = {}
  if (options?.ids && options.ids.length > 0) {
    params.ids = options.ids.join(',')
  } else if (options?.filters) {
    const { protocol, status, search, sort_by, sort_order } = options.filters
    if (protocol) params.protocol = protocol
    if (status) params.status = status
    if (search) params.search = search
    if (sort_by) params.sort_by = sort_by
    if (sort_order) params.sort_order = sort_order
  }
  const { data } = await apiClient.get<AdminDataPayload>('/admin/proxies/data', { params })
  return data
}

export async function importData(payload: {
  data: AdminDataPayload
}): Promise<AdminDataImportResult> {
  const { data } = await apiClient.post<AdminDataImportResult>('/admin/proxies/data', payload)
  return data
}

/** Proxy pool provider settings; the token itself is never returned. */
export interface ProxyPoolConfig {
  url: string
  url_source: 'setting' | 'env' | ''
  token_source: 'setting' | 'env' | ''
  token_configured: boolean
  /** Share settings a newly leased slot starts with; max 0 = no limit. */
  default_shareable: boolean
  default_share_max: number
}

export async function getPoolConfig(): Promise<ProxyPoolConfig> {
  const { data } = await apiClient.get<ProxyPoolConfig>('/admin/proxies/pool/config')
  return data
}

/**
 * Save provider settings. url '' removes the saved URL (the environment
 * variable applies again). Omit token to keep the saved one; '' removes it.
 */
export async function updatePoolConfig(payload: {
  url: string
  token?: string
  default_shareable?: boolean
  default_share_max?: number
}): Promise<ProxyPoolConfig> {
  const { data } = await apiClient.put<ProxyPoolConfig>('/admin/proxies/pool/config', payload)
  return data
}

export async function getPoolHealth(): Promise<Record<string, unknown>> {
  const { data } = await apiClient.get<Record<string, unknown>>('/admin/proxies/pool/health')
  return data
}

/** A pool proxy given to an account: a new slot, or a shared one (shared=true). */
export type PoolAllocation = Proxy & { shared: boolean }

/** Get a pool proxy for one account (use its id as proxy_id). */
export async function leaseFromPool(): Promise<PoolAllocation> {
  const { data } = await apiClient.post<PoolAllocation>('/admin/proxies/pool/lease')
  return data
}

/** A shared pool slot that still has room. max 0 = no limit. */
export interface PoolShareCandidate {
  proxy: Proxy
  used: number
  max: number
}

export async function listShareablePoolProxies(): Promise<PoolShareCandidate[]> {
  const { data } = await apiClient.get<PoolShareCandidate[]>('/admin/proxies/pool/shareable')
  return data
}

/** Move a pool-managed proxy to another exit node; returns the new node. */
export async function rotatePoolProxy(id: number): Promise<{ node: string }> {
  const { data } = await apiClient.post<{ node: string }>(`/admin/proxies/${id}/pool/rotate`)
  return data
}

export const proxiesAPI = {
  list,
  getAll,
  getAllWithCount,
  getById,
  create,
  update,
  delete: deleteProxy,
  toggleStatus,
  testProxy,
  checkProxyQuality,
  getStats,
  getProxyAccounts,
  batchCreate,
  batchDelete,
  exportData,
  importData,
  getPoolConfig,
  updatePoolConfig,
  getPoolHealth,
  leaseFromPool,
  listShareablePoolProxies,
  rotatePoolProxy
}

export default proxiesAPI
