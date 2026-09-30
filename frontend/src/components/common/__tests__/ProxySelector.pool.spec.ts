import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import ProxySelector from '../ProxySelector.vue'
import type { Proxy } from '@/types'

const testProxy = vi.hoisted(() => vi.fn())
const leaseFromPool = vi.hoisted(() => vi.fn())
const rotatePoolProxy = vi.hoisted(() => vi.fn())
vi.mock('@/api/admin', () => ({ adminAPI: { proxies: { testProxy, leaseFromPool, rotatePoolProxy } } }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
enableAutoUnmount(afterEach)
beforeEach(() => {
  vi.clearAllMocks()
})

const proxy = (id: number, managed = false) =>
  ({ id, name: `Proxy ${id}`, host: 'h', port: 8080, protocol: 'http', managed }) as Proxy

function mountSelector(props: Record<string, unknown>) {
  return mount(ProxySelector, {
    props: { modelValue: null, proxies: [proxy(1), proxy(2, true)], ...props },
    global: { stubs: { Icon: true } }
  })
}

const optionLabels = (wrapper: ReturnType<typeof mountSelector>) =>
  wrapper.findAll('.select-option').map((o) => o.text())

describe('proxy pool mode', () => {
  it('never offers a pool-managed proxy but still shows it when selected', async () => {
    const wrapper = mountSelector({ modelValue: 2 })
    expect(wrapper.get('.select-trigger').text()).toContain('Proxy 2')
    await wrapper.get('.select-trigger').trigger('click')
    const labels = optionLabels(wrapper)
    expect(labels.some((l) => l.includes('Proxy 1'))).toBe(true)
    expect(labels.some((l) => l.includes('Proxy 2'))).toBe(false)
  })

  it('offers the pool option only when allowPool is set', async () => {
    const plain = mountSelector({})
    await plain.get('.select-trigger').trigger('click')
    expect(plain.find('.pool-option').exists()).toBe(false)

    const pooled = mountSelector({ allowPool: true })
    await pooled.get('.select-trigger').trigger('click')
    expect(pooled.find('.pool-option').exists()).toBe(true)
  })

  it('leases once and selects the leased proxy', async () => {
    leaseFromPool.mockResolvedValue(proxy(7, true))
    const wrapper = mountSelector({ allowPool: true })
    await wrapper.get('.select-trigger').trigger('click')
    await wrapper.get('.pool-option').trigger('click')
    await flushPromises()
    expect(leaseFromPool).toHaveBeenCalledTimes(1)
    expect(wrapper.emitted('update:modelValue')).toEqual([[7]])
    expect(wrapper.emitted('leased')?.[0][0]).toMatchObject({ id: 7 })

    // The parent's list does not contain the new proxy yet; the trigger still names it.
    await wrapper.setProps({ modelValue: 7 })
    expect(wrapper.get('.select-trigger').text()).toContain('Proxy 7')
    expect(wrapper.find('.pool-rotate-btn').exists()).toBe(true)
  })

  it('does not lease again when the account already has a pool-managed proxy', async () => {
    const wrapper = mountSelector({ allowPool: true, modelValue: 2 })
    await wrapper.get('.select-trigger').trigger('click')
    await wrapper.get('.pool-option').trigger('click')
    await flushPromises()
    expect(leaseFromPool).not.toHaveBeenCalled()
    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
  })

  it('keeps the selection when leasing fails and shows why', async () => {
    leaseFromPool.mockRejectedValue({ status: 503, message: 'proxy pool has no free proxy' })
    const wrapper = mountSelector({ allowPool: true, modelValue: 1 })
    await wrapper.get('.select-trigger').trigger('click')
    await wrapper.get('.pool-option').trigger('click')
    await flushPromises()
    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
    expect(wrapper.get('.pool-status').text()).toContain('proxy pool has no free proxy')
  })

  it('rotates the selected pool-managed proxy', async () => {
    rotatePoolProxy.mockResolvedValue({ node: 'VPNGate-JP-2' })
    const wrapper = mountSelector({ allowPool: true, modelValue: 2 })
    await wrapper.get('.pool-rotate-btn').trigger('click')
    await flushPromises()
    expect(rotatePoolProxy).toHaveBeenCalledWith(2)
    expect(wrapper.get('.pool-status').text()).toContain('admin.proxies.pool.rotated')
  })

  it('shows no change-exit button for a hand-made proxy', () => {
    const wrapper = mountSelector({ allowPool: true, modelValue: 1 })
    expect(wrapper.find('.pool-rotate-btn').exists()).toBe(false)
  })
})
