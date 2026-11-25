package main

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/IZZY-Agency/quartermaster/internal/plain"
)

// printLine prints one line: nothing in it, of the names, folders and notes
// it shows, can start another (plain.Line).
func printLine(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintln(w, plain.Line(fmt.Sprintf(format, args...)))
}

// printErr prints err after what it is of, its lines after the first
// indented, so that a name or folder in it cannot start a line of its own
// (plain.Text).
func printErr(w io.Writer, of string, err error) {
	_, _ = fmt.Fprintf(w, "%s: %s\n", of, plain.Text(err.Error()))
}

// printJSON prints v as indented JSON.
func printJSON(w io.Writer, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\n", plain.JSON(b))
	return err
}

// printPath prints path for a program to open: exactly, or not at all.
func printPath(w io.Writer, path string) error {
	if !plain.Exact(path) {
		return fmt.Errorf("the path %q is not UTF-8 or holds a control character or a line break: qm cannot print it as it is", path)
	}
	_, err := fmt.Fprintln(w, path)
	return err
}
