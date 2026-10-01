import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import ProxyPoolSettingsModal from '../ProxyPoolSettingsModal.vue'

const getPoolConfig = vi.hoisted(() => vi.fn())
const updatePoolConfig = vi.hoisted(() => vi.fn())
vi.mock('@/api/admin', () => ({
  adminAPI: { proxies: { getPoolConfig, updatePoolConfig, getPoolHealth: vi.fn() } }
}))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showSuccess: vi.fn(), showError: vi.fn() }) }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (k: string) => k }) }))

function mountModal() {
  return mount(ProxyPoolSettingsModal, {
    props: { show: true },
    global: { stubs: { BaseDialog: { template: '<div><slot /><slot name="footer" /></div>' } } }
  })
}

describe('ProxyPoolSettingsModal share defaults', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    getPoolConfig.mockResolvedValue({
      url: '',
      url_source: 'env',
      token_source: 'env',
      token_configured: true,
      default_shareable: true,
      default_share_max: 3
    })
    updatePoolConfig.mockImplementation(async (p) => ({
      ...p,
      url_source: 'setting',
      token_source: 'env',
      token_configured: true
    }))
  })

  it('loads and saves the share defaults', async () => {
    const wrapper = mountModal()
    await flushPromises()
    expect((wrapper.get('.pool-default-shareable').element as HTMLInputElement).checked).toBe(true)
    expect((wrapper.get('.pool-default-share-max').element as HTMLInputElement).value).toBe('3')

    await wrapper.get('.pool-default-share-max').setValue('5')
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(updatePoolConfig).toHaveBeenCalledWith(
      expect.objectContaining({ default_shareable: true, default_share_max: 5 })
    )
  })

  it('never saves a negative default limit', async () => {
    const wrapper = mountModal()
    await flushPromises()
    await wrapper.get('.pool-default-share-max').setValue('-2')
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(updatePoolConfig).toHaveBeenCalledWith(expect.objectContaining({ default_share_max: 0 }))
  })
})
