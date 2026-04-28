/*
Copyright 2024 The Kubernetes-CSI-Addons Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package helpers

import (
	"testing"
)

func TestParseConnectivityBaselineFromLog(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		log     string
		wantP   bool
		wantR   bool
		wantTr  bool
		wantErr bool
	}{
		{
			name:   "standard line",
			log:    "foo\nCSI_BASELINE ping=1 ip_route=0 traceroute=1\n",
			wantP:  true,
			wantR:  false,
			wantTr: true,
		},
		{
			name:    "missing line",
			log:     "no marker here\n",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b, err := ParseConnectivityBaselineFromLog("192.0.2.1", tc.log)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if b.TargetIP != "192.0.2.1" {
				t.Errorf("TargetIP: got %q", b.TargetIP)
			}
			if b.PingOK != tc.wantP || b.IPRouteOK != tc.wantR || b.TracerouteOK != tc.wantTr {
				t.Fatalf("got ping=%v route=%v trace=%v", b.PingOK, b.IPRouteOK, b.TracerouteOK)
			}
		})
	}
}

func TestConnectivityBaseline_AnyProbeSucceeded(t *testing.T) {
	t.Parallel()
	if (&ConnectivityBaseline{}).AnyProbeSucceeded() {
		t.Fatal("empty baseline should be false")
	}
	b := &ConnectivityBaseline{PingOK: true}
	if !b.AnyProbeSucceeded() {
		t.Fatal("expected true")
	}
}

func TestCompareProbeResultsToBaseline_Fenced(t *testing.T) {
	t.Parallel()
	before := &ConnectivityBaseline{PingOK: true, IPRouteOK: true, TracerouteOK: false}
	afterLoss := &ConnectivityBaseline{PingOK: false, IPRouteOK: false, TracerouteOK: false}
	if !CompareProbeResultsToBaseline(before, afterLoss, true) {
		t.Fatal("expected fenced match when ping/route lost")
	}
	afterStillPing := &ConnectivityBaseline{PingOK: true, IPRouteOK: false, TracerouteOK: false}
	if CompareProbeResultsToBaseline(before, afterStillPing, true) {
		t.Fatal("expected failure when ping still ok")
	}
}

func TestCompareProbeResultsToBaseline_Fenced_pingLossEnoughWhenTracerouteNoisy(t *testing.T) {
	t.Parallel()
	// Mirrors e2e: after fence, ping drops but ip route + traceroute probes can stay "true"
	// (local route lookup; traceroute matches any hop line).
	before := &ConnectivityBaseline{PingOK: true, IPRouteOK: true, TracerouteOK: true}
	after := &ConnectivityBaseline{PingOK: false, IPRouteOK: true, TracerouteOK: true}
	if !CompareProbeResultsToBaseline(before, after, true) {
		t.Fatal("expected fenced match when baseline ping worked and ping fails after fence")
	}
}

func TestCompareProbeResultsToBaseline_Reachable(t *testing.T) {
	t.Parallel()
	before := &ConnectivityBaseline{PingOK: true, IPRouteOK: false, TracerouteOK: true}
	after := &ConnectivityBaseline{PingOK: true, IPRouteOK: false, TracerouteOK: true}
	if !CompareProbeResultsToBaseline(before, after, false) {
		t.Fatal("expected reachable match")
	}
	afterBad := &ConnectivityBaseline{PingOK: false, IPRouteOK: false, TracerouteOK: true}
	if CompareProbeResultsToBaseline(before, afterBad, false) {
		t.Fatal("expected failure when ping lost after unfence")
	}
}

func TestCompareProbeResultsToBaseline_Nil(t *testing.T) {
	t.Parallel()
	if CompareProbeResultsToBaseline(nil, &ConnectivityBaseline{}, true) {
		t.Fatal("nil before should be false")
	}
	if CompareProbeResultsToBaseline(&ConnectivityBaseline{}, nil, true) {
		t.Fatal("nil after should be false")
	}
}
