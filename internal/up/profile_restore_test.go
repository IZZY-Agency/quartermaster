package up

import (
	"slices"
	"strings"
	"testing"

	"github.com/IZZY-Agency/quartermaster/internal/route"
	"github.com/IZZY-Agency/quartermaster/internal/spawn"
)

// A restored worker's profile takes its plugin folder and its MCP config
// from this qm and the config, never from the decision log alone.
func TestWorkerProfileRestore(t *testing.T) {
	plugin := t.TempDir()
	t.Setenv("QM_WORKER_PLUGIN", plugin)
	step := func(prof route.Profile) Step {
		return Step{Candidate: Candidate{Name: "w-a", Record: &route.Record{Decision: &route.Decision{Profile: &prof}}}}
	}
	p := &Project{ID: "proj-1", Workspace: "ws"}

	t.Run("only this qm's plugin folder", func(t *testing.T) {
		got, warn, err := workerProfile(p, step(route.Profile{PluginDirs: []string{"/old/qm-worker", "/elsewhere/plugin"}}), spawn.Install{}, nil, nil)
		if err != nil || warn != "" {
			t.Fatalf("err %v, warning %q", err, warn)
		}
		if !slices.Equal(got.PluginDirs, []string{plugin}) {
			t.Errorf("plugin folders = %v, want only %s", got.PluginDirs, plugin)
		}
	})
	t.Run("no plugin folder logged", func(t *testing.T) {
		got, _, err := workerProfile(p, step(route.Profile{}), spawn.Install{}, nil, nil)
		if err != nil || !slices.Equal(got.PluginDirs, []string{plugin}) {
			t.Errorf("plugin folders = %v, err %v", got.PluginDirs, err)
		}
	})
	t.Run("MCP config the config names is kept", func(t *testing.T) {
		got, warn, err := workerProfile(p, step(route.Profile{MCPConfig: "/cfg/a.mcp.json"}), spawn.Install{}, nil, []string{"/cfg/b.mcp.json", "/cfg/a.mcp.json"})
		if err != nil || warn != "" || got.MCPConfig != "/cfg/a.mcp.json" {
			t.Errorf("MCP config %q, warning %q, err %v", got.MCPConfig, warn, err)
		}
	})
	t.Run("MCP config the config does not name is left out", func(t *testing.T) {
		got, warn, err := workerProfile(p, step(route.Profile{MCPConfig: "/elsewhere/x.mcp.json"}), spawn.Install{}, nil, []string{"/cfg/a.mcp.json"})
		if err != nil || got.MCPConfig != "" {
			t.Fatalf("MCP config %q, err %v", got.MCPConfig, err)
		}
		if !strings.Contains(warn, "project_mcp") || strings.Contains(warn, "/elsewhere") {
			t.Errorf("warning = %q, want it to name project_mcp and not the logged path", warn)
		}
	})
	t.Run("no MCP config logged, no warning", func(t *testing.T) {
		got, warn, err := workerProfile(p, step(route.Profile{}), spawn.Install{}, nil, []string{"/cfg/a.mcp.json"})
		if err != nil || warn != "" || got.MCPConfig != "" {
			t.Errorf("MCP config %q, warning %q, err %v", got.MCPConfig, warn, err)
		}
	})
}
