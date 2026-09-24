<template>
  <div class="space-y-1.5">
    <div
      v-for="block in blocks"
      :key="block.pool"
      class="space-y-0.5"
      :data-test="`antigravity-pool-${block.pool}`"
    >
      <div
        v-for="row in block.rows"
        :key="row.key"
        class="flex items-center gap-1"
        :title="row.title"
        :data-test="`antigravity-pool-row-${row.key}`"
      >
        <span
          :class="[
            'max-w-[72px] shrink-0 truncate rounded px-1 text-left text-[10px] font-medium',
            block.labelClass
          ]"
        >
          {{ row.label }}
        </span>

        <div class="h-1.5 w-8 shrink-0 overflow-hidden rounded-full bg-gray-200 dark:bg-gray-700">
          <div
            :class="['h-full transition-all duration-300', barClass(row.used)]"
            :style="{ width: `${Math.min(Math.max(row.used, 0), 100)}%` }"
          ></div>
        </div>

        <span :class="['shrink-0 text-[10px] font-medium', textClass(row.used)]" data-test="used">
          {{ t('admin.accounts.antigravityPool.used', { value: formatPoolPercent(row.used) }) }}
        </span>
        <span class="shrink-0 text-[10px] text-gray-500 dark:text-gray-400" data-test="remaining">
          {{ t('admin.accounts.antigravityPool.remaining', { value: formatPoolPercent(row.remaining) }) }}
        </span>
        <span class="shrink-0 text-[10px] text-gray-400" data-test="reset">
          {{ row.resetText }}
        </span>
      </div>

      <div
        class="flex flex-wrap items-center gap-1 pl-1 text-[9px] text-gray-500 dark:text-gray-400"
        :title="t('admin.accounts.antigravityPool.localHint')"
        data-test="local-stats"
      >
        <span>{{ t('admin.accounts.antigravityPool.local') }}</span>
        <span
          v-for="stat in block.stats"
          :key="stat.key"
          class="rounded bg-gray-100 px-1.5 py-0.5 dark:bg-gray-800"
        >
          {{ stat.text }}
        </span>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, ref } from 'vue'
import { useIntervalFn } from '@vueuse/core'
import { useI18n } from 'vue-i18n'
import type { AntigravityPoolUsage, UsageProgress, WindowStats } from '@/types'
import { formatCompactNumber, formatShortDuration } from '@/utils/format'

const props = defineProps<{
  pools: AntigravityPoolUsage[]
}>()

const { t } = useI18n()

// 倒计时每分钟刷新一次
const now = ref(new Date())
useIntervalFn(() => {
  now.value = new Date()
}, 60_000)

// 百分比保留 1 位小数；上游给的是 7 位小数的比例，整数取整会把小额消耗抹成 0%。
function formatPoolPercent(value: number): string {
  if (!Number.isFinite(value) || value <= 0) return '0%'
  if (value >= 100) return '100%'
  if (value < 0.1) return '<0.1%'
  if (value > 99.9) return '>99.9%'
  return `${Number(value.toFixed(1))}%`
}

function barClass(used: number): string {
  if (used >= 90) return 'bg-red-500'
  if (used >= 75) return 'bg-amber-500'
  return 'bg-green-500'
}

function textClass(used: number): string {
  if (used >= 90) return 'text-red-600 dark:text-red-400'
  if (used >= 75) return 'text-amber-600 dark:text-amber-400'
  return 'text-gray-600 dark:text-gray-400'
}

function resetText(window: UsageProgress, used: number): string {
  // 未开始的窗口：上游把 resets_at 填成「取数时刻 + 窗口长度」，不是真的倒计时
  if (window.window_idle) return t('admin.accounts.antigravityPool.idle')
  if (!window.resets_at) return '-'
  const diffMs = new Date(window.resets_at).getTime() - now.value.getTime()
  if (diffMs <= 0) return used > 0 ? t('usage.resetPending') : t('usage.resetNow')
  return formatShortDuration(diffMs)
}

function rowTitle(window: UsageProgress): string {
  const lines: string[] = []
  if (window.upstream_note) lines.push(window.upstream_note)
  if (window.remaining_fraction != null) {
    lines.push(
      t('admin.accounts.antigravityPool.upstreamRemaining', {
        value: `${(window.remaining_fraction * 100).toFixed(4)}%`
      })
    )
  }
  if (window.window_idle) lines.push(t('admin.accounts.antigravityPool.idleHint'))
  return lines.join('\n')
}

function statText(label: string, stats: WindowStats | null | undefined): string {
  if (!stats || (stats.requests <= 0 && stats.tokens <= 0)) return `${label} 0 req`
  const parts = [
    `${label} ${formatCompactNumber(stats.requests, { allowBillions: false })} req`,
    formatCompactNumber(stats.tokens),
    `A $${stats.cost.toFixed(2)}`
  ]
  if (stats.user_cost != null) parts.push(`U $${stats.user_cost.toFixed(2)}`)
  return parts.join(' · ')
}

const blocks = computed(() =>
  props.pools.map((pool) => {
    const isGemini = pool.pool === 'gemini'
    const poolLabel = isGemini
      ? t('admin.accounts.antigravityPool.gemini')
      : t('admin.accounts.antigravityPool.claudeGpt')

    // 周在前、5h 在后，与 Antigravity 客户端设置页的行序一致。
    // 上游没下发的窗口（null/undefined）整行不渲染——不要补一行 0%。
    const windows: Array<{ key: string; label: string; progress: UsageProgress }> = []
    if (pool.weekly) {
      windows.push({ key: 'weekly', label: t('admin.accounts.antigravityPool.weekly'), progress: pool.weekly })
    }
    if (pool.five_hour) {
      windows.push({ key: '5h', label: t('admin.accounts.antigravityPool.fiveHour'), progress: pool.five_hour })
    }

    return {
      pool: pool.pool,
      labelClass: isGemini
        ? 'bg-indigo-100 text-indigo-700 dark:bg-indigo-900/40 dark:text-indigo-300'
        : 'bg-amber-100 text-amber-700 dark:bg-amber-900/40 dark:text-amber-300',
      rows: windows.map(({ key, label, progress }) => {
        const used = progress.utilization
        const remaining =
          progress.remaining_fraction != null ? progress.remaining_fraction * 100 : 100 - used
        return {
          key: `${pool.pool}-${key}`,
          label: `${poolLabel} ${label}`,
          used,
          remaining,
          resetText: resetText(progress, used),
          title: rowTitle(progress)
        }
      }),
      // 每个池各自一行本地统计，0 次也显示，两个池分开计量一眼可见
      stats: windows.map(({ key, label, progress }) => ({
        key,
        text: statText(label, progress.window_stats)
      }))
    }
  })
)
</script>
