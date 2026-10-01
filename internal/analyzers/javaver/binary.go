package javaver

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"anamnesis/internal/engine"
	"anamnesis/internal/model"
	"anamnesis/internal/scan"
)

const (
	maxLooseSample = 2000
	maxManifest    = 64 * 1024
)

type archiveResult struct {
	rel      string
	major    int // 0 when no class entry was found
	noClass  bool
	err      string
	manifest map[string]string
}

// readMajor returns the class-file major version from the first 8 bytes of r.
func readMajor(r io.Reader) (int, error) {
	var b [8]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}
	if b[0] != 0xCA || b[1] != 0xFE || b[2] != 0xBA || b[3] != 0xBE {
		return 0, fmt.Errorf("not a class file (bad magic)")
	}
	return int(b[6])<<8 | int(b[7]), nil
}

func inspectArchive(ix *scan.Index, f scan.File) archiveResult {
	res := archiveResult{rel: f.Rel}
	zr, err := zip.OpenReader(ix.Abs(f))
	if err != nil {
		res.err = err.Error()
		return res
	}
	defer func() { _ = zr.Close() }()
	var cls, mf *zip.File
	for _, zf := range zr.File {
		n := zf.Name
		switch {
		case strings.EqualFold(n, "META-INF/MANIFEST.MF"):
			mf = zf
		case strings.HasSuffix(n, ".class") && !strings.HasSuffix(n, "module-info.class") && !strings.HasPrefix(n, "META-INF/versions/"):
			if cls == nil || (!strings.HasPrefix(cls.Name, "WEB-INF/classes/") && strings.HasPrefix(n, "WEB-INF/classes/")) {
				cls = zf
			}
		}
	}
	if cls == nil {
		res.noClass = true
	} else if rc, err := cls.Open(); err != nil {
		res.err = "class entry " + cls.Name + ": " + err.Error()
	} else {
		m, err := readMajor(rc)
		_ = rc.Close()
		if err != nil {
			res.err = "class entry " + cls.Name + ": " + err.Error()
		} else {
			res.major = m
		}
	}
	if mf != nil {
		if rc, err := mf.Open(); err == nil {
			data, _ := io.ReadAll(io.LimitReader(rc, maxManifest))
			_ = rc.Close()
			res.manifest = parseManifest(string(data))
		}
	}
	return res
}

// parseManifest returns the JDK-related main attributes, unfolding continuation lines.
func parseManifest(s string) map[string]string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.ReplaceAll(s, "\n ", "")
	out := map[string]string{}
	for l := range strings.SplitSeq(s, "\n") {
		if l == "" {
			break // end of the main section
		}
		i := strings.Index(l, ":")
		if i <= 0 {
			continue
		}
		switch k := strings.ToLower(l[:i]); k {
		case "created-by", "build-jdk", "build-jdk-spec":
			out[k] = strings.TrimSpace(l[i+1:])
		}
	}
	return out
}

func analyzeBinaries(ctx context.Context, p *engine.Project, sink engine.Sink) {
	ix := p.Files
	var archives []scan.File
	for _, ext := range []string{".jar", ".war", ".ear"} {
		archives = append(archives, ix.WithExt(ext)...)
	}
	loose := ix.WithExt(".class")
	if len(archives) == 0 && len(loose) == 0 {
		sink.Emit(model.Evidence{
			Category: model.CatJavaVersion, Kind: model.KindJavaClassMajor, Subject: "binaries",
			Finding: "No jar/war/ear archives or loose .class files were found.", Status: model.Info, Confidence: model.Observed,
			Values: map[string]string{"archives": "0", "loose_classes": "0"},
		})
		return
	}

	var mu sync.Mutex
	var results []archiveResult
	scan.ForEach(ctx, archives, func(f scan.File) {
		r := inspectArchive(ix, f)
		mu.Lock()
		results = append(results, r)
		mu.Unlock()
	})
	sort.Slice(results, func(i, j int) bool { return results[i].rel < results[j].rel })

	if len(archives) > 0 {
		emitArchiveMajor(sink, results)
		emitManifest(sink, results)
	}
	if len(loose) > 0 {
		emitLoose(ctx, ix, loose, sink)
	}
}

func emitArchiveMajor(sink engine.Sink, results []archiveResult) {
	dist := map[int]int{}
	maxMajor, noClass := 0, 0
	var bad []model.Location
	for _, r := range results {
		switch {
		case r.err != "":
			if len(bad) < maxLocations {
				bad = append(bad, model.Location{Path: r.rel})
			}
		case r.noClass:
			noClass++
		default:
			dist[r.major]++
			maxMajor = max(maxMajor, r.major)
		}
	}
	badTotal := 0
	for _, r := range results {
		if r.err != "" {
			badTotal++
		}
	}
	var locs []model.Location
	total := 0
	for _, r := range results {
		if r.err == "" && !r.noClass && r.major == maxMajor {
			total++
			if len(locs) < maxLocations {
				locs = append(locs, model.Location{Path: r.rel})
			}
		}
	}
	withClass := len(results) - noClass - badTotal
	vals := map[string]string{
		"archives": strconv.Itoa(len(results)), "archives_with_class": strconv.Itoa(withClass),
		"archives_without_class": strconv.Itoa(noClass), "archives_unreadable": strconv.Itoa(badTotal),
		"distribution": distributionString(dist), "locations_total": strconv.Itoa(total),
	}
	finding := "No readable class files were found in " + strconv.Itoa(len(results)) + " archives."
	conf := model.Unknown
	if withClass > 0 {
		vals["max_major"] = strconv.Itoa(maxMajor)
		vals["max_java"] = javaFromMajor(maxMajor)
		finding = "Archives contain class files up to major version " + strconv.Itoa(maxMajor) + " (Java " + javaFromMajor(maxMajor) +
			") across " + strconv.Itoa(withClass) + " archives."
		conf = model.Observed
	}
	sink.Emit(model.Evidence{
		Category: model.CatJavaVersion, Kind: model.KindJavaClassMajor, Subject: "archives",
		Finding: finding, Status: model.Info, Confidence: conf, Locations: locs, Values: vals,
		Assumptions: []string{"max_java is mapped from the class-file major version table (45=1.1 ... 52=8, otherwise major-44); locations are the archives at max_major."},
		Limitations: []string{
			"Only the first class entry of each archive was read, so an archive mixing class versions is represented by one class.",
			"Multi-release entries (META-INF/versions) and module-info.class are skipped; archives nested inside war/ear files are not opened.",
			"A class file's major version states the bytecode level it was compiled for, not the JDK that compiled it.",
		},
	})
	if badTotal > 0 {
		sink.Emit(model.Evidence{
			Category: model.CatJavaVersion, Kind: model.KindJavaClassMajor, Subject: "archives-unreadable",
			Finding: strconv.Itoa(badTotal) + " archive(s) could not be read as zip files or had an unreadable class entry.",
			Status:  model.Warn, Confidence: model.Observed, Locations: bad,
			Values:      map[string]string{"locations_total": strconv.Itoa(badTotal)},
			Limitations: []string{"These archives are excluded from the class-version distribution."},
		})
	}
}

func emitManifest(sink engine.Sink, results []archiveResult) {
	counts := map[string]map[string]int{"created-by": {}, "build-jdk": {}, "build-jdk-spec": {}}
	withManifest := 0
	for _, r := range results {
		if r.manifest != nil {
			withManifest++
		}
		for k, v := range r.manifest {
			counts[k][v]++
		}
	}
	vals := map[string]string{"archives": strconv.Itoa(len(results)), "manifests": strconv.Itoa(withManifest)}
	var all []string
	found := false
	for _, k := range []string{"created-by", "build-jdk", "build-jdk-spec"} {
		s := rank(counts[k])
		vals[strings.ReplaceAll(k, "-", "_")] = s
		if s != "" {
			found = true
			all = append(all, k+"="+s)
		}
	}
	vals["distribution"] = strings.Join(all, " | ")
	finding := "No Created-By, Build-Jdk or Build-Jdk-Spec manifest attributes were found in " + strconv.Itoa(len(results)) + " archives."
	if found {
		finding = "Archive manifests record the JDK that built them (Created-By / Build-Jdk / Build-Jdk-Spec) in " + strconv.Itoa(withManifest) + " manifests."
	}
	sink.Emit(model.Evidence{
		Category: model.CatJavaVersion, Kind: model.KindJavaManifestJDK, Subject: "archives",
		Finding: finding, Status: model.Info, Confidence: model.Observed, Values: vals,
		Limitations: []string{"Manifest attributes are self-reported by whatever tool built each archive, including third-party libraries, and may be absent or wrong; top 30 values per attribute are listed."},
	})
}

// rank renders "value:count" pairs by descending count, limited to 30 values.
func rank(m map[string]int) string {
	type vc struct {
		v string
		c int
	}
	var list []vc
	for v, c := range m {
		list = append(list, vc{v, c})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].c != list[j].c {
			return list[i].c > list[j].c
		}
		return list[i].v < list[j].v
	})
	if len(list) > 30 {
		list = list[:30]
	}
	parts := make([]string, len(list))
	for i, x := range list {
		parts[i] = x.v + ":" + strconv.Itoa(x.c)
	}
	return strings.Join(parts, "; ")
}

func emitLoose(ctx context.Context, ix *scan.Index, loose []scan.File, sink engine.Sink) {
	sample := loose
	if len(loose) > maxLooseSample {
		step := (len(loose) + maxLooseSample - 1) / maxLooseSample
		sample = make([]scan.File, 0, maxLooseSample)
		for i := 0; i < len(loose); i += step {
			sample = append(sample, loose[i])
		}
	}
	var mu sync.Mutex
	dist := map[int]int{}
	byMajor := map[int][]string{}
	maxMajor, invalid := 0, 0
	scan.ForEach(ctx, sample, func(f scan.File) {
		fh, err := os.Open(ix.Abs(f))
		var m int
		if err == nil {
			m, err = readMajor(fh)
			_ = fh.Close()
		}
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			invalid++
			return
		}
		dist[m]++
		byMajor[m] = append(byMajor[m], f.Rel)
		maxMajor = max(maxMajor, m)
	})
	vals := map[string]string{
		"loose_classes": strconv.Itoa(len(loose)), "sampled": strconv.Itoa(len(sample)),
		"invalid": strconv.Itoa(invalid), "distribution": distributionString(dist),
	}
	finding := "No loose .class file could be read."
	conf := model.Unknown
	var locs []model.Location
	if len(dist) > 0 {
		paths := byMajor[maxMajor]
		sort.Strings(paths)
		vals["locations_total"] = strconv.Itoa(len(paths))
		for i, rel := range paths {
			if i >= maxLocations {
				break
			}
			locs = append(locs, model.Location{Path: rel})
		}
		vals["max_major"] = strconv.Itoa(maxMajor)
		vals["max_java"] = javaFromMajor(maxMajor)
		finding = "Loose .class files reach major version " + strconv.Itoa(maxMajor) + " (Java " + javaFromMajor(maxMajor) + ") in the sampled " + strconv.Itoa(len(sample)) + " of " + strconv.Itoa(len(loose)) + "."
		conf = model.Observed
	}
	sink.Emit(model.Evidence{
		Category: model.CatJavaVersion, Kind: model.KindJavaClassMajor, Subject: "loose-classes",
		Finding: finding, Status: model.Info, Confidence: conf, Locations: locs, Values: vals,
		Assumptions: []string{"max_java is mapped from the class-file major version table (45=1.1 ... 52=8, otherwise major-44)."},
		Limitations: []string{"At most 2000 loose .class files are sampled, evenly spaced in path order; classes outside the sample are not represented."},
	})
}
