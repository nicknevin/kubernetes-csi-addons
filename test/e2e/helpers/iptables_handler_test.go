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
	"context"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestIptablesHandlerPortExtraction tests that port is correctly extracted from targets
func TestIptablesHandlerPortExtraction(t *testing.T) {
	tests := []struct {
		name         string
		target       string
		expectedPort string
		expectedCIDR string
	}{
		{
			name:         "IPv4 CIDR with port",
			target:       "192.168.122.82/32:9283",
			expectedPort: "9283",
			expectedCIDR: "192.168.122.82/32",
		},
		{
			name:         "IPv4 CIDR without port",
			target:       "192.168.122.82/32",
			expectedPort: "",
			expectedCIDR: "192.168.122.82/32",
		},
		{
			name:         "IPv6 CIDR with port",
			target:       "[2001:db8::1]/64:6800",
			expectedPort: "6800",
			expectedCIDR: "[2001:db8::1]/64",
		},
		{
			name:         "Plain IPv4 IP with colon (invalid format, no CIDR notation)",
			target:       "10.0.0.1:443",
			expectedPort: "",
			expectedCIDR: "10.0.0.1:443", // Returns whole string as CIDR since no "/" found
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cidr, port, err := parseTargetCIDRWithPort(tt.target)
			if err != nil {
				t.Fatalf("parseTargetCIDRWithPort(%q) failed: %v", tt.target, err)
			}
			if cidr != tt.expectedCIDR {
				t.Errorf("parseTargetCIDRWithPort(%q) CIDR: got %q, want %q", tt.target, cidr, tt.expectedCIDR)
			}
			if port != tt.expectedPort {
				t.Errorf("parseTargetCIDRWithPort(%q) port: got %q, want %q", tt.target, port, tt.expectedPort)
			}
		})
	}
}

// TestExtractIPFromTarget tests extracting plain IP from "IP:port" format
func TestExtractIPFromTarget(t *testing.T) {
	tests := []struct {
		name       string
		target     string
		expectedIP string
	}{
		{
			name:       "CIDR with port",
			target:     "192.168.122.82/32:9283",
			expectedIP: "192.168.122.82/32",
		},
		{
			name:       "CIDR without port",
			target:     "192.168.122.82/32",
			expectedIP: "192.168.122.82/32",
		},
		{
			name:       "Plain IP with port",
			target:     "192.168.122.82:9283",
			expectedIP: "192.168.122.82",
		},
		{
			name:       "Plain IP without port",
			target:     "192.168.122.82",
			expectedIP: "192.168.122.82",
		},
		{
			name:       "IPv6 with port",
			target:     "[2001:db8::1]:9283",
			expectedIP: "[2001:db8::1]",
		},
		{
			name:       "IPv6 without port",
			target:     "[2001:db8::1]",
			expectedIP: "[2001:db8::1]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractIPFromTarget(tt.target)
			if got != tt.expectedIP {
				t.Errorf("extractIPFromTarget(%q) = %q, want %q", tt.target, got, tt.expectedIP)
			}
		})
	}
}

// TestIptablesHandlerCreation tests that IptablesHandler can be created
func TestIptablesHandlerCreation(t *testing.T) {
	config := FaultInjectionConfig{
		Type:      FaultInjectorIptables,
		Client:    fake.NewClientBuilder().Build(),
		Namespace: "default",
		ProviderParams: map[string]string{
			"image": DefaultIptablesImageWithRegistry,
		},
	}

	handler, err := NewIptablesHandler(context.Background(), config)
	if err != nil {
		t.Fatalf("NewIptablesHandler failed: %v", err)
	}

	if handler == nil {
		t.Fatalf("NewIptablesHandler returned nil handler")
	}

	// Verify it's the correct type
	_, ok := handler.(*IptablesHandler)
	if !ok {
		t.Fatalf("handler is not *IptablesHandler: %T", handler)
	}
}

// TestIptablesHandlerFields tests that handler fields are properly initialized
func TestIptablesHandlerFields(t *testing.T) {
	config := FaultInjectionConfig{
		Type:      FaultInjectorIptables,
		Client:    fake.NewClientBuilder().Build(),
		Namespace: "default",
		ProviderParams: map[string]string{
			"image": DefaultIptablesImageWithRegistry,
		},
	}

	handler, err := NewIptablesHandler(context.Background(), config)
	if err != nil {
		t.Fatalf("NewIptablesHandler failed: %v", err)
	}

	h := handler.(*IptablesHandler)
	if h.targets == nil {
		t.Errorf("handler.targets should be initialized, got nil")
	}
	if len(h.targets) != 0 {
		t.Errorf("handler.targets should be empty initially, got %d items", len(h.targets))
	}
}
