import { createHash } from "node:crypto"
import { constants, readFileSync, statSync } from "node:fs"
import { lstat, mkdir, open } from "node:fs/promises"
import os from "node:os"
import path from "node:path"

/** The part of Claude Code's PreToolUse hook input this hook reads. */
export type HookInput = { session_id?: string; cwd?: string; tool_name?: string; tool_input?: Record<string, unknown> }

/** A nib call that does what a refused call meant to; `content` stands for content the refused call carried, which the message does not repeat. */
type Call = { tool: "read" | "grep" | "find" | "edit" | "write"; args: Record<string, unknown>; content?: "write" | "append" }

/** A shell word; `glob` is an unquoted wildcard, `quotedGlob` a quoted or escaped one, which the shell keeps literal. */
type Word = { text: string; dynamic: boolean; glob: boolean; quotedGlob: boolean; tilde: boolean; quoted: boolean }
type Operator = { op: string; body?: string; dynamic?: boolean }
type Token = { word: Word } | Operator
type Redirect = { op: string; target: Word; body?: string; dynamic?: boolean }
type Command = { words: Word[]; redirects: Redirect[] }
/** A pipeline and the operator that joined it to the one before. */
type Pipeline = { commands: Command[]; after?: string }
/** Lines of one file: a range (`to` absent runs to the end), or its last lines. */
type Lines = { from: number; to?: number } | { last: number }

// Longest first, so a prefix never wins over the operator it starts.
const OPERATORS = ["&&", "||", "|&", ";;", "&>>", "&>", ">>", ">|", ">&", "<<<", "<<-", "<<", "<&", "<>", "|", ";", "&", "(", ")", ">", "<"]
const SEPARATORS = new Set([";", "&&", "||", "&"])
const RESERVED = new Set(["if", "then", "else", "elif", "fi", "for", "while", "until", "do", "done", "case", "esac", "function", "select", "{", "}", "!", "[[", "]]"])
const SPECIAL = ["/dev/", "/proc/", "/sys/"]
// ripgrep's --type names agents use most, as globs.
const TYPES: Record<string, string[]> = {
  c: ["*.c", "*.h"], cpp: ["*.cpp", "*.cc", "*.cxx", "*.hpp", "*.hh", "*.h"], css: ["*.css"], go: ["*.go"], html: ["*.html", "*.htm"], java: ["*.java"],
  js: ["*.js", "*.jsx", "*.mjs", "*.cjs"], json: ["*.json"], kotlin: ["*.kt", "*.kts"], md: ["*.md", "*.markdown"], markdown: ["*.md", "*.markdown"],
  py: ["*.py", "*.pyi"], rust: ["*.rs"], sh: ["*.sh", "*.bash"], toml: ["*.toml"], ts: ["*.ts", "*.tsx", "*.mts", "*.cts"], yaml: ["*.yaml", "*.yml"], zig: ["*.zig"],
}
// File types an agent reads to change them, so the tail of one keeps its numbered rows; ripgrep's types above, plus a few more.
const EDITED = new Set([...Object.values(TYPES).flat().map((glob) => glob.slice(2)), "xml", "sql", "rb", "php", "swift", "vue", "svelte", "scss", "sass", "less", "gradle", "properties", "ini", "cfg", "conf", "hwk"])
// engine.ts's TEXT_LIMIT: past it nib reads a file only plain, since it cannot tag it, and cannot edit it.
const TAGGED_LIMIT = 2 * 1024 * 1024

/** The tools the hook is registered for in Claude Code's settings. */
export const HOOK_MATCHER = "Bash|Read|Grep|Glob|Edit|MultiEdit|Write"

/** Index of the bracket closing the one at `at`, skipping quoted text, or -1. */
function closing(source: string, at: number) {
  const open = source[at]!
  const close = open === "(" ? ")" : "}"
  let depth = 0
  for (let index = at; index < source.length; index++) {
    const char = source[index]!
    if (char === "\\") index++
    else if (char === "'") index = source.indexOf("'", index + 1)
    else if (char === open) depth++
    else if (char === close && --depth === 0) return index
    if (index < 0) return -1
  }
  return -1
}

/** Splits a shell command into words and operators, with each here-document's body on its operator; undefined when the quoting does not close. */
export function tokenize(source: string): Token[] | undefined {
  const tokens: Token[] = []
  const pending: Operator[] = []
  let word: Word | undefined
  const part = () => (word ??= { text: "", dynamic: false, glob: false, quotedGlob: false, tilde: false, quoted: false })
  /** Adds quoted or escaped text, whose wildcards the shell keeps literal. */
  const literal = (text: string) => {
    const current = part()
    current.text += text
    current.quoted = true
    if (/[*?[]/.test(text)) current.quotedGlob = true
  }
  const flush = () => {
    if (word) tokens.push({ word })
    word = undefined
  }
  let index = 0
  while (index < source.length) {
    const char = source[index]!
    if (char === "\\") {
      if (source[index + 1] === "\n") {
        index += 2
        continue
      }
      if (index + 1 >= source.length) return undefined
      literal(source[index + 1]!)
      index += 2
    } else if (char === "'") {
      const end = source.indexOf("'", index + 1)
      if (end < 0) return undefined
      literal(source.slice(index + 1, end))
      index = end + 1
    } else if (char === '"') {
      let text = ""
      index++
      while (index < source.length && source[index] !== '"') {
        if (source[index] === "\\" && '"\\$`\n'.includes(source[index + 1] ?? "")) {
          if (source[index + 1] !== "\n") text += source[index + 1]
          index += 2
          continue
        }
        if (source[index] === "$" || source[index] === "`") part().dynamic = true
        text += source[index]
        index++
      }
      if (index >= source.length) return undefined
      literal(text)
      index++
    } else if (char === "$" || char === "`") {
      // An expansion, whose value the hook cannot know.
      const current = part()
      current.dynamic = true
      const end = char === "`" ? source.indexOf("`", index + 1) : source[index + 1] === "(" || source[index + 1] === "{" ? closing(source, index + 1) : index
      if (end < 0) return undefined
      current.text += source.slice(index, end + 1)
      index = end + 1
    } else if (char === "#" && !word) {
      while (index < source.length && source[index] !== "\n") index++
    } else if (char === " " || char === "\t") {
      flush()
      index++
    } else if (char === "\n") {
      flush()
      tokens.push({ op: ";" })
      index++
      for (const document of pending.splice(0)) index = hereDocument(source, index, document, tokens)
    } else {
      const op = OPERATORS.find((candidate) => source.startsWith(candidate, index))
      if (!op) {
        if ("*?[".includes(char)) part().glob = true
        if (char === "~" && !word) part().tilde = true
        part().text += char
        index++
        continue
      }
      // A number right before a redirection names the file descriptor it redirects.
      const descriptor = word && !word.quoted && !word.dynamic && /^\d+$/.test(word.text) && (op.startsWith(">") || op.startsWith("<")) ? word.text : ""
      if (descriptor) word = undefined
      flush()
      const token: Operator = { op: descriptor + op }
      tokens.push(token)
      if (op === "<<" || op === "<<-") pending.push(token)
      index += op.length
    }
  }
  flush()
  for (const document of pending) index = hereDocument(source, index, document, tokens)
  return tokens
}

/** Reads the body of the here-document `document` starting at `index`, ending at its delimiter line; returns where the command goes on. */
function hereDocument(source: string, index: number, document: Operator, tokens: Token[]) {
  const at = tokens.indexOf(document)
  const delimiter = at >= 0 ? tokens[at + 1] : undefined
  if (!delimiter || !("word" in delimiter)) return index
  let body = ""
  while (index < source.length) {
    const end = source.indexOf("\n", index)
    const raw = source.slice(index, end < 0 ? source.length : end)
    index = end < 0 ? source.length : end + 1
    const line = document.op.endsWith("<<-") ? raw.replace(/^\t+/, "") : raw
    if (line === delimiter.word.text) break
    body += `${line}\n`
  }
  document.body = body
  // An unquoted delimiter lets the shell expand the body.
  document.dynamic = !delimiter.word.quoted && /[$`]/.test(body)
  return index
}

/** Groups tokens into pipelines of commands, each with the operator before it; undefined for subshells, loops, conditionals and the like. */
export function structure(tokens: Token[]): Pipeline[] | undefined {
  const pipelines: Pipeline[] = []
  let pipeline: Pipeline = { commands: [] }
  let command: Command = { words: [], redirects: [] }
  const endCommand = () => {
    if (command.words.length || command.redirects.length) pipeline.commands.push(command)
    command = { words: [], redirects: [] }
  }
  for (let index = 0; index < tokens.length; index++) {
    const token = tokens[index]!
    if ("word" in token) {
      if (!command.words.length && RESERVED.has(token.word.text) && !token.word.quoted) return undefined
      command.words.push(token.word)
      continue
    }
    if (SEPARATORS.has(token.op)) {
      endCommand()
      if (pipeline.commands.length) pipelines.push(pipeline)
      pipeline = { commands: [], after: token.op }
    } else if (token.op === "|" || token.op === "|&") {
      endCommand()
    } else if (token.op === "(" || token.op === ")" || token.op === ";;") {
      return undefined
    } else {
      const next = tokens[index + 1]
      if (!next || !("word" in next)) return undefined
      command.redirects.push({ op: token.op, target: next.word, body: token.body, dynamic: token.dynamic })
      index++
    }
  }
  endCommand()
  if (pipeline.commands.length) pipelines.push(pipeline)
  return pipelines
}

/** The command's words after leading variable assignments and wrappers that do not change what it reads. */
function program(command: Command) {
  let start = 0
  while (start < command.words.length) {
    const word = command.words[start]!
    if (/^[A-Za-z_][A-Za-z0-9_]*=/.test(word.text) && !word.quoted) start++
    else if (["command", "builtin", "nohup", "time"].includes(word.text)) start++
    else break
  }
  return command.words.slice(start)
}

/** True for a redirection that only moves error output out of the way, which a nib call does not need. */
const quiet = (redirect: Redirect) => (redirect.op === "2>" && redirect.target.text === "/dev/null") || (redirect.op === "2>&" && redirect.target.text === "1")

/**
 * The files a word names, globs expanded like the shell; undefined when the word mixes wildcards the shell expands with ones it keeps literal,
 * which a glob library cannot tell apart.
 */
function files(word: Word, cwd: string, home: string): string[] | undefined {
  if (word.dynamic || (word.glob && word.quotedGlob)) return undefined
  const text = word.tilde && (word.text === "~" || word.text.startsWith("~/")) ? home + word.text.slice(1) : word.text
  const absolute = path.resolve(cwd, text)
  if (!word.glob) return [absolute]
  const parts = absolute.split("/")
  const fixed = parts.findIndex((segment) => /[*?[]/.test(segment))
  const base = parts.slice(0, fixed).join("/") || "/"
  try {
    const found = [...new Bun.Glob(parts.slice(fixed).join("/")).scanSync({ cwd: base, absolute: true, onlyFiles: false })].sort()
    // Like the shell, a glob that matches nothing stays as written.
    return found.length ? found : [absolute]
  } catch {
    return [absolute]
  }
}

/** All files `words` name, or undefined when one is standard input, a device or process file, or a word `files` declines. */
function allFiles(words: Word[], cwd: string, home: string) {
  const found: string[] = []
  for (const word of words) {
    if (word.text === "-") return undefined
    const names = files(word, cwd, home)
    if (!names || names.some((name) => SPECIAL.some((prefix) => name.startsWith(prefix)))) return undefined
    found.push(...names)
  }
  return found
}

const isDirectory = (name: string) => { try { return statSync(name).isDirectory() } catch { return false } }
const exists = (name: string) => { try { statSync(name); return true } catch { return false } }
const edited = (name: string) => EDITED.has(path.extname(name).slice(1).toLowerCase())

/**
 * True for a file read only to look at, which a plain read shows without numbers or tag: a log or output file by extension, a file in a log folder
 * that is not code or config (a package named `log` holds code), or one too big to tag.
 */
function lookOnly(name: string) {
  if (/\.(log|out|err)(\.\d+)?$/i.test(name)) return true
  if (!edited(name) && name.split("/").some((segment) => segment === "log" || segment === "logs")) return true
  try { return statSync(name).size > TAGGED_LIMIT } catch { return false }
}

/** A path as the agent should pass it to nib: relative to the session's folder when inside it. */
function shown(name: string, session: string) {
  const relative = path.relative(session, name)
  return !relative ? "." : !relative.startsWith("..") && !path.isAbsolute(relative) ? relative : name
}

const number = (value: string | undefined) => value !== undefined && /^\d+$/.test(value) ? Number(value) : undefined

/** head's or tail's line count, a tail's starting line, and file operands; undefined for byte counts, follow mode or options the hook does not know. */
function lineCount(args: Word[], tail: boolean) {
  let count = 10
  let from: number | undefined
  const operands: Word[] = []
  let options = true
  for (let index = 0; index < args.length; index++) {
    const text = args[index]!.text
    if (!options || !text.startsWith("-") || text === "-") {
      operands.push(args[index]!)
      continue
    }
    if (text === "--") {
      options = false
      continue
    }
    const value = text === "-n" || text === "--lines" ? args[++index]?.text : text.startsWith("--lines=") ? text.slice(8) : /^-n.+/.test(text) ? text.slice(2) : /^-\d+$/.test(text) ? text.slice(1) : undefined
    if (value === undefined) {
      if (text !== "-q" && text !== "-v") return undefined
    } else if (tail && value.startsWith("+") && number(value.slice(1))) from = number(value.slice(1))
    else if (number(value) !== undefined) count = number(value)!
    else return undefined
  }
  return { count, from, operands }
}

/** The lines a sed -n script prints, as in `5,20p`, `7p` or `30,$p`. */
function sedLines(script: string): Lines | undefined {
  const match = /^(\d+)(?:,(\d+|\$))?p$/.exec(script)
  if (!match || Number(match[1]) < 1) return undefined
  return { from: Number(match[1]), ...match[2] !== "$" && { to: Number(match[2] ?? match[1]) } }
}

/** `lines` narrowed by a later head or tail in the pipeline, or undefined when the result is no fixed range of the file. */
function narrow(lines: Lines, stage: "head" | "tail", count: number): Lines | undefined {
  if ("last" in lines) return stage === "tail" ? { last: Math.min(lines.last, count) } : undefined
  if (stage === "head") return { from: lines.from, to: lines.to === undefined ? lines.from + count - 1 : Math.min(lines.to, lines.from + count - 1) }
  if (lines.to !== undefined) return { from: Math.max(lines.from, lines.to - count + 1), to: lines.to }
  return lines.from === 1 ? { last: count } : undefined
}

function selector(lines: Lines) {
  if ("last" in lines) return `:-${lines.last}`
  if (lines.to === undefined) return lines.from === 1 ? "" : `:${lines.from}-`
  return lines.from === lines.to ? `:${lines.from}` : `:${lines.from}-${lines.to}`
}

/** A basic regular expression as grep reads it, in the extended syntax ripgrep and nib read. */
export function extended(basic: string) {
  let out = ""
  for (let index = 0; index < basic.length; index++) {
    const char = basic[index]!
    if (char === "\\" && index + 1 < basic.length) {
      const next = basic[++index]!
      out += "|(){}+?".includes(next) ? next : next === "<" || next === ">" ? "\\b" : `\\${next}`
    } else out += "|(){}+?".includes(char) ? `\\${char}` : char
  }
  return out
}

const escapeRegex = (text: string) => text.replace(/[\\^$.*+?()[\]{}|]/g, "\\$&")

type Search = {
  patterns: string[]; operands: Word[]; globs: string[]; types: string[]; ignoreCase?: boolean; smart?: boolean; literal?: boolean; extended?: boolean
  word?: boolean; line?: boolean; mode?: "files" | "count"; before?: number; after?: number; listFiles?: boolean; recursive?: boolean; noIgnore?: boolean
}

/** The search a grep or rg command asks for, or undefined for any option nib has no exact counterpart for (inverted, only-matching, PCRE and others). */
function searchOf(name: "grep" | "rg", args: Word[]): Search | undefined {
  const search: Search = { patterns: [], operands: [], globs: [], types: [], recursive: name === "rg" }
  const valued = new Set(name === "rg" ? ["e", "A", "B", "C", "g", "t", "m", "M", "j"] : ["e", "A", "B", "C", "m"])
  const ignored = new Set(name === "rg" ? ["n", "H", "N", "I", "s", "p"] : ["n", "H", "h", "s", "I", "G", "E"])
  const apply = (flag: string, value: string | undefined): boolean => {
    switch (flag) {
      case "i": case "ignore-case": search.ignoreCase = true; return true
      case "S": case "smart-case": search.smart = true; return true
      case "F": case "fixed-strings": search.literal = true; return true
      case "w": case "word-regexp": search.word = true; return true
      case "x": case "line-regexp": search.line = true; return true
      case "l": case "files-with-matches": search.mode = "files"; return true
      case "c": case "count": search.mode = "count"; return true
      case "r": case "R": case "recursive": search.recursive = true; return true
      case "extended-regexp": search.extended = true; return true
      case "u": case "no-ignore": case "no-ignore-vcs": case "unrestricted": search.noIgnore = true; return true
      case "e": case "regexp": if (value === undefined) return false; search.patterns.push(value); return true
      case "A": case "after-context": if (number(value) === undefined) return false; search.after = number(value); return true
      case "B": case "before-context": if (number(value) === undefined) return false; search.before = number(value); return true
      case "C": case "context": if (number(value) === undefined) return false; search.before = search.after = number(value); return true
      case "g": case "glob": case "include": if (value === undefined) return false; search.globs.push(value); return true
      case "exclude": case "exclude-dir": if (value === undefined) return false; search.globs.push(`!${value}`); return true
      case "t": case "type": { const globs = value === undefined ? undefined : TYPES[value]; if (!globs) return false; search.types.push(...globs); return true }
      case "files": search.listFiles = true; return true
      case "m": case "max-count": case "M": case "max-columns": case "j": case "threads": return true
      case "hidden": case "no-heading": case "heading": case "line-number": case "with-filename": case "no-filename": case "no-messages": case "color": case "colour": case "binary-files": return true
      default: return ignored.has(flag)
    }
  }
  if (name === "grep") search.extended = false
  let options = true
  for (let index = 0; index < args.length; index++) {
    const text = args[index]!.text
    if (options && text === "--") options = false
    else if (options && text.startsWith("--")) {
      const [flag, inline] = text.slice(2).split(/=(.*)/s, 2) as [string, string | undefined]
      const takes = ["regexp", "after-context", "before-context", "context", "glob", "include", "exclude", "exclude-dir", "type", "max-count", "max-columns", "threads"].includes(flag)
      const value = takes ? inline ?? args[++index]?.text : inline
      if (!apply(flag, value)) return undefined
    } else if (options && text.startsWith("-") && text !== "-") {
      for (let at = 1; at < text.length; at++) {
        const flag = text[at]!
        if (flag === "E") search.extended = true
        if (valued.has(flag)) {
          const value = at + 1 < text.length ? text.slice(at + 1) : args[++index]?.text
          if (!apply(flag, value)) return undefined
          break
        }
        if (/\d/.test(flag) && name === "grep") {
          // grep -3 means three lines of context either side.
          const digits = /^\d+/.exec(text.slice(at))![0]
          search.before = search.after = Number(digits)
          at += digits.length - 1
          continue
        }
        if (!apply(flag, undefined)) return undefined
      }
    } else search.operands.push(args[index]!)
  }
  if (!search.listFiles && !search.patterns.length) {
    const pattern = search.operands.shift()
    if (!pattern) return undefined
    search.patterns.push(pattern.text)
  }
  // A type and an including glob both narrow the files, where nib's globs widen each other.
  if (search.types.length && search.globs.some((glob) => !glob.startsWith("!"))) return undefined
  search.globs.push(...search.types)
  return search
}

/** The nib grep call for `search` over `paths` (a nib find for rg --files), or undefined for a pattern nib's regex engine would read otherwise. */
function grepCall(name: "grep" | "rg", search: Search, paths: string[] | undefined): Call | undefined {
  const globs = search.globs.length ? { glob: search.globs.length === 1 ? search.globs[0] : search.globs } : {}
  if (search.listFiles) return { tool: "find", args: { ...paths && { paths }, ...globs } }
  const basic = name === "grep" && !search.literal && !search.extended
  let patterns = search.patterns.map((pattern) => basic ? extended(pattern) : pattern)
  // Backreferences exist in grep's syntax but not in nib's.
  if (!search.literal && patterns.some((pattern) => /\\[1-9]/.test(pattern))) return undefined
  let literal = !!search.literal
  // grep -w wants a non-word character or the line's edge on both sides, which \b gives only for a match that starts and ends in a word character.
  if (search.word && !search.patterns.every((pattern) => /^\w/.test(pattern) && /\w$/.test(pattern) && !/\\\w$/.test(pattern))) return undefined
  if (literal && (patterns.length > 1 || search.word || search.line)) {
    patterns = patterns.map(escapeRegex)
    literal = false
  }
  let pattern = patterns.length > 1 ? patterns.map((item) => `(?:${item})`).join("|") : patterns[0]!
  if (search.word) pattern = `\\b(?:${pattern})\\b`
  if (search.line) pattern = `^(?:${pattern})$`
  const ignoreCase = search.ignoreCase || (search.smart && pattern === pattern.toLowerCase())
  return {
    tool: "grep",
    args: {
      pattern,
      ...paths && { paths },
      ...globs,
      ...ignoreCase && { ignoreCase: true },
      ...literal && { literal: true },
      ...search.mode && { mode: search.mode },
      ...search.noIgnore && { noIgnore: true },
      ...search.before !== undefined && { before: search.before },
      ...search.after !== undefined && { after: search.after },
    },
  }
}

const text = (words: Word[]) => words.map((word) => word.text).join(" ")

/** A later stage of a pipeline that only shows or narrows the output before it: head or tail with a line count, a pager, cat, wc -l. */
function viewer(words: Word[]): { kind: "head" | "tail"; count: number } | { kind: "same" } | { kind: "count" } | undefined {
  const name = words[0]?.text
  if ((name === "cat" || name === "less" || name === "more") && words.length === 1) return { kind: "same" }
  if (text(words) === "wc -l") return { kind: "count" }
  if (name !== "head" && name !== "tail") return undefined
  const counted = lineCount(words.slice(1), name === "tail")
  return counted && !counted.operands.length && counted.from === undefined ? { kind: name, count: counted.count } : undefined
}

/** What a file-reading command at the start of a pipeline reads, and the nib call for the whole pipeline, or undefined when it is not exactly that. */
function readerCall(pipeline: Command[], cwd: string, session: string, home: string): Call | undefined {
  // Output written to a file, or input read from one, is a job for the shell.
  if (pipeline.some((command) => command.redirects.some((redirect) => !quiet(redirect)))) return undefined
  const [first, ...rest] = pipeline
  const words = program(first!)
  const name = words[0]?.text
  if (!name) return undefined
  const args = words.slice(1)
  const display = (names: string[]) => names.map((item) => shown(item, session))
  const stages = rest.map((command) => program(command))
  if (name === "cat" || name === "head" || name === "tail" || name === "sed") {
    let source: { names: string[]; lines: Lines } | undefined
    if (name === "cat") {
      // -n numbers the lines, as nib's rows are; every other option changes what cat prints.
      if (args.some((arg) => arg.text.startsWith("-") && arg.text !== "-" && arg.text !== "-n")) return undefined
      const names = allFiles(args.filter((arg) => arg.text !== "-n"), cwd, home)
      if (names?.length) source = { names, lines: { from: 1 } }
    } else if (name === "sed") {
      const script = args[0]?.text === "-n" ? args[1]?.text === "-e" ? args[2] : args[1] : undefined
      const lines = script && sedLines(script.text)
      const names = lines ? allFiles(args.slice(args[1]!.text === "-e" ? 3 : 2), cwd, home) : undefined
      // Over several files sed counts lines across all of them, which no selector of one file gives.
      if (lines && names?.length === 1) source = { names, lines }
    } else {
      const counted = lineCount(args, name === "tail")
      const names = counted && allFiles(counted.operands, cwd, home)
      if (counted && names?.length) source = { names, lines: name === "head" ? { from: 1, to: counted.count } : counted.from !== undefined ? { from: counted.from } : { last: counted.count } }
    }
    if (!source || source.names.some(isDirectory)) return undefined
    let lines: Lines | undefined = source.lines
    let index = 0
    // Every head or tail after a read of one file is a narrower range of that file; over several files it is not.
    for (; index < stages.length; index++) {
      const stage = viewer(stages[index]!)
      if (!stage || stage.kind === "count") break
      if (stage.kind === "same") continue
      if (source.names.length > 1) return undefined
      lines = narrow(lines, stage.kind, stage.count)
      if (!lines) return undefined
    }
    const paths = display(source.names).map((item) => item + selector(lines!))
    if (index === stages.length) {
      // The end of a file that is not code or config is most likely a log.
      const plain = source.names.every((file) => lookOnly(file) || (name === "tail" && !edited(file)))
      return { tool: "read", args: { paths, ...plain && { plain: true } } }
    }
    const [stage, ...after] = stages.slice(index)
    if (text(stage!) === "wc -l" && !after.length && name === "cat" && selector(lines!) === "") return { tool: "read", args: { paths, info: true } }
    // A grep over what a read shows is a grep of those lines; nib pages what comes after it.
    if (stage![0]?.text !== "grep" || !after.every((item) => viewer(item)?.kind !== "count" && viewer(item) !== undefined)) return undefined
    const search = searchOf("grep", stage!.slice(1))
    return search && !search.operands.length && !search.recursive && !search.listFiles ? grepCall("grep", search, paths) : undefined
  }
  if (name === "grep" || name === "egrep" || name === "fgrep" || name === "rg") {
    const search = searchOf(name === "rg" ? "rg" : "grep", args)
    if (!search) return undefined
    if (name === "egrep") search.extended = true
    if (name === "fgrep") search.literal = true
    // grep without files reads its input, which nib cannot search.
    if (name !== "rg" && !search.operands.length && !search.recursive) return undefined
    const names = allFiles(search.operands, cwd, home)
    if (!names || (name !== "rg" && !search.recursive && names.some(isDirectory))) return undefined
    const views = stages.map(viewer)
    if (views.some((view) => !view) || views.slice(0, -1).some((view) => view!.kind === "count")) return undefined
    if (views.at(-1)?.kind === "count" && !search.mode) search.mode = "count"
    // Without paths both search the folder they run in.
    return grepCall(name === "rg" ? "rg" : "grep", search, names.length || cwd !== session ? display(names.length ? names : [cwd]) : undefined)
  }
  if (name === "find") {
    if (stages.some((stage) => !viewer(stage))) return undefined
    const paths: Word[] = []
    let index = 0
    while (index < args.length && !args[index]!.text.startsWith("-") && args[index]!.text !== "!" && args[index]!.text !== "(") paths.push(args[index++]!)
    const call: Record<string, unknown> = {}
    for (; index < args.length; index++) {
      const flag = args[index]!.text
      const value = args[index + 1]
      if (flag === "-type" && value?.text === "f") index++
      else if (flag === "-name" && value && !call.glob && !value.glob) {
        call.glob = value.text
        index++
      } else if ((flag === "-mtime" || flag === "-mmin") && /^-\d+$/.test(value?.text ?? "")) {
        call.newer = `${value!.text.slice(1)}${flag === "-mtime" ? "d" : "m"}`
        index++
      } else if (flag === "-size" && /^\+\d+[kMG]?$/.test(value?.text ?? "")) {
        call.minSize = value!.text.slice(1)
        index++
      } else if (flag !== "-print") return undefined
    }
    const names = allFiles(paths, cwd, home)
    if (!names) return undefined
    return { tool: "find", args: { ...(names.length || cwd !== session) && { paths: display(names.length ? names : [cwd]) }, ...call } }
  }
  if (name === "ls") {
    if (stages.length || args.some((arg) => arg.text.startsWith("-") && !/^-[1aAFp]+$/.test(arg.text))) return undefined
    const names = allFiles(args.filter((arg) => !arg.text.startsWith("-")), cwd, home)
    if (!names || !names.every(isDirectory)) return undefined
    return { tool: "read", args: { paths: display(names.length ? names : [cwd]) } }
  }
  if (name === "wc") {
    if (stages.length || args.some((arg) => arg.text.startsWith("-") && !/^-[lc]+$/.test(arg.text))) return undefined
    const names = allFiles(args.filter((arg) => !arg.text.startsWith("-")), cwd, home)
    if (!names?.length || names.some(isDirectory)) return undefined
    return { tool: "read", args: { paths: display(names), info: true } }
  }
  return undefined
}

/** The text printf prints for the two forms agents write with: `printf 'TEXT\n'` and `printf '%s\n' LINE...`. */
function printed(args: Word[]) {
  const [format, ...rest] = args
  if (!format) return undefined
  if (format.text === "%s\\n") return rest.map((arg) => `${arg.text}\n`).join("")
  if (rest.length || format.text.includes("%")) return undefined
  return format.text.replace(/\\(n|t|\\)/g, (_, code: string) => code === "n" ? "\n" : code === "t" ? "\t" : "\\")
}

/**
 * True when nib's tagless append to `file` writes the same bytes the shell's append would: a file nib can edit that ends with a newline (or is
 * empty) and holds no carriage return, so nib's rows take the shell's LF line endings.
 */
function appendsAlike(file: string) {
  try {
    const info = statSync(file)
    if (!info.isFile() || info.size > TAGGED_LIMIT) return false
    const bytes = readFileSync(file)
    return (bytes.length === 0 || bytes[bytes.length - 1] === 10) && !bytes.includes(13)
  } catch {
    return false
  }
}

/** A command that only writes fixed text to a file (a here-document through cat or tee, echo or printf) as the nib write or append it is. */
function writerCall(pipeline: Command[], cwd: string, session: string, home: string): Call | undefined {
  if (pipeline.length !== 1) return undefined
  const command = pipeline[0]!
  const words = program(command)
  const name = words[0]?.text
  const document = command.redirects.find((redirect) => redirect.op === "<<" || redirect.op === "<<-")
  const output = command.redirects.find((redirect) => redirect.op === ">" || redirect.op === ">>" || redirect.op === "1>" || redirect.op === "1>>")
  if (command.redirects.some((redirect) => redirect !== document && redirect !== output && !quiet(redirect))) return undefined
  let content: string | undefined
  let target: Word | undefined
  let append = false
  if (name === "tee" && document && !output) {
    const args = words.slice(1)
    append = args[0]?.text === "-a"
    const rest = append ? args.slice(1) : args
    if (rest.length !== 1) return undefined
    target = rest[0]
    content = document.body
  } else if (output) {
    target = output.target
    append = output.op.endsWith(">>")
    if (name === "cat" && words.length === 1 && document) content = document.body
    else if (name === "echo" && !document && !["-n", "-e", "-E"].includes(words[1]?.text ?? "")) content = `${text(words.slice(1))}\n`
    else if (name === "printf" && !document) content = printed(words.slice(1))
  }
  if (content === undefined || !target || target.glob) return undefined
  const file = files(target, cwd, home)?.[0]
  if (!file || SPECIAL.some((prefix) => file.startsWith(prefix)) || isDirectory(file)) return undefined
  const display = shown(file, session)
  // The shell does not create a missing folder for the file, while nib's write would.
  if (!exists(file)) return isDirectory(path.dirname(file)) ? { tool: "write", args: { path: display }, content: "write" } : undefined
  // Overwriting an existing file needs its tag in nib, which would cost a read, so the shell keeps that job.
  if (!append || !content.endsWith("\n") || content.includes("\r") || !appendsAlike(file)) return undefined
  return { tool: "edit", args: { input: `[${display}]\nPUT >$:` }, content: "append" }
}

/** How a call is written in the refusal. */
function render(call: Call) {
  const name = `mcp__nib__${call.tool}`
  if (call.content === "write") return `${name} with path ${JSON.stringify(call.args.path)} and the same content`
  if (call.content === "append") return `${name} with input ${JSON.stringify(call.args.input)} followed by one + row per line (an append needs no tag)`
  return `${name} ${JSON.stringify(call.args)}`
}

const RETRY = "(If you need the shell's own output here, or nib's tools are missing from this session, send the same command again and it runs.)"

/** The folder a `cd DIR` pipeline moves to, or undefined for anything else, such as `cd -`, an option, or a word the hook cannot know. */
function cdTarget(pipeline: Pipeline, cwd: string, home: string) {
  if (pipeline.commands.length !== 1 || pipeline.commands[0]!.redirects.length) return undefined
  const words = program(pipeline.commands[0]!)
  if (words[0]?.text !== "cd") return undefined
  const operands = words[1]?.text === "--" ? words.slice(2) : words.slice(1)
  if (operands.length > 1 || (operands[0] && (operands[0].glob || (words[1]?.text !== "--" && operands[0].text.startsWith("-"))))) return undefined
  return operands[0] ? files(operands[0], cwd, home)?.[0] : home
}

/**
 * The refusal for a Bash command that reads, searches or writes files exactly as one nib call would, or undefined to let it run. Only `cd DIR &&`
 * may come before that one command: alternatives, background jobs and other commands change what runs, which a single call cannot keep.
 */
function bash(command: string, session: string, home: string) {
  const tokens = tokenize(command)
  if (!tokens || tokens.some((token) => "word" in token ? token.word.dynamic : token.dynamic || ["||", "&", "|&"].includes(token.op))) return undefined
  const pipelines = structure(tokens)
  if (!pipelines?.length) return undefined
  let cwd = session
  for (const [index, pipeline] of pipelines.slice(0, -1).entries()) {
    const target = cdTarget(pipeline, cwd, home)
    if (!target || (index + 1 < pipelines.length && pipelines[index + 1]!.after !== "&&")) return undefined
    cwd = target
  }
  const last = pipelines.at(-1)!.commands
  const call = readerCall(last, cwd, session, home) ?? writerCall(last, cwd, session, home)
  if (!call) return undefined
  const plain = call.tool === "read" && call.args.plain === true
  return [`nib's hook refused this command: nib does this with fewer tokens${plain ? "" : ", and its rows are edit anchors"}. Use instead:`, render(call), RETRY].join("\n")
}

const string = (value: unknown) => typeof value === "string" ? value : undefined
const count = (value: unknown) => typeof value === "number" && Number.isInteger(value) && value >= 0 ? value : undefined

/** The refusal for a built-in Claude Code file tool nib replaces, or undefined to let it run: PDFs and notebooks stay with Read, as do searches nib cannot match. */
function builtin(tool: string, input: Record<string, unknown>, session: string) {
  const file = string(input.file_path)
  const where = file ? shown(path.resolve(session, file), session) : undefined
  let call: string
  if (tool === "Read") {
    if (!where || /\.(pdf|ipynb)$/i.test(where)) return undefined
    const offset = count(input.offset)
    const limit = count(input.limit)
    const selector = offset && limit ? `:${offset}+${limit}` : limit ? `:1-${limit}` : offset ? `:${offset}-` : ""
    call = render({ tool: "read", args: { paths: [where + selector], ...lookOnly(path.resolve(session, file!)) && { plain: true } } })
  } else if (tool === "Grep") {
    const pattern = string(input.pattern)
    const type = string(input.type)
    const glob = string(input.glob)
    // A type nib cannot name, a type and a glob together, or a multiline search would come out as another search.
    if (!pattern || (type && (!TYPES[type] || glob)) || input.multiline === true) return undefined
    const mode = input.output_mode === "files_with_matches" ? "files" : input.output_mode === "count" ? "count" : undefined
    const globs = glob ? [glob] : type ? TYPES[type]! : []
    const around = count(input["-C"])
    const at = string(input.path)
    call = render({ tool: "grep", args: {
      pattern,
      ...at && { paths: [shown(path.resolve(session, at), session)] },
      ...globs.length && { glob: globs.length === 1 ? globs[0] : globs },
      ...input["-i"] === true && { ignoreCase: true },
      ...mode && { mode },
      ...(count(input["-B"]) ?? around) !== undefined && { before: count(input["-B"]) ?? around },
      ...(count(input["-A"]) ?? around) !== undefined && { after: count(input["-A"]) ?? around },
    } })
  } else if (tool === "Glob") {
    const pattern = string(input.pattern)
    if (!pattern) return undefined
    const at = string(input.path)
    call = render({ tool: "find", args: { glob: pattern, ...at && { paths: [shown(path.resolve(session, at), session)] } } })
  } else if (tool === "Write") {
    if (!where) return undefined
    call = exists(path.resolve(session, file!))
      ? `mcp__nib__write with path ${JSON.stringify(where)}, the same content and the tag of its latest read, or mcp__nib__edit for a few lines`
      : render({ tool: "write", args: { path: where }, content: "write" })
  } else if (tool === "Edit" || tool === "MultiEdit") {
    if (!where) return undefined
    call = `mcp__nib__edit: read or grep the lines of ${where}, then send [${where}#TAG] with PUT or SUB hunks by line number, never the old text`
  } else return undefined
  return [`nib's hook refused ${tool}: nib replaces it with fewer tokens. Use instead:`, call, `(If you need ${tool} itself here, or nib's tools are missing from this session, send the same call again and it runs.)`].join("\n")
}

/** The refusal reason for a tool call, or undefined to let it run. */
export function decide(input: HookInput, home = os.homedir()) {
  const session = input.cwd ? path.resolve(input.cwd) : process.cwd()
  const tool = input.tool_name ?? ""
  const args = input.tool_input ?? {}
  if (tool === "Bash") {
    const command = string(args.command)
    return command ? bash(command, session, home) : undefined
  }
  return builtin(tool, args, session)
}

/** What makes two calls the same for the resend rule: a Bash command's text, or another tool's arguments in key order, without a description. */
function identity(input: HookInput) {
  const args = input.tool_input ?? {}
  if (input.tool_name === "Bash") return `Bash\0${string(args.command) ?? ""}`
  const kept = Object.keys(args).filter((key) => key !== "description").sort().map((key) => [key, args[key]])
  return `${input.tool_name}\0${JSON.stringify(kept)}`
}

/** The hook's state folder: the per-user runtime folder when there is one, else the user's cache, never a folder another user could have made. */
function stateDirectory() {
  return process.env.XDG_RUNTIME_DIR ? path.join(process.env.XDG_RUNTIME_DIR, "nib-hook") : path.join(os.homedir(), ".cache", "nib", "hook")
}

/**
 * True the first time this session makes this call; a resend means the agent has no nib, or a reason to insist. A state folder that is not this
 * user's own and private, or a state file that is a link, turns refusals off instead of being trusted.
 */
async function firstTime(session: string, call: string, directory = stateDirectory()) {
  await mkdir(directory, { recursive: true, mode: 0o700 }).catch(() => undefined)
  const info = await lstat(directory).catch(() => undefined)
  if (!info?.isDirectory() || (process.getuid && info.uid !== process.getuid()) || (info.mode & 0o077) !== 0) return false
  const file = path.join(directory, `${session.replace(/[^\w-]/g, "_") || "session"}.txt`)
  const digest = createHash("sha256").update(call).digest("hex").slice(0, 16)
  const handle = await open(file, constants.O_RDWR | constants.O_CREAT | constants.O_APPEND | constants.O_NOFOLLOW, 0o600).catch(() => undefined)
  if (!handle) return false
  try {
    if ((await handle.readFile("utf8")).split("\n").includes(digest)) return false
    await handle.write(`${digest}\n`)
    return true
  } finally {
    await handle.close()
  }
}

/** Reads one hook call from stdin and prints Claude Code's deny decision when nib should take the call; prints nothing to let it run. */
export async function runHook(stdin: string, state?: string) {
  let input: HookInput
  try {
    input = JSON.parse(stdin) as HookInput
  } catch {
    return ""
  }
  const reason = decide(input)
  if (!reason || !await firstTime(input.session_id ?? "", identity(input), state)) return ""
  return JSON.stringify({ hookSpecificOutput: { hookEventName: "PreToolUse", permissionDecision: "deny", permissionDecisionReason: reason } })
}
