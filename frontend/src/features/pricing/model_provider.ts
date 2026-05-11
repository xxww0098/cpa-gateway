import modelRegistry from '@/data/cliproxy-models-registry.json'
import customModelRegistry from '@/data/cpa-models-registry.json'

export interface ProviderAwareModel {
  id?: string
  owned_by?: string
  type?: string
  [key: string]: unknown
}

type ModelRegistry = Record<string, ProviderAwareModel[]>

const PROVIDER_ALIAS_MAP: Record<string, string> = {
  claude: 'anthropic',
  anthropic: 'anthropic',
  openai: 'openai',
  gemini: 'google',
  google: 'google',
  kimi: 'kimi',
  moonshot: 'kimi',
  minimax: 'minimax',
  deepseek: 'deepseek',
  glm: 'glm',
  zai: 'glm',
  zhipu: 'glm',
  bigmodel: 'glm',
  mimo: 'mimo',
  xiaomi: 'mimo',
  llama: 'meta',
  meta: 'meta',
}

function normalizeProvider(value?: string): string {
  const lower = value?.trim().toLowerCase() || ''
  if (!lower) return ''
  return PROVIDER_ALIAS_MAP[lower] || lower
}

// Lazy-initialized maps — populated on first access to avoid module-level side effects
let _providerByModelId: Map<string, string> | null = null
let _registryByModelId: Map<string, ProviderAwareModel> | null = null

function ensureMaps(): { providerByModelId: Map<string, string>; registryByModelId: Map<string, ProviderAwareModel> } {
  if (_providerByModelId && _registryByModelId) {
    return { providerByModelId: _providerByModelId, registryByModelId: _registryByModelId }
  }

  const providerByModelId = new Map<string, string>()
  const registryByModelId = new Map<string, ProviderAwareModel>()
  const registries = [modelRegistry, customModelRegistry] as ModelRegistry[]

  registries.forEach((registry) => {
    Object.values(registry).forEach((models) => {
      if (!Array.isArray(models)) return
      models.forEach((model) => {
        const modelId = model.id?.trim().toLowerCase()
        if (!modelId) return
        const provider = normalizeProvider(model.owned_by) || normalizeProvider(model.type)
        registryByModelId.set(modelId, model)
        if (provider) {
          providerByModelId.set(modelId, provider)
        }
      })
    })
  })

  _providerByModelId = providerByModelId
  _registryByModelId = registryByModelId
  return { providerByModelId, registryByModelId }
}

export function getModelRegistryMetadata(modelId?: string): ProviderAwareModel | undefined {
  const key = modelId?.trim().toLowerCase() || ''
  if (!key) return undefined
  return ensureMaps().registryByModelId.get(key)
}

export function resolveModelProvider(model: ProviderAwareModel): string {
  return normalizeProvider(model.owned_by) ||
    normalizeProvider(model.type) ||
    ensureMaps().providerByModelId.get(model.id?.trim().toLowerCase() || '') ||
    'other'
}

/**
 * Returns the resolved provider key for a given model id string.
 * Useful for callers that only have the model id, not a full ProviderAwareModel object.
 */
export function getProviderForModelId(modelId: string): string {
  const key = modelId?.trim().toLowerCase() || ''
  if (!key) return 'other'
  return ensureMaps().providerByModelId.get(key) || 'other'
}
