import { describe, expect, it } from 'bun:test'
import { buildCustomMediaModelWrites, getCustomMediaModelIds, parseCustomMediaModelIds } from './customMediaModels'

describe('custom media model helpers', () => {
  it('parses comma, semicolon, and newline separated ids without duplicates', () => {
    expect(parseCustomMediaModelIds(' model-a,model-b;\nmodel-a \n ')).toEqual(['model-a', 'model-b'])
  })

  it('reads model ids from /models entries and ignores empty entries', () => {
    expect(getCustomMediaModelIds(['model-a', { name: 'model-b' }, { model: 'model-c' }, {}])).toEqual([
      'model-a', 'model-b', 'model-c',
    ])
  })

  it('builds the custom-model storage keys and values', () => {
    expect(buildCustomMediaModelWrites(['model-a'], 'kenari', 'image')).toEqual([{
      id: 'model-a',
      key: 'kenari|model-a|image',
      value: { id: 'model-a', providerAlias: 'kenari', type: 'image' },
    }])
  })
})
