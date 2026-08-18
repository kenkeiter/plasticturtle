package ports

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func render(t *testing.T, rows []Row, global, jsonOut bool) string {
	t.Helper()
	var b strings.Builder
	if err := Render(&b, rows, global, jsonOut); err != nil {
		t.Fatalf("Render: %v", err)
	}
	return b.String()
}

func TestRenderTableProject(t *testing.T) {
	rows := []Row{
		{VMPort: 3000, HostPort: 3000, Status: StatusForwarding},
		{VMPort: 5432, HostPort: 15432, OriginalHostPort: 5432, Status: StatusForwarding},
		{VMPort: 6379, HostPort: 6379, Status: StatusStale},
	}
	want := strings.Join([]string{
		"VM PORT  HOST PORT  STATUS",
		"3000     3000       forwarding",
		"5432     15432      forwarding (remapped from 5432)",
		"6379     6379       stale",
		"",
	}, "\n")
	if got := render(t, rows, false, false); got != want {
		t.Errorf("table:\n%s\nwant:\n%s", got, want)
	}
}

func TestRenderTableGlobalGroupsAndFlagsConflicts(t *testing.T) {
	rows := []Row{
		{ProjectPath: "/w/alpha", VMPort: 3000, HostPort: 3000, Status: StatusForwarding, Conflict: "/w/beta"},
		{ProjectPath: "/w/alpha", VMPort: 5432, HostPort: 15432, OriginalHostPort: 5432, Status: StatusForwarding},
		{ProjectPath: "/w/beta", VMPort: 8080, HostPort: 3000, Status: StatusStale, Conflict: "/w/alpha"},
	}
	want := strings.Join([]string{
		"PROJECT   VM PORT  HOST PORT  STATUS",
		"/w/alpha  3000     3000       forwarding  conflict: also claimed by /w/beta",
		"          5432     15432      forwarding (remapped from 5432)",
		"/w/beta   8080     3000       stale  conflict: also claimed by /w/alpha",
		"",
	}, "\n")
	if got := render(t, rows, true, false); got != want {
		t.Errorf("table:\n%s\nwant:\n%s", got, want)
	}
}

func TestRenderTableEmpty(t *testing.T) {
	if got, want := render(t, nil, false, false), "No port forwards configured for this project.\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got, want := render(t, nil, true, false), "No running instances.\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRenderJSON(t *testing.T) {
	rows := []Row{
		{ProjectPath: "/w/alpha", VMPort: 5432, HostPort: 15432, OriginalHostPort: 5432, Status: StatusForwarding, Conflict: "/w/beta"},
	}
	want := `[
  {
    "projectPath": "/w/alpha",
    "vmPort": 5432,
    "hostPort": 15432,
    "originalHostPort": 5432,
    "status": "forwarding",
    "conflict": "/w/beta"
  }
]
`
	if got := render(t, rows, true, true); got != want {
		t.Errorf("json:\n%s\nwant:\n%s", got, want)
	}
}

func TestRenderJSONEmptyIsAnArray(t *testing.T) {
	// `pt ports --json | jq '.[]'` must not have to special-case null.
	if got, want := render(t, nil, false, true), "[]\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRenderJSONFillsMissingStatus(t *testing.T) {
	var out []map[string]any
	if err := json.Unmarshal([]byte(render(t, []Row{{VMPort: 3000, HostPort: 3000}}, false, true)), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out[0]["status"] != string(StatusInactive) {
		t.Errorf("status = %v, want %q for a zero Row", out[0]["status"], StatusInactive)
	}
}

// TestJSONRowCoversRow is the guard that keeps the documented --json interface
// from drifting away from Row. A field added to Row and forgotten here would
// otherwise vanish from the output with nothing to notice it.
func TestJSONRowCoversRow(t *testing.T) {
	rowT, jsonT := reflect.TypeOf(Row{}), reflect.TypeOf(jsonRow{})
	if rowT.NumField() != jsonT.NumField() {
		t.Fatalf("Row has %d fields, jsonRow has %d", rowT.NumField(), jsonT.NumField())
	}
	for i := range rowT.NumField() {
		f := rowT.Field(i)
		jf, ok := jsonT.FieldByName(f.Name)
		if !ok {
			t.Errorf("jsonRow is missing Row.%s", f.Name)
			continue
		}
		if jf.Type != f.Type {
			t.Errorf("jsonRow.%s is %s, want %s", f.Name, jf.Type, f.Type)
		}
		if jf.Tag.Get("json") == "" {
			t.Errorf("jsonRow.%s has no json tag", f.Name)
		}
	}
}
