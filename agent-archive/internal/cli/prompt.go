package cli

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// prompter reads line-oriented answers from a scripted or interactive
// stdin. Secrets are read the same way as any other field: never echoed
// back, never accepted as a command-line argument, never written anywhere
// except through Keychain-backed storage. On-screen keystroke masking is a
// deferred UX enhancement, not a gap in those handling guarantees.
type prompter struct {
	in  *bufio.Reader
	out io.Writer
}

func newPrompter(in io.Reader, out io.Writer) *prompter {
	return &prompter{in: bufio.NewReader(in), out: out}
}

func (p *prompter) line(label string) (string, error) {
	fmt.Fprint(p.out, label)
	text, err := p.in.ReadString('\n')
	if err != nil {
		if err == io.EOF && text != "" {
			// A final answer with no trailing newline is still a real one.
			return strings.TrimSpace(text), nil
		}
		// No more input at all: treated as an error, never as a silent
		// blank. Otherwise a truncated scripted input, or a real Ctrl-D,
		// would make every remaining prompt take its default silently —
		// including "Enable automatic capture?", which defaults to yes —
		// so setup could commit real changes the user never confirmed.
		return "", fmt.Errorf("no more input: %w", err)
	}
	return strings.TrimSpace(text), nil
}

// help prints guidance for the prompt that follows: the first line flush
// with the prompts, any continuation lines indented beneath it.
func (p *prompter) help(lines ...string) {
	for i, l := range lines {
		if i > 0 {
			l = "  " + l
		}
		fmt.Fprintln(p.out, l)
	}
}

// options prints an indented list of choices under a question.
func (p *prompter) options(lines ...string) {
	for _, l := range lines {
		fmt.Fprintln(p.out, "  "+l)
	}
}

// withDefault prompts once, returning def when the answer is blank. A blank
// default shows no bracketed value rather than a confusing "[]".
func (p *prompter) withDefault(label, def string) (string, error) {
	prompt := label + ": "
	if def != "" {
		prompt = fmt.Sprintf("%s [%s]: ", label, def)
	}
	answer, err := p.line(prompt)
	if err != nil {
		return "", err
	}
	if answer == "" {
		return def, nil
	}
	return answer, nil
}

func (p *prompter) yesNo(label string, def bool) (bool, error) {
	hint := "Y/n"
	if !def {
		hint = "y/N"
	}
	answer, err := p.line(fmt.Sprintf("%s [%s] ", label, hint))
	if err != nil {
		return false, err
	}
	switch strings.ToLower(answer) {
	case "":
		return def, nil
	case "y", "yes":
		return true, nil
	case "n", "no":
		return false, nil
	default:
		return false, fmt.Errorf("expected y or n, got %q", answer)
	}
}

func (p *prompter) intWithDefault(label string, def int) (int, error) {
	answer, err := p.withDefault(label, strconv.Itoa(def))
	if err != nil {
		return 0, err
	}
	value, err := strconv.Atoi(answer)
	if err != nil {
		return 0, fmt.Errorf("expected a number, got %q", answer)
	}
	return value, nil
}

// lines reads one answer per call until a blank line ends the list.
func (p *prompter) lines(label string) ([]string, error) {
	fmt.Fprintln(p.out, label)
	var out []string
	for {
		answer, err := p.line("> ")
		if err != nil {
			return nil, err
		}
		if answer == "" {
			return out, nil
		}
		out = append(out, answer)
	}
}
