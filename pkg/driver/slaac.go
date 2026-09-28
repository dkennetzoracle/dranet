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

	// DefaultSLAACRollbackReserve is the floor of the time withheld from the
	// request deadline for returning the Pod's devices to the host if a wait
	// does not succeed. Rollback is the same link-down, namespace move and
	// link-up as the attach, so the reserve actually used is the larger of this
	// and the time the request has already spent attaching devices; the floor
	// covers the netlink round trips when the attach was quick.
	DefaultSLAACRollbackReserve = 500 * time.Millisecond

	// slaacPollInterval is how often the Pod namespace is checked for a usable
	// address. Autoconfiguration normally completes in a few milliseconds once
	// the router replies, so this is short enough not to dominate the budget.
	slaacPollInterval = 25 * time.Millisecond
)

// readinessBudget returns how long a readiness check may wait, given the
// deadline of the request that triggered it, and whether that deadline has
// already passed. It reserves `reserve` for the rollback that a failed check has
// to perform, and never waits longer than `limit`.
//
// A context without a deadline falls back to `limit`: the container runtime
// enforces its own timeout on the plugin request whether or not it propagates a
// deadline to us, so an unbounded wait is never correct.
//
// Three cases follow from how much of the request is left:
//
//   - more than the reserve: wait for the smaller of the remainder and `limit`,
//     leaving the reserve for a rollback if nothing arrives;
//   - the reserve or less: there is time to roll back but not to wait as well,
//     so return a zero budget and let the caller roll back at once, which keeps
//     the request inside its deadline and lets the kubelet retry the sandbox;
//   - already past: the runtime has stopped waiting for this request, so it
//     will neither fail the sandbox nor retry, and the reserve has nothing left
//     to protect. Report it, and the caller finishes the wait on `limit` rather
//     than take a NIC away from a Pod that is starting regardless.
func readinessBudget(ctx context.Context, limit, reserve time.Duration) (budget time.Duration, deadlineExceeded bool) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return limit, false
	}
	until := time.Until(deadline)
	switch {
	case until <= 0:
		return limit, true
	case until <= reserve:
		return 0, false
	default:
		return min(until-reserve, limit), false
	}
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
			// running or has failed. Re-read the link so the operational state
			// is the current one, not the one from before the wait; a carrier
			// that never came up is the other common cause.
			if current, err := nhNs.LinkByName(ifName); err == nil {
				link = current
			}
			return nil, fmt.Errorf("interface %s has no autoconfigured IPv6 address after %v (link %s, addresses: %s): %w",
				ifName, time.Since(start).Round(time.Millisecond), link.Attrs().OperState, describeAddresses(addresses), ctx.Err())
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
		// Deprecated: preferred lifetime ran out; not usable as a source address.
		// Permanent: configured by hand, not autoconfigured.
		if address.Flags&(unix.IFA_F_TENTATIVE|unix.IFA_F_DADFAILED|unix.IFA_F_DEPRECATED|unix.IFA_F_PERMANENT) != 0 {
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
		if address.IPNet == nil {
			continue
		}
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
// inside the Pod namespace, within what is left of the request once `reserve`
// is set aside for returning the Pod's devices to the host.
//
// It does not roll anything back itself. By the time it runs, the request has
// moved every device of the Pod into the namespace, and a failure here has to
// return all of them, not just this one, so that the kubelet's retry of the
// sandbox starts from the same host state as the first attempt. That is the
// caller's job; see runPodSandbox.
func awaitSLAACReady(ctx context.Context, containerNsPath, ifNameInNs string, limit, reserve time.Duration) ([]string, error) {
	containerNs, err := netns.GetFromPath(containerNsPath)
	if err != nil {
		return nil, fmt.Errorf("failed to get container network namespace %s: %w", containerNsPath, err)
	}
	defer containerNs.Close()

	budget, deadlineExceeded := readinessBudget(ctx, limit, reserve)
	waitCtx := ctx
	if deadlineExceeded {
		// The deadline has already passed, so detach from it, keeping the logger
		// and the rest of the context values. Attaching devices one at a time
		// can outrun the runtime's timeout on its own; the wait itself takes
		// milliseconds, so finishing it gives the Pod a usable interface where
		// giving up would only hand it a missing one. (With time still on the
		// clock but not enough to wait, readinessBudget returns a zero budget
		// instead: one check, then the caller rolls back inside the deadline.)
		klog.FromContext(ctx).Info("Request deadline passed before IPv6 autoconfiguration; waiting anyway rather than taking the interface back",
			"interface", ifNameInNs, "budget", budget)
		waitCtx = context.WithoutCancel(ctx)
	}

	return waitForSLAAC(waitCtx, containerNs, ifNameInNs, budget)
}
