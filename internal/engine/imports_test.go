package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const serviceJava = `package be.example.app;

import be.example.common.time.Backoff;
import be.example.common.time.Pace;
import be.example.orm.Row;
import be.example.orm.Datasource;
import be.example.orm.query.Query;
import be.example.orm.query.criteria.*;
import com.couchbase.client.java.Bucket;
import com.couchbase.client.java.Cluster;

import java.util.List;
import java.util.Map;

public class Service {
    private final Datasource datasource;

    public Service(Datasource datasource) {
        this.datasource = datasource;
    }

    public List<Row> rows() {
        return List.of();
    }
}
`

// javaWorkspace is a workspace of four modules: app holds Service and Helper, common the classes Clock, Gauge and Ticker, other a second
// Ticker and orm a criteria package, so a name has one class, two, one in the same package, or one the JDK has too.
func javaWorkspace(t *testing.T) (*Engine, *Context, string) {
	t.Helper()
	directory, _ := filepath.EvalSymlinks(t.TempDir())
	files := map[string]string{
		"app/src/main/java/be/example/app/Service.java":           serviceJava,
		"app/src/main/java/be/example/app/Helper.java":            "package be.example.app;\n\nclass Helper {}\n",
		"common/src/main/java/be/example/common/Clock.java":       "package be.example.common;\n\npublic class Clock {}\n",
		"common/src/main/java/be/example/common/Gauge.java":       "package be.example.common;\n\npublic class Gauge {}\n",
		"common/src/main/java/be/example/common/Ticker.java":      "package be.example.common;\n\npublic class Ticker {}\n",
		"other/src/main/java/be/example/other/Ticker.java":        "package be.example.other;\n\npublic class Ticker {}\n",
		"orm/src/main/java/be/example/orm/query/criteria/Eq.java": "package be.example.orm.query.criteria;\n\npublic class Eq {}\n",
	}
	for name, content := range files {
		path := filepath.Join(directory, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return New(), &Context{Ctx: context.Background(), Directory: directory, Session: "s"}, filepath.Join(directory, filepath.FromSlash("app/src/main/java/be/example/app/Service.java"))
}

const servicePath = "app/src/main/java/be/example/app/Service.java"

func TestAReadPastTheImportsShowsThemByPackage(t *testing.T) {
	e, ctx, _ := javaWorkspace(t)
	read, err := e.Read(ctx, ReadArgs{Paths: []string{servicePath + ":1-20"}})
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"1:package be.example.app;",
		"2:",
		"[lines 3-13: 10 imports as a package tree; read that range for their lines]",
		"be.example:",
		"  common.time: Backoff Pace",
		"  orm: Datasource Row",
		"    query: Query",
		"      criteria: *",
		"com.couchbase.client.java: Bucket Cluster",
		"java.util: List Map",
		"14:",
		"15:public class Service {",
	}, "\n")
	if !strings.Contains(read.Text, "]\n"+want+"\n") {
		t.Fatalf("read:\n%s", read.Text)
	}
	// The folded lines were not seen, so an edit of them needs a read; a range of just the imports shows them.
	tag := read.Text[strings.Index(read.Text, "#")+1 : strings.Index(read.Text, "]")]
	if _, err := e.Patch(ctx, "["+servicePath+"#"+tag+"]\nPUT 5:\n+import be.example.orm.Column;"); err == nil || !strings.Contains(err.Error(), "never displayed") {
		t.Fatalf("an edit of a folded line: %v", err)
	}
	imports, err := e.Read(ctx, ReadArgs{Paths: []string{servicePath + ":3-13"}})
	if err != nil || !strings.Contains(imports.Text, "\n5:import be.example.orm.Row;\n") || strings.Contains(imports.Text, "package tree") {
		t.Fatalf("the imports alone: %v\n%s", err, imports.Text)
	}
}

func TestImportsTheAgentSawPartOfStillFold(t *testing.T) {
	e, ctx, _ := javaWorkspace(t)
	ctx.Markers = true
	if _, err := e.Read(ctx, ReadArgs{Paths: []string{servicePath + ":10-16"}}); err != nil {
		t.Fatal(err)
	}
	read, err := e.Read(ctx, ReadArgs{Paths: []string{servicePath + ":1-20"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read.Text, "\n2:\n[lines 3-13: 10 imports as a package tree; read that range for their lines]\n") || strings.Contains(read.Text, "4:import") {
		t.Fatalf("read:\n%s", read.Text)
	}
	// Once the agent holds every import, the rows for what it holds are shorter than the fold.
	if _, err := e.Read(ctx, ReadArgs{Paths: []string{servicePath + ":3-13"}}); err != nil {
		t.Fatal(err)
	}
	again, err := e.Read(ctx, ReadArgs{Paths: []string{servicePath + ":1-20"}})
	if err != nil || strings.Contains(again.Text, "package tree") || !strings.Contains(again.Text, "\n3~import be.example.common.time.") {
		t.Fatalf("held: %v\n%s", err, again.Text)
	}
}

func TestImportPlacesTheLineWithoutATagOrARead(t *testing.T) {
	e, ctx, file := javaWorkspace(t)
	out, err := e.Patch(ctx, "["+servicePath+"]\nIMPORT be.example.orm.Column, java.util.Set, java.util.List")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(file)
	if !strings.Contains(string(data), "import be.example.common.time.Pace;\nimport be.example.orm.Column;\nimport be.example.orm.Row;\n") ||
		!strings.Contains(string(data), "import java.util.Map;\nimport java.util.Set;\n") {
		t.Fatalf("file:\n%s", data)
	}
	if !strings.Contains(out.Text, "[IMPORT java.util.List: line 12 imports it already]") || !strings.Contains(out.Text, "\n5:import be.example.orm.Column;\n") {
		t.Fatalf("response:\n%s", out.Text)
	}
	again, err := e.Patch(ctx, "["+servicePath+"]\nIMPORT java.util.Set")
	if err != nil || !strings.HasSuffix(again.Text, "\n[IMPORT java.util.Set: line 15 imports it already]") {
		t.Fatalf("again: %v\n%s", err, again.Text)
	}
	if _, err := e.Patch(ctx, "["+servicePath+"]\nIMPORT other.Row"); err == nil || !strings.Contains(err.Error(), "clashes with other.Row") {
		t.Fatalf("a clash: %v", err)
	}
	kotlin := filepath.Join(ctx.Directory, "A.kt")
	if err := os.WriteFile(kotlin, []byte("package a\n\nclass A\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Patch(ctx, "[A.kt]\nIMPORT x.y.Z as W"); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(kotlin); string(data) != "package a\n\nimport x.y.Z as W\n\nclass A\n" {
		t.Fatalf("kotlin:\n%s", data)
	}
	if err := os.WriteFile(filepath.Join(ctx.Directory, "a.ts"), []byte("let a = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Patch(ctx, "[a.ts]\nIMPORT x"); err == nil || !strings.Contains(err.Error(), "nib reads imports only in Java and Kotlin files") {
		t.Fatalf("typescript: %v", err)
	}
}

func TestAnEditThatUsesAClassNothingImportsSaysWhereItIs(t *testing.T) {
	e, ctx, _ := javaWorkspace(t)
	read, err := e.Read(ctx, ReadArgs{Paths: []string{servicePath + ":20-25"}})
	if err != nil {
		t.Fatal(err)
	}
	tag := read.Text[strings.Index(read.Text, "#")+1 : strings.Index(read.Text, "]")]
	out, err := e.Patch(ctx, "["+servicePath+"#"+tag+"]\nPUT 23:\n+        Gauge gauge = new Gauge();\n+        Duration d = Duration.ofSeconds(1);\n+        Clock clock = null;\n+        Ticker ticker = null;\n+        Helper helper = new Helper();\n+        Eq eq = null;\n+        String s = \"\";\n+        return List.of();")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Text, "Gauge, Duration, used in the new rows, are not imported, and nib knows one class of each name; if they are the ones meant, this adds them:\n["+servicePath+"]\nIMPORT be.example.common.Gauge, java.time.Duration") {
		t.Fatalf("Gauge:\n%s", out.Text)
	}
	for _, both := range []string{"Clock is not imported, and nib finds be.example.common.Clock and java.time.Clock; IMPORT the one meant.",
		"Ticker is not imported, and nib finds be.example.common.Ticker and be.example.other.Ticker; IMPORT the one meant."} {
		if !strings.Contains(out.Text, both) {
			t.Fatalf("no %q in:\n%s", both, out.Text)
		}
	}
	for _, quiet := range []string{"Helper", "Eq", "String", "List"} {
		if strings.Contains(out.Text, quiet+",") || strings.Contains(out.Text, quiet+" is") {
			t.Errorf("%s needs no import:\n%s", quiet, out.Text)
		}
	}
}

func TestAnEditThatBreaksTheParseSaysWhere(t *testing.T) {
	e, ctx, _ := javaWorkspace(t)
	read, err := e.Read(ctx, ReadArgs{Paths: []string{servicePath + ":20-25"}})
	if err != nil {
		t.Fatal(err)
	}
	tag := read.Text[strings.Index(read.Text, "#")+1 : strings.Index(read.Text, "]")]
	out, err := e.Patch(ctx, "["+servicePath+"#"+tag+"]\nPUT 23:\n+        return = List.of();")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Text, "The file no longer parses as Java: before this edit it did, and now its first error is at line 23;") {
		t.Fatalf("response:\n%s", out.Text)
	}
	// Unbalanced brackets say it already, so the parse adds nothing.
	tag = out.Text[strings.Index(out.Text, "#")+1 : strings.Index(out.Text, "]")]
	fixed, err := e.Patch(ctx, "["+servicePath+"#"+tag+"]\nPUT 23:\n+        return List.of();")
	if err != nil || strings.Contains(fixed.Text, "Warnings:") {
		t.Fatalf("fixed: %v\n%s", err, fixed.Text)
	}
	tag = fixed.Text[strings.Index(fixed.Text, "#")+1 : strings.Index(fixed.Text, "]")]
	broken, err := e.Patch(ctx, "["+servicePath+"#"+tag+"]\nPUT 22:\n+    public List<Row> rows( {")
	if err != nil || !strings.Contains(broken.Text, "Brackets no longer balance") || strings.Contains(broken.Text, "no longer parses") {
		t.Fatalf("brackets: %v\n%s", err, broken.Text)
	}
}

func TestALibraryWildcardKeepsTheImportCheckQuietAndAJDKOneDoesNot(t *testing.T) {
	for wildcard, warned := range map[string]bool{"org.library.*": false, "java.util.concurrent.*": true} {
		e, ctx, file := javaWorkspace(t)
		if err := os.WriteFile(file, []byte(strings.Replace(serviceJava, "import java.util.List;", "import java.util.List;\nimport "+wildcard+";", 1)), 0o644); err != nil {
			t.Fatal(err)
		}
		read, err := e.Read(ctx, ReadArgs{Paths: []string{servicePath + ":20-26"}})
		if err != nil {
			t.Fatal(err)
		}
		tag := read.Text[strings.Index(read.Text, "#")+1 : strings.Index(read.Text, "]")]
		out, err := e.Patch(ctx, "["+servicePath+"#"+tag+"]\nPUT 24:\n+        Gauge gauge = new Gauge();\n+        return List.of();")
		if err != nil || strings.Contains(out.Text, "IMPORT be.example.common.Gauge") != warned {
			t.Fatalf("%s: %v\n%s", wildcard, err, out.Text)
		}
	}
}
