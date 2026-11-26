//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

func openRouteHandle(pid int) (*netlink.Handle, error) {
	if pid <= 0 {
		return netlink.NewHandle(unix.NETLINK_ROUTE)
	}
	type result struct {
		handle *netlink.Handle
		err    error
	}
	done := make(chan result, 1)
	go func() {
		var r result
		defer func() { done <- r }()
		runtime.LockOSThread()
		restored := true
		defer func() {
			// A failed restoration must retire this thread, not return it to Go's pool.
			if restored {
				runtime.UnlockOSThread()
			}
		}()
		current, err := netns.Get()
		if err != nil {
			r.err = fmt.Errorf("open current network namespace: %w", err)
			return
		}
		defer func() { r.err = errors.Join(r.err, current.Close()) }()
		target, err := netns.GetFromPid(pid)
		if err != nil {
			r.err = fmt.Errorf("open network namespace for PID %d: %w", pid, err)
			return
		}
		defer func() { r.err = errors.Join(r.err, target.Close()) }()
		if err := netns.Set(target); err != nil {
			r.err = fmt.Errorf("enter network namespace for PID %d: %w", pid, err)
			return
		}
		restored = false
		r.handle, r.err = netlink.NewHandle(unix.NETLINK_ROUTE)
		if err := netns.Set(current); err != nil {
			r.err = errors.Join(r.err, fmt.Errorf("restore network namespace: %w", err))
		} else {
			restored = true
		}
	}()
	r := <-done
	if r.err != nil && r.handle != nil {
		r.handle.Close()
		r.handle = nil
	}
	return r.handle, r.err
}

func routeTimeout(ctx context.Context, h *netlink.Handle) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	timeout := 10 * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < timeout {
			timeout = remaining
		}
	}
	if timeout < time.Microsecond {
		return context.DeadlineExceeded
	}
	return h.SetSocketTimeout(timeout)
}

func withRouteHandle(ctx context.Context, pid int, run func(*netlink.Handle) error) error {
	if pid < 0 {
		return fmt.Errorf("invalid network namespace PID %d", pid)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	h, err := openRouteHandle(pid)
	if err != nil {
		return err
	}
	defer h.Close()
	if err := routeTimeout(ctx, h); err != nil {
		return err
	}
	if err := run(h); err != nil {
		return fmt.Errorf("rtnetlink (PID %d): %w", pid, err)
	}
	return ctx.Err()
}

func nativeLinkRecords(links []netlink.Link) ([]link, error) {
	names := make(map[int]string, len(links))
	for _, item := range links {
		names[item.Attrs().Index] = item.Attrs().Name
	}
	records := make([]link, 0, len(links))
	for _, item := range links {
		a := item.Attrs()
		if a.Index <= 0 || a.Name == "" {
			return nil, fmt.Errorf("rtnetlink returned an invalid interface identity")
		}
		record := link{Index: a.Index, Name: a.Name, Peer: a.ParentIndex,
			Address: a.HardwareAddr.String(), AltNames: a.AltNames}
		record.Info.Kind = item.Type()
		if a.MasterIndex != 0 {
			var exists bool
			record.Master, exists = names[a.MasterIndex]
			if !exists {
				return nil, fmt.Errorf("interface %q has an unknown master index %d", a.Name, a.MasterIndex)
			}
		}
		if a.NetNsID >= 0 {
			id := a.NetNsID
			record.NetNSID = &id
		}
		record.Flags = []string{}
		for _, flag := range []struct {
			mask uint32
			name string
		}{
			{unix.IFF_LOOPBACK, "LOOPBACK"}, {unix.IFF_BROADCAST, "BROADCAST"},
			{unix.IFF_POINTOPOINT, "POINTOPOINT"}, {unix.IFF_MULTICAST, "MULTICAST"},
			{unix.IFF_UP, "UP"}, {unix.IFF_LOWER_UP, "LOWER_UP"}, {unix.IFF_DORMANT, "DORMANT"},
		} {
			if a.RawFlags&flag.mask != 0 {
				record.Flags = append(record.Flags, flag.name)
			}
		}
		if a.RawFlags&unix.IFF_UP != 0 && a.RawFlags&unix.IFF_RUNNING == 0 {
			record.Flags = append(record.Flags, "NO-CARRIER")
		}
		records = append(records, record)
	}
	return records, nil
}

func routeLinks(ctx context.Context, h *netlink.Handle) ([]link, error) {
	if err := routeTimeout(ctx, h); err != nil {
		return nil, err
	}
	items, err := h.LinkList()
	if err != nil {
		return nil, err
	}
	return nativeLinkRecords(items)
}

func (l nativeLinks) host(ctx context.Context) ([]link, error) {
	var links []link
	err := withRouteHandle(ctx, 0, func(h *netlink.Handle) (err error) {
		links, err = routeLinks(ctx, h)
		return err
	})
	return links, err
}

func (l nativeLinks) namespace(ctx context.Context, pid int) ([]link, error) {
	if pid <= 0 {
		return nil, fmt.Errorf("invalid network namespace PID %d", pid)
	}
	var links []link
	err := withRouteHandle(ctx, pid, func(h *netlink.Handle) (err error) {
		links, err = routeLinks(ctx, h)
		return err
	})
	return links, err
}

func (l nativeLinks) addressed(ctx context.Context) (map[int]bool, error) {
	found := map[int]bool{}
	err := withRouteHandle(ctx, 0, func(h *netlink.Handle) error {
		links, err := routeLinks(ctx, h)
		if err != nil {
			return err
		}
		for _, item := range links {
			found[item.Index] = false
		}
		if err := routeTimeout(ctx, h); err != nil {
			return err
		}
		addresses, err := h.AddrList(nil, netlink.FAMILY_ALL)
		if err != nil {
			return err
		}
		for _, address := range addresses {
			if _, exists := found[address.LinkIndex]; !exists {
				return fmt.Errorf("address has an unknown interface index %d", address.LinkIndex)
			}
			found[address.LinkIndex] = true
		}
		return nil
	})
	return found, err
}

func (l nativeLinks) mutate(ctx context.Context, pid int, expected *link, name, action, value string) error {
	return withRouteHandle(ctx, pid, func(h *netlink.Handle) error {
		links, err := routeLinks(ctx, h)
		if err != nil {
			return err
		}
		var target netlink.Link
		for _, current := range links {
			if current.Name == name && (expected == nil || sameLink(current, *expected)) {
				target = &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Index: current.Index, Name: current.Name}}
				break
			}
		}
		if target == nil {
			return fmt.Errorf("interface identity changed before %s %q; refusing mutation", action, value)
		}
		if err := routeTimeout(ctx, h); err != nil {
			return err
		}
		switch action {
		case "name":
			return h.LinkSetName(target, value)
		case "add-altname":
			return h.LinkAddAltName(target, value)
		case "del-altname":
			return h.LinkDelAltName(target, value)
		case "state":
			if value == "up" {
				return h.LinkSetUp(target)
			}
			if value == "down" {
				return h.LinkSetDown(target)
			}
		case "delete":
			return h.LinkDel(target)
		}
		return fmt.Errorf("unsupported link operation %q %q", action, value)
	})
}
