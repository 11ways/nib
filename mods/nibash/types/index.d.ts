/** A nibash call still running in its request, as nib's nibash_status reports it; times are Unix milliseconds. */
export type Command = { agent: string; command: string; description?: string; started: number; tail?: string[] }

/** A nibash job, running or ended; exit and ended are set once it ended. */
export type Job = { id: number; agent: string; command: string; started: number; ended?: number; running: boolean; exit?: number; log: string; tail?: string[] }

/** One nibash_status answer, with when it was asked. */
export type Status = { commands: Command[]; jobs: Job[]; at: number }

/** A path nib moved to the trash that is still there; path is absolute and what a restore takes. */
export type TrashEntry = { path: string; shown: string; when: number; dir?: boolean }

/** The named directories of each agent, as nib lists them; source is server, defined, automatic or inherited. */
export type NameList = { agent: string; names: { name: string; directory: string; source: string; uses: number }[] }[]

/** The lines nib counts each agent as having seen of its files: inclusive 1-based ranges; anywhere says nib has no record, so edits may anchor on any line. */
export type SeenList = {
  agent: string
  files: { path: string; tag: string; lines: number; anywhere?: boolean; seen: [number, number][]; moreRanges?: number; known?: number }[]
  more?: number
}[]

/** What nib's tools returned in this session, in characters of text: by agent and tool, and the largest results. */
export type Tokens = {
  chars: number
  calls: number
  agents: { agent: string; calls: number; chars: number; tools: { tool: string; calls: number; chars: number }[] }[]
  largest: { agent: string; tool: string; chars: number; call: string }[]
}

/** The lists the view panes draw, each as nib last gave it; note is nib's answer to the last restore. */
export type Views = { trash?: TrashEntry[]; tokens?: Tokens; names?: NameList; seen?: SeenList; note?: string }

declare module 'claude-code' {
  interface PluginState {
    nibash: { status: Status | null; server: string; views: Views }
  }
}
