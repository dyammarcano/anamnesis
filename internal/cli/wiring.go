package cli

import (
	"anamnesis/internal/analyzers/antfacts"
	"anamnesis/internal/analyzers/antsafety"
	"anamnesis/internal/analyzers/buildsys"
	"anamnesis/internal/analyzers/ci"
	"anamnesis/internal/analyzers/deps"
	"anamnesis/internal/analyzers/enterprise"
	"anamnesis/internal/analyzers/gitinfo"
	"anamnesis/internal/analyzers/identity"
	"anamnesis/internal/analyzers/javaver"
	"anamnesis/internal/analyzers/localbuild"
	"anamnesis/internal/analyzers/mtapredict"
	"anamnesis/internal/analyzers/prereq"
	"anamnesis/internal/analyzers/tech"
	"anamnesis/internal/ant"
	"anamnesis/internal/engine"
	"anamnesis/internal/migration/jdktools"
	"anamnesis/internal/migration/mta"
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
