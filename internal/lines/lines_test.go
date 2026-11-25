package lines

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
)

func each(t *testing.T, text string, max int) []string {
	t.Helper()
	var got []string
	// A 16-byte buffer, so a line is read in several chunks.
	err := Each(bufio.NewReaderSize(strings.NewReader(text), 16), max, func(n int, line []byte) {
		got = append(got, fmt.Sprintf("%d:%s", n, line))
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestEachBoundsLines(t *testing.T) {
	fits, over := strings.Repeat("a", 40), strings.Repeat("b", 41)
	got := each(t, fits+"\n"+over+"\nc\n"+over, 40)
	if want := []string{"1:" + fits + "\n", "3:c\n"}; !slices.Equal(got, want) {
		t.Errorf("Each = %q, want %q", got, want)
	}
}

func TestEachLastLineWithoutNewline(t *testing.T) {
	if got, want := each(t, "a\n\nb", 40), []string{"1:a\n", "2:\n", "3:b"}; !slices.Equal(got, want) {
		t.Errorf("Each = %q, want %q", got, want)
	}
	if got := each(t, "", 40); got != nil {
		t.Errorf("Each of nothing = %q, want none", got)
	}
}

type broken struct{}

func (broken) Read([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestEachReadError(t *testing.T) {
	err := Each(bufio.NewReader(broken{}), 40, func(int, []byte) { t.Error("a line of a broken reader") })
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Errorf("err = %v, want the reader's", err)
	}
}
