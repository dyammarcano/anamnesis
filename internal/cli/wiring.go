package cli

import (
	"github.com/dyammarcano/anamnesis/internal/analyzers/antfacts"
	"github.com/dyammarcano/anamnesis/internal/analyzers/antsafety"
	"github.com/dyammarcano/anamnesis/internal/analyzers/buildsys"
	"github.com/dyammarcano/anamnesis/internal/analyzers/ci"
	"github.com/dyammarcano/anamnesis/internal/analyzers/deps"
	"github.com/dyammarcano/anamnesis/internal/analyzers/enterprise"
	"github.com/dyammarcano/anamnesis/internal/analyzers/gitinfo"
	"github.com/dyammarcano/anamnesis/internal/analyzers/identity"
	"github.com/dyammarcano/anamnesis/internal/analyzers/javaver"
	"github.com/dyammarcano/anamnesis/internal/analyzers/localbuild"
	"github.com/dyammarcano/anamnesis/internal/analyzers/mtapredict"
	"github.com/dyammarcano/anamnesis/internal/analyzers/prereq"
	"github.com/dyammarcano/anamnesis/internal/analyzers/tech"
	"github.com/dyammarcano/anamnesis/internal/ant"
	"github.com/dyammarcano/anamnesis/internal/engine"
	"github.com/dyammarcano/anamnesis/internal/migration/jdktools"
	"github.com/dyammarcano/anamnesis/internal/migration/mta"
)

// registerAll registers every analyzer with the engine. Assess calls it exactly once.
func registerAll() {
	for _, set := range [][]engine.Analyzer{
		identity.Analyzers(),
		gitinfo.Analyzers(),
		buildsys.Analyzers(),
		javaver.Analyzers(),
		prereq.Analyzers(),
		mtapredict.Analyzers(),
		antfacts.Analyzers(),
		antsafety.Analyzers(),
		enterprise.Analyzers(),
		tech.Analyzers(),
		deps.Analyzers(),
		localbuild.Analyzers(),
		ci.Analyzers(),
		mta.Analyzers(),
		jdktools.Analyzers(),
	} {
		engine.Register(set...)
	}
	localbuild.AntTargetChooser = ant.ChooseTarget
}
