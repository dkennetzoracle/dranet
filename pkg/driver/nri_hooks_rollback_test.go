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

	"sigs.k8s.io/dranet/internal/nlwrap"
	userns "sigs.k8s.io/dranet/internal/testutils"
	"sigs.k8s.io/dranet/pkg/apis"
)

// TestRollbackAttachedDevices checks that everything a request moved into the
// Pod namespace comes back to the host, under the original names and up, so the
// kubelet's retry of the sandbox starts from the same state as the first attempt.
func TestRollbackAttachedDevices(t *testing.T) {
	userns.Run(t, testRollbackAttachedDevices_Namespaced, syscall.CLONE_NEWNET, syscall.CLONE_NEWNS)
}

func testRollbackAttachedDevices_Namespaced(t *testing.T) {
	containerNsPath, containerNs, _ := testNetns(t)

	var attached []attachedDevice
	for i, names := range [][2]string{{"rbhost4", "rbpod4"}, {"rbhost5", "rbpod5"}} {
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
