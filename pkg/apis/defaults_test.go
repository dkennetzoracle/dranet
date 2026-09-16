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
package apis

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"k8s.io/utils/ptr"
)

func TestInterfaceConfigDefault(t *testing.T) {
	tests := []struct {
		name string
		cfg  InterfaceConfig
		want InterfaceConfig
	}{
		{
			name: "no addressing leaves the config untouched",
			cfg:  InterfaceConfig{Name: "eth0"},
			want: InterfaceConfig{Name: "eth0"},
		},
		{
			name: "deprecated dhcp field folds into addressing",
			cfg:  InterfaceConfig{Name: "eth0", DHCP: ptr.To(true)},
			want: InterfaceConfig{Name: "eth0", DHCP: ptr.To(true), Addressing: AddressingModeDHCP},
		},
		{
			name: "static addressing does not get the IPv6 sysctls",
			cfg:  InterfaceConfig{Name: "eth0", Addressing: AddressingModeStatic},
			want: InterfaceConfig{Name: "eth0", Addressing: AddressingModeStatic},
		},
		{
			name: "SLAAC fills in the sysctls autoconfiguration needs",
			cfg:  InterfaceConfig{Name: "eth0", Addressing: AddressingModeSLAAC},
			want: InterfaceConfig{
				Name:                    "eth0",
				Addressing:              AddressingModeSLAAC,
				AcceptRA:                ptr.To[int32](2),
				DADTransmits:            ptr.To[int32](0),
				RouterSolicitationDelay: ptr.To[int32](0),
			},
		},
		{
			name: "SLAAC does not overwrite explicit values",
			cfg: InterfaceConfig{
				Name:                    "eth0",
				Addressing:              AddressingModeSLAAC,
				AcceptRA:                ptr.To[int32](1),
				DADTransmits:            ptr.To[int32](2),
				RouterSolicitationDelay: ptr.To[int32](1),
			},
			want: InterfaceConfig{
				Name:                    "eth0",
				Addressing:              AddressingModeSLAAC,
				AcceptRA:                ptr.To[int32](1),
				DADTransmits:            ptr.To[int32](2),
				RouterSolicitationDelay: ptr.To[int32](1),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg
			got.Default()
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("Default() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
