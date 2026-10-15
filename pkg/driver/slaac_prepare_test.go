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
	"net"
	"net/netip"
	"os"
	"path"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"

	"sigs.k8s.io/dranet/internal/nlwrap"
	userns "sigs.k8s.io/dranet/internal/testutils"
	"sigs.k8s.io/dranet/pkg/apis"
)

// Preparing a passthrough device with addressing SLAAC solicits the router
// advertisement on the host interface, like DHCP, and stores what the Pod
// gets: the host's IPv4 address, the address of the claim and device in the
// advertised prefix, and the default route through the advertising router,
// but none of the host's IPv6 routes.
func TestPrepareResourceClaimSLAAC(t *testing.T) {
	userns.Run(t, testPrepareResourceClaimSLAAC, syscall.CLONE_NEWNET)
}

func testPrepareResourceClaimSLAAC(t *testing.T) {
	client, routerLink := slaacTestLink(t)
	router := linkLocalAddress(t, routerLink)
	// The host's own configuration of the interface: an IPv4 address, which
	// the Pod inherits, and IPv6 routes, which SLAAC replaces: a route and the
	// default route a host's kernel adds from the advertisements. The test
	// adds that one itself, with the kernel's autoconfiguration off, so that
	// it is there regardless of timing.
	if err := os.WriteFile("/proc/sys/net/ipv6/conf/"+client.Attrs().Name+"/accept_ra", []byte("0"), 0o644); err != nil {
		t.Fatalf("turn off router advertisements: %v", err)
	}
	if err := netlink.AddrAdd(client, &netlink.Addr{IPNet: &net.IPNet{IP: net.ParseIP("192.0.2.10"), Mask: net.CIDRMask(24, 32)}}); err != nil {
		t.Fatalf("add IPv4 address: %v", err)
	}
	for _, dst := range []string{"2001:db8:99::/64", "::/0"} {
		_, hostRoute, _ := net.ParseCIDR(dst)
		if err := netlink.RouteAdd(&netlink.Route{LinkIndex: client.Attrs().Index, Dst: hostRoute, Gw: net.IP(router.AsSlice()), Protocol: unix.RTPROT_RA}); err != nil {
			t.Fatalf("add host IPv6 route %s: %v", dst, err)
		}
	}
	advertisement := testRouterAdvertisement(1800, testPrefixInformation("2001:db8:1::", 64, pioOnLink|pioAutonomous, 2592000, 604800))
	startTestRouter(t, routerLink, advertisement, ndpHopLimit, 0)

	np, claim := slaacTestDriver(client.Attrs().Name)
	if result := np.prepareResourceClaim(t.Context(), claim); result.Err != nil {
		t.Fatalf("prepareResourceClaim() error = %v", result.Err)
	}

	podCfg, ok := np.podConfigStore.GetPodConfig("pod-uid-slaac")
	if !ok {
		t.Fatalf("no pod configuration stored")
	}
	devCfg, ok := podCfg.DeviceConfigs["net-dev-0"]
	if !ok {
		t.Fatalf("no configuration stored for net-dev-0: %v", podCfg.DeviceConfigs)
	}
	inPod := devCfg.NetworkInterfaceConfigInPod
	slaac := slaacAddress(netip.MustParsePrefix("2001:db8:1::/64"), "claim-uid-slaac", "net-dev-0")
	if diff := cmp.Diff([]string{"192.0.2.10/24", slaac.String()}, inPod.Interface.Addresses); diff != "" {
		t.Errorf("addresses mismatch (-want +got):\n%s", diff)
	}
	if inPod.Interface.AcceptRA == nil || *inPod.Interface.AcceptRA != 0 {
		t.Errorf("acceptRA = %v, want 0", inPod.Interface.AcceptRA)
	}
	var ipv6Routes []apis.RouteConfig
	hasIPv4Subnet := false
	for _, route := range inPod.Routes {
		if strings.Contains(route.Destination, ":") {
			ipv6Routes = append(ipv6Routes, route)
		} else if route.Destination == "192.0.2.0/24" {
			hasIPv4Subnet = true
		}
	}
	if diff := cmp.Diff([]apis.RouteConfig{{Destination: "::/0", Gateway: router.String()}}, ipv6Routes); diff != "" {
		t.Errorf("IPv6 routes mismatch (-want +got):\n%s", diff)
	}
	if !hasIPv4Subnet {
		t.Errorf("routes %v do not include the host's IPv4 subnet route", inPod.Routes)
	}
}

// A device whose router advertisement cannot be obtained fails the prepare,
// and nothing is stored for the Pod.
func TestPrepareResourceClaimSLAACFailure(t *testing.T) {
	userns.Run(t, testPrepareResourceClaimSLAACFailure, syscall.CLONE_NEWNET)
}

func testPrepareResourceClaimSLAACFailure(t *testing.T) {
	client, _ := slaacTestLink(t)
	if err := os.WriteFile("/proc/sys/net/ipv6/conf/"+client.Attrs().Name+"/disable_ipv6", []byte("1"), 0o644); err != nil {
		t.Fatalf("disable IPv6: %v", err)
	}
	np, claim := slaacTestDriver(client.Attrs().Name)
	result := np.prepareResourceClaim(t.Context(), claim)
	if result.Err == nil || !strings.Contains(result.Err.Error(), "SLAAC") {
		t.Fatalf("prepareResourceClaim() error = %v, want a SLAAC error", result.Err)
	}
	if podCfg, ok := np.podConfigStore.GetPodConfig("pod-uid-slaac"); ok {
		t.Errorf("pod configuration stored after a failed prepare: %+v", podCfg)
	}
}

// slaacTestDriver returns a driver whose only device is the host interface
// ifName, and a claim for it, for pod-uid-slaac, with addressing SLAAC.
func slaacTestDriver(ifName string) (*NetworkDriver, *resourcev1.ResourceClaim) {
	const driverName = "test.driver"
	claim := &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{UID: "claim-uid-slaac", Namespace: "default", Name: "claim-slaac"},
		Status: resourcev1.ResourceClaimStatus{
			ReservedFor: []resourcev1.ResourceClaimConsumerReference{
				{APIGroup: "", Resource: "pods", Name: "test-pod", UID: "pod-uid-slaac"},
			},
			Allocation: &resourcev1.AllocationResult{
				Devices: resourcev1.DeviceAllocationResult{
					Results: []resourcev1.DeviceRequestAllocationResult{
						{Driver: driverName, Device: "net-dev-0", Request: "req-0"},
					},
					Config: []resourcev1.DeviceAllocationConfiguration{{
						Source: resourcev1.AllocationConfigSourceClaim,
						DeviceConfiguration: resourcev1.DeviceConfiguration{
							Opaque: &resourcev1.OpaqueDeviceConfiguration{
								Driver:     driverName,
								Parameters: apiruntime.RawExtension{Raw: []byte(`{"interface":{"addressing":"SLAAC"}}`)},
							},
						},
					}},
				},
			},
		},
	}
	fakeDB := newFakeInventoryDB()
	fakeDB.IsIBOnlyDeviceFunc = func(string) bool { return false }
	fakeDB.GetNetInterfaceNameFunc = func(string) (string, error) { return ifName, nil }
	fakeDB.GetDeviceFunc = func(name string) (resourcev1.Device, bool) { return resourcev1.Device{Name: name}, true }
	np := &NetworkDriver{
		netdb:          fakeDB,
		driverName:     driverName,
		podConfigStore: mustNewPodConfigStore(),
		eventRecorder:  record.NewFakeRecorder(100),
	}
	return np, claim
}

// Moving a SLAAC interface into a Pod namespace whose CNI disabled IPv6, as
// the OCI VCN-Native CNI does in IPv4-only clusters, enables IPv6 on that
// interface, so that its address can be added, and on lo, through which the
// Pod reaches its own address, and keeps router advertisements off.
func TestNsAttachNetdevSLAAC(t *testing.T) {
	userns.Run(t, testNsAttachNetdevSLAAC, syscall.CLONE_NEWNET, syscall.CLONE_NEWNS)
}

func testNsAttachNetdevSLAAC(t *testing.T) {
	nsPath, ns := newSLAACTestNetns(t)
	// The CNI's setting; writing all also disables IPv6 on lo.
	for _, conf := range []string{"all", "default"} {
		writeNetnsSysctl(t, ns, "net/ipv6/conf/"+conf+"/disable_ipv6", "1")
	}
	if got := readNetnsSysctl(t, ns, "net/ipv6/conf/lo/disable_ipv6"); got != "1" {
		t.Fatalf("lo disable_ipv6 = %s after disabling all, want 1", got)
	}

	const ifName = "slaacpod"
	if err := netlink.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: ifName}}); err != nil {
		t.Fatalf("add dummy: %v", err)
	}
	cfg := apis.InterfaceConfig{
		Name:       ifName,
		Addressing: apis.AddressingModeSLAAC,
		Addresses:  []string{"192.0.2.10/24", "2001:db8:1::1/64"},
	}
	cfg.Default()
	if _, err := nsAttachNetdev(ifName, nsPath, cfg); err != nil {
		t.Fatalf("nsAttachNetdev() error = %v", err)
	}

	nhNs, err := nlwrap.NewHandleAt(ns)
	if err != nil {
		t.Fatalf("netlink handle in the Pod namespace: %v", err)
	}
	defer nhNs.Close()
	link, err := nhNs.LinkByName(ifName)
	if err != nil {
		t.Fatalf("link %s in the Pod namespace: %v", ifName, err)
	}
	addrs, err := nhNs.AddrList(link, netlink.FAMILY_V6)
	if err != nil {
		t.Fatalf("list IPv6 addresses: %v", err)
	}
	found := slices.ContainsFunc(addrs, func(a netlink.Addr) bool { return a.IPNet.String() == "2001:db8:1::1/64" })
	if !found {
		t.Errorf("address 2001:db8:1::1/64 not configured; addresses: %v", addrs)
	}
	for setting, want := range map[string]string{
		"net/ipv6/conf/" + ifName + "/disable_ipv6": "0",
		"net/ipv6/conf/" + ifName + "/accept_ra":    "0",
		"net/ipv6/conf/lo/disable_ipv6":             "0",
		// The namespace settings the CNI chose stay.
		"net/ipv6/conf/all/disable_ipv6":     "1",
		"net/ipv6/conf/default/disable_ipv6": "1",
	} {
		if got := readNetnsSysctl(t, ns, setting); got != want {
			t.Errorf("%s = %s, want %s", setting, got, want)
		}
	}
}

// newSLAACTestNetns creates a named network namespace, the way the container
// runtime creates a Pod's, and returns its path and handle.
func newSLAACTestNetns(t *testing.T) (string, netns.NsHandle) {
	t.Helper()
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatalf("random name: %v", err)
	}
	name := fmt.Sprintf("ns%x", suffix)
	// NewNamed moves the calling thread into the new namespace; keep the
	// goroutine on that thread until it is moved back.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	origns, err := netns.Get()
	if err != nil {
		t.Fatalf("current namespace: %v", err)
	}
	defer origns.Close()
	ns, err := netns.NewNamed(name)
	if err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	if err := netns.Set(origns); err != nil {
		t.Fatalf("restore namespace: %v", err)
	}
	t.Cleanup(func() {
		ns.Close()
		_ = netns.DeleteNamed(name)
	})
	return path.Join("/run/netns", name), ns
}

// inNetns runs f with the calling thread in ns.
func inNetns(t *testing.T, ns netns.NsHandle, f func() error) error {
	t.Helper()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	origns, err := netns.Get()
	if err != nil {
		t.Fatalf("current namespace: %v", err)
	}
	defer origns.Close()
	if err := netns.Set(ns); err != nil {
		t.Fatalf("join namespace: %v", err)
	}
	ferr := f()
	if err := netns.Set(origns); err != nil {
		t.Fatalf("restore namespace: %v", err)
	}
	return ferr
}

func writeNetnsSysctl(t *testing.T, ns netns.NsHandle, setting, value string) {
	t.Helper()
	if err := inNetns(t, ns, func() error {
		return os.WriteFile("/proc/sys/"+setting, []byte(value), 0o644)
	}); err != nil {
		t.Fatalf("write %s: %v", setting, err)
	}
}

func readNetnsSysctl(t *testing.T, ns netns.NsHandle, setting string) string {
	t.Helper()
	var value []byte
	if err := inNetns(t, ns, func() (err error) {
		value, err = os.ReadFile("/proc/sys/" + setting)
		return err
	}); err != nil {
		t.Fatalf("read %s: %v", setting, err)
	}
	return strings.TrimSpace(string(value))
}
