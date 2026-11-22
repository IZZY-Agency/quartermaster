package setup

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/IZZY-Agency/quartermaster/internal/providers"
)

// qmStatusLine is the wrapped command's prefix: qm by the absolute path it
// was invoked as (invokedExecutable), so the status line does not depend on
// the PATH Claude Code's shell sees. Symlinks are kept as they are, on macOS
// and Linux alike, so a Homebrew upgrade or a make install over the binary
// keeps the path working. Plain qm when the path cannot be found.
func qmStatusLine() string {
	exe, err := executable()
	if err != nil {
		return "qm statusline"
	}
	return shellWord(exe) + " statusline"
}

// shellSafe is a word the shell reads as it is.
var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// shellWord quotes s as one shell word when it needs quoting.
func shellWord(s string) string {
	if shellSafe.MatchString(s) {
		return s
	}
	return shellQuote(s)
}

// WrapStatusLine wraps the status line of a Claude Code settings file in
// qm statusline: "<qm> statusline -- '<cmd>'", qm by its absolute path and
// the user's command passed as one shell-quoted argument. With no status line set it adds one running
// qm statusline alone. Only statusLine.command changes (or the statusLine
// member is added); every other byte stays as it was. A command that already
// runs qm statusline, a statusLine with no command string, and a file that is
// not a JSON object are left alone (changed false). diff shows the change.
func WrapStatusLine(settings []byte) (updated []byte, diff string, changed bool) {
	doc := settings
	if len(bytes.TrimSpace(doc)) == 0 {
		doc = []byte("{}\n")
	}
	top, err := topObject(doc)
	if err != nil {
		return settings, "", false
	}
	sl, ok := find(top.members, "statusLine")
	if !ok || string(doc[sl.valStart:sl.valEnd]) == "null" {
		value := "{\"type\": \"command\", \"command\": " + jsonString(qmStatusLine()) + "}"
		var out []byte
		if ok {
			out = splice(doc, sl.valStart, sl.valEnd, value)
		} else {
			out = addMember(doc, top, "statusLine", value)
		}
		return out, "+ statusLine: " + value + "\n", true
	}
	cmd, ok := commandMember(doc, sl)
	if !ok {
		return settings, "", false
	}
	var old string
	if json.Unmarshal(doc[cmd.valStart:cmd.valEnd], &old) != nil || providers.Wrapped(old) {
		return settings, "", false
	}
	wrapped := qmStatusLine() + " -- " + shellQuote(old)
	diff = "- statusLine.command: " + string(doc[cmd.valStart:cmd.valEnd]) + "\n" +
		"+ statusLine.command: " + jsonString(wrapped) + "\n"
	return splice(doc, cmd.valStart, cmd.valEnd, jsonString(wrapped)), diff, true
}

// statusLineCommand is statusLine.command of a settings file; ok is false
// when there is none.
func statusLineCommand(settings []byte) (cmd string, ok bool) {
	top, err := topObject(settings)
	if err != nil {
		return "", false
	}
	sl, found := find(top.members, "statusLine")
	if !found {
		return "", false
	}
	m, found := commandMember(settings, sl)
	if !found || json.Unmarshal(settings[m.valStart:m.valEnd], &cmd) != nil {
		return "", false
	}
	return cmd, true
}

// setStatusLine puts the status line back to cmd: its command when cmd is
// set, the whole statusLine member removed when cmd is nil. Every other byte
// stays.
func setStatusLine(settings []byte, cmd *string) ([]byte, error) {
	top, err := topObject(settings)
	if err != nil {
		return nil, err
	}
	sl, ok := find(top.members, "statusLine")
	if !ok {
		if cmd == nil {
			return settings, nil
		}
		return addMember(settings, top, "statusLine", "{\"type\": \"command\", \"command\": "+jsonString(*cmd)+"}"), nil
	}
	if cmd == nil {
		return removeMember(settings, top, sl), nil
	}
	m, ok := commandMember(settings, sl)
	if !ok {
		return nil, errors.New("statusLine has no command")
	}
	return splice(settings, m.valStart, m.valEnd, jsonString(*cmd)), nil
}

// commandMember finds "command" inside the statusLine member's object.
func commandMember(doc []byte, sl member) (member, bool) {
	if sl.valStart >= len(doc) || doc[sl.valStart] != '{' {
		return member{}, false
	}
	obj, err := objectMembers(doc, sl.valStart)
	if err != nil {
		return member{}, false
	}
	return find(obj.members, "command")
}

// member is one "key": value of a JSON object, by byte offsets into the
// document: start is the key's opening quote, valStart..valEnd the value.
type member struct {
	key                     string
	start, valStart, valEnd int
}

// object is a JSON object's members and the offsets of its braces.
type object struct {
	open, close int
	members     []member
}

// topObject reads a document that must be one JSON object.
func topObject(doc []byte) (object, error) {
	if !json.Valid(doc) {
		return object{}, errors.New("not valid JSON")
	}
	return objectMembers(doc, 0)
}

// objectMembers reads the object that starts at or after off (whitespace
// skipped) and returns its members' offsets.
func objectMembers(doc []byte, off int) (object, error) {
	dec := json.NewDecoder(bytes.NewReader(doc[off:]))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return object{}, errors.New("not a JSON object")
	}
	o := object{open: off + int(dec.InputOffset()) - 1}
	for dec.More() {
		start := skip(doc, off+int(dec.InputOffset()), ", \t\r\n")
		t, err := dec.Token()
		if err != nil {
			return object{}, err
		}
		key, _ := t.(string)
		valStart := skip(doc, off+int(dec.InputOffset()), ": \t\r\n")
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return object{}, err
		}
		o.members = append(o.members, member{key: key, start: start, valStart: valStart, valEnd: off + int(dec.InputOffset())})
	}
	if _, err := dec.Token(); err != nil {
		return object{}, err
	}
	o.close = off + int(dec.InputOffset()) - 1
	return o, nil
}

// skip moves i past any of chars.
func skip(doc []byte, i int, chars string) int {
	for i < len(doc) && strings.IndexByte(chars, doc[i]) >= 0 {
		i++
	}
	return i
}

func find(ms []member, key string) (member, bool) {
	for _, m := range ms {
		if m.key == key {
			return m, true
		}
	}
	return member{}, false
}

// splice replaces doc[from:to] with s, in a new slice.
func splice(doc []byte, from, to int, s string) []byte {
	out := make([]byte, 0, len(doc)-(to-from)+len(s))
	out = append(out, doc[:from]...)
	out = append(out, s...)
	return append(out, doc[to:]...)
}

// addMember appends "key": value as the object's last member, indented like
// its first member (two spaces when it has none).
func addMember(doc []byte, o object, key, value string) []byte {
	indent := "  "
	if len(o.members) > 0 {
		first := o.members[0].start
		ls := bytes.LastIndexByte(doc[:first], '\n') + 1
		if ws := doc[ls:first]; ls > 0 && len(bytes.TrimSpace(ws)) == 0 {
			indent = string(ws)
		}
		last := o.members[len(o.members)-1]
		return splice(doc, last.valEnd, last.valEnd, ",\n"+indent+jsonString(key)+": "+value)
	}
	return splice(doc, o.open+1, o.close, "\n"+indent+jsonString(key)+": "+value+"\n")
}

// removeMember cuts m out of the object, with the comma that joined it.
func removeMember(doc []byte, o object, m member) []byte {
	i := 0
	for o.members[i].start != m.start {
		i++
	}
	switch {
	case len(o.members) == 1:
		return splice(doc, o.open+1, o.close, "")
	case i > 0:
		return splice(doc, o.members[i-1].valEnd, m.valEnd, "")
	}
	return splice(doc, m.start, o.members[1].start, "")
}

// jsonString encodes s as a JSON string without HTML escaping, so a & stays
// a &.
func jsonString(s string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		panic(fmt.Sprintf("encode string: %v", err)) // a Go string always encodes
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// shellQuote quotes s as one POSIX shell word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// enabledPlugin reports whether the settings file turns plugin key on.
func enabledPlugin(settings []byte, key string) bool {
	var s struct {
		EnabledPlugins map[string]json.RawMessage `json:"enabledPlugins"`
	}
	if json.Unmarshal(settings, &s) != nil {
		return false
	}
	v := bytes.TrimSpace(s.EnabledPlugins[key])
	return string(v) == "true" || bytes.HasPrefix(v, []byte("["))
}
