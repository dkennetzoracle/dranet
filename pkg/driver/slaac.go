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
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"k8s.io/klog/v2"

	"sigs.k8s.io/dranet/internal/nlwrap"
	"sigs.k8s.io/dranet/pkg/apis"
)

const (
	icmpv6RouterSolicitation   = 133
	icmpv6RouterAdvertisement  = 134
	ndpOptionPrefixInformation = 3
	// ndpHopLimit is the hop limit every neighbor discovery message is sent
	// with; a router advertisement with any other value did not come from the
	// link and is discarded (RFC 4861 section 6.1.2).
	ndpHopLimit = 255
	// slaacPrefixLength is the only prefix length SLAAC forms addresses in on
	// Ethernet, whose interface identifiers are 64 bits (RFC 4862 section
	// 5.5.3, RFC 4291 section 2.5.1).
	slaacPrefixLength = 64
	// slaacSolicitInterval is how long the client waits for an advertisement
	// before it solicits again. The kernel waits 4s, which leaves no second
	// solicitation in a prepare timeout; routers may delay an answer by up to
	// 0.5s (RFC 4861 section 6.2.6).
	slaacSolicitInterval = 1 * time.Second
	// slaacReadTimeout bounds each read, so the loop notices the context
	// ending and the next solicitation falling due.
	slaacReadTimeout = 100 * time.Millisecond
)

// routerAdvertisement holds what SLAAC uses from a Router Advertisement
// (RFC 4861 section 4.2).
type routerAdvertisement struct {
	// routerLifetime is zero when the sender is not a default router.
	routerLifetime time.Duration
	// prefixes are the advertised prefixes an address may be formed in:
	// autonomous, on-link, /64, not link-local, and not deprecated.
	prefixes []netip.Prefix
}

// parseRouterAdvertisement parses the ICMPv6 message b, as a raw ICMPv6
// socket returns it, into the fields SLAAC uses. It rejects a message that is
// not a valid Router Advertisement and skips the prefixes SLAAC must ignore
// (RFC 4862 section 5.5.3) or that this client does not support.
func parseRouterAdvertisement(b []byte) (*routerAdvertisement, error) {
	// Type, code, checksum, current hop limit, flags, router lifetime,
	// reachable time and retransmission timer: 16 octets before the options.
	if len(b) < 16 {
		return nil, fmt.Errorf("router advertisement too short: %d octets", len(b))
	}
	if b[0] != icmpv6RouterAdvertisement || b[1] != 0 {
		return nil, fmt.Errorf("not a router advertisement: ICMPv6 type %d code %d", b[0], b[1])
	}
	ra := &routerAdvertisement{
		routerLifetime: time.Duration(binary.BigEndian.Uint16(b[6:8])) * time.Second,
	}
	for options := b[16:]; len(options) > 0; {
		if len(options) < 2 {
			return nil, fmt.Errorf("truncated router advertisement option")
		}
		// The length counts units of 8 octets, including the type and length.
		length := int(options[1]) * 8
		if length == 0 {
			return nil, fmt.Errorf("router advertisement option %d has length 0", options[0])
		}
		if length > len(options) {
			return nil, fmt.Errorf("router advertisement option %d is longer than the message", options[0])
		}
		if options[0] == ndpOptionPrefixInformation {
			if prefix, ok := autoconfigurablePrefix(options[:length]); ok {
				ra.prefixes = append(ra.prefixes, prefix)
			}
		}
		options = options[length:]
	}
	return ra, nil
}

// autoconfigurablePrefix returns the prefix of a Prefix Information option
// (RFC 4861 section 4.6.2) if SLAAC may form an address in it.
func autoconfigurablePrefix(option []byte) (netip.Prefix, bool) {
	if len(option) != 32 {
		return netip.Prefix{}, false
	}
	prefixLength := int(option[2])
	onLink := option[3]&0x80 != 0
	autonomous := option[3]&0x40 != 0
	validLifetime := binary.BigEndian.Uint32(option[4:8])
	preferredLifetime := binary.BigEndian.Uint32(option[8:12])
	addr := netip.AddrFrom16([16]byte(option[16:32]))
	switch {
	case !autonomous, prefixLength != slaacPrefixLength:
		return netip.Prefix{}, false
	case !onLink:
		// Not supported: the address is configured with its /64, which makes
		// the prefix on-link, and traffic within it would not go through the
		// router.
		return netip.Prefix{}, false
	case !addr.IsGlobalUnicast():
		// Also excludes link-local prefixes, which SLAAC ignores.
		return netip.Prefix{}, false
	case preferredLifetime > validLifetime:
		return netip.Prefix{}, false
	case preferredLifetime == 0:
		// Deprecated. SLAAC still forms an address that is not preferred, but
		// this address is configured without lifetimes and would be preferred.
		return netip.Prefix{}, false
	}
	return netip.PrefixFrom(addr, prefixLength).Masked(), true
}

// slaacAddress returns the address SLAAC configures in prefix for device of
// the claim with UID claimUID. The interface identifier is a hash of the
// three, like a stable interface identifier (RFC 7217) without the secret:
// the same each time the claim is prepared, different for each device and
// prefix, and independent of the hardware address, which an IPVLAN child
// shares with its parent.
func slaacAddress(prefix netip.Prefix, claimUID, device string) netip.Prefix {
	network := prefix.Masked().Addr().As16()
	for counter := uint32(0); ; counter++ {
		h := sha256.New()
		h.Write(network[:])
		// The terminators keep ("ab", "c") and ("a", "bc") apart.
		h.Write([]byte(claimUID + "\x00" + device + "\x00"))
		_ = binary.Write(h, binary.BigEndian, counter)
		id := binary.BigEndian.Uint64(h.Sum(nil))
		if reservedInterfaceID(id) {
			continue
		}
		b := network
		binary.BigEndian.PutUint64(b[8:], id)
		return netip.PrefixFrom(netip.AddrFrom16(b), prefix.Bits())
	}
}

// reservedInterfaceID reports whether id is a reserved interface identifier
// (RFC 5453): the Subnet-Router anycast identifier, the identifiers of the
// IANA Ethernet block (00-00-5E, as modified EUI-64), and the reserved subnet
// anycast identifiers.
func reservedInterfaceID(id uint64) bool {
	return id == 0 ||
		id>>24 == 0x02005efffe ||
		(id >= 0xfdffffffffffff80 && id <= 0xfdffffffffffffff)
}

// getSLAAC solicits a Router Advertisement on ifName and returns what
// stateless address autoconfiguration takes from it: the advertised
// autonomous /64 prefixes, and a default route through the advertising router
// when it is a default router. The caller forms the addresses with
// slaacAddress. Like getDHCP it runs on the host interface, and the caller
// applies the result as static configuration. It returns when an
// advertisement with a usable prefix arrives, or with an error when ctx ends
// first.
func getSLAAC(ctx context.Context, ifName string) (prefixes []netip.Prefix, routes []apis.RouteConfig, err error) {
	link, err := nlwrap.LinkByName(ifName)
	if err != nil {
		return nil, nil, err
	}
	if link.Attrs().OperState != netlink.OperUp {
		if err := netlink.LinkSetUp(link); err != nil {
			return nil, nil, fmt.Errorf("failed to set interface %s up: %w", ifName, err)
		}
	}
	// Without IPv6 on the interface the solicitation is dropped without an
	// error. The kernel removes the IPv6 settings of an interface whose mtu is
	// below the IPv6 minimum, so that case fails the read.
	disabled, err := sysctlProvider().GetSysctl("net/ipv6/conf/" + ifName + "/disable_ipv6")
	if err != nil {
		return nil, nil, fmt.Errorf("interface %s has no IPv6 settings, as when its mtu is below %d, so it cannot solicit a router advertisement: %w", ifName, apis.MinIPv6MTU, err)
	}
	if disabled != 0 {
		return nil, nil, fmt.Errorf("interface %s has IPv6 disabled, so it cannot solicit a router advertisement", ifName)
	}

	fd, err := ndpSocket(ifName, link.Attrs().Index)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open a neighbor discovery socket on interface %s: %w", ifName, err)
	}
	defer unix.Close(fd)

	allRouters := &unix.SockaddrInet6{ZoneId: uint32(link.Attrs().Index)}
	copy(allRouters.Addr[:], net.ParseIP("ff02::2").To16())
	// Type, code, checksum (filled in by the kernel) and 4 reserved octets.
	// No source link-layer address option, which RFC 4861 recommends but does
	// not require: routers answer with a multicast advertisement either way.
	solicitation := []byte{icmpv6RouterSolicitation, 0, 0, 0, 0, 0, 0, 0}

	// An advertisement may be as large as the link mtu, and jumbo frames
	// are common on the links SLAAC serves.
	buf := make([]byte, 1<<16)
	oob := make([]byte, unix.CmsgSpace(4))
	lastIgnored := "no router advertisement received"
	var nextSolicitation time.Time
	var solicitErr error
	for ctx.Err() == nil {
		if now := time.Now(); !now.Before(nextSolicitation) {
			// Fails with EADDRNOTAVAIL while the link-local address is still
			// tentative; the next solicitation tries again.
			if solicitErr = unix.Sendto(fd, solicitation, 0, allRouters); solicitErr != nil {
				klog.V(4).Infof("SLAAC on %s: router solicitation failed: %v", ifName, solicitErr)
			}
			nextSolicitation = now.Add(slaacSolicitInterval)
		}
		n, oobn, _, from, err := unix.Recvmsg(fd, buf, oob, 0)
		if err != nil {
			if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
				continue
			}
			return nil, nil, fmt.Errorf("failed to read from interface %s: %w", ifName, err)
		}
		router, ra, err := validRouterAdvertisement(buf[:n], oob[:oobn], from)
		if err != nil {
			lastIgnored = err.Error()
			klog.V(4).Infof("SLAAC on %s: ignoring message: %v", ifName, err)
			continue
		}
		if len(ra.prefixes) == 0 {
			lastIgnored = fmt.Sprintf("router advertisement from %s has no on-link, autonomous /%d prefix that is not deprecated", router, slaacPrefixLength)
			klog.V(4).Infof("SLAAC on %s: %s", ifName, lastIgnored)
			continue
		}
		if ra.routerLifetime > 0 {
			routes = append(routes, apis.RouteConfig{Destination: "::/0", Gateway: router.String()})
		}
		klog.V(2).Infof("SLAAC on %s: router %s advertised %v (router lifetime %v)", ifName, router, ra.prefixes, ra.routerLifetime)
		return ra.prefixes, routes, nil
	}
	if solicitErr != nil {
		lastIgnored += fmt.Sprintf("; the last router solicitation failed: %v", solicitErr)
	}
	return nil, nil, fmt.Errorf("no usable router advertisement on interface %s (%s): %w", ifName, lastIgnored, ctx.Err())
}

// ndpSocket opens a raw ICMPv6 socket on ifName that sends router
// solicitations and receives only router advertisements, with the hop limit
// they arrived with.
func ndpSocket(ifName string, ifIndex int) (int, error) {
	fd, err := unix.Socket(unix.AF_INET6, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.IPPROTO_ICMPV6)
	if err != nil {
		return -1, err
	}
	var filter unix.ICMPv6Filter
	for i := range filter.Data {
		filter.Data[i] = 0xffffffff // a set bit blocks the type
	}
	filter.Data[icmpv6RouterAdvertisement>>5] &^= 1 << (icmpv6RouterAdvertisement & 31)
	readTimeout := unix.NsecToTimeval(slaacReadTimeout.Nanoseconds())
	for _, set := range []func() error{
		func() error { return unix.BindToDevice(fd, ifName) },
		func() error { return unix.SetsockoptICMPv6Filter(fd, unix.SOL_ICMPV6, unix.ICMPV6_FILTER, &filter) },
		func() error { return unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_RECVHOPLIMIT, 1) },
		func() error { return unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_MULTICAST_HOPS, ndpHopLimit) },
		func() error { return unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_MULTICAST_IF, ifIndex) },
		func() error { return unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &readTimeout) },
	} {
		if err := set(); err != nil {
			unix.Close(fd)
			return -1, err
		}
	}
	return fd, nil
}

// validRouterAdvertisement checks that a received message is a router
// advertisement from the link (RFC 4861 section 6.1.2), and returns its
// sender and contents.
func validRouterAdvertisement(message, oob []byte, from unix.Sockaddr) (netip.Addr, *routerAdvertisement, error) {
	sender, ok := from.(*unix.SockaddrInet6)
	if !ok {
		return netip.Addr{}, nil, fmt.Errorf("message from a non-IPv6 address %v", from)
	}
	router := netip.AddrFrom16(sender.Addr)
	if !router.IsLinkLocalUnicast() {
		return netip.Addr{}, nil, fmt.Errorf("router advertisement from %s, which is not a link-local address", router)
	}
	hopLimit, err := receivedHopLimit(oob)
	if err != nil {
		return netip.Addr{}, nil, err
	}
	if hopLimit != ndpHopLimit {
		return netip.Addr{}, nil, fmt.Errorf("router advertisement from %s arrived with hop limit %d, not %d", router, hopLimit, ndpHopLimit)
	}
	ra, err := parseRouterAdvertisement(message)
	if err != nil {
		return netip.Addr{}, nil, fmt.Errorf("invalid router advertisement from %s: %w", router, err)
	}
	return router, ra, nil
}

// receivedHopLimit returns the IPV6_HOPLIMIT control message of a received
// packet.
func receivedHopLimit(oob []byte) (int, error) {
	messages, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return 0, fmt.Errorf("failed to parse control messages: %w", err)
	}
	for _, m := range messages {
		if m.Header.Level == unix.IPPROTO_IPV6 && m.Header.Type == unix.IPV6_HOPLIMIT && len(m.Data) >= 4 {
			return int(int32(binary.NativeEndian.Uint32(m.Data[:4]))), nil
		}
	}
	return 0, fmt.Errorf("no hop limit in the received message")
}
