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
	"path"
	"runtime"
	"testing"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"

	"sigs.k8s.io/dranet/internal/nlwrap"
)

// testNetns creates a named network namespace with a dummy interface up inside
// it, standing in for a Pod namespace, and returns the namespace path, a handle
// to it and the interface name. Everything is removed when the test ends.
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

	ifName := "testif0"
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
