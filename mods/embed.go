// Package mods carries the Claude Code mods nib ships, so `nib setup` installs the ones that fit the binary that runs it.
package mods

import "embed"

// Nibash is the nibash mod's files under "nibash": what Claude Code loads, without the tests or the types the engine lays beside a loaded mod.
//
//go:embed nibash/.claude-plugin/plugin.json nibash/hooks nibash/types nibash/tsconfig.json
var Nibash embed.FS
