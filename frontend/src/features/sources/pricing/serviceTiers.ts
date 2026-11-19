// Match existing provider prefixes and effort/compact aliases, not all GPT models.
function canonical(model: string): string {
  return (model.trim().split('/').pop() ?? '').trim().toLowerCase().replace(/[_\s]+/g, '-').replace(/-+/g, '-')
}

export function supportsUltrafast(model: string): boolean {
  const value = canonical(model)
  return value === 'gpt-6' || /^gpt-6-astra(?:-.+)?$/.test(value) ||
    /^gpt-6\.1-sol(?:-(?:none|minimal|low|medium|high|xhigh|max|openai-compact))?$/.test(value) ||
    /^gpt-6\.2-(?:sol|astra)(?:-(?:none|minimal|low|medium|high|xhigh|max|openai-compact))?$/.test(value)
}

export function supportsFast(model: string): boolean {
  return supportsUltrafast(model) || /^gpt-6-luna(?:-(?:none|low|medium|high|xhigh|max|openai-compact))?$/.test(canonical(model))
}
