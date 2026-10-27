export type CompatibleNodeType = 'openai-compatible' | 'anthropic-compatible' | 'custom-embedding'

export const VARIANT_CONFIG: Record<CompatibleNodeType, {
  label: string
  summary: string
  title: string
  defaultBaseUrl: string
  namePlaceholder: string
  prefixPlaceholder: string
  baseUrlHint: string
  modelIdPlaceholder: string
  nameHint: string
  prefixHint: string
  modelIdHint: string
  hasApiType: boolean
}> = {
  'openai-compatible': {
    label: 'OpenAI Compatible',
    summary: 'Chat Completions or Responses endpoints',
    title: 'Add Custom Provider',
    defaultBaseUrl: 'https://api.openai.com/v1',
    namePlaceholder: 'OpenAI Compatible (Prod)',
    prefixPlaceholder: 'oc-prod',
    baseUrlHint: 'Use the base URL (ending in /v1) for your OpenAI-compatible API.',
    modelIdPlaceholder: 'e.g. gpt-4, claude-3-opus',
    nameHint: 'Required. A friendly label for this node.',
    prefixHint: 'Required. Used as the provider prefix for model IDs.',
    modelIdHint: 'If provider lacks /models endpoint, enter a model ID to validate via chat/completions instead.',
    hasApiType: true,
  },
  'anthropic-compatible': {
    label: 'Anthropic Compatible',
    summary: 'Endpoints speaking the Anthropic Messages API',
    title: 'Add Custom Provider',
    defaultBaseUrl: 'https://api.anthropic.com/v1',
    namePlaceholder: 'Anthropic Compatible (Prod)',
    prefixPlaceholder: 'ac-prod',
    baseUrlHint: 'Use the base URL (ending in /v1) for your Anthropic-compatible API. The system will append /messages.',
    modelIdPlaceholder: 'e.g. claude-3-opus',
    nameHint: 'Required. A friendly label for this node.',
    prefixHint: 'Required. Used as the provider prefix for model IDs.',
    modelIdHint: 'If provider lacks /models endpoint, enter a model ID to validate via messages instead.',
    hasApiType: false,
  },
  'custom-embedding': {
    label: 'Custom Embedding',
    summary: 'OpenAI-compatible /embeddings endpoints',
    title: 'Add Custom Embedding',
    defaultBaseUrl: 'https://api.openai.com/v1',
    namePlaceholder: 'Voyage AI',
    prefixPlaceholder: 'voyage',
    baseUrlHint: 'Most embedding APIs are OpenAI-compatible: Voyage, Cohere, Jina, Mistral, Together...',
    modelIdPlaceholder: 'e.g. voyage-3, embed-english-v3.0, text-embedding-3-small',
    nameHint: 'Required. A friendly label for this embedding provider.',
    prefixHint: 'Required. Used as the provider prefix for model IDs (e.g. voyage/voyage-3).',
    modelIdHint: 'Required for validation. Will send a test embeddings request.',
    hasApiType: false,
  },
}
