package syntax

import (
	"fmt"
	"strings"
	"testing"

	grammarruntime "github.com/odvcencio/gotreesitter/grammars/runtime"
)

const javaSrc = `
package x;

@Service
@RequestMapping(value = "/api", produces = "json")
public class Repo<K extends Comparable<K>> implements Store {
    private static final Map<String, Runnable> handlers = new HashMap<>();

    private final Comparator<String> byLen = (a, b) -> {
        return a.length() - b.length();
    };

    public Repo(Store store) {
        this.store = store;
    }

    public <T> List<T> find(Class<T> type) {
        return null;
    }

    public static Map<String, List<Integer>> group(List<Integer> xs) {
        return null;
    }

    public int[] values() {
        return null;
    }

    @Transactional(readOnly = true,
        timeout = 5)
    public Optional<Record> load(
            String id,
            boolean deep)
            throws IOException {
        return null;
    }

    public void allman()
    {
        run();
    }

    void listener() {
        button.addListener(new Listener() {
            @Override
            public void onClick(Event e) {
                save(e);
            }
        });
    }

    record Point(int x, int y) {
        Point {
            check(x);
        }
    }

    enum Color {
        RED {
            @Override String hex() { return "f00"; }
        },
        GREEN;
        String hex() { return ""; }
    }

    interface Visitor {
        default void visit(Node n) {
            n.accept(this);
        }
    }

    void caller() {
        String find = "x";
        find(String.class);
    }
}
`

const java2 = `
public class Two {
  public void a() {
    x();
  }

    public void b() {
        y();
    }
  public void c() {
	if (z) {
		w();
	}
  }
}
`

const javaWrapped = `
public class W {
    public ResponseEntity<Map<String, Object>> handle(@RequestBody Request request,
                                                      HttpServletRequest http) {
        return ok();
    }

    public void
    kr() {
        go();
    }
}
`

const ktSrc = `
class Svc(private val repo: Repo) {
    val cache by lazy {
        load()
    }

    fun String.shout(): String {
        return uppercase()
    }

    fun <T> List<T>.second(): T {
        return this[1]
    }

    suspend fun fetch(id: String): Item =
        repo.get(id)

    companion object {
        fun create() = Svc(Repo())
    }
}
`

const tsSrc = `
export default function main() {
  run()
}

const handler = async (req: Request) => {
  return req
}

export class Store {
  count = 0
  onChange = () => {
    this.count++
  }

  @Input()
  @HostListener("click", ["$event"])
  click(e: Event): void {
    this.onChange()
  }

  get size(): number {
    return this.count
  }

  static async *items(): AsyncGenerator<number> {
    yield 1
  }
}

export function parse(x: string): number
export function parse(x: number): number
export function parse(x: any): number {
  return 0
}

const api = {
  fetchAll() {
    return []
  },
  remove: function (id) {
    return id
  },
}

module.exports.legacy = function () {
  return 1
}

function useThing<T extends object>(v: T) {
  const parse = (s) => s
  return parse(v)
}
`

const goSrc = `
package x

type Repo[T any] struct {
	items []T
}

func (r *Repo[T]) Add(v T) {
	r.items = append(r.items, v)
}

func Map[T, U any](xs []T, f func(T) U) []U {
	return nil
}

func (Repo[T]) Len() int { return 0 }

var (
	Default = New()
	Other   = 2
)

func New() *Repo[int] {
	Map := 3
	return nil
}
`

const pySrc = `
import functools

@functools.lru_cache(maxsize=None)
def fib(n):
    return n

class Api:
    @property
    def name(self):
        return self._name

    @name.setter
    def name(self, v):
        self._name = v

    async def fetch(self, url):
        def fetch(x):
            return x
        return fetch(url)

handler = lambda e: e

if True:
    def conditional():
        pass

QUERY = """
def fib(n):
    pass
"""
`

const rsSrc = `
pub struct Parser {
    pos: usize,
}

impl Parser {
    pub(crate) fn new() -> Self {
        Parser { pos: 0 }
    }
}

impl Iterator for Parser {
    type Item = u8;
    fn next(&mut self) -> Option<u8> {
        None
    }
}

impl<T: Display> fmt::Debug for Wrapper<T> {
    fn fmt(&self, f: &mut Formatter) -> Result {
        Ok(())
    }
}

#[derive(Debug)]
pub enum Token {
    Num(i64),
    Plus,
}

macro_rules! check {
    ($e:expr) => { assert!($e) };
}
`

const cSrc = `
#include <stdio.h>

static int
parse_args(int argc, char **argv)
{
    return 0;
}

int Foo::bar(int x) const {
    return x;
}

struct node *
list_find(struct node *head, int key)
{
    return NULL;
}

#define MAX(a, b) ((a) > (b) ? (a) : (b))

int main(void) {
    parse_args(0, NULL);
    return 0;
}
`

const minSrc = `!function(){function a(){return 1}function b(){return a()}window.x={a:a,b:b}}();`

const falsePos = `
function setup() {
  // render() is called later
  const render = makeRenderer()
  render({
    id: 1,
  })
}

function render(x) {
  return x
}

const msg = "function render() {"
`

const falsePos2 = `
export class View {
  init() {
    this.render()
  }
}

render(view, {
  root: document.body,
})
`

// TestFindDeclarations pins what each language's rules find, including what the indentation heuristic got wrong: generic and array
// return types, constructors, wrapped signatures, mixed indentation, locals and calls with a declaration's name, and commented-out code.
func TestFindDeclarations(t *testing.T) {
	for _, c := range []struct{ file, src, symbol, want string }{
		{"Repo.java", javaSrc, "Repo", "3-75"},
		{"Repo.java", javaSrc, "Repo.Repo", "12-14"},
		{"Repo.java", javaSrc, "find", "16-18"},
		{"Repo.java", javaSrc, "group", "20-22"},
		{"Repo.java", javaSrc, "values", "24-26"},
		{"Repo.java", javaSrc, "load", "28-35"},
		{"Repo.java", javaSrc, "allman", "37-40"},
		{"Repo.java", javaSrc, "byLen", "8-10"},
		{"Repo.java", javaSrc, "handlers", "6-6"},
		{"Repo.java", javaSrc, "onClick", "44-47"},
		{"Repo.java", javaSrc, "Point", "51-55"},
		{"Repo.java", javaSrc, "Color", "57-63"},
		{"Repo.java", javaSrc, "Color.hex", "62-62"},
		{"Repo.java", javaSrc, "RED", "58-60"},
		{"Repo.java", javaSrc, "visit", "66-68"},
		{"Repo.java", javaSrc, "Visitor.visit", "66-68"},
		{"Repo.java", javaSrc, "Repo.find", "16-18"},
		{"Repo.java", javaSrc, "caller", "71-74"},
		{"Two.java", java2, "a", "2-4"},
		{"Two.java", java2, "b", "6-8"},
		{"Two.java", java2, "c", "9-13"},
		{"Two.java", java2, "Two.b", "6-8"},
		{"W.java", javaWrapped, "handle", "2-5"},
		{"W.java", javaWrapped, "kr", "7-10"},
		{"Svc.kt", ktSrc, "cache", "2-4"},
		{"Svc.kt", ktSrc, "shout", "6-8"},
		{"Svc.kt", ktSrc, "second", "10-12"},
		{"Svc.kt", ktSrc, "fetch", "14-15"},
		{"Svc.kt", ktSrc, "create", "18-18"},
		{"Svc.kt", ktSrc, "Svc", "1-20"},
		{"a.ts", tsSrc, "main", "1-3"},
		{"a.ts", tsSrc, "handler", "5-7"},
		{"a.ts", tsSrc, "onChange", "11-13"},
		{"a.ts", tsSrc, "Store.onChange", "11-13"},
		{"a.ts", tsSrc, "click", "15-19"},
		{"a.ts", tsSrc, "size", "21-23"},
		{"a.ts", tsSrc, "items", "25-27"},
		{"a.ts", tsSrc, "parse", "30-30 | 31-31 | 32-34"},
		{"a.ts", tsSrc, "fetchAll", "37-39"},
		{"a.ts", tsSrc, "api.fetchAll", "37-39"},
		{"a.ts", tsSrc, "remove", "40-42"},
		{"a.ts", tsSrc, "legacy", "45-47"},
		{"a.ts", tsSrc, "useThing", "49-52"},
		{"a.ts", tsSrc, "count", "10-10"},
		{"x.go", goSrc, "Repo", "3-5"},
		{"x.go", goSrc, "Add", "7-9"},
		{"x.go", goSrc, "Map", "11-13"},
		{"x.go", goSrc, "Len", "15-15"},
		{"x.go", goSrc, "Default", "18-18"},
		{"x.go", goSrc, "New", "22-25"},
		{"x.go", goSrc, "Repo.Add", "7-9"},
		{"a.py", pySrc, "fib", "3-5"},
		{"a.py", pySrc, "name", "8-10 | 12-14"},
		{"a.py", pySrc, "fetch", "16-19"},
		{"a.py", pySrc, "Api.fetch", "16-19"},
		{"a.py", pySrc, "handler", "21-21"},
		{"a.py", pySrc, "conditional", "24-25"},
		{"a.rs", rsSrc, "Parser", "1-3 | 5-9 | 11-16"},
		{"a.rs", rsSrc, "new", "6-8"},
		{"a.rs", rsSrc, "Parser.new", "6-8"},
		{"a.rs", rsSrc, "next", "13-15"},
		{"a.rs", rsSrc, "fmt", "19-21"},
		{"a.rs", rsSrc, "Token", "24-28"},
		{"a.rs", rsSrc, "check", "30-32"},
		{"a.rs", rsSrc, "Item", "12-12"},
		{"a.c", cSrc, "parse_args", "3-7"},
		{"a.cpp", cSrc, "bar", "9-11"},
		{"a.cpp", cSrc, "Foo.bar", "9-11"},
		{"a.c", cSrc, "list_find", "13-17"},
		{"a.c", cSrc, "MAX", "19-19"},
		{"a.c", cSrc, "main", "21-24"},
		{"m.js", minSrc, "a", "1-1"},
		{"m.js", minSrc, "b", "1-1"},
		{"f.js", falsePos, "render", "9-11"},
		{"f.js", falsePos2, "render", "ERR nib found no declaration of render"},
		{"f.js", falsePos2, "View.init", "2-4"},
	} {
		found, err := Find(c.file, []byte(strings.TrimPrefix(c.src, "\n")), c.symbol)
		var got []string
		if err != nil {
			got = append(got, "ERR "+err.Error())
		}
		for _, r := range found {
			got = append(got, fmt.Sprintf("%d-%d", r.Start, r.End))
		}
		if strings.Join(got, " | ") != c.want {
			t.Errorf("%s:%s: got %s, want %s", c.file, c.symbol, strings.Join(got, " | "), c.want)
		}
	}
}

func TestFindInZigAndWhatItRefuses(t *testing.T) {
	zig := "/// A repository.\npub const Repo = struct {\n    store: Store,\n\n    pub fn add(self: *Repo, v: u32) !void {\n        const local = v;\n        _ = local;\n    }\n};\n\nfn helper() void {}\n"
	for symbol, want := range map[string]string{"Repo": "1-9", "Repo.add": "5-8", "store": "3-3", "helper": "11-11"} {
		found, err := Find("a.zig", []byte(zig), symbol)
		if err != nil || len(found) != 1 || fmt.Sprintf("%d-%d", found[0].Start, found[0].End) != want {
			t.Errorf("%s: got %v %v, want %s", symbol, found, err, want)
		}
	}
	if _, err := Find("a.zig", []byte(zig), "local"); err == nil {
		t.Error("a local is not a declaration")
	}
	if _, err := Find("notes.md", []byte("# Title\n"), "Title"); err != ErrNoGrammar {
		t.Errorf("a file without a grammar is refused, got %v", err)
	}
	// A declaration whose lines did not parse is refused with its line; a name the file lacks, past a parse error, may still be there.
	broken := "class A {\n  void ok() {}\n  void bad( {\n  }\n}\n"
	if _, err := Find("A.java", []byte(broken), "bad"); err == nil || !strings.Contains(err.Error(), "did not parse cleanly") {
		t.Errorf("got %v", err)
	}
	if _, err := Find("A.java", []byte(broken), "missing"); err == nil || !strings.Contains(err.Error(), "so it may be there") {
		t.Errorf("got %v", err)
	}
}

// TestBlockEndsWhereTheTreeSays covers the cases the indentation rule got wrong (a deeper sibling in a mixed-indent file, a tab-indented
// body) and the ones it got right.
func TestBlockEndsWhereTheTreeSays(t *testing.T) {
	for _, c := range []struct {
		file, src string
		line      int
		want      string
	}{
		{"Two.java", java2, 2, "4"},
		{"Two.java", java2, 9, "13"},
		{"Two.java", java2, 10, "12"},
		// A doc comment or annotation line takes the declaration below it; a signature line under its annotations ends with its body.
		{"Repo.java", javaSrc, 3, "75"},
		{"Repo.java", javaSrc, 28, "35"},
		{"Repo.java", javaSrc, 30, "35"},
		{"a.py", pySrc, 3, "5"},
		{"a.py", pySrc, 16, "19"},
		{"Two.java", java2, 3, "ERR " + ErrNotABlock.Error()},
		{"x.md", "# a\n", 1, "ERR " + ErrNoGrammar.Error()},
	} {
		end, err := Block(c.file, []byte(strings.TrimPrefix(c.src, "\n")), c.line)
		got := fmt.Sprint(end)
		if err != nil {
			got = "ERR " + err.Error()
		}
		if got != c.want {
			t.Errorf("%s:%d: got %s, want %s", c.file, c.line, got, c.want)
		}
	}
}

func TestAtMostTwoGrammarsStayDecoded(t *testing.T) {
	for _, file := range []string{"a.go", "a.py", "a.zig", "A.java"} {
		if _, err := Find(file, []byte("x\n"), "x"); err == ErrNoGrammar {
			t.Fatal(err)
		}
	}
	if loaded, limit := grammarruntime.EmbeddedLanguageCacheStats(); loaded > 2 || limit != 2 {
		t.Fatalf("%d grammars decoded, limit %d", loaded, limit)
	}
}
