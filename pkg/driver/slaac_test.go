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
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"path"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/dranet/internal/nlwrap"
	userns "sigs.k8s.io/dranet/internal/testutils"
	"sigs.k8s.io/dranet/pkg/apis"
)

func TestReadinessBudget(t *testing.T) {
	const (
		max     = 1500 * time.Millisecond
		reserve = 500 * time.Millisecond
	)

	tests := []struct {
		name         string
		deadline     time.Duration // 0 means no deadline
		want         time.Duration
		wantExceeded bool
	}{
		{
			name: "no deadline falls back to the maximum",
			want: max,
		},
		{
			name:     "a deadline beyond the maximum does not extend the wait",
			deadline: 10 * time.Second,
			want:     max,
		},
		{
			name:     "a tight deadline shortens the wait by the reserve",
			deadline: 900 * time.Millisecond,
			want:     400 * time.Millisecond,
		},
		{
			// Time to roll back, not to wait as well: a zero budget makes the
			// caller roll back at once and stay inside the request deadline.
			name:     "a deadline inside the reserve leaves no budget",
			deadline: 300 * time.Millisecond,
			want:     0,
		},
		{
			// Nothing is left to protect with a rollback once the runtime has
			// stopped waiting for the request, so the caller gets the full
			// budget and a heads-up instead of being told to give up.
			name:         "an expired deadline is treated as exceeded",
			deadline:     -time.Second,
			want:         max,
			wantExceeded: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			if tt.deadline != 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, time.Now().Add(tt.deadline))
				defer cancel()
			}

			got, exceeded := readinessBudget(ctx, max, reserve)
			if exceeded != tt.wantExceeded {
				t.Errorf("readinessBudget() deadlineExceeded = %v, want %v", exceeded, tt.wantExceeded)
			}
			// time.Until loses a little to the clock between the two calls.
			if diff := tt.want - got; diff < 0 || diff > 50*time.Millisecond {
				t.Errorf("readinessBudget() = %v, want approximately %v", got, tt.want)
			}
		})
	}
}

func TestAutoconfiguredAddresses(t *testing.T) {
	addr := func(cidr string, flags int) netlink.Addr {
		ip, ipnet, err := net.ParseCIDR(cidr)
		if err != nil {
			t.Fatalf("bad test address %s: %v", cidr, err)
		}
		return netlink.Addr{IPNet: &net.IPNet{IP: ip, Mask: ipnet.Mask}, Flags: flags}
	}

	tests := []struct {
		name      string
		addresses []netlink.Addr
		want      []string
	}{
		{
			name: "no addresses",
		},
		{
			name:      "an autoconfigured address is ready",
			addresses: []netlink.Addr{addr("2001:db8::1/64", 0)},
			want:      []string{"2001:db8::1/64"},
		},
		{
			name:      "a link-local address is not enough",
			addresses: []netlink.Addr{addr("fe80::1/64", 0)},
		},
		{
			name:      "a tentative address is still running duplicate address detection",
			addresses: []netlink.Addr{addr("2001:db8::1/64", unix.IFA_F_TENTATIVE)},
		},
		{
			name:      "an address that failed duplicate address detection is unusable",
			addresses: []netlink.Addr{addr("2001:db8::1/64", unix.IFA_F_DADFAILED)},
		},
		{
			name:      "a permanent address did not come from a router advertisement",
			addresses: []netlink.Addr{addr("2001:db8::1/64", unix.IFA_F_PERMANENT)},
		},
		{
			name: "the ready addresses are picked out of a mixed list",
			addresses: []netlink.Addr{
				addr("fe80::1/64", unix.IFA_F_PERMANENT),
				addr("2001:db8::1/64", unix.IFA_F_TENTATIVE),
				addr("fdcd:8200:cde5:20b7::708/64", unix.IFA_F_MANAGETEMPADDR),
			},
			want: []string{"fdcd:8200:cde5:20b7::708/64"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if diff := cmp.Diff(tt.want, autoconfiguredAddresses(tt.addresses)); diff != "" {
				t.Errorf("autoconfiguredAddresses() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestDescribeAddresses(t *testing.T) {
	if got := describeAddresses(nil); got != "none" {
		t.Errorf("describeAddresses(nil) = %q, want %q", got, "none")
	}
	addresses := []netlink.Addr{
		{IPNet: &net.IPNet{IP: net.ParseIP("fe80::1"), Mask: net.CIDRMask(64, 128)}, Flags: unix.IFA_F_PERMANENT},
		{IPNet: &net.IPNet{IP: net.ParseIP("2001:db8::1"), Mask: net.CIDRMask(64, 128)}, Flags: unix.IFA_F_TENTATIVE},
	}
	want := "fe80::1(permanent) 2001:db8::1(tentative)"
	if got := describeAddresses(addresses); got != want {
		t.Errorf("describeAddresses() = %q, want %q", got, want)
	}
}

// testNetns creates a named network namespace with a dummy interface inside it
// and returns the namespace path and the interface name.
func testNetns(t *testing.T) (string, netns.NsHandle, string) {
	t.Helper()

	origns, err := netns.Get()
	if err != nil {
		t.Fatalf("failed to get the current namespace: %v", err)
	}
	t.Cleanup(func() { origns.Close() })

	rndString := make([]byte, 4)
	if _, err := rand.Read(rndString); err != nil {
		t.Fatalf("failed to generate a random name: %v", err)
	}
	nsName := fmt.Sprintf("ns%x", rndString)
	// NewNamed unshares the calling OS thread and leaves it in the new
	// namespace. Pin the goroutine to that thread until the original namespace
	// is restored, so the restore lands on the thread that was moved.
	runtime.LockOSThread()
	testNS, err := netns.NewNamed(nsName)
	if err != nil {
		runtime.UnlockOSThread()
		t.Fatalf("failed to create a network namespace: %v", err)
	}
	t.Cleanup(func() {
		testNS.Close()
		netns.DeleteNamed(nsName)
	})
	if err := netns.Set(origns); err != nil {
		runtime.UnlockOSThread()
		t.Fatalf("failed to restore the original namespace: %v", err)
	}
	runtime.UnlockOSThread()

	ifName := "slaac0"
	nhNs, err := nlwrap.NewHandleAt(testNS)
	if err != nil {
		t.Fatalf("failed to open a netlink handle: %v", err)
	}
	t.Cleanup(nhNs.Close)

	la := netlink.NewLinkAttrs()
	la.Name = ifName
	if err := nhNs.LinkAdd(&netlink.Dummy{LinkAttrs: la}); err != nil {
		t.Fatalf("failed to add dummy link %s: %v", ifName, err)
	}
	link, err := nhNs.LinkByName(ifName)
	if err != nil {
		t.Fatalf("failed to get link %s: %v", ifName, err)
	}
	if err := nhNs.LinkSetUp(link); err != nil {
		t.Fatalf("failed to set up link %s: %v", ifName, err)
	}
	return path.Join("/run/netns", nsName), testNS, ifName
}

func TestWaitForSLAAC(t *testing.T) {
	userns.Run(t, testWaitForSLAAC_Namespaced, syscall.CLONE_NEWNET, syscall.CLONE_NEWNS)
}

func testWaitForSLAAC_Namespaced(t *testing.T) {
	_, testNS, ifName := testNetns(t)

	nhNs, err := nlwrap.NewHandleAt(testNS)
	if err != nil {
		t.Fatalf("failed to open a netlink handle: %v", err)
	}
	defer nhNs.Close()
	link, err := nhNs.LinkByName(ifName)
	if err != nil {
		t.Fatalf("failed to get link %s: %v", ifName, err)
	}

	// A router advertisement arrives while the wait is already running. Lifetimes
	// are what make the kernel treat the address as dynamic rather than
	// permanent, the same as an autoconfigured one.
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = nhNs.AddrAdd(link, &netlink.Addr{
			IPNet:       &net.IPNet{IP: net.ParseIP("2001:db8::1"), Mask: net.CIDRMask(64, 128)},
			ValidLft:    3600,
			PreferedLft: 1800,
			Flags:       unix.IFA_F_NODAD,
		})
	}()

	addresses, err := waitForSLAAC(context.Background(), testNS, ifName, time.Second)
	if err != nil {
		t.Fatalf("waitForSLAAC() error: %v", err)
	}
	if diff := cmp.Diff([]string{"2001:db8::1/64"}, addresses); diff != "" {
		t.Errorf("waitForSLAAC() addresses mismatch (-want +got):\n%s", diff)
	}
}

func TestWaitForSLAACTimesOut(t *testing.T) {
	userns.Run(t, testWaitForSLAACTimesOut_Namespaced, syscall.CLONE_NEWNET, syscall.CLONE_NEWNS)
}

func testWaitForSLAACTimesOut_Namespaced(t *testing.T) {
	_, testNS, ifName := testNetns(t)

	start := time.Now()
	_, err := waitForSLAAC(context.Background(), testNS, ifName, 150*time.Millisecond)
	if err == nil {
		t.Fatal("waitForSLAAC() succeeded on an interface with no router, want an error")
	}
	if !strings.Contains(err.Error(), ifName) {
		t.Errorf("waitForSLAAC() error %q does not name the interface %s", err, ifName)
	}
	// The wait must honour its budget rather than block the runtime request.
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("waitForSLAAC() took %v, want it to give up near its 150ms budget", elapsed)
	}
}

func TestWaitForSLAACUnknownInterface(t *testing.T) {
	userns.Run(t, testWaitForSLAACUnknownInterface_Namespaced, syscall.CLONE_NEWNET, syscall.CLONE_NEWNS)
}

func testWaitForSLAACUnknownInterface_Namespaced(t *testing.T) {
	_, testNS, _ := testNetns(t)

	if _, err := waitForSLAAC(context.Background(), testNS, "nosuchif", time.Second); err == nil {
		t.Fatal("waitForSLAAC() succeeded for an interface that does not exist, want an error")
	}
}

// addHostDummy creates a dummy interface on the host side of the test and
// removes it again at the end, wherever it has ended up.
func addHostDummy(t *testing.T, name string) {
	t.Helper()
	la := netlink.NewLinkAttrs()
	la.Name = name
	if err := netlink.LinkAdd(&netlink.Dummy{LinkAttrs: la}); err != nil {
		t.Fatalf("failed to add dummy link %s: %v", name, err)
	}
	t.Cleanup(func() {
		if link, err := nlwrap.LinkByName(name); err == nil {
			_ = netlink.LinkDel(link)
		}
	})
}

// TestAwaitSLAACReadyNoRouter checks that the wait gives up within its budget
// when nothing on the link answers, and leaves the rollback to the caller: the
// interface is still in the Pod namespace afterwards.
func TestAwaitSLAACReadyNoRouter(t *testing.T) {
	userns.Run(t, testAwaitSLAACReadyNoRouter_Namespaced, syscall.CLONE_NEWNET, syscall.CLONE_NEWNS)
}

func testAwaitSLAACReadyNoRouter_Namespaced(t *testing.T) {
	containerNsPath, _, _ := testNetns(t)

	hostIfName := "slaachost0"
	addHostDummy(t, hostIfName)

	ifNameInNs := "slaacpod0"
	config := apis.InterfaceConfig{
		Name:                    ifNameInNs,
		Addressing:              apis.AddressingModeSLAAC,
		AcceptRA:                ptr.To[int32](2),
		DADTransmits:            ptr.To[int32](0),
		RouterSolicitationDelay: ptr.To[int32](0),
	}
	if _, err := nsAttachNetdev(hostIfName, containerNsPath, config); err != nil {
		t.Fatalf("nsAttachNetdev() error: %v", err)
	}

	start := time.Now()
	_, err := awaitSLAACReady(context.Background(), containerNsPath, ifNameInNs, 150*time.Millisecond, DefaultSLAACRollbackReserve)
	if err == nil {
		t.Fatal("awaitSLAACReady() succeeded with no router on the link, want an error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("awaitSLAACReady() error %q does not wrap context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("awaitSLAACReady() took %v, want it to give up near its 150ms budget", elapsed)
	}
	if _, err := nlwrap.LinkByName(hostIfName); err == nil {
		t.Errorf("interface %s is back on the host, but awaitSLAACReady must leave the rollback to its caller", hostIfName)
	}
}

// TestRollbackAttachedDevices checks that everything a request moved into the
// Pod namespace comes back to the host, under the original names and up, so the
// kubelet's retry of the sandbox starts from the same state as the first attempt.
func TestRollbackAttachedDevices(t *testing.T) {
	userns.Run(t, testRollbackAttachedDevices_Namespaced, syscall.CLONE_NEWNET, syscall.CLONE_NEWNS)
}

func testRollbackAttachedDevices_Namespaced(t *testing.T) {
	containerNsPath, containerNs, _ := testNetns(t)

	var attached []attachedDevice
	for i, names := range [][2]string{{"slaachost4", "slaacpod4"}, {"slaachost5", "slaacpod5"}} {
		hostIfName, ifNameInNs := names[0], names[1]
		addHostDummy(t, hostIfName)
		if _, err := nsAttachNetdev(hostIfName, containerNsPath, apis.InterfaceConfig{Name: ifNameInNs}); err != nil {
			t.Fatalf("nsAttachNetdev(%s) error: %v", hostIfName, err)
		}
		attached = append(attached, attachedDevice{deviceName: fmt.Sprintf("dev%d", i), hostIfName: hostIfName, ifNameInNs: ifNameInNs})
	}

	rescan, err := rollbackAttachedDevices(context.Background(), containerNsPath, attached, true)
	if err != nil {
		t.Fatalf("rollbackAttachedDevices() error: %v", err)
	}
	if rescan {
		t.Errorf("rollbackAttachedDevices() asked for a rescan with no RDMA device returned")
	}

	nhNs, err := nlwrap.NewHandleAt(containerNs)
	if err != nil {
		t.Fatalf("failed to open a netlink handle: %v", err)
	}
	defer nhNs.Close()
	for _, dev := range attached {
		link, err := nlwrap.LinkByName(dev.hostIfName)
		if err != nil {
			t.Errorf("interface %s was not returned to the host: %v", dev.hostIfName, err)
			continue
		}
		if link.Attrs().Flags&net.FlagUp == 0 {
			t.Errorf("interface %s was returned to the host but left down", dev.hostIfName)
		}
		if _, err := nhNs.LinkByName(dev.ifNameInNs); err == nil {
			t.Errorf("interface %s is still in the Pod namespace", dev.ifNameInNs)
		}
	}
}

// TestAwaitSLAACReadyPastDeadline checks that a request whose deadline has
// already passed still finishes the wait, instead of giving up on an interface
// in a Pod the runtime is going to start anyway.
func TestAwaitSLAACReadyPastDeadline(t *testing.T) {
	userns.Run(t, testAwaitSLAACReadyPastDeadline_Namespaced, syscall.CLONE_NEWNET, syscall.CLONE_NEWNS)
}

func testAwaitSLAACReadyPastDeadline_Namespaced(t *testing.T) {
	containerNsPath, _, _ := testNetns(t)

	hostIfName := "slaachost1"
	addHostDummy(t, hostIfName)

	ifNameInNs := "slaacpod1"
	config := apis.InterfaceConfig{Name: ifNameInNs, Addressing: apis.AddressingModeSLAAC}
	if _, err := nsAttachNetdev(hostIfName, containerNsPath, config); err != nil {
		t.Fatalf("nsAttachNetdev() error: %v", err)
	}

	containerNs, err := netns.GetFromPath(containerNsPath)
	if err != nil {
		t.Fatalf("failed to open the container namespace: %v", err)
	}
	defer containerNs.Close()
	nhNs, err := nlwrap.NewHandleAt(containerNs)
	if err != nil {
		t.Fatalf("failed to open a netlink handle: %v", err)
	}
	defer nhNs.Close()
	link, err := nhNs.LinkByName(ifNameInNs)
	if err != nil {
		t.Fatalf("failed to get link %s: %v", ifNameInNs, err)
	}

	// The address turns up after the deadline has already passed.
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = nhNs.AddrAdd(link, &netlink.Addr{
			IPNet:       &net.IPNet{IP: net.ParseIP("2001:db8::2"), Mask: net.CIDRMask(64, 128)},
			ValidLft:    3600,
			PreferedLft: 1800,
			Flags:       unix.IFA_F_NODAD,
		})
	}()

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	addresses, err := awaitSLAACReady(ctx, containerNsPath, ifNameInNs, time.Second, DefaultSLAACRollbackReserve)
	if err != nil {
		t.Fatalf("awaitSLAACReady() gave up after the deadline passed: %v", err)
	}
	if diff := cmp.Diff([]string{"2001:db8::2/64"}, addresses); diff != "" {
		t.Errorf("awaitSLAACReady() addresses mismatch (-want +got):\n%s", diff)
	}
}

// TestAwaitSLAACReadyWithinReserveFailsFast checks the middle case: the request
// is still live but has no more than the rollback reserve left, so there is no
// time to wait. The check runs once and fails at once, leaving the caller the
// whole remainder for the rollback.
func TestAwaitSLAACReadyWithinReserveFailsFast(t *testing.T) {
	userns.Run(t, testAwaitSLAACReadyWithinReserveFailsFast_Namespaced, syscall.CLONE_NEWNET, syscall.CLONE_NEWNS)
}

func testAwaitSLAACReadyWithinReserveFailsFast_Namespaced(t *testing.T) {
	containerNsPath, _, _ := testNetns(t)

	hostIfName := "slaachost2"
	addHostDummy(t, hostIfName)

	ifNameInNs := "slaacpod2"
	config := apis.InterfaceConfig{Name: ifNameInNs, Addressing: apis.AddressingModeSLAAC}
	if _, err := nsAttachNetdev(hostIfName, containerNsPath, config); err != nil {
		t.Fatalf("nsAttachNetdev() error: %v", err)
	}

	// Less than the reserve remains, so the wait must not start.
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(200*time.Millisecond))
	defer cancel()

	start := time.Now()
	_, err := awaitSLAACReady(ctx, containerNsPath, ifNameInNs, time.Second, DefaultSLAACRollbackReserve)
	if err == nil {
		t.Fatal("awaitSLAACReady() succeeded with no time to wait, want an error")
	}
	if elapsed := time.Since(start); elapsed > 150*time.Millisecond {
		t.Errorf("awaitSLAACReady() took %v, want a single check well inside the 200ms left", elapsed)
	}
}

// TestSLAACFromRouterAdvertisement drives the real kernel path rather than
// adding the address by hand: one end of a veth pair moves into the Pod
// namespace with the settings SLAAC defaults to, a router advertisement
// arrives on its link from the other end, and the wait returns the address the
// kernel derived from the advertised prefix.
func TestSLAACFromRouterAdvertisement(t *testing.T) {
	userns.Run(t, testSLAACFromRouterAdvertisement_Namespaced, syscall.CLONE_NEWNET, syscall.CLONE_NEWNS)
}

func testSLAACFromRouterAdvertisement_Namespaced(t *testing.T) {
	containerNsPath, _, _ := testNetns(t)

	// One end plays the rail NIC and moves into the Pod; the other stays here
	// and plays the router.
	hostIfName, routerIfName := "slaacnic0", "slaacrtr0"
	if err := netlink.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: hostIfName}, PeerName: routerIfName}); err != nil {
		t.Fatalf("failed to add veth pair %s/%s: %v", hostIfName, routerIfName, err)
	}
	t.Cleanup(func() {
		// Deleting either end removes the pair, wherever the other end is.
		if link, err := nlwrap.LinkByName(routerIfName); err == nil {
			_ = netlink.LinkDel(link)
		}
	})
	router, err := nlwrap.LinkByName(routerIfName)
	if err != nil {
		t.Fatalf("failed to get link %s: %v", routerIfName, err)
	}
	// The router needs a link-local source address that is not tentative to
	// send from. Skipping its duplicate address detection is a shortcut; if
	// it is not allowed here, the sender below retries until detection ends.
	_ = os.WriteFile("/proc/sys/net/ipv6/conf/"+routerIfName+"/dad_transmits", []byte("0"), 0o644)
	if err := netlink.LinkSetUp(router); err != nil {
		t.Fatalf("failed to set up link %s: %v", routerIfName, err)
	}

	ifNameInNs := "slaacpod3"
	config := apis.InterfaceConfig{
		Name:                    ifNameInNs,
		Addressing:              apis.AddressingModeSLAAC,
		AcceptRA:                ptr.To[int32](2),
		DADTransmits:            ptr.To[int32](0),
		RouterSolicitationDelay: ptr.To[int32](0),
	}
	if _, err := nsAttachNetdev(hostIfName, containerNsPath, config); err != nil {
		t.Fatalf("nsAttachNetdev() error: %v", err)
	}

	_, prefix, err := net.ParseCIDR("2001:db8:1::/64")
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	defer close(stop)
	go advertisePrefix(t, router.Attrs().Index, *prefix, stop)

	addresses, err := awaitSLAACReady(context.Background(), containerNsPath, ifNameInNs, 3*time.Second, DefaultSLAACRollbackReserve)
	if err != nil {
		t.Fatalf("awaitSLAACReady() error: %v", err)
	}
	if len(addresses) != 1 || !strings.HasPrefix(addresses[0], "2001:db8:1:") || !strings.HasSuffix(addresses[0], "/64") {
		t.Errorf("awaitSLAACReady() = %v, want one address in 2001:db8:1::/64", addresses)
	}
}

// advertisePrefix sends an unsolicited Router Advertisement for prefix out of
// the interface with index ifIndex every 50ms until stop is closed. It reports
// failures through t.Log only: the test that uses it fails on the wait instead,
// with the diagnostic the wait produces.
func advertisePrefix(t *testing.T, ifIndex int, prefix net.IPNet, stop <-chan struct{}) {
	fd, err := unix.Socket(unix.AF_INET6, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.IPPROTO_ICMPV6)
	if err != nil {
		t.Logf("failed to open a raw ICMPv6 socket: %v", err)
		return
	}
	defer unix.Close(fd)
	// Neighbor discovery messages are only valid with a hop limit of 255.
	if err := unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_MULTICAST_HOPS, 255); err != nil {
		t.Logf("failed to set the multicast hop limit: %v", err)
	}
	if err := unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_MULTICAST_IF, ifIndex); err != nil {
		t.Logf("failed to bind multicast sends to interface %d: %v", ifIndex, err)
	}
	allNodes := &unix.SockaddrInet6{ZoneId: uint32(ifIndex)}
	copy(allNodes.Addr[:], net.ParseIP("ff02::1").To16())

	packet := routerAdvertisement(prefix)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			// EADDRNOTAVAIL until the router's link-local address is usable;
			// the next tick tries again.
			_ = unix.Sendto(fd, packet, 0, allNodes)
		}
	}
}

// routerAdvertisement builds an ICMPv6 Router Advertisement (RFC 4861 section
// 4.2) carrying one Prefix Information option (section 4.6.2) with the
// autonomous flag set, which is what makes a host derive an address from it.
// The kernel fills in the checksum for raw ICMPv6 sockets.
func routerAdvertisement(prefix net.IPNet) []byte {
	ra := make([]byte, 16+32)
	ra[0] = 134                               // Router Advertisement
	ra[4] = 64                                // current hop limit
	binary.BigEndian.PutUint16(ra[6:8], 1800) // router lifetime, seconds

	option := ra[16:]
	option[0] = 3 // Prefix Information
	option[1] = 4 // length, in units of 8 octets
	ones, _ := prefix.Mask.Size()
	option[2] = byte(ones)
	option[3] = 0xc0                               // on-link, autonomous
	binary.BigEndian.PutUint32(option[4:8], 3600)  // valid lifetime
	binary.BigEndian.PutUint32(option[8:12], 1800) // preferred lifetime
	copy(option[16:32], prefix.IP.To16())
	return ra
}

// TestRollbackRestoresHostLinkState checks that an interface returned to the
// host gets back what the move dropped: its master, since a VRF slave would
// otherwise come back outside its VRF, and its MTU.
func TestRollbackRestoresHostLinkState(t *testing.T) {
	userns.Run(t, testRollbackRestoresHostLinkState_Namespaced, syscall.CLONE_NEWNET, syscall.CLONE_NEWNS)
}

func testRollbackRestoresHostLinkState_Namespaced(t *testing.T) {
	containerNsPath, _, _ := testNetns(t)

	vrf := &netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: "vrfslaac9"}, Table: 90}
	if err := netlink.LinkAdd(vrf); err != nil {
		t.Fatalf("failed to add VRF: %v", err)
	}
	t.Cleanup(func() { _ = netlink.LinkDel(vrf) })
	if err := netlink.LinkSetUp(vrf); err != nil {
		t.Fatalf("failed to set up VRF: %v", err)
	}

	hostIfName := "slaachost6"
	addHostDummy(t, hostIfName)
	host, err := nlwrap.LinkByName(hostIfName)
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetMTU(host, 1400); err != nil {
		t.Fatalf("failed to set MTU: %v", err)
	}
	if err := netlink.LinkSetMaster(host, vrf); err != nil {
		t.Fatalf("failed to enslave %s to the VRF: %v", hostIfName, err)
	}
	if err := netlink.LinkSetUp(host); err != nil {
		t.Fatal(err)
	}
	// Re-read the link: the attributes above were captured before it changed.
	if host, err = nlwrap.LinkByName(hostIfName); err != nil {
		t.Fatal(err)
	}
	state := hostLinkState(host, nil)
	if state.Master != vrf.Name || state.MTU != 1400 {
		t.Fatalf("hostLinkState() = %+v, want master %s and MTU 1400", state, vrf.Name)
	}

	// The Pod configuration changes the MTU, and the move itself drops the master.
	ifNameInNs := "slaacpod6"
	if _, err := nsAttachNetdev(hostIfName, containerNsPath, apis.InterfaceConfig{Name: ifNameInNs, MTU: ptr.To[int32](1300)}); err != nil {
		t.Fatalf("nsAttachNetdev() error: %v", err)
	}

	attached := []attachedDevice{{deviceName: "dev6", hostIfName: hostIfName, ifNameInNs: ifNameInNs, hostState: state}}
	if _, err := rollbackAttachedDevices(context.Background(), containerNsPath, attached, true); err != nil {
		t.Fatalf("rollbackAttachedDevices() error: %v", err)
	}

	host, err = nlwrap.LinkByName(hostIfName)
	if err != nil {
		t.Fatalf("interface %s was not returned to the host: %v", hostIfName, err)
	}
	if host.Attrs().MasterIndex != vrf.Attrs().Index {
		t.Errorf("interface %s came back with master index %d, want the VRF (%d)", hostIfName, host.Attrs().MasterIndex, vrf.Attrs().Index)
	}
	if host.Attrs().MTU != 1400 {
		t.Errorf("interface %s came back with MTU %d, want 1400", hostIfName, host.Attrs().MTU)
	}
	if host.Attrs().Flags&net.FlagUp == 0 {
		t.Errorf("interface %s came back down", hostIfName)
	}
}

// TestReturnHostLink covers the interface the kernel hands back on its own when
// a namespace is destroyed before DraNet could return it: found under its
// fallback name through the PCI function, renamed back, restored and brought up.
func TestReturnHostLink(t *testing.T) {
	userns.Run(t, testReturnHostLink_Namespaced, syscall.CLONE_NEWNET, syscall.CLONE_NEWNS)
}

func testReturnHostLink_Namespaced(t *testing.T) {
	// Nothing there at all: not an error, just not found.
	found, err := returnHostLink("slaacgone0", &HostLinkState{PCIAddress: "0000:ff:00.0"})
	if found || err != nil {
		t.Fatalf("returnHostLink() for a missing interface = (%v, %v), want (false, nil)", found, err)
	}

	// The kernel's fallback name for a returned device is dev<ifindex>.
	hostIfName := "slaachost7"
	addHostDummy(t, hostIfName)
	link, err := nlwrap.LinkByName(hostIfName)
	if err != nil {
		t.Fatal(err)
	}
	renamed := fmt.Sprintf("dev%d", link.Attrs().Index)
	if err := netlink.LinkSetName(link, renamed); err != nil {
		t.Fatalf("failed to rename %s to %s: %v", hostIfName, renamed, err)
	}
	t.Cleanup(func() {
		if link, err := nlwrap.LinkByName(renamed); err == nil {
			_ = netlink.LinkDel(link)
		}
	})

	previous := hostNetdevsForPCI
	hostNetdevsForPCI = func(pciAddress string) ([]string, error) {
		if pciAddress != "0000:03:00.4" {
			return nil, fmt.Errorf("unexpected PCI address %s", pciAddress)
		}
		return []string{renamed}, nil
	}
	t.Cleanup(func() { hostNetdevsForPCI = previous })

	found, err = returnHostLink(hostIfName, &HostLinkState{PCIAddress: "0000:03:00.4", MTU: 1450})
	if err != nil {
		t.Fatalf("returnHostLink() error: %v", err)
	}
	if !found {
		t.Fatal("returnHostLink() did not find the renamed interface through its PCI function")
	}
	link, err = nlwrap.LinkByName(hostIfName)
	if err != nil {
		t.Fatalf("interface %s was not renamed back: %v", hostIfName, err)
	}
	if link.Attrs().MTU != 1450 {
		t.Errorf("interface %s has MTU %d, want 1450", hostIfName, link.Attrs().MTU)
	}
	if link.Attrs().Flags&net.FlagUp == 0 {
		t.Errorf("interface %s was left down", hostIfName)
	}
}
