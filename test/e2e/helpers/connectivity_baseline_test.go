/*
Copyright 2024 The Kubernetes-CSI-Addons Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package helpers

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestIsIptablesBaselineSkipEnvironmentError(t *testing.T) {
	if !IsIptablesBaselineSkipEnvironmentError(fmt.Errorf("connectivity baseline: no probe succeeded for x")) {
		t.Fatal("expected skip for no probe succeeded")
	}
	if !IsIptablesBaselineSkipEnvironmentError(fmt.Errorf("cannot verify fencing")) {
		t.Fatal("expected skip for cannot verify fencing")
	}
	if IsIptablesBaselineSkipEnvironmentError(fmt.Errorf("kube API timeout")) {
		t.Fatal("unexpected skip for unrelated error")
	}
	if IsIptablesBaselineSkipEnvironmentError(nil) {
		t.Fatal("nil should not be skip")
	}
}

// mockBaselineProvider embeds NoOpFaultProvider and overrides EstablishConnectivityBaseline only.
type mockBaselineProvider struct {
	NoOpFaultProvider
	establish func(ctx context.Context, cidr string) (*ConnectivityBaseline, error)
}

func (m *mockBaselineProvider) EstablishConnectivityBaseline(ctx context.Context, cidr string) (*ConnectivityBaseline, error) {
	if m.establish != nil {
		return m.establish(ctx, cidr)
	}
	return m.NoOpFaultProvider.EstablishConnectivityBaseline(ctx, cidr)
}

func TestEstablishIptablesConnectivityBaselines_nonIptables(t *testing.T) {
	ctx := context.Background()
	m, err := EstablishIptablesConnectivityBaselines(ctx, FaultInjectorNetworkFence, nil, []string{"10.0.0.1/32"})
	if err != nil || m != nil {
		t.Fatalf("got (%v, %v), want (nil, nil)", m, err)
	}
}

func TestEstablishIptablesConnectivityBaselines_iptablesNilProvider(t *testing.T) {
	ctx := context.Background()
	_, err := EstablishIptablesConnectivityBaselines(ctx, FaultInjectorIptables, nil, []string{"10.0.0.1/32"})
	if err == nil {
		t.Fatal("expected error for nil provider")
	}
}

func TestEstablishIptablesConnectivityBaselines_skipEnvironment(t *testing.T) {
	ctx := context.Background()
	p := &mockBaselineProvider{
		establish: func(ctx context.Context, cidr string) (*ConnectivityBaseline, error) {
			return nil, fmt.Errorf("connectivity baseline: no probe succeeded for %s — cannot verify fencing", cidr)
		},
	}
	_, err := EstablishIptablesConnectivityBaselines(ctx, FaultInjectorIptables, p, []string{"192.0.2.1/32"})
	if !errors.Is(err, ErrIptablesBaselineSkipEnvironment) {
		t.Fatalf("got %v want %v", err, ErrIptablesBaselineSkipEnvironment)
	}
}

func TestEstablishIptablesConnectivityBaselines_fatal(t *testing.T) {
	ctx := context.Background()
	p := &mockBaselineProvider{
		establish: func(ctx context.Context, cidr string) (*ConnectivityBaseline, error) {
			return nil, fmt.Errorf("apiserver unavailable")
		},
	}
	_, err := EstablishIptablesConnectivityBaselines(ctx, FaultInjectorIptables, p, []string{"192.0.2.1/32"})
	if err == nil || errors.Is(err, ErrIptablesBaselineSkipEnvironment) {
		t.Fatalf("expected fatal error, got %v", err)
	}
}

func TestEstablishIptablesConnectivityBaselines_ok(t *testing.T) {
	ctx := context.Background()
	want := &ConnectivityBaseline{TargetIP: "192.0.2.1", PingOK: true}
	p := &mockBaselineProvider{
		establish: func(ctx context.Context, cidr string) (*ConnectivityBaseline, error) {
			return want, nil
		},
	}
	m, err := EstablishIptablesConnectivityBaselines(ctx, FaultInjectorIptables, p, []string{"192.0.2.1/32"})
	if err != nil {
		t.Fatal(err)
	}
	if m == nil || m["192.0.2.1/32"] != want {
		t.Fatalf("map: %v", m)
	}
}

func TestBaselineForCIDR(t *testing.T) {
	if BaselineForCIDR(nil, "x") != nil {
		t.Fatal("nil map")
	}
	b := &ConnectivityBaseline{}
	m := map[string]*ConnectivityBaseline{"10.0.0.1/32": b}
	if BaselineForCIDR(m, "10.0.0.1/32") != b {
		t.Fatal("lookup")
	}
	if BaselineForCIDR(m, "other") != nil {
		t.Fatal("missing key")
	}
}
