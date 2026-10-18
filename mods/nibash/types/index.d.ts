/** A nibash call still running in its request, as nib's nibash_status reports it; times are Unix milliseconds. */
export type Command = { agent: string; command: string; description?: string; started: number; tail?: string[] }

/** A nibash job, running or ended; exit and ended are set once it ended. */
export type Job = { id: number; agent: string; command: string; started: number; ended?: number; running: boolean; exit?: number; log: string; tail?: string[] }

/** One nibash_status answer, with when it was asked. */
export type Status = { commands: Command[]; jobs: Job[]; at: number }

/** A path nib moved to the trash that is still there; path is absolute and what a restore takes. */
export type TrashEntry = { path: string; shown: string; when: number; dir?: boolean }

/** The lists the view panes draw, each as nib last gave it; note is nib's answer to the last restore. */
export type Views = { trash?: TrashEntry[]; note?: string }

declare module 'claude-code' {
  interface PluginState {
    nibash: { status: Status | null; server: string; views: Views }
  }
}
