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
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"path"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/dranet/internal/nlwrap"
	userns "sigs.k8s.io/dranet/internal/testutils"
	"sigs.k8s.io/dranet/pkg/apis"
)

// The tests below move a SLAAC interface into a Pod namespace whose CNI
// disabled IPv6, as the OCI VCN-Native CNI does in IPv4-only clusters. The
// interface gets IPv6 enabled, so that its address can be added, and so does
// lo, through which the Pod reaches its own address; router advertisements
// stay off, and the namespace defaults the CNI chose are left alone.

func TestNsAttachNetdevSLAAC(t *testing.T) {
	userns.Run(t, testNsAttachNetdevSLAAC, syscall.CLONE_NEWNET, syscall.CLONE_NEWNS)
}

func testNsAttachNetdevSLAAC(t *testing.T) {
	nsPath, ns := newSLAACTestNetns(t)
	disableIPv6LikeCNI(t, ns)

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
	checkSLAACInterfaceInNetns(t, ns, ifName, "2001:db8:1::1/64")
	// The Pod reaches its own address through lo.
	if err := connectInNetns(t, ns, "2001:db8:1::1", ""); err != nil {
		t.Errorf("connect to 2001:db8:1::1: %v", err)
	}
}

func TestNsCreateSubinterfaceSLAAC(t *testing.T) {
	userns.Run(t, testNsCreateSubinterfaceSLAAC, syscall.CLONE_NEWNET, syscall.CLONE_NEWNS)
}

// The same holds for an IPVLAN child, and its parent on the host keeps its own
// settings.
func testNsCreateSubinterfaceSLAAC(t *testing.T) {
	env := newIPVlanTestEnv(t, 1500)
	disableIPv6LikeCNI(t, env.testNS)
	parentSettings := func() []string {
		var values []string
		for _, setting := range []string{"disable_ipv6", "accept_ra"} {
			b, err := os.ReadFile("/proc/sys/net/ipv6/conf/" + env.parent + "/" + setting)
			if err != nil {
				t.Fatalf("read %s of %s: %v", setting, env.parent, err)
			}
			values = append(values, strings.TrimSpace(string(b)))
		}
		return values
	}
	before := parentSettings()

	cfg := apis.InterfaceConfig{
		Name:       "slaacchild",
		Type:       apis.InterfaceTypeIPVLAN,
		Addressing: apis.AddressingModeSLAAC,
		Addresses:  []string{"2001:db8:1::2/64"},
	}
	cfg.Default()
	if _, err := nsCreateSubinterface(env.parent, env.nsPath, cfg); err != nil {
		t.Fatalf("nsCreateSubinterface() error = %v", err)
	}
	checkSLAACInterfaceInNetns(t, env.testNS, cfg.Name, "2001:db8:1::2/64")
	if after := parentSettings(); !slices.Equal(before, after) {
		t.Errorf("parent %s disable_ipv6, accept_ra = %v, was %v", env.parent, after, before)
	}
}

func TestApplyVRFConfigSLAAC(t *testing.T) {
	userns.Run(t, testApplyVRFConfigSLAAC, syscall.CLONE_NEWNET, syscall.CLONE_NEWNS)
}

// A VRF created in that namespace starts with IPv6 disabled too. The Pod
// reaches the addresses of the interface through the VRF device, so it gets
// IPv6 enabled with the interface.
func testApplyVRFConfigSLAAC(t *testing.T) {
	nsPath, ns := newSLAACTestNetns(t)
	disableIPv6LikeCNI(t, ns)

	const ifName = "slaacvrf"
	if err := netlink.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: ifName}}); err != nil {
		t.Fatalf("add dummy: %v", err)
	}
	cfg := apis.InterfaceConfig{
		Name:       ifName,
		Addressing: apis.AddressingModeSLAAC,
		Addresses:  []string{"2001:db8:1::5/64"},
		VRF:        &apis.VRFConfig{Name: "vrf-blue", Table: ptr.To(101)},
	}
	cfg.Default()
	if _, err := nsAttachNetdev(ifName, nsPath, cfg); err != nil {
		t.Fatalf("nsAttachNetdev() error = %v", err)
	}
	if _, err := applyVRFConfig(nsPath, ifName, cfg.VRF); err != nil {
		t.Fatalf("applyVRFConfig() error = %v", err)
	}
	checkSLAACInterfaceInNetns(t, ns, ifName, "2001:db8:1::5/64")
	if got := readNetnsSysctl(t, ns, "net/ipv6/conf/vrf-blue/disable_ipv6"); got != "0" {
		t.Errorf("vrf-blue disable_ipv6 = %s, want 0", got)
	}

	// A connection in the VRF to the Pod's own address.
	if err := connectInNetns(t, ns, "2001:db8:1::5", "vrf-blue"); err != nil {
		t.Errorf("connect to 2001:db8:1::5 in vrf-blue: %v", err)
	}
}

// connectInNetns opens a TCP connection in ns to a listener on address, with
// both sockets bound to device when it is not empty.
func connectInNetns(t *testing.T, ns netns.NsHandle, address, device string) error {
	t.Helper()
	control := func(_, _ string, rc syscall.RawConn) error {
		if device == "" {
			return nil
		}
		var serr error
		if err := rc.Control(func(fd uintptr) { serr = unix.BindToDevice(int(fd), device) }); err != nil {
			return err
		}
		return serr
	}
	return inNetns(t, ns, func() error {
		lc := net.ListenConfig{Control: control}
		ln, err := lc.Listen(context.Background(), "tcp6", net.JoinHostPort(address, "0"))
		if err != nil {
			return err
		}
		defer ln.Close()
		go func() {
			if c, err := ln.Accept(); err == nil {
				c.Close()
			}
		}()
		dialer := net.Dialer{Timeout: 2 * time.Second, Control: control}
		c, err := dialer.DialContext(context.Background(), "tcp6", ln.Addr().String())
		if err != nil {
			return err
		}
		return c.Close()
	})
}

// disableIPv6LikeCNI disables IPv6 in ns the way a CNI plugin does; writing
// all also disables it on lo.
func disableIPv6LikeCNI(t *testing.T, ns netns.NsHandle) {
	t.Helper()
	for _, conf := range []string{"all", "default"} {
		writeNetnsSysctl(t, ns, "net/ipv6/conf/"+conf+"/disable_ipv6", "1")
	}
	if got := readNetnsSysctl(t, ns, "net/ipv6/conf/lo/disable_ipv6"); got != "1" {
		t.Fatalf("lo disable_ipv6 = %s after disabling all, want 1", got)
	}
}

// checkSLAACInterfaceInNetns checks that ifName in ns has address, IPv6
// enabled and router advertisements off, that lo has IPv6 enabled, and that
// the namespace defaults are still disabled.
func checkSLAACInterfaceInNetns(t *testing.T, ns netns.NsHandle, ifName, address string) {
	t.Helper()
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
	if !slices.ContainsFunc(addrs, func(a netlink.Addr) bool { return a.IPNet.String() == address }) {
		t.Errorf("address %s not configured on %s; addresses: %v", address, ifName, addrs)
	}
	for setting, want := range map[string]string{
		"net/ipv6/conf/" + ifName + "/disable_ipv6": "0",
		"net/ipv6/conf/" + ifName + "/accept_ra":    "0",
		"net/ipv6/conf/lo/disable_ipv6":             "0",
		"net/ipv6/conf/all/disable_ipv6":            "1",
		"net/ipv6/conf/default/disable_ipv6":        "1",
	} {
		if got := readNetnsSysctl(t, ns, setting); got != want {
			t.Errorf("%s = %s, want %s", setting, got, want)
		}
	}
}

// newSLAACTestNetns creates a named network namespace with lo up, the way the
// container runtime creates a Pod's, and returns its path and handle.
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
	// The container runtime brings lo up.
	nhNs, err := nlwrap.NewHandleAt(ns)
	if err != nil {
		t.Fatalf("netlink handle in the namespace: %v", err)
	}
	defer nhNs.Close()
	lo, err := nhNs.LinkByName("lo")
	if err != nil {
		t.Fatal(err)
	}
	if err := nhNs.LinkSetUp(lo); err != nil {
		t.Fatalf("set lo up: %v", err)
	}
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
