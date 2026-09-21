package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// How the guided setup asks.
//
// Every question goes through here, and nothing here knows what it is asking
// about, which is what lets a test drive the whole conversation from a string
// and read back what the user would have seen.

// errNotATerminal is what a guided run refuses with when there is nobody to
// answer it. A prompt written into a pipe waits for a line that is never
// coming, and a service that starts by hanging is worse than one that says it
// cannot do this here.
var errNotATerminal = errors.New("-setup asks questions and this is not a terminal: " +
	"run it from a shell, or write the configuration yourself " +
	"(https://jmrp.io/docs/ghchronicle/configuration/builder/ writes one)")

// asker carries the conversation: where the answers come from, where the
// questions go, and whether a secret can be hidden while it is typed.
type asker struct {
	in  *bufio.Reader
	out io.Writer
	// hidden reads a line without echoing it. Nil means the answers are not
	// coming from a terminal and a secret is typed in the open, which a test
	// does and a person does not.
	hidden func() (string, error)
}

// newAsker reads answers from in and writes questions to out.
func newAsker(in io.Reader, out io.Writer) *asker {
	return &asker{in: bufio.NewReader(in), out: out}
}

// say writes a line of its own.
func (a *asker) sayf(format string, args ...any) {
	fmt.Fprintf(a.out, format+"\n", args...)
}

// errNoAnswer is what running out of answers looks like: somebody pressed
// ctrl-D, or a script piped in fewer lines than there are questions. "EOF" on
// its own tells a person nothing about which of those happened or what to do.
var errNoAnswer = errors.New("no answer, and the question was not optional: " +
	"nothing further was written")

// line reads one answer, and gives back the fallback for an empty one.
func (a *asker) line(fallback string) (string, error) {
	text, err := a.in.ReadString('\n')
	if err != nil && text == "" && errors.Is(err, io.EOF) {
		return "", errNoAnswer
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return fallback, nil
	}
	return text, nil
}

// text asks for an answer, showing the fallback when there is one.
func (a *asker) text(question, fallback string) (string, error) {
	if fallback != "" {
		fmt.Fprintf(a.out, "%s [%s]: ", question, fallback)
	} else {
		fmt.Fprintf(a.out, "%s: ", question)
	}
	return a.line(fallback)
}

// required asks until it gets something, because a question with no answer and
// no default is one the rest of the setup cannot go on without.
func (a *asker) required(question string) (string, error) {
	for {
		answer, err := a.text(question, "")
		if err != nil {
			return "", err
		}
		if answer != "" {
			return answer, nil
		}
		a.sayf("  that one is needed.")
	}
}

// secret asks for a credential, hidden while it is typed where the terminal
// allows that.
//
// What it never does is print the fallback. Every other question shows its
// default in the prompt, which is how a person knows what pressing enter will
// do; doing that here would put the token already in the environment on the
// screen, and from there into the scrollback of whatever window this was run
// in. So the offer is worded rather than shown.
func (a *asker) secret(question, fallback string) (string, error) {
	if fallback != "" {
		fmt.Fprintf(a.out, "%s [enter to keep the one already set]: ", question)
	} else {
		fmt.Fprintf(a.out, "%s: ", question)
	}
	read := a.line
	if a.hidden != nil {
		read = func(string) (string, error) {
			typed, err := a.hidden()
			fmt.Fprintln(a.out)
			return typed, err
		}
	}
	typed, err := read("")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(typed) == "" {
		return fallback, nil
	}
	return strings.TrimSpace(typed), nil
}

// yes asks a question with two answers, and takes the default on an empty one.
func (a *asker) yes(question string, fallback bool) (bool, error) {
	hint := "y/N"
	if fallback {
		hint = "Y/n"
	}
	for {
		fmt.Fprintf(a.out, "%s [%s]: ", question, hint)
		answer, err := a.line("")
		if err != nil {
			return false, err
		}
		switch strings.ToLower(answer) {
		case "":
			return fallback, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
		a.sayf("  answer y or n.")
	}
}

// choice is one option in a list, with the line a person reads beside it.
type choice struct {
	name string
	what string
}

// pick offers a numbered list and takes a number or the name itself, because
// somebody who knows the answer should not have to count.
//
// The first option is the default, which is why the order of the list is a
// decision rather than an arrangement: it is the answer somebody gets by
// pressing enter.
func (a *asker) pick(question string, options []choice) (string, error) {
	fallback := options[0].name
	a.sayf("%s", question)
	for i, option := range options {
		marker := " "
		if option.name == fallback {
			marker = "*"
		}
		a.sayf("  %s %d) %-14s %s", marker, i+1, option.name, option.what)
	}
	for {
		answer, err := a.text("  choose", fallback)
		if err != nil {
			return "", err
		}
		if n, numeric := strconv.Atoi(answer); numeric == nil && n >= 1 && n <= len(options) {
			return options[n-1].name, nil
		}
		for _, option := range options {
			if strings.EqualFold(answer, option.name) {
				return option.name, nil
			}
		}
		a.sayf("  not one of those.")
	}
}

// interactive says whether there is somebody on the other end.
//
// The question is whether this is a terminal, not whether it is a character
// device: /dev/null is one of those, and a guided setup reading from it
// answered its own first question with an end of file and carried on.
