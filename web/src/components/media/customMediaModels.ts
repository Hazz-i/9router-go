import type { CustomModelWrite } from '../../lib/customModels'

export function parseCustomMediaModelIds(value: string): string[] {
  return [...new Set(value.split(/[,;\n]+/).map((id) => id.trim()).filter(Boolean))]
}

export function getCustomMediaModelIds(models: Array<{ id?: string; name?: string; model?: string } | string>): string[] {
  return parseCustomMediaModelIds(models.map((model) => {
    if (typeof model === 'string') return model
    return model.id || model.name || model.model || ''
  }).join('\n'))
}

export function buildCustomMediaModelWrites(ids: string[], providerAlias: string, type: string): CustomModelWrite[] {
  return ids.map((id) => ({
    id,
    key: `${providerAlias}|${id}|${type}`,
    value: { id, providerAlias, type },
  }))
}
