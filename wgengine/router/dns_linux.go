// Copyright (c) 2020 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package router

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"
	"inet.af/netaddr"
	"tailscale.com/net/interfaces"
)

// systemdSetTimeout is the time interval within which
// all operations in systemdUpDNS must complete.
//
// This is useful because certain conditions (such as improper dbus auth)
// can cause a context-less Call to hang indefinitely.
const systemdSetTimeout = time.Second

// systemdStubPaths are the locations of systemd resolv.conf stubs.
//
// The official documentation mentions, in roughly descending order of goodness:
// 1. /usr/lib/systemd/resolv.conf
// 2. /var/run/systemd/resolve/stun-resolv.conf
// 3. /var/run/systemd/resolve/resolv.conf
// Our approach here does not support (3): it does not proxy requests
// through resolved, instead trying to figure out what the "best" global resolver is.
// This is probably not useful for us: a link can request priority with
//   SetLinkDomains([]{{"~.", true}, ...})
// but in practice other links do this too.
// At best, (3) ends up being a flat list of nameservers from all links.
// This does not work for us, as there is a possibility of getting NXDOMAIN
// from another server before we are asked or get a chance to respond.
// We consider this case as lacking systemd support and fall through to replaceResolvConf.
//
// As for (1) and (2), we include the literal paths and their variants
// to account for /lib being symlinked to /usr/lib and /var/run to /run.
var systemdStubPaths = []string{
	"/lib/systemd/resolv.conf",
	"/usr/lib/systemd/resolv.conf",
	"/run/systemd/resolve/stub-resolv.conf",
	"/var/run/systemd/resolve/stub-resolv.conf",
}

var (
	errNotSystemd = errors.New("systemd-resolved is not in use")
	errNotReady   = errors.New("interface not ready")
)

type systemdLinkNameserver struct {
	Family  int
	Address []byte
}

type systemdLinkDomain struct {
	Domain      string
	RoutingOnly bool
}

// systemdIsActive determines if systemd is currently managing system DNS settings.
func systemdIsActive() bool {
	dst, err := os.Readlink("/etc/resolv.conf")
	if err != nil {
		return false
	}

	for _, path := range systemdStubPaths {
		if dst == path {
			return true
		}
	}

	return false
}

// systemdUpDNS sets the DNS parameters for the Tailscale interface
// to given nameservers and search domains.
func systemdUpDNS(servers []netaddr.IP, domains []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), systemdSetTimeout)
	defer cancel()

	if !systemdIsActive() {
		return errNotSystemd
	}

	conn, err := dbus.SystemBus()
	if err != nil {
		return fmt.Errorf("connecting to system bus: %w", err)
	}

	resolved := conn.Object(
		"org.freedesktop.resolve1",
		dbus.ObjectPath("/org/freedesktop/resolve1"),
	)

	_, iface, err := interfaces.Tailscale()
	if err != nil {
		return fmt.Errorf("getting interface index: %w", err)
	}
	if iface == nil {
		return errNotReady
	}

	var linkNameservers = make([]systemdLinkNameserver, len(servers))
	for i, server := range servers {
		ip := server.As16()
		if server.Is4() {
			linkNameservers[i] = systemdLinkNameserver{
				Family:  unix.AF_INET,
				Address: ip[12:],
			}
		} else {
			linkNameservers[i] = systemdLinkNameserver{
				Family:  unix.AF_INET6,
				Address: ip[:],
			}
		}
	}

	call := resolved.CallWithContext(
		ctx, "org.freedesktop.resolve1.Manager.SetLinkDNS", 0,
		iface.Index, linkNameservers,
	)
	if call.Err != nil {
		return fmt.Errorf("SetLinkDNS: %w", call.Err)
	}

	var linkDomains = make([]systemdLinkDomain, len(domains))
	for i, domain := range domains {
		linkDomains[i] = systemdLinkDomain{
			Domain:      domain,
			RoutingOnly: false,
		}
	}

	call = resolved.CallWithContext(
		ctx, "org.freedesktop.resolve1.Manager.SetLinkDomains", 0,
		iface.Index, linkDomains,
	)
	if call.Err != nil {
		return fmt.Errorf("SetLinkDomains: %w", call.Err)
	}

	return nil
}

// systemdDownDNS undoes the changes made by systemdUpDNS.
func systemdDownDNS() error {
	ctx, cancel := context.WithTimeout(context.Background(), systemdSetTimeout)
	defer cancel()

	if !systemdIsActive() {
		return errNotSystemd
	}

	conn, err := dbus.SystemBus()
	if err != nil {
		return fmt.Errorf("connecting to system bus: %w", err)
	}

	resolved := conn.Object(
		"org.freedesktop.resolve1",
		dbus.ObjectPath("/org/freedesktop/resolve1"),
	)

	_, iface, err := interfaces.Tailscale()
	if err != nil {
		return fmt.Errorf("getting interface index: %w", err)
	}
	if iface == nil {
		return errNotReady
	}

	call := resolved.CallWithContext(ctx, "org.freedesktop.resolve1.Manager.RevertLink", 0, iface.Index)
	if call.Err != nil {
		return fmt.Errorf("RevertLink: %w", call.Err)
	}

	return nil
}
