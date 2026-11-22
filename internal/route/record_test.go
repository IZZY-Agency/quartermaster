package route

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRecordNewFieldsRoundTrip(t *testing.T) {
	in := Record{Kind: "worker", Name: "task-a", Cwd: "/work/repo", UpProject: "p1", Replaces: "s-old"}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"cwd":"/work/repo"`, `"up_project":"p1"`, `"replaces":"s-old"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("%s lacks %s", b, want)
		}
	}
	var out Record
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Cwd != in.Cwd || out.UpProject != in.UpProject || out.Replaces != in.Replaces {
		t.Errorf("round trip = %+v", out)
	}
	b, _ = json.Marshal(Record{Kind: "worker"})
	for _, key := range []string{"cwd", "up_project", "replaces"} {
		if strings.Contains(string(b), `"`+key+`"`) {
			t.Errorf("empty %s not omitted: %s", key, b)
		}
	}
}
