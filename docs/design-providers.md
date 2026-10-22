# Providers: other tools hosted inside nib

A provider is a program nib starts and talks to privately, whose tools nib offers as its own. The client configures one
MCP server, nib; the provider's tools appear as `NAME_TOOL` (zemble's `search` as `zemble_search`). nib, never the
provider, shows file lines: the provider says where code is, and nib prints those lines from the file as it is now, as
`read` prints them, so every `N:TEXT` row is a real line, its tag is the file's, and the lines count as seen for the
agent that called. Edits from a provider's answer are then exactly as safe as edits from a read.

## Configuration

`nib --provider NAME=PROGRAM` (repeatable), or `nib setup --provider NAME=PROGRAM`, which puts the flag in nib's server
entries and removes the server entries and Claude Code registrations named `NAME` that it replaces (a project setup only
names a user-scope one); `NAME=` stops hosting it, and a setup without `--provider` keeps what an entry hosts and gives
a new one the providers remembered from earlier `--provider`s (`~/.config/nib/setup.json`). `NAME` matches
`[a-z][a-z0-9]*`. Under `NIB_SANDBOX=1` providers are off unless `NIB_PROVIDERS_SANDBOX=1` is set too: a provider runs
outside the sandbox and may send code elsewhere (zemble's embedder does), so hosting one there is the user's explicit
choice. The rows nib prints for it are still confined to the roots.

## Protocol, version 1

- nib runs `PROGRAM nib-provider` with nib's project directory as the working directory and nib's environment. The
  program speaks MCP over stdio.
- At startup nib lists the provider's tools once. A tool name matches `[a-z][a-z0-9_]*`; its description is at most 2048
  characters, or nib leaves the tool out and says why on stderr. The input schema and annotations pass through
  unchanged.
- A tool result's `structuredContent` is `{"nib": 1, "parts": [...]}`, in order:
  - `{"text": "..."}`: prose, shown as it is. It never carries file lines; code always comes as a location.
  - `{"path": "/absolute/file", "start": 10, "end": 20, "indexed": "..."}`: lines `start` to `end` of a file, shown as
    `[PATH#TAG]` and `N:TEXT` rows. `indexed`, optional, is the text of those lines (joined by `\n`) as the provider saw
    them, so nib can tell when the file changed since.
- Locations of one file that follow each other with no text between them print as one block.
- `content` still carries a plain-text rendering for clients without nib. nib uses it only when `structuredContent` is
  missing, has another `nib` version, or holds a part it does not know: then the text is shown as prose and no rows are
  printed, so nothing is ever guessed.
- An `isError` result is shown as an error, with its text.

## What nib does with an answer

- Prose and rows share read's 40 KiB budget; past it the answer is cut with a note.
- A location whose `indexed` text no longer matches the file is looked for: found exactly once, its rows print where the
  lines are now, with a note saying they moved; otherwise the rows print at the given lines with a note that the
  provider's index is older than the file.
- A prose line shaped like a row (`12:text`) follows a note that the provider's text is not anchors.
- A path the sandbox refuses, or one that does not exist, is noted in its place, as in a read.

## Lifecycle

Providers start with nib, before it answers the client, so their tools are listed from the first request; one that fails
to start or to list its tools within 30 seconds is left out with a note on stderr. A provider may begin work for nib's
project directory as it starts, as zemble warms its daemon's index and symbol graph there, so the agent's first question
is answered warm; nib waits for none of it. A provider whose process ended is started again on the next call to one of
its tools; a call it was answering when it ended fails, saying so, and is not sent again, since nib cannot know what it
did.
