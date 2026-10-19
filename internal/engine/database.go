package engine

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/11ways/nib/internal/sqlitero"
)

const (
	// databaseRows bounds the rows one read of a table fetches; the budget usually shows fewer.
	databaseRows = 500
	// cellChars is the most of one value a row shows.
	cellChars = 120
)

// isDatabase reports whether file starts like a SQLite 3 database.
func isDatabase(file *os.File) bool {
	start := make([]byte, len(sqlitero.Magic))
	n, _ := file.ReadAt(start, 0)
	return bytes.Equal(start[:n], sqlitero.Magic)
}

// databaseTable resolves input as DATABASE:TABLE, the path a row selector was split from, when DATABASE is a SQLite database.
func databaseTable(ctx *Context, input string) (*target, string) {
	at := strings.LastIndex(input, ":")
	if at <= 0 || at == len(input)-1 {
		return nil, ""
	}
	t, err := resolveTarget(input[:at], ctx, false)
	if err != nil {
		return nil, ""
	}
	file, err := os.Open(t.canonical)
	if err != nil {
		return nil, ""
	}
	defer file.Close()
	if !isDatabase(file) {
		return nil, ""
	}
	return t, input[at+1:]
}

// quoted is name as an SQL identifier.
func quoted(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

// dbObject is a table or view of a database, with its columns' names and declared types.
type dbObject struct {
	kind, name     string
	columns, types []string
	// primary are the primary key's columns in key order; order is how rows are ordered, as said in a heading.
	primary []string
	noRowid bool
}

// databaseBlock reads a SQLite database read-only: without tables, an outline of its tables and views; with them, their rows by position.
// Values are shown, never anchors, and nothing counts as seen.
func (e *Engine) databaseBlock(ctx *Context, t *target, sel selection) (*block, error) {
	db, err := sqlitero.Open(t.canonical)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	background := ctx.Ctx
	if background == nil {
		background = context.Background()
	}
	objects, err := databaseObjects(background, db)
	if err != nil {
		return noteBlock(fmt.Sprintf("[%s: %s]", t.display, err)), nil
	}
	if len(sel.symbols) == 0 {
		if sel.ranges != nil || sel.tail > 0 {
			return noteBlock(fmt.Sprintf("[%s is a SQLite database, read by table: %s:TABLE:%s]", t.display, t.display, selectorText(sel))), nil
		}
		return databaseOutline(background, db, t, objects)
	}
	var sections []*block
	for _, name := range sel.symbols {
		var found *dbObject
		for i := range objects {
			if objects[i].name == name {
				found = &objects[i]
			}
		}
		if found == nil {
			var names []string
			for _, o := range objects {
				names = append(names, o.name)
			}
			return noteBlock(fmt.Sprintf("[%s has no table or view %s; it has %s]", t.display, name, strings.Join(names, ", "))), nil
		}
		section, err := databaseRowsOf(background, db, t, found, sel)
		if err != nil {
			return nil, err
		}
		sections = append(sections, section)
	}
	if len(sections) == 1 {
		return sections[0], nil
	}
	// Several tables share one block: each heading becomes a row above its rows, and each notes what it left out.
	joined := &block{head: "[" + t.display + "]"}
	var ends []int
	var finishes []func(int) []string
	for _, s := range sections {
		joined.rows = append(joined.rows, blockRow{text: strings.TrimPrefix(s.head, "["+t.display+"] ")})
		joined.rows = append(joined.rows, s.rows...)
		ends = append(ends, len(joined.rows))
		finishes = append(finishes, s.finish)
	}
	joined.finish = func(shown int) []string {
		var notes []string
		start := 0
		for i, end := range ends {
			// A section's rows start after its heading row.
			notes = append(notes, finishes[i](max(0, min(shown, end)-start-1))...)
			start = end
		}
		return notes
	}
	return joined, nil
}

// selectorText writes a row selector back as typed.
func selectorText(sel selection) string {
	if sel.tail > 0 {
		return "-" + strconv.Itoa(sel.tail)
	}
	var parts []string
	for _, r := range sel.ranges {
		switch {
		case r.end == 0:
			parts = append(parts, fmt.Sprintf("%d-", r.start))
		case r.end == r.start:
			parts = append(parts, strconv.Itoa(r.start))
		default:
			parts = append(parts, fmt.Sprintf("%d-%d", r.start, r.end))
		}
	}
	return strings.Join(parts, ",")
}

// databaseObjects lists the tables and views, sqlite's own left out, in the order they were created.
func databaseObjects(ctx context.Context, db *sql.DB) ([]dbObject, error) {
	rows, err := db.QueryContext(ctx, `select type, name from sqlite_schema where type in ('table', 'view') and name not like 'sqlite\_%' escape '\' order by rowid`)
	if err != nil {
		return nil, err
	}
	var objects []dbObject
	for rows.Next() {
		var o dbObject
		if err := rows.Scan(&o.kind, &o.name); err != nil {
			rows.Close()
			return nil, err
		}
		objects = append(objects, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range objects {
		o := &objects[i]
		columns, err := db.QueryContext(ctx, "select name, type, pk from pragma_table_info(?)", o.name)
		if err != nil {
			return nil, err
		}
		keys := map[int]string{}
		for columns.Next() {
			var name, kind string
			var pk int
			if err := columns.Scan(&name, &kind, &pk); err != nil {
				columns.Close()
				return nil, err
			}
			o.columns, o.types = append(o.columns, name), append(o.types, kind)
			if pk > 0 {
				keys[pk] = name
			}
		}
		columns.Close()
		for k := 1; k <= len(keys); k++ {
			o.primary = append(o.primary, keys[k])
		}
		if o.kind == "table" {
			var wr int
			if err := db.QueryRowContext(ctx, "select wr from pragma_table_list where name = ? and schema = 'main'", o.name).Scan(&wr); err == nil {
				o.noRowid = wr == 1
			}
		}
	}
	return objects, nil
}

func tableSize(ctx context.Context, db *sql.DB, name string) (int, error) {
	var total int
	err := db.QueryRowContext(ctx, "select count(*) from "+quoted(name)).Scan(&total)
	return total, err
}

// databaseOutline is a row per table and view: its row count and its columns with their declared types.
func databaseOutline(ctx context.Context, db *sql.DB, t *target, objects []dbObject) (*block, error) {
	head := "[" + t.display + "] SQLite database, read-only"
	if len(objects) == 0 {
		return noteBlock(head + ", no tables"), nil
	}
	b := &block{head: head}
	for _, o := range objects {
		count := "rows unknown"
		if total, err := tableSize(ctx, db, o.name); err == nil {
			count = plural(total, "row")
		}
		facts := []string{o.kind + " " + o.name, count}
		if o.noRowid {
			facts = append(facts, "without rowid")
		}
		columns := make([]string, len(o.columns))
		for i, name := range o.columns {
			columns[i] = strings.TrimSpace(name + " " + o.types[i])
		}
		b.rows = append(b.rows, blockRow{text: strings.Join(facts, ", ") + ": " + strings.Join(columns, ", ")})
	}
	b.finish = func(shown int) []string {
		var notes []string
		if shown < len(b.rows) {
			notes = append(notes, fmt.Sprintf("[%d more tables and views not shown]", len(b.rows)-shown))
		}
		return append(notes, fmt.Sprintf("[read %s:TABLE for its rows, %s:TABLE:-15 for the last 15]", t.display, t.display))
	}
	return b, nil
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// databaseRowsOf is the rows of one table or view at the positions sel asks, in rowid order, primary key order for a table without rowid,
// and the view's own order for a view.
func databaseRowsOf(ctx context.Context, db *sql.DB, t *target, o *dbObject, sel selection) (*block, error) {
	total, err := tableSize(ctx, db, o.name)
	if err != nil {
		return noteBlock(fmt.Sprintf("[%s: %s %s: %s]", t.display, o.kind, o.name, err)), nil
	}
	order, orderBy := "in rowid order", " order by rowid"
	switch {
	case o.kind == "view":
		order, orderBy = "in the view's order", ""
	case o.noRowid:
		keys := make([]string, len(o.primary))
		for i, k := range o.primary {
			keys[i] = quoted(k)
		}
		order, orderBy = "in primary key order", " order by "+strings.Join(keys, ", ")
	}
	heading := fmt.Sprintf("[%s] %s %s", t.display, o.kind, o.name)
	if total == 0 {
		return noteBlock(heading + ", no rows"), nil
	}
	// An open range runs to the last row rather than read's line count.
	sel.ranges = slices.Clone(sel.ranges)
	if sel.tail == 0 && sel.ranges == nil {
		sel.ranges = []lineRange{{1, total}}
	}
	for i := range sel.ranges {
		if sel.ranges[i].end == 0 {
			sel.ranges[i].end = max(total, sel.ranges[i].start)
		}
	}
	spans, err := selectLines(sel, total)
	if err != nil {
		return noteBlock(fmt.Sprintf("%s has %s; nothing at the positions asked", heading, plural(total, "row"))), nil
	}
	// A unary plus leaves a value as it is but drops the column's declared type, so the driver never turns a DATETIME's text into a time.
	values := make([]string, len(o.columns))
	for i, c := range o.columns {
		values[i] = "+" + quoted(c)
	}
	query := "select " + strings.Join(values, ", ") + " from " + quoted(o.name) + orderBy + " limit ? offset ?"
	b := &block{rows: []blockRow{{text: strings.Join(o.columns, " | ")}}}
	var positions []int
	var unfetched []span2
	fetched := 0
	for _, span := range spans {
		end := min(span.end, span.start+databaseRows-fetched-1)
		if end < span.start {
			unfetched = append(unfetched, span)
			continue
		}
		if end < span.end {
			unfetched = append(unfetched, span2{end + 1, span.end})
		}
		rows, err := db.QueryContext(ctx, query, end-span.start+1, span.start-1)
		if err != nil {
			return nil, err
		}
		position := span.start
		for rows.Next() {
			cells := make([]any, len(o.columns))
			pointers := make([]any, len(cells))
			for i := range cells {
				pointers[i] = &cells[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				rows.Close()
				return nil, err
			}
			texts := make([]string, len(cells))
			for i, v := range cells {
				texts[i] = cell(v)
			}
			b.rows = append(b.rows, blockRow{text: strings.Join(texts, " | ")})
			positions = append(positions, position)
			position++
			fetched++
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	shownSpans := func(count int) string {
		var parts []string
		for i := 0; i < count; {
			j := i
			for j+1 < count && positions[j+1] == positions[j]+1 {
				j++
			}
			parts = append(parts, spanText(positions[i], positions[j]))
			i = j + 1
		}
		return strings.Join(parts, ", ")
	}
	b.head = fmt.Sprintf("%s, rows %s of %d, %s", heading, shownSpans(len(positions)), total, order)
	b.finish = func(shown int) []string {
		// The first row is the columns' names.
		kept := max(0, shown-1)
		var left []span2
		for i := kept; i < len(positions); i++ {
			left = append(left, span2{positions[i], positions[i]})
		}
		left = append(left, unfetched...)
		slices.SortFunc(left, func(a, b span2) int { return a.start - b.start })
		var merged []span2
		for _, s := range left {
			if n := len(merged); n > 0 && merged[n-1].end+1 >= s.start {
				merged[n-1].end = max(merged[n-1].end, s.end)
			} else {
				merged = append(merged, s)
			}
		}
		left = merged
		if len(left) == 0 {
			return nil
		}
		var parts []string
		for _, s := range left {
			parts = append(parts, spanText(s.start, s.end))
		}
		return []string{fmt.Sprintf("[rows %s not shown; read %s:%s:%d+50 for more]", strings.Join(parts, ", "), t.display, o.name, left[0].start)}
	}
	return b, nil
}

func spanText(start, end int) string {
	if start == end {
		return strconv.Itoa(start)
	}
	return fmt.Sprintf("%d-%d", start, end)
}

// cellEscapes keep a value on its row and its columns apart.
var cellEscapes = strings.NewReplacer(`\`, `\\`, "\n", `\n`, "\r", `\r`, "\t", `\t`, "|", `\|`)

// cell is a value as a row shows it: NULL, a number, a blob's size, or text escaped and cut to cellChars characters.
func cell(value any) string {
	switch v := value.(type) {
	case nil:
		return "NULL"
	case []byte:
		return fmt.Sprintf("<blob %d bytes>", len(v))
	case string:
		text := []rune(cellEscapes.Replace(v))
		if len(text) > cellChars {
			return fmt.Sprintf("%s... (+%d chars)", string(text[:cellChars]), len(text)-cellChars)
		}
		return string(text)
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64)
	default:
		return fmt.Sprint(v)
	}
}
