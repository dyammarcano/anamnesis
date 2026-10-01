package mta

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dyammarcano/anamnesis/internal/buildrun"
	"github.com/dyammarcano/anamnesis/internal/engine"
)

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// An Ant-style tree with two source roots, a jar, Eclipse metadata and a duplicate class is reshaped
// in the STAGED copy only; every moved file maps back to its repository path.
func TestSynthesizePOMReshapesStagedCopyOnly(t *testing.T) {
	repo := t.TempDir()
	write(t, repo, "build.xml", `<project default="b"><target name="b"/></project>`)
	write(t, repo, ".project", `<projectDescription/>`)
	write(t, repo, "src/java/org/acme/core/A.java", "\ufeff// header\npackage org.acme.core;\npublic class A {}\n")
	write(t, repo, "modules/web/src/org/acme/web/B.java", "/* c */\npackage org.acme.web;\nclass B {}\n")
	write(t, repo, "samples/org/acme/core/A.java", "package org.acme.core;\nclass A {}\n") // duplicate
	write(t, repo, "Loose.java", "public class Loose {}\n")                                // default package
	write(t, repo, "lib/commons & co.jar", "PK")
	write(t, repo, "WEB-INF/web.xml", "<web-app/>")

	runDir := t.TempDir()
	p, err := engine.NewProject(context.Background(), repo, runDir)
	if err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(runDir, "mta-src", p.Name)
	if err := buildrun.Stage(context.Background(), p.Root, staged); err != nil {
		t.Fatal(err)
	}
	rel, info, err := synthesizePOM(context.Background(), p, staged)
	if err != nil {
		t.Fatal(err)
	}
	if info.JavaFiles != 4 || info.Collisions != 1 || info.Jars != 1 {
		t.Fatalf("info = %+v; want 4 java files, 1 collision, 1 jar", info)
	}
	for stagedRel, repoRel := range map[string]string{
		"src/main/java/org/acme/core/A.java": "samples/org/acme/core/A.java", // sorted first wins
		"src/main/java/org/acme/web/B.java":  "modules/web/src/org/acme/web/B.java",
		"src/main/java/Loose.java":           "Loose.java",
	} {
		if rel[stagedRel] != repoRel {
			t.Errorf("relocated[%s] = %q, want %q", stagedRel, rel[stagedRel], repoRel)
		}
		if _, err := os.Stat(filepath.Join(staged, filepath.FromSlash(stagedRel))); err != nil {
			t.Errorf("missing in staged copy: %s", stagedRel)
		}
	}
	pom, err := os.ReadFile(filepath.Join(staged, "pom.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(pom), "<systemPath>${project.basedir}/lib/commons &amp; co.jar</systemPath>") ||
		!strings.Contains(string(pom), "<maven.compiler.source>1.8</maven.compiler.source>") {
		t.Fatalf("unexpected pom:\n%s", pom)
	}
	if _, err := os.Stat(filepath.Join(staged, ".project")); err == nil {
		t.Error(".project left at the staged root; JDT LS might import it as an Eclipse project")
	}
	if _, err := os.Stat(filepath.Join(staged, "WEB-INF", "web.xml")); err != nil {
		t.Error("descriptor not kept at its repository-relative path")
	}
	// the repository itself is untouched
	for _, f := range []string{"src/java/org/acme/core/A.java", ".project"} {
		if _, err := os.Stat(filepath.Join(repo, filepath.FromSlash(f))); err != nil {
			t.Errorf("repository file changed: %s", f)
		}
	}
	if _, err := os.Stat(filepath.Join(repo, "pom.xml")); err == nil {
		t.Error("pom.xml was written into the repository")
	}
	// incident URIs from the staged copy map back to repository paths
	m := newPathMapper(p)
	m.staged, m.relocated = filepath.ToSlash(staged), rel
	if got, space := m.mapURI("file:///" + filepath.ToSlash(filepath.Join(staged, "src/main/java/org/acme/web/B.java"))); got != "modules/web/src/org/acme/web/B.java" || space != spaceRepo {
		t.Errorf("mapURI = %q (%s), want the repository path", got, space)
	}
}
