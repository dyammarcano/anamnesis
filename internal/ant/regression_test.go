package ant_test

import (
	"testing"

	"github.com/dyammarcano/anamnesis/internal/ant"
)

// Regressions found by the independent review of the EJBCA validation (docs/validation/ejbca-4.0.16.md).

// An unresolved destination that merely LOOKS like a build directory must not be SAFE:
// "${build.dir}/../../outside" can point anywhere once build.dir is defined elsewhere.
func TestUnresolvedBuildishDestinationIsNotSafe(t *testing.T) {
	for name, body := range map[string]string{
		"javac destdir escapes": `<target name="c"><javac srcdir="src" destdir="${build.dir}/../../outside"/></target>`,
		"jar destfile unknown":  `<target name="c"><jar destfile="${dist.dir}/app.jar" basedir="classes"/></target>`,
	} {
		t.Run(name, func(t *testing.T) {
			if ts := classOf(t, body, "c"); ts.Class == ant.ClassSafe {
				t.Fatalf("class = SAFE; an unresolved destination must be at least REVIEW_REQUIRED (reasons: %+v)", ts.Reasons)
			}
		})
	}
}

// Ant rejects cyclic depends graphs, so a target in one can never run as declared: UNKNOWN, not SAFE.
func TestDependsCycleIsUnknownNotSafe(t *testing.T) {
	body := `<target name="a" depends="b"><javac srcdir="s" destdir="build"/></target>` +
		`<target name="b" depends="a"><javac srcdir="s" destdir="build"/></target>`
	ts := classOf(t, body, "a")
	if ts.Class == ant.ClassSafe {
		t.Fatalf("class = SAFE for a target in a depends cycle; want UNKNOWN (reasons: %+v)", ts.Reasons)
	}
}
