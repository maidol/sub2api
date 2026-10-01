import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, shallowMount } from '@vue/test-utils'
import ProxiesView from '../ProxiesView.vue'
import Select from '@/components/common/Select.vue'

const { list, update, getAllWithCount } = vi.hoisted(() => ({ list: vi.fn(), update: vi.fn(), getAllWithCount: vi.fn() }))
vi.mock('@/api/admin', () => ({ adminAPI: { proxies: { list, update, getAllWithCount } } }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showError: vi.fn(), showSuccess: vi.fn() }) }))
vi.mock('vue-i18n', async () => ({
  ...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'),
  useI18n: () => ({ t: (key: string) => key }),
}))
const mountView = () => shallowMount(ProxiesView, {
  global: { stubs: {
    AppLayout: { template: '<div><slot /></div>' },
    TablePageLayout: { template: '<div><slot name="table" /></div>' },
    DataTable: { props: ['data'], template: '<div v-for="row in data" :key="row.id"><slot name="cell-actions" :row="row" /></div>' },
    BaseDialog: { props: ['show'], template: '<div v-if="show"><slot /><slot name="footer" /></div>' },
  } },
})
let wrapper: ReturnType<typeof mountView>
beforeEach(() => {
  vi.clearAllMocks()
  list.mockResolvedValue({ items: [{ id: 9, name: 'proxy', protocol: 'http', host: 'proxy.example', port: 8080, status: 'active', fallback_mode: 'proxy', backup_proxy_id: null }], total: 1, pages: 1 })
  getAllWithCount.mockResolvedValue([
    { id: 9, name: 'proxy', protocol: 'http', host: 'proxy.example', port: 8080, status: 'active' },
    { id: 11, name: 'hand-made', protocol: 'http', host: 'hand.example', port: 8080, status: 'active' },
    { id: 12, name: 'Proxy pool · slot01', protocol: 'http', host: 'vpngate', port: 20001, status: 'active', managed: true },
  ])
})
afterEach(() => wrapper?.unmount())

describe('backup proxy options', () => {
  it('never offers a pool-managed proxy as a backup', async () => {
    wrapper = mountView(); await flushPromises()
    await wrapper.findAll('button').find(button => button.text() === 'common.edit')!.trigger('click')
    const values = wrapper.findAllComponents(Select)
      .flatMap(select => ((select.props('options') ?? []) as Array<{ value: unknown }>).map(option => option.value))
    expect(values).toContain(11)
    expect(values).not.toContain(9)
    expect(values).not.toContain(12)
  })
})
