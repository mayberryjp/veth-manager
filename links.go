package main

import (
	"context"
	"fmt"
	"os/exec"
	"reflect"
	"strings"
	"time"
)

type link struct {
	Index    int      `json:"ifindex"`
	Name     string   `json:"ifname"`
	Peer     int      `json:"link_index"`
	Address  string   `json:"address"`
	Master   string   `json:"master"`
	AltNames []string `json:"altnames"`
	PeerName string   `json:"link"`
	Flags    []string `json:"flags"`
	NetNSID  *int     `json:"link_netnsid"`
	Info     struct {
		Kind string `json:"info_kind"`
	} `json:"linkinfo"`
}

type linkAPI interface {
	host(context.Context) ([]link, error)
	namespace(context.Context, int) ([]link, error)
	apply(context.Context, int, string, string, string) error
}

type nativeLinks struct{}

func command(ctx context.Context, program string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, program, args...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", program, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func (l nativeLinks) deletePair(ctx context.Context, expected dhcpPair) error {
	links, err := l.host(ctx)
	if err != nil {
		return err
	}
	addressed, err := l.addressed(ctx)
	if err != nil {
		return err
	}
	pairs := cleanupCandidates(links, addressed,
		map[string]string{expected.BridgeEnd.Master: expected.NetworkID}, map[string]bool{})
	if actual, exists := pairs[expected.BridgeEnd.Name]; !exists || !reflect.DeepEqual(actual, expected) {
		return fmt.Errorf("interface identity changed immediately before deletion")
	}
	if err := l.mutate(ctx, 0, &expected.BridgeEnd, expected.BridgeEnd.Name, "delete", ""); err != nil {
		return err
	}
	after, err := l.host(ctx)
	if err != nil {
		return fmt.Errorf("deletion succeeded but verification failed: %w", err)
	}
	for _, actual := range after {
		if actual.Index == expected.BridgeEnd.Index || actual.Index == expected.Peer.Index ||
			actual.Name == expected.BridgeEnd.Name || actual.Name == expected.Peer.Name {
			return fmt.Errorf("orphan pair removal was not verified")
		}
	}
	return nil
}

func (l nativeLinks) apply(ctx context.Context, pid int, current, desired, mode string) error {
	if err := validName(desired); err != nil {
		return err
	}
	action := "name"
	if mode == "alternative" {
		action = "add-altname"
	}
	return l.mutate(ctx, pid, nil, current, action, desired)
}

func sameLink(current, expected link) bool {
	return current.Index == expected.Index && current.Name == expected.Name &&
		current.Peer == expected.Peer && current.PeerName == expected.PeerName &&
		current.Master == expected.Master && current.Info.Kind == expected.Info.Kind &&
		strings.EqualFold(current.Address, expected.Address)
}

func (l nativeLinks) change(ctx context.Context, expected link, action, value string) error {
	switch action {
	case "state":
		if value != "up" && value != "down" {
			return fmt.Errorf("invalid link state %q", value)
		}
	case "name", "del-altname", "add-altname":
		if err := validName(value); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported repair operation %q", action)
	}
	return l.mutate(ctx, 0, &expected, expected.Name, action, value)
}

func ownsName(l link, name string) bool {
	if l.Name == name {
		return true
	}
	for _, alt := range l.AltNames {
		if alt == name {
			return true
		}
	}
	return false
}

func findPeer(inner, host []link, mac, bridge string) (link, error) {
	if mac == "" {
		return link{}, fmt.Errorf("Docker endpoint has no MAC address yet")
	}
	var matches []link
	for _, in := range inner {
		if !strings.EqualFold(in.Address, mac) || in.Info.Kind != "veth" || in.Peer <= 0 {
			continue
		}
		for _, h := range host {
			if h.Index == in.Peer && h.Peer == in.Index && h.Info.Kind == "veth" && h.Master == bridge {
				matches = append(matches, h)
			}
		}
	}
	if len(matches) != 1 {
		return link{}, fmt.Errorf("expected one reciprocal veth peer on bridge %q for MAC %s; found %d", bridge, mac, len(matches))
	}
	return matches[0], nil
}

func findMacvlan(inner, host []link, mac, parent string) (link, error) {
	if mac == "" {
		return link{}, fmt.Errorf("Docker endpoint has no MAC address yet")
	}
	if parent == "" {
		return link{}, fmt.Errorf("macvlan network has no parent interface configured")
	}
	var parents []link
	for _, h := range host {
		if ownsName(h, parent) && h.Index > 0 {
			parents = append(parents, h)
		}
	}
	if len(parents) != 1 {
		return link{}, fmt.Errorf("expected one host parent interface %q; found %d", parent, len(parents))
	}
	var matches []link
	for _, in := range inner {
		if strings.EqualFold(in.Address, mac) && in.Info.Kind == "macvlan" && in.Peer == parents[0].Index {
			matches = append(matches, in)
		}
	}
	if len(matches) != 1 {
		return link{}, fmt.Errorf("expected one macvlan interface on parent %q for MAC %s; found %d", parent, mac, len(matches))
	}
	return matches[0], nil
}
