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

// javaWorkspace is a workspace of three modules: app holds Service and Helper, common the classes Clock and Ticker, and other a second
// Ticker, so a name has one class, two, or one in the same package.
func javaWorkspace(t *testing.T) (*Engine, *Context, string) {
	t.Helper()
	directory, _ := filepath.EvalSymlinks(t.TempDir())
	files := map[string]string{
		"app/src/main/java/be/example/app/Service.java":           serviceJava,
		"app/src/main/java/be/example/app/Helper.java":            "package be.example.app;\n\nclass Helper {}\n",
		"common/src/main/java/be/example/common/Clock.java":       "package be.example.common;\n\npublic class Clock {}\n",
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
		"[lines 3-13: 10 imports, by package; read " + servicePath + ":3-13 for their lines]",
		"[be.example.common.time: Backoff, Pace]",
		"[be.example.orm: Datasource, Row]",
		"[be.example.orm.query: Query]",
		"[be.example.orm.query.criteria: *]",
		"[com.couchbase.client.java: Bucket, Cluster]",
		"[java.util: List, Map]",
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
	if err != nil || !strings.Contains(imports.Text, "\n5:import be.example.orm.Row;\n") || strings.Contains(imports.Text, "by package") {
		t.Fatalf("the imports alone: %v\n%s", err, imports.Text)
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
	out, err := e.Patch(ctx, "["+servicePath+"#"+tag+"]\nPUT 23:\n+        Clock clock = new Clock();\n+        Ticker ticker = null;\n+        Helper helper = new Helper();\n+        Eq eq = null;\n+        String s = \"\";\n+        return List.of();")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Text, "Clock, used in the new rows, is not imported, and nib finds each in one place in the workspace; if that is the one meant, this adds it:\n["+servicePath+"]\nIMPORT be.example.common.Clock") {
		t.Fatalf("Clock:\n%s", out.Text)
	}
	if !strings.Contains(out.Text, "Ticker is not imported either, and nib finds be.example.common.Ticker and be.example.other.Ticker; IMPORT the one meant.") {
		t.Fatalf("Ticker:\n%s", out.Text)
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
	out, err := e.Patch(ctx, "["+servicePath+"#"+tag+"]\nPUT 22:\n+    public List<Row> rows( {")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Text, "The file no longer parses as Java: before this edit it did, and now its first error is at line ") {
		t.Fatalf("response:\n%s", out.Text)
	}
}

func TestALibraryWildcardKeepsTheImportCheckQuiet(t *testing.T) {
	e, ctx, file := javaWorkspace(t)
	if err := os.WriteFile(file, []byte(strings.Replace(serviceJava, "import java.util.List;", "import java.util.List;\nimport org.library.*;", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	read, err := e.Read(ctx, ReadArgs{Paths: []string{servicePath + ":20-26"}})
	if err != nil {
		t.Fatal(err)
	}
	tag := read.Text[strings.Index(read.Text, "#")+1 : strings.Index(read.Text, "]")]
	out, err := e.Patch(ctx, "["+servicePath+"#"+tag+"]\nPUT 24:\n+        Clock clock = new Clock();\n+        return List.of();")
	if err != nil || strings.Contains(out.Text, "Warnings:") {
		t.Fatalf("org.library.* may hold a Clock: %v\n%s", err, out.Text)
	}
}
