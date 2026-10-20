package syntax

import (
	_ "embed"
	"strings"
	"sync"
)

// jdkText is the public classes of the JDK modules java.base, java.sql, java.net.http and java.logging in JDK 25, one exported package a
// line followed by its classes; ListJdk.java writes it.
//
//go:embed jdk.txt
var jdkText string

// jdkTable is jdkText read: the packages that hold each class name, and the classes of each package.
type jdkTable struct {
	packages map[string][]string
	classes  map[string]map[string]bool
}

var jdk = sync.OnceValue(func() jdkTable {
	table := jdkTable{packages: map[string][]string{}, classes: map[string]map[string]bool{}}
	for _, line := range strings.Split(strings.TrimSpace(jdkText), "\n") {
		fields := strings.Fields(line)
		table.classes[fields[0]] = set(fields[1:]...)
		for _, name := range fields[1:] {
			table.packages[name] = append(table.packages[name], fields[0])
		}
	}
	return table
})

// JDKClasses are the full names of the JDK's public classes named name, in the modules jdk.txt holds.
func JDKClasses(name string) []string {
	var found []string
	for _, pkg := range jdk().packages[name] {
		found = append(found, pkg+"."+name)
	}
	return found
}

// JDKPackage says whether pkg is a package of the JDK modules jdk.txt holds, whose classes it lists whole.
func JDKPackage(pkg string) bool { return jdk().classes[pkg] != nil }

// jdkPackageClasses are the names of the public classes of pkg, a JDK package; none for "".
func jdkPackageClasses(pkg string) map[string]bool { return jdk().classes[pkg] }
