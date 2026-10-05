import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { defineComponent } from 'vue'

import AccountsView from '../AccountsView.vue'

const { listAccounts, listWithEtag, getById, getBatchTodayStats, getUpstreamBillingProbeSettings, getAllProxies, getAllGroups } = vi.hoisted(() => ({
  listAccounts: vi.fn(),
  listWithEtag: vi.fn(),
  getById: vi.fn(),
  getBatchTodayStats: vi.fn(),
  getUpstreamBillingProbeSettings: vi.fn(),
  getAllProxies: vi.fn(),
  getAllGroups: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      list: listAccounts,
      getById,
      listWithEtag,
      getBatchTodayStats,
      getUpstreamBillingProbeSettings,
      delete: vi.fn(),
      batchClearError: vi.fn(),
      batchRefresh: vi.fn(),
      toggleSchedulable: vi.fn()
    },
    proxies: { getAll: getAllProxies },
    groups: { getAll: getAllGroups }
  }
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError: vi.fn(), showWarning: vi.fn(), showSuccess: vi.fn(), showInfo: vi.fn() })
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({ token: 'test-token', isSimpleMode: false })
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

const DataTableStub = defineComponent({
  props: { data: { type: Array, default: () => [] } },
  template: `
    <div>
      <div v-for="row in data" :key="row.id" :data-account-name="row.name">
        <div data-test="proxy-cell"><slot name="cell-proxy" :row="row" /></div>
        <span data-test="proxy-health-state">{{ row.proxy_health?.latency_status ?? '' }}</span>
        <slot name="cell-actions" :row="row" />
      </div>
    </div>
  `
})

const HelpTooltipStub = defineComponent({
  props: { content: { type: String, default: '' } },
  template: '<span data-test="proxy-health-tooltip" :title="content"><slot /><slot name="trigger" /></span>'
})

const EditAccountModalStub = defineComponent({
  name: 'EditAccountModal',
  props: { show: Boolean, account: { type: Object, default: null }, proxies: { type: Array, default: () => [] } },
  emits: ['updated'],
  template: '<div data-test="edit-account">{{ show ? account?.name : "" }}</div>'
})

const listRow = {
  id: 42,
  name: 'proxy health account',
  platform: 'openai',
  type: 'oauth',
  status: 'active',
  schedulable: true,
  concurrency: 2,
  priority: 1,
  proxy_id: 91,
  proxy: { id: 91, name: 'test proxy' },
  group_ids: [7],
  extra: {},
  credentials: {}
}

const failedHealth = {
  latency_status: 'failed',
  latency_ms: null,
  latency_message: 'connection refused',
  checked_at: 1791201600
}

function mountView() {
  return mount(AccountsView, {
    attachTo: document.body,
    global: {
      stubs: {
        AppLayout: { template: '<div><slot /></div>' },
        TablePageLayout: { template: '<div><slot name="filters" /><slot name="table" /><slot name="pagination" /></div>' },
        DataTable: DataTableStub,
        AccountTableActions: { template: '<div><slot name="after" /></div>' },
        AccountTableFilters: true,
        AccountBulkActionsBar: true,
        Pagination: true,
        ConfirmDialog: true,
        AccountActionMenu: true,
        ImportDataModal: true,
        ReAuthAccountModal: true,
        AccountTestModal: true,
        AccountStatsModal: true,
        ScheduledTestsPanel: true,
        SyncFromCrsModal: true,
        TempUnschedStatusModal: true,
        ErrorPassthroughRulesModal: true,
        TLSFingerprintProfilesModal: true,
        CreateAccountModal: true,
        EditAccountModal: EditAccountModalStub,
        BulkEditAccountModal: true,
        PlatformTypeBadge: true,
        AccountCapacityCell: true,
        AccountStatusIndicator: true,
        AccountTodayStatsCell: true,
        AccountGroupsCell: true,
        AccountUsageCell: true,
        UpstreamBillingRateCell: true,
        HelpTooltip: HelpTooltipStub,
        Icon: true
      }
    }
  })
}

describe('admin AccountsView proxy health', () => {
  beforeEach(() => {
    localStorage.clear()
    listAccounts.mockReset().mockResolvedValue({ items: [{ ...listRow }], total: 1, page: 1, page_size: 20, pages: 1 })
    listWithEtag.mockReset().mockResolvedValue({ notModified: true, etag: 'proxy-health-etag', data: null })
    getById.mockReset().mockResolvedValue({ ...listRow })
    getBatchTodayStats.mockReset().mockResolvedValue({ stats: {} })
    getUpstreamBillingProbeSettings.mockReset().mockResolvedValue({ enabled: true })
    getAllProxies.mockReset().mockResolvedValue([listRow.proxy])
    getAllGroups.mockReset().mockResolvedValue([{ id: 7, name: 'codex', platform: 'openai' }])
  })

  afterEach(() => {
    vi.useRealTimers()
    vi.restoreAllMocks()
  })

  it('renders the failed proxy status and its tooltip through the proxy cell slot', async () => {
    listAccounts.mockResolvedValueOnce({
      items: [{ ...listRow, proxy_health: failedHealth }],
      total: 1,
      page: 1,
      page_size: 20,
      pages: 1
    })
    const wrapper = mountView()
    await flushPromises()

    expect(wrapper.get('[data-test="proxy-health-state"]').text()).toBe('failed')
    expect(wrapper.get('[data-test="proxy-cell"]').text()).toContain('admin.accounts.proxyHealth.connectionFailed')
    expect(wrapper.get('[data-test="proxy-health-tooltip"]').attributes('title')).toContain('connection refused')
    wrapper.unmount()
  })

  it('replaces a row when only proxy health changes in an ETag refresh', async () => {
    vi.useFakeTimers()
    vi.spyOn(document, 'hidden', 'get').mockReturnValue(false)
    localStorage.setItem('account-auto-refresh', JSON.stringify({ enabled: true, interval_seconds: 5 }))
    listAccounts.mockResolvedValueOnce({
      items: [{ ...listRow, proxy_health: failedHealth }],
      total: 1,
      page: 1,
      page_size: 20,
      pages: 1
    })
    listWithEtag.mockResolvedValueOnce({
      notModified: false,
      etag: 'proxy-health-etag-updated',
      data: {
        items: [{ ...listRow, proxy_health: { ...failedHealth, latency_status: 'success', latency_ms: 125 } }],
        total: 1,
        page: 1,
        page_size: 20,
        pages: 1
      }
    })
    const wrapper = mountView()
    await flushPromises()
    expect(wrapper.get('[data-test="proxy-health-state"]').text()).toBe('failed')

    await vi.advanceTimersByTimeAsync(6000)
    await flushPromises()

    expect(wrapper.get('[data-test="proxy-health-state"]').text()).toBe('success')
    wrapper.unmount()
  })

  it('preserves the previous health when an edit keeps the same proxy binding', async () => {
    listAccounts.mockResolvedValueOnce({
      items: [{ ...listRow, proxy_health: failedHealth }],
      total: 1,
      page: 1,
      page_size: 20,
      pages: 1
    })
    getById.mockResolvedValueOnce({ ...listRow })
    const wrapper = mountView()
    await flushPromises()

    const editButton = wrapper.findAll('button').find(button => button.text().includes('common.edit'))
    expect(editButton).toBeTruthy()
    await editButton!.trigger('click')
    await flushPromises()
    const editModal = wrapper.findComponent(EditAccountModalStub)
    expect(editModal.props('show')).toBe(true)
    expect(editModal.props('account')).toMatchObject({ id: 42 })
    editModal.vm.$emit('updated', { ...listRow, name: 'edited account' })
    await flushPromises()

    expect(wrapper.get('[data-test="proxy-health-state"]').text()).toBe('failed')
    wrapper.unmount()
  })

  it('drops previous health when an update changes the proxy binding', async () => {
    listAccounts.mockResolvedValueOnce({
      items: [{ ...listRow, proxy_health: failedHealth }],
      total: 1,
      page: 1,
      page_size: 20,
      pages: 1
    })
    const wrapper = mountView()
    await flushPromises()

    const editButton = wrapper.findAll('button').find(button => button.text().includes('common.edit'))
    expect(editButton).toBeTruthy()
    await editButton!.trigger('click')
    await flushPromises()
    const editModal = wrapper.findComponent(EditAccountModalStub)
    expect(editModal.props('show')).toBe(true)
    expect(editModal.props('account')).toMatchObject({ id: 42 })
    editModal.vm.$emit('updated', { ...listRow, proxy_id: 92, name: 'rebound account' })
    await flushPromises()

    expect(wrapper.get('[data-test="proxy-health-state"]').text()).toBe('')
    wrapper.unmount()
  })

  it('shows not checked for a bound proxy when health has no checked_at', async () => {
    listAccounts.mockResolvedValueOnce({
      items: [{ ...listRow, proxy_health: { latency_status: 'failed' } }],
      total: 1,
      page: 1,
      page_size: 20,
      pages: 1
    })
    const wrapper = mountView()
    await flushPromises()

    expect(wrapper.get('[data-test="proxy-health-state"]').text()).toBe('failed')
    expect(wrapper.get('[data-test="proxy-cell"]').text()).toContain('admin.accounts.proxyHealth.notChecked')
    wrapper.unmount()
  })

  it('shows zero minutes ago for a fresh health check', async () => {
    listAccounts.mockResolvedValueOnce({
      items: [{ ...listRow, proxy_health: { latency_status: 'success', checked_at: Math.floor(Date.now() / 1000) } }],
      total: 1,
      page: 1,
      page_size: 20,
      pages: 1
    })
    const wrapper = mountView()
    await flushPromises()

    expect(wrapper.get('[data-test="proxy-cell"]').text()).toContain('admin.accounts.proxyHealth.minutesAgo')
    wrapper.unmount()
  })

  it('shows not checked for a bound proxy without health data', async () => {
    const wrapper = mountView()
    await flushPromises()

    expect(wrapper.get('[data-test="proxy-health-state"]').text()).toBe('')
    expect(wrapper.get('[data-test="proxy-cell"]').text()).toContain('admin.accounts.proxyHealth.notChecked')
    wrapper.unmount()
  })
})
