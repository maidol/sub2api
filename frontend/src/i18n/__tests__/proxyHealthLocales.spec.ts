import { describe, expect, it } from 'vitest'

import enAccounts from '../locales/en/admin/accounts'
import zhAccounts from '../locales/zh/admin/accounts'

const requiredProxyHealthKeys = [
  'label',
  'proxyDisabled',
  'connectionFailed',
  'qualityWarn',
  'qualityAbnormal',
  'notChecked',
  'tooltip',
  'minutesAgo',
  'hoursAgo',
  'daysAgo'
] as const

describe('account proxy health locales', () => {
  it.each([
    ['English', enAccounts.accounts.proxyHealth],
    ['Chinese', zhAccounts.accounts.proxyHealth]
  ])('%s defines the proxy health labels and tooltips', (_locale, messages) => {
    expect(messages).toBeDefined()
    expect(messages).toEqual(
      expect.objectContaining(
        Object.fromEntries(requiredProxyHealthKeys.map(key => [key, expect.any(String)]))
      )
    )
    for (const key of requiredProxyHealthKeys) {
      expect((messages[key] as string).trim()).not.toBe('')
    }
  })
})
