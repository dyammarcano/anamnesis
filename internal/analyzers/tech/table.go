package tech

import "strings"

// entry maps a package prefix to a technology. removed is the first JDK release without the API.
type entry struct {
	prefix  string
	tech    string
	removed string
	note    string
}

const (
	noteJavaEE11 = "Java EE module removed from the JDK in 11 (JEP 320); needs an explicit dependency on newer JDKs"
)

var entries = []entry{
	{"javax.ejb", "EJB", "", ""},
	{"javax.servlet", "Servlet", "", ""},
	{"javax.servlet.jsp", "JSP", "", ""},
	{"javax.servlet.jsp.jstl", "JSTL", "", ""},
	{"javax.persistence", "JPA", "", ""},
	{"org.hibernate", "Hibernate", "", ""},
	{"javax.jms", "JMS", "", ""},
	{"javax.xml.ws", "JAX-WS", "11", noteJavaEE11},
	{"javax.jws", "JAX-WS", "11", noteJavaEE11},
	{"javax.xml.soap", "SOAP/SAAJ", "11", noteJavaEE11},
	{"javax.xml.bind", "JAXB", "11", noteJavaEE11},
	{"com.sun.xml.bind", "JAXB RI (com.sun.xml.bind)", "", ""},
	{"javax.xml.rpc", "JAX-RPC", "", ""},
	{"javax.ws.rs", "JAX-RS", "", ""},
	{"javax.naming", "JNDI", "", ""},
	{"java.sql", "JDBC", "", ""},
	{"javax.sql", "JDBC", "", ""},
	{"oracle.jdbc", "Oracle JDBC", "", ""},
	{"oracle.sql", "Oracle JDBC", "", ""},
	{"org.springframework", "Spring", "", ""},
	{"org.apache.struts", "Struts", "", ""},
	{"org.apache.struts2", "Struts 2", "", ""},
	{"com.opensymphony.xwork2", "Struts 2", "", ""},
	{"javax.faces", "JSF", "", ""},
	{"javax.inject", "CDI", "", ""},
	{"javax.enterprise", "CDI", "", ""},
	{"javax.transaction", "JTA", "11", "javax.transaction (JTA API) is no longer in the JDK from 11; javax.transaction.xa remains (partial removal)"},
	{"javax.transaction.xa", "JTA XA (java.transaction.xa)", "", ""},
	{"java.rmi", "RMI", "", ""},
	{"org.omg", "CORBA", "11", "CORBA / RMI-IIOP modules removed from the JDK in 11 (JEP 320)"},
	{"javax.rmi", "CORBA", "11", "CORBA / RMI-IIOP modules removed from the JDK in 11 (JEP 320)"},
	{"javax.activation", "javax.activation", "11", noteJavaEE11},
	{"javax.annotation", "javax.annotation (Common Annotations)", "11", "Common Annotations module removed from the JDK in 11; javax.annotation.processing stays in the JDK"},
	{"javax.annotation.processing", "Annotation processing (JDK)", "", ""},
	{"javax.mail", "JavaMail", "", ""},
	{"javax.security.auth", "javax.security.auth/cert", "", ""},
	{"javax.security.cert", "javax.security.auth/cert", "", ""},
	{"weblogic", "WebLogic APIs", "", ""},
	{"com.ibm.websphere", "WebSphere APIs", "", ""},
	{"com.ibm.ws", "WebSphere APIs", "", ""},
	{"org.jboss", "JBoss", "", ""},
	{"org.apache.catalina", "Tomcat", "", ""},
	{"org.apache.tomcat", "Tomcat", "", ""},
	{"org.apache.coyote", "Tomcat", "", ""},
	{"sun", "JDK internals (sun.*, com.sun.*)", "", "internal, non-public JDK packages; strongly encapsulated in newer JDKs"},
	{"com.sun", "JDK internals (sun.*, com.sun.*)", "", "internal, non-public JDK packages; strongly encapsulated in newer JDKs"},
	{"sun.misc", "sun.misc", "9", "partial: sun.misc.Unsafe is kept in jdk.unsupported, other classes (for example BASE64Encoder/Decoder) were removed in 9"},
	{"com.sun.net.httpserver", "com.sun supported JDK APIs", "", ""},
	{"com.sun.management", "com.sun supported JDK APIs", "", ""},
	{"com.sun.source", "com.sun supported JDK APIs", "", ""},
	{"com.sun.tools.javac", "com.sun supported JDK APIs", "", ""},
	{"com.sun.security.auth", "com.sun supported JDK APIs", "", ""},
	{"com.sun.faces", "Mojarra (com.sun.faces)", "", ""},
	{"com.sun.xml", "com.sun.xml (Metro / JAXB / JAX-WS implementation)", "", ""},
	{"com.sun.mail", "JavaMail", "", ""},
	{"com.sun.jersey", "Jersey 1.x (com.sun.jersey)", "", ""},
	{"org.apache.axis", "Axis", "", ""},
	{"org.apache.axis2", "Axis2", "", ""},
	{"org.codehaus.xfire", "XFire", "", ""},
	{"org.apache.cxf", "CXF", "", ""},
	{"org.bouncycastle", "BouncyCastle", "", ""},
	{"org.apache.log4j", "log4j 1.x", "", ""},
	{"org.apache.logging.log4j", "Log4j 2", "", ""},
	{"org.apache.commons.logging", "commons-logging", "", ""},
	{"jakarta", "jakarta.* namespace", "", ""},
}

var (
	byPrefix = buildPrefixIndex()
	techInfo = buildTechInfo()
)

func buildPrefixIndex() map[string]entry {
	m := make(map[string]entry, len(entries))
	for _, e := range entries {
		m[e.prefix] = e
	}
	return m
}

// buildTechInfo keeps, per technology, the entry that carries removal information when there is one.
func buildTechInfo() map[string]entry {
	m := map[string]entry{}
	for _, e := range entries {
		cur, ok := m[e.tech]
		if !ok || (cur.removed == "" && e.removed != "") {
			m[e.tech] = e
		}
	}
	return m
}

// lookup finds the entry with the longest matching package prefix for a dotted import name.
func lookup(name string) (entry, bool) {
	for s := name; s != ""; {
		if e, ok := byPrefix[s]; ok {
			return e, true
		}
		i := strings.LastIndexByte(s, '.')
		if i < 0 {
			break
		}
		s = s[:i]
	}
	return entry{}, false
}

// pkgOf returns the package part of an import name: the segments before the first upper-case one.
func pkgOf(name string) string {
	segs := strings.Split(name, ".")
	for i, s := range segs {
		if s != "" && s[0] >= 'A' && s[0] <= 'Z' {
			return strings.Join(segs[:i], ".")
		}
	}
	return name
}

const tagOther = "JSP custom taglibs"

// taglibTech maps a taglib URI to a technology name.
func taglibTech(uri string) string {
	low := strings.ToLower(uri)
	switch {
	case strings.Contains(low, "java.sun.com/jsp/jstl"), strings.Contains(low, "java.sun.com/jstl"),
		strings.Contains(low, "xmlns.jcp.org/jsp/jstl"), strings.Contains(low, "jakarta.tags"):
		return "JSTL"
	case strings.Contains(low, "struts.apache.org/tags"), strings.Contains(low, "jakarta.apache.org/struts"),
		strings.Contains(low, "struts-") && strings.Contains(low, ".tld"):
		return "Struts"
	case strings.Contains(low, "springframework.org/tags"):
		return "Spring"
	case strings.Contains(low, "java.sun.com/jsf"), strings.Contains(low, "xmlns.jcp.org/jsf"):
		return "JSF"
	}
	return tagOther
}
