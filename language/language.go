// Package language is the workflow language as a client is taught it: the reference agentiik/schemas
// generates from its schemas, and the schema parts a client may ask for by name.
//
// "The language documentation is generated from the schema, never written beside it." The pages
// under reference/ are what tools/language.py wrote in agentiik/schemas, every sentence a
// description of the schema and every example one of its examples, so workflow.language, the
// command line and the site teach the language the validator enforces rather than three readings of
// it. This package holds them as they were written and reads nothing into them: a page is served as
// it stands, and topics.json says which topic answers for which place in agentiik.yaml.
//
// It is vendored rather than fetched, as internal/fixtures is, so that serving it reaches no network
// and a change upstream is a commit here that a person reads; and it is embedded, since the API
// serves it from a binary that carries nothing beside it. Standard library only.
package language

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"slices"
	"strings"
)

// Source is the commit of agentiik/schemas the files under reference/ and schemas/ were taken
// from, since the release they will be taken from at v0.7.0 is not tagged yet: the one sentence a
// person updating them reads to know what they are, and the test holds the files to it no further
// than that. At the release they are taken from the tag, as the fixtures are.
const Source = "agentiik/schemas@a4dae4acd2d9ecf776c44e133e3385d125790ef6"

//go:embed reference schemas
var files embed.FS

// Topic is one section of the language, as topics.json names it.
type Topic struct {
	// Name is what a client asks for: repository, inputs and so on to mcp.
	Name string `json:"name"`
	// Page is the file under reference/ that holds it.
	Page string `json:"page"`
	// Summary is the first sentence of the page.
	Summary string `json:"summary"`
	// Keywords are the places in workflow.schema.json the topic teaches, as JSON Pointers.
	Keywords []string `json:"keywords"`
	// Paths are where those keywords are written in agentiik.yaml or an included file, as JSON
	// Pointer patterns: * stands for any one name or index, .* for any one name starting with a
	// dot, a hidden block's.
	Paths []string `json:"paths"`
}

// Part is one schema document a client may ask for by name.
type Part struct {
	Name    string `json:"name"`
	File    string `json:"file"`
	ID      string `json:"$id"`
	Title   string `json:"title"`
	Summary string `json:"summary"`
}

// index is topics.json.
type index struct {
	Orientation string  `json:"orientation"`
	Topics      []Topic `json:"topics"`
	Parts       []Part  `json:"parts"`
}

var read index

func init() {
	b, err := files.ReadFile("reference/topics.json")
	if err != nil {
		panic("language: the reference carries no topics.json: " + err.Error())
	}
	if err := json.Unmarshal(b, &read); err != nil {
		panic("language: topics.json is not what agentiik/schemas writes: " + err.Error())
	}
}

// Orientation is what a client reads with no topic: what the language is, one complete minimal
// workflow, the topics and the schema parts.
func Orientation() string {
	b, err := files.ReadFile("reference/" + read.Orientation)
	if err != nil {
		panic("language: the orientation topics.json names is not carried: " + err.Error())
	}
	return string(b)
}

// Topics are the sections of the language, in the documentation's order.
func Topics() []Topic { return slices.Clone(read.Topics) }

// Names are the topics' names, in the same order.
func Names() []string {
	names := make([]string, len(read.Topics))
	for i, t := range read.Topics {
		names[i] = t.Name
	}
	return names
}

// Page is the page of one topic, and false for a name that is none.
func Page(topic string) (string, bool) {
	for _, t := range read.Topics {
		if t.Name == topic {
			b, err := files.ReadFile("reference/" + t.Page)
			if err != nil {
				panic(fmt.Sprintf("language: the page of %s is not carried: %v", topic, err))
			}
			return string(b), true
		}
	}
	return "", false
}

// Parts are the schema parts, in the order topics.json lists them.
func Parts() []Part { return slices.Clone(read.Parts) }

// Schema is the JSON Schema 2020-12 document of one part, as agentiik/schemas publishes it, and
// false for a name that is none.
func Schema(part string) ([]byte, bool) {
	for _, p := range read.Parts {
		if p.Name == part {
			b, err := fs.ReadFile(files, "schemas/"+p.File)
			if err != nil {
				panic(fmt.Sprintf("language: the part %s is not carried: %v", part, err))
			}
			return b, true
		}
	}
	return nil, false
}

// TopicOf is the topic that explains a place in agentiik.yaml, given as the JSON Pointer a
// validation error names: the topic of the longest path pattern matching the pointer or the start
// of it, token by token. Every pointer has one, since the document itself is the repository topic's.
func TopicOf(pointer string) string {
	tokens := split(pointer)
	best, length := "", -1
	for _, t := range read.Topics {
		for _, p := range t.Paths {
			pattern := split(p)
			if len(pattern) > len(tokens) || len(pattern) <= length {
				continue
			}
			if matches(pattern, tokens[:len(pattern)]) {
				best, length = t.Name, len(pattern)
			}
		}
	}
	return best
}

// split reads a JSON Pointer into its tokens, unescaped as RFC 6901 says: ~1 is a slash and ~0 a
// tilde, in that order. The empty pointer is the document, no token.
func split(pointer string) []string {
	if pointer == "" {
		return nil
	}
	tokens := strings.Split(strings.TrimPrefix(pointer, "/"), "/")
	for i, t := range tokens {
		tokens[i] = strings.ReplaceAll(strings.ReplaceAll(t, "~1", "/"), "~0", "~")
	}
	return tokens
}

// matches says whether pattern tokens match as many tokens of a pointer.
func matches(pattern, tokens []string) bool {
	for i, p := range pattern {
		switch {
		case p == "*":
		case p == ".*":
			if !strings.HasPrefix(tokens[i], ".") {
				return false
			}
		case p != tokens[i]:
			return false
		}
	}
	return true
}
