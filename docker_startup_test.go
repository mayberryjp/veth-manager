package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func testContainerStarter(t *testing.T, invocation *string) containerStarter {
	t.Helper()
	return containerStarter{
		statePath: filepath.Join(t.TempDir(), "docker-startup.json"),
		invocation: func(context.Context) (string, error) {
			return *invocation, nil
		},
	}
}

type startupDocker struct {
	ids            []string
	containersByID map[string]container
	started        []string
	failID         string
	inventoryError error
	inventoryCalls int
	afterStart     func(string)
}

func (d *startupDocker) allContainers(context.Context) ([]string, error) {
	d.inventoryCalls++
	return d.ids, d.inventoryError
}

func (d *startupDocker) inspect(_ context.Context, id string) (container, error) {
	c, exists := d.containersByID[id]
	if !exists {
		return container{}, fmt.Errorf("container %s not found", id)
	}
	return c, nil
}

func (d *startupDocker) start(_ context.Context, id string) error {
	if id == d.failID {
		return fmt.Errorf("start %s failed", id)
	}
	c, exists := d.containersByID[id]
	if !exists {
		return fmt.Errorf("container %s not found", id)
	}
	c.State.Running = true
	d.containersByID[id] = c
	d.started = append(d.started, id)
	if d.afterStart != nil {
		d.afterStart(id)
	}
	return nil
}

func startupFixture() *startupDocker {
	return &startupDocker{ids: []string{"a", "b"}, containersByID: map[string]container{
		"a": {ID: "a", Name: "/alpha"},
		"b": {ID: "b", Name: "/beta"},
	}}
}

func startupSettings() dockerConfig {
	return dockerConfig{ControlContainerStartOrder: true, order: []string{"alpha", "beta"}}
}

func TestDockerInvocationParsing(t *testing.T) {
	id := strings.Repeat("a", 32)
	for _, tc := range []struct {
		name, output string
		invalid      bool
	}{
		{"active", "InvocationID=" + id + "\nActiveState=active\n", false},
		{"reload", "ActiveState=reloading\nInvocationID=" + id + "\n", false},
		{"activating", "InvocationID=" + id + "\nActiveState=activating\n", true},
		{"inactive", "InvocationID=\nActiveState=inactive\n", true},
		{"missing ID", "ActiveState=active\n", true},
		{"invalid ID", "ActiveState=active\nInvocationID=not-an-id\n", true},
		{"zero ID", "ActiveState=active\nInvocationID=" + strings.Repeat("0", 32) + "\n", true},
		{"duplicate ID", "ActiveState=active\nInvocationID=" + id + "\nInvocationID=" + id + "\n", true},
		{"unknown property", "ActiveState=active\nMainPID=123\nInvocationID=" + id + "\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseDockerInvocation(tc.output)
			if (err != nil) != tc.invalid || (!tc.invalid && got != id) {
				t.Fatalf("invocation=%q error=%v", got, err)
			}
		})
	}
}

func TestCompletedStartupSurvivesManagerRestartAndContainerChanges(t *testing.T) {
	id := strings.Repeat("a", 32)
	starter := testContainerStarter(t, &id)
	d := startupFixture()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := starter.run(context.Background(), d, startupSettings(), false, log); err != nil {
		t.Fatal(err)
	}
	state, err := loadDockerStartupState(starter.statePath)
	if err != nil || !state.Completed || state.InvocationID != id {
		t.Fatalf("completion not persisted: %+v %v", state, err)
	}
	for key, c := range d.containersByID {
		c.State.Running = false
		d.containersByID[key] = c
	}
	d.ids = append(d.ids, "c")
	d.containersByID["c"] = container{ID: "c", Name: "/gamma"}
	inventoryCalls := d.inventoryCalls
	restarted := containerStarter{statePath: starter.statePath, invocation: starter.invocation}
	settings := startupSettings()
	settings.order = []string{"beta", "alpha", "*"}
	if err := restarted.run(context.Background(), d, settings, false, log); err != nil ||
		d.inventoryCalls != inventoryCalls || !reflect.DeepEqual(d.started, []string{"a", "b"}) {
		t.Fatalf("manager restart or container changes repeated startup: %v %v", d.started, err)
	}
	id = strings.Repeat("b", 32)
	if err := restarted.run(context.Background(), d, settings, false, log); err != nil ||
		!reflect.DeepEqual(d.started, []string{"a", "b", "b", "a", "c"}) {
		t.Fatalf("new Docker invocation did not run new order: %v %v", d.started, err)
	}
}

func TestEmptyStartupOrderDoesNotArmLaterStartsInSameInvocation(t *testing.T) {
	id := strings.Repeat("a", 32)
	starter := testContainerStarter(t, &id)
	d := startupFixture()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	empty := dockerConfig{ControlContainerStartOrder: true}
	if err := starter.run(context.Background(), d, empty, false, log); err != nil || d.inventoryCalls != 0 {
		t.Fatalf("empty sequence contacted Docker: %v", err)
	}
	if err := starter.run(context.Background(), d, startupSettings(), false, log); err != nil || len(d.started) != 0 {
		t.Fatalf("order change armed starts without a Docker restart: %v %v", d.started, err)
	}
	id = strings.Repeat("b", 32)
	if err := starter.run(context.Background(), d, startupSettings(), false, log); err != nil ||
		!reflect.DeepEqual(d.started, []string{"a", "b"}) {
		t.Fatalf("new Docker invocation did not use the order: %v %v", d.started, err)
	}
}

func TestPendingStartupDoesNotRestartPreviouslyProcessedContainers(t *testing.T) {
	id := strings.Repeat("a", 32)
	starter := testContainerStarter(t, &id)
	d := startupFixture()
	d.failID = "b"
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := starter.run(context.Background(), d, startupSettings(), false, log); err == nil {
		t.Fatal("start failure hidden")
	}
	state, err := loadDockerStartupState(starter.statePath)
	if err != nil || state.Completed || !reflect.DeepEqual(state.Pending, []startupTarget{{ID: "b", Name: "beta"}}) {
		t.Fatalf("progress not persisted: %+v %v", state, err)
	}
	c := d.containersByID["a"]
	c.State.Running = false
	d.containersByID["a"] = c
	d.failID = ""
	restarted := containerStarter{statePath: starter.statePath, invocation: starter.invocation}
	if err := restarted.run(context.Background(), d, startupSettings(), false, log); err != nil ||
		!reflect.DeepEqual(d.started, []string{"a", "b"}) || d.containersByID["a"].State.Running {
		t.Fatalf("retry restarted an already processed container: %v %v", d.started, err)
	}
}

func TestStartupSkipsRetiredIDsWithoutStartingReplacements(t *testing.T) {
	for _, change := range []string{"removed", "renamed"} {
		t.Run(change, func(t *testing.T) {
			id := strings.Repeat("a", 32)
			starter := testContainerStarter(t, &id)
			d := startupFixture()
			d.failID = "a"
			log := slog.New(slog.NewTextHandler(io.Discard, nil))
			if err := starter.run(context.Background(), d, startupSettings(), false, log); err == nil {
				t.Fatal("start failure hidden")
			}
			d.failID = ""
			d.containersByID["new"] = container{ID: "new", Name: "/alpha"}
			if change == "removed" {
				delete(d.containersByID, "a")
				d.ids = []string{"new", "b"}
			} else {
				c := d.containersByID["a"]
				c.Name = "/alpha-old"
				d.containersByID["a"] = c
				d.ids = []string{"a", "new", "b"}
			}
			if err := starter.run(context.Background(), d, startupSettings(), false, log); err != nil ||
				!reflect.DeepEqual(d.started, []string{"b"}) {
				t.Fatalf("retired target or replacement started: %v %v", d.started, err)
			}
		})
	}
}

func TestStartupProgressWriteFailureStopsFurtherStarts(t *testing.T) {
	id := strings.Repeat("a", 32)
	starter := testContainerStarter(t, &id)
	d := startupFixture()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	d.afterStart = func(id string) {
		if id != "a" {
			return
		}
		if err := os.Remove(starter.statePath); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(starter.statePath, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := starter.run(context.Background(), d, startupSettings(), false, log); err == nil ||
		!reflect.DeepEqual(d.started, []string{"a"}) {
		t.Fatalf("failed progress write did not stop sequence: %v %v", d.started, err)
	}
	if err := os.Remove(starter.statePath); err != nil {
		t.Fatal(err)
	}
	c := d.containersByID["a"]
	c.State.Running = false
	d.containersByID["a"] = c
	if err := starter.run(context.Background(), d, startupSettings(), false, log); err != nil ||
		!reflect.DeepEqual(d.started, []string{"a", "b"}) || d.containersByID["a"].State.Running {
		t.Fatalf("progress-write retry repeated successful start: %v %v", d.started, err)
	}
}

func TestStartupWaitsForDockerServiceAndAPI(t *testing.T) {
	id := strings.Repeat("a", 32)
	starter := testContainerStarter(t, &id)
	getInvocation := starter.invocation
	starter.invocation = func(context.Context) (string, error) {
		return "", errors.New("Docker service is activating")
	}
	d := startupFixture()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := starter.run(context.Background(), d, startupSettings(), false, log); err == nil || d.inventoryCalls != 0 {
		t.Fatalf("inactive Docker service was used: %v", err)
	}
	starter.invocation = getInvocation
	d.inventoryError = errors.New("Docker API is not ready")
	if err := starter.run(context.Background(), d, startupSettings(), false, log); err == nil {
		t.Fatal("unready Docker API accepted")
	}
	if _, err := os.Stat(starter.statePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unready Docker startup recorded a plan: %v", err)
	}
	d.inventoryError = nil
	if err := starter.run(context.Background(), d, startupSettings(), false, log); err != nil ||
		!reflect.DeepEqual(d.started, []string{"a", "b"}) {
		t.Fatalf("ready Docker startup not applied: %v %v", d.started, err)
	}
}

func TestDockerInvocationChangeAbortsRemainingStartup(t *testing.T) {
	id := strings.Repeat("a", 32)
	starter := testContainerStarter(t, &id)
	d := startupFixture()
	d.afterStart = func(containerID string) {
		if containerID == "a" {
			id = strings.Repeat("b", 32)
		}
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := starter.run(context.Background(), d, startupSettings(), false, log); err == nil ||
		!reflect.DeepEqual(d.started, []string{"a"}) {
		t.Fatalf("sequence crossed Docker invocations: %v %v", d.started, err)
	}
	d.afterStart = nil
	c := d.containersByID["a"]
	c.State.Running = false
	d.containersByID["a"] = c
	if err := starter.run(context.Background(), d, startupSettings(), false, log); err != nil ||
		!reflect.DeepEqual(d.started, []string{"a", "a", "b"}) {
		t.Fatalf("new invocation did not get a fresh startup plan: %v %v", d.started, err)
	}
}

func TestStartupInvalidStateFailsClosedAndDryRunDoesNotPersist(t *testing.T) {
	id := strings.Repeat("a", 32)
	for _, data := range []string{
		"invalid-json",
		`{"invocation_id":"` + id + `","completed":false,"pending":[]}`,
		`{"invocation_id":"` + id + `","completed":true,"pending":[],"unknown":true}`,
		`{"invocation_id":"` + id + `","completed":false,"pending":[{"id":"a","name":"alpha"},{"id":"a","name":"beta"}]}`,
	} {
		starter := testContainerStarter(t, &id)
		if err := os.WriteFile(starter.statePath, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		d := startupFixture()
		if err := starter.run(context.Background(), d, startupSettings(), false,
			slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil || d.inventoryCalls != 0 || len(d.started) != 0 {
			t.Fatalf("invalid state triggered startup: %v %v", d.started, err)
		}
	}
	starter := testContainerStarter(t, &id)
	d := startupFixture()
	var output bytes.Buffer
	if err := starter.run(context.Background(), d, startupSettings(), true,
		slog.New(slog.NewTextHandler(&output, nil))); err != nil || len(d.started) != 0 {
		t.Fatalf("dry-run changed Docker: %v %v", d.started, err)
	}
	if !strings.Contains(output.String(), "would start container") {
		t.Fatal("dry-run did not describe the startup plan")
	}
	if _, err := os.Stat(starter.statePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry-run persisted startup state: %v", err)
	}
	actual := containerStarter{statePath: starter.statePath, invocation: starter.invocation}
	if err := actual.run(context.Background(), d, startupSettings(), false,
		slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil || !reflect.DeepEqual(d.started, []string{"a", "b"}) {
		t.Fatalf("dry-run prevented actual startup: %v %v", d.started, err)
	}
}
