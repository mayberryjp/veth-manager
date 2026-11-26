package main

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"
)

type cleanupDockerAPI interface {
	allContainers(context.Context) ([]string, error)
	networkIDs(context.Context) ([]string, error)
	inspect(context.Context, string) (container, error)
	network(context.Context, string) (network, error)
}

type cleanupLinkAPI interface {
	host(context.Context) ([]link, error)
	addressed(context.Context) (map[int]bool, error)
	deletePair(context.Context, dhcpPair) error
}

type dhcpPair struct {
	BridgeEnd link
	Peer      link
	NetworkID string
}

type orphanObservation struct {
	pair  dhcpPair
	first time.Time
}

type orphanCleaner struct {
	docker cleanupDockerAPI
	links  cleanupLinkAPI
	minAge time.Duration
	dryRun bool
	seen   map[string]orphanObservation
}

type cleanupResult struct {
	Candidates int
	Eligible   int
	Deleted    []dhcpPair
	Planned    []dhcpPair
}

func (c *orphanCleaner) reset() {
	c.seen = nil
}

func (c *orphanCleaner) ownership(ctx context.Context) (map[string]string, map[string]bool, error) {
	ids, err := c.docker.networkIDs(ctx)
	if err != nil {
		return nil, nil, err
	}
	bridges, protected := map[string]string{}, map[string]bool{}
	bridgeOwners := map[string]string{}
	protect := func(id string) {
		if len(id) >= 12 {
			protected[strings.ToLower(id[:12])] = true
		}
	}
	for _, id := range ids {
		n, err := c.docker.network(ctx, id)
		if err != nil {
			return nil, nil, err
		}
		if n.ID != id {
			return nil, nil, fmt.Errorf("Docker network identity changed during cleanup")
		}
		if networkKind(n) == "bridge" {
			bridge, err := bridgeName(n)
			if err != nil {
				return nil, nil, err
			}
			if previous, exists := bridgeOwners[bridge]; exists && previous != id {
				return nil, nil, fmt.Errorf("multiple Docker networks share bridge %q; refusing cleanup", bridge)
			}
			bridgeOwners[bridge] = id
		}
		if isDHCPNetwork(n) && networkKind(n) == "bridge" {
			bridge, err := bridgeName(n)
			if err != nil {
				return nil, nil, err
			}
			bridges[bridge] = id
		}
		for containerID, ep := range n.Containers {
			protect(containerID)
			protect(ep.EndpointID)
		}
	}
	ids, err = c.docker.allContainers(ctx)
	if err != nil {
		return nil, nil, err
	}
	for _, id := range ids {
		container, err := c.docker.inspect(ctx, id)
		if err != nil {
			return nil, nil, err
		}
		if container.ID != id {
			return nil, nil, fmt.Errorf("Docker container identity changed during cleanup")
		}
		protect(container.ID)
		for _, ep := range container.NetworkSettings.Networks {
			protect(ep.EndpointID)
		}
	}
	return bridges, protected, nil
}

func disconnectedDHCPPair(links []link, stale link, addressed map[int]bool) (dhcpPair, bool) {
	peer, disconnected := disconnectedHostVethPeer(links, stale)
	_, staleAddressesKnown := addressed[stale.Index]
	_, peerAddressesKnown := addressed[peer.Index]
	if !disconnected ||
		!staleAddressesKnown || !peerAddressesKnown || addressed[stale.Index] || addressed[peer.Index] {
		return dhcpPair{}, false
	}
	identity, ok := dhcpEndpointIdentity(peer.Name)
	if !ok || stale.Name != "dh-"+identity {
		return dhcpPair{}, false
	}
	return dhcpPair{BridgeEnd: stale, Peer: peer}, true
}

func cleanupCandidates(links []link, addressed map[int]bool, bridges map[string]string, protected map[string]bool) map[string]dhcpPair {
	found := map[string]dhcpPair{}
	for _, stale := range links {
		networkID, exists := bridges[stale.Master]
		if !exists {
			continue
		}
		pair, ok := disconnectedDHCPPair(links, stale, addressed)
		if !ok || protected[strings.ToLower(strings.TrimPrefix(stale.Name, "dh-"))] {
			continue
		}
		pair.NetworkID = networkID
		found[stale.Name] = pair
	}
	return found
}

func (c *orphanCleaner) candidates(ctx context.Context) (map[string]dhcpPair, error) {
	bridges, protected, err := c.ownership(ctx)
	if err != nil {
		return nil, fmt.Errorf("cleanup Docker discovery: %w", err)
	}
	links, err := c.links.host(ctx)
	if err != nil {
		return nil, err
	}
	addressed, err := c.links.addressed(ctx)
	if err != nil {
		return nil, err
	}
	return cleanupCandidates(links, addressed, bridges, protected), nil
}

func (c *orphanCleaner) revalidate(ctx context.Context) error {
	pairs, err := c.candidates(ctx)
	if err != nil {
		c.reset()
		return err
	}
	for name, observation := range c.seen {
		if pair, exists := pairs[name]; !exists || !reflect.DeepEqual(pair, observation.pair) {
			delete(c.seen, name)
		}
	}
	return nil
}

func (c *orphanCleaner) run(ctx context.Context, now time.Time, deleted func(dhcpPair)) (result cleanupResult, problem error) {
	defer func() {
		if problem != nil {
			c.reset()
		}
	}()
	pairs, err := c.candidates(ctx)
	if err != nil {
		return result, err
	}
	next := map[string]orphanObservation{}
	var eligible []string
	for name, pair := range pairs {
		previous, exists := c.seen[name]
		if !exists || !reflect.DeepEqual(previous.pair, pair) {
			next[name] = orphanObservation{pair: pair, first: now}
			continue
		}
		next[name] = previous
		if now.Sub(previous.first) >= c.minAge {
			eligible = append(eligible, name)
		}
	}
	c.seen = next
	result.Candidates, result.Eligible = len(pairs), len(eligible)
	sort.Strings(eligible)
	for _, name := range eligible {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if c.dryRun {
			result.Planned = append(result.Planned, pairs[name])
			continue
		}
		// Refresh ownership for each deletion, including unlabeled and stopped containers.
		fresh, err := c.candidates(ctx)
		if err != nil {
			return result, err
		}
		expected := pairs[name]
		actual, exists := fresh[name]
		if !exists || !reflect.DeepEqual(actual, expected) {
			return result, fmt.Errorf("orphan pair %q changed before deletion; refusing cleanup", name)
		}
		if err := c.links.deletePair(ctx, expected); err != nil {
			return result, fmt.Errorf("delete orphan pair %q: %w", name, err)
		}
		delete(c.seen, name)
		result.Deleted = append(result.Deleted, expected)
		deleted(expected)
	}
	return result, nil
}
