package server

// instructionLimit is where clients cut server instructions and tool descriptions.
const instructionLimit = 2048

// shellInstructions replace toolInstructions when nibash is offered: the shell is nib's too, so shell views anchor edits.
const shellInstructions = `Hashline file tools and nibash, nib's shell; use them instead of the built-in Read, Grep, Glob, Edit, Write and Bash (built-in Read stays for PDFs and notebooks).
read and grep print a [PATH#TAG] header (TAG: 4-hex snapshot of the whole file) and LINE:TEXT rows; so do cat, head, tail, grep, rg, sed -n and wc in nibash when their output comes to you. edit addresses those line numbers, so never repeat old text.
Save calls and tokens: grep for a name instead of reading a whole file; read several files and ranges in one read; edit straight from grep rows; put every change, across files, into one edit call; after an edit or write, use the new tag and rows from its response instead of re-reading.`

// namedHeader introduces the named directories at the end of the instructions.
const namedHeader = "Named directories: ~name/ paths work in every tool and, unquoted, in nibash; nib prints paths that way. They mean nothing elsewhere."

// Schemas are lean: nib reads and checks arguments itself, so they only tell the model what to send.
const readSchema = `{"type":"object","properties":{"paths":{"description":"Required: files or directories, each optionally with a :selector, or path#TAG","anyOf":[{"type":"array","items":{"type":"string"}},{"type":"string"}]},"maxLineLength":{"description":"Default 2000","type":"integer"},"plain":{"description":"For logs and files you will not edit: no numbers or tags, color codes stripped, long lines shortened, repeats collapsed; any size","type":"boolean"},"collapse":{"description":"Plain only: false keeps repeated lines","type":"boolean"},"info":{"description":"One line of facts per file (tag, lines, size, line endings, age) instead of its content","type":"boolean"}}}`

const grepSchema = `{"type":"object","properties":{"pattern":{"description":"Regex; with literal:true a plain string","type":"string"},"paths":{"description":"Files or directories; a file may carry a read selector like log.txt:100-250. Default the project","anyOf":[{"type":"array","items":{"type":"string"}},{"type":"string"}]},"glob":{"description":"Like *.ts or !*.test.ts, or a list","anyOf":[{"type":"string"},{"type":"array","items":{"type":"string"}}]},"ignoreCase":{"type":"boolean"},"literal":{"type":"boolean"},"before":{"description":"Context lines before each match; default 1, at most 20","type":"integer"},"after":{"description":"Context lines after each match; default 3, at most 20. Past 40 context lines in all, definitions get them first","type":"integer"},"mode":{"description":"matches (default); files: matching paths only; count: matches per file","type":"string"},"noIgnore":{"description":"Also search ignored files","type":"boolean"},"maxLineLength":{"description":"Default 512","type":"integer"},"skip":{"description":"Files to skip, for the next page","type":"integer"},"replace":{"description":"Replacement with $1 or ${name}; previews the changes and returns a token for edit","type":"string"}},"required":["pattern"]}`

const findSchema = `{"type":"object","properties":{"paths":{"description":"Directories or files to list; default the project","anyOf":[{"type":"array","items":{"type":"string"}},{"type":"string"}]},"glob":{"description":"Like *.ts, src/**/test_*.py or !*.min.js, or a list","anyOf":[{"type":"string"},{"type":"array","items":{"type":"string"}}]},"minSize":{"description":"Like 100k or 2M","type":"string"},"newer":{"description":"Modified within, like 30m, 2h or 3d","type":"string"},"sort":{"description":"path (default), size (largest first) or modified (newest first)","type":"string","enum":["path","size","modified"]},"noIgnore":{"description":"Also list ignored files","type":"boolean"},"skip":{"description":"Files to skip, for the next page","type":"integer"}}}`

const editSchema = `{"type":"object","properties":{"input":{"description":"The complete hashline patch, or REAPPLY TOKEN from a refusal or a grep replace","type":"string"}},"required":["input"]}`

const writeSchema = `{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"},"tag":{"description":"Required to overwrite an existing file","type":"string"}},"required":["path","content"]}`

const nibashSchema = `{"type":"object","properties":{"command":{"type":"string"},"description":{"description":"What it is for, in 5 to 12 words","type":"string"},"timeout":{"description":"Milliseconds before it goes on as a job; default 120000, at most 600000","type":"integer"},"background":{"description":"Run as a job; returns its id and log at once","type":"boolean"}},"required":["command"]}`
