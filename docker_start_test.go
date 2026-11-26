package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestStartContainersOrderAndRestart(t *testing.T) {
	for _, tc := range []struct {
		name      string
		order     []string
		dryRun    bool
		fail      string
		want      []string
		wantError bool
	}{
		{"ordered wildcard", []string{"beta", "alpha", "*"}, false, "", []string{"beta", "alpha", "delta", "zeta"}, false},
		{"wildcard before explicit", []string{"*", "beta"}, false, "", []string{"alpha", "delta", "zeta", "beta"}, false},
		{"no wildcard", []string{"beta", "alpha"}, false, "", []string{"beta", "alpha"}, false},
		{"dry run", []string{"beta", "alpha", "*"}, true, "", nil, false},
		{"missing name", []string{"alpha", "missing", "*"}, false, "", nil, true},
		{"start failure", []string{"beta", "alpha", "*"}, false, "alpha", []string{"beta", "alpha"}, true},
		{"disabled", nil, false, "", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			names := []string{"zeta", "running", "beta", "delta", "alpha"}
			running := map[string]bool{"running": true}
			var starts []string
			d := testDockerServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/containers/json" {
					if len(tc.order) == 0 {
						t.Error("disabled startup contacted Docker")
					}
					if r.URL.Query().Get("all") != "1" {
						t.Error("startup inventory omitted stopped containers")
					}
					var list []map[string]string
					for _, name := range names {
						list = append(list, map[string]string{"Id": "id-" + name})
					}
					json.NewEncoder(w).Encode(list)
					return
				}
				parts := strings.Split(r.URL.Path, "/")
				if len(parts) != 4 || parts[1] != "containers" {
					http.NotFound(w, r)
					return
				}
				name := strings.TrimPrefix(parts[2], "id-")
				switch parts[3] {
				case "json":
					var c container
					c.ID, c.Name, c.State.Running = parts[2], "/"+name, running[name]
					json.NewEncoder(w).Encode(c)
				case "start":
					if r.Method != http.MethodPost {
						t.Error("start must POST")
					}
					starts = append(starts, name)
					if name == tc.fail {
						http.Error(w, "cannot start", http.StatusInternalServerError)
						return
					}
					running[name] = true
					w.WriteHeader(http.StatusNoContent)
				default:
					http.NotFound(w, r)
				}
			})
			var output bytes.Buffer
			log := slog.New(slog.NewJSONHandler(&output, nil))
			settings := dockerConfig{ControlContainerStartOrder: true, order: tc.order}
			err := startContainers(context.Background(), d, settings, tc.dryRun, log)
			if (err != nil) != tc.wantError || !reflect.DeepEqual(starts, tc.want) {
				t.Fatalf("starts=%v want=%v err=%v", starts, tc.want, err)
			}
			if tc.dryRun {
				var planned []string
				decoder := json.NewDecoder(&output)
				for decoder.More() {
					var entry struct {
						Message   string `json:"msg"`
						Container string `json:"container"`
					}
					if err := decoder.Decode(&entry); err != nil {
						t.Fatal(err)
					}
					if entry.Message != "would start container" {
						t.Fatalf("unexpected dry-run message: %s", entry.Message)
					}
					planned = append(planned, entry.Container)
				}
				if !reflect.DeepEqual(planned, []string{"beta", "alpha", "delta", "zeta"}) {
					t.Fatalf("dry-run order: %v", planned)
				}
			}
			if tc.name == "ordered wildcard" {
				starts = nil
				if err := startContainers(context.Background(), d, settings, false, log); err != nil || len(starts) != 0 {
					t.Fatalf("running containers restarted: %v %v", starts, err)
				}
				running["beta"] = false
				if err := startContainers(context.Background(), d, settings, false, log); err != nil ||
					!reflect.DeepEqual(starts, []string{"beta"}) {
					t.Fatalf("stopped container not restarted alone: %v %v", starts, err)
				}
			}
		})
	}
}

func TestStartupInspectionFailuresPreventStarts(t *testing.T) {
	for _, mode := range []string{"inventory failure", "inspect failure", "invalid identity", "duplicate name", "identity changed", "already started"} {
		t.Run(mode, func(t *testing.T) {
			inspections := 0
			d := testDockerServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/containers/json" {
					if mode == "inventory failure" {
						http.Error(w, "offline", http.StatusServiceUnavailable)
						return
					}
					if mode == "duplicate name" {
						io.WriteString(w, `[{"Id":"a"},{"Id":"b"}]`)
					} else {
						io.WriteString(w, `[{"Id":"a"}]`)
					}
					return
				}
				if strings.HasSuffix(r.URL.Path, "/start") {
					t.Error("inspection failure or running container must not start")
					w.WriteHeader(http.StatusNoContent)
					return
				}
				inspections++
				if mode == "inspect failure" {
					http.Error(w, "missing", http.StatusNotFound)
					return
				}
				var c container
				c.ID, c.Name = "a", "/alpha"
				if r.URL.Path == "/containers/b/json" {
					c.ID = "b"
				}
				if mode == "invalid identity" {
					c.ID = ""
				}
				if inspections > 1 && mode == "identity changed" {
					c.Name = "/renamed"
				}
				c.State.Running = inspections > 1 && mode == "already started"
				json.NewEncoder(w).Encode(c)
			})
			err := startContainers(context.Background(), d, dockerConfig{ControlContainerStartOrder: true, order: []string{"alpha"}}, false,
				slog.New(slog.NewTextHandler(io.Discard, nil)))
			if (err != nil) != (mode != "already started") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestStartupControlDisabledPreservesDockerAccess(t *testing.T) {
	requests := 0
	d := testDockerServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodGet || r.URL.RequestURI() != "/containers/json" {
			t.Errorf("disabled startup issued request: %s %s", r.Method, r.URL.RequestURI())
		}
		io.WriteString(w, `[{"Id":"running-container"}]`)
	})
	settings := dockerConfig{order: []string{"missing-container", "*"}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, dryRun := range []bool{false, true} {
		if err := startContainers(context.Background(), d, settings, dryRun, log); err != nil {
			t.Fatal(err)
		}
	}
	if requests != 0 {
		t.Fatal("disabled control contacted Docker for startup management")
	}
	ids, err := d.containers(context.Background())
	if err != nil || len(ids) != 1 || ids[0] != "running-container" {
		t.Fatalf("disabled startup prevented normal Docker access: %v %v", ids, err)
	}
}
