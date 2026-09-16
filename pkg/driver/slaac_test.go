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
	"fmt"
	"net"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/dranet/internal/nlwrap"
	"sigs.k8s.io/dranet/pkg/apis"
)

func TestReadinessBudget(t *testing.T) {
	const (
		max     = 1500 * time.Millisecond
		reserve = 500 * time.Millisecond
	)

	tests := []struct {
		name      string
		deadline  time.Duration // 0 means no deadline
		want      time.Duration
		expectErr bool
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
			name:      "a deadline inside the reserve leaves no time to wait",
			deadline:  300 * time.Millisecond,
			expectErr: true,
		},
		{
			name:      "an expired deadline leaves no time to wait",
			deadline:  -time.Second,
			expectErr: true,
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

			got, err := readinessBudget(ctx, max, reserve)
			if tt.expectErr {
				if err == nil {
					t.Fatalf("readinessBudget() = %v, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("readinessBudget() error: %v", err)
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
	testNS, err := netns.NewNamed(nsName)
	if err != nil {
		t.Fatalf("failed to create a network namespace: %v", err)
	}
	t.Cleanup(func() {
		testNS.Close()
		netns.DeleteNamed(nsName)
	})
	// NewNamed leaves the calling thread in the new namespace.
	if err := netns.Set(origns); err != nil {
		t.Fatalf("failed to restore the original namespace: %v", err)
	}

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
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges.")
	}
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
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges.")
	}
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
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges.")
	}
	_, testNS, _ := testNetns(t)

	if _, err := waitForSLAAC(context.Background(), testNS, "nosuchif", time.Second); err == nil {
		t.Fatal("waitForSLAAC() succeeded for an interface that does not exist, want an error")
	}
}

// TestAwaitSLAACReadyRollsBack drives the full path: an interface is moved into
// a Pod namespace, autoconfiguration never completes because nothing answers on
// the link, and the interface has to come back to the host.
func TestAwaitSLAACReadyRollsBack(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges.")
	}
	containerNsPath, _, _ := testNetns(t)

	hostIfName := "slaachost0"
	la := netlink.NewLinkAttrs()
	la.Name = hostIfName
	if err := netlink.LinkAdd(&netlink.Dummy{LinkAttrs: la}); err != nil {
		t.Fatalf("failed to add dummy link %s: %v", hostIfName, err)
	}
	t.Cleanup(func() {
		if link, err := nlwrap.LinkByName(hostIfName); err == nil {
			_ = netlink.LinkDel(link)
		}
	})

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

	if _, err := awaitSLAACReady(context.Background(), containerNsPath, ifNameInNs, hostIfName, 150*time.Millisecond); err == nil {
		t.Fatal("awaitSLAACReady() succeeded with no router on the link, want an error")
	}

	// The rollback must leave the interface on the host, under its original name
	// and up, so the next attempt at the sandbox starts from the same state.
	link, err := nlwrap.LinkByName(hostIfName)
	if err != nil {
		t.Fatalf("interface %s was not rolled back to the host: %v", hostIfName, err)
	}
	if link.Attrs().Flags&net.FlagUp == 0 {
		t.Errorf("interface %s was rolled back to the host but left down", hostIfName)
	}
}

// TestAwaitSLAACReadyNoBudget checks that a request that is already out of time
// skips the wait and still rolls the interface back.
func TestAwaitSLAACReadyNoBudget(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges.")
	}
	containerNsPath, _, _ := testNetns(t)

	hostIfName := "slaachost1"
	la := netlink.NewLinkAttrs()
	la.Name = hostIfName
	if err := netlink.LinkAdd(&netlink.Dummy{LinkAttrs: la}); err != nil {
		t.Fatalf("failed to add dummy link %s: %v", hostIfName, err)
	}
	t.Cleanup(func() {
		if link, err := nlwrap.LinkByName(hostIfName); err == nil {
			_ = netlink.LinkDel(link)
		}
	})

	ifNameInNs := "slaacpod1"
	config := apis.InterfaceConfig{Name: ifNameInNs, Addressing: apis.AddressingModeSLAAC}
	if _, err := nsAttachNetdev(hostIfName, containerNsPath, config); err != nil {
		t.Fatalf("nsAttachNetdev() error: %v", err)
	}

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(10*time.Millisecond))
	defer cancel()

	start := time.Now()
	_, err := awaitSLAACReady(ctx, containerNsPath, ifNameInNs, hostIfName, time.Second)
	if err == nil {
		t.Fatal("awaitSLAACReady() succeeded without any time to wait, want an error")
	}
	// Nothing should have been waited on: the reserve is for the rollback.
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("awaitSLAACReady() took %v, want it to skip the wait entirely", elapsed)
	}
	if _, err := nlwrap.LinkByName(hostIfName); err != nil {
		t.Fatalf("interface %s was not rolled back to the host: %v", hostIfName, err)
	}
}
