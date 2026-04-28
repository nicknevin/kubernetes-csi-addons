/*
Copyright 2024 The Kubernetes-CSI-Addons Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package helpers

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestNetworkFenceHandlerStripPort verifies that the NetworkFenceHandler correctly strips port from CIDR
func TestNetworkFenceHandlerStripPort(t *testing.T) {
	testCases := []struct {
		name        string
		cidr        string
		expectError bool
		expectedStr string
	}{
		{
			name:        "IPv4 CIDR with port",
			cidr:        "192.168.1.10/32:6800",
			expectError: false,
			expectedStr: "192.168.1.10/32",
		},
		{
			name:        "IPv4 CIDR without port",
			cidr:        "192.168.1.10/32",
			expectError: false,
			expectedStr: "192.168.1.10/32",
		},
		{
			name:        "IPv6 CIDR with port",
			cidr:        "[2001:db8::1]/128:6800",
			expectError: false,
			expectedStr: "[2001:db8::1]/128",
		},
		{
			name:        "IPv6 CIDR without port",
			cidr:        "[2001:db8::1]/128",
			expectError: false,
			expectedStr: "[2001:db8::1]/128",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := stripPort(tc.cidr)

			if tc.expectError {
				assert.Error(t, err, "expected error but got none")
			} else {
				assert.NoError(t, err, "expected no error")
				assert.Equal(t, tc.expectedStr, result, "CIDR mismatch")
			}
		})
	}
}

// TestNetworkFenceHandlerValidateCIDR checks CIDR validation
func TestNetworkFenceHandlerValidateCIDR(t *testing.T) {
	testCases := []struct {
		name    string
		cidr    string
		isValid bool
	}{
		{
			name:    "Valid IPv4 CIDR",
			cidr:    "192.168.1.0/24",
			isValid: true,
		},
		{
			name:    "Valid IPv4 single host",
			cidr:    "192.168.1.10/32",
			isValid: true,
		},
		{
			name:    "Invalid CIDR",
			cidr:    "192.168.1.999/24",
			isValid: false,
		},
		{
			name:    "Invalid CIDR mask",
			cidr:    "192.168.1.0/33",
			isValid: false,
		},
		{
			name:    "Empty CIDR",
			cidr:    "",
			isValid: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseCIDR(tc.cidr)

			if tc.isValid {
				assert.NoError(t, err, "expected valid CIDR")
			} else {
				assert.Error(t, err, "expected invalid CIDR")
			}
		})
	}
}
