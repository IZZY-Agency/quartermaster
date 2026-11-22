package plugins

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// repoFile reads a file by its path from the repository root.
func repoFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", path))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type pluginManifest struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
}

func TestMarketplaceListsCoordinatorOnly(t *testing.T) {
	var m struct {
		Name  string `json:"name"`
		Owner struct {
			Name string `json:"name"`
		} `json:"owner"`
		Plugins []struct {
			Name        string `json:"name"`
			Source      string `json:"source"`
			Description string `json:"description"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(repoFile(t, ".claude-plugin/marketplace.json"), &m); err != nil {
		t.Fatalf("marketplace.json: %v", err)
	}
	if m.Name != "quartermaster" || m.Owner.Name != "IZZY Agency" {
		t.Errorf("marketplace %q owned by %q, want quartermaster owned by IZZY Agency", m.Name, m.Owner.Name)
	}
	if len(m.Plugins) != 1 {
		t.Fatalf("%d plugins listed, want qm-coordinator only", len(m.Plugins))
	}
	p := m.Plugins[0]
	if p.Name != "qm-coordinator" || p.Source != "./plugins/qm-coordinator" || p.Description == "" {
		t.Errorf("plugin entry %+v, want qm-coordinator from ./plugins/qm-coordinator with a description", p)
	}
	var pm pluginManifest
	if err := json.Unmarshal(repoFile(t, filepath.Join(p.Source, ".claude-plugin", "plugin.json")), &pm); err != nil {
		t.Fatalf("%s plugin.json: %v", p.Source, err)
	}
	if pm.Name != p.Name {
		t.Errorf("the source folder holds plugin %q, want %q", pm.Name, p.Name)
	}
}

func TestPluginManifestsValid(t *testing.T) {
	for _, plugin := range pluginNames {
		var pm pluginManifest
		if err := json.Unmarshal(repoFile(t, filepath.Join("plugins", plugin, ".claude-plugin", "plugin.json")), &pm); err != nil {
			t.Fatalf("%s plugin.json: %v", plugin, err)
		}
		if pm.Name != plugin || pm.Version == "" || pm.Description == "" {
			t.Errorf("%s plugin.json: %+v, want its name, a version and a description", plugin, pm)
		}
		hs := loadHooks(t, plugin)
		if len(hs) == 0 {
			t.Errorf("%s: no hooks", plugin)
		}
		for _, h := range hs {
			hookName(t, h)
		}
	}
}
