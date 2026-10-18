/** The name of nib's skim tool, found in a tool list as the one that requires expect, so a rename stays one Go edit (engine.Skim). */
export function skimName(tools: { name: string; inputSchema: { required?: string[] } }[]) {
  const found = tools.filter((tool) => tool.inputSchema.required?.includes("expect"))
  if (found.length !== 1) throw new Error(`expected one tool that requires expect, found ${found.map((tool) => tool.name).join(", ") || "none"}`)
  return found[0]!.name
}
