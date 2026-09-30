<template>
  <BaseDialog
    :show="show"
    :title="t('admin.proxies.pool.settings')"
    width="normal"
    close-on-click-outside
    @close="emit('close')"
  >
    <form id="proxy-pool-settings-form" class="space-y-4" @submit.prevent="handleSave">
      <div class="text-sm text-gray-600 dark:text-dark-300">
        {{ t('admin.proxies.pool.settingsHint') }}
      </div>

      <div>
        <label class="input-label">{{ t('admin.proxies.pool.url') }}</label>
        <input
          v-model="url"
          type="text"
          class="input"
          :placeholder="t('admin.proxies.pool.urlPlaceholder')"
          autocomplete="off"
        />
        <p class="input-hint">{{ t('admin.proxies.pool.source.label') }}: {{ sourceText(config?.url_source) }}</p>
      </div>

      <div>
        <label class="input-label">{{ t('admin.proxies.pool.token') }}</label>
        <input
          v-model="token"
          type="password"
          class="input"
          :disabled="clearToken"
          :placeholder="
            config?.token_configured
              ? t('admin.proxies.pool.tokenPlaceholderKeep')
              : t('admin.proxies.pool.tokenPlaceholderNew')
          "
          autocomplete="new-password"
        />
        <p class="input-hint">{{ t('admin.proxies.pool.source.label') }}: {{ sourceText(config?.token_source) }}</p>
        <label v-if="config?.token_source === 'setting'" class="mt-2 flex items-center gap-2 text-sm">
          <input v-model="clearToken" type="checkbox" class="pool-clear-token" />
          {{ t('admin.proxies.pool.clearToken') }}
        </label>
      </div>

      <div v-if="healthText" :class="['text-sm', healthOk ? 'text-emerald-600' : 'text-red-600']">
        {{ healthText }}
      </div>
    </form>

    <template #footer>
      <div class="flex justify-between gap-3">
        <button class="btn btn-secondary pool-test-btn" type="button" :disabled="busy" @click="handleTest">
          {{ t('admin.proxies.pool.test') }}
        </button>
        <div class="flex gap-3">
          <button class="btn btn-secondary" type="button" :disabled="busy" @click="emit('close')">
            {{ t('common.cancel') }}
          </button>
          <button class="btn btn-primary" type="submit" form="proxy-pool-settings-form" :disabled="busy">
            {{ t('common.save') }}
          </button>
        </div>
      </div>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import { adminAPI } from '@/api/admin'
import type { ProxyPoolConfig } from '@/api/admin/proxies'
import { useAppStore } from '@/stores/app'

const props = defineProps<{ show: boolean }>()
const emit = defineEmits<{ close: [] }>()

const { t } = useI18n()
const appStore = useAppStore()

const config = ref<ProxyPoolConfig | null>(null)
const url = ref('')
const token = ref('')
const clearToken = ref(false)
const busy = ref(false)
const healthText = ref('')
const healthOk = ref(false)

const sourceText = (source?: string) =>
  source === 'setting' || source === 'env'
    ? t(`admin.proxies.pool.source.${source}`)
    : t('admin.proxies.pool.source.none')

const load = async () => {
  healthText.value = ''
  token.value = ''
  clearToken.value = false
  try {
    config.value = await adminAPI.proxies.getPoolConfig()
    // Only a saved URL is editable here; an env URL is shown as the placeholder source.
    url.value = config.value.url_source === 'setting' ? config.value.url : ''
  } catch (error: any) {
    appStore.showError(error?.message || t('admin.proxies.pool.saveFailed'))
  }
}

watch(
  () => props.show,
  (show) => {
    if (show) load()
  },
  { immediate: true }
)

const handleSave = async () => {
  busy.value = true
  try {
    const payload: { url: string; token?: string } = { url: url.value.trim() }
    // Omitted token keeps the saved one; '' removes it.
    if (clearToken.value) payload.token = ''
    else if (token.value.trim()) payload.token = token.value.trim()
    config.value = await adminAPI.proxies.updatePoolConfig(payload)
    token.value = ''
    clearToken.value = false
    appStore.showSuccess(t('admin.proxies.pool.saved'))
    emit('close')
  } catch (error: any) {
    appStore.showError(error?.message || t('admin.proxies.pool.saveFailed'))
  } finally {
    busy.value = false
  }
}

const handleTest = async () => {
  busy.value = true
  healthText.value = ''
  try {
    const health = await adminAPI.proxies.getPoolHealth()
    healthOk.value = true
    healthText.value = t('admin.proxies.pool.testOk', {
      leased: health.leased ?? '?',
      slots: health.slots ?? '?',
      nodes: health.nodes ?? '?'
    })
  } catch (error: any) {
    healthOk.value = false
    healthText.value = error?.message || t('admin.proxies.pool.testFailed')
  } finally {
    busy.value = false
  }
}
</script>
