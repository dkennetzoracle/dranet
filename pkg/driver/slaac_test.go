/*
Copyright The Kubernetes Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package driver

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	userns "sigs.k8s.io/dranet/internal/testutils"
	"sigs.k8s.io/dranet/pkg/apis"
)

// testRouterAdvertisement builds an ICMPv6 Router Advertisement (RFC 4861
// section 4.2) with the given router lifetime and options. The kernel fills in
// the checksum when it is sent through a raw ICMPv6 socket.
func testRouterAdvertisement(routerLifetime uint16, options ...[]byte) []byte {
	ra := make([]byte, 16)
	ra[0] = icmpv6RouterAdvertisement
	ra[4] = 64 // current hop limit
	binary.BigEndian.PutUint16(ra[6:8], routerLifetime)
	for _, option := range options {
		ra = append(ra, option...)
	}
	return ra
}

// testPrefixInformation builds a Prefix Information option (RFC 4861 section
// 4.6.2).
func testPrefixInformation(prefix string, length, flags byte, valid, preferred uint32) []byte {
	option := make([]byte, 32)
	option[0] = ndpOptionPrefixInformation
	option[1] = 4 // units of 8 octets
	option[2] = length
	option[3] = flags
	binary.BigEndian.PutUint32(option[4:8], valid)
	binary.BigEndian.PutUint32(option[8:12], preferred)
	addr := netip.MustParseAddr(prefix).As16()
	copy(option[16:32], addr[:])
	return option
}

const (
	pioOnLink     = 0x80
	pioAutonomous = 0x40
)

// The address is a host address in the prefix. It stays the same for the same
// claim, device and prefix, also across releases, since a claim prepared again
// must get the address it has, and its interface identifier changes when any
// of them changes.
func TestSLAACAddress(t *testing.T) {
	prefix := netip.MustParsePrefix("2001:db8:1::/64")
	got := slaacAddress(prefix, "claim-uid", "rdma0")
	if want := netip.MustParsePrefix("2001:db8:1:0:7742:a0fa:8b20:5407/64"); got != want {
		t.Errorf("slaacAddress() = %s, want %s", got, want)
	}
	if again := slaacAddress(netip.MustParsePrefix("2001:db8:1::1/64"), "claim-uid", "rdma0"); again != got {
		t.Errorf("slaacAddress() = %s for the prefix with host bits set, want %s", again, got)
	}

	interfaceID := func(p netip.Prefix) uint64 {
		b := p.Addr().As16()
		return binary.BigEndian.Uint64(b[8:])
	}
	for name, other := range map[string]netip.Prefix{
		"another claim":        slaacAddress(prefix, "other-uid", "rdma0"),
		"another device":       slaacAddress(prefix, "claim-uid", "rdma1"),
		"another split":        slaacAddress(prefix, "claim-ui", "drdma0"),
		"another prefix":       slaacAddress(netip.MustParsePrefix("2001:db8:2::/64"), "claim-uid", "rdma0"),
		"another prefix bits":  slaacAddress(netip.MustParsePrefix("2001:db8:1:1::/64"), "claim-uid", "rdma0"),
		"another empty device": slaacAddress(prefix, "claim-uid", ""),
	} {
		if interfaceID(other) == interfaceID(got) {
			t.Errorf("%s: slaacAddress() = %s, want an interface identifier other than that of %s", name, other, got)
		}
	}
}

func TestReservedInterfaceID(t *testing.T) {
	for id, want := range map[uint64]bool{
		0x0000000000000000: true, // Subnet-Router anycast
		0x0000000000000001: false,
		0x02005efffe000000: true, // IANA Ethernet block
		0x02005efffeffffff: true,
		0x02005efffdffffff: false,
		0x02005effff000000: false,
		0xfdffffffffffff7f: false,
		0xfdffffffffffff80: true, // reserved subnet anycast
		0xfdffffffffffffff: true,
		0xfe00000000000000: false,
	} {
		if got := reservedInterfaceID(id); got != want {
			t.Errorf("reservedInterfaceID(%016x) = %t, want %t", id, got, want)
		}
	}
}

func TestParseRouterAdvertisement(t *testing.T) {
	sourceLinkLayer := []byte{1, 1, 0x02, 0, 0, 0, 0, 0x01} // ignored option
	tests := []struct {
		name         string
		message      []byte
		wantLifetime time.Duration
		wantPrefixes []netip.Prefix
		wantErr      string
	}{
		{
			name:         "default router with one prefix",
			message:      testRouterAdvertisement(1800, sourceLinkLayer, testPrefixInformation("2001:db8:1::", 64, pioOnLink|pioAutonomous, 2592000, 604800)),
			wantLifetime: 1800 * time.Second,
			wantPrefixes: []netip.Prefix{netip.MustParsePrefix("2001:db8:1::/64")},
		},
		{
			name:         "host bits in the prefix are cleared",
			message:      testRouterAdvertisement(1800, testPrefixInformation("2001:db8:1::1", 64, pioOnLink|pioAutonomous, 3600, 1800)),
			wantLifetime: 1800 * time.Second,
			wantPrefixes: []netip.Prefix{netip.MustParsePrefix("2001:db8:1::/64")},
		},
		{
			name: "prefixes SLAAC ignores or the client does not support",
			message: testRouterAdvertisement(1800,
				testPrefixInformation("2001:db8:1::", 64, pioOnLink, 3600, 1800),               // not autonomous
				testPrefixInformation("2001:db8:2::", 48, pioOnLink|pioAutonomous, 3600, 1800), // not a /64
				testPrefixInformation("fe80::", 64, pioOnLink|pioAutonomous, 3600, 1800),       // link-local
				testPrefixInformation("2001:db8:3::", 64, pioOnLink|pioAutonomous, 3600, 0),    // deprecated
				testPrefixInformation("2001:db8:4::", 64, pioOnLink|pioAutonomous, 1800, 3600), // preferred beyond valid
				testPrefixInformation("2001:db8:6::", 64, pioAutonomous, 3600, 1800),           // not on-link
				testPrefixInformation("2001:db8:5::", 64, pioOnLink|pioAutonomous, 3600, 1800), // usable
			),
			wantLifetime: 1800 * time.Second,
			wantPrefixes: []netip.Prefix{netip.MustParsePrefix("2001:db8:5::/64")},
		},
		{
			name: "two usable prefixes",
			message: testRouterAdvertisement(1800,
				testPrefixInformation("2001:db8:1::", 64, pioOnLink|pioAutonomous, 3600, 1800),
				testPrefixInformation("fdcd:10:66b0:a033::", 64, pioOnLink|pioAutonomous, 3600, 1800)),
			wantLifetime: 1800 * time.Second,
			wantPrefixes: []netip.Prefix{netip.MustParsePrefix("2001:db8:1::/64"), netip.MustParsePrefix("fdcd:10:66b0:a033::/64")},
		},
		{
			name:         "not a default router",
			message:      testRouterAdvertisement(0, testPrefixInformation("2001:db8:1::", 64, pioOnLink|pioAutonomous, 3600, 1800)),
			wantPrefixes: []netip.Prefix{netip.MustParsePrefix("2001:db8:1::/64")},
		},
		{
			name:    "too short",
			message: testRouterAdvertisement(1800)[:15],
			wantErr: "too short",
		},
		{
			name:    "router solicitation",
			message: []byte{icmpv6RouterSolicitation, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
			wantErr: "not a router advertisement",
		},
		{
			name:    "option of length zero",
			message: testRouterAdvertisement(1800, []byte{ndpOptionPrefixInformation, 0, 0, 0, 0, 0, 0, 0}),
			wantErr: "length 0",
		},
		{
			name:    "option longer than the message",
			message: testRouterAdvertisement(1800, testPrefixInformation("2001:db8:1::", 64, pioAutonomous, 3600, 1800)[:24]),
			wantErr: "longer than the message",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ra, err := parseRouterAdvertisement(tt.message)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("parseRouterAdvertisement() error = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseRouterAdvertisement() error = %v", err)
			}
			if ra.routerLifetime != tt.wantLifetime {
				t.Errorf("router lifetime = %v, want %v", ra.routerLifetime, tt.wantLifetime)
			}
			if diff := cmp.Diff(tt.wantPrefixes, ra.prefixes, cmp.Comparer(func(a, b netip.Prefix) bool { return a == b })); diff != "" {
				t.Errorf("prefixes mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// slaacTestLink creates a veth pair, client and router, with duplicate
// address detection off so their link-local addresses are usable at once,
// and returns both links once they are up.
func slaacTestLink(t *testing.T) (client, router netlink.Link) {
	t.Helper()
	if err := netlink.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "slaacc"}, PeerName: "slaacr"}); err != nil {
		t.Fatalf("add veth: %v", err)
	}
	links := map[string]netlink.Link{}
	for _, name := range []string{"slaacc", "slaacr"} {
		// Best effort: with detection on, the link-local addresses become
		// usable about a second later, and getSLAAC solicits again.
		_ = os.WriteFile("/proc/sys/net/ipv6/conf/"+name+"/dad_transmits", []byte("0"), 0o644)
		link, err := netlink.LinkByName(name)
		if err != nil {
			t.Fatalf("link %s: %v", name, err)
		}
		if err := netlink.LinkSetUp(link); err != nil {
			t.Fatalf("set %s up: %v", name, err)
		}
		links[name] = link
	}
	return links["slaacc"], links["slaacr"]
}

// testRouter answers router solicitations on link with advertisement, sent
// with the given hop limit. It ignores the first ignoreFirst solicitations,
// and counts all it receives.
type testRouter struct {
	solicitations atomic.Int32
	done          chan struct{}
	stopped       chan struct{}
}

func startTestRouter(t *testing.T, link netlink.Link, advertisement []byte, hopLimit int, ignoreFirst int32) *testRouter {
	t.Helper()
	fd, err := unix.Socket(unix.AF_INET6, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.IPPROTO_ICMPV6)
	if err != nil {
		t.Fatalf("open a raw ICMPv6 socket: %v", err)
	}
	var filter unix.ICMPv6Filter
	for i := range filter.Data {
		filter.Data[i] = 0xffffffff
	}
	filter.Data[icmpv6RouterSolicitation>>5] &^= 1 << (icmpv6RouterSolicitation & 31)
	// Solicitations go to all-routers, which only a router's interface joins.
	allRouters := &unix.IPv6Mreq{Interface: uint32(link.Attrs().Index)}
	copy(allRouters.Multiaddr[:], net.ParseIP("ff02::2").To16())
	readTimeout := unix.NsecToTimeval((50 * time.Millisecond).Nanoseconds())
	for _, set := range []func() error{
		func() error { return unix.BindToDevice(fd, link.Attrs().Name) },
		func() error { return unix.SetsockoptICMPv6Filter(fd, unix.SOL_ICMPV6, unix.ICMPV6_FILTER, &filter) },
		func() error { return unix.SetsockoptIPv6Mreq(fd, unix.IPPROTO_IPV6, unix.IPV6_JOIN_GROUP, allRouters) },
		func() error { return unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_MULTICAST_HOPS, hopLimit) },
		func() error {
			return unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_MULTICAST_IF, link.Attrs().Index)
		},
		func() error { return unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &readTimeout) },
	} {
		if err := set(); err != nil {
			unix.Close(fd)
			t.Fatalf("configure the router socket: %v", err)
		}
	}
	allNodes := &unix.SockaddrInet6{ZoneId: uint32(link.Attrs().Index)}
	copy(allNodes.Addr[:], net.ParseIP("ff02::1").To16())

	r := &testRouter{done: make(chan struct{}), stopped: make(chan struct{})}
	go func() {
		defer close(r.stopped)
		defer unix.Close(fd)
		buf := make([]byte, 1500)
		for {
			select {
			case <-r.done:
				return
			default:
			}
			n, _, err := unix.Recvfrom(fd, buf, 0)
			if err != nil || n < 1 || buf[0] != icmpv6RouterSolicitation {
				continue
			}
			if r.solicitations.Add(1) <= ignoreFirst {
				continue
			}
			if err := unix.Sendto(fd, advertisement, 0, allNodes); err != nil {
				t.Logf("send router advertisement: %v", err)
			}
		}
	}()
	t.Cleanup(r.stop)
	return r
}

func (r *testRouter) stop() {
	select {
	case <-r.done:
	default:
		close(r.done)
	}
	<-r.stopped
}

// linkLocalAddress returns the link-local address of link.
func linkLocalAddress(t *testing.T, link netlink.Link) netip.Addr {
	t.Helper()
	addrs, err := netlink.AddrList(link, netlink.FAMILY_V6)
	if err != nil {
		t.Fatalf("list addresses of %s: %v", link.Attrs().Name, err)
	}
	for _, a := range addrs {
		if addr, ok := netip.AddrFromSlice(a.IP); ok && addr.IsLinkLocalUnicast() {
			return addr
		}
	}
	t.Fatalf("%s has no link-local address", link.Attrs().Name)
	return netip.Addr{}
}

// The client solicits on the interface, retransmits when a solicitation goes
// unanswered, and takes the prefix and the default route from the answer.
func TestGetSLAAC(t *testing.T) {
	userns.Run(t, testGetSLAAC, syscall.CLONE_NEWNET)
}

func testGetSLAAC(t *testing.T) {
	client, routerLink := slaacTestLink(t)
	advertisement := testRouterAdvertisement(1800, testPrefixInformation("2001:db8:1::", 64, pioOnLink|pioAutonomous, 2592000, 604800))
	router := startTestRouter(t, routerLink, advertisement, ndpHopLimit, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	prefixes, routes, err := getSLAAC(ctx, client.Attrs().Name)
	if err != nil {
		t.Fatalf("getSLAAC() error = %v", err)
	}

	if want := []netip.Prefix{netip.MustParsePrefix("2001:db8:1::/64")}; !slices.Equal(want, prefixes) {
		t.Errorf("getSLAAC() prefixes = %v, want %v", prefixes, want)
	}
	wantRoutes := []apis.RouteConfig{{Destination: "::/0", Gateway: linkLocalAddress(t, routerLink).String()}}
	if diff := cmp.Diff(wantRoutes, routes); diff != "" {
		t.Errorf("routes mismatch (-want +got):\n%s", diff)
	}
	if got := router.solicitations.Load(); got < 2 {
		t.Errorf("router saw %d solicitations, want a retransmission after the first went unanswered", got)
	}
}

// An advertisement larger than 1500 octets, on a link with jumbo frames, is
// read whole.
func TestGetSLAACLargeAdvertisement(t *testing.T) {
	userns.Run(t, testGetSLAACLargeAdvertisement, syscall.CLONE_NEWNET)
}

func testGetSLAACLargeAdvertisement(t *testing.T) {
	client, routerLink := slaacTestLink(t)
	for _, link := range []netlink.Link{client, routerLink} {
		if err := netlink.LinkSetMTU(link, 9000); err != nil {
			t.Fatalf("set mtu: %v", err)
		}
	}
	// Prefixes that are not autonomous, then the usable one at the end.
	var options [][]byte
	for i := range 60 {
		options = append(options, testPrefixInformation(fmt.Sprintf("2001:db8:%x::", 0x100+i), 64, pioOnLink, 3600, 1800))
	}
	options = append(options, testPrefixInformation("2001:db8:1::", 64, pioOnLink|pioAutonomous, 3600, 1800))
	advertisement := testRouterAdvertisement(1800, options...)
	if len(advertisement) <= 1500 {
		t.Fatalf("advertisement of %d octets, want more than 1500", len(advertisement))
	}
	startTestRouter(t, routerLink, advertisement, ndpHopLimit, 0)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	prefixes, _, err := getSLAAC(ctx, client.Attrs().Name)
	if err != nil {
		t.Fatalf("getSLAAC() error = %v", err)
	}
	if want := []netip.Prefix{netip.MustParsePrefix("2001:db8:1::/64")}; !slices.Equal(want, prefixes) {
		t.Errorf("getSLAAC() prefixes = %v, want %v", prefixes, want)
	}
}

// Without an answer, or with an answer that did not come from the link, the
// client gives up when the context ends and says why.
func TestGetSLAACWithoutUsableAdvertisement(t *testing.T) {
	userns.Run(t, testGetSLAACWithoutUsableAdvertisement, syscall.CLONE_NEWNET)
}

func testGetSLAACWithoutUsableAdvertisement(t *testing.T) {
	advertisement := testRouterAdvertisement(1800, testPrefixInformation("2001:db8:1::", 64, pioOnLink|pioAutonomous, 3600, 1800))
	tests := []struct {
		name    string
		setup   func(t *testing.T, client, router netlink.Link)
		wantErr string
	}{
		{
			name:    "no router",
			setup:   func(t *testing.T, client, router netlink.Link) {},
			wantErr: "no router advertisement received",
		},
		{
			name: "hop limit other than 255",
			setup: func(t *testing.T, client, router netlink.Link) {
				startTestRouter(t, router, advertisement, 64, 0)
			},
			wantErr: "hop limit 64",
		},
		{
			name: "no autonomous prefix",
			setup: func(t *testing.T, client, router netlink.Link) {
				startTestRouter(t, router, testRouterAdvertisement(1800, testPrefixInformation("2001:db8:1::", 64, pioOnLink, 3600, 1800)), ndpHopLimit, 0)
			},
			wantErr: "no on-link, autonomous /64 prefix",
		},
		{
			// Without a link-local address no solicitation can go out, and the
			// error says why the router never answered.
			name: "solicitations fail",
			setup: func(t *testing.T, client, router netlink.Link) {
				startTestRouter(t, router, advertisement, ndpHopLimit, 0)
				lla := linkLocalAddress(t, client)
				if err := netlink.AddrDel(client, &netlink.Addr{IPNet: &net.IPNet{IP: lla.AsSlice(), Mask: net.CIDRMask(64, 128)}}); err != nil {
					t.Fatalf("delete the link-local address: %v", err)
				}
			},
			wantErr: "the last router solicitation failed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, routerLink := slaacTestLink(t)
			t.Cleanup(func() { _ = netlink.LinkDel(client) })
			tt.setup(t, client, routerLink)

			ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
			defer cancel()
			prefixes, routes, err := getSLAAC(ctx, client.Attrs().Name)
			if err == nil {
				t.Fatalf("getSLAAC() = %v, %v, want an error", prefixes, routes)
			}
			if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("getSLAAC() error = %v, want a deadline error containing %q", err, tt.wantErr)
			}
		})
	}
}

// Without IPv6 on the interface no solicitation goes out, so the client says
// so at once instead of when the context ends.
func TestGetSLAACWithoutIPv6(t *testing.T) {
	userns.Run(t, testGetSLAACWithoutIPv6, syscall.CLONE_NEWNET)
}

func testGetSLAACWithoutIPv6(t *testing.T) {
	tests := []struct {
		name    string
		disable func(t *testing.T, link netlink.Link)
		wantErr string
	}{
		{
			name:    "IPv6 disabled",
			wantErr: "has IPv6 disabled",
			disable: func(t *testing.T, link netlink.Link) {
				if err := os.WriteFile("/proc/sys/net/ipv6/conf/"+link.Attrs().Name+"/disable_ipv6", []byte("1"), 0o644); err != nil {
					t.Fatalf("disable IPv6: %v", err)
				}
			},
		},
		{
			name:    "mtu below the IPv6 minimum",
			wantErr: "has no IPv6 settings",
			disable: func(t *testing.T, link netlink.Link) {
				if err := netlink.LinkSetMTU(link, 1000); err != nil {
					t.Fatalf("set mtu: %v", err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, _ := slaacTestLink(t)
			t.Cleanup(func() { _ = netlink.LinkDel(client) })
			tt.disable(t, client)

			ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
			defer cancel()
			prefixes, routes, err := getSLAAC(ctx, client.Attrs().Name)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("getSLAAC() = %v, %v, %v, want an error about IPv6 on the interface", prefixes, routes, err)
			}
			if ctx.Err() != nil {
				t.Errorf("getSLAAC() returned only when the context ended")
			}
		})
	}
}
