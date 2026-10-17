/** A nibash call still running in its request, as nib's nibash_status reports it; times are Unix milliseconds. */
export type Command = { agent: string; command: string; description?: string; started: number; tail?: string[] }

/** A nibash job, running or ended; exit and ended are set once it ended. */
export type Job = { id: number; agent: string; command: string; started: number; ended?: number; running: boolean; exit?: number; log: string; tail?: string[] }

/** One nibash_status answer, with when it was asked. */
export type Status = { commands: Command[]; jobs: Job[]; at: number }

declare module 'claude-code' {
  interface PluginState {
    nibash: { status: Status | null; server: string }
  }
}
