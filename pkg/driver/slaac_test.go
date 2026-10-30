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
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"

	userns "sigs.k8s.io/dranet/internal/testutils"
	"sigs.k8s.io/dranet/pkg/apis"
	"sigs.k8s.io/dranet/pkg/internal/slaac"
)

var (
	slaacTestRouter = netip.MustParseAddr("fe80::1")
	slaacTestPrefix = netip.MustParsePrefix("2001:db8::/64")
)

// slaacTestRA is what a router on a fabric that hands out prefixes by Router
// Advertisement typically sends: a default route, an on-link prefix for
// autoconfiguration, a second on-link prefix that is routed only, and a more
// specific route (RFC 4191).
var slaacTestRA = &slaac.RouterAdvertisement{
	CurrentHopLimit: 64,
	RouterLifetime:  30 * time.Minute,
	MTU:             9000,
	Prefixes: []slaac.PrefixInformation{
		{Prefix: slaacTestPrefix, OnLink: true, Autonomous: true, ValidLifetime: 24 * time.Hour, PreferredLifetime: 4 * time.Hour},
		{Prefix: netip.MustParsePrefix("2001:db8:2::/64"), OnLink: true, ValidLifetime: 24 * time.Hour},
	},
	Routes: []slaac.RouteInformation{
		{Prefix: netip.MustParsePrefix("2001:db8:1::/48"), Lifetime: time.Hour},
	},
}

// slaacTestRoutes are the routes slaacTestRA configures through router.
func slaacTestRoutes(router netip.Addr) []apis.RouteConfig {
	return []apis.RouteConfig{
		{Destination: "2001:db8:2::/64", Scope: unix.RT_SCOPE_LINK},
		{Destination: "::/0", Gateway: router.String()},
		{Destination: "2001:db8:1::/48", Gateway: router.String()},
	}
}

// slaacTestSourceRouting is the routing the driver configures for a SLAAC
// interface with address, the address slaacTestRA gives it in slaacTestPrefix,
// whose host interface has index hostIndex.
func slaacTestSourceRouting(address string, hostIndex int) ([]apis.RouteConfig, apis.RuleConfig) {
	table := apis.SLAACRouteTableOffset + hostIndex
	routes := []apis.RouteConfig{{Destination: slaacTestPrefix.String(), Scope: unix.RT_SCOPE_LINK, Table: table}}
	for _, route := range slaacTestRoutes(slaacTestRouter) {
		route.Table = table
		routes = append(routes, route)
	}
	source := netip.MustParsePrefix(address).Addr()
	return routes, apis.RuleConfig{Priority: apis.SourceRoutingRulePriority, Source: netip.PrefixFrom(source, 128).String(), Table: table}
}

func TestSLAACSourceRouting(t *testing.T) {
	routes := slaacTestRoutes(slaacTestRouter)
	table := apis.SLAACRouteTableOffset + 7
	tests := []struct {
		name       string
		addresses  []string
		inVRF      bool
		wantRoutes []apis.RouteConfig
		wantRules  []apis.RuleConfig
	}{
		{
			name:      "a table of its own with a rule from the address",
			addresses: []string{"192.0.2.10/24", "2001:db8::10/64"},
			wantRoutes: []apis.RouteConfig{
				{Destination: "2001:db8::/64", Scope: unix.RT_SCOPE_LINK, Table: table},
				{Destination: "2001:db8:2::/64", Scope: unix.RT_SCOPE_LINK, Table: table},
				{Destination: "::/0", Gateway: slaacTestRouter.String(), Table: table},
				{Destination: "2001:db8:1::/48", Gateway: slaacTestRouter.String(), Table: table},
			},
			wantRules: []apis.RuleConfig{{Priority: apis.SourceRoutingRulePriority, Source: "2001:db8::10/128", Table: table}},
		},
		{
			// An address in a prefix that is not on-link has no link route.
			name:      "an address without its prefix on-link",
			addresses: []string{"2001:db8::10/128"},
			wantRoutes: []apis.RouteConfig{
				{Destination: "2001:db8:2::/64", Scope: unix.RT_SCOPE_LINK, Table: table},
				{Destination: "::/0", Gateway: slaacTestRouter.String(), Table: table},
				{Destination: "2001:db8:1::/48", Gateway: slaacTestRouter.String(), Table: table},
			},
			wantRules: []apis.RuleConfig{{Priority: apis.SourceRoutingRulePriority, Source: "2001:db8::10/128", Table: table}},
		},
		{
			// The driver puts the routes of an interface in a VRF in its table.
			name:       "in a VRF",
			addresses:  []string{"2001:db8::10/64"},
			inVRF:      true,
			wantRoutes: routes,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotRoutes, gotRules := slaacSourceRouting(tt.addresses, routes, 7, tt.inVRF)
			if diff := cmp.Diff(tt.wantRoutes, gotRoutes); diff != "" {
				t.Errorf("routes mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tt.wantRules, gotRules); diff != "" {
				t.Errorf("rules mismatch (-want +got):\n%s", diff)
			}
		})
	}
	// The routes passed in are not changed.
	if diff := cmp.Diff(slaacTestRoutes(slaacTestRouter), routes); diff != "" {
		t.Errorf("slaacSourceRouting changed its input (-want +got):\n%s", diff)
	}
}

func TestDescribeUnusableAdvertisement(t *testing.T) {
	for name, tc := range map[string]struct {
		ra   *slaac.RouterAdvertisement
		want string
	}{
		"managed without prefixes": {
			ra:   &slaac.RouterAdvertisement{ManagedConfiguration: true},
			want: "DHCPv6",
		},
		"no prefixes": {
			ra:   &slaac.RouterAdvertisement{RouterLifetime: time.Hour},
			want: "advertises no prefix",
		},
		"on-link only and managed": {
			ra:   &slaac.RouterAdvertisement{ManagedConfiguration: true, Prefixes: []slaac.PrefixInformation{{Prefix: slaacTestPrefix, OnLink: true, ValidLifetime: time.Hour}}},
			want: "DHCPv6",
		},
		"on-link only": {
			ra:   &slaac.RouterAdvertisement{Prefixes: []slaac.PrefixInformation{{Prefix: slaacTestPrefix, OnLink: true, ValidLifetime: time.Hour}}},
			want: "none with the autonomous flag",
		},
		"autonomous but not a /64": {
			ra:   &slaac.RouterAdvertisement{Prefixes: []slaac.PrefixInformation{{Prefix: netip.MustParsePrefix("2001:db8::/56"), Autonomous: true, ValidLifetime: time.Hour}}},
			want: "none usable",
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := describeUnusableAdvertisement(tc.ra, slaacTestRouter)
			if !strings.Contains(got, tc.want) || !strings.Contains(got, slaacTestRouter.String()) {
				t.Errorf("describeUnusableAdvertisement = %q, want it to mention %q and the router", got, tc.want)
			}
		})
	}
}

// slaacTestLink is a veth pair whose router end answers solicitations the way
// a router on the fabric would.
type slaacTestLink struct {
	host   string
	router *slaac.Conn

	mu           sync.Mutex
	solicitation *slaac.RouterSolicitation
	source       netip.Addr
}

// newSlaacTestLink creates the pair in the test's network namespace and
// starts answering solicitations on the router end with ra; a nil ra leaves
// the link without a router.
func newSlaacTestLink(t *testing.T, host, peer string, ra *slaac.RouterAdvertisement) *slaacTestLink {
	t.Helper()
	veth := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: host}, PeerName: peer}
	if err := netlink.LinkAdd(veth); err != nil {
		t.Fatalf("add veth: %v", err)
	}
	// The router would answer the kernel's own solicitations too, and hide a
	// driver that does not solicit when it should.
	if err := os.WriteFile(fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/router_solicitations", host), []byte("0"), 0o644); err != nil {
		t.Fatalf("turn off the kernel's solicitations on %s: %v", host, err)
	}
	for _, name := range []string{host, peer} {
		link, err := netlink.LinkByName(name)
		if err != nil {
			t.Fatalf("link %s: %v", name, err)
		}
		if err := netlink.LinkSetUp(link); err != nil {
			t.Fatalf("set %s up: %v", name, err)
		}
	}
	l := &slaacTestLink{host: host}
	if ra == nil {
		return l
	}
	ifi, err := net.InterfaceByName(peer)
	if err != nil {
		t.Fatal(err)
	}
	l.router, err = slaac.Listen(ifi, slaac.TypeRouterSolicitation)
	if err != nil {
		t.Fatalf("router listen: %v", err)
	}
	t.Cleanup(func() { l.router.Close() })
	go func() {
		for {
			m, src, err := l.router.ReadFrom()
			if errors.Is(err, syscall.ENETDOWN) {
				continue // the router end went down; it reads again once up
			}
			if err != nil {
				return
			}
			rs, ok := m.(*slaac.RouterSolicitation)
			if !ok {
				continue
			}
			l.mu.Lock()
			l.solicitation, l.source = rs, src
			l.mu.Unlock()
			if err := l.router.WriteTo(ra, slaacTestRouter, slaac.AllNodes, nil); err != nil {
				t.Errorf("router send: %v", err)
			}
		}
	}()
	return l
}

// lastSolicitation returns the last solicitation the router saw and its source.
func (l *slaacTestLink) lastSolicitation() (*slaac.RouterSolicitation, netip.Addr) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.solicitation, l.source
}

// slaacTestAddress is the address id forms in prefix, with its length.
func slaacTestAddress(t *testing.T, id slaac.InterfaceIdentifier, prefix netip.Prefix) string {
	t.Helper()
	addr, err := id.Address(prefix)
	if err != nil {
		t.Fatal(err)
	}
	return netip.PrefixFrom(addr, prefix.Bits()).String()
}

// waitForLinkLocal waits until the host end has a link-local address that
// completed duplicate address detection, as a host interface that has been
// up for a while has.
func (l *slaacTestLink) waitForLinkLocal(t *testing.T) netip.Addr {
	t.Helper()
	link, err := netlink.LinkByName(l.host)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if addr := linkLocalAddress(link); !addr.IsUnspecified() {
			return addr
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s got no usable link-local address", l.host)
	return netip.Addr{}
}

func TestGetSLAAC(t *testing.T) {
	userns.Run(t, testGetSLAAC, syscall.CLONE_NEWNET)
}

// The configuration is what a host would autoconfigure from the
// advertisement: the address of the identifier in the autonomous prefix with
// its on-link prefix length, the on-link route for the prefix without one,
// the default route and the more specific route through the router. Once the
// interface has a link-local address, the solicitation is sent from it with
// the hardware address, so the router can answer by unicast.
func testGetSLAAC(t *testing.T) {
	link := newSlaacTestLink(t, "host0", "router0", slaacTestRA)
	linkLocal := link.waitForLinkLocal(t)

	ifi, err := net.InterfaceByName(link.host)
	if err != nil {
		t.Fatal(err)
	}
	id := slaac.StableIdentifier("claim-uid", "net-dev-0")
	ctx, cancel := context.WithTimeout(context.Background(), slaacTimeout)
	defer cancel()
	start := time.Now()
	addresses, routes, err := getSLAAC(ctx, link.host, id)
	if err != nil {
		t.Fatalf("getSLAAC after %v: %v", time.Since(start), err)
	}
	if want := []string{slaacTestAddress(t, id, slaacTestPrefix)}; !cmp.Equal(addresses, want) {
		t.Errorf("addresses = %v, want %v", addresses, want)
	}
	if diff := cmp.Diff(slaacTestRoutes(slaacTestRouter), routes); diff != "" {
		t.Errorf("routes mismatch (-want +got):\n%s", diff)
	}
	rs, src := link.lastSolicitation()
	if rs == nil {
		t.Fatal("the router saw no solicitation")
	}
	if src != linkLocal {
		t.Errorf("solicitation from %v, want the link-local address %v", src, linkLocal)
	}
	if rs.SourceLinkLayerAddress.String() != ifi.HardwareAddr.String() {
		t.Errorf("solicitation link-layer address = %v, want %v", rs.SourceLinkLayerAddress, ifi.HardwareAddr)
	}
}

func TestGetSLAACWithIPv6DisabledOnHost(t *testing.T) {
	userns.Run(t, testGetSLAACWithIPv6DisabledOnHost, syscall.CLONE_NEWNET)
}

// A host that has IPv6 disabled on the interface, as a node that only uses
// IPv4 itself may, can still autoconfigure the Pod: the exchange does not go
// through the host's IPv6 stack. The solicitation then comes from the
// unspecified address without a link-layer address (RFC 4861 section 4.1).
func testGetSLAACWithIPv6DisabledOnHost(t *testing.T) {
	link := newSlaacTestLink(t, "host0", "router0", slaacTestRA)
	if err := os.WriteFile(fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/disable_ipv6", link.host), []byte("1"), 0o644); err != nil {
		t.Fatalf("disable IPv6 on %s: %v", link.host, err)
	}
	hostLink, err := netlink.LinkByName(link.host)
	if err != nil {
		t.Fatal(err)
	}
	if addrs, err := netlink.AddrList(hostLink, netlink.FAMILY_V6); err != nil || len(addrs) != 0 {
		t.Fatalf("%s still has IPv6 addresses %v (err %v)", link.host, addrs, err)
	}

	id := slaac.InterfaceIdentifier{0, 0, 0, 0, 0, 0, 0, 1}
	ctx, cancel := context.WithTimeout(context.Background(), slaacTimeout)
	defer cancel()
	addresses, routes, err := getSLAAC(ctx, link.host, id)
	if err != nil {
		t.Fatalf("getSLAAC: %v", err)
	}
	if want := []string{"2001:db8::1/64"}; !cmp.Equal(addresses, want) {
		t.Errorf("addresses = %v, want %v", addresses, want)
	}
	if diff := cmp.Diff(slaacTestRoutes(slaacTestRouter), routes); diff != "" {
		t.Errorf("routes mismatch (-want +got):\n%s", diff)
	}
	rs, src := link.lastSolicitation()
	if rs == nil {
		t.Fatal("the router saw no solicitation")
	}
	if !src.IsUnspecified() {
		t.Errorf("solicitation from %v, want the unspecified address", src)
	}
	if rs.SourceLinkLayerAddress != nil {
		t.Errorf("solicitation from the unspecified address carried link-layer address %v", rs.SourceLinkLayerAddress)
	}
}

func TestGetSLAACUnusableAdvertisement(t *testing.T) {
	userns.Run(t, testGetSLAACUnusableAdvertisement, syscall.CLONE_NEWNET)
}

// A link whose router points to DHCPv6 must fail with that diagnosis rather
// than a bare timeout, and must not configure a default route without an
// address to use it from.
func testGetSLAACUnusableAdvertisement(t *testing.T) {
	link := newSlaacTestLink(t, "host0", "router0", &slaac.RouterAdvertisement{ManagedConfiguration: true, RouterLifetime: time.Hour})

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	addresses, routes, err := getSLAAC(ctx, link.host, slaac.InterfaceIdentifier{0, 0, 0, 0, 0, 0, 0, 1})
	if err == nil {
		t.Fatalf("getSLAAC = %v %v, want an error", addresses, routes)
	}
	for _, want := range []string{"DHCPv6", slaacTestRouter.String(), context.DeadlineExceeded.Error()} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("getSLAAC error = %q, want it to mention %q", err, want)
		}
	}
}

func TestGetSLAACWaitsForCarrier(t *testing.T) {
	userns.Run(t, testGetSLAACWaitsForCarrier, syscall.CLONE_NEWNET)
}

// A host interface that was down gets its carrier only after the driver set
// it up, here once its peer comes up after the last retransmission is due.
// The driver solicits only then, rather than into a link without carrier.
func testGetSLAACWaitsForCarrier(t *testing.T) {
	link := newSlaacTestLink(t, "host0", "router0", slaacTestRA)
	for _, name := range []string{link.host, "router0"} {
		l, err := netlink.LinkByName(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := netlink.LinkSetDown(l); err != nil {
			t.Fatal(err)
		}
	}
	peerUp := time.AfterFunc(3500*time.Millisecond, func() {
		if l, err := netlink.LinkByName("router0"); err == nil {
			_ = netlink.LinkSetUp(l)
		}
	})
	defer peerUp.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), slaacTimeout)
	defer cancel()
	start := time.Now()
	if _, _, err := getSLAAC(ctx, link.host, slaac.StableIdentifier("claim-uid", "net-dev-0")); err != nil {
		t.Fatalf("getSLAAC after %v: %v", time.Since(start), err)
	}
}

func TestGetSLAACNotEthernet(t *testing.T) {
	userns.Run(t, testGetSLAACNotEthernet, syscall.CLONE_NEWNET)
}

func testGetSLAACNotEthernet(t *testing.T) {
	_, _, err := getSLAAC(context.Background(), "lo", slaac.InterfaceIdentifier{0, 0, 0, 0, 0, 0, 0, 1})
	if err == nil || !strings.Contains(err.Error(), "not an Ethernet interface") {
		t.Errorf("getSLAAC(lo) error = %v, want not an Ethernet interface", err)
	}
}

func TestGetSLAACWithoutRouter(t *testing.T) {
	userns.Run(t, testGetSLAACWithoutRouter, syscall.CLONE_NEWNET)
}

func testGetSLAACWithoutRouter(t *testing.T) {
	link := newSlaacTestLink(t, "host0", "router0", nil)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, _, err := getSLAAC(ctx, link.host, slaac.InterfaceIdentifier{0, 0, 0, 0, 0, 0, 0, 1})
	if err == nil || !strings.Contains(err.Error(), "no router advertisement received") {
		t.Errorf("getSLAAC error = %v, want no router advertisement received", err)
	}
}

func TestPrepareResourceClaimSLAAC(t *testing.T) {
	userns.Run(t, testPrepareResourceClaimSLAAC, syscall.CLONE_NEWNET)
}

// The prepare path with SLAAC behaves like the one with DHCP: the exchange
// runs on the host interface and its result lands in the Pod configuration
// as static addresses and routes.
func testPrepareResourceClaimSLAAC(t *testing.T) {
	const testDriverName = "test.driver"
	link := newSlaacTestLink(t, "host0", "router0", slaacTestRA)
	hostLink, err := netlink.LinkByName(link.host)
	if err != nil {
		t.Fatal(err)
	}
	// The host has an IPv4 address, an IPv6 address of its own in another
	// prefix, and routes it learned from its own router advertisements.
	for _, cidr := range []string{"192.0.2.10/24", "2001:db8:ffff::10/64"} {
		addr, err := netlink.ParseAddr(cidr)
		if err != nil {
			t.Fatal(err)
		}
		if err := netlink.AddrAdd(hostLink, addr); err != nil {
			t.Fatalf("add %s: %v", cidr, err)
		}
	}
	link.waitForLinkLocal(t)
	_, raDst, _ := net.ParseCIDR("2001:db8:ffff:1::/64")
	if err := netlink.RouteAdd(&netlink.Route{LinkIndex: hostLink.Attrs().Index, Dst: raDst, Gw: slaacTestRouter.AsSlice(), Protocol: unix.RTPROT_RA}); err != nil {
		t.Fatalf("add proto ra route: %v", err)
	}
	_, staticDst, _ := net.ParseCIDR("198.51.100.0/24")
	if err := netlink.RouteAdd(&netlink.Route{LinkIndex: hostLink.Attrs().Index, Dst: staticDst, Gw: net.ParseIP("192.0.2.1")}); err != nil {
		t.Fatalf("add static route: %v", err)
	}
	// A static IPv6 route, as a node agent may install.
	_, staticV6Dst, _ := net.ParseCIDR("2001:db8:ffff:2::/64")
	if err := netlink.RouteAdd(&netlink.Route{LinkIndex: hostLink.Attrs().Index, Dst: staticV6Dst, Gw: slaacTestRouter.AsSlice()}); err != nil {
		t.Fatalf("add static IPv6 route: %v", err)
	}

	claim := func(name string, parameters string) *resourcev1.ResourceClaim {
		return &resourcev1.ResourceClaim{
			ObjectMeta: metav1.ObjectMeta{UID: types.UID("claim-uid-" + name), Namespace: "default", Name: name},
			Status: resourcev1.ResourceClaimStatus{
				ReservedFor: []resourcev1.ResourceClaimConsumerReference{
					{APIGroup: "", Resource: "pods", Name: "test-pod", UID: types.UID("pod-uid-" + name)},
				},
				Allocation: &resourcev1.AllocationResult{
					Devices: resourcev1.DeviceAllocationResult{
						Results: []resourcev1.DeviceRequestAllocationResult{
							{Driver: testDriverName, Device: "net-dev-0", Request: "req-0"},
						},
						Config: []resourcev1.DeviceAllocationConfiguration{{
							Source: resourcev1.AllocationConfigSourceClaim,
							DeviceConfiguration: resourcev1.DeviceConfiguration{
								Opaque: &resourcev1.OpaqueDeviceConfiguration{
									Driver:     testDriverName,
									Parameters: runtime.RawExtension{Raw: []byte(parameters)},
								},
							},
						}},
					},
				},
			},
		}
	}
	newDriver := func() *NetworkDriver {
		fakeDB := newFakeInventoryDB()
		fakeDB.GetNetInterfaceNameFunc = func(string) (string, error) { return link.host, nil }
		fakeDB.GetDeviceFunc = func(deviceName string) (resourcev1.Device, bool) {
			return resourcev1.Device{Name: deviceName}, true
		}
		return &NetworkDriver{
			netdb:          fakeDB,
			driverName:     testDriverName,
			podConfigStore: mustNewPodConfigStore(),
			eventRecorder:  record.NewFakeRecorder(100),
		}
	}
	hostIndex := func(t *testing.T) int {
		t.Helper()
		hostLink, err := netlink.LinkByName(link.host)
		if err != nil {
			t.Fatal(err)
		}
		return hostLink.Attrs().Index
	}
	prepare := func(t *testing.T, claim *resourcev1.ResourceClaim) DeviceConfig {
		t.Helper()
		np := newDriver()
		if result := np.prepareResourceClaim(t.Context(), claim); result.Err != nil {
			t.Fatalf("prepareResourceClaim: %v", result.Err)
		}
		podCfg, ok := np.podConfigStore.GetPodConfig(claim.Status.ReservedFor[0].UID)
		if !ok {
			t.Fatal("no pod config stored")
		}
		return podCfg.DeviceConfigs["net-dev-0"]
	}
	// The address is the claim's and device's in the advertised prefix, and
	// the Pod accepts no router advertisements of its own.
	checkInterface := func(t *testing.T, cfg DeviceConfig, claimUID string, wantIPv4 ...string) {
		t.Helper()
		want := append(wantIPv4, slaacTestAddress(t, slaac.StableIdentifier(claimUID, "net-dev-0"), slaacTestPrefix))
		if diff := cmp.Diff(want, cfg.NetworkInterfaceConfigInPod.Interface.Addresses); diff != "" {
			t.Errorf("addresses mismatch (-want +got):\n%s", diff)
		}
		if acceptRA := cfg.NetworkInterfaceConfigInPod.Interface.AcceptRA; acceptRA == nil || *acceptRA != 0 {
			t.Errorf("acceptRA = %v, want 0", acceptRA)
		}
	}

	t.Run("passthrough", func(t *testing.T) {
		cfg := prepare(t, claim("slaac-passthrough", `{"interface":{"addressing":"SLAAC"}}`))
		// The host IPv4 address is kept, the host IPv6 address is not: the
		// autoconfigured one replaces it.
		checkInterface(t, cfg, "claim-uid-slaac-passthrough", "192.0.2.10/24")
		// The advertised routes come first, in the interface's own table,
		// then the host's IPv4 routes. None of its IPv6 routes are copied,
		// learned or static: the advertisement replaces the host's IPv6
		// configuration.
		slaacRoutes, slaacRule := slaacTestSourceRouting(cfg.NetworkInterfaceConfigInPod.Interface.Addresses[1], hostIndex(t))
		wantRoutes := append(slaacRoutes,
			apis.RouteConfig{Destination: "192.0.2.0/24", Source: "192.0.2.10", Scope: unix.RT_SCOPE_LINK, Table: unix.RT_TABLE_MAIN},
			apis.RouteConfig{Destination: "198.51.100.0/24", Gateway: "192.0.2.1", Table: unix.RT_TABLE_MAIN},
		)
		if diff := cmp.Diff(wantRoutes, cfg.NetworkInterfaceConfigInPod.Routes); diff != "" {
			t.Errorf("routes mismatch (-want +got):\n%s", diff)
		}
		if !slices.Contains(cfg.NetworkInterfaceConfigInPod.Rules, slaacRule) {
			t.Errorf("rules %+v do not include the source rule %+v", cfg.NetworkInterfaceConfigInPod.Rules, slaacRule)
		}
		if cfg.NetworkInterfaceStateInPod != nil {
			t.Errorf("SLAAC left state to release: %+v", cfg.NetworkInterfaceStateInPod)
		}
	})

	t.Run("passthrough with IPv4 addresses in the claim", func(t *testing.T) {
		cfg := prepare(t, claim("slaac-passthrough-v4", `{"interface":{"addressing":"SLAAC","addresses":["192.0.2.20/24"]}}`))
		checkInterface(t, cfg, "claim-uid-slaac-passthrough-v4", "192.0.2.20/24")
	})

	// Without SLAAC the host's IPv6 configuration moves with the interface,
	// as before.
	t.Run("static passthrough", func(t *testing.T) {
		cfg := prepare(t, claim("static-passthrough", `{"interface":{}}`))
		// The host kernel may also have autoconfigured an address from the
		// test router by now.
		for _, want := range []string{"192.0.2.10/24", "2001:db8:ffff::10/64"} {
			if !slices.Contains(cfg.NetworkInterfaceConfigInPod.Interface.Addresses, want) {
				t.Errorf("addresses %v do not include the host address %s", cfg.NetworkInterfaceConfigInPod.Interface.Addresses, want)
			}
		}
		for _, want := range []apis.RouteConfig{
			{Destination: "2001:db8:ffff:1::/64", Gateway: slaacTestRouter.String(), Table: unix.RT_TABLE_MAIN},
			{Destination: "2001:db8:ffff:2::/64", Gateway: slaacTestRouter.String(), Table: unix.RT_TABLE_MAIN},
		} {
			if !slices.Contains(cfg.NetworkInterfaceConfigInPod.Routes, want) {
				t.Errorf("routes %+v do not include the host route %+v", cfg.NetworkInterfaceConfigInPod.Routes, want)
			}
		}
	})

	t.Run("IPVLAN", func(t *testing.T) {
		cfg := prepare(t, claim("slaac-ipvlan", `{"interface":{"type":"IPVLAN","addressing":"SLAAC"}}`))
		// The child gets no IPv4 address of its parent.
		checkInterface(t, cfg, "claim-uid-slaac-ipvlan")
		// A subinterface inherits no host routes or rules, only the
		// advertised routes, in the table of its parent's index.
		wantRoutes, wantRule := slaacTestSourceRouting(cfg.NetworkInterfaceConfigInPod.Interface.Addresses[0], hostIndex(t))
		if diff := cmp.Diff(wantRoutes, cfg.NetworkInterfaceConfigInPod.Routes); diff != "" {
			t.Errorf("routes mismatch (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff([]apis.RuleConfig{wantRule}, cfg.NetworkInterfaceConfigInPod.Rules); diff != "" {
			t.Errorf("rules mismatch (-want +got):\n%s", diff)
		}
	})

	// An advertisement with more prefixes than a claim can report addresses
	// for gives the first of them, after the claim's own addresses.
	t.Run("more prefixes than the claim status holds", func(t *testing.T) {
		ra := &slaac.RouterAdvertisement{RouterLifetime: time.Hour}
		for i := range 20 {
			ra.Prefixes = append(ra.Prefixes, slaac.PrefixInformation{
				Prefix:     netip.MustParsePrefix(fmt.Sprintf("2001:db8:%x::/64", 0x100+i)),
				OnLink:     true,
				Autonomous: true, ValidLifetime: time.Hour, PreferredLifetime: time.Hour,
			})
		}
		many := newSlaacTestLink(t, "host3", "router3", ra)
		np := newDriver()
		np.netdb.(*fakeInventoryDB).GetNetInterfaceNameFunc = func(string) (string, error) { return many.host, nil }
		c := claim("slaac-many", `{"interface":{"addressing":"SLAAC","addresses":["192.0.2.30/24"]}}`)
		if result := np.prepareResourceClaim(t.Context(), c); result.Err != nil {
			t.Fatalf("prepareResourceClaim: %v", result.Err)
		}
		podCfg, ok := np.podConfigStore.GetPodConfig(c.Status.ReservedFor[0].UID)
		if !ok {
			t.Fatal("no pod config stored")
		}
		addresses := podCfg.DeviceConfigs["net-dev-0"].NetworkInterfaceConfigInPod.Interface.Addresses
		if len(addresses) != resourcev1.NetworkDeviceDataMaxIPs || addresses[0] != "192.0.2.30/24" {
			t.Errorf("addresses = %v, want the claim's IPv4 address and %d SLAAC addresses", addresses, resourcev1.NetworkDeviceDataMaxIPs-1)
		}
	})

	// A host interface whose MTU leaves no room for IPv6 fails before any
	// solicitation.
	t.Run("host MTU below the IPv6 minimum", func(t *testing.T) {
		small := newSlaacTestLink(t, "host2", "router2", nil)
		smallLink, err := netlink.LinkByName(small.host)
		if err != nil {
			t.Fatal(err)
		}
		if err := netlink.LinkSetMTU(smallLink, 1200); err != nil {
			t.Fatal(err)
		}
		np := newDriver()
		np.netdb.(*fakeInventoryDB).GetNetInterfaceNameFunc = func(string) (string, error) { return small.host, nil }
		c := claim("slaac-small-mtu", `{"interface":{"addressing":"SLAAC"}}`)
		result := np.prepareResourceClaim(t.Context(), c)
		if result.Err == nil || !strings.Contains(result.Err.Error(), "requires an MTU of at least 1280") {
			t.Fatalf("prepareResourceClaim() error = %v, want the MTU error", result.Err)
		}
	})

	// Without an advertisement the prepare fails, and nothing is stored for
	// the Pod.
	t.Run("without a router", func(t *testing.T) {
		silent := newSlaacTestLink(t, "host1", "router1", nil)
		np := newDriver()
		np.netdb.(*fakeInventoryDB).GetNetInterfaceNameFunc = func(string) (string, error) { return silent.host, nil }
		ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
		defer cancel()
		c := claim("slaac-silent", `{"interface":{"addressing":"SLAAC"}}`)
		result := np.prepareResourceClaim(ctx, c)
		if result.Err == nil || !strings.Contains(result.Err.Error(), "SLAAC") {
			t.Fatalf("prepareResourceClaim() error = %v, want a SLAAC error", result.Err)
		}
		if podCfg, ok := np.podConfigStore.GetPodConfig(c.Status.ReservedFor[0].UID); ok {
			t.Errorf("pod configuration stored after a failed prepare: %+v", podCfg)
		}
	})
}
