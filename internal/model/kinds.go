package model

// Evidence kinds shared across analyzers. Correlation and reporting match on these, so analyzers
// must use these constants rather than ad-hoc strings. Values keys listed per kind are the contract.
const (
	// identity — Values: files, bytes, java_files, java_lines, jars, wars, ears
	KindIdentity = "identity.summary"

	// git — Values: state=none|enclosed|own, toplevel, sha, branch, dirty_count, remotes
	KindGitRepository = "git.repository"

	// build systems — Subject: ant|maven|gradle|eclipse|netbeans|manual-libs; Values: at_root=true|false, count
	KindBuildSystem       = "build.system"
	KindBuildSystemAbsent = "build.system.absent" // Subject: the system not found

	// java version — never merge these kinds.
	// Values: raw, resolved, origin=ant|maven|gradle|eclipse|netbeans, defined_at (file:line)
	KindJavaSource       = "java.source"
	KindJavaTarget       = "java.target"
	KindJavaRelease      = "java.release"
	KindJavaImplicit     = "java.javac.implicit"      // Values: javac_total, no_source, no_target
	KindJavaClassMajor   = "java.binary.class-major"  // Values: max_major, max_java, distribution
	KindJavaManifestJDK  = "java.binary.manifest-jdk" // Values: distribution
	KindJavaLocalRuntime = "java.local-runtime"       // Category ENVIRONMENT. Values: major, version, java_home, javac
	KindJavaSummary      = "java.summary"             // INFERRED side-by-side view

	// prerequisites — Subject: tool name. Values: requirement=REQUIRED|REQUIRED_FOR_CAPABILITY|OPTIONAL,
	// state=AVAILABLE|MISSING|INCOMPATIBLE, capability, version, suggestion, reason
	KindPrereqTool = "prereq.tool"

	// Ant — Subject per kind as noted.
	KindAntProject          = "ant.project"           // Subject: build file; Values: default, targets, imports
	KindAntImportUnresolved = "ant.import.unresolved" // Values: raw, reason
	KindAntPackaging        = "ant.packaging"         // Subject: war|ear|jar|ejbjar
	KindAntEnvRef           = "ant.env-ref"           // Subject: env var name
	KindAntTargetSafety     = "ant.target.safety"     // Subject: target; Values: class=SAFE|REVIEW_REQUIRED|DANGEROUS|UNKNOWN, reasons, closure, default
	KindAntBuildCandidate   = "ant.build-candidate"   // Subject: target; Values: class, reason

	// enterprise / technology / dependencies
	KindEnterpriseDescriptor = "enterprise.descriptor" // Subject: descriptor type (web.xml, ejb-jar.xml…)
	KindEnterpriseAppServer  = "enterprise.appserver"  // Subject: family (jboss, weblogic, websphere, glassfish, tomcat, geronimo)
	KindEnterpriseJavaEE     = "enterprise.javaee"     // INFERRED summary; Values: detected=true|false, signals
	KindTechUsage            = "tech.usage"            // Subject: technology; Values: files, occurrences, packages
	KindDepsInventory        = "deps.inventory"        // Values: jars, wars, ears, total_bytes
	KindDepsDuplicate        = "deps.duplicate"        // Subject: artifact name; Values: versions
	KindDepsOld              = "deps.old"              // Subject: artifact; Values: version, reason

	// local build — Values: attempted=true|false, class=<BuildFailureClass>|SUCCESS, tool, target, exit_code,
	// staged_copy (path relative to run dir), java_home, tool_version
	KindBuildLocal = "build.local"

	// CI — workflow: Subject file; Values: purpose=BUILD|TEST|LINT|DEPLOY|RELEASE|OTHER, java_versions, tools.
	// same-sha: Values: state=SUCCESS_SAME_SHA|FAILED_SAME_SHA|NO_RUN_SAME_SHA|SUCCESS_OTHER_SHA_ONLY|UNAVAILABLE, run_url, workflow
	KindCIWorkflow = "ci.workflow"
	KindCISameSHA  = "ci.same-sha"

	// migration — run: Values: state=SUCCEEDED|FAILED|NOT_RUN, failure_class, route=source|binary|staged-pom, duration_ms.
	// violation: RuleID, Target, Effort set; Values: category, labels, files.
	KindMTAPrediction = "mta.prediction" // Values: java_provider=APPLICABLE|NOT_APPLICABLE, reason
	KindMTAPrereq     = "mta.prereq"     // Subject: item; Values: state, detail
	KindMTARun        = "mta.run"
	KindMTAViolation  = "mta.violation"
	KindMTAInsight    = "mta.insight"
	KindMTARuleError  = "mta.rule-error"
	KindMTACoverage   = "mta.coverage"  // Values: rules_total, rules_evaluable, effort_rules_total, effort_rules_evaluable
	KindJdeps         = "jdk.jdeps"     // Subject: internal API / module
	KindJdeprscan     = "jdk.jdeprscan" // Subject: deprecated/removed API

	// environment preparation (internal/prepare) — install: Subject tool/package; Values package,
	// state=INSTALLED|ALREADY_PRESENT|FAILED|NOT_INSTALLABLE, version_after, reason.
	// java-home: Values user_before, user_after (observed, user-level JAVA_HOME).
	// build-jdk: Values name, home, reason (why this JDK matches the declared target).
	KindEnvInstall  = "env.install"
	KindEnvJavaHome = "env.java-home"
	KindEnvBuildJDK = "env.build-jdk"

	// engine
	KindAnalyzerFailed  = "analyzer.failed"
	KindAnalyzerSkipped = "analyzer.skipped"
)

// BuildFailureClass values for KindBuildLocal Values["class"].
const (
	BuildSuccess                = "SUCCESS"
	BuildJDKIncompatible        = "JDK_INCOMPATIBLE"
	BuildMissingDependency      = "MISSING_DEPENDENCY_OR_SOURCE"
	BuildMissingEnvVar          = "MISSING_ENVIRONMENT_VARIABLE"
	BuildMissingRepository      = "MISSING_OR_INACCESSIBLE_REPOSITORY"
	BuildMissingAppServer       = "MISSING_APP_SERVER_OR_RUNTIME"
	BuildSourceOrBuildError     = "SOURCE_OR_BUILD_ERROR"
	BuildToolNotAvailable       = "TOOL_NOT_AVAILABLE"
	BuildUnsafeTarget           = "UNSAFE_BUILD_TARGET"
	BuildTimeout                = "TIMEOUT"
	BuildUnknown                = "UNKNOWN"
	BuildNotAttemptedNoDecision = "NOT_ATTEMPTED_NO_DECISION" // side-effect capability not enabled
)
