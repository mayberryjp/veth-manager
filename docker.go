package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type endpoint struct {
	NetworkID         string
	EndpointID        string
	MacAddress        string
	IPAddress         string
	GlobalIPv6Address string
}

type container struct {
	ID     string `json:"Id"`
	Name   string
	Config struct {
		Labels map[string]string
	}
	State struct {
		Running bool
		Pid     int
	}
	HostConfig struct {
		NetworkMode string
	}
	NetworkSettings struct {
		Networks map[string]endpoint
	}
}

type network struct {
	ID         string `json:"Id"`
	Name       string
	Driver     string
	Options    map[string]string
	Containers map[string]endpoint
}

type dockerAPI interface {
	containers(context.Context) ([]string, error)
	inspect(context.Context, string) (container, error)
	network(context.Context, string) (network, error)
}

type dockerClient struct {
	client *http.Client
	stream *http.Client
}

func newDocker(socket string) *dockerClient {
	return dockerTransport("unix", socket)
}

func newDockerEndpoint(address string) (*dockerClient, error) {
	network, target, err := parseDockerEndpoint(address)
	if err != nil {
		return nil, err
	}
	return dockerTransport(network, target), nil
}

func parseDockerEndpoint(address string) (string, string, error) {
	if strings.HasPrefix(address, "/") {
		return "unix", address, nil
	}
	u, err := url.Parse(address)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", "", fmt.Errorf("Docker endpoint must be an absolute Unix socket path, unix:///path, or tcp://host:port")
	}
	switch u.Scheme {
	case "unix":
		if u.Host == "" && strings.HasPrefix(u.Path, "/") && u.Path != "/" {
			return "unix", u.Path, nil
		}
	case "tcp":
		host, port, err := net.SplitHostPort(u.Host)
		number, portErr := strconv.Atoi(port)
		if err == nil && portErr == nil && host != "" && number >= 1 && number <= 65535 && u.Path == "" {
			return "tcp", u.Host, nil
		}
	}
	return "", "", fmt.Errorf("invalid Docker endpoint %q; use an absolute Unix socket path, unix:///path, or tcp://host:port", address)
}

func dockerTransport(network, address string) *dockerClient {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, address)
		},
		ResponseHeaderTimeout: 10 * time.Second,
	}
	return &dockerClient{
		client: &http.Client{Transport: transport, Timeout: 15 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		stream: &http.Client{Transport: transport},
	}
}

func (d *dockerClient) start(ctx context.Context, id string) error {
	path := "/containers/" + url.PathEscape(id) + "/start"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://docker"+path, nil)
	if err != nil {
		return err
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("Docker start %s: %w", id, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotModified {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return fmt.Errorf("Docker %s: HTTP %d (reading error: %w)", path, resp.StatusCode, err)
	}
	return fmt.Errorf("Docker %s: HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
}

func dockerRequest(ctx context.Context, client *http.Client, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if readErr != nil {
			return nil, fmt.Errorf("Docker %s: HTTP %d (reading error: %w)", path, resp.StatusCode, readErr)
		}
		return nil, fmt.Errorf("Docker %s: HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return resp, nil
}

func (d *dockerClient) get(ctx context.Context, path string, out interface{}) error {
	resp, err := dockerRequest(ctx, d.client, path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode Docker %s: %w", path, err)
	}
	return nil
}

func (d *dockerClient) containers(ctx context.Context) ([]string, error) {
	return d.containerIDs(ctx, "/containers/json")
}

func (d *dockerClient) allContainers(ctx context.Context) ([]string, error) {
	return d.containerIDs(ctx, "/containers/json?all=1")
}

func (d *dockerClient) containerIDs(ctx context.Context, path string) ([]string, error) {
	var list []struct {
		ID string `json:"Id"`
	}
	if err := d.get(ctx, path, &list); err != nil {
		return nil, err
	}
	if list == nil {
		return nil, fmt.Errorf("Docker %s returned a null container list", path)
	}
	ids := make([]string, 0, len(list))
	for _, c := range list {
		ids = append(ids, c.ID)
	}
	return ids, nil
}

func (d *dockerClient) networkIDs(ctx context.Context) ([]string, error) {
	var list []struct {
		ID string `json:"Id"`
	}
	if err := d.get(ctx, "/networks", &list); err != nil {
		return nil, err
	}
	if list == nil {
		return nil, fmt.Errorf("Docker returned a null network list")
	}
	ids := make([]string, 0, len(list))
	for _, n := range list {
		ids = append(ids, n.ID)
	}
	return ids, nil
}

func (d *dockerClient) inspect(ctx context.Context, id string) (container, error) {
	var c container
	err := d.get(ctx, "/containers/"+url.PathEscape(id)+"/json", &c)
	return c, err
}

func (d *dockerClient) network(ctx context.Context, id string) (network, error) {
	var n network
	err := d.get(ctx, "/networks/"+url.PathEscape(id), &n)
	return n, err
}

func notify(ch chan<- struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// Subscribe before reconciliation; periodic scans cover the subscription/reconnect gap.
func (d *dockerClient) watch(ctx context.Context, changes chan<- struct{}, log *slog.Logger) {
	for ctx.Err() == nil {
		err := d.readEvents(ctx, changes)
		if ctx.Err() != nil {
			return
		}
		log.Error("Docker event stream disconnected; retrying in 3s", "error", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}

func (d *dockerClient) readEvents(ctx context.Context, changes chan<- struct{}) error {
	filter := url.QueryEscape(`{"type":["container","network"]}`)
	resp, err := dockerRequest(ctx, d.stream, "/events?filters="+filter)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	notify(changes)
	decoder := json.NewDecoder(resp.Body)
	for {
		var event json.RawMessage
		if err := decoder.Decode(&event); err != nil {
			return err
		}
		notify(changes)
	}
}
