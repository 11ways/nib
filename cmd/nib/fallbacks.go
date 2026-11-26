package main

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/11ways/nib/internal/fallback"
)

// fallbacksRequest is what nib stats fallbacks is asked: events from since on, how many reasons to list, the hosts whose logs join this
// one's over ssh, and whether to print the events as JSON lines instead, as a host asked over ssh does.
type fallbacksRequest struct {
	since time.Time
	limit int
	hosts []string
	json  bool
}

// fallbacksFlags reads --since, --limit, --host (repeatable) and --json, each value as --name VALUE or --name=VALUE.
func fallbacksFlags(args []string, now time.Time) (fallbacksRequest, error) {
	r := fallbacksRequest{limit: 30}
	for i := 0; i < len(args); i++ {
		name, value, inline := strings.Cut(args[i], "=")
		switch name {
		case "--json":
			r.json = true
			continue
		case "--since", "--limit", "--host":
		default:
			return r, fmt.Errorf("unknown option %s", args[i])
		}
		if !inline {
			if i+1 == len(args) {
				return r, fmt.Errorf("%s needs a value", name)
			}
			i++
			value = args[i]
		}
		switch name {
		case "--limit":
			n, err := strconv.Atoi(value)
			if err != nil || n < 0 {
				return r, fmt.Errorf("--limit takes a number, not %q", value)
			}
			r.limit = n
		case "--host":
			r.hosts = append(r.hosts, value)
		default:
			since, err := sinceTime(value, now)
			if err != nil {
				return r, err
			}
			r.since = since
		}
	}
	return r, nil
}

// hostedEvent is an event with the machine whose log it came from, "" for this one.
type hostedEvent struct {
	fallback.Event
	host string
}

// runFallbacks prints what nib's own commands handed to a real program, by reason and form, from this machine's log and the hosts asked.
func runFallbacks(args []string) int {
	r, err := fallbacksFlags(args, time.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "nib stats fallbacks: %v\n", err)
		return 2
	}
	path := fallback.Default(os.Getenv)
	if path == "" {
		fmt.Fprintln(os.Stderr, "nib stats fallbacks: NIB_FALLBACK_LOG is off, or there is no home folder to keep the log in")
		return 1
	}
	local, err := fallback.Read(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nib stats fallbacks: %v\n", err)
		return 1
	}
	var events []hostedEvent
	for _, e := range local {
		if !e.Time.Before(r.since) {
			events = append(events, hostedEvent{e, ""})
		}
	}
	if r.json {
		out := json.NewEncoder(os.Stdout)
		for _, e := range events {
			out.Encode(e.Event)
		}
		return 0
	}
	sources := []string{path}
	status := 0
	for _, host := range r.hosts {
		remote, err := hostFallbacks(host, r.since)
		if err != nil {
			fmt.Fprintf(os.Stderr, "nib stats fallbacks: %s: %v\n", host, err)
			status = 1
			continue
		}
		sources = append(sources, host)
		for _, e := range remote {
			events = append(events, hostedEvent{e, host})
		}
	}
	reportFallbacks(os.Stdout, sources, events, r.limit)
	return status
}

// hostFallbacks is host's log from since on, read by the nib in its ~/.local/bin or on its PATH, over ssh.
func hostFallbacks(host string, since time.Time) ([]fallback.Event, error) {
	command := `sh -c 'PATH="$HOME/.local/bin:$HOME/go/bin:$PATH" exec nib stats fallbacks --json`
	if !since.IsZero() {
		command += " --since " + since.Format(time.RFC3339)
	}
	command += `'`
	var stderr bytes.Buffer
	cmd := exec.Command("ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", host, command)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%v: %s", err, strings.TrimSpace(stderr.String()))
	}
	var events []fallback.Event
	for _, line := range bytes.Split(out, []byte("\n")) {
		var e fallback.Event
		if json.Unmarshal(line, &e) == nil && e.Command != "" {
			events = append(events, e)
		}
	}
	return events, nil
}

// fallbackGroup is one reason a command was handed to a real program: its count, the forms it came in, and the hosts it came from.
type fallbackGroup struct {
	command, kind, reason string
	count                 int
	forms                 map[string]int
	hosts                 map[string]bool
}

// fallbackKinds are the kinds in the order the report lists them, what nib lacks first.
var fallbackKinds = []string{fallback.Missing, fallback.Machine, fallback.Error}

// fallbackKindTitles say what each kind's section counts.
var fallbackKindTitles = map[string]string{
	fallback.Missing: "forms nib's own lacks",
	fallback.Machine: "forms whose output follows the locale or the machine",
	fallback.Error:   "forms bash or the system program reports as errors",
}

// reportFallbacks writes the totals, then per kind the reasons by count with their commonest forms, up to limit reasons each.
func reportFallbacks(w io.Writer, sources []string, events []hostedEvent, limit int) {
	fmt.Fprintf(w, "fallbacks from %s\n", strings.Join(sources, ", "))
	if len(events) == 0 {
		fmt.Fprintln(w, "none recorded")
		return
	}
	first, last := events[0].Time, events[0].Time
	groups := map[[3]string]*fallbackGroup{}
	total := 0
	for _, e := range events {
		if e.Time.Before(first) {
			first = e.Time
		}
		if e.Time.After(last) {
			last = e.Time
		}
		key := [3]string{e.Command, e.Kind, e.Reason}
		g := groups[key]
		if g == nil {
			g = &fallbackGroup{command: e.Command, kind: e.Kind, reason: e.Reason, forms: map[string]int{}, hosts: map[string]bool{}}
			groups[key] = g
		}
		count := max(e.Count, 1)
		g.count += count
		g.forms[e.Form] += count
		g.hosts[cmp.Or(e.host, "local")] = true
		total += count
	}
	fmt.Fprintf(w, "%d fallbacks, %s to %s\n", total, first.Local().Format("2006-01-02 15:04"), last.Local().Format("2006-01-02 15:04"))
	for _, kind := range fallbackKinds {
		var list []*fallbackGroup
		sum := 0
		for _, g := range groups {
			if g.kind == kind {
				list = append(list, g)
				sum += g.count
			}
		}
		if len(list) == 0 {
			continue
		}
		slices.SortFunc(list, func(a, b *fallbackGroup) int {
			return cmp.Or(cmp.Compare(b.count, a.count), cmp.Compare(a.command, b.command), cmp.Compare(a.reason, b.reason))
		})
		fmt.Fprintf(w, "\n%s (%s): %d\n", kind, fallbackKindTitles[kind], sum)
		for i, g := range list {
			if i == limit {
				fmt.Fprintf(w, "  ... %d more reasons\n", len(list)-limit)
				break
			}
			hosts := slices.Sorted(maps.Keys(g.hosts))
			fmt.Fprintf(w, "%7d  %-7s %s  [%s]\n", g.count, g.command, g.reason, strings.Join(hosts, ", "))
			forms := make([]string, 0, len(g.forms))
			for form := range g.forms {
				forms = append(forms, form)
			}
			slices.SortFunc(forms, func(a, b string) int { return cmp.Or(cmp.Compare(g.forms[b], g.forms[a]), cmp.Compare(a, b)) })
			for j, form := range forms {
				if j == 3 {
					fmt.Fprintf(w, "%16s %d more forms\n", "...", len(forms)-3)
					break
				}
				fmt.Fprintf(w, "%16d  %s\n", g.forms[form], form)
			}
		}
	}
}
