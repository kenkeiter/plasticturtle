package ports

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"
)

// jsonRow is the wire form of Row.
//
// It exists so that the JSON emitted by `plasticturtle ports --json` is a stable,
// documented interface with lower-camel field names, rather than whatever Go
// field names happen to be called this week. Every exported field of Row must
// appear here; a test enforces that by reflection, because a new field that
// silently fails to reach --json is exactly the kind of drift nobody notices.
//
// No omitempty: a consumer piping this into jq should be able to rely on every
// key existing on every object.
type jsonRow struct {
	ProjectPath      string `json:"projectPath"`
	VMPort           int    `json:"vmPort"`
	HostPort         int    `json:"hostPort"`
	OriginalHostPort int    `json:"originalHostPort"`
	Status           Status `json:"status"`
	Conflict         string `json:"conflict"`
}

// Render writes rows as an aligned table, or as JSON when jsonOut is set.
func Render(w io.Writer, rows []Row, global, jsonOut bool) error {
	if jsonOut {
		return renderJSON(w, rows)
	}
	return renderTable(w, rows, global)
}

// renderJSON always emits an array, never null: a script doing
// `plasticturtle ports --json | jq '.[]'` must not have to special-case the empty case.
func renderJSON(w io.Writer, rows []Row) error {
	out := make([]jsonRow, 0, len(rows))
	for _, r := range rows {
		status := r.Status
		if status == "" {
			status = StatusInactive
		}
		out = append(out, jsonRow{
			ProjectPath:      r.ProjectPath,
			VMPort:           r.VMPort,
			HostPort:         r.HostPort,
			OriginalHostPort: r.OriginalHostPort,
			Status:           status,
			Conflict:         r.Conflict,
		})
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("ports: encode json: %w", err)
	}
	return nil
}

// renderTable prints VM PORT / HOST PORT / STATUS, prefixed by a PROJECT
// column in global mode.
//
// Grouping is done by blanking the repeated project path rather than by
// emitting per-project sub-tables: one tabwriter block keeps every column
// aligned down the whole report, which is what makes a duplicated host port
// visible by eye.
func renderTable(w io.Writer, rows []Row, global bool) error {
	if len(rows) == 0 {
		var err error
		if global {
			_, err = fmt.Fprintln(w, "No running instances.")
		} else {
			_, err = fmt.Fprintln(w, "No port forwards configured for this project.")
		}
		return err
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if global {
		fmt.Fprintln(tw, "PROJECT\tVM PORT\tHOST PORT\tSTATUS")
	} else {
		fmt.Fprintln(tw, "VM PORT\tHOST PORT\tSTATUS")
	}

	prevProject := ""
	first := true
	for _, r := range rows {
		if !global {
			fmt.Fprintf(tw, "%d\t%d\t%s\n", r.VMPort, r.HostPort, statusText(r))
			continue
		}
		project := r.ProjectPath
		if !first && project == prevProject {
			project = ""
		}
		prevProject, first = r.ProjectPath, false
		fmt.Fprintf(tw, "%s\t%d\t%d\t%s\n", project, r.VMPort, r.HostPort, statusText(r))
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("ports: render table: %w", err)
	}
	return nil
}

// statusText renders the status cell, carrying the remap and conflict
// annotations the design document calls for.
func statusText(r Row) string {
	s := string(r.Status)
	if s == "" {
		s = string(StatusInactive)
	}
	// A remap where the two ports agree is not a remap; guard rather than
	// print "remapped from 5432" beside host port 5432.
	if r.OriginalHostPort != 0 && r.OriginalHostPort != r.HostPort {
		s += fmt.Sprintf(" (remapped from %d)", r.OriginalHostPort)
	}
	if r.Conflict != "" {
		s += "  " + conflictText(r.Conflict)
	}
	return s
}
