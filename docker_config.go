package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

const defaultDockerConfig = "/etc/veth-manager/docker.yaml"

type dockerConfig struct {
	ControlContainerStartOrder bool   `yaml:"control_container_start_order"`
	Endpoint                   string `yaml:"endpoint"`
	StartOrder                 string `yaml:"start_order"`
	order                      []string
}

var dockerContainerName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

func loadDockerConfig(path string, explicit bool) (dockerConfig, error) {
	c := dockerConfig{Endpoint: "/var/run/docker.sock"}
	if ext := filepath.Ext(path); ext != ".yaml" && ext != ".yml" {
		return c, fmt.Errorf("Docker configuration must be a .yaml or .yml file")
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) && !explicit {
		return c, nil
	}
	if err != nil {
		return c, fmt.Errorf("Docker configuration %s: %w", path, err)
	}
	if json.Valid(data) {
		return c, fmt.Errorf("Docker configuration must be YAML, not JSON")
	}
	var document yaml.Node
	d := yaml.NewDecoder(bytes.NewReader(data))
	if err := d.Decode(&document); err != nil {
		return c, fmt.Errorf("Docker configuration: %w", err)
	}
	var extra yaml.Node
	if err := d.Decode(&extra); err != io.EOF {
		return c, fmt.Errorf("Docker configuration must contain one YAML document")
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return c, fmt.Errorf("Docker configuration must be a YAML mapping")
	}
	root := document.Content[0]
	for i := 0; i < len(root.Content); i += 2 {
		key, value := root.Content[i], root.Content[i+1]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" ||
			(key.Value != "endpoint" && key.Value != "start_order" && key.Value != "control_container_start_order") {
			return c, fmt.Errorf("unknown Docker configuration field %q", key.Value)
		}
		tag, kind := "!!str", "string"
		if key.Value == "control_container_start_order" {
			tag, kind = "!!bool", "boolean"
		}
		if value.Kind != yaml.ScalarNode || value.Tag != tag {
			return c, fmt.Errorf("Docker %s must be a %s", key.Value, kind)
		}
	}
	if err := root.Decode(&c); err != nil {
		return c, fmt.Errorf("Docker configuration: %w", err)
	}
	if _, _, err := parseDockerEndpoint(c.Endpoint); err != nil {
		return c, err
	}
	c.order = strings.Fields(c.StartOrder)
	seen := make(map[string]bool)
	for _, name := range c.order {
		if name != "*" && !dockerContainerName.MatchString(name) {
			return c, fmt.Errorf("start_order contains invalid container name %q", name)
		}
		if seen[name] {
			return c, fmt.Errorf("start_order contains duplicate entry %q", name)
		}
		seen[name] = true
	}
	return c, nil
}
