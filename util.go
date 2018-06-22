package pgdesc

import (
	"fmt"
)

// Various constants.
const (
	NULL = ""
)

// Gettext is used to translate strings.
var Gettext = func(s string, v ...interface{}) string {
	return fmt.Sprintf(s, v...)
}

// GettextNoop is used to translate column titles in the returned queries.
var GettextNoop = func(s string) string {
	return s
}

// PgDesc handles executing and displaying schema descriptions for a postgres
// database.
type PgDesc struct {
	db       interface{}
	version  int
	sversion string
}

// NewPgDesc creates a new PgDesc for the supplied database and options.
func NewPgDesc(db interface{}, version int, opts ...Option) *PgDesc {
	d := &PgDesc{
		db:      db,
		version: version,
	}

	// apply opts
	for _, o := range opts {
		o(d)
	}

	return d
}

// Option is a postgres description option.
type Option func(*PgDesc)
