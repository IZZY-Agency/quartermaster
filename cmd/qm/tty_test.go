package main

import (
	"os"
	"testing"
)

// /dev/null is a character device but no terminal: qm up --adopt </dev/null
// must read as scripted, not ask and abort on the end of input.
func TestIsTTYRejectsDevNullFilesAndPipes(t *testing.T) {
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = null.Close() }()
	if isTTY(null.Fd()) {
		t.Fatal("/dev/null read as a terminal")
	}
	f, err := os.Create(t.TempDir() + "/in")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if isTTY(f.Fd()) {
		t.Fatal("a file read as a terminal")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	defer func() { _ = w.Close() }()
	if isTTY(r.Fd()) {
		t.Fatal("a pipe read as a terminal")
	}
}
