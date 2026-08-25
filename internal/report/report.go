// Package report writes a session out as JSON and renders that JSON as a
// self-contained HTML report.
package report

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"os"
	"path/filepath"

	"github.com/oppai/puffy/internal/model"
)

//go:embed report.html.tmpl
var assets embed.FS

// WriteJSON writes the session document. A path of "-" writes to stdout, which
// is what makes `puffy ping --json - | jq` work.
func WriteJSON(s *model.Session, path string, indent bool) error {
	w, close, err := create(path)
	if err != nil {
		return err
	}
	defer close()

	enc := json.NewEncoder(w)
	if indent {
		enc.SetIndent("", "  ")
	}
	if err := enc.Encode(s); err != nil {
		return fmt.Errorf("write json: %w", err)
	}
	return nil
}

// ReadJSON loads a session document written by WriteJSON. A path of "-" reads
// stdin.
func ReadJSON(path string) (*model.Session, error) {
	var r io.Reader = os.Stdin
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", path, err)
		}
		defer f.Close()
		r = f
	}
	var s model.Session
	if err := json.NewDecoder(r).Decode(&s); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if s.Tool != "" && s.Tool != "puffy" {
		return nil, fmt.Errorf("%s: not a puffy session (tool=%q)", path, s.Tool)
	}
	if s.Schema > 1 {
		return nil, fmt.Errorf("%s: session schema %d is newer than this puffy understands", path, s.Schema)
	}
	if len(s.Hops) == 0 {
		return nil, fmt.Errorf("%s: session has no hops", path)
	}
	return &s, nil
}

type htmlData struct {
	Title string
	Theme string
	JSON  template.JS
}

// WriteHTML renders the session into a single HTML file with the measurement
// data embedded, so the report can be attached to a ticket and opened offline.
// theme is "auto", "light" or "dark".
func WriteHTML(s *model.Session, path, theme string) error {
	tmpl, err := template.ParseFS(assets, "report.html.tmpl")
	if err != nil {
		return fmt.Errorf("parse report template: %w", err)
	}

	// json.Marshal escapes <, > and & as < and friends, so the document
	// cannot break out of the script element it is embedded in.
	blob, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("encode session: %w", err)
	}

	switch theme {
	case "light", "dark":
	default:
		theme = "auto"
	}

	title := fmt.Sprintf("puffy %s %s", s.Mode, s.TargetIP)
	if s.Target != "" && s.Target != s.TargetIP {
		title = fmt.Sprintf("puffy %s %s (%s)", s.Mode, s.Target, s.TargetIP)
	}

	w, closeFn, err := create(path)
	if err != nil {
		return err
	}
	defer closeFn()

	if err := tmpl.Execute(w, htmlData{Title: title, Theme: theme, JSON: template.JS(blob)}); err != nil {
		return fmt.Errorf("render report: %w", err)
	}
	return nil
}

// create opens a destination for writing, treating "-" as stdout and creating
// parent directories for a real path.
func create(path string) (io.Writer, func() error, error) {
	if path == "-" || path == "" {
		return os.Stdout, func() error { return nil }, nil
	}
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, nil, fmt.Errorf("create %s: %w", dir, err)
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, nil, fmt.Errorf("create %s: %w", path, err)
	}
	return f, f.Close, nil
}
