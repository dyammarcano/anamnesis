package deps

import "slices"

import "strings"

// parseVer reads the leading numeric components of a version ("3.0.5.RELEASE" -> [3 0 5]).
func parseVer(s string) []int {
	var out []int
	for _, seg := range strings.FieldsFunc(s, func(r rune) bool { return r == '.' || r == '-' || r == '_' }) {
		n, digits := 0, 0
		for _, r := range seg {
			if r < '0' || r > '9' || digits >= 9 {
				break
			}
			n = n*10 + int(r-'0')
			digits++
		}
		if digits == 0 {
			break
		}
		out = append(out, n)
		if digits < len(seg) {
			break
		}
	}
	return out
}

func cmpVer(a, b []int) int {
	for i := 0; i < len(a) || i < len(b); i++ {
		x, y := 0, 0
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

var springModules = map[string]bool{
	"spring": true, "spring-core": true, "spring-beans": true, "spring-context": true, "spring-context-support": true,
	"spring-web": true, "spring-webmvc": true, "spring-webmvc-portlet": true, "spring-jdbc": true, "spring-orm": true,
	"spring-tx": true, "spring-aop": true, "spring-aspects": true, "spring-jms": true, "spring-expression": true,
	"spring-test": true, "spring-instrument": true, "spring-oxm": true, "spring-struts": true, "spring-asm": true,
	"spring-agent": true, "spring-framework": true,
	"org.springframework.core": true, "org.springframework.beans": true, "org.springframework.context": true,
	"org.springframework.web": true, "org.springframework.web.servlet": true, "org.springframework.jdbc": true,
	"org.springframework.orm": true, "org.springframework.transaction": true, "org.springframework.aop": true,
	"org.springframework.asm": true, "org.springframework.expression": true, "org.springframework.jms": true,
}

var hibernateModules = map[string]bool{
	"hibernate": true, "hibernate-core": true, "hibernate-entitymanager": true, "hibernate-annotations": true,
	"hibernate-commons-annotations": true, "hibernate3": true,
}

var strutsModules = map[string]bool{
	"struts": true, "struts-core": true, "struts-taglib": true, "struts-tiles": true, "struts-extras": true, "struts-el": true,
}

func firstKey(keys []string, pred func(string) bool) (string, bool) {
	for _, k := range keys {
		if pred(k) {
			return k, true
		}
	}
	return "", false
}

func isOneOf(names ...string) func(string) bool {
	return func(k string) bool {
		return slices.Contains(names, k)
	}
}

// checkOld reports a well-known library on a clearly old line. It uses only name and version.
func checkOld(keys []string, ver string) (subject, reason string, ok bool) {
	v := parseVer(ver)
	major := -1
	if len(v) > 0 {
		major = v[0]
	}
	if k, f := firstKey(keys, isOneOf("log4j")); f && major == 1 {
		return k, "Log4j 1.x line; superseded by Log4j 2 and no longer released", true
	}
	if k, f := firstKey(keys, isOneOf("commons-collections")); f && major >= 0 && major <= 3 && cmpVer(v, []int{3, 2, 2}) < 0 {
		return k, "commons-collections before 3.2.2: InvokerTransformer is serializable in these releases, which enables gadget-chain attacks when untrusted data is deserialized (no advisory id asserted here; check current advisories)", true
	}
	if k, f := firstKey(keys, isOneOf("commons-collections4")); f && major == 4 && cmpVer(v, []int{4, 1}) < 0 {
		return k, "commons-collections4 before 4.1: InvokerTransformer is serializable in these releases, which enables gadget-chain attacks when untrusted data is deserialized (no advisory id asserted here; check current advisories)", true
	}
	if k, f := firstKey(keys, func(k string) bool { return strutsModules[k] }); f && major == 1 {
		return k, "Struts 1.x line; the project is end-of-life", true
	}
	if k, f := firstKey(keys, func(k string) bool { return springModules[k] }); f && major >= 1 && major < 4 {
		return k, "Spring Framework older than 4.0", true
	}
	if k, f := firstKey(keys, func(k string) bool { return hibernateModules[k] }); f && major >= 1 && major < 4 {
		return k, "Hibernate older than 4.0", true
	}
	if k, f := firstKey(keys, isOneOf("xercesimpl", "xerces", "xalan", "xml-apis")); f {
		return k, "bundles an XML parser/transformer implementation (Xerces/Xalan/xml-apis) that can shadow the JDK's built-in JAXP implementation depending on class loader order", true
	}
	if k, f := firstKey(keys, isOneOf("junit")); f && major == 3 {
		return k, "JUnit 3.x line", true
	}
	if k, f := firstKey(keys, isOneOf("axis", "axis-jaxrpc", "axis-saaj", "axis-wsdl4j")); f && major == 1 {
		return k, "Apache Axis 1.x line; superseded by Axis2 and CXF", true
	}
	if k, f := firstKey(keys, func(k string) bool {
		return strings.HasPrefix(k, "bcprov") || strings.HasPrefix(k, "bcpkix") || strings.HasPrefix(k, "bcmail")
	}); f {
		bv := v
		if len(v) == 1 && len(ver) >= 3 && v[0] >= 100 && v[0] < 1000 && strings.Trim(ver, "0123456789") == "" {
			bv = []int{v[0] / 100, v[0] % 100} // old file names: bcprov-jdk15-146.jar means 1.46
		}
		if len(bv) > 0 && cmpVer(bv, []int{1, 50}) < 0 {
			return k, "BouncyCastle older than 1.50", true
		}
	}
	return "", "", false
}
