package limits

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/buildinfo"
)

// maxReply bounds what is read of the app-server's output.
const maxReply = 1 << 20

// exitGrace is how long an app-server that has answered may take to exit once
// its stdin is closed, and a killed one to be reaped; termGrace is what is
// left of its group gets between SIGTERM and SIGKILL, for a git to remove its
// lock files. Tests change them.
var (
	exitGrace = 500 * time.Millisecond
	termGrace = 200 * time.Millisecond
)

// usageReply is the part of an account/rateLimits/read result qm reads. The
// plan, the credits, the banner and everything else in it are never decoded.
type usageReply struct {
	Allowed   *bool                   `json:"ordinaryUsageAllowed"`
	AccountID string                  `json:"accountId"`
	Limits    *serverBucket           `json:"rateLimits"`
	ByID      map[string]serverBucket `json:"rateLimitsByLimitId"`
}

type serverBucket struct {
	LimitID   string        `json:"limitId"`
	Primary   *serverWindow `json:"primary"`
	Secondary *serverWindow `json:"secondary"`
}

type serverWindow struct {
	UsedPercent   *float64 `json:"usedPercent"`
	WindowMinutes *int64   `json:"windowDurationMins"`
	ResetsAt      *int64   `json:"resetsAt"`
}

// windows is the codex bucket's windows; ok is false when the reply has no
// such bucket or it is not what qm knows. An older Codex has only the
// single-bucket view.
func (r usageReply) windows() (ws []limitWindow, ok bool) {
	b, found := r.ByID[codexBucket]
	if !found {
		if r.Limits == nil || (r.Limits.LimitID != "" && r.Limits.LimitID != codexBucket) {
			return nil, false
		}
		b = *r.Limits
	}
	for _, w := range []*serverWindow{b.Primary, b.Secondary} {
		if w == nil {
			continue
		}
		if w.UsedPercent == nil || *w.UsedPercent < 0 {
			return nil, false
		}
		ws = append(ws, newWindow(*w.UsedPercent, w.WindowMinutes, w.ResetsAt))
	}
	return ws, len(ws) > 0 || r.Allowed != nil
}

// rpcMessage is a line from the app-server. Result is set only on an answer.
type rpcMessage struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code int `json:"code"`
	} `json:"error"`
}

// JSON-RPC's "invalid request" and "method not found". Codex answers a login
// it cannot read limits with, and a method it does not know, with the first.
const (
	rpcInvalidRequest = -32600
	rpcMethodNotFound = -32601
)

// appServer is one running codex app-server.
type appServer struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	lines    chan []byte // closed at the end of its output
	tooLong  bool        // the output ended at maxReply; read after lines is closed
	quit     chan struct{}
	deadline time.Time
	signals  chan os.Signal
	got      os.Signal // the signal that ended the read, if one did
}

// askUsage asks the codex at path for its usage: a codex app-server over
// stdio, initialize, initialized, account/rateLimits/read and nothing else.
// It runs in the root folder, in a process group of its own that is ended with
// it, all of that within timeout. A SIGINT, SIGTERM or SIGHUP that would end
// qm ends it too, and then qm as it would have.
func askUsage(path string, timeout time.Duration) (usageReply, usageFailure) {
	s := &appServer{
		cmd:      exec.Command(path, "app-server"),
		lines:    make(chan []byte),
		quit:     make(chan struct{}),
		deadline: time.Now().Add(timeout - termGrace - exitGrace),
		signals:  make(chan os.Signal, 1),
	}
	s.cmd.Dir = "/"
	s.cmd.Env = codexEnv(os.Environ())
	s.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := s.cmd.StdoutPipe()
	if err != nil {
		return usageReply{}, failStart
	}
	if s.stdin, err = s.cmd.StdinPipe(); err != nil {
		return usageReply{}, failStart
	}
	for _, sig := range []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP} {
		if !signal.Ignored(sig) {
			signal.Notify(s.signals, sig)
		}
	}
	defer func() {
		signal.Stop(s.signals)
		select {
		case s.got = <-s.signals: // one that came after the answer
		default:
		}
		if sig, ok := s.got.(syscall.Signal); ok {
			_ = syscall.Kill(os.Getpid(), sig)
			time.Sleep(exitGrace) // the signal is on its way; do not go on
		}
	}()
	if err := s.cmd.Start(); err != nil {
		return usageReply{}, failStart
	}
	go s.read(stdout)
	defer s.end()
	return s.ask()
}

func (s *appServer) ask() (usageReply, usageFailure) {
	client := map[string]string{"name": "quartermaster", "version": buildinfo.Version}
	if fail := s.call(0, "initialize", map[string]any{"clientInfo": client}, nil); fail != "" {
		return usageReply{}, fail
	}
	if s.send(rpcRequest{Method: "initialized"}) != nil {
		return usageReply{}, failEnded
	}
	var r *usageReply
	if fail := s.call(1, "account/rateLimits/read", map[string]any{"excludeResetCreditDetails": true}, &r); fail != "" {
		return usageReply{}, fail
	}
	if r == nil {
		return usageReply{}, failShape
	}
	// Answered: let it exit by itself, as it does when its input ends.
	_ = s.stdin.Close()
	grace := time.NewTimer(min(exitGrace, time.Until(s.deadline)))
	defer grace.Stop()
	for {
		select {
		case _, ok := <-s.lines:
			if !ok {
				return *r, ""
			}
		case <-grace.C:
			return *r, ""
		case s.got = <-s.signals:
			return *r, ""
		}
	}
}

type rpcRequest struct {
	Method string `json:"method"`
	ID     *int   `json:"id,omitempty"`
	Params any    `json:"params,omitempty"`
}

func (s *appServer) send(r rpcRequest) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = s.stdin.Write(append(b, '\n'))
	return err
}

// call sends a request and decodes its answer into result (nil: not read).
// Other lines, the server's own notifications and requests, are skipped.
func (s *appServer) call(id int, method string, params, result any) usageFailure {
	if s.send(rpcRequest{Method: method, ID: &id, Params: params}) != nil {
		// It has closed its input; what it printed may still say more.
		_ = s.stdin.Close()
	}
	timer := time.NewTimer(time.Until(s.deadline))
	defer timer.Stop()
	for {
		select {
		case line, ok := <-s.lines:
			switch {
			case !ok && s.tooLong:
				return failShape
			case !ok:
				return failEnded
			}
			var m rpcMessage
			if json.Unmarshal(line, &m) != nil || m.Method != "" || string(m.ID) != strconv.Itoa(id) {
				continue
			}
			switch {
			case m.Error != nil && (m.Error.Code == rpcInvalidRequest || m.Error.Code == rpcMethodNotFound):
				return failRefused
			case m.Error != nil:
				return failFetch
			case result != nil && json.Unmarshal(m.Result, result) != nil:
				return failShape
			}
			return ""
		case <-timer.C:
			return failTimeout
		case s.got = <-s.signals:
			return failInterrupted
		}
	}
}

// read sends the app-server's lines until its output, or the part of it qm
// reads, ends.
func (s *appServer) read(stdout io.Reader) {
	defer close(s.lines)
	lr := &io.LimitedReader{R: stdout, N: maxReply}
	sc := bufio.NewScanner(lr)
	sc.Buffer(nil, maxReply)
	for sc.Scan() {
		select {
		case s.lines <- slices.Clone(sc.Bytes()):
		case <-s.quit:
			return
		}
	}
	s.tooLong = lr.N <= 0 || errors.Is(sc.Err(), bufio.ErrTooLong)
}

// end ends what is left of the app-server's process group, after termGrace
// with SIGKILL. The group is signalled before its leader is reaped, so its id
// cannot be another's by then; that also leaves no way to see that it is
// empty, so the grace is always waited out.
func (s *appServer) end() {
	close(s.quit)
	_ = s.stdin.Close()
	_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGTERM)
	time.Sleep(termGrace)
	_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
	done := make(chan struct{})
	go func() { _ = s.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(exitGrace):
	}
}

// repoEnv are the variables that tie git to one repository or command (git
// rev-parse --local-env-vars); git sets them for its hooks. The app-server's
// own git must not work in that one.
var repoEnv = []string{"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_CONFIG", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT",
	"GIT_OBJECT_DIRECTORY", "GIT_DIR", "GIT_WORK_TREE", "GIT_IMPLICIT_WORK_TREE", "GIT_GRAFT_FILE", "GIT_INDEX_FILE",
	"GIT_NO_REPLACE_OBJECTS", "GIT_REPLACE_REF_BASE", "GIT_PREFIX", "GIT_SHALLOW_FILE", "GIT_COMMON_DIR",
	"GIT_INTERNAL_SUPER_PREFIX", "GIT_NAMESPACE"}

// codexEnv is env for a codex that runs in the root folder: without repoEnv
// and the folders qm was run from, and with a relative CODEX_HOME made
// absolute.
func codexEnv(env []string) []string {
	out := []string{"PWD=/"}
	for _, kv := range env {
		name, value, _ := strings.Cut(kv, "=")
		switch {
		case name == "PWD", name == "OLDPWD", slices.Contains(repoEnv, name),
			strings.HasPrefix(name, "GIT_CONFIG_KEY_"), strings.HasPrefix(name, "GIT_CONFIG_VALUE_"):
			continue
		case name == "CODEX_HOME" && value != "":
			if abs, err := filepath.Abs(value); err == nil {
				kv = name + "=" + abs
			}
		}
		out = append(out, kv)
	}
	return out
}
