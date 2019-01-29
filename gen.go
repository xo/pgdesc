// +build ignore

// Command gen handles automatically generating code (pgdesc.go) from the postgres source.
//
// It works by downloading (and caching) files in the source, extracting the
// appropriate code blocks and applying simple text replacement and regexp
// replacements, before formatting the code.
package main

import (
	"bytes"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/knq/snaker"
	"golang.org/x/tools/imports"
)

const (
	postgresSrc = "https://raw.githubusercontent.com/postgres/postgres/master/"

	pgcasthURL       = postgresSrc + "src/include/catalog/pg_cast.h"
	pgclasshURL      = postgresSrc + "src/include/catalog/pg_class.h"
	pgdefaultaclhURL = postgresSrc + "src/include/catalog/pg_default_acl.h"
	helpcURL         = postgresSrc + "src/bin/psql/help.c"
	describehURL     = postgresSrc + "src/bin/psql/describe.h"
	describecURL     = postgresSrc + "src/bin/psql/describe.c"
	//chURL        = postgresSrc + "src/include/c.h"
)

var (
	flagTTL   = flag.Duration("ttl", 24*time.Hour, "file cache time")
	flagCache = flag.String("cache", "", "cache path")
	flagOut   = flag.String("o", filepath.Join(os.Getenv("GOPATH"), "src/github.com/xo/pgdesc/pgdesc.go"), "out")
	flagDebug = flag.Bool("debug", false, "enable debugging")
)

func main() {
	flag.Parse()
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	var err error

	// set cache path
	if *flagCache == "" {
		cacheDir, err := os.UserCacheDir()
		if err != nil {
			return err
		}
		*flagCache = filepath.Join(cacheDir, "pgdesc")
	}

	consts := make(map[string][2]string)

	// load R* constants in pg_class.h
	buf, err := get(cache{
		url:  pgclasshURL,
		path: filepath.Join(*flagCache, "pg_class.h"),
		ttl:  *flagTTL,
	})
	if err != nil {
		return err
	}
	if err = loadConsts(buf, "R", consts); err != nil {
		return err
	}

	// load R* constants in pg_default_acl.h
	buf, err = get(cache{
		url:  pgdefaultaclhURL,
		path: filepath.Join(*flagCache, "pg_default_acl.h"),
		ttl:  *flagTTL,
	})
	if err != nil {
		return err
	}
	if err = loadConsts(buf, "D", consts); err != nil {
		return err
	}

	// load COERCION_* consts in pg_cast.h
	buf, err = get(cache{
		url:  pgcasthURL,
		path: filepath.Join(*flagCache, "pg_cast.h"),
		ttl:  *flagTTL,
	})
	if err != nil {
		return err
	}
	if err = loadCharConsts(buf, "COERCION", consts); err != nil {
		return err
	}

	// load \d* comments in describe.h
	buf, err = get(cache{
		url:  describehURL,
		path: filepath.Join(*flagCache, "describe.h"),
		ttl:  *flagTTL,
	})
	if err != nil {
		return err
	}
	comments, err := loadDescribeComments(buf)
	if err != nil {
		return err
	}

	// load \d* help text in help.c
	buf, err = get(cache{
		url:  helpcURL,
		path: filepath.Join(*flagCache, "help.c"),
		ttl:  *flagTTL,
	})
	if err != nil {
		return err
	}
	help, err := loadHelp(buf)
	if err != nil {
		return err
	}

	logf("consts: %d, comments: %d, help: %d", len(consts), len(comments), len(help))

	// convert describe.c
	buf, err = get(cache{
		url:  describecURL,
		path: filepath.Join(*flagCache, "describe.c"),
		ttl:  *flagTTL,
	})
	if err != nil {
		return err
	}
	err = convertDescribe(buf, consts, comments, help)
	if err != nil {
		return err
	}

	return nil
}

// loadConsts extracts the <typ>_* constants in buf.
func loadConsts(buf []byte, typ string, consts map[string][2]string) error {
	defineRE := regexp.MustCompile(`#define\s+(` + typ + `[A-Z_]+)\s+(.*)`)
	for _, m := range defineRE.FindAllStringSubmatch(string(buf), -1) {
		v, comment := m[2], ""
		if i := strings.IndexAny(v, " \t"); i != -1 {
			comment, v = strings.TrimSpace(v[i:]), v[:i]
		}
		v = strings.Replace(v, "'", "", -1)
		comment = strings.TrimSpace(strings.TrimPrefix(strings.TrimSuffix(comment, "*/"), "/*"))
		consts[m[1]] = [2]string{v, comment}
	}
	return nil
}

// loadCharConsts extracts the <typ>_* constants in buf.
func loadCharConsts(buf []byte, typ string, consts map[string][2]string) error {
	defineRE := regexp.MustCompile(`\s+(` + typ + `[A-Z_]+)\s+=\s+(.*)`)
	for _, m := range defineRE.FindAllStringSubmatch(string(buf), -1) {
		v, comment := strings.Replace(m[2], ",", "", -1), ""
		if i := strings.IndexAny(v, " \t"); i != -1 {
			comment, v = strings.TrimSpace(v[i:]), v[:i]
		}
		v = strings.Replace(v, "'", "", -1)
		comment = strings.TrimSpace(strings.TrimPrefix(strings.TrimSuffix(comment, "*/"), "/*"))
		consts[m[1]] = [2]string{v, comment}
	}
	return nil
}

// describeCommentRE is a regexp matching the declarations in describe.h.
var describeCommentRE = regexp.MustCompile(`/\*(.*)\*/\n([a-zA-Z \t]+)`)

// loadDescribeComments extracts the function definitions in describe.h.
func loadDescribeComments(buf []byte) (map[string]string, error) {
	comments := make(map[string]string)
	for _, m := range describeCommentRE.FindAllStringSubmatch(string(buf), -1) {
		k := m[2][strings.LastIndexAny(m[2], " \t")+1:]
		v := strings.TrimSpace(m[1])
		comments[k] = v
	}
	return comments, nil
}

// helpTextRE is a regexp matching help text in help.c.
var helpTextRE = regexp.MustCompile(`_\(([^)]+)\)`)

// loadHelp extracts the command help text in help.c.
func loadHelp(buf []byte) (map[string]string, error) {
	// trim buf to
	start := bytes.Index(buf, []byte("Informational\\n"))
	if start == -1 {
		return nil, errors.New("could not find start of \"Informational\" section in help text")
	}
	buf = buf[start:]
	end := bytes.Index(buf, []byte("\n\n"))
	if end == -1 {
		return nil, errors.New("could not find end of Informational section in help text")
	}
	buf = buf[:end]

	return nil, nil
}

// convertDescribe converts describe.c into a Go equivalent.
func convertDescribe(src []byte, consts map[string][2]string, funcs map[string]string, help map[string]string) error {
	var err error

	// setup file
	buf := new(bytes.Buffer)
	if err = addHeader(buf, consts); err != nil {
		return err
	}

	// add additional funcs
	funcs["describeOneTSConfig"] = "[none]"
	funcs["describeOneTSParser"] = "[none]"
	funcs["listOneExtensionContents"] = "[none]"
	funcs["listTSConfigsVerbose"] = "[none]"
	funcs["listTSParsersVerbose"] = "[none]"

	// add comments to additional funcs
	src = bytes.Replace(src,
		[]byte("static bool\ndescribeOneTSConfig"),
		[]byte("/*\n * show one description of text search config\n */\nstatic bool\ndescribeOneTSConfig"), -1)
	src = bytes.Replace(src,
		[]byte("static bool\ndescribeOneTSParser"),
		[]byte("/*\n * show one description of text search parser\n */\nstatic bool\ndescribeOneTSParser"), -1)
	src = bytes.Replace(src,
		[]byte("static bool\nlistOneExtensionContents"),
		[]byte("/*\n * show one extension contents\n */\nstatic bool\nlistOneExtensionContents"), -1)
	src = bytes.Replace(src,
		[]byte("static bool\nlistTSConfigsVerbose"),
		[]byte("/*\n * full description of configs\n */\nstatic bool\nlistTSConfigsVerbose"), -1)

	// generate funcs
	if err = generateFuncs(buf, src, funcs, help); err != nil {
		return err
	}

	// write to disk and bail
	if *flagDebug {
		return ioutil.WriteFile(*flagOut, buf.Bytes(), 0644)
	}

	// format via imports
	dst, err := imports.Process(*flagOut, buf.Bytes(), nil)
	if err != nil {
		return err
	}

	return ioutil.WriteFile(*flagOut, dst, 0644)
}

// addHeader adds the beginning of the Go file.
func addHeader(w io.Writer, consts map[string][2]string) error {
	keys := make([]string, len(consts))
	var i int
	for k := range consts {
		keys[i] = k
		i++
	}
	sort.Strings(keys)

	var str string
	for _, n := range keys {
		c := consts[n]
		var comment string
		if c[1] != "" {
			comment = " // " + c[1]
		}
		str += fmt.Sprintf("\n\t%s = '%s'%s", n, c[0], comment)
	}

	_, err := fmt.Fprintf(w, start, str)
	return err
}

// generateFuncs generates the func bodies for the converted funcs.
func generateFuncs(w io.Writer, src []byte, funcs, help map[string]string) error {
	var keys []string
	funcMap := make(map[string]string)
	var maxnamelen, maxnlen int
	for n := range funcs {
		// chop list, describe, All prefixes + List suffix
		// TS => TextSearch
		// Db => Database
		// force to Go identifier
		name := n
		if n == "describePublications" {
			name = "PublicationDetails"
		} else {
			for _, s := range []string{"list", "describe", "All"} {
				name = strings.TrimPrefix(name, s)
			}
			for _, s := range []string{"List"} {
				name = strings.TrimSuffix(name, s)
			}
		}
		name = strings.Replace(name, "TS", "TextSearch", -1)
		name = strings.Replace(name, "Db", "Database", -1)
		name = snaker.ForceCamelIdentifier(name)

		maxnamelen = max(maxnamelen, len(name))
		maxnlen = max(maxnlen, len(n))

		if _, ok := funcMap[name]; ok {
			panic(fmt.Sprintf("%s already present in func map", name))
		}

		funcMap[name] = n
		keys = append(keys, name)
	}
	sort.Strings(keys)

	// safety check
	if len(funcs) != len(keys) {
		panic("number of funcs must equal number of keys")
	}

	// generate
	var err error
	for _, name := range keys {
		n := funcMap[name]
		comment := funcs[n]

		logf("GENERATING: %s => %s [%s]", pad(name, maxnamelen), pad(n, maxnlen), comment)
		if err = genFunc(w, src, name, n, comment); err != nil {
			return err
		}
	}

	return nil
}

// lastBlockMap is a map of last block strings to search for, for specific funcs.
var lastBlockMap = map[string]string{
	"describeOneTSConfig":      "Dictionaries",
	"describeOneTSParser":      "Get token types",
	"describePublications":     `"ORDER BY 2;");`,
	"describeRoles":            `/* END */`,
	"listDefaultACLs":          "ORDER BY 1",
	"listOneExtensionContents": "oid)",
	"permissionsList":          "ORDER BY 1",
}

// genFunc extracts func with name orig from src and writes a Go equivalent to
// w.
func genFunc(w io.Writer, src []byte, name, orig, comment string) error {
	start := bytes.Index(src, []byte("\n"+orig+"("))
	if start == -1 {
		panic(fmt.Sprintf("cannot find start of block %s", orig))
	}

	returnTypeStart := bytes.LastIndex(src[:start], []byte("\n"))
	if returnTypeStart == -1 {
		panic(fmt.Sprintf("cannot find return type start for %s", orig))
	}

	returnType := strings.TrimSpace(string(src[returnTypeStart:start]))
	if returnType != "bool" && returnType != "static bool" {
		panic(fmt.Sprintf("return type for %s is not bool, has: %q", orig, returnType))
	}

	funcCommentStart := bytes.LastIndex(src[:returnTypeStart], []byte("\n\n"))
	if funcCommentStart == -1 {
		panic(fmt.Sprintf("cannot find comment start for %s", orig))
	}

	funcComment := strings.TrimSpace(string(src[funcCommentStart:returnTypeStart]))
	if !strings.HasPrefix(funcComment, "/*") || !strings.HasSuffix(funcComment, "*/") {
		panic(fmt.Sprintf("invalid comment block for %s", orig))
	}

	funcComment = strings.Replace(funcComment, "/*", "//", -1)
	funcComment = strings.Replace(funcComment, "*/", "", -1)
	funcComment = strings.Replace(funcComment, "\n * ", "\n// ", -1)
	funcComment = strings.Replace(funcComment, "\n *", "\n//", -1)
	funcComment = strings.TrimSpace(funcComment)

	src = src[start:]

	// build params
	paramStart := bytes.Index(src, []byte("("))
	if paramStart == -1 {
		panic(fmt.Sprintf("could not find start of parameter list for %s", orig))
	}
	paramEnd := bytes.Index(src, []byte(")"))
	if paramEnd == -1 {
		panic(fmt.Sprintf("could not find end of parameter list for %s", orig))
	}
	paramList := spaceRE.ReplaceAllString(string(src[paramStart+1:paramEnd]), " ")
	var params [][2]string
	for _, p := range strings.Split(paramList, ", ") {
		typ := p[:strings.LastIndex(p, " ")]
		if typ == "const char" {
			typ = "string"
		}
		n := strings.TrimPrefix(p[strings.LastIndex(p, " ")+1:], "*")
		params = append(params, [2]string{n, typ})
	}
	// logf("PARAMS: %v", params)

	end := bytes.Index(src, []byte("\n}\n"))
	if end == -1 {
		panic(fmt.Sprintf("could not find end to code block for %s", orig))
	}
	start = bytes.Index(src, []byte("{"))
	src = src[start+1 : end]

	// write start of func
	fmt.Fprintf(w, "// %s\n%sfunc (d *PgDesc) %s(w io.Writer",
		name+" handles "+strings.TrimSuffix(comment, ".")+".\n//\n// Generated from "+orig+" in psql's describe.c.",
		funcComment+"\n", name,
	)
	for _, p := range params {
		fmt.Fprintf(w, ", %s %s", p[0], p[1])
	}
	fmt.Fprint(w, ") error {\n")

	// comment out first block (variable declarations)
	declBlockEnd := bytes.Index(src, []byte("PQExpBufferData"))
	if declBlockEnd == -1 {
		panic(fmt.Sprintf("could not find PQExpBufferData for %s", orig))
	}
	declEndOffset := bytes.Index(src[declBlockEnd:], []byte("\n\n"))
	if declEndOffset == -1 {
		panic(fmt.Sprintf("could not find end of decl block for %s", orig))
	}
	declBlockEnd += declEndOffset
	w.Write(fixDeclBlock(src[:declBlockEnd], orig))
	src = src[declBlockEnd:]

	// plain text replacements
	src = bytes.Replace(src, []byte("gettext_noop"), []byte("GettextNoop"), -1)
	src = bytes.Replace(src, []byte("&buf"), []byte("w"), -1)
	src = bytes.Replace(src, []byte("appendPQExpBufferStr"), []byte("fmt.Fprint"), -1)
	src = bytes.Replace(src, []byte("printfPQExpBuffer"), []byte("fmt.Fprintf"), -1)
	src = bytes.Replace(src, []byte("appendPQExpBuffer"), []byte("fmt.Fprintf"), -1)
	src = bytes.Replace(src, []byte("psql_error"), []byte("return fmt.Errorf"), -1)
	src = bytes.Replace(src, []byte("pset.sversion"), []byte("d.version"), -1)
	src = bytes.Replace(src, []byte("initPQExpBuffer"), []byte("// initPQExpBuffer"), -1)
	src = bytes.Replace(src, []byte("myopt.default_footer = false"), []byte("// myopt.default_footer = false"), -1)

	// replace additional func calls
	src = bytes.Replace(src, []byte("describeOneTSConfig(oid"), []byte("d.OneTextSearchConfig(w, oid"), -1)
	src = bytes.Replace(src, []byte("describeOneTSParser(oid"), []byte("d.OneTextSearchParser(w, oid"), -1)
	src = bytes.Replace(src, []byte("listOneExtensionContents("), []byte("d.OneExtensionContents(w, "), -1)
	src = bytes.Replace(src, []byte("listTSConfigsVerbose(pattern)"), []byte("d.TextSearchConfigsVerbose(w, pattern)"), -1)
	src = bytes.Replace(src, []byte("listTSParsersVerbose(pattern)"), []byte("d.TextSearchParsersVerbose(w, pattern)"), -1)

	// fix pattern
	src = bytes.Replace(src, []byte("!pattern"), []byte(`pattern != NULL`), -1)
	src = bytes.Replace(src, []byte("pattern && pattern2"), []byte("pattern != NULL && pattern2 != NULL"), -1)
	src = bytes.Replace(src, []byte("(pattern)"), []byte("(pattern != NULL)"), -1)
	src = bytes.Replace(src, []byte("|| pattern"), []byte("|| pattern != NULL"), -1)
	src = bytes.Replace(src, []byte("pset.db, w, pattern"), []byte("w, pattern"), -1)

	src = bytes.Replace(src, []byte("printACLColumn"), []byte("d.printACLColumn"), -1)
	src = bytes.Replace(src, []byte("\tstatic const"), []byte("\t// static const"), -1)
	src = bytes.Replace(src,
		[]byte("showAggregate = showNormal = showTrigger = true"),
		[]byte("showAggregate, showNormal, showTrigger = true, true, true"), -1)
	src = bytes.Replace(src,
		[]byte("showTables = showViews = showMatViews = showSeq = showForeign = true"),
		[]byte("showTables, showViews, showMatViews, showSeq, showForeign = true, true, true, true, true"), -1)

	// general regexp replacements
	src = startParenRE.ReplaceAll(src, []byte(") {"))
	src = charSverbufRE.ReplaceAll(src, []byte("// char sverbuf"))
	src = formatPGVerRE.ReplaceAll(src, []byte("d.sversion"))
	src = needsOrRE.ReplaceAll(src, []byte("var needs_or bool"))

	// grouped replacements
	src = returnBoolRE.ReplaceAll(src, []byte("// return $1"))
	src = forRE.ReplaceAll(src, []byte("\tfor $1 := $2 {"))

	// cpp string replacements (done before the fix for "\n below)
	src = cppString1RE.ReplaceAll(src, []byte(`'"+string($1)+"'`))
	src = cppString2RE.ReplaceAll(src, []byte(`"'"+string($1)+"'"`))
	src = cppStringFixRE.ReplaceAll(src, []byte("$1"))

	// fix missing + on strings at a line end
	src = stringRE.ReplaceAll(src, []byte("\"+\n"))

	// fix closing parens and strings
	src = fixParensRE.ReplaceAll(src, []byte(`))`))
	src = fixStringEndRE.ReplaceAll(src, []byte(`")`))

	// listAllDbs is missing the standard blank line, so add one
	if orig == "listAllDbs" {
		src = bytes.Replace(src, []byte(`ORDER BY 1;");`), []byte("ORDER BY 1;\");\n\n"), -1)
	}

	// describeRoles has problems
	if orig == "describeRoles" {
		src = bytes.Replace(src, []byte(`ORDER BY 1;");`), []byte(`ORDER BY 1;");`+"\n\n/* END */\n\n"), 1)
		src = bytes.Replace(src, []byte("ncols++"), []byte("// ncols++"), -1)
	}

	// find last block marker, comment out everything afterwards
	lastBlockMarker := "fmt.Fprint"
	if v, ok := lastBlockMap[orig]; ok {
		lastBlockMarker = v
	}
	lastBlockEnd := bytes.LastIndex(src, []byte(lastBlockMarker))
	if lastBlockEnd == -1 {
		panic(fmt.Sprintf("cannot find last fmt.Fprint for %s", orig))
	}
	offset := bytes.Index(src[lastBlockEnd:], []byte("\n\n"))
	if offset == -1 {
		panic(fmt.Sprintf("cannot find empty line after last  fmt.Fprint for %s", orig))
	}

	// emit all lines to last block end
	w.Write(fixCondBlocks(src[:lastBlockEnd+offset], orig))

	// comment out remaining lines
	src = src[lastBlockEnd+offset:]
	src = bytes.Replace(src, []byte("\n\t"), []byte("\n\t// "), -1)
	src = doubleCommentRE.ReplaceAll(src, []byte("// "))
	w.Write(src)

	fmt.Fprint(w, "\nreturn nil\n}\n\n")

	return nil
}

// various code regexps
var (
	spaceRE         = regexp.MustCompile(`\s+`)
	startParenRE    = regexp.MustCompile(`\)\n\s*\{`)
	charSverbufRE   = regexp.MustCompile(`char\s+sverbuf`)
	formatPGVerRE   = regexp.MustCompile(`(?s)formatPGVersionNumber\(.*sizeof\([^\)]+\)\)`)
	stringRE        = regexp.MustCompile(`"\n`)
	doubleCommentRE = regexp.MustCompile(`//\s*//\s*`)
	returnBoolRE    = regexp.MustCompile(`return\s+(true|false)`)
	cppString1RE    = regexp.MustCompile(`" CppAsString2\(([^\)]+)\) "`)
	cppString2RE    = regexp.MustCompile(`CppAsString2\(([^\)]+)\)`)
	cppStringFixRE  = regexp.MustCompile(`([^\\])" "`)
	needsOrRE       = regexp.MustCompile(`bool\s+needs_or\s+=\s+false`)
	forRE           = regexp.MustCompile(`(?m)^\s+for\s+\(([a-z]+)\s+=\s+(.+?)\)\s+{$`)
	fixParensRE     = regexp.MustCompile(`(?sm)\)\s*$\s*\)`)
	fixStringEndRE  = regexp.MustCompile(`(?sm)"\s*\+\s*$\s*\)`)
)

// fixDeclBlock fixes the decl block.
func fixDeclBlock(src []byte, orig string) []byte {
	buf := new(bytes.Buffer)
	for _, line := range bytes.Split(src, []byte("\n")) {
		if m := boolVarRE.FindIndex(line); m != nil {
			buf.Write(boolVarRE.ReplaceAll(line, []byte("$1 var $2 bool")))
		} else if bytes.HasPrefix(line, []byte("\t")) && len(bytes.TrimSpace(line)) != 0 {
			buf.Write([]byte("\t// "))
			buf.Write(bytes.TrimPrefix(line, []byte("\t")))
		}
		buf.Write([]byte("\n"))
	}
	return buf.Bytes()
}

var (
	boolVarRE = regexp.MustCompile(`^(\s+)bool\s+([a-zA-Z_]+)`)
)

// fixCondBlocks fixes conditional blocks in src.
func fixCondBlocks(src []byte, orig string) []byte {
	// fix dangling
	src = ifFixRE.ReplaceAll(src, []byte("$1 {"))
	src = elseFixRE.ReplaceAll(src, []byte("$1 {"))
	src = fixElseLeftRE.ReplaceAll(src, []byte("} else"))

	buf := new(bytes.Buffer)
	for {
		m := ifStartRE.FindIndex(src)
		if m == nil {
			buf.Write(src)
			z := buf.Bytes()
			z = fixElseLeftRE.ReplaceAll(z, []byte("} else"))
			z = fixElseRightRE.ReplaceAll(z, []byte("else {"))
			return fixDanglingElse(z, orig)
		}

		// flush
		buf.Write(src[:m[1]])

		src = src[m[1]:]

		// find first terminating ;
		m = ifSemicolonRE.FindIndex(src)
		if m == nil {
			panic(fmt.Sprintf("could not find end to if statement in %s", orig))
		}

		// add { and }
		buf.Write([]byte(" {\n"))
		buf.Write(bytes.TrimRight(bytes.TrimLeft(src[:m[1]], "\n"), "\n"))
		buf.Write([]byte("\n}"))
		src = src[m[1]:]

		// check for else in src
		m = elseRE.FindIndex(src)
		if m == nil {
			continue
		}

		// flush
		buf.Write(src[:m[1]])
		src = src[m[1]:]

		// find first terminating ;
		m = ifSemicolonRE.FindIndex(src)
		if m == nil {
			panic(fmt.Sprintf("could not find end to else statement in %s", orig))
		}

		// add { and }
		buf.Write([]byte(" {\n"))
		buf.Write(bytes.TrimRight(bytes.TrimLeft(src[:m[1]], "\n"), "\n"))
		buf.Write([]byte("\n}"))
		src = src[m[1]:]
	}
}

// fixDanglingElse fixes the remaining "} else" case.
func fixDanglingElse(src []byte, orig string) []byte {
	m := danglingElseRE.FindIndex(src)
	if m == nil {
		return src
	}

	buf := new(bytes.Buffer)

	// flush
	buf.Write(src[:m[1]])
	src = src[m[1]:]

	// find first terminating ;
	m = ifSemicolonRE.FindIndex(src)
	if m == nil {
		panic(fmt.Sprintf("could not find end to dangling else statement in %s", orig))
	}

	// add { and }
	buf.Write([]byte(" {\n"))
	buf.Write(bytes.TrimRight(bytes.TrimLeft(src[:m[1]], "\n"), "\n"))
	buf.Write([]byte("\n}\n"))
	buf.Write(bytes.TrimRight(src[m[1]:], "\n"))

	return buf.Bytes()
}

var (
	ifFixRE        = regexp.MustCompile(`(?m)^(\s+(else\s+)?if\s+\(.+?\))\s*$\s*\{`)
	elseFixRE      = regexp.MustCompile(`(?m)^(\s+else)\s*$\s*\{`)
	ifStartRE      = regexp.MustCompile(`(?m)^\s+(else\s+)?if\s+\(.+?\)\s*$`)
	ifSemicolonRE  = regexp.MustCompile(`(?m);(\s*/\*[^*]+\*/\s*)?$`)
	elseRE         = regexp.MustCompile(`^\s*else\s*\n`)
	fixElseLeftRE  = regexp.MustCompile(`}\s+else`)
	fixElseRightRE = regexp.MustCompile(`else\s+{`)
	danglingElseRE = regexp.MustCompile(`(?m)^\s*}\s+else\s*$`)
)

// cache holds information about a cached file.
type cache struct {
	path   string
	ttl    time.Duration
	decode bool
	url    string
}

// get retrieves a file from disk or from the remote URL, optionally base64
// decoding it and writing it to disk.
func get(c cache) ([]byte, error) {
	var err error

	if err = os.MkdirAll(filepath.Dir(c.path), 0755); err != nil {
		return nil, err
	}

	// check if exists on disk
	fi, err := os.Stat(c.path)
	if err == nil && c.ttl != 0 && !time.Now().After(fi.ModTime().Add(c.ttl)) {
		return ioutil.ReadFile(c.path)
	}

	logf("RETRIEVING: %s", c.url)

	// retrieve
	cl := &http.Client{}
	req, err := http.NewRequest("GET", c.url, nil)
	if err != nil {
		return nil, err
	}
	res, err := cl.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	buf, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}

	// decode
	if c.decode {
		buf, err = base64.StdEncoding.DecodeString(string(buf))
		if err != nil {
			return nil, err
		}
	}

	logf("WRITING: %s", c.path)
	if err = ioutil.WriteFile(c.path, buf, 0644); err != nil {
		return nil, err
	}

	return buf, nil
}

// logf is a wrapper around log.Printf.
func logf(s string, v ...interface{}) {
	log.Printf(s, v...)
}

// pathJoin is a simple wrapper around filepath.Join to simplify inline syntax.
func pathJoin(n string, m ...string) string {
	return filepath.Join(append([]string{n}, m...)...)
}

// pad pads s to length n.
func pad(s string, n int) string {
	return s + strings.Repeat(" ", n-len(s))
}

// max returns the maximum of a, b.
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

const (
	// start is the start of the generated code.
	start = `// Package pgdesc builds SQL introspection queries for PostgreSQL databases.
//
// Mix of generated and manually rewritten code from PostgreSQL's psql codebase.
//
// Written for use by xo and usql.
package pgdesc

// Code generated by gen.go. DO NOT EDIT.

//go:generate go run gen.go

import (
	"fmt"
	"io"
)

// Postgres RELKIND and other related constants.
const (%s
)

`
)
