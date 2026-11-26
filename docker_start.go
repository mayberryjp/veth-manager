package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type dockerStartupAPI interface {
	allContainers(context.Context) ([]string, error)
	inspect(context.Context, string) (container, error)
	start(context.Context, string) error
}

const defaultDockerStartupState = "/var/lib/veth-manager/docker-startup.json"

type startupTarget struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type dockerStartupState struct {
	InvocationID string          `json:"invocation_id"`
	Pending      []startupTarget `json:"pending"`
	Completed    bool            `json:"completed"`
}

type containerStarter struct {
	statePath  string
	invocation func(context.Context) (string, error)
	state      *dockerStartupState
	loaded     bool
	dirty      bool
}

func validInvocationID(id string) bool {
	decoded, err := hex.DecodeString(id)
	return err == nil && len(decoded) == 16 && id != strings.Repeat("0", 32)
}

func parseDockerInvocation(output string) (string, error) {
	properties := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || (key != "ActiveState" && key != "InvocationID") {
			return "", fmt.Errorf("unexpected Docker service property %q", line)
		}
		if _, exists := properties[key]; exists {
			return "", fmt.Errorf("duplicate Docker service property %q", key)
		}
		properties[key] = value
	}
	if state := properties["ActiveState"]; state != "active" && state != "reloading" {
		return "", fmt.Errorf("Docker service is not active (state %q); startup sequence will wait", state)
	}
	id := properties["InvocationID"]
	if !validInvocationID(id) {
		return "", fmt.Errorf("Docker service returned an invalid InvocationID")
	}
	return id, nil
}

func dockerServiceInvocation(ctx context.Context, systemctl string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output, err := command(ctx, systemctl, "--no-pager", "show", "docker.service",
		"--property=ActiveState", "--property=InvocationID")
	if err != nil {
		return "", fmt.Errorf("read local Docker service invocation: %w", err)
	}
	return parseDockerInvocation(string(output))
}

func loadDockerStartupState(path string) (*dockerStartupState, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read Docker startup state %s: %w", path, err)
	}
	var state dockerStartupState
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return nil, fmt.Errorf("decode Docker startup state %s: %w", path, err)
	}
	if err := decoder.Decode(new(interface{})); err != io.EOF {
		return nil, fmt.Errorf("Docker startup state must contain one JSON document")
	}
	if !validInvocationID(state.InvocationID) || state.Completed != (len(state.Pending) == 0) {
		return nil, fmt.Errorf("invalid Docker startup state %s", path)
	}
	seen := make(map[string]bool)
	for _, target := range state.Pending {
		if target.ID == "" || !dockerContainerName.MatchString(target.Name) || seen[target.ID] {
			return nil, fmt.Errorf("invalid pending container in Docker startup state %s", path)
		}
		seen[target.ID] = true
	}
	return &state, nil
}

func (s *containerStarter) save(dryRun bool) error {
	if !s.dirty {
		return nil
	}
	if !dryRun {
		if err := os.MkdirAll(filepath.Dir(s.statePath), 0700); err != nil {
			return fmt.Errorf("create Docker startup state directory: %w", err)
		}
		if err := writeJSONAtomic(s.statePath, s.state, 0600); err != nil {
			return fmt.Errorf("save Docker startup state %s: %w", s.statePath, err)
		}
	}
	s.dirty = false
	return nil
}

func (s *containerStarter) checkInvocation(ctx context.Context, expected string) error {
	current, err := s.invocation(ctx)
	if err != nil {
		return err
	}
	if current != expected {
		return fmt.Errorf("Docker service changed invocation during startup; deferring remaining starts")
	}
	return nil
}

func (s *containerStarter) run(ctx context.Context, docker dockerStartupAPI, settings dockerConfig, dryRun bool, log *slog.Logger) error {
	if !settings.ControlContainerStartOrder {
		return nil
	}
	id, err := s.invocation(ctx)
	if err != nil {
		return err
	}
	if !validInvocationID(id) {
		return fmt.Errorf("invalid Docker service InvocationID")
	}
	if !s.loaded {
		s.state, err = loadDockerStartupState(s.statePath)
		if err != nil {
			return err
		}
		s.loaded = true
	}
	if s.state == nil || s.state.InvocationID != id {
		plan, err := planContainerStartup(ctx, docker, settings.order)
		if err != nil {
			return err
		}
		s.state = &dockerStartupState{InvocationID: id, Pending: plan, Completed: len(plan) == 0}
		s.dirty = true
	}
	// Persist the plan before mutations, and retry any failed progress write before more starts.
	if err := s.save(dryRun); err != nil {
		return err
	}
	if s.state.Completed {
		return nil
	}
	ids, err := docker.allContainers(ctx)
	if err != nil {
		return fmt.Errorf("startup inventory: %w", err)
	}
	present := make(map[string]bool, len(ids))
	for _, id := range ids {
		present[id] = true
	}
	for len(s.state.Pending) > 0 {
		target := s.state.Pending[0]
		if !present[target.ID] {
			log.Info("startup target removed; replacement left to its lifecycle manager", "container", target.Name, "container_id", target.ID)
		} else {
			current, err := docker.inspect(ctx, target.ID)
			if err != nil {
				return fmt.Errorf("startup inspect %s: %w", target.Name, err)
			}
			if current.ID != target.ID {
				return fmt.Errorf("container %q changed identity during startup", target.Name)
			}
			if strings.TrimPrefix(current.Name, "/") != target.Name {
				log.Info("startup target renamed; skipping retired container", "container", target.Name, "container_id", target.ID)
			} else if !current.State.Running {
				if current.State.Restarting || current.State.Dead || current.State.Status == "removing" {
					return fmt.Errorf("startup container %q is not startable yet (state %q)", target.Name, current.State.Status)
				}
				if err := s.checkInvocation(ctx, id); err != nil {
					return err
				}
				if dryRun {
					log.Info("would start container", "container", target.Name, "container_id", target.ID)
				} else {
					if err := docker.start(ctx, target.ID); err != nil {
						return fmt.Errorf("start_order container %q: %w", target.Name, err)
					}
					log.Info("started container", "container", target.Name, "container_id", target.ID)
				}
			}
		}
		s.state.Pending = s.state.Pending[1:]
		s.state.Completed = len(s.state.Pending) == 0
		s.dirty = true
		if err := s.save(dryRun); err != nil {
			return err
		}
	}
	if !dryRun {
		log.Info("Docker startup sequence completed", "invocation_id", id)
	}
	return nil
}

func planContainerStartup(ctx context.Context, docker dockerStartupAPI, order []string) ([]startupTarget, error) {
	if len(order) == 0 {
		return nil, nil
	}
	ids, err := docker.allContainers(ctx)
	if err != nil {
		return nil, fmt.Errorf("startup inventory: %w", err)
	}
	byName := make(map[string]container, len(ids))
	for _, id := range ids {
		c, err := docker.inspect(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("startup inspect %s: %w", id, err)
		}
		name := strings.TrimPrefix(c.Name, "/")
		if c.ID != id || name == "" {
			return nil, fmt.Errorf("invalid startup inventory for container %s", id)
		}
		if _, exists := byName[name]; exists {
			return nil, fmt.Errorf("duplicate container name %q in startup inventory", name)
		}
		byName[name] = c
	}
	explicit := make(map[string]bool)
	for _, name := range order {
		if name == "*" {
			continue
		}
		if _, exists := byName[name]; !exists {
			return nil, fmt.Errorf("start_order container %q does not exist", name)
		}
		explicit[name] = true
	}
	var remaining []string
	for name := range byName {
		if !explicit[name] {
			remaining = append(remaining, name)
		}
	}
	sort.Strings(remaining)
	var plan []string
	for _, name := range order {
		if name == "*" {
			plan = append(plan, remaining...)
		} else {
			plan = append(plan, name)
		}
	}
	targets := make([]startupTarget, 0, len(plan))
	for _, name := range plan {
		targets = append(targets, startupTarget{ID: byName[name].ID, Name: name})
	}
	return targets, nil
}
