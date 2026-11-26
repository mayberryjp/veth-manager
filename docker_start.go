package main

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
)

type dockerStartupAPI interface {
	allContainers(context.Context) ([]string, error)
	inspect(context.Context, string) (container, error)
	start(context.Context, string) error
}

func startContainers(ctx context.Context, docker dockerStartupAPI, order []string, dryRun bool, log *slog.Logger) error {
	if len(order) == 0 {
		return nil
	}
	ids, err := docker.allContainers(ctx)
	if err != nil {
		return fmt.Errorf("startup inventory: %w", err)
	}
	byName := make(map[string]container, len(ids))
	for _, id := range ids {
		c, err := docker.inspect(ctx, id)
		if err != nil {
			return fmt.Errorf("startup inspect %s: %w", id, err)
		}
		name := strings.TrimPrefix(c.Name, "/")
		if c.ID != id || name == "" {
			return fmt.Errorf("invalid startup inventory for container %s", id)
		}
		if _, exists := byName[name]; exists {
			return fmt.Errorf("duplicate container name %q in startup inventory", name)
		}
		byName[name] = c
	}
	explicit := make(map[string]bool)
	for _, name := range order {
		if name == "*" {
			continue
		}
		if _, exists := byName[name]; !exists {
			return fmt.Errorf("start_order container %q does not exist", name)
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
	for _, name := range plan {
		c := byName[name]
		// Reread state because earlier starts or another lifecycle manager may have changed it.
		current, err := docker.inspect(ctx, c.ID)
		if err != nil {
			return fmt.Errorf("startup inspect %s: %w", name, err)
		}
		if current.ID != c.ID || current.Name != c.Name {
			return fmt.Errorf("container %q changed identity during startup", name)
		}
		if current.State.Running {
			continue
		}
		if dryRun {
			log.Info("would start container", "container", name, "container_id", c.ID)
			continue
		}
		if err := docker.start(ctx, c.ID); err != nil {
			return fmt.Errorf("start_order container %q: %w", name, err)
		}
		log.Info("started container", "container", name, "container_id", c.ID)
	}
	return nil
}
