package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

const defaultManagerConfig = "/etc/veth-manager/manager.yaml"

type managerConfig struct {
	Renaming struct {
		Enabled bool `yaml:"enabled"`
	} `yaml:"renaming"`
	OrphanCleanup struct {
		Enabled  bool   `yaml:"enabled"`
		Interval string `yaml:"interval"`
		MinAge   string `yaml:"min_age"`
		interval time.Duration
		minAge   time.Duration
	} `yaml:"orphan_cleanup"`
}

func loadManagerConfig(path string, explicit bool) (managerConfig, error) {
	c := managerConfig{}
	c.OrphanCleanup.Interval, c.OrphanCleanup.MinAge = "5m", "10m"
	c.OrphanCleanup.interval, c.OrphanCleanup.minAge = 5*time.Minute, 10*time.Minute
	if ext := filepath.Ext(path); ext != ".yaml" && ext != ".yml" {
		return c, fmt.Errorf("manager configuration must be a .yaml or .yml file")
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) && !explicit {
		return c, nil
	}
	if err != nil {
		return c, fmt.Errorf("manager configuration %s: %w", path, err)
	}
	if json.Valid(data) {
		return c, fmt.Errorf("manager configuration must be YAML, not JSON")
	}
	var document yaml.Node
	d := yaml.NewDecoder(bytes.NewReader(data))
	if err := d.Decode(&document); err != nil {
		return c, fmt.Errorf("manager configuration: %w", err)
	}
	var extra yaml.Node
	if err := d.Decode(&extra); err != io.EOF {
		return c, fmt.Errorf("manager configuration must contain one YAML document")
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return c, fmt.Errorf("manager configuration must be a YAML mapping")
	}
	root := document.Content[0]
	for i := 0; i < len(root.Content); i += 2 {
		key, section := root.Content[i], root.Content[i+1]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" ||
			(key.Value != "renaming" && key.Value != "orphan_cleanup") ||
			section.Kind != yaml.MappingNode || section.Tag != "!!map" {
			return c, fmt.Errorf("manager configuration requires renaming and orphan_cleanup mappings")
		}
		for j := 0; j < len(section.Content); j += 2 {
			field, value := section.Content[j], section.Content[j+1]
			tag := "!!bool"
			switch field.Value {
			case "enabled":
			case "interval", "min_age":
				if key.Value != "orphan_cleanup" {
					return c, fmt.Errorf("unknown renaming field %q", field.Value)
				}
				tag = "!!str"
			default:
				return c, fmt.Errorf("unknown %s field %q", key.Value, field.Value)
			}
			if field.Kind != yaml.ScalarNode || field.Tag != "!!str" ||
				value.Kind != yaml.ScalarNode || value.Tag != tag {
				return c, fmt.Errorf("%s.%s must have type %s", key.Value, field.Value, tag)
			}
		}
	}
	if err := root.Decode(&c); err != nil {
		return c, fmt.Errorf("manager configuration: %w", err)
	}
	for _, item := range []struct {
		name, text string
		out        *time.Duration
	}{
		{"interval", c.OrphanCleanup.Interval, &c.OrphanCleanup.interval},
		{"min_age", c.OrphanCleanup.MinAge, &c.OrphanCleanup.minAge},
	} {
		duration, err := time.ParseDuration(item.text)
		if err != nil || duration < time.Second {
			return c, fmt.Errorf("orphan_cleanup.%s must be a duration of at least 1s", item.name)
		}
		*item.out = duration
	}
	if c.OrphanCleanup.minAge < c.OrphanCleanup.interval {
		return c, fmt.Errorf("orphan_cleanup.min_age must be at least orphan_cleanup.interval")
	}
	return c, nil
}
