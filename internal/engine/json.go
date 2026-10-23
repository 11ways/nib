package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/11ways/nib/internal/snapshots"
	"github.com/11ways/nib/internal/syntax"
)

// jsonProblem is the first way text fails as JSON: a syntax error, or a key that repeats within one object, which parsers resolve silently by
// keeping one of them; "" when it is sound. Lines count from 1.
func jsonProblem(text string) string {
	line := func(offset int64) int { return strings.Count(text[:min(int(offset), len(text))], "\n") + 1 }
	type frame struct {
		object bool
		key    bool
		keys   map[string]int
	}
	var stack []*frame
	decoder := json.NewDecoder(strings.NewReader(text))
	// done is set once the top-level value is complete; JSON has one, where the decoder would read a stream of them.
	done := false
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return ""
		}
		if err != nil {
			// The token reader words some errors as if a value were expected; the parser's own message names the real fault.
			var syntax *json.SyntaxError
			var value any
			if errors.As(json.Unmarshal([]byte(text), &value), &syntax) || errors.As(err, &syntax) {
				return fmt.Sprintf("line %d: %s", line(syntax.Offset), syntax.Error())
			}
			return err.Error()
		}
		if len(stack) == 0 && done {
			return fmt.Sprintf("line %d: a second value follows the first", line(decoder.InputOffset()))
		}
		var top *frame
		if len(stack) > 0 {
			top = stack[len(stack)-1]
		}
		if delim, ok := token.(json.Delim); ok && (delim == '}' || delim == ']') {
			stack = stack[:len(stack)-1]
			done = len(stack) == 0
			continue
		}
		if top != nil && top.object && top.key {
			key := token.(string)
			at := line(decoder.InputOffset())
			if first, ok := top.keys[key]; ok {
				return fmt.Sprintf("line %d: key %q repeats in its object (first on line %d)", at, key, first)
			}
			top.keys[key] = at
			top.key = false
			continue
		}
		if top != nil && top.object {
			top.key = true
		}
		if delim, ok := token.(json.Delim); ok {
			stack = append(stack, &frame{object: delim == '{', key: delim == '{', keys: map[string]int{}})
		} else if len(stack) == 0 {
			done = true
		}
	}
}

// dataWarnings warns when a data file (.json, or a format nib has a grammar for) that was sound, or is new (before nil), is no longer: a file
// that never parsed, like JSON with comments, is left alone. One too big to parse on every change says so once.
func dataWarnings(ctx *Context, store *snapshots.Store, file string, before, after []string) []string {
	if !strings.EqualFold(filepath.Ext(file), ".json") && !syntax.DataFormat(file) {
		return nil
	}
	text := strings.Join(after, "\n")
	format := "JSON"
	if syntax.DataFormat(file) {
		format = syntax.Language(file)
		if len(text) > checkBytes {
			if store.Teach(ctx.Session, "unchecked-size:"+file) {
				return []string{fmt.Sprintf("Changes to this file get no %s check: it is over %d KiB, too big to parse on every change.", format, checkBytes>>10)}
			}
			return nil
		}
	}
	problem := dataProblem(file, text)
	if problem == "" || (before != nil && dataProblem(file, strings.Join(before, "\n")) != "") {
		return nil
	}
	return []string{fmt.Sprintf("The file is not valid %s now: %s.", format, problem)}
}

// dataProblem is the first way text fails as the data format of file, "" when it is sound.
func dataProblem(file, text string) string {
	if !syntax.DataFormat(file) {
		return jsonProblem(text)
	}
	found, err := syntax.Inspect(file, []byte(text))
	if err != nil || found.ErrorLine == 0 {
		return ""
	}
	return fmt.Sprintf("line %d holds its first error", found.ErrorLine)
}
