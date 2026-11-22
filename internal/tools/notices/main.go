// Command notices writes THIRD_PARTY_NOTICES.md, the licences of everything
// quartermaster ships besides its own code:
//
//   - Go: the standard library and runtime compiled into qm, with the notices
//     of other authors that the standard library carries for the release
//     platforms (all of them, used by qm or not, so the file changes with Go
//     and not with each new import)
//   - every module in the build of ./cmd/qm on each release platform (`go
//     list -deps`, not the whole module graph); none today
//   - a note that the Swift helper qm-privcheck links only Apple's system
//     frameworks and libraries (scripts/package.sh checks it with otool)
//
// Run from the repository root, through scripts/notices.sh (make notices):
//
//	go run ./internal/tools/notices [-check] [-out <file>]
//
// With -check it writes nothing and fails when the file is out of date, so a
// new module cannot ship without an entry. It stops on a module whose licence
// has not been reviewed (the reviewed table below: its licence and the digest
// of the licence file), on a replaced module, and when Go's licence or its
// other notices differ from the pinned ones.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// goLicenseSHA256 pins Go's LICENSE as of the Go release go.mod names. The
// text has changed before (in 2024); a Go upgrade that changes it again
// stops the generator until the new text is reviewed and pinned here.
const goLicenseSHA256 = "911f8f5782931320f5b8d1160a76365b83aea6447ee6c04fa6d5591467db9dad"

// goNoticeSHA256 pins the notices of other authors that the standard library
// carries for the release platforms, one digest per distinct notice text, so
// a Go upgrade that changes them stops the generator instead of changing the
// file silently.
var goNoticeSHA256 = map[string]bool{
	"080c994962188f757c3b6bf6a500444078a16a65f993e6c8d9b14ac9a5769f4e": true,
	"1fad26590385d93b00c0d91989ee71cd1c91c74a126485f1b4f0bb44f7a32354": true,
	"54dc5d0ff22c24a5499108a1ff48377fefdc18efb2089f683241657eb5b7a039": true,
	"97321d2fb4054118aa5da108fa293a341490fe5ca11386c320acc85f7329c056": true,
	"9f7f8dba0edbdc891e676f3c4239bfd485bb71e8bb767f103a3244f853df1b03": true,
	"b010adcefa10bbced885f54291172d9ff42c566670826685e11f65cf0a0cfa8b": true,
	"b29230c2fd1b481ef5306da20b8c49a3c8211255d44db7007156321fb270463d": true,
	"e288ea1b559cffc33e788a2c8d7eddb78df101c298f06986986865d2090046cc": true,
}

// goNotNoticeSHA256 pins the comment blocks that the search below takes for a
// notice but that were read and are not one (a comment that mentions a
// copyright year in passing), so they are left out of the file.
var goNotNoticeSHA256 = map[string]bool{
	// math/big/internal/asmgen/asm.go: why the generated header says 2025.
	"f378d4a46558b884cc807c98928502686dfad0839efb28fc9e3b296918253e6d": true,
}

// platforms are the release builds; a module used on any of them is listed.
var platforms = [][2]string{{"darwin", "arm64"}, {"darwin", "amd64"}, {"linux", "arm64"}, {"linux", "amd64"}}

// review is the outcome of reading one module's licence by hand.
type review struct {
	SPDX       string // the licence, as an SPDX id
	LicenceSHA string // sha256 of the licence file that was read
}

// reviewed holds every module that may be compiled into qm, by module path.
// A new dependency stops the generator until its licence is read and entered
// here (the standard library comes first: see the plan's global constraints).
var reviewed = map[string]review{}

// entry is one component in the notices. Group orders the sections: Go,
// Go's other notices, Go modules.
type entry struct {
	Group                    int
	Name, Version, SPDX, URL string
	What                     string
	Text                     string
}

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("notices", flag.ContinueOnError)
	fl.SetOutput(stderr)
	root := fl.String("root", ".", "the repository root")
	out := fl.String("out", "", "the file to write or check (default <root>/THIRD_PARTY_NOTICES.md)")
	check := fl.Bool("check", false, "write nothing; fail when the file is out of date")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	if *out == "" {
		*out = filepath.Join(*root, "THIRD_PARTY_NOTICES.md")
	}
	failf := func(err error) int {
		_, _ = fmt.Fprintln(stderr, "notices:", err)
		return 1
	}
	text, err := generate(*root)
	if err != nil {
		return failf(err)
	}
	if *check {
		have, err := os.ReadFile(*out)
		if err != nil || string(have) != text {
			return failf(fmt.Errorf("%s is out of date: run make notices and commit it", *out))
		}
		_, _ = fmt.Fprintf(stdout, "notices: %s is up to date\n", *out)
		return 0
	}
	if err := os.WriteFile(*out, []byte(text), 0o644); err != nil {
		return failf(err)
	}
	_, _ = fmt.Fprintf(stdout, "notices: wrote %s\n", *out)
	return 0
}

func generate(root string) (string, error) {
	goEntries, err := goItself(root)
	if err != nil {
		return "", err
	}
	mods, err := buildModules(root)
	if err != nil {
		return "", err
	}
	modEntries, err := moduleEntries(mods, reviewed)
	if err != nil {
		return "", err
	}
	return render(append(goEntries, modEntries...))
}

func sha(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// --- Go ------------------------------------------------------------------------------

func goItself(root string) ([]entry, error) {
	out, err := exec.Command("go", "env", "GOROOT").Output()
	if err != nil {
		return nil, fmt.Errorf("go env GOROOT: %w", err)
	}
	goroot := strings.TrimSpace(string(out))
	text, err := goLicense(goroot, goLicenseSHA256)
	if err != nil {
		return nil, err
	}
	version, err := goVersion(filepath.Join(root, "go.mod"))
	if err != nil {
		return nil, err
	}
	es := []entry{{
		Group: 0, Name: "Go", Version: version, SPDX: "BSD-3-Clause", URL: "https://go.dev",
		What: "The Go standard library and runtime, compiled into qm.",
		Text: text,
	}}
	files, err := stdFiles(root)
	if err != nil {
		return nil, err
	}
	notices, err := stdNotices(filepath.Join(goroot, "src"), files, goNoticeSHA256, goNotNoticeSHA256)
	if err != nil {
		return nil, err
	}
	return append(es, notices...), nil
}

// goLicense finds Go's LICENSE: in GOROOT, or one folder up where Homebrew
// keeps it (GOROOT is <prefix>/libexec there). Only the file with the pinned
// digest is accepted.
func goLicense(goroot, pinned string) (string, error) {
	for _, p := range []string{filepath.Join(goroot, "LICENSE"), filepath.Join(filepath.Dir(goroot), "LICENSE")} {
		b, err := os.ReadFile(p)
		if err == nil && sha(b) == pinned {
			return string(b), nil
		}
	}
	return "", fmt.Errorf("no LICENSE with Go's pinned sha256 %s in %s or one folder up", pinned, goroot)
}

// goVersion is the go directive of go.mod: the language version the release
// toolchain must provide. It keeps the output the same on every machine.
func goVersion(gomod string) (string, error) {
	b, err := os.ReadFile(gomod)
	if err != nil {
		return "", err
	}
	m := regexp.MustCompile(`(?m)^go ([0-9.]+)$`).FindSubmatch(b)
	if m == nil {
		return "", errors.New("go.mod has no go directive")
	}
	return string(m[1]), nil
}

// module is a module as `go list` describes it.
type module struct {
	Path, Version, Dir string
	Main               bool
	Replace            *module
}

type pkgInfo struct {
	ImportPath      string
	Standard        bool
	GoFiles, SFiles []string
	Module          *module
}

// goList runs `go list -json` with the given arguments for every release
// platform, from root.
func goList(root string, args ...string) ([]pkgInfo, error) {
	var all []pkgInfo
	for _, p := range platforms {
		cmd := exec.Command("go", append([]string{"list", "-json=ImportPath,Standard,GoFiles,SFiles,Module"}, args...)...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GOOS="+p[0], "GOARCH="+p[1], "CGO_ENABLED=0", "GOFLAGS=-mod=readonly")
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("go list %s for %s/%s: %w", strings.Join(args, " "), p[0], p[1], err)
		}
		dec := json.NewDecoder(bytes.NewReader(out))
		for dec.More() {
			var pkg pkgInfo
			if err := dec.Decode(&pkg); err != nil {
				return nil, err
			}
			all = append(all, pkg)
		}
	}
	return all, nil
}

// stdFiles lists the files of every standard library package built for the
// release platforms, relative to GOROOT/src.
func stdFiles(root string) ([]string, error) {
	pkgs, err := goList(root, "std")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, p := range pkgs {
		for _, f := range append(append([]string(nil), p.GoFiles...), p.SFiles...) {
			seen[p.ImportPath+"/"+f] = true
		}
	}
	files := make([]string, 0, len(seen))
	for f := range seen {
		files = append(files, f)
	}
	sort.Strings(files)
	return files, nil
}

// reForeign is a copyright statement, with a year, (c) or ©.
var reForeign = regexp.MustCompile(`(?i)\bcopyright\b.*(\b(19|20)[0-9]{2}\b|\(c\)|©)`)

// stdNotices finds, in the given standard library files, the notices of other
// authors: in each comment block with a copyright statement that is not the
// Go Authors', the notice runs from that statement (with the title lines just
// above it) to the closing "====" rule or the end of the block. Identical
// notices are listed once with their files. Every distinct notice text must
// be pinned, as a notice or as not one, and every pinned notice found.
func stdNotices(src string, files []string, pinned, notNotice map[string]bool) ([]entry, error) {
	byText := map[string][]string{}
	for _, f := range files {
		b, err := os.ReadFile(filepath.Join(src, filepath.FromSlash(f)))
		if err != nil {
			return nil, err
		}
		for _, n := range noticesIn(string(b)) {
			if notNotice[sha([]byte(n))] {
				continue
			}
			byText[n] = append(byText[n], f)
		}
	}
	var es []entry
	var unpinned []string
	for text, where := range byText {
		if !pinned[sha([]byte(text))] {
			unpinned = append(unpinned, fmt.Sprintf("%s (in %s)", sha([]byte(text)), strings.Join(where, ", ")))
		}
		es = append(es, entry{Group: 1, Name: strings.Join(where, ", "), Text: text})
	}
	if len(unpinned) > 0 || len(byText) != len(pinned) {
		sort.Strings(unpinned)
		return nil, fmt.Errorf("the notices in Go's standard library differ from the pinned ones (%d found, %d pinned); review them and update goNoticeSHA256: %s",
			len(byText), len(pinned), strings.Join(unpinned, "; "))
	}
	return es, nil
}

// noticesIn returns the foreign notices in one source file.
func noticesIn(src string) []string {
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	comment := func(i int) (string, bool) {
		t := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(t, "//") {
			return "", false
		}
		t = strings.TrimPrefix(t, "//")
		if strings.HasPrefix(t, " ") || strings.HasPrefix(t, "\t") {
			t = t[1:]
		}
		return strings.TrimRight(t, " \t"), true
	}
	rule := func(s string) bool { return len(s) >= 10 && strings.Trim(s, "=") == "" }
	var out []string
	for i := 0; i < len(lines); i++ {
		c, ok := comment(i)
		if !ok || !reForeign.MatchString(c) || strings.Contains(c, "The Go Authors") {
			continue
		}
		start := i
		for start > 0 {
			prev, ok := comment(start - 1)
			if !ok || prev == "" || rule(prev) {
				break
			}
			start--
		}
		ruled := false
		if start > 0 {
			prev, ok := comment(start - 1)
			ruled = ok && rule(prev)
		}
		end := i
		for end+1 < len(lines) {
			next, ok := comment(end + 1)
			if !ok || (ruled && rule(next)) {
				break
			}
			end++
		}
		var b strings.Builder
		for j := start; j <= end; j++ {
			c, _ := comment(j)
			b.WriteString(c + "\n")
		}
		out = append(out, clean(b.String()))
		i = end
	}
	return out
}

// --- Go modules ----------------------------------------------------------------------

// buildModules lists the modules, other than quartermaster itself, in the
// build of ./cmd/qm on each release platform.
func buildModules(root string) ([]module, error) {
	pkgs, err := goList(root, "-deps", "./cmd/qm")
	if err != nil {
		return nil, err
	}
	seen := map[string]module{}
	for _, p := range pkgs {
		if m := p.Module; m != nil && !m.Main && !p.Standard {
			seen[m.Path] = *m
		}
	}
	mods := make([]module, 0, len(seen))
	for _, m := range seen {
		mods = append(mods, m)
	}
	sort.Slice(mods, func(i, j int) bool { return mods[i].Path < mods[j].Path })
	return mods, nil
}

// moduleEntries reads each module's licence file from the module cache and
// requires it to be the one reviewed for that module.
func moduleEntries(mods []module, table map[string]review) ([]entry, error) {
	var es []entry
	for _, m := range mods {
		if m.Replace != nil {
			return nil, fmt.Errorf("module %s is replaced: review its licence by hand", m.Path)
		}
		r, ok := table[m.Path]
		if !ok {
			return nil, fmt.Errorf("module %s %s is in the build but its licence has not been reviewed: read the licence in %s, then add it to reviewed in internal/tools/notices/main.go", m.Path, m.Version, m.Dir)
		}
		text, err := licenceFile(m.Dir)
		if err != nil {
			return nil, fmt.Errorf("module %s: %w", m.Path, err)
		}
		if got := sha([]byte(text)); got != r.LicenceSHA {
			return nil, fmt.Errorf("module %s %s: its licence file (sha256 %s) is not the reviewed one (%s): read it again and update reviewed", m.Path, m.Version, got, r.LicenceSHA)
		}
		es = append(es, entry{Group: 2, Name: m.Path, Version: m.Version, SPDX: r.SPDX, URL: moduleURL(m.Path),
			What: "A Go module compiled into qm.", Text: text})
	}
	return es, nil
}

// licenceFile reads the one licence file in dir.
func licenceFile(dir string) (string, error) {
	des, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var found []string
	for _, d := range des {
		switch strings.ToUpper(d.Name()) {
		case "LICENSE", "LICENSE.MD", "LICENSE.TXT", "LICENCE", "COPYING":
			if !d.IsDir() {
				found = append(found, d.Name())
			}
		}
	}
	sort.Strings(found)
	if len(found) != 1 {
		return "", fmt.Errorf("%s: want exactly one licence file, found %v", dir, found)
	}
	b, err := os.ReadFile(filepath.Join(dir, found[0]))
	return string(b), err
}

func moduleURL(path string) string {
	switch {
	case strings.HasPrefix(path, "github.com/"):
		return "https://" + path
	case strings.HasPrefix(path, "golang.org/x/"):
		return "https://go.googlesource.com/" + strings.TrimPrefix(path, "golang.org/x/")
	}
	return "https://pkg.go.dev/" + path
}

// --- output ----------------------------------------------------------------------------

// clean normalises a licence text: LF line ends, no trailing spaces, one
// final newline.
func clean(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	return strings.Trim(strings.Join(lines, "\n"), "\n") + "\n"
}

// render writes the notices in a fixed order: group, name, version, then the
// text itself. Two entries with one name and version are ambiguous: an error.
func render(es []entry) (string, error) {
	es = append([]entry(nil), es...)
	sort.Slice(es, func(i, j int) bool {
		a, b := es[i], es[j]
		if a.Group != b.Group {
			return a.Group < b.Group
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.Version != b.Version {
			return a.Version < b.Version
		}
		return a.Text < b.Text
	})
	modules := 0
	for i, e := range es {
		if e.Group == 2 {
			modules++
		}
		if i > 0 {
			a := es[i-1]
			if e.Group != 1 && a.Group == e.Group && a.Name == e.Name && a.Version == e.Version {
				return "", fmt.Errorf("two entries for %s %s", e.Name, e.Version)
			}
		}
	}
	var b strings.Builder
	b.WriteString("# Third-party notices\n\n")
	b.WriteString("quartermaster is released under the MIT License (see LICENSE). Its binaries\n")
	b.WriteString("also carry the components below, each under its own licence. This file is\n")
	b.WriteString("generated by `make notices` from the components' own licence files; do not\n")
	b.WriteString("edit it by hand.\n\n")
	b.WriteString("The Swift helper qm-privcheck, in the macOS archives, links only Apple's\n")
	b.WriteString("system frameworks and libraries, which are part of macOS and not shipped\n")
	b.WriteString("with it.\n")
	if modules == 0 {
		b.WriteString("\nNo Go module besides the standard library is compiled into qm.\n")
	}
	stdHeading := false
	for _, e := range es {
		if e.Group == 1 {
			// The other authors' notices in Go's standard library, one per
			// distinct text, titled by the files that carry it.
			if !stdHeading {
				b.WriteString("\n## Also in the Go standard library\n\n")
				b.WriteString("Notices of other authors that Go's licence does not cover, carried by the\n")
				b.WriteString("standard library for the release platforms, whether or not qm uses the\n")
				b.WriteString("package that holds them.\n")
				stdHeading = true
			}
			fmt.Fprintf(&b, "\n### %s\n\n````text\n%s````\n", e.Name, clean(e.Text))
			continue
		}
		fmt.Fprintf(&b, "\n## %s\n\n", strings.TrimSpace(e.Name+" "+e.Version))
		if e.What != "" {
			fmt.Fprintf(&b, "%s\n\n", e.What)
		}
		if e.SPDX != "" {
			fmt.Fprintf(&b, "- Licence: %s\n- Source: %s\n\n", e.SPDX, e.URL)
		}
		b.WriteString("````text\n")
		b.WriteString(clean(e.Text))
		b.WriteString("````\n")
	}
	return b.String(), nil
}
