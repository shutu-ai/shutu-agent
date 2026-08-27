import { describe, expect, it } from 'vitest'
import type { ProviderView } from './api'
import { configuredModelIds, isSelectableProvider, selectConfiguredModel } from './model-selection'

const provider = (overrides: Partial<ProviderView> = {}): ProviderView => ({
  id: 'openai',
  available: true,
  configured: true,
  model: 'gpt-default',
  candidates: ['discovered-only'],
  ...overrides,
})

describe('session model selection', () => {
  it('requires both configured and available provider state', () => {
    expect(isSelectableProvider(provider())).toBe(true)
    expect(isSelectableProvider(provider({ configured: false }))).toBe(false)
    expect(isSelectableProvider(provider({ available: false }))).toBe(false)
    expect(configuredModelIds(provider({ configured: false }))).toEqual([])
  })

  it('uses configured models and ignores discovery candidates', () => {
    expect(configuredModelIds(provider({
      models: [{ id: 'gpt-4.1' }, { id: 'gpt-4.1' }, { id: '  ' }, { id: 'o3' }],
    }))).toEqual(['gpt-4.1', 'o3'])
  })

  it('falls back to the single configured model when the list is empty', () => {
    expect(configuredModelIds(provider())).toEqual(['gpt-default'])
    expect(configuredModelIds(provider({ models: [], model: '' }))).toEqual([])
  })

  it('normalizes a stale session model to the first configured model', () => {
    const current = provider({ models: [{ id: 'gpt-4.1' }, { id: 'o3' }] })
    expect(selectConfiguredModel(current, 'o3')).toBe('o3')
    expect(selectConfiguredModel(current, 'removed-model')).toBe('gpt-4.1')
    expect(selectConfiguredModel(provider({ configured: false }), 'anything')).toBe('')
  })
})
