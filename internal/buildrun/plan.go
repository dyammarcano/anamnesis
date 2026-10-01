package buildrun

import (
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// DefaultTimeout bounds a build.
const DefaultTimeout = 20 * time.Minute

// Command is a planned build invocation. Dir is under the staged copy, never the analyzed repo.
type Command struct {
	Name    string
	Args    []string
	Dir     string
	Timeout time.Duration
}

// PlanAnt plans `ant -noinput -buildfile <staged>/<buildFile> <target>`. buildFile is repo-relative.
func PlanAnt(staged, buildFile, target string) Command {
	bf := filepath.Join(staged, filepath.FromSlash(buildFile))
	args := []string{"-noinput", "-buildfile", bf}
	if target != "" {
		args = append(args, target)
	}
	return Command{Name: "ant", Args: args, Dir: filepath.Dir(bf), Timeout: DefaultTimeout}
}

// PlanMaven plans `mvn -B -DskipTests package`. Callers must hold the build side-effect capability.
func PlanMaven(staged string) Command {
	return Command{Name: "mvn", Args: []string{"-B", "-DskipTests", "package"}, Dir: staged, Timeout: DefaultTimeout}
}

// PlanGradle plans the project's wrapper (`gradlew.bat assemble -x test` on Windows). The wrapper is
// repository code, so this is a side effect. ok=false when no wrapper exists in the staged copy.
func PlanGradle(staged string) (Command, bool) {
	name := "gradlew"
	if runtime.GOOS == "windows" {
		name = "gradlew.bat"
	}
	p := filepath.Join(staged, name)
	if _, err := os.Stat(p); err != nil {
		return Command{}, false
	}
	return Command{Name: p, Args: []string{"assemble", "-x", "test"}, Dir: staged, Timeout: DefaultTimeout}, true
}
