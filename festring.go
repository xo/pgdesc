package pgdesc

import (
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// processSQLNamePattern is manually translated func from the postgres source.
//
// It handles procssing the [NAME] patterns for the various \d* funcs.
//
// See: postgres/src/fe_utils/string_utils.c
//
// processSQLNamePattern
//
// Scan a wildcard-pattern string and generate appropriate WHERE clauses
// to limit the set of objects returned.  The WHERE clauses are appended
// to the already-partially-constructed query in buf.  Returns whether
// any clause was added.
//
// conn: connection query will be sent to (consulted for escaping rules).
// buf: output parameter.
// pattern: user-specified pattern option, or NULL if none ("*" is implied).
// have_where: true if caller already emitted "WHERE" (clauses will be ANDed
// onto the existing WHERE clause).
// force_escape: always quote regexp special characters, even outside
// double quotes (else they are quoted only between double quotes).
// schemavar: name of query variable to match against a schema-name pattern.
// Can be NULL if no schema.
// namevar: name of query variable to match against an object-name pattern.
// altnamevar: NULL, or name of an alternative variable to match against name.
// visibilityrule: clause to use if we want to restrict to visible objects
// (for example, "pg_catalog.pg_table_is_visible(p.oid)").  Can be NULL.
//
// Formatting note: the text already present in buf should end with a newline.
// The appended text, if any, will end with one too.
func processSQLNamePattern(
	w io.Writer,
	pattern string,
	haveWhere, forceEscape bool,
	schemavar, namevar string,
	altnamevar, visibilityrule string,
) bool {
	var addedClause bool

	if pattern == "" {
		// Default: select all visible objects
		if visibilityrule != "" {
			// WHEREAND
			if haveWhere {
				fmt.Fprint(w, "  AND ")
			} else {
				fmt.Fprint(w, "WHERE ")
			}
			haveWhere, addedClause = true, true
			// END WHEREAND

			fmt.Fprintf(w, "%s\n", visibilityrule)
		}
		return addedClause
	}

	schemabuf, namebuf := new(bytes.Buffer), new(bytes.Buffer)

	fmt.Fprint(namebuf, "^(")

	parsePattern(schemabuf, namebuf, pattern, forceEscape)

	/*
	 * Now decide what we need to emit.  We may run under a hostile
	 * search_path, so qualify EVERY name.  Note there will be a leading "^("
	 * in the patterns in any case.
	 */
	if namebuf.Len() > 2 {
		/* We have a name pattern, so constrain the namevar(s) */
		fmt.Fprint(namebuf, ")$")
		/* Optimize away a "*" pattern */
		if namebuf.String() != "^(.*)$" {
			// WHEREAND
			if haveWhere {
				fmt.Fprint(w, "  AND ")
			} else {
				fmt.Fprint(w, "WHERE ")
			}
			haveWhere, addedClause = true, true
			// END WHEREAND

			if altnamevar != "" {
				fmt.Fprintf(w, "(%s OPERATOR(pg_catalog.~) ", namevar)
				fmt.Fprint(w, stringLiteral(namebuf.String()))
				fmt.Fprintf(w, "\n        OR %s OPERATOR(pg_catalog.~) ", altnamevar)
				fmt.Fprint(w, stringLiteral(namebuf.String()))
				fmt.Fprint(w, ")\n")
			} else {
				fmt.Fprintf(w, "%s OPERATOR(pg_catalog.~) ", namevar)
				fmt.Fprint(w, stringLiteral(namebuf.String()))
				fmt.Fprint(w, "\n")
			}
		}
	}

	if schemabuf.Len() > 2 {
		/* We have a schema pattern, so constrain the schemavar */
		fmt.Fprint(schemabuf, ")$")
		/* Optimize away a "*" pattern */
		if schemabuf.String() != "^(.*)$" && schemavar != "" {
			// WHEREAND
			if haveWhere {
				fmt.Fprint(w, "  AND ")
			} else {
				fmt.Fprint(w, "WHERE ")
			}
			haveWhere, addedClause = true, true
			// END WHEREAND

			fmt.Fprintf(w, "%s OPERATOR(pg_catalog.~) ", schemavar)
			fmt.Fprint(w, stringLiteral(schemabuf.String()))
			fmt.Fprint(w, "\n")
		}
	} else {
		/* No schema pattern given, so select only visible objects */
		if visibilityrule != "" {
			// WHEREAND
			if haveWhere {
				fmt.Fprint(w, "  AND ")
			} else {
				fmt.Fprint(w, "WHERE ")
			}
			haveWhere, addedClause = true, true
			// END WHEREAND

			fmt.Fprintf(w, "%s\n", visibilityrule)
		}
	}

	return addedClause
}

// parsePattern parses the passed pattern to namebuf, schemabuf.
//
// This is a manual rewrite of a section of processSQLNamePattern.
//
// See: postgres/src/fe_utils/string_utils.c
//
// Parse the pattern, converting quotes and lower-casing unquoted letters.
// Also, adjust shell-style wildcard characters into regexp notation.
//
// We surround the pattern with "^(...)$" to force it to match the whole
// string, as per SQL practice.  We have to have parens in case the string
// contains "|", else the "^" and "$" will be bound into the first and
// last alternatives which is not what we want.
//
// Note: the result of this pass is the actual regexp pattern(s) we want
// to execute.  Quoting/escaping into SQL literal format will be done
// below using appendStringLiteralConn().
func parsePattern(schemabuf, namebuf *bytes.Buffer, pattern string, forceEscape bool) {
	var inquotes bool

	var i int
	for ; i < len(pattern); i++ {
		var ch, next byte = pattern[i], 0
		if i < len(pattern)-1 {
			next = pattern[i+1]
		}
		switch {
		case ch == '"':
			if inquotes && next == '"' {
				/* emit one quote, stay in inquotes mode */
				fmt.Fprint(namebuf, '"')
				i++
			} else {
				inquotes = !inquotes
			}
			i++

		case !inquotes && unicode.IsUpper(rune(ch)):
			fmt.Fprint(namebuf, strings.ToLower(string(ch)))
			i++

		case !inquotes && ch == '*':
			fmt.Fprint(namebuf, ".*")
			i++

		case !inquotes && ch == '?':
			fmt.Fprint(namebuf, ".")
			i++

		case !inquotes && ch == '.':
			/* Found schema/name separator, move current pattern to schema */
			schemabuf.Reset()
			schemabuf.Write(namebuf.Bytes())
			namebuf.Reset()
			fmt.Fprint(namebuf, "^(")
			i++

		case ch == '$':
			/*
			 * Dollar is always quoted, whether inside quotes or not. The
			 * reason is that it's allowed in SQL identifiers, so there's a
			 * significant use-case for treating it literally, while because
			 * we anchor the pattern automatically there is no use-case for
			 * having it possess its regexp meaning.
			 */
			fmt.Fprint(namebuf, "\\$")
			i++

		default:
			/*
			 * Ordinary data character, transfer to pattern
			 *
			 * Inside double quotes, or at all times if force_escape is true,
			 * quote regexp special characters with a backslash to avoid
			 * regexp errors.  Outside quotes, however, let them pass through
			 * as-is; this lets knowledgeable users build regexp expressions
			 * that are more powerful than shell-style patterns.
			 */
			if inquotes || forceEscape && strings.ContainsRune("|*+?()[]{}.^$\\", rune(ch)) {
				fmt.Fprint(namebuf, "\\")
			}
			r, n := utf8.DecodeRuneInString(pattern[i:])
			fmt.Fprint(namebuf, string(r))
			i += n
		}
	}
}

// printACLColumn
//
// Helper function for consistently formatting ACL (privilege) columns.
// The proper targetlist entry is appended to buf.  Note lack of any
// whitespace or comma decoration.
//
// manually rewritten from describe.c
func (d *PgDesc) printACLColumn(w io.Writer, colname string) {
	if d.version >= 80100 {
		fmt.Fprintf(w,
			"pg_catalog.array_to_string(%s, E'\\n') AS \"%s\"",
			colname, GettextNoop("Access privileges"))

	} else {
		fmt.Fprintf(w,
			"pg_catalog.array_to_string(%s, '\\n') AS \"%s\"",
			colname, GettextNoop("Access privileges"))
	}
}

// stringLiteral returns a postgres escaped string literal for s.
func stringLiteral(s string) string {
	s = strconv.QuoteToASCII(s)
	return "E'" + strings.Replace(s[1:len(s)-1], "'", "''", -1) + "'"
}

// strchr is a pseudo implementation of strchr.
func strchr(s string, r rune) string {
	if v := string(r); strings.Contains(s, v) {
		return v
	}
	return NULL
}

// strlen is a pseudo implementation of strlen.
func strlen(s string) int {
	return len(s)
}

// strspn is a pseudo implementation of strspn.
func strspn(a, b string) int {
	var i int
	for ; i < len(a); i++ {
		if !strings.Contains(b, string(a[i])) {
			break
		}
	}
	return i
}
