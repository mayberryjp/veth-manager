package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
)

type fakeCleanupDocker struct {
	*fakeDocker
	inventoryHook func()
}

func (d *fakeCleanupDocker) networkIDs(context.Context) ([]string, error) {
	if d.inventoryHook != nil {
		d.inventoryHook()
	}
	var ids []string
	for id := range d.ns {
		ids = append(ids, id)
	}
	return ids, d.listErr
}

func (d *fakeCleanupDocker) allContainers(ctx context.Context) ([]string, error) {
	return d.containers(ctx)
}

type fakeCleanupLinks struct {
	*fakeLinks
	addresses  map[int]bool
	addressErr error
	deleteErr  error
	deleted    []dhcpPair
}

func (l *fakeCleanupLinks) addressed(context.Context) (map[int]bool, error) {
	return l.addresses, l.addressErr
}

func (l *fakeCleanupLinks) deletePair(_ context.Context, pair dhcpPair) error {
	if l.deleteErr != nil {
		return l.deleteErr
	}
	l.deleted = append(l.deleted, pair)
	var remaining []link
	for _, link := range l.hs {
		if link.Index != pair.BridgeEnd.Index && link.Index != pair.Peer.Index {
			remaining = append(remaining, link)
		}
	}
	l.hs = remaining
	return nil
}

func cleanupFixture() (*fakeCleanupDocker, *fakeCleanupLinks, *orphanCleaner) {
	d, _, _ := dhcpBridgeFixture("bridge")
	stale := veth(51, 50, "dh-abcdef123456", "02:00:00:00:00:01", "br-vlan20")
	stale.Flags = []string{"BROADCAST", "UP", "NO-CARRIER"}
	peer := veth(50, 51, "abcdef123456-dh", "02:00:00:00:00:02", "")
	peer.Flags = []string{"BROADCAST"}
	docker := &fakeCleanupDocker{fakeDocker: d}
	links := &fakeCleanupLinks{fakeLinks: &fakeLinks{hs: []link{stale, peer}}, addresses: map[int]bool{50: false, 51: false}}
	return docker, links, &orphanCleaner{docker: docker, links: links, minAge: time.Minute}
}

func TestOrphanCleanupRequiresRepeatedObservation(t *testing.T) {
	_, links, cleaner := cleanupFixture()
	now := time.Now()
	logged := 0
	onDelete := func(dhcpPair) { logged++ }
	for _, offset := range []time.Duration{0, 59 * time.Second} {
		result, err := cleaner.run(context.Background(), now.Add(offset), onDelete)
		if err != nil || result.Candidates != 1 || len(links.deleted) != 0 {
			t.Fatalf("premature deletion: %+v %v", result, err)
		}
	}
	result, err := cleaner.run(context.Background(), now.Add(time.Minute), onDelete)
	if err != nil || len(result.Deleted) != 1 || len(links.hs) != 0 || logged != 1 {
		t.Fatalf("mature orphan not removed/logged: %+v %v", result, err)
	}
	if _, err := cleaner.run(context.Background(), now.Add(2*time.Minute), onDelete); err != nil || len(links.deleted) != 1 {
		t.Fatalf("repeat cleanup: %v", err)
	}
}

func TestOrphanCleanupDryRunAndReset(t *testing.T) {
	_, links, cleaner := cleanupFixture()
	now := time.Now()
	cleaner.dryRun = true
	if _, err := cleaner.run(context.Background(), now, func(dhcpPair) {}); err != nil {
		t.Fatal(err)
	}
	result, err := cleaner.run(context.Background(), now.Add(time.Minute), func(dhcpPair) {})
	if err != nil || result.Eligible != 1 || len(links.deleted) != 0 {
		t.Fatalf("dry-run mutated links or lost plan: %+v %v", result, err)
	}
	cleaner.dryRun = false
	cleaner.reset()
	if _, err := cleaner.run(context.Background(), now.Add(2*time.Minute), func(dhcpPair) {}); err != nil || len(links.deleted) != 0 {
		t.Fatal("event/restart reset did not restart observation")
	}
}

func TestOrphanCleanupRejectsUnsafePairs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*fakeCleanupDocker, *fakeCleanupLinks)
	}{
		{"live carrier", func(_ *fakeCleanupDocker, l *fakeCleanupLinks) { l.hs[0].Flags = []string{"UP", "LOWER_UP"} }},
		{"peer up", func(_ *fakeCleanupDocker, l *fakeCleanupLinks) { l.hs[1].Flags = []string{"UP"} }},
		{"unknown flags", func(_ *fakeCleanupDocker, l *fakeCleanupLinks) { l.hs[0].Flags = nil }},
		{"namespace peer", func(_ *fakeCleanupDocker, l *fakeCleanupLinks) { id := 3; l.hs[0].NetNSID = &id }},
		{"wrong kind", func(_ *fakeCleanupDocker, l *fakeCleanupLinks) { l.hs[0].Info.Kind = "bridge" }},
		{"nonreciprocal", func(_ *fakeCleanupDocker, l *fakeCleanupLinks) { l.hs[1].Peer = 999 }},
		{"peer bridged", func(_ *fakeCleanupDocker, l *fakeCleanupLinks) { l.hs[1].Master = "br-other" }},
		{"address on bridge end", func(_ *fakeCleanupDocker, l *fakeCleanupLinks) { l.addresses[51] = true }},
		{"address on peer", func(_ *fakeCleanupDocker, l *fakeCleanupLinks) { l.addresses[50] = true }},
		{"missing address inventory", func(_ *fakeCleanupDocker, l *fakeCleanupLinks) { delete(l.addresses, 50) }},
		{"unrecognized name", func(_ *fakeCleanupDocker, l *fakeCleanupLinks) { l.hs[0].Name = "veth_someone_else" }},
		{"wrong bridge", func(_ *fakeCleanupDocker, l *fakeCleanupLinks) { l.hs[0].Master = "br-other" }},
		{"stopped unlabeled owner", func(d *fakeCleanupDocker, _ *fakeCleanupLinks) {
			c := container{ID: "stopped"}
			c.NetworkSettings.Networks = map[string]endpoint{"lan": {EndpointID: "abcdef1234567890"}}
			d.cs[c.ID] = c
		}},
		{"network-only owner", func(d *fakeCleanupDocker, _ *fakeCleanupLinks) {
			for id, n := range d.ns {
				n.Containers = map[string]endpoint{"unknown": {EndpointID: "abcdef1234567890"}}
				d.ns[id] = n
			}
		}},
		{"ordinary Docker bridge", func(d *fakeCleanupDocker, _ *fakeCleanupLinks) {
			for id, n := range d.ns {
				n.Driver = "bridge"
				n.Options = map[string]string{"com.docker.network.bridge.name": "br-vlan20"}
				d.ns[id] = n
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, links, cleaner := cleanupFixture()
			tc.change(d, links)
			for _, now := range []time.Time{time.Now(), time.Now().Add(time.Hour)} {
				result, err := cleaner.run(context.Background(), now, func(dhcpPair) {})
				if err != nil || result.Candidates != 0 || len(links.deleted) != 0 {
					t.Fatalf("unsafe cleanup: %+v %v", result, err)
				}
			}
		})
	}
}

func TestOrphanCleanupChangesAndFailuresResetGrace(t *testing.T) {
	for _, scenario := range []string{"Docker failure", "address failure", "link failure", "identity change", "disappeared", "delete failure"} {
		t.Run(scenario, func(t *testing.T) {
			d, links, cleaner := cleanupFixture()
			now := time.Now()
			if _, err := cleaner.run(context.Background(), now, func(dhcpPair) {}); err != nil {
				t.Fatal(err)
			}
			original := append([]link{}, links.hs...)
			switch scenario {
			case "Docker failure":
				d.inspectErr = errors.New("Docker unavailable")
			case "address failure":
				links.addressErr = errors.New("address lookup failed")
			case "link failure":
				links.hostErr = errors.New("link lookup failed")
			case "identity change":
				links.hs[0].Address = "02:00:00:00:00:99"
			case "disappeared":
				links.hs = nil
			case "delete failure":
				links.deleteErr = errors.New("delete failed")
			}
			_, err := cleaner.run(context.Background(), now.Add(time.Minute), func(dhcpPair) {})
			expectError := scenario != "identity change" && scenario != "disappeared"
			if (err != nil) != expectError || len(links.deleted) != 0 {
				t.Fatalf("failure hidden or premature delete: %v", err)
			}
			d.inspectErr, links.addressErr, links.hostErr, links.deleteErr = nil, nil, nil, nil
			links.hs = original
			if _, err := cleaner.run(context.Background(), now.Add(2*time.Minute), func(dhcpPair) {}); err != nil || len(links.deleted) != 0 {
				t.Fatalf("grace period not reset after change/failure: %v", err)
			}
		})
	}
}

func TestOrphanCleanupRechecksOwnershipBeforeDeletion(t *testing.T) {
	d, links, cleaner := cleanupFixture()
	now := time.Now()
	if _, err := cleaner.run(context.Background(), now, func(dhcpPair) {}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	d.inventoryHook = func() {
		calls++
		if calls == 2 {
			c := container{ID: "new-owner"}
			c.NetworkSettings.Networks = map[string]endpoint{"lan": {EndpointID: "abcdef1234567890"}}
			d.cs[c.ID] = c
		}
	}
	if _, err := cleaner.run(context.Background(), now.Add(time.Minute), func(dhcpPair) {}); err == nil || len(links.deleted) != 0 || cleaner.seen != nil {
		t.Fatalf("new ownership ignored: %v", err)
	}
}

func TestOrphanCleanupRejectsSharedBridge(t *testing.T) {
	d, links, cleaner := cleanupFixture()
	d.ns["shared"] = network{ID: "shared", Driver: "bridge", Options: map[string]string{"com.docker.network.bridge.name": "br-vlan20"}}
	if _, err := cleaner.run(context.Background(), time.Now(), func(dhcpPair) {}); err == nil || len(links.deleted) != 0 {
		t.Fatalf("shared bridge cleanup allowed: %v", err)
	}
}

func TestOrphanCleanupKernelIntegration(t *testing.T) {
	if runtime.GOOS != "linux" || os.Getenv("ORPHAN_CLEANUP_INTEGRATION") != "1" {
		t.Skip("requires Linux root and ORPHAN_CLEANUP_INTEGRATION=1; uses an isolated network namespace")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if os.Getenv("ORPHAN_CLEANUP_NETNS_CHILD") != "1" {
		binary, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		child := exec.CommandContext(ctx, "unshare", "--net", "--", binary, "-test.run=^TestOrphanCleanupKernelIntegration$", "-test.v")
		child.Env = append(os.Environ(), "ORPHAN_CLEANUP_NETNS_CHILD=1")
		if out, err := child.CombinedOutput(); err != nil {
			t.Fatalf("isolated cleanup test: %v\n%s", err, out)
		} else {
			t.Logf("%s", out)
		}
		return
	}
	ip, err := exec.LookPath("ip")
	if err != nil {
		t.Fatal(err)
	}
	backend := nativeLinks{}
	stale, peer := "dh-abcdef123456", "abcdef123456-dh"
	for _, args := range [][]string{
		{"link", "add", "br-test", "type", "bridge"},
		{"link", "set", "br-test", "up"},
		{"link", "add", stale, "type", "veth", "peer", "name", peer},
		{"link", "set", stale, "addrgenmode", "none"},
		{"link", "set", peer, "addrgenmode", "none"},
		{"link", "set", stale, "master", "br-test"},
		{"link", "set", stale, "up"},
		{"link", "add", "keep0", "type", "veth", "peer", "name", "keep1"},
		{"link", "set", "keep0", "master", "br-test"},
		{"link", "set", "keep0", "up"},
		{"link", "set", "keep1", "up"},
	} {
		if _, err := command(ctx, ip, args...); err != nil {
			t.Fatal(err)
		}
	}
	links, err := backend.host(ctx)
	if err != nil {
		t.Fatal(err)
	}
	addressed, err := backend.addressed(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pairs := cleanupCandidates(links, addressed, map[string]string{"br-test": "network"}, map[string]bool{})
	expected, exists := pairs[stale]
	if !exists || len(pairs) != 1 {
		t.Fatalf("kernel orphan not recognized: %+v links=%+v", pairs, links)
	}
	if _, err := command(ctx, ip, "address", "add", "192.0.2.2/32", "dev", peer); err != nil {
		t.Fatal(err)
	}
	if err := backend.deletePair(ctx, expected); err == nil {
		t.Fatal("final address recheck allowed deletion")
	}
	if _, err := command(ctx, ip, "address", "del", "192.0.2.2/32", "dev", peer); err != nil {
		t.Fatal(err)
	}
	changed := expected
	changed.Peer.Address = "02:00:00:00:00:99"
	if err := backend.deletePair(ctx, changed); err == nil {
		t.Fatal("final identity recheck allowed deletion")
	}
	if err := backend.deletePair(ctx, expected); err != nil {
		t.Fatal(err)
	}
	after, err := backend.host(ctx)
	if err != nil {
		t.Fatal(err)
	}
	kept := map[string]bool{}
	for _, link := range after {
		kept[link.Name] = true
	}
	if kept[stale] || kept[peer] || !kept["br-test"] || !kept["keep0"] || !kept["keep1"] {
		t.Fatalf("wrong kernel links removed: %+v", after)
	}
	testPeriodicCleanupDaemon(t, ctx, backend, ip, stale, peer)
}

func testPeriodicCleanupDaemon(t *testing.T, ctx context.Context, backend nativeLinks, ip, stale, peer string) {
	t.Helper()
	for _, args := range [][]string{
		{"link", "add", stale, "type", "veth", "peer", "name", peer},
		{"link", "set", stale, "addrgenmode", "none"},
		{"link", "set", peer, "addrgenmode", "none"},
		{"link", "set", stale, "master", "br-test"},
		{"link", "set", stale, "up"},
	} {
		if _, err := command(ctx, ip, args...); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	socket := filepath.Join(dir, "docker.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/containers/json":
			io.WriteString(w, `[]`)
		case "/networks":
			io.WriteString(w, `[{"Id":"network"}]`)
		case "/networks/network":
			io.WriteString(w, `{"Id":"network","Driver":"ghcr.io/claymore666/docker-net-dhcp:v2.3.1","Options":{"bridge":"br-test","mode":"bridge"},"Containers":{}}`)
		case "/events":
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		default:
			http.NotFound(w, r)
		}
	})}
	serverErrors := make(chan error, 1)
	go func() { serverErrors <- server.Serve(listener) }()
	defer func() {
		if err := server.Close(); err != nil {
			t.Errorf("close fake Docker server: %v", err)
		}
		if err := <-serverErrors; err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("fake Docker server failed: %v", err)
		}
	}()
	config := filepath.Join(dir, "manager.yaml")
	if err := os.WriteFile(config, []byte("renaming:\n  enabled: false\norphan_cleanup:\n  enabled: true\n  interval: 1s\n  min_age: 1s\n"), 0600); err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.CommandContext(ctx, binary, "-test.run=^TestSandoSyncCLIHelper$", "--",
		"--socket="+socket, "--manager-config="+config, "--vnstat=off", "--interval=1s")
	child.Env = append(os.Environ(), "VETH_SANDO_CLI_TEST=1")
	var output bytes.Buffer
	child.Stdout, child.Stderr = &output, &output
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := child.Process.Signal(syscall.SIGTERM); err != nil {
			t.Errorf("stop test daemon: %v", err)
		}
		if err := child.Wait(); err != nil {
			t.Errorf("test daemon failed: %v\n%s", err, output.String())
		}
	}()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		links, err := backend.host(ctx)
		if err != nil {
			t.Fatal(err)
		}
		present := map[string]bool{}
		for _, link := range links {
			present[link.Name] = true
		}
		if !present[stale] && !present[peer] {
			if !present["br-test"] || !present["keep0"] || !present["keep1"] {
				t.Fatal("periodic daemon cleanup removed unrelated links")
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("periodic daemon did not clean the observed orphan pair")
}
