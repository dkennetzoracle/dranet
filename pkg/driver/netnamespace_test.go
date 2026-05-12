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
	"path"
	"runtime"
	"testing"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"k8s.io/component-helpers/node/util/sysctl"
)

func Test_applyRoutingConfig(t *testing.T) {
	// TODO: see hostdevice_test.go and ethtool_test.go
}

// Test_enableIPv6InNamespace verifies that the helper flips
// net.ipv6.conf.all.disable_ipv6 and the per-interface counterpart from 1 to
// 0 inside the target namespace, restoring IPv6 functionality so a subsequent
// AddrAdd of an IPv6 address can succeed.
//
// Container runtimes in single-stack IPv4 clusters create the pod ns with
// all.disable_ipv6=1; this fixture reproduces that condition.
func Test_enableIPv6InNamespace(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges.")
	}

	origns, err := netns.Get()
	if err != nil {
		t.Fatalf("netns.Get(): %v", err)
	}
	defer origns.Close()

	rnd := make([]byte, 4)
	if _, err := rand.Read(rnd); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	nsName := fmt.Sprintf("dranet-test-v6-%x", rnd)
	testNS, err := netns.NewNamed(nsName)
	if err != nil {
		t.Fatalf("netns.NewNamed(%s): %v", nsName, err)
	}
	defer netns.DeleteNamed(nsName) // nolint:errcheck
	defer testNS.Close()
	// NewNamed leaves us inside testNS; switch back to origns immediately.
	if err := netns.Set(origns); err != nil {
		t.Fatalf("netns.Set(origns): %v", err)
	}

	// Establish initial sysctl state and an extra dummy interface
	// (so we can verify the per-interface knob too) — all from inside testNS.
	const ifName = "v6testif"
	withNs(t, testNS, origns, func() {
		// Bring lo up so the ns is usable.
		lo, err := netlink.LinkByName("lo")
		if err != nil {
			t.Fatalf("LinkByName(lo) in ns: %v", err)
		}
		if err := netlink.LinkSetUp(lo); err != nil {
			t.Fatalf("LinkSetUp(lo): %v", err)
		}
		dummy := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: ifName}}
		if err := netlink.LinkAdd(dummy); err != nil {
			t.Fatalf("LinkAdd(%s) in ns: %v", ifName, err)
		}
		s := sysctl.New()
		if err := s.SetSysctl("net/ipv6/conf/all/disable_ipv6", 1); err != nil {
			t.Fatalf("set all/disable_ipv6=1: %v", err)
		}
		if err := s.SetSysctl("net/ipv6/conf/"+ifName+"/disable_ipv6", 1); err != nil {
			t.Fatalf("set %s/disable_ipv6=1: %v", ifName, err)
		}
		// Sanity: both should now read 1.
		if v, err := s.GetSysctl("net/ipv6/conf/all/disable_ipv6"); err != nil || v != 1 {
			t.Fatalf("pre-condition all/disable_ipv6 = %d, err=%v; want 1", v, err)
		}
		if v, err := s.GetSysctl("net/ipv6/conf/" + ifName + "/disable_ipv6"); err != nil || v != 1 {
			t.Fatalf("pre-condition %s/disable_ipv6 = %d, err=%v; want 1", ifName, v, err)
		}
	})

	// Now exercise the helper from the host ns.
	if err := enableIPv6InNamespace(path.Join("/run/netns", nsName), ifName); err != nil {
		t.Fatalf("enableIPv6InNamespace: %v", err)
	}

	// Verify the helper landed in the right ns: both knobs should be 0 there
	// and our host-side defaults should be unchanged.
	withNs(t, testNS, origns, func() {
		s := sysctl.New()
		if v, err := s.GetSysctl("net/ipv6/conf/all/disable_ipv6"); err != nil || v != 0 {
			t.Errorf("after enableIPv6InNamespace, all/disable_ipv6 = %d, err=%v; want 0", v, err)
		}
		if v, err := s.GetSysctl("net/ipv6/conf/" + ifName + "/disable_ipv6"); err != nil || v != 0 {
			t.Errorf("after enableIPv6InNamespace, %s/disable_ipv6 = %d, err=%v; want 0", ifName, v, err)
		}
	})
}

// withNs locks the OS thread, enters target, runs fn, and returns to origin.
// Test helpers must not leak the thread into another netns on failure, hence
// the deferred netns.Set(origin) inside the same locked region.
func withNs(t *testing.T, target, origin netns.NsHandle, fn func()) {
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
	fn()
}
