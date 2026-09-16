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
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
	"k8s.io/klog/v2"

	"sigs.k8s.io/dranet/internal/nlwrap"
)

const (
	// DefaultSLAACReadyTimeout bounds how long the driver waits for an
	// autoconfigured address. It has to stay below the container runtime's
	// timeout for a plugin request (2s for both containerd and CRI-O), because
	// the runtime abandons the request once that expires and the interface would
	// be left in the Pod namespace with no address and no rollback.
	DefaultSLAACReadyTimeout = 1500 * time.Millisecond

	// slaacRollbackReserve is withheld from the request deadline so a wait that
	// does not succeed still has time to move the interface back to the host
	// namespace. Rollback is a handful of netlink round trips.
	slaacRollbackReserve = 500 * time.Millisecond

	// slaacPollInterval is how often the Pod namespace is checked for a usable
	// address. Autoconfiguration normally completes in a few milliseconds once
	// the router replies, so this is short enough not to dominate the budget.
	slaacPollInterval = 25 * time.Millisecond
)

// readinessBudget returns how long a readiness check may wait, given the
// deadline of the request that triggered it. It reserves `reserve` for the
// rollback that a failed check has to perform, and never waits longer than
// `max`.
//
// A context without a deadline falls back to `max`: the container runtime
// enforces its own timeout on the plugin request whether or not it propagates a
// deadline to us, so an unbounded wait is never correct.
func readinessBudget(ctx context.Context, max, reserve time.Duration) (time.Duration, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return max, nil
	}
	budget := time.Until(deadline) - reserve
	if budget <= 0 {
		return 0, fmt.Errorf("no time left to wait: %v until the request deadline, which is within the %v reserved for rollback", time.Until(deadline).Round(time.Millisecond), reserve)
	}
	return min(budget, max), nil
}

// waitForSLAAC blocks until ifName holds an autoconfigured global unicast IPv6
// address in the given network namespace, and returns those addresses in CIDR
// form. It reports an error if the budget expires first, in which case the
// caller is expected to roll the interface back out of the namespace.
//
// An address counts once the kernel has finished with it: still-tentative
// addresses are waiting on duplicate address detection and cannot be used as a
// source address yet, and permanent addresses did not come from a router
// advertisement.
func waitForSLAAC(ctx context.Context, containerNs netns.NsHandle, ifName string, budget time.Duration) ([]string, error) {
	logger := klog.LoggerWithValues(klog.FromContext(ctx), "interface", ifName, "budget", budget)

	// Netlink from outside the namespace, the same way the rest of the interface
	// configuration is applied, so no thread has to join it.
	nhNs, err := nlwrap.NewHandleAt(containerNs)
	if err != nil {
		return nil, fmt.Errorf("failed to get netlink handle in container namespace: %w", err)
	}
	defer nhNs.Close()

	link, err := nhNs.LinkByName(ifName)
	if err != nil {
		return nil, fmt.Errorf("link not found for interface %s: %w", ifName, err)
	}

	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	start := time.Now()
	ticker := time.NewTicker(slaacPollInterval)
	defer ticker.Stop()
	for {
		addresses, err := nhNs.AddrList(link, netlink.FAMILY_V6)
		if err != nil {
			return nil, fmt.Errorf("failed to list IPv6 addresses for interface %s: %w", ifName, err)
		}
		if ready := autoconfiguredAddresses(addresses); len(ready) > 0 {
			logger.V(2).Info("Interface completed IPv6 autoconfiguration", "addresses", ready, "elapsed", time.Since(start).Round(time.Millisecond))
			return ready, nil
		}

		select {
		case <-ticker.C:
		case <-ctx.Done():
			// The addresses the interface does have explain most failures: no
			// link-local address at all means IPv6 is disabled in the namespace,
			// and a tentative one means duplicate address detection is still
			// running or has failed.
			return nil, fmt.Errorf("interface %s has no autoconfigured IPv6 address after %v (link %s, addresses: %s)",
				ifName, time.Since(start).Round(time.Millisecond), link.Attrs().OperState, describeAddresses(addresses))
		}
	}
}

// autoconfiguredAddresses returns the CIDRs of the addresses that came from a
// router advertisement and are ready to use.
func autoconfiguredAddresses(addresses []netlink.Addr) []string {
	var ready []string
	for _, address := range addresses {
		if address.IPNet == nil || !address.IP.IsGlobalUnicast() {
			continue
		}
		// Tentative: duplicate address detection has not finished.
		// Permanent: configured by hand, not autoconfigured.
		if address.Flags&(unix.IFA_F_TENTATIVE|unix.IFA_F_DADFAILED|unix.IFA_F_PERMANENT) != 0 {
			continue
		}
		ready = append(ready, address.IPNet.String())
	}
	return ready
}

// describeAddresses renders an address list with the flags that matter for
// autoconfiguration, for diagnostics when a readiness check fails.
func describeAddresses(addresses []netlink.Addr) string {
	if len(addresses) == 0 {
		return "none"
	}
	described := make([]string, 0, len(addresses))
	for _, address := range addresses {
		var flags []string
		for _, flag := range []struct {
			name string
			bit  int
		}{
			{"tentative", unix.IFA_F_TENTATIVE},
			{"dadfailed", unix.IFA_F_DADFAILED},
			{"permanent", unix.IFA_F_PERMANENT},
			{"deprecated", unix.IFA_F_DEPRECATED},
		} {
			if address.Flags&flag.bit != 0 {
				flags = append(flags, flag.name)
			}
		}
		entry := address.IP.String()
		if len(flags) > 0 {
			entry += "(" + strings.Join(flags, ",") + ")"
		}
		described = append(described, entry)
	}
	return strings.Join(described, " ")
}

// awaitSLAACReady waits for the interface to finish IPv6 autoconfiguration
// inside the Pod namespace and, if it does not, moves it back to the host under
// its original name before returning the error.
//
// The interface is already in the Pod namespace by the time this runs, so a
// failure that left it there would strand a host NIC in a namespace that is
// about to be torn down. Rolling it back means the kubelet's retry of the
// sandbox starts from the same state as the first attempt.
func awaitSLAACReady(ctx context.Context, containerNsPath, ifNameInNs, hostIfName string, timeout time.Duration) ([]string, error) {
	containerNs, err := netns.GetFromPath(containerNsPath)
	if err != nil {
		return nil, fmt.Errorf("failed to get container network namespace %s: %w", containerNsPath, err)
	}
	defer containerNs.Close()

	budget, err := readinessBudget(ctx, timeout, slaacRollbackReserve)
	if err == nil {
		var addresses []string
		addresses, err = waitForSLAAC(ctx, containerNs, ifNameInNs, budget)
		if err == nil {
			return addresses, nil
		}
	}

	if rollbackErr := nsDetachNetdevFromNS(containerNs, containerNsPath, ifNameInNs, hostIfName); rollbackErr != nil {
		return nil, errors.Join(err, fmt.Errorf("failed to roll interface %s back to the host: %w", ifNameInNs, rollbackErr))
	}
	return nil, err
}
