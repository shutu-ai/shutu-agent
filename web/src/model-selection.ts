import type { ProviderView } from './api'

/**
 * The settings directory may contain dormant providers and discovery
 * candidates. The session composer must only expose a provider that can be
 * used right now.
 */
export function isSelectableProvider(provider: ProviderView | undefined): provider is ProviderView {
  return provider?.configured === true && provider.available === true
}

/**
 * Return the configured model list for the composer.
 *
 * `candidates` is intentionally excluded: it is a discovery/catalog field for
 * settings, not a promise that the model is configured for this provider.
 * An empty `models` list keeps the single configured `model` fallback used by
 * the legacy single-model provider profile.
 */
export function configuredModelIds(provider: ProviderView | undefined): string[] {
  if (!isSelectableProvider(provider)) return []
  const modelIds = (provider.models ?? [])
    .map(model => model.id.trim())
    .filter(model => model !== '')
  const uniqueModelIds = [...new Set(modelIds)]
  if (uniqueModelIds.length > 0) return uniqueModelIds
  const fallback = provider.model?.trim() ?? ''
  return fallback === '' ? [] : [fallback]
}

export function selectConfiguredModel(provider: ProviderView | undefined, requested: string): string {
  const models = configuredModelIds(provider)
  return models.includes(requested) ? requested : (models[0] ?? '')
}
