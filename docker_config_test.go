package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDockerConfigOrderAndValidation(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		invalid    bool
	}{
		{"disabled", "start_order: ''\n", false},
		{"ordered", "endpoint: tcp://127.0.0.1:2375\nstart_order: container1 container2 container3 *\n", false},
		{"wildcard first", "start_order: '* container1'\n", false},
		{"unknown", "socket: /var/run/docker.sock\n", true},
		{"duplicate key", "start_order: a\nstart_order: b\n", true},
		{"duplicate name", "start_order: a b a\n", true},
		{"duplicate wildcard", "start_order: '* a *'\n", true},
		{"invalid name", "start_order: /container1\n", true},
		{"sequence", "start_order: [a, b]\n", true},
		{"boolean", "start_order: false\n", true},
		{"null", "start_order: null\n", true},
		{"empty endpoint", "endpoint: ''\n", true},
		{"invalid endpoint", "endpoint: tcp://127.0.0.1\n", true},
		{"alias", "endpoint: &endpoint /var/run/docker.sock\nstart_order: *endpoint\n", true},
		{"multiple documents", "start_order: a\n---\nstart_order: b\n", true},
		{"JSON", `{"start_order":"a"}`, true},
		{"empty file", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "docker.yaml")
			if err := os.WriteFile(path, []byte(tc.text), 0600); err != nil {
				t.Fatal(err)
			}
			c, err := loadDockerConfig(path, true)
			if (err != nil) != tc.invalid {
				t.Fatalf("config=%+v err=%v", c, err)
			}
			if tc.name == "ordered" && (c.Endpoint != "tcp://127.0.0.1:2375" ||
				!reflect.DeepEqual(c.order, []string{"container1", "container2", "container3", "*"})) {
				t.Fatalf("order or endpoint lost: %+v", c)
			}
			if tc.name == "disabled" && len(c.order) != 0 {
				t.Fatal("empty order enabled startup")
			}
		})
	}
}

func TestDockerMissingConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "docker.yaml")
	c, err := loadDockerConfig(path, false)
	if err != nil || len(c.order) != 0 || c.Endpoint != "/var/run/docker.sock" {
		t.Fatalf("missing default: %+v %v", c, err)
	}
	if _, err := loadDockerConfig(path, true); err == nil {
		t.Fatal("missing explicit config accepted")
	}
	if _, err := loadDockerConfig(filepath.Join(t.TempDir(), "docker.json"), false); err == nil {
		t.Fatal("non-YAML configuration accepted")
	}
}
