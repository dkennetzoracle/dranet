package apis

import (
	"hash/fnv"

	"k8s.io/utils/ptr"
)

// Default applies default values to the NetworkConfig.
func (c *NetworkConfig) Default() {
	c.Interface.Default()
	if c.Interface.Type == InterfaceTypeIPVLAN {
		if c.Interface.IPVlan == nil {
			c.Interface.IPVlan = &IPVlanConfig{}
		}
		c.Interface.IPVlan.Default()
	}
	if c.Interface.VRF != nil {
		c.Interface.VRF.Default()
	}
}

// Default applies default values to the InterfaceConfig.
func (c *InterfaceConfig) Default() {
	// Fold the deprecated DHCP field into Addressing when Addressing is unset.
	if c.Addressing == "" && c.DHCP != nil && *c.DHCP {
		c.Addressing = AddressingModeDHCP
	}
	if c.Addressing == AddressingModeSLAAC {
		c.defaultSLAAC()
	}
}

// defaultSLAAC fills in the per-interface IPv6 sysctls that autoconfiguration
// needs. The kernel resets all of them when the interface moves into the Pod, so
// SLAAC only works if they are requested explicitly.
func (c *InterfaceConfig) defaultSLAAC() {
	if c.AcceptRA == nil {
		// 2 rather than 1: the kernel ignores router advertisements on an
		// interface with forwarding enabled unless accept_ra is 2, and a Pod
		// namespace may have forwarding on.
		c.AcceptRA = ptr.To[int32](2)
	}
	if c.DADTransmits == nil {
		// Duplicate Address Detection holds a new address tentative for roughly
		// a second per probe, which does not fit the runtime's sandbox deadline.
		// An autoconfigured address derives from the interface's own hardware
		// address, so there is nothing on the link to collide with.
		c.DADTransmits = ptr.To[int32](0)
	}
	if c.RouterSolicitationDelay == nil {
		// Solicit immediately instead of waiting out the kernel's default
		// one second spreading delay.
		c.RouterSolicitationDelay = ptr.To[int32](0)
	}
}

// Default applies default values to the VRFConfig.
func (c *VRFConfig) Default() {
	if c.Table == nil && c.Name != "" {
		// Derive a deterministic table ID from the VRF name to ensure interfaces
		// joining the same VRF automatically share the same table ID.
		tableID := TableIDForName(c.Name)
		c.Table = &tableID
	}
}

// TableIDForName derives a deterministic DRANET-managed routing table ID from a
// name (e.g. a VRF name or a device identifier). VRF and policy based routing
// share this scheme so their tables come from the same reserved range.
func TableIDForName(name string) int {
	h := fnv.New32a()
	h.Write([]byte(name))
	return int((h.Sum32() % 1000) + RouteTableOffset)
}

// Default applies default values to the IPVlanConfig.
func (c *IPVlanConfig) Default() {
	if c.Mode == "" {
		c.Mode = IPVlanModeL2
	}
	if c.Flag == "" {
		c.Flag = IPVlanFlagBridge
	}
}
