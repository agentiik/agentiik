package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/agentiik/agentiik/language"
)

// agk language: "Prints what workflow.language answers, from the reference the binary carries, with
// no installation and no network: with no topic, the orientation, a minimal workflow and the list of
// topics; with a topic, that topic's page."
//
// It reads the language package and nothing else, through the functions workflow.language calls,
// so that the page a person reads at a terminal is the page a model is answered. A reader of the
// reference written here would be one more reading of the language, which is what the package
// exists to prevent.
//
// The page is written as it stands, on a terminal as to a pipe. It is Markdown, which reads as
// text, and drawing it would make it something workflow.language does not answer; nor does it need
// escaping, as what agk logs writes does, since the binary carries it rather than an installation
// or a container sending it. There is no -o: the answer is one page, which a redirection keeps as
// it is, and -o json would wrap it in an object that no route answers.
//
// The function is called languageVerb because language is the package it reads.
func languageVerb(_ context.Context, e Env, args []string) int {
	synopsis := "agk language [<topic>]\n\n\tWith no topic, the orientation; with one, its page. The topics are\n\t" + strings.Join(language.Names(), ", ") + "."
	fs := flags(e, "agk language", synopsis)
	// The verb takes no flag of its own, so -h says what it takes and lists no flags under a
	// heading with nothing beneath it.
	fs.Usage = func() { fmt.Fprintf(e.Err, "Usage:\n\t%s\n", synopsis) }
	topics, code, ok := positional(fs, args)
	if !ok {
		return code
	}

	var page string
	switch len(topics) {
	case 0:
		page = language.Orientation()
	case 1:
		if page, ok = language.Page(topics[0]); !ok {
			// The binary knows every topic there is, as it knows every value of a flag like agk
			// graph's --format, so a name that is none is the command line being wrong, exit 2,
			// where workflow.language answers invalid params, the protocol's word for the same.
			fmt.Fprintln(e.Err, language.Unknown(topics[0]))
			return exitUsage
		}
	default:
		fmt.Fprintf(e.Err, "agk language names one topic at most, as workflow.language does, and was given %d: %s\n", len(topics), strings.Join(topics, " "))
		return exitUsage
	}

	if _, err := io.WriteString(e.Out, page); err != nil {
		fmt.Fprintf(e.Err, "the page could not be written: %v\n", err)
		return exitNoOutcome
	}
	return exitSucceeded
}
