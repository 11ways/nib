/** The models a full benchmark covers: Claude through Claude Code, GPT through OpenCode on the OpenAI subscription login, the rest through OpenCode and OpenRouter. */
import type { Model } from "./runners"

/** Pinned versions rather than -latest aliases, so a model id always means the same weights and cached results stay comparable. */
export const MODELS: Model[] = [
  { harness: "claude", model: "claude-haiku-5-5", label: "Claude Haiku 5.5" },
  { harness: "claude", model: "claude-sonnet-5-5", label: "Claude Sonnet 5.5" },
  { harness: "claude", model: "claude-opus-5-5", label: "Claude Opus 5.5" },
  { harness: "opencode", model: "openai/gpt-6-luna", label: "GPT-6 Luna" },
  { harness: "opencode", model: "openai/gpt-6.1-sol", label: "GPT-6.1 Sol" },
  { harness: "opencode", model: "openai/gpt-6-astra", label: "GPT-6 Astra" },
  { harness: "opencode", model: "openrouter/xiaomi/mimo-v2.6-flash", label: "MiMo v2.6 Flash" },
  // OpenRouter offers "DeepSeek Flash" only as the moving ~deepseek/deepseek-flash-latest alias; v4.1 is the newest pinned Flash it lists.
  { harness: "opencode", model: "openrouter/deepseek/deepseek-v4.1-flash", label: "DeepSeek v4.1 Flash" },
]

/**
 * The models named in `filter` (labels, ids or a harness name, case-insensitive), or all of them, each once even when named twice; an id not in
 * the list runs too, through Claude Code when it starts with claude- and through OpenCode otherwise.
 */
export function selectModels(filter?: string[]): Model[] {
  if (!filter?.length) return MODELS
  const chosen = filter.flatMap((wanted): Model[] => {
    const lower = wanted.toLowerCase()
    const known = MODELS.filter((model) => model.label.toLowerCase() === lower || model.model.toLowerCase() === lower || model.harness === lower)
    if (known.length) return known
    return [{ harness: lower.startsWith("claude-") ? "claude" : "opencode", model: wanted, label: wanted }]
  })
  const same = (a: Model, b: Model) => a.harness === b.harness && a.model === b.model && a.variant === b.variant
  return chosen.filter((model, index) => chosen.findIndex((other) => same(model, other)) === index)
}
