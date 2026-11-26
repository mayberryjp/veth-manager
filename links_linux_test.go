//go:build linux

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"reflect"
	"syscall"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

func TestNativeLinkRecords(t *testing.T) {
	bridge := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Index: 10, Name: "br-test", NetNsID: -1}}
	veth := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{
		Index: 20, Name: "veth0", ParentIndex: 21, MasterIndex: 10, NetNsID: -1,
		HardwareAddr: net.HardwareAddr{2, 0, 0, 0, 0, 1}, AltNames: []string{"veth_app"},
		RawFlags: unix.IFF_BROADCAST | unix.IFF_MULTICAST | unix.IFF_UP,
	}}
	peer := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Index: 21, Name: "peer0", ParentIndex: 20, NetNsID: -1}}
	records, err := nativeLinkRecords([]netlink.Link{veth, peer, bridge})
	if err != nil {
		t.Fatal(err)
	}
	record := records[0]
	if record.Index != 20 || record.Peer != 21 || record.Master != "br-test" ||
		record.Info.Kind != "veth" || record.Address != "02:00:00:00:00:01" ||
		record.NetNSID != nil || !ownsName(record, "veth_app") ||
		!hasFlag(record, "UP") || !hasFlag(record, "NO-CARRIER") || hasFlag(record, "LOWER_UP") {
		t.Fatalf("lost kernel identity or carrier flags: %+v", record)
	}
	veth.NetNsID = 0
	veth.RawFlags |= unix.IFF_RUNNING | unix.IFF_LOWER_UP
	records, err = nativeLinkRecords([]netlink.Link{veth, peer, bridge})
	if err != nil || records[0].NetNSID == nil || *records[0].NetNSID != 0 ||
		hasFlag(records[0], "NO-CARRIER") || !hasFlag(records[0], "LOWER_UP") {
		t.Fatalf("lost namespace or live carrier: %+v %v", records, err)
	}
	if _, err := nativeLinkRecords([]netlink.Link{veth}); err == nil {
		t.Fatal("accepted an unresolved master from an inconsistent snapshot")
	}
}

func TestNativeLinksRejectCanceledAndInvalidRequests(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	backend := nativeLinks{}
	if _, err := backend.host(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled discovery: %v", err)
	}
	if err := backend.apply(ctx, 0, "lo", "newname", "primary"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled mutation: %v", err)
	}
	if _, err := backend.namespace(context.Background(), 0); err == nil {
		t.Fatal("invalid PID selected the host namespace")
	}
	if err := backend.apply(context.Background(), -1, "lo", "newname", "primary"); err == nil {
		t.Fatal("negative PID selected the host namespace")
	}
	if err := backend.change(context.Background(), link{Name: "lo"}, "state", "invalid"); err == nil {
		t.Fatal("invalid state was accepted")
	}
}

func TestNativeNamespaceHelper(t *testing.T) {
	if os.Getenv("NATIVE_NAMESPACE_HELPER") != "1" {
		t.Skip("subprocess fixture")
	}
	if err := netlink.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "inside0"}}); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(os.Stdout, "ready")
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		t.Fatal(err)
	}
}

func TestNativeLinksKernelIntegration(t *testing.T) {
	if os.Getenv("NATIVE_LINKS_INTEGRATION") != "1" {
		t.Skip("requires Linux root and NATIVE_LINKS_INTEGRATION=1; uses isolated network namespaces")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("NATIVE_LINKS_ISOLATED") != "1" {
		child := exec.CommandContext(ctx, binary, "-test.run=^TestNativeLinksKernelIntegration$", "-test.v")
		child.SysProcAttr = &syscall.SysProcAttr{Cloneflags: unix.CLONE_NEWNET}
		child.Env = append(os.Environ(), "NATIVE_LINKS_ISOLATED=1")
		output, err := child.CombinedOutput()
		if err != nil {
			t.Fatalf("isolated native test: %v\n%s", err, output)
		}
		t.Logf("%s", output)
		return
	}
	// No ip/nsenter (or any other executable) can be found during backend operations.
	t.Setenv("PATH", "")
	backend := nativeLinks{}
	bridge := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br-native"}}
	if err := netlink.LinkAdd(bridge); err != nil {
		t.Fatal(err)
	}
	veth := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "native0"}, PeerName: "native1"}
	if err := netlink.LinkAdd(veth); err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetMaster(veth, bridge); err != nil {
		t.Fatal(err)
	}
	find := func(pid int, name string) link {
		t.Helper()
		var records []link
		var err error
		if pid == 0 {
			records, err = backend.host(ctx)
		} else {
			records, err = backend.namespace(ctx, pid)
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, record := range records {
			if record.Name == name {
				return record
			}
		}
		t.Fatalf("interface %q missing in PID %d: %+v", name, pid, records)
		return link{}
	}
	original := find(0, "native0")
	if original.Master != "br-native" || original.Peer != find(0, "native1").Index {
		t.Fatalf("incorrect bridge/veth metadata: %+v", original)
	}
	wrong := original
	wrong.Address = "02:00:00:00:00:99"
	if err := backend.change(ctx, wrong, "name", "wrong0"); err == nil {
		t.Fatal("identity mismatch allowed a mutation")
	}
	if err := backend.change(ctx, original, "name", "renamed0"); err != nil {
		t.Fatal(err)
	}
	renamed := find(0, "renamed0")
	if renamed.Index != original.Index {
		t.Fatal("rename changed interface identity")
	}
	if err := backend.apply(ctx, 0, "renamed0", "native_alias", "alternative"); err != nil {
		t.Fatal(err)
	}
	if !ownsName(find(0, "renamed0"), "native_alias") {
		t.Fatal("alternative name was not added")
	}
	if err := backend.change(ctx, renamed, "del-altname", "native_alias"); err != nil {
		t.Fatal(err)
	}
	if ownsName(find(0, "renamed0"), "native_alias") {
		t.Fatal("alternative name was not removed")
	}
	for _, state := range []string{"up", "down"} {
		if err := backend.change(ctx, find(0, "renamed0"), "state", state); err != nil {
			t.Fatal(err)
		}
		if hasFlag(find(0, "renamed0"), "UP") != (state == "up") {
			t.Fatalf("state %q was not applied", state)
		}
	}
	addr, err := netlink.ParseAddr("192.0.2.1/32")
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.AddrAdd(veth, addr); err != nil {
		t.Fatal(err)
	}
	addressed, err := backend.addressed(ctx)
	if err != nil || !addressed[original.Index] {
		t.Fatalf("addressed link not recognized: %v %v", addressed, err)
	}
	peer := find(0, "native1")
	if _, exists := addressed[peer.Index]; !exists {
		t.Fatal("unaddressed peer missing from address inventory")
	}

	child := exec.CommandContext(ctx, binary, "-test.run=^TestNativeNamespaceHelper$")
	child.SysProcAttr = &syscall.SysProcAttr{Cloneflags: unix.CLONE_NEWNET}
	child.Env = append(os.Environ(), "NATIVE_NAMESPACE_HELPER=1")
	stdin, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stdin.Close()
		if err := child.Wait(); err != nil {
			t.Errorf("namespace fixture: %v", err)
		}
	}()
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() || scanner.Text() != "ready" {
		t.Fatalf("namespace fixture did not become ready: %q %v", scanner.Text(), scanner.Err())
	}
	if err := netlink.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "inside0"}}); err != nil {
		t.Fatal(err)
	}
	macvlan := &netlink.Macvlan{LinkAttrs: netlink.LinkAttrs{
		Name: "macnative0", ParentIndex: find(0, "inside0").Index,
		HardwareAddr: net.HardwareAddr{2, 0, 0, 0, 0, 2},
	}, Mode: netlink.MACVLAN_MODE_BRIDGE}
	if err := netlink.LinkAdd(macvlan); err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetNsPid(macvlan, child.Process.Pid); err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetNsPid(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Index: peer.Index}}, child.Process.Pid); err != nil {
		t.Fatal(err)
	}
	hostLinks, err := backend.host(ctx)
	if err != nil {
		t.Fatal(err)
	}
	containerLinks, err := backend.namespace(ctx, child.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := findMacvlan(containerLinks, hostLinks, "02:00:00:00:00:02", "inside0"); err != nil {
		t.Fatalf("native macvlan parent discovery: %v", err)
	}
	if _, err := findPeer(containerLinks, hostLinks, peer.Address, "br-native"); err != nil {
		t.Fatalf("native cross-namespace veth discovery: %v", err)
	}
	if find(0, "renamed0").NetNSID == nil {
		t.Fatal("cross-namespace peer was incorrectly reported as host-local")
	}
	if err := backend.apply(ctx, child.Process.Pid, "macnative0", "maccontainer0", "primary"); err != nil {
		t.Fatal(err)
	}
	find(child.Process.Pid, "maccontainer0")
	before, err := backend.host(ctx)
	if err != nil {
		t.Fatal(err)
	}
	inside := find(child.Process.Pid, "inside0")
	if err := backend.apply(ctx, child.Process.Pid, "inside0", "container0", "primary"); err != nil {
		t.Fatal(err)
	}
	if find(child.Process.Pid, "container0").Index != inside.Index {
		t.Fatal("container rename changed identity")
	}
	if err := backend.apply(ctx, child.Process.Pid, "container0", "container_alias", "alternative"); err != nil {
		t.Fatal(err)
	}
	if !ownsName(find(child.Process.Pid, "container0"), "container_alias") {
		t.Fatal("container alternative name was not added")
	}
	after, err := backend.host(ctx)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("container operations changed the host namespace: before=%+v after=%+v err=%v", before, after, err)
	}
	originalNS, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	defer originalNS.Close()
	for i := 0; i < 5; i++ {
		find(child.Process.Pid, "container0")
	}
	currentNS, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	defer currentNS.Close()
	if !originalNS.Equal(currentNS) {
		t.Fatal("namespace access leaked onto a caller thread")
	}
}
