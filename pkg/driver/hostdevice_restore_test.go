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
	"syscall"
	"testing"

	"github.com/vishvananda/netlink"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/dranet/internal/nlwrap"
	userns "sigs.k8s.io/dranet/internal/testutils"
	"sigs.k8s.io/dranet/pkg/apis"
)

// TestRollbackRestoresHostLinkState checks that an interface returned to the
// host gets back what the move dropped: its master, since a VRF slave would
// otherwise come back outside its VRF, and its MTU.
func TestRollbackRestoresHostLinkState(t *testing.T) {
	userns.Run(t, testRollbackRestoresHostLinkState_Namespaced, syscall.CLONE_NEWNET, syscall.CLONE_NEWNS)
}

func testRollbackRestoresHostLinkState_Namespaced(t *testing.T) {
	containerNsPath, _, _ := testNetns(t)

	vrf := &netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: "vrfrestore9"}, Table: 90}
	if err := netlink.LinkAdd(vrf); err != nil {
		t.Fatalf("failed to add VRF: %v", err)
	}
	t.Cleanup(func() { _ = netlink.LinkDel(vrf) })
	if err := netlink.LinkSetUp(vrf); err != nil {
		t.Fatalf("failed to set up VRF: %v", err)
	}

	hostIfName := "restorehost6"
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
	ifNameInNs := "restorepod6"
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
	found, err := returnHostLink("restoregone0", &HostLinkState{PCIAddress: "0000:ff:00.0"})
	if found || err != nil {
		t.Fatalf("returnHostLink() for a missing interface = (%v, %v), want (false, nil)", found, err)
	}

	// The kernel's fallback name for a returned device is dev<ifindex>.
	hostIfName := "restorehost7"
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
