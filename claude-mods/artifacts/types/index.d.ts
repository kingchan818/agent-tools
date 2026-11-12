/** A file Claude wrote or rebuilt that a person opens rather than reads as code. */
export type Artifact = {
  /** Absolute path. */
  path: string
  name: string
  /** The extension, lowercase: html, png, pdf, md, ... */
  kind: string
  size: number
  /** Last write, ms since the epoch. */
  at: number
  /** How many times Claude wrote it. */
  writes: number
  /** The tool that wrote it last: Write, Edit or Bash. */
  by: string
  /** The prompt that asked for it, its first 120 characters. */
  asked: string
}

export type Scope = 'session' | 'project'

declare module 'claude-code' {
  interface PluginState {
    artifacts: {
      session: Shaped<Artifact[]>
      project: Shaped<Artifact[]>
      scope: Scope
      selected: string | null
      lastPrompt: string
    }
  }
}
