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
	"net"
	"os"
	"path/filepath"
	"strings"

	"sigs.k8s.io/dranet/pkg/apis"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
	"sigs.k8s.io/dranet/internal/nlwrap"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/klog/v2"
)

func nsAttachNetdev(hostIfName string, containerNsPAth string, interfaceConfig apis.InterfaceConfig) (*resourceapi.NetworkDeviceData, error) {
	hostDev, err := nlwrap.LinkByName(hostIfName)
	if err != nil {
		return nil, fmt.Errorf("failed to get link for interface %s: %w", hostIfName, err)
	}

	// The kernel creates no IPv6 settings below this MTU, so none of accept_ra,
	// dad_transmits or router_solicitation_delay can be set. Reject it before
	// the link is touched so the host device stays usable.
	if interfaceConfig.HasIPv6Sysctls() && interfaceConfig.MTU == nil && hostDev.Attrs().MTU < apis.MinIPv6MTU {
		return nil, fmt.Errorf("the IPv6 settings (acceptRA, dadTransmits, routerSolicitationDelay) require an MTU of at least %d, but %s has MTU %d and the claim sets no mtu", apis.MinIPv6MTU, hostIfName, hostDev.Attrs().MTU)
	}

	// Devices can be renamed only when down
	if err = netlink.LinkSetDown(hostDev); err != nil {
		return nil, fmt.Errorf("failed to set %q down: %w", hostIfName, err)
	}

	containerNs, err := netns.GetFromPath(containerNsPAth)
	if err != nil {
		return nil, fmt.Errorf("failed to get container network namespace %s: %w", containerNsPAth, err)
	}
	defer containerNs.Close()

	attrs := hostDev.Attrs()

	// copy from netlink.LinkModify(dev) using only the parts needed
	flags := unix.NLM_F_REQUEST | unix.NLM_F_ACK
	req := nl.NewNetlinkRequest(unix.RTM_NEWLINK, flags)
	// Get a netlink socket in current namespace
	s, err := nl.GetNetlinkSocketAt(netns.None(), netns.None(), unix.NETLINK_ROUTE)
	if err != nil {
		return nil, fmt.Errorf("could not get network namespace handle: %w", err)
	}
	defer s.Close()

	req.Sockets = map[int]*nl.SocketHandle{
		unix.NETLINK_ROUTE: {Socket: s},
	}

	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Index = int32(attrs.Index)
	req.AddData(msg)

	ifName := attrs.Name
	if interfaceConfig.Name != "" {
		ifName = interfaceConfig.Name
	}
	nameData := nl.NewRtAttr(unix.IFLA_IFNAME, nl.ZeroTerminated(ifName))
	req.AddData(nameData)

	// Configuration values
	if interfaceConfig.MTU != nil {
		ifMtu := uint32(*interfaceConfig.MTU)
		mtu := nl.NewRtAttr(unix.IFLA_MTU, nl.Uint32Attr(ifMtu))
		req.AddData(mtu)
	}

	if interfaceConfig.HardwareAddr != nil {
		if hardwareAddr, err := net.ParseMAC(*interfaceConfig.HardwareAddr); err == nil {
			hwaddr := nl.NewRtAttr(unix.IFLA_ADDRESS, []byte(hardwareAddr))
			req.AddData(hwaddr)
		}
	}

	if interfaceConfig.GSOMaxSize != nil {
		gsoMaxSize := uint32(*interfaceConfig.GSOMaxSize)
		gsoAttr := nl.NewRtAttr(unix.IFLA_GSO_MAX_SIZE, nl.Uint32Attr(gsoMaxSize))
		req.AddData(gsoAttr)
	}

	if interfaceConfig.GROMaxSize != nil {
		groMaxSize := uint32(*interfaceConfig.GROMaxSize)
		groAttr := nl.NewRtAttr(unix.IFLA_GRO_MAX_SIZE, nl.Uint32Attr(groMaxSize))
		req.AddData(groAttr)
	}

	if interfaceConfig.GSOIPv4MaxSize != nil {
		gsoMaxSize := uint32(*interfaceConfig.GSOIPv4MaxSize)
		gsoV4Attr := nl.NewRtAttr(unix.IFLA_GSO_IPV4_MAX_SIZE, nl.Uint32Attr(gsoMaxSize))
		req.AddData(gsoV4Attr)
	}

	if interfaceConfig.GROIPv4MaxSize != nil {
		groMaxSize := uint32(*interfaceConfig.GROIPv4MaxSize)
		groV4Attr := nl.NewRtAttr(unix.IFLA_GRO_IPV4_MAX_SIZE, nl.Uint32Attr(groMaxSize))
		req.AddData(groV4Attr)
	}

	val := nl.Uint32Attr(uint32(containerNs))
	attr := nl.NewRtAttr(unix.IFLA_NET_NS_FD, val)
	req.AddData(attr)

	_, err = req.Execute(unix.NETLINK_ROUTE, 0)
	if err != nil && !errors.Is(err, netlink.ErrDumpInterrupted) {
		return nil, fmt.Errorf("failed to move interface %s to container namespace %s: %w", hostIfName, containerNsPAth, err)
	}

	// to avoid golang problem with goroutines we create the socket in the
	// namespace and use it directly
	nhNs, err := nlwrap.NewHandleAt(containerNs)
	if err != nil {
		return nil, fmt.Errorf("failed to get netlink handle in container namespace %s: %w", containerNsPAth, err)
	}
	defer nhNs.Close()

	nsLink, err := nhNs.LinkByName(ifName)
	if err != nil {
		return nil, fmt.Errorf("link not found for interface %s on namespace %s: %w", ifName, containerNsPAth, err)
	}

	// Apply before the link comes up so it never answers ARP or accepts router
	// advertisements with the wrong policy. The caller restores the host-side
	// state after a failure here, so the rollback only has to bring it back.
	if err := applyInterfaceSysctlConfig(containerNs, ifName, interfaceConfig); err != nil {
		rollbackErr := nsDetachNetdevFromNS(containerNs, containerNsPAth, ifName, hostIfName, nil)
		return nil, fmt.Errorf("failed to apply sysctl configuration to interface %s in namespace %s: %w", ifName, containerNsPAth, errors.Join(err, rollbackErr))
	}

	networkData := &resourceapi.NetworkDeviceData{
		InterfaceName:   nsLink.Attrs().Name,
		HardwareAddress: string(nsLink.Attrs().HardwareAddr.String()),
	}

	for _, address := range interfaceConfig.Addresses {
		ip, ipnet, err := net.ParseCIDR(address)
		if err != nil {
			klog.Infof("failed to parse address %s : %v", address, err)
			continue // this should not happen since it has been already validated
		}
		err = nhNs.AddrAdd(nsLink, &netlink.Addr{IPNet: &net.IPNet{IP: ip, Mask: ipnet.Mask}})
		if err != nil {
			return nil, fmt.Errorf("failed to set up address %s on namespace %s: %w", address, containerNsPAth, err)
		}
		networkData.IPs = append(networkData.IPs, address)
	}

	err = nhNs.LinkSetUp(nsLink)
	if err != nil {
		return nil, fmt.Errorf("failed to set up interface %s on namespace %s: %w", nsLink.Attrs().Name, containerNsPAth, err)
	}

	return networkData, nil
}

// nsDetachNetdev moves the interface devName back from the Pod namespace to the
// host as outName and restores the host-side state recorded before the move.
// A missing namespace is reported with an error that matches fs.ErrNotExist,
// so callers can tell "already torn down" from other failures.
func nsDetachNetdev(containerNsPAth string, devName string, outName string, hostState *HostLinkState) error {
	containerNs, err := netns.GetFromPath(containerNsPAth)
	if err != nil {
		return fmt.Errorf("could not get network namespace from path %s for network device %s : %w", containerNsPAth, devName, err)
	}
	defer containerNs.Close()
	return nsDetachNetdevFromNS(containerNs, containerNsPAth, devName, outName, hostState)
}

// restoreHostLink puts back what a namespace move drops from an interface that
// is on the host again: the master it was enslaved to, since a VRF slave would
// otherwise stay outside its VRF, and its MTU, which the Pod configuration may
// have changed. A nil state restores nothing.
func restoreHostLink(hostDev netlink.Link, state *HostLinkState) error {
	if state == nil {
		return nil
	}
	var errs []error
	if state.MTU > 0 && hostDev.Attrs().MTU != state.MTU {
		if err := netlink.LinkSetMTU(hostDev, state.MTU); err != nil {
			errs = append(errs, fmt.Errorf("failed to restore MTU %d of %s: %w", state.MTU, hostDev.Attrs().Name, err))
		}
	}
	if state.Master != "" {
		master, err := nlwrap.LinkByName(state.Master)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to find master %s to enslave %s to again: %w", state.Master, hostDev.Attrs().Name, err))
		} else if err := netlink.LinkSetMaster(hostDev, master); err != nil {
			errs = append(errs, fmt.Errorf("failed to enslave %s to %s again: %w", hostDev.Attrs().Name, state.Master, err))
		}
	}
	return errors.Join(errs...)
}

// hostNetdevsForPCI lists the network interfaces the host has for a PCI
// function. A variable so tests can stand in for sysfs.
var hostNetdevsForPCI = func(pciAddress string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join("/sys/bus/pci/devices", pciAddress, "net"))
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names, nil
}

// returnHostLink restores an interface the kernel has handed back to the host
// on its own, because the Pod namespace was destroyed before DraNet could
// return it. The kernel leaves such an interface down, with its host settings
// gone, and renames it when its name is already taken. It is found by name
// first, then through its PCI function, renamed back if needed, restored and
// brought up. It reports whether the interface was found at all.
func returnHostLink(hostIfName string, state *HostLinkState) (bool, error) {
	hostDev, err := nlwrap.LinkByName(hostIfName)
	if err != nil {
		if state == nil || state.PCIAddress == "" {
			return false, nil
		}
		names, err := hostNetdevsForPCI(state.PCIAddress)
		if err != nil || len(names) == 0 {
			return false, nil
		}
		// A PCI function normally has one interface. With several, the one the
		// kernel renamed carries its fallback name.
		candidate := names[0]
		if len(names) > 1 {
			candidate = ""
			for _, name := range names {
				if strings.HasPrefix(name, "dev") {
					candidate = name
					break
				}
			}
			if candidate == "" {
				return false, fmt.Errorf("PCI function %s has interfaces %v and none of them can be identified as %s", state.PCIAddress, names, hostIfName)
			}
		}
		hostDev, err = nlwrap.LinkByName(candidate)
		if err != nil {
			return false, fmt.Errorf("failed to get link %s of PCI function %s: %w", candidate, state.PCIAddress, err)
		}
		// Devices can be renamed only when down.
		if err := netlink.LinkSetDown(hostDev); err != nil {
			return true, fmt.Errorf("failed to set %s down to rename it to %s: %w", candidate, hostIfName, err)
		}
		if err := netlink.LinkSetName(hostDev, hostIfName); err != nil {
			return true, fmt.Errorf("failed to rename %s back to %s: %w", candidate, hostIfName, err)
		}
		if hostDev, err = nlwrap.LinkByName(hostIfName); err != nil {
			return true, fmt.Errorf("failed to get link %s after renaming it: %w", hostIfName, err)
		}
	}

	restoreErr := restoreHostLink(hostDev, state)
	if err := netlink.LinkSetUp(hostDev); err != nil {
		return true, errors.Join(restoreErr, fmt.Errorf("failed to set %q up: %w", hostIfName, err))
	}
	return true, restoreErr
}

func nsDetachNetdevFromNS(containerNs netns.NsHandle, containerNsPath string, devName string, outName string, hostState *HostLinkState) error {
	// to avoid golang problem with goroutines we create the socket in the
	// namespace and use it directly
	nhNs, err := nlwrap.NewHandleAt(containerNs)
	if err != nil {
		return fmt.Errorf("could not get network namespace handle: %w", err)
	}
	defer nhNs.Close()

	nsLink, err := nhNs.LinkByName(devName)
	if err != nil {
		return fmt.Errorf("link not found for interface %s on namespace %s: %w", devName, containerNsPath, err)
	}

	// set the device down to avoid network conflicts
	// when it is restored to the original namespace
	err = nhNs.LinkSetDown(nsLink)
	if err != nil {
		return fmt.Errorf("failed to set %q down: %w", devName, err)
	}

	attrs := nsLink.Attrs()
	// restore the original name if it was renamed
	if nsLink.Attrs().Alias != "" {
		attrs.Name = nsLink.Attrs().Alias
	}

	rootNs, err := netns.Get()
	if err != nil {
		return fmt.Errorf("failed to get root network namespace: %w", err)
	}
	defer rootNs.Close()

	s, err := nl.GetNetlinkSocketAt(containerNs, rootNs, unix.NETLINK_ROUTE)
	if err != nil {
		return fmt.Errorf("could not get network namespace handle: %w", err)
	}
	defer s.Close()
	// copy from netlink.LinkModify(dev) using only the parts needed
	flags := unix.NLM_F_REQUEST | unix.NLM_F_ACK
	req := nl.NewNetlinkRequest(unix.RTM_NEWLINK, flags)
	req.Sockets = map[int]*nl.SocketHandle{
		unix.NETLINK_ROUTE: {Socket: s},
	}
	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Index = int32(attrs.Index)
	req.AddData(msg)

	ifName := attrs.Name
	if outName != "" {
		ifName = outName
	}
	nameData := nl.NewRtAttr(unix.IFLA_IFNAME, nl.ZeroTerminated(ifName))
	req.AddData(nameData)

	val := nl.Uint32Attr(uint32(rootNs))
	attr := nl.NewRtAttr(unix.IFLA_NET_NS_FD, val)
	req.AddData(attr)

	_, err = req.Execute(unix.NETLINK_ROUTE, 0)
	if err != nil {
		return fmt.Errorf("failed to move interface %s to root namespace: %w", devName, err)
	}

	// Put the host-side state back, then set up the interface in case host
	// network workloads depend on it. A restore failure is reported after the
	// interface is up: it is better off up and outside its master than down.
	hostDev, err := nlwrap.LinkByName(ifName)
	if err != nil {
		return fmt.Errorf("failed to get link for interface %s: %w", ifName, err)
	}
	restoreErr := restoreHostLink(hostDev, hostState)

	if err = netlink.LinkSetUp(hostDev); err != nil {
		return errors.Join(restoreErr, fmt.Errorf("failed to set %q up: %w", ifName, err))
	}
	return restoreErr
}
