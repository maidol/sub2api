import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import AntigravityPoolUsage from '../AntigravityPoolUsage.vue'
import type { AntigravityPoolUsage as Pool } from '@/types'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, unknown>) =>
        params?.value != null ? `${key}=${params.value}` : key
    })
  }
})

const NOW = new Date('2026-09-24T08:00:00Z')

function mountPools(pools: Pool[]) {
  return mount(AntigravityPoolUsage, { props: { pools } })
}

function row(wrapper: ReturnType<typeof mountPools>, key: string) {
  return wrapper.find(`[data-test="antigravity-pool-row-${key}"]`)
}

describe('AntigravityPoolUsage', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    vi.setSystemTime(NOW)
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('每行同时显示已用与剩余（1 位小数），倒计时来自 resets_at', () => {
    const wrapper = mountPools([
      {
        pool: 'claude_gpt',
        weekly: {
          utilization: (1 - 0.41361254) * 100,
          remaining_fraction: 0.41361254,
          resets_at: '2026-09-27T14:00:00Z',
          remaining_seconds: 0,
          upstream_note: 'You have used some of your weekly limit, it will fully refresh in 3 days, 6 hours.'
        }
      }
    ])

    const weekly = row(wrapper, 'claude_gpt-weekly')
    expect(weekly.find('[data-test="used"]').text()).toBe('admin.accounts.antigravityPool.used=58.6%')
    expect(weekly.find('[data-test="remaining"]').text()).toBe('admin.accounts.antigravityPool.remaining=41.4%')
    expect(weekly.find('[data-test="reset"]').text()).toBe('3d 6h')
    // 悬停可核对上游原文与原始比例
    expect(weekly.attributes('title')).toContain('it will fully refresh in 3 days, 6 hours.')
    expect(weekly.attributes('title')).toContain('admin.accounts.antigravityPool.upstreamRemaining=41.3613%')
  })

  it('小额消耗不被抹成 0%：<0.1% 与 >99.9%', () => {
    const wrapper = mountPools([
      {
        pool: 'claude_gpt',
        weekly: { utilization: 0.04, remaining_fraction: 0.9996, resets_at: '2026-09-27T14:00:00Z', remaining_seconds: 0 },
        five_hour: { utilization: 0.4, remaining_fraction: 0.996, resets_at: '2026-09-24T10:00:00Z', remaining_seconds: 0 }
      }
    ])

    const weekly = row(wrapper, 'claude_gpt-weekly')
    expect(weekly.find('[data-test="used"]').text()).toBe('admin.accounts.antigravityPool.used=<0.1%')
    expect(weekly.find('[data-test="remaining"]').text()).toBe('admin.accounts.antigravityPool.remaining=>99.9%')
    const fiveHour = row(wrapper, 'claude_gpt-5h')
    expect(fiveHour.find('[data-test="used"]').text()).toBe('admin.accounts.antigravityPool.used=0.4%')
    expect(fiveHour.find('[data-test="remaining"]').text()).toBe('admin.accounts.antigravityPool.remaining=99.6%')
  })

  it('未开始的窗口显示「未开始」，不显示假的满格倒计时', () => {
    const wrapper = mountPools([
      {
        pool: 'gemini',
        weekly: {
          utilization: 0,
          remaining_fraction: 1,
          resets_at: '2026-10-01T07:59:00Z',
          remaining_seconds: 0,
          window_idle: true
        }
      }
    ])

    const weekly = row(wrapper, 'gemini-weekly')
    expect(weekly.find('[data-test="used"]').text()).toBe('admin.accounts.antigravityPool.used=0%')
    expect(weekly.find('[data-test="remaining"]').text()).toBe('admin.accounts.antigravityPool.remaining=100%')
    expect(weekly.find('[data-test="reset"]').text()).toBe('admin.accounts.antigravityPool.idle')
    expect(weekly.text()).not.toContain('6d 23h')
    expect(weekly.attributes('title')).toContain('admin.accounts.antigravityPool.idleHint')
  })

  it('两池各自一行本地统计，0 次也显示；周在前、5h 在后', () => {
    const wrapper = mountPools([
      {
        pool: 'gemini',
        weekly: { utilization: 0, resets_at: null, remaining_seconds: 0, window_idle: true,
          window_stats: { requests: 0, tokens: 0, cost: 0, standard_cost: 0, user_cost: 0 } },
        five_hour: { utilization: 0, resets_at: null, remaining_seconds: 0, window_idle: true }
      },
      {
        pool: 'claude_gpt',
        weekly: { utilization: 0.4, resets_at: '2026-09-27T14:00:00Z', remaining_seconds: 0,
          window_stats: { requests: 14, tokens: 5600, cost: 0.02, standard_cost: 0.02, user_cost: 0.03 } },
        five_hour: { utilization: 0, resets_at: null, remaining_seconds: 0, window_idle: true,
          window_stats: { requests: 0, tokens: 0, cost: 0, standard_cost: 0, user_cost: 0 } }
      }
    ])

    const blocks = wrapper.findAll('[data-test="antigravity-pool-gemini"], [data-test="antigravity-pool-claude_gpt"]')
    expect(blocks.map((b) => b.attributes('data-test'))).toEqual([
      'antigravity-pool-gemini',
      'antigravity-pool-claude_gpt'
    ])

    const gemini = wrapper.find('[data-test="antigravity-pool-gemini"]')
    const geminiRows = gemini.findAll('[data-test^="antigravity-pool-row-"]')
    expect(geminiRows.map((r) => r.attributes('data-test'))).toEqual([
      'antigravity-pool-row-gemini-weekly',
      'antigravity-pool-row-gemini-5h'
    ])
    expect(gemini.find('[data-test="local-stats"]').text()).toContain('admin.accounts.antigravityPool.weekly 0 req')
    expect(gemini.find('[data-test="local-stats"]').text()).toContain('admin.accounts.antigravityPool.fiveHour 0 req')

    const claude = wrapper.find('[data-test="antigravity-pool-claude_gpt"]')
    const claudeStats = claude.find('[data-test="local-stats"]').text()
    expect(claudeStats).toContain('admin.accounts.antigravityPool.weekly 14 req · 5.6K · A $0.02 · U $0.03')
    expect(claudeStats).toContain('admin.accounts.antigravityPool.fiveHour 0 req')
    // Claude 的请求不会出现在 Gemini 那一块
    expect(gemini.text()).not.toContain('14 req')
  })

  it('上游没下发的窗口不渲染，也不补一行 0%', () => {
    const wrapper = mountPools([
      {
        pool: 'gemini',
        weekly: null,
        five_hour: { utilization: 4, remaining_fraction: 0.96, resets_at: '2026-09-24T10:00:00Z', remaining_seconds: 0 }
      }
    ])

    const rows = wrapper.findAll('[data-test^="antigravity-pool-row-"]')
    expect(rows.map((r) => r.attributes('data-test'))).toEqual(['antigravity-pool-row-gemini-5h'])
    expect(wrapper.text()).not.toContain('used=0%')
  })
})
