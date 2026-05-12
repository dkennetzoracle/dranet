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
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path"
	"runtime"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"k8s.io/component-helpers/node/util/sysctl"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/dranet/internal/nlwrap"
	"sigs.k8s.io/dranet/pkg/apis"
)

func Test_nhNetdev(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges.")
	}

	origns, err := netns.Get()
	if err != nil {
		t.Fatalf("unexpected error trying to get namespace: %v", err)
	}
	defer origns.Close()

	rndString := make([]byte, 4)
	_, err = rand.Read(rndString)
	if err != nil {
		t.Errorf("fail to generate random name: %v", err)
	}
	nsName := fmt.Sprintf("ns%x", rndString)
	testNS, err := netns.NewNamed(nsName)
	if err != nil {
		t.Fatalf("Failed to create network namespace: %v", err)
	}
	defer netns.DeleteNamed(nsName)
	defer testNS.Close()

	// Switch back to the original namespace
	netns.Set(origns)

	// Create a dummy interface in the test namespace
	nhNs, err := nlwrap.NewHandleAt(testNS)
	if err != nil {
		t.Fatalf("fail to open netlink handle: %v", err)
	}
	defer nhNs.Close()

	loLink, err := nhNs.LinkByName("lo")
	if err != nil {
		t.Fatalf("Failed to get loopback interface: %v", err)
	}
	if err := nhNs.LinkSetUp(loLink); err != nil {
		t.Fatalf("Failed to set up loopback interface: %v", err)
	}

	ifaceName := "testdummy-0"
	// Create a veth pair
	la := netlink.NewLinkAttrs()
	la.Name = ifaceName
	link := &netlink.Dummy{
		LinkAttrs: la,
	}
	if err := netlink.LinkAdd(link); err != nil {
		t.Fatalf("Failed to add dummy link %s in ns %s: %v", ifaceName, nsName, err)
	}

	t.Cleanup(func() {
		link, err := nlwrap.LinkByName(ifaceName)
		if err == nil {
			_ = netlink.LinkDel(link)
		}
	})
	if err := netlink.LinkSetUp(link); err != nil {
		t.Fatalf("Failed to add veth link %s in ns %s: %v", ifaceName, nsName, err)
	}
	config := apis.InterfaceConfig{
		Name:           "dranet0",
		Addresses:      []string{"192.168.7.7/32"},
		MTU:            ptr.To[int32](1234),
		HardwareAddr:   ptr.To("00:11:22:33:44:55"),
		GSOMaxSize:     ptr.To[int32](1024),
		GROMaxSize:     ptr.To[int32](1025),
		GSOIPv4MaxSize: ptr.To[int32](1026),
		GROIPv4MaxSize: ptr.To[int32](1027),
	}

	deviceData, err := nsAttachNetdev(ifaceName, path.Join("/run/netns", nsName), config)
	if err != nil {
		t.Fatalf("fail to attach netdev to namespace: %v", err)
	}

	// check against  ip lin
	func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		err := netns.Set(testNS)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("ip", "-d", "link", "show", config.Name)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("not able to use ethtool from namespace: %v", err)
		}
		outputStr := string(output)

		if !strings.Contains(outputStr, fmt.Sprintf("mtu %d", *config.MTU)) {
			t.Errorf("mtu not changed %s", outputStr)
		}
		if !strings.Contains(outputStr, fmt.Sprintf("gso_max_size %d", *config.GSOMaxSize)) {
			t.Errorf("GSOMaxSize not changed wanted %s got %s", fmt.Sprintf("gso_max_size %d", *config.GSOMaxSize), outputStr)
		}
		if !strings.Contains(outputStr, fmt.Sprintf("gro_max_size %d", *config.GROMaxSize)) {
			t.Errorf("GROMaxSize not changed %s", outputStr)
		}
		// require iproute 6.3.0+
		// TODO: validate the ip version to check it
		// https://github.com/iproute2/iproute2/commit/1dafe448c7a2f2be5dfddd8da250980708a48c41
		/*
			if !strings.Contains(outputStr, fmt.Sprintf("gso_ipv4_max_size %d", *config.GSOIPv4MaxSize)) {
				t.Errorf("GSOIPv4MaxSize not changed %s", outputStr)
			}
			if !strings.Contains(outputStr, fmt.Sprintf("gro_ipv4_max_size %d", *config.GROIPv4MaxSize)) {
				t.Errorf("GROIPv4MaxSize not changed %s", outputStr)
			}
		*/
		if !strings.Contains(outputStr, fmt.Sprintf("link/ether %s", *config.HardwareAddr)) {
			t.Errorf("HardwareAddr not changed %s", outputStr)
		}
		if *config.HardwareAddr != deviceData.HardwareAddress {
			t.Errorf("HardwareAddr not reported")
		}

		cmd = exec.Command("ip", "addr", "show", config.Name)
		output, err = cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("not able to use ethtool from namespace: %v", err)
		}
		outputStr = string(output)
		// TODO check reported state
		for _, addr := range config.Addresses {
			if !strings.Contains(outputStr, addr) {
				t.Errorf("address %s not found", addr)
			}
		}

		// Switch back to the original namespace
		err = netns.Set(origns)
		if err != nil {
			t.Fatal(err)
		}
	}()

	err = nsDetachNetdev(path.Join("/run/netns", nsName), config.Name, ifaceName)
	if err != nil {
		t.Fatalf("fail to attach netdev to namespace: %v", err)
	}

}

// Test_nsAttachNetdev_IPv6_EnablesOnEACCES exercises the IPv6 EACCES retry
// path in nsAttachNetdev. Container runtimes in single-stack IPv4 clusters
// set net.ipv6.conf.all.disable_ipv6=1 in the pod ns, which causes
// netlink.AddrAdd of an IPv6 GUA to return EACCES.
//
// Without the fix, nsAttachNetdev surfaces that EACCES and the pod sandbox
// fails to come up. With the fix, dranet enables IPv6 in the pod ns and
// retries once. This test reproduces that condition and asserts the IPv6
// address ends up configured on the interface inside the pod ns and that
// IPv6 is enabled there.
func Test_nsAttachNetdev_IPv6_EnablesOnEACCES(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges.")
	}
	const ifName = "v6attach"
	nsName, testNS, origns, cleanup := newTestNetns(t)
	defer cleanup()

	// Reproduce the single-stack-IPv4 pod ns: all.disable_ipv6=1.
	if err := setSysctlInNs(testNS, origns, "net/ipv6/conf/all/disable_ipv6", 1); err != nil {
		t.Fatalf("disable IPv6 in pod ns: %v", err)
	}

	createDummyLink(t, ifName)

	cfg := apis.InterfaceConfig{
		Name:      ifName,
		Addresses: []string{"fd00:dead:beef::5/64"},
	}
	if _, err := nsAttachNetdev(ifName, path.Join("/run/netns", nsName), cfg); err != nil {
		t.Fatalf("nsAttachNetdev: %v", err)
	}

	// Inside the pod ns: the address must be on the interface and IPv6
	// must be enabled (both globally and per-interface). The address would
	// not have been applied unless the retry path ran end-to-end.
	assertInNs(t, testNS, origns, func(t *testing.T) {
		assertAddrPresent(t, ifName, "fd00:dead:beef::5")
		if v, err := sysctl.New().GetSysctl("net/ipv6/conf/all/disable_ipv6"); err != nil || v != 0 {
			t.Errorf("after retry, all/disable_ipv6 = %d, err=%v; want 0", v, err)
		}
		if v, err := sysctl.New().GetSysctl("net/ipv6/conf/" + ifName + "/disable_ipv6"); err != nil || v != 0 {
			t.Errorf("after retry, %s/disable_ipv6 = %d, err=%v; want 0", ifName, v, err)
		}
	})
}

// Test_nsAttachNetdev_IPv4_NoRetryNeeded ensures the EACCES retry branch is
// gated on IPv6: an IPv4-only config must not exercise enableIPv6InNamespace
// and must succeed even when IPv6 is disabled in the pod ns.
func Test_nsAttachNetdev_IPv4_NoRetryNeeded(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges.")
	}
	const ifName = "v4attach"
	nsName, testNS, origns, cleanup := newTestNetns(t)
	defer cleanup()

	// Disable IPv6 in the pod ns regardless — the IPv4 path must not be
	// affected. We don't expect enableIPv6InNamespace to run here.
	if err := setSysctlInNs(testNS, origns, "net/ipv6/conf/all/disable_ipv6", 1); err != nil {
		t.Fatalf("disable IPv6 in pod ns: %v", err)
	}

	createDummyLink(t, ifName)

	cfg := apis.InterfaceConfig{
		Name:      ifName,
		Addresses: []string{"192.168.42.5/24"},
	}
	if _, err := nsAttachNetdev(ifName, path.Join("/run/netns", nsName), cfg); err != nil {
		t.Fatalf("nsAttachNetdev: %v", err)
	}

	assertInNs(t, testNS, origns, func(t *testing.T) {
		assertAddrPresent(t, ifName, "192.168.42.5/24")
		// IPv6 should remain disabled in the pod ns — the retry path
		// should not have been triggered for an IPv4-only allocation.
		if v, err := sysctl.New().GetSysctl("net/ipv6/conf/all/disable_ipv6"); err != nil || v != 1 {
			t.Errorf("after IPv4-only path, all/disable_ipv6 = %d, err=%v; want 1 (retry must not run)", v, err)
		}
	})
}

// newTestNetns creates a named netns for use as the "pod ns", returning the
// name (so callers can build /run/netns/<name>), the NsHandle for direct
// switching, the original (host) ns, and a cleanup function the caller must
// defer.
func newTestNetns(t *testing.T) (string, netns.NsHandle, netns.NsHandle, func()) {
	t.Helper()
	origns, err := netns.Get()
	if err != nil {
		t.Fatalf("netns.Get(): %v", err)
	}
	rnd := make([]byte, 4)
	if _, err := rand.Read(rnd); err != nil {
		_ = origns.Close()
		t.Fatalf("rand.Read: %v", err)
	}
	nsName := fmt.Sprintf("dranet-test-%x", rnd)
	testNS, err := netns.NewNamed(nsName)
	if err != nil {
		_ = origns.Close()
		t.Fatalf("netns.NewNamed: %v", err)
	}
	// NewNamed leaves us inside testNS; switch back to origns immediately.
	if err := netns.Set(origns); err != nil {
		_ = testNS.Close()
		_ = netns.DeleteNamed(nsName)
		_ = origns.Close()
		t.Fatalf("netns.Set(origns): %v", err)
	}
	return nsName, testNS, origns, func() {
		_ = netns.DeleteNamed(nsName)
		_ = testNS.Close()
		_ = origns.Close()
	}
}

// createDummyLink adds a dummy interface in the current netns and brings it
// up, registering a t.Cleanup that removes it if it is still in the current
// netns when the test ends. We refetch the link by name after creation so
// the returned Link carries a valid Index (LinkAdd does not populate the
// caller-supplied struct).
func createDummyLink(t *testing.T, name string) {
	t.Helper()
	if err := netlink.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: name}}); err != nil {
		t.Fatalf("LinkAdd(%s) in host ns: %v", name, err)
	}
	t.Cleanup(func() {
		if l, err := nlwrap.LinkByName(name); err == nil {
			_ = netlink.LinkDel(l)
		}
	})
	link, err := nlwrap.LinkByName(name)
	if err != nil {
		t.Fatalf("LinkByName(%s): %v", name, err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		t.Fatalf("LinkSetUp(%s): %v", name, err)
	}
}

// setSysctlInNs jumps into target, sets a sysctl, and returns to origin.
// The OS thread is locked for the duration so the ns switch is safe.
func setSysctlInNs(target, origin netns.NsHandle, key string, value int) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := netns.Set(target); err != nil {
		return fmt.Errorf("netns.Set(target): %w", err)
	}
	defer func() { _ = netns.Set(origin) }()
	if err := sysctl.New().SetSysctl(key, value); err != nil {
		return fmt.Errorf("SetSysctl(%s=%d): %w", key, value, err)
	}
	return nil
}

// assertInNs runs the assertion closure inside target and returns to origin.
// Wrapping the switch in its own function scope guarantees the OS-thread
// lock and netns switch unwind cleanly even if the closure t.Fatal's.
func assertInNs(t *testing.T, target, origin netns.NsHandle, fn func(t *testing.T)) {
	t.Helper()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := netns.Set(target); err != nil {
		t.Fatalf("netns.Set(target): %v", err)
	}
	defer func() {
		if err := netns.Set(origin); err != nil {
			t.Fatalf("netns.Set(origin): %v", err)
		}
	}()
	fn(t)
}

// assertAddrPresent verifies via netlink that the given CIDR or IP literal
// appears in the address list of ifName in the *current* netns.
func assertAddrPresent(t *testing.T, ifName, addr string) {
	t.Helper()
	link, err := nlwrap.LinkByName(ifName)
	if err != nil {
		t.Fatalf("LinkByName(%s) in pod ns: %v", ifName, err)
	}
	addrs, err := netlink.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		t.Fatalf("AddrList(%s): %v", ifName, err)
	}
	for _, a := range addrs {
		if a.IPNet != nil && (a.IPNet.String() == addr || strings.HasPrefix(a.IPNet.String(), strings.SplitN(addr, "/", 2)[0]+"/")) {
			return
		}
	}
	t.Errorf("address %q not present on %s in pod ns; have: %v", addr, ifName, addrs)
}
