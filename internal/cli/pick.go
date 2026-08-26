package cli

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"

	"recall/internal/search"
)

// Pick lets the user choose one session from items by printing a numbered
// list and reading a choice from in (spec session-search, "Choosing a
// session requires no other application" - change resume-in-current-terminal
// removed the external fuzzy finder this used to prefer; the in-process
// browser is the supported interactive picker now, this is only the
// non-interactive fallback for `recall resume` with no session named).
// Returns the chosen item, or ok=false if the user entered nothing.
func Pick(items []search.Item, in io.Reader, out io.Writer) (search.Item, bool, error) {
	if len(items) == 0 {
		return search.Item{}, false, nil
	}

	opts := DetermineOptions(out)
	for i, it := range items {
		fmt.Fprintf(out, "%3d) %s\n", i+1, RenderRow(it, opts))
	}
	fmt.Fprint(out, "Enter a number (blank to cancel): ")

	scanner := bufio.NewScanner(in)
	if !scanner.Scan() {
		return search.Item{}, false, nil
	}
	text := strings.TrimSpace(scanner.Text())
	if text == "" {
		return search.Item{}, false, nil
	}
	n, err := strconv.Atoi(text)
	if err != nil || n < 1 || n > len(items) {
		return search.Item{}, false, fmt.Errorf("cli: %q is not a valid choice", text)
	}
	return items[n-1], true, nil
}
