package syntax

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
)

const javaImports = `package be.example.orm;

import be.example.common.time.Backoff;
import be.example.orm.datasource.Row;
import be.example.orm.datasource.Datasource;
import be.example.orm.query.criteria.*;
import com.couchbase.client.java.Bucket;
import static org.junit.Assert.assertEquals;
import module java.base;

import java.util.List;
import java.util.Map;

public class Store {
}
`

func TestImportsReadsEachFormOnItsLine(t *testing.T) {
	found, ok := Imports("Store.java", strings.Split(javaImports, "\n"))
	if !ok || len(found) != 9 {
		t.Fatalf("found %d: %+v", len(found), found)
	}
	want := []Import{
		{Line: 6, Path: "be.example.orm.query.criteria", Wildcard: true},
		{Line: 8, Path: "org.junit.Assert.assertEquals", Static: true},
		{Line: 9, Path: "java.base", Module: true},
	}
	for _, w := range want {
		if !slices.Contains(found, w) {
			t.Errorf("no %+v in %+v", w, found)
		}
	}
	if found[0].Name() != "Backoff" || found[3].Name() != "" {
		t.Errorf("names %q %q", found[0].Name(), found[3].Name())
	}
	if _, ok := Imports("main.go", []string{`import "fmt"`}); ok {
		t.Error("Go imports are not read")
	}
	kotlin, _ := Imports("A.kt", []string{"import a.b.C as D", "import a.b.*", "import static a.b.C"})
	if len(kotlin) != 2 || kotlin[0].Alias != "D" || kotlin[0].Name() != "D" || !kotlin[1].Wildcard {
		t.Errorf("kotlin %+v", kotlin)
	}
}

func TestImportGroupsShowEachPackageOnce(t *testing.T) {
	found, _ := Imports("Store.java", strings.Split(javaImports, "\n"))
	want := []string{
		"be.example.common.time: Backoff",
		"be.example.orm.datasource: Datasource, Row",
		"be.example.orm.query.criteria: *",
		"com.couchbase.client.java: Bucket",
		"java.util: List, Map",
		"static org.junit.Assert: assertEquals",
		"module java.base",
	}
	if got := ImportGroups(found); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got)
	}
}

func TestPlaceImportBesideItsNearestPackageInOrder(t *testing.T) {
	lines := strings.Split(javaImports, "\n")
	cases := []struct {
		spec  string
		after int
		line  string
	}{
		// Between Datasource and Row would be alphabetical, but the file keeps Row first: it goes after the last one before it.
		{"be.example.orm.datasource.Field", 5, "import be.example.orm.datasource.Field;"},
		{"import java.util.ArrayList;", 10, "import java.util.ArrayList;"},
		{"java.util.Set", 12, "import java.util.Set;"},
		{"static org.junit.Assert.assertTrue", 8, "import static org.junit.Assert.assertTrue;"},
		{"org.example.Thing", 12, "import org.example.Thing;"},
	}
	for _, c := range cases {
		place, err := PlaceImport("Store.java", lines, c.spec)
		if err != nil || place.Existing != 0 || place.After != c.after || !slices.Equal(place.Lines, []string{c.line}) {
			t.Errorf("%s: %+v %v", c.spec, place, err)
		}
	}
	if place, err := PlaceImport("Store.java", lines, "java.util.Map;"); err != nil || place.Existing != 12 {
		t.Errorf("an import the file has: %+v %v", place, err)
	}
	if _, err := PlaceImport("Store.java", lines, "other.pkg.Row"); err == nil || !strings.Contains(err.Error(), "line 4 imports be.example.orm.datasource.Row as Row already") {
		t.Errorf("a clash: %v", err)
	}
	for _, bad := range []string{"Row", "a b", "static a.b.C"} {
		if _, err := PlaceImport("A.kt", []string{"package a"}, bad); err == nil {
			t.Errorf("%q was taken", bad)
		}
	}
	if _, err := PlaceImport("main.go", nil, "fmt"); !errors.Is(err, ErrNoImports) {
		t.Errorf("go: %v", err)
	}
}

func TestPlaceImportInAFileWithoutImports(t *testing.T) {
	place, err := PlaceImport("A.kt", []string{"package a.b", "", "class A"}, "x.y.Z as W")
	if err != nil || place.After != 1 || !slices.Equal(place.Lines, []string{"", "import x.y.Z as W"}) {
		t.Errorf("after the package line: %+v %v", place, err)
	}
	place, err = PlaceImport("A.java", []string{"class A {}"}, "x.y.Z")
	if err != nil || place.After != 0 || !slices.Equal(place.Lines, []string{"import x.y.Z;", ""}) {
		t.Errorf("at the top: %+v %v", place, err)
	}
}

func TestInspectTellsTheClassNamesEachLineUses(t *testing.T) {
	src := `package a.b;

import java.util.List;

@Nullable
public class Foo<T extends Bar> extends Base implements Map.Entry<String, java.util.List<Baz>> {
    class Inner {}
    @Override
    public <R> R run(Qux q) throws Oops {
        var x = new Widget<>();
        Collections.emptyList();
        Duration d = Duration.ofSeconds(1);
        Runnable r = Ref::go;
        LOG.info("x");
        return null;
    }
}
`
	found, err := Inspect("Foo.java", []byte(src))
	if err != nil || found.ErrorLine != 0 || !found.Types || !found.TopLevel {
		t.Fatalf("%+v %v", found, err)
	}
	want := map[int][]string{5: {"Nullable"}, 6: {"Bar", "Base", "Map", "String", "Baz"}, 8: {"Override"}, 9: {"Qux", "Oops"}, 10: {"Widget"},
		11: {"Collections"}, 12: {"Duration", "Duration"}, 13: {"Runnable", "Ref"}}
	if !reflect.DeepEqual(found.Uses, want) {
		t.Errorf("uses %v", found.Uses)
	}
	for _, name := range []string{"Foo", "Inner", "T", "R"} {
		if !found.Declared[name] {
			t.Errorf("%s is not declared: %v", name, found.Declared)
		}
	}
	if !found.Implicit["String"] || found.Implicit["List"] {
		t.Error("java.lang is implicit, java.util is not")
	}
	broken, err := Inspect("Foo.java", []byte("class Foo {\n  void f( {\n}\n"))
	if err != nil || broken.ErrorLine == 0 {
		t.Errorf("a parse error: %+v %v", broken, err)
	}
	kotlin, err := Inspect("A.kt", []byte("class A\n"))
	if err != nil || kotlin.Types {
		t.Errorf("kotlin names are not told apart: %+v %v", kotlin, err)
	}
}
