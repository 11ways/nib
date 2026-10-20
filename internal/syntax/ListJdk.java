// Writes jdk.txt, the public classes of the JDK modules named as arguments, one exported package a line with its classes:
// java ListJdk.java java.base java.sql java.net.http java.logging > jdk.txt
import java.lang.module.ModuleDescriptor;
import java.lang.reflect.Modifier;
import java.net.URI;
import java.nio.file.*;
import java.util.*;

public class ListJdk {
    public static void main(String[] args) throws Exception {
        FileSystem fs = FileSystems.getFileSystem(URI.create("jrt:/"));
        TreeMap<String, TreeSet<String>> packages = new TreeMap<>();
        for (String module : args) {
            ModuleDescriptor descriptor = ModuleLayer.boot().findModule(module).orElseThrow().getDescriptor();
            for (ModuleDescriptor.Exports export : descriptor.exports()) {
                if (export.isQualified()) continue;
                String pkg = export.source();
                Path dir = fs.getPath("/modules", module, pkg.replace('.', '/'));
                try (var stream = Files.list(dir)) {
                    for (Path p : (Iterable<Path>) stream::iterator) {
                        String f = p.getFileName().toString();
                        if (!f.endsWith(".class") || f.contains("$") || f.startsWith("module-info") || f.startsWith("package-info")) continue;
                        String n = f.substring(0, f.length() - 6);
                        Class<?> c = Class.forName(pkg + "." + n, false, ClassLoader.getPlatformClassLoader());
                        if (Modifier.isPublic(c.getModifiers())) packages.computeIfAbsent(pkg, k -> new TreeSet<>()).add(n);
                    }
                }
            }
        }
        for (var e : packages.entrySet()) System.out.println(e.getKey() + " " + String.join(" ", e.getValue()));
    }
}
