---
title: "Interface Configuration"
date: 2025-05-25T11:30:40Z
---

To configure network interfaces in DRANET, users can provide custom configurations through the parameters field of a ResourceClaim or ResourceClaimTemplate. This configuration adheres to the NetworkConfig structure, which defines the desired state for network interfaces and their associated routes.

### Network Configuration Overview

The primary structure for custom network configuration is NetworkConfig. It encompasses settings for the network interface itself and any specific routes and rules to be applied within the Pod's network namespace.

```go
type NetworkConfig struct {
	// Interface defines core properties of the network interface.
	// Settings here are typically managed by `ip link` commands.
	Interface InterfaceConfig `json:"interface"`

	// Routes defines static routes to be configured for this interface.
	Routes []RouteConfig `json:"routes,omitempty"`

	// Rules defines routing rules to be configured for this interface.
	Rules []RuleConfig `json:"rules,omitempty"`

	// Neighbors defines permanent neighbor (ARP/NDP) entries to be added for this interface.
	Neighbors []NeighborConfig `json:"neighbors,omitempty"`

	// Ethtool defines hardware offload features and other settings managed by `ethtool`.
	Ethtool *EthtoolConfig `json:"ethtool,omitempty"`
}
```

#### Interface Configuration

The InterfaceConfig structure allows you to specify details for a single network interface.

```go
type InterfaceConfig struct {
	// Name is the desired logical name of the interface inside the Pod (e.g., "net0", "eth_app").
	// If not specified, DRANET may use or derive a name from the original interface.
	Name string `json:"name,omitempty"`

	// Type selects how the allocated device is presented to the Pod:
	//   - "Passthrough" (default): the network device itself is moved into the
	//     Pod's network namespace.
	//   - "IPVLAN": the device stays in the host namespace and an IPVLAN
	//     subinterface is created on top of it inside the Pod.
	// It may be set by the cloud provider or in the user's ResourceClaim.
	// If empty, it is treated as "Passthrough".
	Type InterfaceType `json:"type,omitempty"`

	// Addressing selects the IP address configuration strategy:
	// "Static" (default), "DHCP", "SLAAC", or "Unnumbered".
	// An empty value is treated as "Static".
	Addressing AddressingMode `json:"addressing,omitempty"`

	// Addresses is a list of IP addresses in CIDR format (e.g., "192.168.1.10/24")
	// to be assigned to the interface.
	Addresses []string `json:"addresses,omitempty"`

	// MTU is the Maximum Transmission Unit for the interface.
	// An IPVLAN subinterface must not exceed the parent MTU. When unset, the
	// child inherits the parent MTU.
	MTU *int32 `json:"mtu,omitempty"`

	// HardwareAddr is the MAC address of the interface. Passthrough only: an
	// IPVLAN subinterface always uses its parent's MAC address.
	HardwareAddr *string `json:"hardwareAddr,omitempty"`

	// GSOMaxSize sets the maximum Generic Segmentation Offload size for IPv6.
	// Managed by `ip link set <dev> gso_max_size <val>`. For enabling Big TCP.
	GSOMaxSize *int32 `json:"gsoMaxSize,omitempty"`

	// GROMaxSize sets the maximum Generic Receive Offload size for IPv6.
	// Managed by `ip link set <dev> gro_max_size <val>`. For enabling Big TCP.
	GROMaxSize *int32 `json:"groMaxSize,omitempty"`

	// GSOv4MaxSize sets the maximum Generic Segmentation Offload size.
	// Managed by `ip link set <dev> gso_ipv4_max_size <val>`. For enabling Big TCP.
	GSOIPv4MaxSize *int32 `json:"gsoIPv4MaxSize,omitempty"`

	// GROv4MaxSize sets the maximum Generic Receive Offload size.
	// Managed by `ip link set <dev> gro_ipv4_max_size <val>`. For enabling Big TCP.
	GROIPv4MaxSize *int32 `json:"groIPv4MaxSize,omitempty"`

	// ARPIgnore controls which ARP requests the interface answers through
	// /proc/sys/net/ipv4/conf/<iface>/arp_ignore. Valid values are 0-3 and 8.
	// Linux uses max(conf/all, conf/<iface>) as the effective value.
	// Moving the interface resets it to the destination namespace default, so it
	// must be requested explicitly.
	ARPIgnore *int32 `json:"arpIgnore,omitempty"`

	// ARPAnnounce controls the source address used in ARP requests through
	// /proc/sys/net/ipv4/conf/<iface>/arp_announce. Valid values are 0-2.
	// Linux uses max(conf/all, conf/<iface>) as the effective value.
	// Moving the interface resets it to the destination namespace default, so it
	// must be requested explicitly.
	ARPAnnounce *int32 `json:"arpAnnounce,omitempty"`

	// AcceptRA controls IPv6 router advertisement acceptance through
	// /proc/sys/net/ipv6/conf/<iface>/accept_ra. Valid values are 0-2.
	// Moving the interface resets it to the destination namespace default, so it
	// must be requested explicitly. Addressing SLAAC defaults it to 0 and
	// accepts no other value.
	AcceptRA *int32 `json:"acceptRA,omitempty"`
}
```

* **name** (string, optional): The logical name that the interface will have inside the Pod (e.g., "eth0", "enp0s3"). If not specified, DRANET will keep the original name if compliant.
* **type** (string, optional): How the device is presented to the Pod. `Passthrough` (the default) moves the device into the Pod. `IPVLAN` keeps the device on the host and creates an IPVLAN subinterface on top of it inside the Pod.
* **addressing** (string, optional): How the interface gets its IP configuration: `Static` (the default), `DHCP`, `SLAAC` or `Unnumbered`. See [Addressing modes](#addressing-modes).
* **addresses** ([]string, optional): A list of IP addresses in CIDR format (e.g., "192.168.1.10/24", "2001:db8::1/64") to be assigned to the interface.
* **mtu** (int32, optional): The Maximum Transmission Unit for the interface. For an `IPVLAN` subinterface the value must not exceed the parent MTU. When omitted, the child inherits the parent MTU.
* **hardwareAddr** (string, optional): The MAC address of the interface. Passthrough only. An `IPVLAN` subinterface always uses its parent's MAC address, so the field is rejected.
* **gsoMaxSize** (int32, optional): The maximum Generic Segmentation Offload size for IPv6.
* **groMaxSize** (int32, optional): The maximum Generic Receive Offload size for IPv6.
* **gsoIPv4MaxSize** (int32, optional): The maximum Generic Segmentation Offload size for IPv4.
* **groIPv4MaxSize** (int32, optional): The maximum Generic Receive Offload size for IPv4.
* **arpIgnore** (int32, optional): Which ARP requests the interface answers. Valid values are 0, 1, 2, 3, and 8. Sets `/proc/sys/net/ipv4/conf/<iface>/arp_ignore`.
* **arpAnnounce** (int32, optional): The source address the interface uses in ARP requests, from 0 to 2. Sets `/proc/sys/net/ipv4/conf/<iface>/arp_announce`.
* **acceptRA** (int32, optional): Whether the interface accepts IPv6 router advertisements. `0` rejects them, `1` accepts them when forwarding is off, and `2` accepts them also when forwarding is on. Sets `/proc/sys/net/ipv6/conf/<iface>/accept_ra`.

The kernel resets both ARP settings to the network namespace default when an interface
moves into a Pod, so a value configured on the host does not survive the move and has to
be requested here. The kernel resets `accept_ra` the same way when the interface moves.
The kernel creates IPv6 settings only for an interface with an MTU of 1280 or more.
A claim with `acceptRA` is rejected when its `mtu` is below 1280, or when it sets no `mtu`
and the interface would keep a smaller MTU from the host. The check runs before the interface
is touched. When the interface has no `accept_ra` sysctl, for example on a node that boots
with `ipv6.disable=1`, `acceptRA: 0` is already satisfied, and `1` or `2` fail with an error
that says so.
Setups that attach several interfaces sharing one IP subnet, such as
multi-NIC RDMA nodes, typically need `arpIgnore: 1` and `arpAnnounce: 2`. Without them an
interface can answer ARP for another interface's address, or send requests with a source
address from the wrong subnet, which makes neighbor resolution pick the wrong link.

Linux uses the maximum of the namespace-wide `conf/all` value and the per-interface value
for both ARP settings. A per-interface ARP setting cannot reduce the effective value below
`conf/all`. New network namespaces normally inherit the IPv4 `conf/all` and `conf/default`
values from the initial network namespace, subject to `net.core.devconf_inherit_init_net`.
The IPv6 values start at the kernel defaults (`accept_ra` is 1) unless that sysctl is 1 or 3.
DRANET only changes the per-interface value.

##### Addressing modes

`addressing` selects where the interface's IP configuration comes from:

* `Static` (default): the addresses in `addresses`, or the ones a cloud provider profile
  resolves. A passthrough interface with neither keeps the global addresses it had on the host.
* `DHCP`: an IPv4 address and the classless static routes (option 121) of a DHCP lease. The
  lease is obtained on the host interface before it is handed to the Pod, and released when the
  claim goes away. `addresses` must be empty.
* `SLAAC`: the IPv6 configuration of the link's Router Advertisement, applied statically. See
  [SLAAC addressing](#slaac-addressing).
* `Unnumbered`: the interface is brought up without addresses or routes. Subinterfaces only.

##### SLAAC addressing

`addressing: SLAAC` configures the interface from the IPv6 Router Advertisement of its link, the
way `DHCP` configures it from a DHCP lease. During NodePrepareResources DRANET sends Router
Solicitations on the host interface, which for an `IPVLAN` child is its parent, and waits up to
5 seconds, soliciting at 0, 1 and 3 seconds. From the first advertisement with a usable
autonomous `/64` prefix, the Pod gets:

* An address in each autonomous `/64` prefix that is not deprecated, with the prefix length when
  the prefix is on-link and `/128` when it is not.
* A link route for each on-link prefix it gets no address in.
* The default route through the advertising router when it is a default router, and the more
  specific routes it advertises (RFC 4191).
* IPv4 as for `Static`: the IPv4 addresses in `addresses`, or else, for a passthrough interface,
  the host's global IPv4 addresses. A passthrough interface also keeps its host IPv4 routes. Its
  host IPv6 addresses and routes are not copied: the advertisement replaces them.

The interface identifier is a hash of the claim UID and the device name. The address is the same
each time the claim is prepared, differs for each claim and device, and does not depend on the
MAC address, which an `IPVLAN` child shares with its parent.

DRANET applies the result once, as static configuration, when the Pod starts. The Pod does not
process router advertisements: SLAAC sets `acceptRA` to 0 and rejects any other value. It also
rejects IPv6 addresses in `addresses`, `dhcp: true`, and an `mtu` below 1280. Without an `mtu`,
prepare fails when the host interface has an MTU below 1280.

```yaml
config:
- opaque:
    driver: dra.net
    parameters:
      interface:
        name: "net1"
        addressing: "SLAAC"
```

* DRANET brings the host interface up if it is down. Where the host has IPv6 enabled on it, the
  host kernel then also autoconfigures from the advertisement, until the interface moves into
  the Pod, or for good on the parent of an `IPVLAN` child.
* The host interface must be an Ethernet interface. The exchange uses a packet socket rather than
  the host's IPv6 stack, so it works on a host interface with IPv6 disabled. A link without a
  router, or whose routers hand out addresses through DHCPv6 (`M` flag without a prefix), fails
  the claim after 5 seconds with an error that says so.
* Any sender on the link can answer the solicitation, as with kernel SLAAC. DRANET only takes
  advertisements that arrive on the host interface itself, not those of a VLAN on it, and not
  those an `IPVLAN` child of it sends. Protect the fabric with RA Guard where other hosts or
  Pods on the link are not trusted. Switch RA Guard does not see traffic between functions of
  the same NIC, such as SR-IOV VFs of its PF: run untrusted Pods that get such functions without
  `NET_RAW`, or filter advertisements on the NIC.
* The lifetimes in the advertisement are not tracked. The addresses and routes stay until the
  Pod ends, and a later advertisement, from another router or with a new prefix, does not change
  them. The router that answered first is the Pod's only default router, even on a link with
  several. A claim reports at most 16 addresses, so further prefixes get no address. The other fields of the advertisement, such as the MTU, the hop limit, the neighbor
  discovery timers and DNS options, are ignored.
* Duplicate address detection runs as the kernel default, so the address is tentative for a
  second or two after the Pod starts. A failed detection is not reported.
* Some CNI plugins disable IPv6 in the Pod network namespace of an IPv4-only cluster. DRANET
  enables it on the interface, on `lo`, through which the Pod reaches its own addresses, and on
  the interface's VRF device, and leaves the namespace defaults as they are.
* DRANET does not replace a route the Pod's table already has to the same destination with the
  same metric, and logs it instead at verbosity 4. In a dual-stack cluster the CNI's IPv6 default route through
  `eth0` stays, and of several SLAAC interfaces only one, which one is not specified, gets its
  default route; the others reach only their on-link prefixes and the routes their router
  advertises. Without that conflict, the advertised default route carries all of the Pod's IPv6
  traffic that has no more specific route, such as traffic to AAAA records.
* To give each SLAAC interface its own default route, put it in its own VRF (`interface.vrf`):
  its routes then go in the VRF's table, and the interface keeps its IPv6 addresses when it
  joins the VRF. Applications must then bind to the VRF or the interface, with
  `SO_BINDTODEVICE` or `ip vrf exec`, to use it; an unbound socket cannot reach it, nor bind to
  its address.

##### IPVLAN subinterfaces

An `IPVLAN` subinterface is created inside the Pod network namespace on top of the
host device. The `mtu`, GSO and GRO sizes, `arpIgnore`, `arpAnnounce`, and `acceptRA` settings
apply to the child. They work the same way as on a passthrough interface. The child inherits
the TSO maximum of its parent. The kernel rejects a GSO size above that maximum. It also
rejects a GRO size above the global kernel maximum. A later change of the parent MTU
resets the child MTU to the new parent value.

A minimal claim configuration that requests an IPVLAN subinterface:

```yaml
config:
- opaque:
    driver: dra.net
    parameters:
      interface:
        type: "IPVLAN"
        name: "net1"
        addresses:
        - "192.0.2.10/24"
        mtu: 1400
        arpIgnore: 1
        arpAnnounce: 2
```

Two settings are not supported for subinterfaces and are rejected by validation:

* `hardwareAddr`: the child always uses the parent MAC address.
* DHCP addressing: unsupported and untested. The DHCP client runs on the host parent
  interface before the subinterface exists.

`SLAAC` addressing is supported. DRANET solicits the Router Advertisement on the parent, whose
link the child shares, and the child gets an interface identifier of its own.

#### Route Configuration (RouteConfig)

The RouteConfig structure defines individual network routes to be added to the Pod's network namespace, associated with the configured interface.

```go
type RouteConfig struct {
	Destination string `json:"destination,omitempty"`
	Gateway     string `json:"gateway,omitempty"`
	Source      string `json:"source,omitempty"`
	Scope       uint8  `json:"scope,omitempty"`
	Table       int    `json:"table,omitempty"`
}
```

* **destination** (string, optional): The destination network in CIDR format (e.g., "0.0.0.0/0" for a default route, "10.0.0.0/8" for a specific subnet).  
* **gateway** (string, optional): The IP address of the gateway for the route. This field is mandatory for routes with Universe scope (0).  
* **source** (string, optional): An optional source IP address for policy routing.  
* **scope** (uint8, optional): The scope of the route. Only Link (253) or Universe (0) are allowed.  
  * Link (253): Routes directly to a device without a gateway (e.g., for directly connected subnets).  
  * Universe (0): Routes to a network via a gateway.
* **table** (int, optional): The routing table to use for the route. Defaults to the main table (254) if not specified.

#### Rule Configuration (RuleConfig)

The RuleConfig structure defines individual routing rules to be added to the Pod's network namespace.

```go
type RuleConfig struct {
	// Priority is the priority of the rule.
	Priority int `json:"priority,omitempty"`
	// Source is the source IP address for the rule.
	Source string `json:"source,omitempty"`
	// Destination is the destination IP address for the rule.
	Destination string `json:"destination,omitempty"`
	// Table is the routing table to use for the rule.
	Table int `json:"table,omitempty"`
}
```

* **priority** (int, optional): The priority of the rule. Lower values mean higher priority. Defaults to a kernel-assigned value if not specified.
* **source** (string, optional): The source IP address or CIDR for the rule (e.g., "192.168.1.0/24").
* **destination** (string, optional): The destination IP address or CIDR for the rule (e.g., "10.0.0.0/8").
* **table** (int, optional): The routing table to use for the rule. Defaults to the main table (254) if not specified.

#### Neighbor Configuration (NeighborConfig)

The NeighborConfig structure defines permanent neighbor entries (ARP for IPv4, NDP for IPv6) to be added to the Pod's network namespace.

```go
type NeighborConfig struct {
	// Destination is the target IP address.
	Destination string `json:"destination,omitempty"`
	// HardwareAddr is the MAC address of the neighbor.
	HardwareAddr string `json:"hardwareAddr,omitempty"`
}
```

* **ipAddress** (string, required): The IP address of the neighbor (e.g., "192.168.1.1", "2001:db8::1").
* **hardwareAddr** (string, required): The MAC address of the neighbor (e.g., "00:11:22:33:44:55").

#### Ethtool Configuration (EthtoolConfig)

The EthtoolConfig structure allows for the configuration of hardware offload features and other settings managed by ethtool.

```go
// EthtoolConfig defines ethtool-based optimizations for a network interface.
// These settings correspond to features typically toggled using `ethtool -K <dev> <feature> on|off`.
type EthtoolConfig struct {
	// Features is a map of ethtool feature names to their desired state (true for on, false for off).
	// Example: {"tcp-segmentation-offload": true, "rx-checksum": true}
	Features map[string]bool `json:"features,omitempty"`

	// PrivateFlags is a map of device-specific private flag names to their desired state.
	// Example: {"my-custom-flag": true}
	PrivateFlags map[string]bool `json:"privateFlags,omitempty"`
}
```

* **features** (map[string]bool, optional): A map of ethtool feature names to their desired state (true for on, false for off). For example, {"tcp-segmentation-offload": true, "rx-checksum": true}.
* **privateFlags** (map[string]bool, optional): A map of device-specific private flag names to their desired state. For example, {"my-custom-flag": true}.

### Example: Customizing a Network Interface and Routes

Below is an example of a ResourceClaim that allocates a dummy interface, renames it to "dranet0", assigns a static IP address, configures two routes (one to a subnet via a gateway and another link-scoped route), and adds a permanent IPv4 neighbor entry. It also disables several ethtool features.

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: dummy-interface-advanced
spec:
  devices:
    requests:
    - name: req-dummy-advanced
      exactly:
        deviceClassName: dra.net
        selectors:
          - cel:
              expression: device.attributes["dra.net"].ifName == "dummy3"
    config:
    - opaque:
        driver: dra.net
        parameters:
          interface:
            name: "dranet0"
            addresses:
            - "169.254.169.14/24"
            mtu: 4321
            hardwareAddr: "00:11:22:33:44:55"
          routes:
          - destination: "169.254.169.0/24"
            gateway: "169.254.169.1"
          - destination: "169.254.169.1/32"
            scope: 253
          neighbors:
          - ipAddress: "192.168.1.1"
            hardwareAddr: "00:11:22:33:44:55"
          ethtool:
            features:
              tcp-segmentation-offload: false
              generic-receive-offload: false
              large-receive-offload: false
---
apiVersion: v1
kind: Pod
metadata:
  name: pod-advanced-cfg
  labels:
    app: pod
spec:
  containers:
  - name: ctr1
    image: registry.k8s.io/e2e-test-images/agnhost:2.54
    # Keep the container running
    command: ["sleep", "infinity"]
  resourceClaims:
  - name: dummy1
    resourceClaimName: dummy-interface-advanced
```
