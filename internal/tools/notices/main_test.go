package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNoticesInFindsForeignNoticeAndSkipsGoAuthors(t *testing.T) {
	src := `// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package x

// The original C code came with this notice.
//
// ====================================================
// Copyright (C) 1993 by Example Corp. All rights reserved.
//
// Permission to use, copy, modify, and distribute this
// software is freely granted.
// ====================================================
func f() {}
`
	got := noticesIn(src)
	if len(got) != 1 {
		t.Fatalf("noticesIn found %d notices, want 1: %q", len(got), got)
	}
	want := "Copyright (C) 1993 by Example Corp. All rights reserved.\n\nPermission to use, copy, modify, and distribute this\nsoftware is freely granted.\n"
	if got[0] != want {
		t.Errorf("notice = %q, want %q", got[0], want)
	}
}

func TestStdNoticesRequiresEveryNoticePinned(t *testing.T) {
	src := t.TempDir()
	file := "pkg/a.go"
	if err := os.MkdirAll(filepath.Join(src, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "// Copyright 2001 by Example Corp.\n// Permission granted.\npackage pkg\n// Note: a year such as Copyright 2025 in passing.\nvar x int\n"
	if err := os.WriteFile(filepath.Join(src, file), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	found := noticesIn(body)
	if len(found) != 2 {
		t.Fatalf("setup: want 2 matches, got %q", found)
	}
	notice, passing := sha([]byte(found[0])), sha([]byte(found[1]))

	if _, err := stdNotices(src, []string{file}, map[string]bool{}, map[string]bool{}); err == nil || !strings.Contains(err.Error(), "differ from the pinned ones") {
		t.Errorf("unpinned notices: err = %v, want the pinned-notices error", err)
	}
	es, err := stdNotices(src, []string{file}, map[string]bool{notice: true}, map[string]bool{passing: true})
	if err != nil {
		t.Fatalf("pinned: %v", err)
	}
	if len(es) != 1 || es[0].Name != file {
		t.Errorf("entries = %+v, want one for %s", es, file)
	}
	if _, err := stdNotices(src, []string{file}, map[string]bool{notice: true, "0000": true}, map[string]bool{passing: true}); err == nil {
		t.Error("a pinned notice that is not found must stop the generator")
	}
}

// writeModule makes a module folder holding one licence file.
func writeModule(t *testing.T, licence string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "LICENSE"), []byte(licence), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestModuleEntriesNeedsAReviewedLicence(t *testing.T) {
	licence := "MIT License\n\nCopyright (c) 2026 Example Corp\n"
	dir := writeModule(t, licence)
	mod := module{Path: "example.com/dep", Version: "v1.0.0", Dir: dir}

	_, err := moduleEntries([]module{mod}, map[string]review{})
	if err == nil || !strings.Contains(err.Error(), "has not been reviewed") {
		t.Errorf("unreviewed module: err = %v", err)
	}
	_, err = moduleEntries([]module{mod}, map[string]review{"example.com/dep": {SPDX: "MIT", LicenceSHA: sha([]byte("other"))}})
	if err == nil || !strings.Contains(err.Error(), "is not the reviewed one") {
		t.Errorf("changed licence: err = %v", err)
	}
	replaced := mod
	replaced.Replace = &module{Path: "example.com/fork"}
	if _, err := moduleEntries([]module{replaced}, map[string]review{"example.com/dep": {SPDX: "MIT", LicenceSHA: sha([]byte(licence))}}); err == nil {
		t.Error("a replaced module must stop the generator")
	}
	es, err := moduleEntries([]module{mod}, map[string]review{"example.com/dep": {SPDX: "MIT", LicenceSHA: sha([]byte(licence))}})
	if err != nil {
		t.Fatalf("reviewed module: %v", err)
	}
	if len(es) != 1 || es[0].SPDX != "MIT" || es[0].Text != licence || es[0].URL != "https://pkg.go.dev/example.com/dep" {
		t.Errorf("entries = %+v", es)
	}
}

func TestRenderOrdersAndSaysWhenNoModule(t *testing.T) {
	es := []entry{
		{Group: 2, Name: "github.com/example/b", Version: "v1.0.0", SPDX: "MIT", URL: "https://github.com/example/b", Text: "b"},
		{Group: 0, Name: "Go", Version: "1.27", SPDX: "BSD-3-Clause", URL: "https://go.dev", Text: "go"},
		{Group: 1, Name: "math/x.go", Text: "foreign"},
	}
	out, err := render(es)
	if err != nil {
		t.Fatal(err)
	}
	goAt, stdAt, modAt := strings.Index(out, "## Go 1.27"), strings.Index(out, "## Also in the Go standard library"), strings.Index(out, "## github.com/example/b v1.0.0")
	if goAt < 0 || stdAt < goAt || modAt < stdAt {
		t.Errorf("sections out of order (%d, %d, %d):\n%s", goAt, stdAt, modAt, out)
	}
	if strings.Contains(out, "No Go module besides") {
		t.Error("the no-module line is printed although a module is listed")
	}
	out, err = render(es[1:])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "No Go module besides the standard library is compiled into qm.") {
		t.Errorf("no module: the file does not say so:\n%s", out)
	}
	if _, err := render([]entry{es[0], es[0]}); err == nil {
		t.Error("two entries for one module and version must be an error")
	}
}
