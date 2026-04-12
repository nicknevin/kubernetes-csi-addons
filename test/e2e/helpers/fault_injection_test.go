/*
Copyright 2024 The Kubernetes-CSI-Addons Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.

Unit tests for fault injection helpers (not the replication E2E Ginkgo suite).
*/

package helpers

import (
	"context"
	"testing"

	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestNewFaultInjectionProvider_UnsupportedEnv(t *testing.T) {
	t.Setenv(EnvFaultInjector, "not-a-valid-injector")
	_, err := NewFaultInjectionProvider(FaultInjectionConfig{
		Client: fake.NewClientBuilder().Build(),
	})
	if err == nil {
		t.Fatal("expected error for unsupported E2E_FAULT_INJECTOR value")
	}
}

func TestNewFaultInjectionProvider_ConfigTypeOverridesEnv(t *testing.T) {
	t.Setenv(EnvFaultInjector, string(FaultInjectorNone))
	p, err := NewFaultInjectionProvider(FaultInjectionConfig{
		Type:       FaultInjectorIptables,
		Client:     fake.NewClientBuilder().Build(),
		Namespace:  "default",
		RESTConfig: &rest.Config{},
		ProviderParams: map[string]string{
			"image": DefaultIptablesImageWithRegistry,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.GetProviderType() != FaultInjectorIptables {
		t.Fatalf("got %v want iptables", p.GetProviderType())
	}
}

func TestGetFaultInjectorTypeFromEnv(t *testing.T) {
	cases := []struct {
		env  string
		want FaultInjectorType
	}{
		{"", FaultInjectorIptables},
		{"iptables", FaultInjectorIptables},
		{"  IPTABLES  ", FaultInjectorIptables},
		{"networkfence", FaultInjectorNetworkFence},
		{"none", FaultInjectorNone},
	}
	for _, tc := range cases {
		t.Run(tc.env, func(t *testing.T) {
			t.Setenv(EnvFaultInjector, tc.env)
			got := GetFaultInjectorTypeFromEnv()
			if got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestNoOpFaultProvider(t *testing.T) {
	cfg := FaultInjectionConfig{Client: fake.NewClientBuilder().Build(), Namespace: "ns"}
	p, err := NewNoOpFaultProvider(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if p.GetProviderType() != FaultInjectorNone {
		t.Fatalf("GetProviderType: got %v", p.GetProviderType())
	}
	ctx := context.Background()
	if !p.IsSupported(ctx) {
		t.Fatal("NoOp should be supported")
	}
	if err := p.FenceIP(ctx, "192.0.2.1/32", nil); err != nil {
		t.Fatal(err)
	}
	b, err := p.EstablishConnectivityBaseline(ctx, "192.0.2.1/32")
	if err != nil || b != nil {
		t.Fatalf("EstablishConnectivityBaseline = (%v, %v), want (nil, nil)", b, err)
	}
	ok, err := p.VerifyConnectivity(ctx, "192.0.2.1/32", true, nil)
	if err != nil || !ok {
		t.Fatalf("VerifyConnectivity = %v, %v", ok, err)
	}
	if err := p.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
}
