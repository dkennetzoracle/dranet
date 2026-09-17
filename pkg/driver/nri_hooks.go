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
	"io/fs"
	"strings"
	"time"

	"github.com/containerd/nri/pkg/api"

	"sigs.k8s.io/dranet/pkg/apis"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	metav1apply "k8s.io/client-go/applyconfigurations/meta/v1"
	resourceapply "k8s.io/client-go/applyconfigurations/resource/v1"
	"k8s.io/klog/v2"
	"k8s.io/utils/set"
)

// NRI hooks into the container runtime, the lifecycle of the Pod seen here is local to the runtime
// and is not the same as the Pod lifecycle for kubernetes, per example, a Pod that can fail to start
// is retried locally multiple times, so the hooks need to be idempotent to all operations on the Pod.
// The NRI hooks are time sensitive, any slow operation needs to be added on the DRA hooks and only
// the information necessary should passed to the NRI hooks via the np.podConfigStore so it can be executed
// quickly.

func (np *NetworkDriver) Synchronize(ctx context.Context, pods []*api.PodSandbox, containers []*api.Container) ([]*api.ContainerUpdate, error) {
	logger := klog.FromContext(ctx)
	logger.Info("Synchronized state with the runtime", "pods", len(pods), "containers", len(containers))

	// livePodNetNs map tracks live pods by UID and their network namespace paths.
	livePodNetNs := make(map[types.UID]string)
	for _, pod := range pods {
		podLogger := klog.LoggerWithValues(logger, "pod", klog.KRef(pod.Namespace, pod.Name), "podUID", pod.Uid)
		podLogger.Info("Synchronize Pod")
		podLogger.V(2).Info("Pod network details", "netns", getNetworkNamespace(pod), "ips", pod.GetIps())
		livePodNetNs[types.UID(pod.Uid)] = getNetworkNamespace(pod)
	}

	// Process stored pods: update NetNS for live pods.
	for _, storedUID := range np.podConfigStore.ListPods() {
		if ns, isLive := livePodNetNs[storedUID]; isLive {
			np.podConfigStore.SetPodNetNs(storedUID, ns)
		}
	}

	return nil, nil
}

// CreateContainer handles container creation requests.
func (np *NetworkDriver) CreateContainer(ctx context.Context, pod *api.PodSandbox, ctr *api.Container) (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
	logger := klog.LoggerWithValues(klog.FromContext(ctx), "pod", klog.KRef(pod.Namespace, pod.Name), "podUID", pod.Uid, "container", ctr.Name)
	ctx = klog.NewContext(ctx, logger)
	logger.V(2).Info("CreateContainer")
	start := time.Now()
	status := statusNoop
	defer func() {
		nriPluginRequestsTotal.WithLabelValues(methodCreateContainer, status).Inc()
		nriPluginRequestsLatencySeconds.WithLabelValues(methodCreateContainer, status).Observe(time.Since(start).Seconds())
	}()
	podConfig, ok := np.podConfigStore.GetPodConfig(types.UID(pod.GetUid()))
	if !ok {
		return nil, nil, nil
	}

	defer func() {
		// Update container creation activity timestamp.
		logger.V(3).Info("Updating activity timestamp after CreateContainer")
		np.podConfigStore.UpdateLastNRIActivity(types.UID(pod.GetUid()), time.Now())
	}()

	adjust, update, err := np.createContainer(ctx, pod, ctr, podConfig)
	if err != nil {
		status = statusFailed
	} else {
		status = statusSuccess
	}
	return adjust, update, err
}

func (np *NetworkDriver) createContainer(_ context.Context, _ *api.PodSandbox, _ *api.Container, podConfig PodConfig) (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
	// Containers only care about the RDMA char devices.
	devPaths := set.Set[string]{}
	adjust := &api.ContainerAdjustment{}

	for _, config := range podConfig.DeviceConfigs {
		for _, dev := range config.RDMADevice.DevChars {
			// do not insert the same path multiple times
			if devPaths.Has(dev.Path) {
				continue
			}
			devPaths.Insert(dev.Path)
			// TODO check the file permissions and uid and gid fields
			adjust.AddDevice(&api.LinuxDevice{
				Path:  dev.Path,
				Type:  dev.Type,
				Major: dev.Major,
				Minor: dev.Minor,
			})
		}
	}

	return adjust, nil, nil
}

func (np *NetworkDriver) RunPodSandbox(ctx context.Context, pod *api.PodSandbox) error {
	logger := klog.LoggerWithValues(klog.FromContext(ctx), "pod", klog.KRef(pod.Namespace, pod.Name), "podUID", pod.Uid)
	ctx = klog.NewContext(ctx, logger)
	logger.V(2).Info("RunPodSandbox")
	start := time.Now()
	status := statusNoop
	defer func() {
		nriPluginRequestsTotal.WithLabelValues(methodRunPodSandbox, status).Inc()
		logger.V(2).Info("RunPodSandbox finished", "duration", time.Since(start))
		nriPluginRequestsLatencySeconds.WithLabelValues(methodRunPodSandbox, status).Observe(time.Since(start).Seconds())

	}()
	// get the devices associated to this Pod
	podConfig, ok := np.podConfigStore.GetPodConfig(types.UID(pod.GetUid()))
	if !ok {
		return nil
	}
	err := np.runPodSandbox(ctx, pod, podConfig)
	if err != nil {
		status = statusFailed
	} else {
		status = statusSuccess
	}
	return err
}
func (np *NetworkDriver) runPodSandbox(ctx context.Context, pod *api.PodSandbox, podConfig PodConfig) error {
	logger := klog.FromContext(ctx)
	// get the pod network namespace
	ns := getNetworkNamespace(pod)
	// host network pods can not allocate network devices because it impact the host
	if ns == "" {
		return fmt.Errorf("RunPodSandbox pod %s/%s using host network can not claim host devices", pod.Namespace, pod.Name)
	}
	// store the Pod network namespace in the pod config store
	np.podConfigStore.SetPodNetNs(types.UID(pod.GetUid()), ns)

	// Track all the status updates needed for the resource claims of the pod.
	statusUpdates := map[types.NamespacedName]*resourceapply.ResourceClaimStatusApplyConfiguration{}
	// The per-device statuses are added to their claim only at the end:
	// WithDevices copies the value, and the SLAAC waits below still add to them.
	type deviceStatus struct {
		claim  *resourceapply.ResourceClaimStatusApplyConfiguration
		device *resourceapply.AllocatedDeviceStatusApplyConfiguration
	}
	var deviceStatuses []deviceStatus

	// Everything this request puts into the namespace, so that a failure part
	// way through returns all of it instead of leaving the earlier devices
	// behind in a namespace the runtime is about to tear down.
	var attached []attachedDevice
	// The interfaces whose autoconfiguration has to complete before the
	// sandbox starts. They are checked once every device is in place: by then
	// most of them have their address already, and one that does not fails
	// the request through the single rollback below.
	var pending []slaacPending
	// What this request has spent moving devices in. Returning them costs
	// about the same, so it sizes the reserve the waits leave for a rollback.
	var attachTime time.Duration

	fail := func(cause error) error {
		if len(attached) == 0 {
			return cause
		}
		start := time.Now()
		rescan, rollbackErr := rollbackAttachedDevices(ctx, ns, attached, np.rdmaSharedMode)
		if rescan {
			np.netdb.RequestRescan()
		}
		logger.Info("Returned the devices attached by this request to the host", "devices", len(attached), "duration", time.Since(start).Round(time.Millisecond))
		if rollbackErr != nil {
			return errors.Join(cause, rollbackErr)
		}
		return cause
	}

	// Process the configurations of the ResourceClaim
	for deviceName, config := range podConfig.DeviceConfigs {
		logger.V(4).Info("RunPodSandbox processing device", "device", deviceName, "config", fmt.Sprintf("%#v", config))
		resourceClaim := types.NamespacedName{Name: config.Claim.Name, Namespace: config.Claim.Namespace}
		resourceClaimStatus := statusUpdates[resourceClaim]
		if statusUpdates[resourceClaim] == nil {
			resourceClaimStatus = resourceapply.ResourceClaimStatus()
			statusUpdates[resourceClaim] = resourceClaimStatus
		}
		// resourceClaim status for this specific device
		resourceClaimStatusDevice := resourceapply.
			AllocatedDeviceStatus().
			WithDevice(deviceName).
			WithDriver(np.driverName).
			WithPool(np.nodeName)

		ifName := config.NetworkInterfaceConfigInHost.Interface.Name

		// Block 1: netdev operations — only when a network interface is present.
		if ifName != "" {
			start := time.Now()
			if config.NetworkInterfaceConfigInPod.Interface.IsSubinterface() {
				ifNameInNs, err := createSubinterfaceInNS(ctx, ns, deviceName, config, resourceClaimStatusDevice)
				attachTime += time.Since(start)
				if ifNameInNs != "" {
					attached = append(attached, attachedDevice{deviceName: deviceName, ifNameInNs: ifNameInNs, subinterface: true})
				}
				if err != nil {
					np.eventRecorder.Eventf(podObjectRef(pod), v1.EventTypeWarning, "NetworkDeviceCreateFailed",
						"failed to create subinterface on network device %s to pod %s/%s: %v", deviceName, pod.GetNamespace(), pod.GetName(), err)
					return fail(err)
				}
			} else {
				ifNameInNs, err := attachNetdevToNS(ctx, ns, deviceName, config, resourceClaimStatusDevice)
				attachTime += time.Since(start)
				if ifNameInNs != "" {
					attached = append(attached, attachedDevice{deviceName: deviceName, hostIfName: ifName, ifNameInNs: ifNameInNs, hostState: config.NetworkInterfaceStateInHost})
				}
				if err != nil {
					np.eventRecorder.Eventf(podObjectRef(pod), v1.EventTypeWarning, "NetworkDeviceAttachFailed",
						"failed to attach network device %s to pod %s/%s: %v", deviceName, pod.GetNamespace(), pod.GetName(), err)
					return fail(err)
				}
				if config.NetworkInterfaceConfigInPod.Interface.Addressing == apis.AddressingModeSLAAC {
					pending = append(pending, slaacPending{deviceName: deviceName, ifNameInNs: ifNameInNs, claim: resourceClaim, status: resourceClaimStatusDevice})
				}
			}
		}

		// Block 2: RDMA link device — independent of whether a netdev exists.
		// For IB-only devices (no netdev) this is the only operation here;
		// for RoCE (netdev + RDMA) it runs after the netdev block above.
		if !np.rdmaSharedMode && config.RDMADevice.LinkDev != "" {
			start := time.Now()
			err := attachRdmaToNS(ctx, config.RDMADevice.LinkDev, ns, resourceClaimStatusDevice)
			attachTime += time.Since(start)
			if err != nil {
				np.eventRecorder.Eventf(podObjectRef(pod), v1.EventTypeWarning, "RDMADeviceAttachFailed",
					"failed to attach RDMA device %s to pod %s/%s: %v", config.RDMADevice.LinkDev, pod.GetNamespace(), pod.GetName(), err)
				return fail(err)
			}
			attached = append(attached, attachedDevice{deviceName: deviceName, rdmaLinkDev: config.RDMADevice.LinkDev})
		}

		// Block 3: Status conditions for IB-only devices (no netdev).
		// In exclusive RDMA mode the RDMA link was moved above; in shared mode
		// char-device injection (createContainer) is sufficient. Either way the
		// device is ready, so emit the condition unconditionally.
		if ifName == "" && config.RDMADevice.LinkDev != "" {
			resourceClaimStatusDevice.WithConditions(
				metav1apply.Condition().
					WithType("Ready").
					WithReason("RDMAOnlyDeviceReady").
					WithStatus(metav1.ConditionTrue).
					WithLastTransitionTime(metav1.Now()),
			)
		}

		deviceStatuses = append(deviceStatuses, deviceStatus{claim: resourceClaimStatus, device: resourceClaimStatusDevice})
	}

	// Every device is in the namespace and configured. Now the interfaces that
	// autoconfigure have to show an address before the workload starts.
	if len(pending) > 0 {
		reserve := max(np.slaacRollbackReserve, attachTime)
		for _, p := range pending {
			addresses, err := awaitSLAACReady(ctx, ns, p.ifNameInNs, np.slaacReadyTimeout, reserve)
			if err != nil {
				logger.Error(err, "RunPodSandbox interface did not complete IPv6 autoconfiguration", "device", p.deviceName, "interface", p.ifNameInNs)
				np.eventRecorder.Eventf(podObjectRef(pod), v1.EventTypeWarning, "NetworkDeviceNotReady",
					"interface %s of network device %s in pod %s/%s did not complete IPv6 autoconfiguration: %v", p.ifNameInNs, p.deviceName, pod.GetNamespace(), pod.GetName(), err)
				// Say so on the claim as well: the Pod's events are the first
				// place to look, the claim is the second, and the rollback below
				// leaves nothing else behind to explain a Pod that never starts.
				np.applyClaimStatus(logger, p.claim, resourceapply.ResourceClaimStatus().WithDevices(
					resourceapply.AllocatedDeviceStatus().
						WithDevice(p.deviceName).
						WithDriver(np.driverName).
						WithPool(np.nodeName).
						WithConditions(metav1apply.Condition().
							WithType("SLAACReady").
							WithStatus(metav1.ConditionFalse).
							WithReason("AutoconfigurationTimedOut").
							WithMessage(err.Error()).
							WithLastTransitionTime(metav1.Now())),
				))
				start := time.Now()
				failErr := fail(fmt.Errorf("error waiting for IPv6 autoconfiguration of device %s in namespace %s: %w", p.deviceName, ns, err))
				if rollback := time.Since(start); rollback > reserve {
					logger.Info("Rollback took longer than the reserve withheld for it; raise --slaac-rollback-reserve if the runtime abandoned this request",
						"rollback", rollback.Round(time.Millisecond), "reserve", reserve.Round(time.Millisecond))
				}
				return failErr
			}
			p.status.WithConditions(
				metav1apply.Condition().
					WithType("SLAACReady").
					WithStatus(metav1.ConditionTrue).
					WithReason("SLAACReady").
					WithMessage(fmt.Sprintf("autoconfigured addresses: %s", strings.Join(addresses, ","))).
					WithLastTransitionTime(metav1.Now()),
			)
			if p.status.NetworkData != nil {
				p.status.NetworkData.WithIPs(addresses...)
			}
		}
	}

	for _, s := range deviceStatuses {
		s.claim.WithDevices(s.device)
	}
	// do not block the handler to update the status
	for claim, status := range statusUpdates {
		np.applyClaimStatus(logger, claim, status)
	}

	return nil
}

// attachRdmaToNS moves the RDMA link device into the pod network namespace and
// records the RDMALinkReady status condition on resourceClaimStatusDevice.
func attachRdmaToNS(ctx context.Context, linkDev, ns string, resourceClaimStatusDevice *resourceapply.AllocatedDeviceStatusApplyConfiguration) error {
	logger := klog.LoggerWithValues(klog.FromContext(ctx), "rdmaDevice", linkDev, "netns", ns)
	logger.V(2).Info("RunPodSandbox processing RDMA device")
	if err := nsAttachRdmadev(linkDev, ns); err != nil {
		logger.Error(err, "RunPodSandbox error moving RDMA device to namespace")
		return fmt.Errorf("error moving RDMA device %s to namespace %s: %v", linkDev, ns, err)
	}
	resourceClaimStatusDevice.WithConditions(
		metav1apply.Condition().
			WithType("RDMALinkReady").
			WithStatus(metav1.ConditionTrue).
			WithReason("RDMALinkReady").
			WithLastTransitionTime(metav1.Now()),
	)
	return nil
}

// attachedDevice is a device the current RunPodSandbox request has moved into,
// or created in, the Pod network namespace. A failure later in the same request
// returns all of them so the kubelet's retry starts from a clean host; the
// runtime does not call StopPodSandbox for a sandbox that failed to start, and
// the kernel would otherwise hand the devices back on its own terms when it
// destroys the namespace: down, renamed if the name is taken, and without the
// settings they had on the host.
type attachedDevice struct {
	deviceName string
	// hostIfName is the name a moved netdev has to get back; empty when the
	// device brought no netdev into the namespace.
	hostIfName string
	// ifNameInNs is the netdev's name inside the namespace, or the name of the
	// subinterface created there.
	ifNameInNs   string
	subinterface bool
	// rdmaLinkDev is the RDMA device moved into the namespace, in exclusive
	// RDMA netns mode only.
	rdmaLinkDev string
	// hostState is what the moved netdev looked like on the host, to restore
	// on the way back.
	hostState *HostLinkState
}

// slaacPending is an interface whose autoconfiguration the request still has
// to see complete, together with the status it reports the addresses on.
type slaacPending struct {
	deviceName string
	ifNameInNs string
	claim      types.NamespacedName
	status     *resourceapply.AllocatedDeviceStatusApplyConfiguration
}

// applyClaimStatus updates a claim's status in the background, so the runtime
// hook that produced it does not wait on the API server.
func (np *NetworkDriver) applyClaimStatus(logger klog.Logger, claim types.NamespacedName, status *resourceapply.ResourceClaimStatusApplyConfiguration) {
	resourceClaimApply := resourceapply.ResourceClaim(claim.Name, claim.Namespace).WithStatus(status)
	claimLogger := klog.LoggerWithValues(logger, "claim", klog.KRef(claim.Namespace, claim.Name))
	go func() {
		ctxStatus, cancel := context.WithTimeout(klog.NewContext(context.Background(), claimLogger), 3*time.Second)
		defer cancel()
		_, err := np.kubeClient.ResourceV1().ResourceClaims(claim.Namespace).ApplyStatus(ctxStatus,
			resourceClaimApply,
			metav1.ApplyOptions{FieldManager: np.driverName, Force: true},
		)
		if err != nil {
			claimLogger.Error(err, "Failed to update status for claim")
		} else {
			claimLogger.V(4).Info("Updated status for claim")
		}
	}()
}

// rollbackAttachedDevices returns to the host everything a RunPodSandbox
// request has put into the Pod namespace, most recent first, and reports
// whether the inventory needs a rescan: returning an RDMA device produces no
// netlink event the inventory would notice on its own.
func rollbackAttachedDevices(ctx context.Context, ns string, attached []attachedDevice, rdmaSharedMode bool) (bool, error) {
	logger := klog.FromContext(ctx)
	var errs []error
	needsRescan := false
	for i := len(attached) - 1; i >= 0; i-- {
		dev := attached[i]
		// RDMA before the netdev, for the reason given in stopPodSandbox.
		if !rdmaSharedMode && dev.rdmaLinkDev != "" {
			if err := nsDetachRdmadev(ns, dev.rdmaLinkDev); err != nil {
				errs = append(errs, fmt.Errorf("failed to return RDMA device %s of %s to the host: %w", dev.rdmaLinkDev, dev.deviceName, err))
			} else {
				needsRescan = true
			}
		}
		switch {
		case dev.subinterface && dev.ifNameInNs != "":
			if err := nsDeleteSubinterface(ns, dev.ifNameInNs); err != nil {
				errs = append(errs, fmt.Errorf("failed to delete subinterface %s of %s: %w", dev.ifNameInNs, dev.deviceName, err))
			}
		case dev.hostIfName != "":
			if err := nsDetachNetdev(ns, dev.ifNameInNs, dev.hostIfName, dev.hostState); err != nil {
				errs = append(errs, fmt.Errorf("failed to return interface %s of %s to the host as %s: %w", dev.ifNameInNs, dev.deviceName, dev.hostIfName, err))
			}
		}
		logger.V(2).Info("Returned device to the host after a failed RunPodSandbox", "device", dev.deviceName)
	}
	return needsRescan, errors.Join(errs...)
}

// attachNetdevToNS moves the host network interface into the pod network namespace,
// applies all associated configuration (ethtool, eBPF, routes, rules, neighbors),
// and records the resulting status conditions on resourceClaimStatusDevice.
//
// It returns the interface's name inside the namespace as soon as the move has
// happened, with or without an error, so the caller knows what to return to the
// host if the request fails after this point. An empty name means the interface
// is still on the host.
func attachNetdevToNS(ctx context.Context, ns, deviceName string, config DeviceConfig, resourceClaimStatusDevice *resourceapply.AllocatedDeviceStatusApplyConfiguration) (string, error) {
	ifName := config.NetworkInterfaceConfigInHost.Interface.Name
	logger := klog.LoggerWithValues(klog.FromContext(ctx), "device", deviceName, "interface", ifName, "netns", ns)
	logger.V(2).Info("RunPodSandbox processing Network device")
	// TODO config options to rename the device and pass parameters
	// use https://github.com/opencontainers/runtime-spec/pull/1271
	networkData, err := nsAttachNetdev(ifName, ns, config.NetworkInterfaceConfigInPod.Interface)
	if err != nil {
		logger.Error(err, "RunPodSandbox error moving network device to namespace")
		// The interface is on the host either way, down: the move sets it
		// down first and a failure before or after it leaves it so. Put it
		// back the way it was so the kubelet's retry starts from a clean host.
		if _, restoreErr := returnHostLink(ifName, config.NetworkInterfaceStateInHost); restoreErr != nil {
			logger.Error(restoreErr, "RunPodSandbox could not fully restore the network device on the host after the failed move")
		}
		return "", fmt.Errorf("error moving network device %s to namespace %s: %v", deviceName, ns, err)
	}

	// With SLAAC the interface is up but not yet usable: its address arrives
	// from a router advertisement some milliseconds later. runPodSandbox waits
	// for it once every device of the Pod is in place, and adds the addresses
	// and the SLAACReady condition to this status then.
	resourceClaimStatusDevice.WithConditions(
		metav1apply.Condition().
			WithType("Ready").
			WithReason("NetworkDeviceReady").
			WithStatus(metav1.ConditionTrue).
			WithLastTransitionTime(metav1.Now()),
	).WithNetworkData(resourceapply.NetworkDeviceData().
		WithInterfaceName(networkData.InterfaceName).
		WithHardwareAddress(networkData.HardwareAddress).
		WithIPs(networkData.IPs...),
	) // End of WithNetworkData

	// Configure the moved device (ethtool, vrf, routes, neighbors, rules)
	return networkData.InterfaceName, configureNetdevInNS(ctx, ns, deviceName, config, networkData.InterfaceName, resourceClaimStatusDevice)
}

// createSubinterfaceInNS creates a subinterface in the pod network namespace,
// applies all associated configurations, and records the status conditions.
//
// It returns the subinterface's name when the subinterface exists on return,
// so the caller can delete it if the request fails later; a configuration
// failure deletes it here already and returns an empty name.
func createSubinterfaceInNS(ctx context.Context, ns, deviceName string, config DeviceConfig, resourceClaimStatusDevice *resourceapply.AllocatedDeviceStatusApplyConfiguration) (string, error) {
	logger := klog.FromContext(ctx)
	hostIfName := config.NetworkInterfaceConfigInHost.Interface.Name
	logger.V(2).Info("RunPodSandbox creating subinterface on parent device", "parentDevice", hostIfName)

	networkData, err := nsCreateSubinterface(hostIfName, ns, config.NetworkInterfaceConfigInPod.Interface)
	if err != nil {
		logger.Error(err, "RunPodSandbox error creating subinterface", "parentDevice", hostIfName, "netns", ns)
		return "", fmt.Errorf("error creating subinterface on parent %s in namespace %s: %v", hostIfName, ns, err)
	}

	// Configure the subinterface (ethtool, vrf, routes, neighbors, rules)
	if err := configureNetdevInNS(ctx, ns, deviceName, config, networkData.InterfaceName, resourceClaimStatusDevice); err != nil {
		// Delete the child now rather than leaving it half configured until pod teardown.
		if delErr := nsDeleteSubinterface(ns, networkData.InterfaceName); delErr != nil {
			return networkData.InterfaceName, errors.Join(err, fmt.Errorf("failed to delete subinterface %s after a configuration failure: %w", networkData.InterfaceName, delErr))
		}
		return "", err
	}

	// Report the device only after the configuration succeeds, so a failure
	// leaves the status without a Ready condition or network data.
	resourceClaimStatusDevice.WithConditions(
		metav1apply.Condition().
			WithType("Ready").
			WithReason("NetworkDeviceReady").
			WithStatus(metav1.ConditionTrue).
			WithLastTransitionTime(metav1.Now()),
	).WithNetworkData(resourceapply.NetworkDeviceData().
		WithInterfaceName(networkData.InterfaceName).
		WithHardwareAddress(networkData.HardwareAddress).
		WithIPs(networkData.IPs...),
	)
	return networkData.InterfaceName, nil
}

// configureNetdevInNS applies common L3 configurations (ethtool, eBPF, VRF, routes, rules, and neighbors)
// to a network interface inside the container's network namespace and marks the claim status as NetworkReady.
func configureNetdevInNS(ctx context.Context, ns, deviceName string, config DeviceConfig, ifNameInNs string, resourceClaimStatusDevice *resourceapply.AllocatedDeviceStatusApplyConfiguration) error {
	logger := klog.FromContext(ctx)
	var err error

	// Apply Ethtool configurations
	if config.NetworkInterfaceConfigInPod.Ethtool != nil {
		err = applyEthtoolConfig(ns, ifNameInNs, config.NetworkInterfaceConfigInPod.Ethtool)
		if err != nil {
			logger.Error(err, "RunPodSandbox error applying ethtool config", "podInterface", ifNameInNs)
			return fmt.Errorf("error applying ethtool config for %s in ns %s: %v", ifNameInNs, ns, err)
		}
	}

	// Check if the ebpf programs should be disabled
	if config.NetworkInterfaceConfigInPod.Interface.DisableEBPFPrograms != nil &&
		*config.NetworkInterfaceConfigInPod.Interface.DisableEBPFPrograms {
		err = detachEBPFPrograms(ns, ifNameInNs)
		if err != nil {
			logger.Error(err, "Error disabling ebpf programs", "podInterface", ifNameInNs)
			return fmt.Errorf("error disabling ebpf programs for %s in ns %s: %v", ifNameInNs, ns, err)
		}
	}

	vrfTable := 0
	if config.NetworkInterfaceConfigInPod.Interface.VRF != nil {
		vrfTable, err = applyVRFConfig(ns, ifNameInNs, config.NetworkInterfaceConfigInPod.Interface.VRF)
		if err != nil {
			return fmt.Errorf("error configuring VRF for device %s in ns %s: %w", deviceName, ns, err)
		}
	}

	// Configure routes
	err = applyRoutingConfig(ns, ifNameInNs, config.NetworkInterfaceConfigInPod.Routes, vrfTable)
	if err != nil {
		logger.Error(err, "RunPodSandbox error configuring routing", "podInterface", ifNameInNs)
		return fmt.Errorf("error configuring device %s routes on namespace %s: %v", deviceName, ns, err)
	}

	// Configure rules
	// If VRF is enabled, rules are not needed/supported as routing is handled by the VRF table + l3mdev.
	if vrfTable == 0 {
		err = applyRulesConfig(ns, config.NetworkInterfaceConfigInPod.Rules)
		if err != nil {
			logger.Error(err, "RunPodSandbox error configuring rules")
			return fmt.Errorf("error configuring device %s rules on namespace %s: %v", deviceName, ns, err)
		}
	}

	// Configure neighbors
	err = applyNeighborConfig(ns, ifNameInNs, config.NetworkInterfaceConfigInPod.Neighbors)
	if err != nil {
		logger.Error(err, "RunPodSandbox failed to apply neighbor configuration", "podInterface", ifNameInNs)
		return fmt.Errorf("failed to apply neighbor configuration for interface %s in namespace %s: %w", ifNameInNs, ns, err)
	}

	resourceClaimStatusDevice.WithConditions(
		metav1apply.Condition().
			WithType("NetworkReady").
			WithStatus(metav1.ConditionTrue).
			WithReason("NetworkReady").
			WithLastTransitionTime(metav1.Now()),
	)
	return nil
}

// StopPodSandbox tries to move back the devices to the rootnamespace but does not fail
// to avoid disrupting the pod shutdown. The kernel will do the cleanup once the namespace
// is deleted.
func (np *NetworkDriver) StopPodSandbox(ctx context.Context, pod *api.PodSandbox) error {
	logger := klog.LoggerWithValues(klog.FromContext(ctx), "pod", klog.KRef(pod.Namespace, pod.Name), "podUID", pod.Uid)
	ctx = klog.NewContext(ctx, logger)
	logger.V(2).Info("StopPodSandbox")
	start := time.Now()
	status := statusNoop
	defer func() {
		nriPluginRequestsTotal.WithLabelValues(methodStopPodSandbox, status).Inc()
		logger.V(2).Info("StopPodSandbox finished", "duration", time.Since(start))
		nriPluginRequestsLatencySeconds.WithLabelValues(methodStopPodSandbox, status).Observe(time.Since(start).Seconds())
	}()
	// get the devices associated to this Pod
	podConfig, ok := np.podConfigStore.GetPodConfig(types.UID(pod.GetUid()))
	if !ok {
		return nil
	}
	err := np.stopPodSandbox(ctx, pod, podConfig)
	if err != nil {
		status = statusFailed
	} else {
		status = statusSuccess
	}
	return err
}

func (np *NetworkDriver) stopPodSandbox(ctx context.Context, pod *api.PodSandbox, podConfig PodConfig) error {
	logger := klog.FromContext(ctx)
	// get the pod network namespace
	ns := getNetworkNamespace(pod)
	if ns == "" {
		// some version of containerd does not send the network namespace information on this hook so
		// we workaround it using the local copy we have in the db to associate interfaces with Pods via
		// the network namespace id.
		if podConfig.NetNS == "" {
			logger.Info("StopPodSandbox: network namespace for DRANET pod is unknown; relying on kernel netns teardown to return the devices and restoring them on the host")
		}
		ns = podConfig.NetNS
	}
	needsRescan := false
	for deviceName, config := range podConfig.DeviceConfigs {
		// Move the RDMA device back to the host namespace BEFORE the netdev.
		// nsDetachNetdev calls LinkSetUp on the VF in the host namespace, which
		// triggers a NEWLINK event causing the inventory to rescan. If the RDMA
		// device is still in the pod namespace at that point it will not be
		// detected, so it must be returned first.
		rdmaDetached := false
		if !np.rdmaSharedMode && config.RDMADevice.LinkDev != "" {
			if err := nsDetachRdmadev(ns, config.RDMADevice.LinkDev); err != nil {
				logger.Error(err, "Failed to return rdma device", "device", deviceName)
			} else {
				rdmaDetached = true
			}
		}

		netdevDetached := false
		ifName := config.NetworkInterfaceConfigInPod.Interface.Name
		if ifName != "" {
			if config.NetworkInterfaceConfigInPod.Interface.IsSubinterface() {
				subIfName := config.NetworkInterfaceConfigInPod.Interface.Name
				if ns == "" {
					// Nothing to do: the child died with its namespace.
				} else if err := nsDeleteSubinterface(ns, subIfName); err != nil {
					logger.Error(err, "Failed to delete subinterface", "subInterface", subIfName, "device", deviceName)
				}
			} else {
				hostIfName := config.NetworkInterfaceConfigInHost.Interface.Name
				var err error
				if ns != "" {
					err = nsDetachNetdev(ns, ifName, hostIfName, config.NetworkInterfaceStateInHost)
				}
				switch {
				case ns != "" && err == nil:
					netdevDetached = true
				case ns == "" || errors.Is(err, fs.ErrNotExist):
					// The namespace is already gone, so the kernel has returned,
					// or is returning, the interface on its own: down, without
					// its host settings, renamed if the name was taken. Find it
					// and put it back the way it was.
					found, restoreErr := returnHostLinkWithRetry(hostIfName, config.NetworkInterfaceStateInHost)
					switch {
					case restoreErr != nil:
						logger.Error(restoreErr, "Failed to restore the network device on the host after its namespace was torn down", "device", deviceName, "interface", hostIfName)
					case !found:
						logger.Info("Network device has not come back to the host yet after its namespace was torn down; leaving it to the kernel", "device", deviceName, "interface", hostIfName)
					default:
						logger.V(2).Info("Restored the network device on the host after its namespace was torn down", "device", deviceName, "interface", hostIfName)
						netdevDetached = true
					}
				default:
					logger.Error(err, "Failed to return network device", "device", deviceName)
				}
			}
		}

		if needsRescanAfterDetach(rdmaDetached, netdevDetached) {
			needsRescan = true
		}
	}
	if needsRescan {
		np.netdb.RequestRescan()
	}
	return nil
}

// returnHostLinkWithRetry gives the kernel a moment to finish handing an
// interface back after its namespace is destroyed, which happens just before
// the runtime calls StopPodSandbox, then restores it. It is bounded well
// inside the runtime's budget for the hook.
func returnHostLinkWithRetry(hostIfName string, state *HostLinkState) (bool, error) {
	const attempts, interval = 10, 50 * time.Millisecond
	for i := 0; ; i++ {
		found, err := returnHostLink(hostIfName, state)
		if found || err != nil || i == attempts-1 {
			return found, err
		}
		time.Sleep(interval)
	}
}

// needsRescanAfterDetach reports whether the inventory needs an explicit
// rescan after returning a device's RDMA / netdev to init_net.
//
// The netdev path's NEWLINK (emitted by nsDetachNetdev's LinkSetUp) acts as
// an implicit rescan trigger for the inventory. RDMA returns to init_net do
// not produce an event the inventory observes, so an explicit rescan is
// needed only when RDMA was successfully returned but the netdev path did
// not fire NEWLINK — that is, IB-only devices (no netdev to detach) or
// SR-IOV pods where nsDetachNetdev failed.
//
// Failure cases for the RDMA detach fall back to the inventory's periodic
// poll because the device is still in the pod namespace and a rescan now
// would not observe any state change.
func needsRescanAfterDetach(rdmaDetached, netdevDetached bool) bool {
	return rdmaDetached && !netdevDetached
}

func (np *NetworkDriver) RemovePodSandbox(ctx context.Context, pod *api.PodSandbox) error {
	logger := klog.LoggerWithValues(klog.FromContext(ctx), "pod", klog.KRef(pod.Namespace, pod.Name), "podUID", pod.Uid)
	ctx = klog.NewContext(ctx, logger)
	logger.V(2).Info("RemovePodSandbox")
	start := time.Now()
	status := statusNoop
	defer func() {
		nriPluginRequestsTotal.WithLabelValues(methodRemovePodSandbox, status).Inc()
		nriPluginRequestsLatencySeconds.WithLabelValues(methodRemovePodSandbox, status).Observe(time.Since(start).Seconds())
	}()
	if _, ok := np.podConfigStore.GetPodConfig(types.UID(pod.GetUid())); !ok {
		return nil
	}
	err := np.removePodSandbox(ctx, pod)
	if err != nil {
		status = statusFailed
	} else {
		status = statusSuccess
	}
	return err
}

func (np *NetworkDriver) removePodSandbox(_ context.Context, pod *api.PodSandbox) error {
	return nil
}

func (np *NetworkDriver) Shutdown(ctx context.Context) {
	klog.FromContext(ctx).Info("Runtime shutting down...")
}

func getNetworkNamespace(pod *api.PodSandbox) string {
	// get the pod network namespace
	for _, namespace := range pod.Linux.GetNamespaces() {
		if namespace.Type == "network" {
			return namespace.Path
		}
	}
	return ""
}

func podKey(pod *api.PodSandbox) string {
	return fmt.Sprintf("%s/%s", pod.GetNamespace(), pod.GetName())
}

// NRI gives us *api.PodSandbox while we need *v1.Pod for the Eventf.
// As such, we construct the minimal *v1.Pod object reference needed for the event.
func podObjectRef(pod *api.PodSandbox) *v1.Pod {
	p := &v1.Pod{}
	p.Name = pod.GetName()
	p.Namespace = pod.GetNamespace()
	p.UID = types.UID(pod.GetUid())
	return p
}
