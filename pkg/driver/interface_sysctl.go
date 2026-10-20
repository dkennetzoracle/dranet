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
	"errors"
	"fmt"
	"os"
	"runtime"

	"github.com/vishvananda/netns"
	"k8s.io/component-helpers/node/util/sysctl"
	"k8s.io/klog/v2"

	"sigs.k8s.io/dranet/pkg/apis"
)

// sysctlProvider is overridden in tests to exercise sysctl failure paths.
var sysctlProvider = sysctl.New

// hasInterfaceSysctlConfig reports whether the interface config asks for any per-interface sysctl.
func hasInterfaceSysctlConfig(interfaceConfig apis.InterfaceConfig) bool {
	return interfaceConfig.ARPIgnore != nil || interfaceConfig.ARPAnnounce != nil || interfaceConfig.AcceptRA != nil ||
		interfaceConfig.DisableIPv6 != nil
}

func applyInterfaceSysctlsWithSysctl(sysctlInterface sysctl.Interface, ifName string, interfaceConfig apis.InterfaceConfig) error {
	var errorList []error
	set := func(family, setting string, value int32) {
		name := fmt.Sprintf("net/%s/conf/%s/%s", family, ifName, setting)
		if err := sysctlInterface.SetSysctl(name, int(value)); err != nil {
			errorList = append(errorList, fmt.Errorf("failed to set %s: %w", name, err))
		}
	}

	if interfaceConfig.ARPIgnore != nil {
		set("ipv4", "arp_ignore", *interfaceConfig.ARPIgnore)
	}
	if interfaceConfig.ARPAnnounce != nil {
		set("ipv4", "arp_announce", *interfaceConfig.ARPAnnounce)
	}
	if interfaceConfig.AcceptRA != nil {
		name := fmt.Sprintf("net/ipv6/conf/%s/accept_ra", ifName)
		err := sysctlInterface.SetSysctl(name, int(*interfaceConfig.AcceptRA))
		switch {
		case err == nil:
		case errors.Is(err, os.ErrNotExist) && *interfaceConfig.AcceptRA == 0:
			// The interface has no IPv6 sysctls, so it accepts no router
			// advertisements and zero is already satisfied.
			klog.V(4).Infof("%s not found; IPv6 is not enabled on %s and acceptRA: 0 is already satisfied", name, ifName)
		case errors.Is(err, os.ErrNotExist):
			errorList = append(errorList, fmt.Errorf("failed to set %s: IPv6 is not enabled on the interface: %w", name, err))
		default:
			errorList = append(errorList, fmt.Errorf("failed to set %s: %w", name, err))
		}
	}
	// Last, after accept_ra. The link is still down here in both callers, so the
	// order changes nothing today; it keeps an interface that is already up from
	// autoconfiguring with the namespace's accept_ra once IPv6 comes on. A CNI
	// that disables IPv6 in the Pod namespace disables it on lo too, and the Pod
	// reaches its own IPv6 addresses through lo, so enabling it on the interface
	// enables it there as well.
	if interfaceConfig.DisableIPv6 != nil {
		name := fmt.Sprintf("net/ipv6/conf/%s/disable_ipv6", ifName)
		value := 0
		if *interfaceConfig.DisableIPv6 {
			value = 1
		}
		err := sysctlInterface.SetSysctl(name, value)
		switch {
		case err == nil:
		case errors.Is(err, os.ErrNotExist) && value == 1:
			// No IPv6 settings (an MTU below 1280): IPv6 is already off.
			klog.V(4).Infof("%s not found; IPv6 is not enabled on %s and disableIPv6: true is already satisfied", name, ifName)
		case errors.Is(err, os.ErrNotExist):
			errorList = append(errorList, fmt.Errorf("failed to set %s: the interface has no IPv6 settings, so IPv6 cannot be enabled on it: %w", name, err))
		default:
			errorList = append(errorList, fmt.Errorf("failed to set %s: %w", name, err))
		}
		if value == 0 {
			if err := sysctlInterface.SetSysctl("net/ipv6/conf/lo/disable_ipv6", 0); err != nil {
				errorList = append(errorList, fmt.Errorf("failed to set net/ipv6/conf/lo/disable_ipv6: %w", err))
			}
		}
	}
	return errors.Join(errorList...)
}

// applyInterfaceSysctlConfig sets the requested per-interface sysctls inside the
// Pod network namespace. These live under /proc/sys, so unlike the rest of the
// interface configuration they cannot be set through a netlink handle and
// require joining the namespace.
func applyInterfaceSysctlConfig(containerNs netns.NsHandle, ifName string, interfaceConfig apis.InterfaceConfig) error {
	if !hasInterfaceSysctlConfig(interfaceConfig) {
		return nil
	}

	result := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		unlockThread := true
		defer func() {
			if unlockThread {
				runtime.UnlockOSThread()
			}
		}()

		originalNs, err := netns.Get()
		if err != nil {
			result <- fmt.Errorf("failed to get current network namespace: %w", err)
			return
		}
		defer originalNs.Close()

		if err := netns.Set(containerNs); err != nil {
			result <- fmt.Errorf("failed to join target network namespace: %w", err)
			return
		}

		applyErr := applyInterfaceSysctlsWithSysctl(sysctlProvider(), ifName, interfaceConfig)
		if err := netns.Set(originalNs); err != nil {
			// Keep this thread locked so the runtime destroys it instead of
			// reusing it in the wrong network namespace.
			unlockThread = false
			result <- errors.Join(applyErr, fmt.Errorf("failed to restore network namespace: %w", err))
			return
		}
		result <- applyErr
	}()
	return <-result
}
