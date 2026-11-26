package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testDockerServer(t *testing.T, handler http.HandlerFunc) *dockerClient {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := server.Client()
	client.Transport = rewriteTransport{base: client.Transport, baseURL: server.URL}
	return &dockerClient{client: client, stream: client}
}

type rewriteTransport struct {
	base    http.RoundTripper
	baseURL string
}

func (rt rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	copy := req.Clone(req.Context())
	parsed, err := http.NewRequest(req.Method, rt.baseURL+req.URL.RequestURI(), nil)
	if err != nil {
		return nil, err
	}
	copy.URL = parsed.URL
	return rt.base.RoundTrip(copy)
}

func TestDockerEndpoints(t *testing.T) {
	d := testDockerServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/containers/json":
			io.WriteString(w, `[{"Id":"abc"}]`)
		case "/containers/abc/json":
			io.WriteString(w, `{"Id":"abc","Name":"/test","State":{"Running":true,"Pid":123},"Config":{"Labels":{"host-veth-name":"veth_test"}},"NetworkSettings":{"Networks":{"lan":{"NetworkID":"net1","EndpointID":"ep1","MacAddress":"02:00:00:00:00:01","IPAddress":"172.18.0.2","GlobalIPv6Address":"fd00::2"}}}}`)
		case "/networks/net1":
			io.WriteString(w, `{"Id":"net1","Name":"lan","Driver":"bridge","Options":{"com.docker.network.bridge.name":"lan0"}}`)
		default:
			http.NotFound(w, r)
		}
	})
	ctx := context.Background()
	ids, err := d.containers(ctx)
	if err != nil || len(ids) != 1 || ids[0] != "abc" {
		t.Fatalf("list: %v %v", ids, err)
	}
	c, err := d.inspect(ctx, "abc")
	if err != nil || c.ID != "abc" || c.State.Pid != 123 || c.Config.Labels[labelKey] != "veth_test" ||
		c.NetworkSettings.Networks["lan"].GlobalIPv6Address != "fd00::2" {
		t.Fatalf("inspect: %+v %v", c, err)
	}
	n, err := d.network(ctx, "net1")
	if err != nil || n.Driver != "bridge" || n.Options["com.docker.network.bridge.name"] != "lan0" {
		t.Fatalf("network: %+v %v", n, err)
	}
}

func TestDockerErrors(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusForbidden, http.StatusInternalServerError, http.StatusOK} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			d := testDockerServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(code)
				io.WriteString(w, "not-json")
			})
			if _, err := d.containers(context.Background()); err == nil {
				t.Fatal("Docker failure hidden")
			}
		})
	}
}

func TestCleanupDockerInventoryIncludesStoppedAndNetworkOwners(t *testing.T) {
	d := testDockerServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/containers/json":
			if r.URL.Query().Get("all") != "1" {
				t.Error("cleanup inventory excludes stopped containers")
			}
			io.WriteString(w, `[{"Id":"stopped"}]`)
		case "/networks":
			io.WriteString(w, `[{"Id":"net1"}]`)
		case "/networks/net1":
			io.WriteString(w, `{"Id":"net1","Driver":"ghcr.io/claymore666/docker-net-dhcp:v2.3.1","Options":{"mode":"bridge","bridge":"br-vlan20"},"Containers":{"unknown":{"EndpointID":"abcdef1234567890"}}}`)
		default:
			http.NotFound(w, r)
		}
	})
	ctx := context.Background()
	ids, err := d.allContainers(ctx)
	if err != nil || len(ids) != 1 || ids[0] != "stopped" {
		t.Fatalf("all containers: %v %v", ids, err)
	}
	ids, err = d.networkIDs(ctx)
	if err != nil || len(ids) != 1 || ids[0] != "net1" {
		t.Fatalf("networks: %v %v", ids, err)
	}
	n, err := d.network(ctx, ids[0])
	if err != nil || n.Containers["unknown"].EndpointID != "abcdef1234567890" {
		t.Fatalf("network ownership missing: %+v %v", n, err)
	}
}

func TestCleanupRejectsNullDockerInventory(t *testing.T) {
	d := testDockerServer(t, func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "null")
	})
	if _, err := d.allContainers(context.Background()); err == nil {
		t.Fatal("null container inventory accepted")
	}
	if _, err := d.networkIDs(context.Background()); err == nil {
		t.Fatal("null network inventory accepted")
	}
}
func TestDockerCancellation(t *testing.T) {
	d := newDocker("unused")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.containers(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation not propagated: %v", err)
	}
}

func TestEventStream(t *testing.T) {
	d := testDockerServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Query().Get("filters"), `"container","network"`) {
			t.Errorf("missing event filters: %s", r.URL.RawQuery)
		}
		io.WriteString(w, `{"Type":"network","Action":"connect"}`+"\n")
	})
	changes := make(chan struct{}, 1)
	err := d.readEvents(context.Background(), changes)
	if !errors.Is(err, io.EOF) || len(changes) != 1 {
		t.Fatalf("events: %v notifications=%d", err, len(changes))
	}
}

func TestEventWatcherStopsOnCancellation(t *testing.T) {
	d := testDockerServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	changes := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		d.watch(ctx, changes, slog.New(slog.NewTextHandler(io.Discard, nil)))
		close(done)
	}()
	select {
	case <-changes:
	case <-time.After(5 * time.Second):
		t.Fatal("event subscription did not connect")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("event watcher did not stop")
	}
}

func TestNotifyCoalesces(t *testing.T) {
	ch := make(chan struct{}, 1)
	notify(ch)
	notify(ch)
	if len(ch) != 1 {
		t.Fatal("notifications not coalesced")
	}
}
func TestDockerTCPEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/containers/json" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		io.WriteString(w, `[{"Id":"tcp-container"}]`)
	}))
	defer server.Close()
	d, err := newDockerEndpoint(strings.Replace(server.URL, "http://", "tcp://", 1))
	if err != nil {
		t.Fatal(err)
	}
	defer d.client.CloseIdleConnections()
	ids, err := d.containers(context.Background())
	if err != nil || len(ids) != 1 || ids[0] != "tcp-container" {
		t.Fatalf("TCP endpoint: %v %v", ids, err)
	}
}

func TestDockerEndpointValidation(t *testing.T) {
	for _, tc := range []struct {
		address, network, target string
	}{
		{"/var/run/docker.sock", "unix", "/var/run/docker.sock"},
		{"unix:///var/run/docker.sock", "unix", "/var/run/docker.sock"},
		{"tcp://127.0.0.1:2375", "tcp", "127.0.0.1:2375"},
		{"tcp://[::1]:2375", "tcp", "[::1]:2375"},
	} {
		network, target, err := parseDockerEndpoint(tc.address)
		if err != nil || network != tc.network || target != tc.target {
			t.Fatalf("%s: %s %s %v", tc.address, network, target, err)
		}
	}
	for _, address := range []string{"", "docker.sock", "unix://host/socket", "unix:///", "tcp://localhost",
		"tcp://localhost:0", "tcp://localhost:65536", "tcp://localhost:abc", "tcp://localhost:2375/path",
		"tcp://user@localhost:2375", "tcp://localhost:2375?x=1", "tcp://localhost:2375#fragment",
		"http://localhost:2375", "https://localhost:2376"} {
		if _, err := newDockerEndpoint(address); err == nil {
			t.Errorf("invalid endpoint accepted: %q", address)
		}
	}
}

func TestDockerStartResponses(t *testing.T) {
	for _, code := range []int{http.StatusNoContent, http.StatusNotModified, http.StatusOK,
		http.StatusNotFound, http.StatusConflict, http.StatusInternalServerError, http.StatusTemporaryRedirect} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			d := testDockerServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/containers/abc/start" {
					t.Errorf("unexpected start request: %s %s", r.Method, r.URL.Path)
				}
				w.WriteHeader(code)
				if code != http.StatusNoContent && code != http.StatusNotModified {
					io.WriteString(w, "start failure")
				}
			})
			err := d.start(context.Background(), "abc")
			wantError := code != http.StatusNoContent && code != http.StatusNotModified
			if (err != nil) != wantError {
				t.Fatalf("HTTP %d: %v", code, err)
			}
			if wantError && !strings.Contains(err.Error(), "start failure") {
				t.Fatalf("error body lost: %v", err)
			}
		})
	}
	d := newDocker("unused")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := d.start(ctx, "abc"); !errors.Is(err, context.Canceled) {
		t.Fatalf("start cancellation not propagated: %v", err)
	}
}
