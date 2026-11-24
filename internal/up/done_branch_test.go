package up

import (
	"strings"
	"testing"
)

func TestFinishedLeavesUnsafeBranchOutOfCommands(t *testing.T) {
	for _, branch := range []string{"-x", "--output=/tmp/x", "a..b", "a b"} {
		c, _ := worker(t)
		c.Branch = branch
		f := &fakeDir{out: originMain()}
		if got := Finished(Project{}, f.run)(c); got != "" {
			t.Errorf("branch %q: Finished = %q, want \"\"", branch, got)
		}
		for _, call := range f.calls {
			if strings.Contains(call, branch) {
				t.Errorf("branch %q reached a command: %q", branch, call)
			}
		}
	}
}

func TestFinishedNeedsAnAbsoluteProjectFolder(t *testing.T) {
	for _, project := range []string{"work/shop", "-C/tmp", "/no/such/folder/for/qm"} {
		c, _ := worker(t)
		c.Record.Project = project
		f := &fakeDir{out: originMain()}
		f.out[ghList] = prs("MERGED", now.Add(1))
		_ = Finished(Project{}, f.run)(c)
		for _, call := range f.calls {
			if strings.HasPrefix(call, project+": ") {
				t.Errorf("project %q used as a command's folder: %q", project, call)
			}
		}
	}
}

func TestFinishedNeedsAnAbsoluteWorkerFolder(t *testing.T) {
	c, _ := worker(t)
	c.Cwd = "shop"
	f := &fakeDir{out: originMain()}
	if got := Finished(Project{}, f.run)(c); got != "" || len(f.calls) != 0 {
		t.Errorf("Finished = %q with calls %q, want \"\" and none", got, f.calls)
	}
}

func TestWriteUpFileNeedsFileNames(t *testing.T) {
	setState(t)
	for _, c := range [][2]string{{"shop-0a1b2c3d", "../x-"}, {"shop-0a1b2c3d", `a\b-`}, {"shop-0a1b2c3d", "a/b-"}, {"shop-0a1b2c3d", ".."}, {`a\b`, "w1-"}} {
		if path, err := writeUpFile(c[0], c[1], "text"); err == nil {
			t.Errorf("writeUpFile(%q, %q) = %q, want an error", c[0], c[1], path)
		}
	}
	if _, err := writeUpFile("shop-0a1b2c3d", "w1-", "text"); err != nil {
		t.Errorf("writeUpFile with a worker name: %v", err)
	}
}

func TestGitLineKeepsBranchCharacters(t *testing.T) {
	nbsp := "feature-x "
	if got := gitLine([]byte(nbsp + "\n")); got != nbsp {
		t.Errorf("gitLine = %q, want %q", got, nbsp)
	}
	if got := gitLine([]byte("main\r\n")); got != "main" {
		t.Errorf("gitLine = %q, want main", got)
	}
}
