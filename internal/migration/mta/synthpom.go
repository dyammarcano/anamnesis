package mta

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/dyammarcano/anamnesis/internal/engine"
)

// Synthetic-POM route (hypothesis H-1, docs/kantra-internals.md §8).
//
// kantra 8.3.0's Java provider only starts when the input root holds a pom.xml or build.gradle; an
// Ant/Eclipse/hand-built project always fails with "unable to get build tool". This route reshapes a
// STAGED COPY of such a project into one the provider accepts: every .java file is moved to
// src/main/java/<package path>, the project's jars become system-scoped dependencies, and a minimal
// pom.xml is written at the root. Descriptors and other files keep their relative paths, so builtin
// rules still see them where the repository has them. The analyzed repository is never touched.

// SyntheticSourceLevel is the Java level declared in the synthetic POM. Recent JDT (used by MTA's JDT
// LS) no longer accepts compliance levels below 1.8; Java 5-7 source is valid Java 8 source.
const SyntheticSourceLevel = "1.8"

const relocateStage = ".anamnesis-relocate"

var packageRe = regexp.MustCompile(`^\s*package\s+([A-Za-z_][\w.]*)\s*;`)

// synthInfo summarises what the reshaping did; it goes into mta.run evidence.
type synthInfo struct {
	JavaFiles  int // .java files found
	Moved      int // moved under src/main/java
	InPlace    int // already at their package location under src/main/java
	Collisions int // same package+class seen twice; later ones left where they were (not compiled)
	Jars       int // jars declared as system dependencies
	Roots      []string
}

// synthesizePOM reshapes staged (a fresh copy of p.Root) and returns stagedRel -> repoRel for every
// moved file, so incident URIs map back to repository paths.
func synthesizePOM(ctx context.Context, p *engine.Project, staged string) (map[string]string, synthInfo, error) {
	var info synthInfo
	type move struct{ from, to string } // staged-relative, slash form
	var moves []move
	taken := map[string]string{}
	roots := map[string]bool{}

	for _, f := range p.Files.WithExt(".java") {
		if ctx.Err() != nil {
			return nil, info, ctx.Err()
		}
		if strings.HasPrefix(f.Rel, "target/") || strings.Contains(f.Rel, "/target/") {
			continue // Maven output trees are not source
		}
		info.JavaFiles++
		pkg := readPackage(filepath.Join(staged, filepath.FromSlash(f.Rel)))
		dest := path.Join("src/main/java", strings.ReplaceAll(pkg, ".", "/"), path.Base(f.Rel))
		if _, dup := taken[dest]; dup {
			info.Collisions++
			continue
		}
		taken[dest] = f.Rel
		roots[sourceRoot(f.Rel, pkg)] = true
		if dest == f.Rel {
			info.InPlace++
			continue
		}
		moves = append(moves, move{f.Rel, dest})
	}
	if info.JavaFiles == 0 {
		return nil, info, fmt.Errorf("no .java files to analyse")
	}

	// Two phases, so a destination that is another file's original location is never overwritten.
	hold := filepath.Join(staged, relocateStage)
	for i, mv := range moves {
		tmp := filepath.Join(hold, fmt.Sprintf("%06d.java", i))
		if err := os.MkdirAll(hold, 0o755); err != nil {
			return nil, info, err
		}
		if err := os.Rename(filepath.Join(staged, filepath.FromSlash(mv.from)), tmp); err != nil {
			return nil, info, fmt.Errorf("stage %s: %w", mv.from, err)
		}
	}
	relocated := make(map[string]string, len(moves))
	for i, mv := range moves {
		tmp := filepath.Join(hold, fmt.Sprintf("%06d.java", i))
		dst := filepath.Join(staged, filepath.FromSlash(mv.to))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return nil, info, err
		}
		if err := os.Rename(tmp, dst); err != nil {
			return nil, info, fmt.Errorf("relocate %s: %w", mv.from, err)
		}
		relocated[mv.to] = mv.from
		info.Moved++
	}
	_ = os.RemoveAll(hold)

	// Eclipse metadata at the root could make JDT LS import the copy as an Eclipse project instead of
	// a Maven one; set it aside in the copy only.
	for _, n := range []string{".project", ".classpath"} {
		src := filepath.Join(staged, n)
		if _, err := os.Stat(src); err == nil {
			_ = os.Rename(src, src+".anamnesis-orig")
		}
	}

	var jars []string
	for _, f := range p.Files.WithExt(".jar") {
		jars = append(jars, f.Rel)
	}
	sort.Strings(jars)
	info.Jars = len(jars)
	for r := range roots {
		info.Roots = append(info.Roots, r)
	}
	sort.Strings(info.Roots)

	if err := os.WriteFile(filepath.Join(staged, "pom.xml"), []byte(syntheticPOM(p.Name, jars)), 0o644); err != nil {
		return nil, info, err
	}
	return relocated, info, nil
}

// readPackage returns the file's package ("" for the default package), reading only the header.
func readPackage(file string) string {
	f, err := os.Open(file)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for n := 0; sc.Scan() && n < 400; n++ {
		line := strings.TrimPrefix(sc.Text(), "\ufeff")
		if m := packageRe.FindStringSubmatch(line); m != nil {
			return m[1]
		}
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "import ") || strings.HasPrefix(t, "public ") || strings.HasPrefix(t, "class ") {
			return "" // past the header: default package
		}
	}
	return ""
}

// sourceRoot is the directory a file's package path hangs from (the file's dir when it does not end
// with the package path, i.e. a misplaced file).
func sourceRoot(rel, pkg string) string {
	dir := path.Dir(rel)
	pp := strings.ReplaceAll(pkg, ".", "/")
	if pp == "" {
		return dir
	}
	if dir == pp {
		return "."
	}
	if strings.HasSuffix(dir, "/"+pp) {
		return strings.TrimSuffix(dir, "/"+pp)
	}
	return dir
}

var artifactSafe = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

func syntheticPOM(name string, jars []string) string {
	artifact := strings.Trim(artifactSafe.ReplaceAllString(strings.ToLower(name), "-"), "-.")
	if artifact == "" {
		artifact = "project"
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!-- Generated by Anamnesis for MTA analysis of a staged copy. Not part of the analyzed project. -->
<project xmlns="http://maven.apache.org/POM/4.0.0" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"
         xsi:schemaLocation="http://maven.apache.org/POM/4.0.0 http://maven.apache.org/xsd/maven-4.0.0.xsd">
  <modelVersion>4.0.0</modelVersion>
  <groupId>anamnesis.synthetic</groupId>
  <artifactId>`)
	b.WriteString(xmlEscape(artifact))
	b.WriteString(`</artifactId>
  <version>0</version>
  <packaging>jar</packaging>
  <properties>
    <maven.compiler.source>` + SyntheticSourceLevel + `</maven.compiler.source>
    <maven.compiler.target>` + SyntheticSourceLevel + `</maven.compiler.target>
    <project.build.sourceEncoding>UTF-8</project.build.sourceEncoding>
  </properties>
  <dependencies>
`)
	for i, j := range jars {
		fmt.Fprintf(&b, `    <dependency>
      <groupId>anamnesis.local</groupId>
      <artifactId>lib-%04d-%s</artifactId>
      <version>0</version>
      <scope>system</scope>
      <systemPath>${project.basedir}/%s</systemPath>
    </dependency>
`, i, xmlEscape(strings.Trim(artifactSafe.ReplaceAllString(strings.TrimSuffix(path.Base(j), ".jar"), "-"), "-.")), xmlEscape(j))
	}
	b.WriteString("  </dependencies>\n</project>\n")
	return b.String()
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;").Replace(s)
}

// limitStrings returns at most n items.
func limitStrings(items []string, n int) []string {
	if len(items) <= n {
		return items
	}
	return items[:n]
}
