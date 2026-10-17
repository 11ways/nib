package nibash

import (
	"os"
	"regexp"
	"strings"
)

// listerPlan runs a real program that prints one path a line, find or ls, with the command line's own arguments, and prints each folder of
// its runs of paths once. A line counts as a path only if it names a file that is there, so nothing else it prints is ever taken for one.
type listerPlan struct {
	name  string
	args  []string
	parse func(dir string) func(string) (listed, bool)
}

func (p *listerPlan) run(inv *invocation) int {
	command := append([]string{p.name}, p.args...)
	parse := p.parse(inv.dir)
	if inv.out.link != nil {
		return streamed(inv, command, func(line string) record { return listingRecord(line, parse) })
	}
	lines, status := captured(inv, command, false)
	emitListing(inv, lines, parse)
	return status
}

// existing is whether path, made absolute against dir, names a file or a directory that is there, symlinks not followed.
func existing(dir, path string) bool {
	_, err := os.Lstat(absolute(dir, path))
	return err == nil
}

// findTests are find's tests and options that print nothing and act on nothing, with how many words follow each; anything else, an action
// like -exec, -delete, -printf or -print0 included, is a form nib's find does not handle.
var findTests = map[string]int{
	"-name": 1, "-iname": 1, "-path": 1, "-ipath": 1, "-wholename": 1, "-iwholename": 1, "-regex": 1, "-iregex": 1, "-lname": 1, "-ilname": 1,
	"-type": 1, "-xtype": 1, "-maxdepth": 1, "-mindepth": 1, "-newer": 1, "-anewer": 1, "-cnewer": 1, "-mtime": 1, "-mmin": 1, "-atime": 1,
	"-amin": 1, "-ctime": 1, "-cmin": 1, "-size": 1, "-perm": 1, "-user": 1, "-group": 1, "-uid": 1, "-gid": 1, "-links": 1, "-inum": 1,
	"-samefile": 1, "-regextype": 1, "-used": 1,
	"-empty": 0, "-print": 0, "-not": 0, "!": 0, "-o": 0, "-or": 0, "-a": 0, "-and": 0, "(": 0, ")": 0, ",": 0, "-prune": 0, "-quit": 0,
	"-true": 0, "-false": 0, "-readable": 0, "-writable": 0, "-executable": 0, "-nouser": 0, "-nogroup": 0, "-xdev": 0, "-mount": 0,
	"-depth": 0, "-d": 0, "-noleaf": 0, "-ignore_readdir_race": 0, "-noignore_readdir_race": 0, "-daystart": 0, "-follow": 0,
}

func prepareFind(name string, args []string) (plan, error) {
	i := 0
	for i < len(args) && (args[i] == "-P" || args[i] == "-H" || args[i] == "-L") {
		i++
	}
	// Starting points come first; the expression starts at the first word that begins with - or is ( or !.
	for i < len(args) && !strings.HasPrefix(args[i], "-") && args[i] != "(" && args[i] != "!" {
		i++
	}
	for ; i < len(args); i++ {
		words, ok := findTests[args[i]]
		if !ok {
			return nil, errUnsupported
		}
		if i+words >= len(args) {
			return nil, errUnsupported
		}
		i += words
	}
	return &listerPlan{name: name, args: args, parse: pathLines}, nil
}

// pathLines reads a line that is one path, as find and ls -1 print them.
func pathLines(dir string) func(string) (listed, bool) {
	return func(line string) (listed, bool) {
		if line == "" || !existing(dir, line) {
			return listed{}, false
		}
		return listed{path: line}, true
	}
}

// lsFlags are the options of ls nib's ls handles, as letters that may bundle; none changes a name ls prints, or the line fails its check.
const lsFlags = "1aAlhRtSrdgGoFpXUvc"

// lsLong is a line of ls -l: the mode, links, owner and group (either may be left out), size, date and then the name; a symlink's "-> target"
// fails the check that the rest names a file.
var lsLong = regexp.MustCompile(`^([-dlcbps][-rwxsStT]{9}[.+@]? +\d+ +(?:\S+ +){0,2}[\d,.]+[KMGTPE]? +\S+ +\S+ +\S+ )(.+)$`)

func prepareLs(name string, args []string) (plan, error) {
	for _, a := range args {
		switch {
		case a == "--":
		case a == "--color=never" || a == "--group-directories-first" || a == "--all" || a == "--almost-all" || a == "--human-readable" ||
			a == "--reverse" || a == "--recursive" || a == "--directory":
		case strings.HasPrefix(a, "--"):
			return nil, errUnsupported
		case strings.HasPrefix(a, "-") && a != "-":
			if strings.Trim(a[1:], lsFlags) != "" {
				return nil, errUnsupported
			}
		}
	}
	return &listerPlan{name: name, args: args, parse: lsLines}, nil
}

// lsLines reads a line of ls that names one path: a whole line, or what follows the columns of ls -l.
func lsLines(dir string) func(string) (listed, bool) {
	whole := pathLines(dir)
	return func(line string) (listed, bool) {
		if m := lsLong.FindStringSubmatch(line); m != nil {
			if existing(dir, m[2]) {
				return listed{before: m[1], path: m[2]}, true
			}
			return listed{}, false
		}
		return whole(line)
	}
}
