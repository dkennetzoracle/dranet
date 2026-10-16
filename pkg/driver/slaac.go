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
	"fmt"
	"net"
	"net/netip"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"k8s.io/klog/v2"

	"sigs.k8s.io/dranet/internal/nlwrap"
	"sigs.k8s.io/dranet/pkg/apis"
	"sigs.k8s.io/dranet/pkg/internal/slaac"
)

// slaacTimeout bounds the wait for a usable Router Advertisement, the same
// budget the DHCP exchange gets. Solicitations go out at 0s, 1s and 3s within
// it, so a router that misses the first one is still heard from.
const slaacTimeout = 5 * time.Second

// getSLAAC solicits a Router Advertisement on the host interface ifName and
// returns the IPv6 addresses and routes a host would autoconfigure from it,
// with id as the interface identifier. Like getDHCP, it only gathers the
// configuration: apart from bringing the link up, nothing is applied in the
// host namespace, and the result is configured statically in the Pod. The
// advertisement's lifetimes are not tracked, as a DHCP lease's are not: the
// Pod's addresses live as long as the Pod.
//
// The exchange goes over a packet socket, so it works whether or not the host
// has IPv6 enabled on the interface. When the interface has a link-local
// address that passed duplicate address detection, the solicitations are sent
// from it so routers can answer by unicast; otherwise they go out from the
// unspecified address and the routers answer all nodes (RFC 4861 section
// 6.2.6).
func getSLAAC(ctx context.Context, ifName string, id slaac.InterfaceIdentifier) (addresses []string, routes []apis.RouteConfig, err error) {
	link, err := nlwrap.LinkByName(ifName)
	if err != nil {
		return nil, nil, err
	}
	// Neighbor discovery maps IPv6 multicast to Ethernet addresses here; other
	// links, such as IPoIB, map it differently (RFC 4391).
	if encap := link.Attrs().EncapType; encap != "ether" {
		return nil, nil, fmt.Errorf("interface %s is not an Ethernet interface (%s); SLAAC addressing needs one", ifName, encap)
	}
	if link.Attrs().OperState != netlink.OperUp {
		if err := netlink.LinkSetUp(link); err != nil {
			return nil, nil, fmt.Errorf("failed to set interface %s up: %w", ifName, err)
		}
		// Solicitations sent before the link has its carrier are lost.
		if err := waitForCarrier(ctx, ifName); err != nil {
			return nil, nil, err
		}
	}
	ifi, err := net.InterfaceByName(ifName)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get interface %s: %w", ifName, err)
	}

	// An advertisement is only useful if an address can be formed from it; a
	// router that only announces itself, or a link where addresses come from
	// DHCPv6, is remembered so the failure says what the link offered.
	var unusable string
	accept := func(ra *slaac.RouterAdvertisement, router netip.Addr) bool {
		if len(slaac.Autoconfigure(ra, router, id).Addresses) > 0 {
			return true
		}
		unusable = describeUnusableAdvertisement(ra, router)
		klog.V(2).Infof("ignoring router advertisement on %s: %s", ifName, unusable)
		return false
	}
	ra, router, err := slaac.Solicit(ctx, ifi, linkLocalAddress(link), accept)
	if err != nil {
		if unusable != "" {
			return nil, nil, fmt.Errorf("%w; last advertisement: %s", err, unusable)
		}
		return nil, nil, err
	}

	autoconf := slaac.Autoconfigure(ra, router, id)
	for _, address := range autoconf.Addresses {
		addresses = append(addresses, address.String())
	}
	for _, route := range autoconf.Routes {
		routeCfg := apis.RouteConfig{Destination: route.Destination.String()}
		if route.Gateway.IsValid() {
			routeCfg.Gateway = route.Gateway.String()
		} else {
			routeCfg.Scope = unix.RT_SCOPE_LINK
		}
		routes = append(routes, routeCfg)
	}
	klog.V(2).Infof("router %v on %s advertised %d prefix(es) and %d route(s): configuring addresses %v and routes %+v", router, ifName, len(ra.Prefixes), len(ra.Routes), addresses, routes)
	if ra.MTU != 0 && int(ra.MTU) != link.Attrs().MTU {
		klog.V(2).Infof("router %v on %s advertises MTU %d, but the interface has MTU %d; the advertised MTU is not applied", router, ifName, ra.MTU, link.Attrs().MTU)
	}
	return addresses, routes, nil
}

// waitForCarrier waits until the link ifName, just set up, has its carrier,
// as a NIC may take seconds to. A link whose driver reports no operational
// state is taken as ready.
func waitForCarrier(ctx context.Context, ifName string) error {
	for {
		link, err := nlwrap.LinkByName(ifName)
		if err != nil {
			return err
		}
		if state := link.Attrs().OperState; state != netlink.OperDown && state != netlink.OperLowerLayerDown {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("interface %s has no carrier: %w", ifName, ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// describeUnusableAdvertisement says why no address could be formed from an
// advertisement, in the terms an operator can check on the router.
func describeUnusableAdvertisement(ra *slaac.RouterAdvertisement, router netip.Addr) string {
	autonomous := 0
	for _, p := range ra.Prefixes {
		if p.Autonomous {
			autonomous++
		}
	}
	var why string
	switch {
	case len(ra.Prefixes) == 0:
		why = fmt.Sprintf("router %v advertises no prefix", router)
	case autonomous == 0:
		why = fmt.Sprintf("router %v advertises %d prefix(es), none with the autonomous flag", router, len(ra.Prefixes))
	default:
		why = fmt.Sprintf("router %v advertises %d autonomous prefix(es), none usable: a prefix must be a global /64 with a non-zero preferred lifetime not longer than its valid lifetime", router, autonomous)
	}
	if ra.ManagedConfiguration {
		why += ", and sets the managed address configuration flag: addresses on this link come from DHCPv6, which is not supported"
	}
	return why
}

// linkLocalAddress returns a link-local address of link that has completed
// duplicate address detection, or the unspecified address if there is none,
// for example because the host has IPv6 disabled on the interface or the link
// just came up. A tentative address must not be used as a source yet
// (RFC 4862 section 5.4), nor an optimistic one for a solicitation that
// carries the hardware address (RFC 4429 section 3.2).
func linkLocalAddress(link netlink.Link) netip.Addr {
	addrs, err := nlwrap.AddrList(link, netlink.FAMILY_V6)
	if err != nil {
		klog.V(2).Infof("failed to list the IPv6 addresses of %s, soliciting from the unspecified address: %v", link.Attrs().Name, err)
		return netip.IPv6Unspecified()
	}
	for _, addr := range addrs {
		if addr.IP == nil || !addr.IP.IsLinkLocalUnicast() || addr.Flags&(unix.IFA_F_TENTATIVE|unix.IFA_F_OPTIMISTIC|unix.IFA_F_DADFAILED) != 0 {
			continue
		}
		if ip, ok := netip.AddrFromSlice(addr.IP); ok {
			return ip
		}
	}
	return netip.IPv6Unspecified()
}
