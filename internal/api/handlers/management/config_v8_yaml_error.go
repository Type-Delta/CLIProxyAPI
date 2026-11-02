package management

import (
	"regexp"
	"strconv"
)

var yamlErrorLine = regexp.MustCompile(`^yaml: line (\d+): (.*)$`)

// yamlParserProblems lists the libyaml parser (not scanner) problems in
// gopkg.in/yaml.v3 v3.0.1. yaml.v3 adds one only to scanner error marks, so
// these messages report a zero-based line.
var yamlParserProblems = map[string]bool{
	"did not find expected <stream-start>":   true,
	"did not find expected <document start>": true,
	"did not find expected '-' indicator":    true,
	"did not find expected key":              true,
	"did not find expected node content":     true,
	"did not find expected ',' or ']'":       true,
	"did not find expected ',' or '}'":       true,
	"found duplicate %TAG directive":         true,
	"found duplicate %YAML directive":        true,
	"found incompatible YAML document":       true,
	"found undefined tag handle":             true,
}

// yamlSyntaxErrorMessage renders a yaml.v3 syntax error with a one-based line
// in the submitted document, for management API error responses.
func yamlSyntaxErrorMessage(err error) string {
	msg := err.Error()
	match := yamlErrorLine.FindStringSubmatch(msg)
	if match == nil || !yamlParserProblems[match[2]] {
		return msg
	}
	line, errAtoi := strconv.Atoi(match[1])
	if errAtoi != nil {
		return msg
	}
	return "yaml: line " + strconv.Itoa(line+1) + ": " + match[2]
}
