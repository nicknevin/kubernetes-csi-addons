/*
Copyright 2024 The Kubernetes-CSI-Addons Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.

ConnectivityBaseline and CompareProbeResultsToBaseline are used only by the iptables fault injector
(E2E Jobs). NetworkFence uses storage-layer CRs (e.g. VolumeReplication status), not these probes.
*/

package helpers

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ErrIptablesBaselineSkipEnvironment is returned by EstablishIptablesConnectivityBaselines when
// probe jobs cannot establish a baseline (e.g. no probe succeeded — ICMP blocked everywhere).
// Replication E2E specs may translate this with Ginkgo Skip; helpers must not import Ginkgo.
var ErrIptablesBaselineSkipEnvironment = errors.New("iptables connectivity baseline: environment unsuitable for verify")

// IsIptablesBaselineSkipEnvironmentError reports whether err indicates the cluster cannot run
// iptables connectivity verification (matches errors from IptablesFaultProvider.EstablishConnectivityBaseline).
func IsIptablesBaselineSkipEnvironmentError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "no probe succeeded") ||
		strings.Contains(msg, "cannot verify fencing")
}

// EstablishIptablesConnectivityBaselines runs EstablishConnectivityBaseline for each CIDR when
// injectorType is iptables. For other injectors returns (nil, nil) without calling the provider
// (NetworkFence and NoOp do not use packet baselines).
//
// On failure: wraps with ErrIptablesBaselineSkipEnvironment when IsIptablesBaselineSkipEnvironmentError(err);
// otherwise returns a fatal error (timeouts, API errors, missing CSI_BASELINE).
func EstablishIptablesConnectivityBaselines(ctx context.Context, injectorType FaultInjectorType, p PeerFenceProvider, cidrs []string) (map[string]*ConnectivityBaseline, error) {
	if injectorType != FaultInjectorIptables {
		return nil, nil
	}
	if p == nil {
		return nil, fmt.Errorf("EstablishIptablesConnectivityBaselines: PeerFenceProvider is required for iptables")
	}
	out := make(map[string]*ConnectivityBaseline, len(cidrs))
	for _, cidr := range cidrs {
		b, err := p.EstablishConnectivityBaseline(ctx, cidr)
		if err != nil {
			if IsIptablesBaselineSkipEnvironmentError(err) {
				return nil, fmt.Errorf("%w: %v", ErrIptablesBaselineSkipEnvironment, err)
			}
			return nil, fmt.Errorf("fence baseline failed (expected reachable path to peer before fence; fix probe/job or networking): %w", err)
		}
		out[cidr] = b
	}
	return out, nil
}

// BaselineForCIDR returns the per-CIDR baseline from EstablishIptablesConnectivityBaselines for VerifyConnectivity, or nil.
func BaselineForCIDR(baselines map[string]*ConnectivityBaseline, cidr string) *ConnectivityBaseline {
	if baselines == nil {
		return nil
	}
	return baselines[cidr]
}

// CompareProbeResultsToBaseline returns whether current probes match expected fence state.
// When expectedFenced is true, probes that were true in baseline must be false after, with
// exceptions: ip route get may stay true (local FIB only); if baseline ping succeeded, ping
// loss alone proves partition even when traceroute still matches (traceroute only checks for
// any hop line and often stays “true” after a fence).
func CompareProbeResultsToBaseline(before, after *ConnectivityBaseline, expectedFenced bool) bool {
	if before == nil || after == nil {
		return false
	}
	if expectedFenced {
		return baselineMatchesFenced(before, after)
	}
	return baselineMatchesReachable(before, after)
}

func baselineMatchesFenced(before, after *ConnectivityBaseline) bool {
	if before.PingOK && after.PingOK {
		return false
	}
	// Baseline had ICMP to the peer; losing it after fence proves partition. Do not require
	// traceroute to flip: the probe marks success if any hop line exists (often only the gateway).
	if before.PingOK && !after.PingOK {
		return true
	}
	if before.TracerouteOK && after.TracerouteOK {
		return false
	}
	if before.IPRouteOK && after.IPRouteOK {
		// `ip route get` can still print a route when traffic is dropped by iptables.
		if before.PingOK || before.TracerouteOK {
			Logf("[INFO]", "ip route get still shows a route after fence; ICMP or traceroute already indicate loss")
			return true
		}
		return false
	}
	return true
}

func baselineMatchesReachable(before, after *ConnectivityBaseline) bool {
	if before.PingOK && !after.PingOK {
		return false
	}
	if before.TracerouteOK && !after.TracerouteOK {
		return false
	}
	if before.IPRouteOK && !after.IPRouteOK {
		return false
	}
	return true
}

// ConnectivityBaseline records which probes succeeded before fencing so that
// VerifyConnectivity can require the same probes to fail after fencing (ICMP may
// be blocked in some clusters even without a fence; baseline avoids false negatives).
type ConnectivityBaseline struct {
	TargetIP string
	// Probes that reported "reachable" during EstablishConnectivityBaseline.
	PingOK         bool
	IPRouteOK      bool
	TracerouteOK   bool
	RawSummaryLine string
}

// AnyProbeSucceeded returns true if at least one probe indicated reachability.
func (b *ConnectivityBaseline) AnyProbeSucceeded() bool {
	if b == nil {
		return false
	}
	return b.PingOK || b.IPRouteOK || b.TracerouteOK
}

// String summarizes the baseline for logs.
func (b *ConnectivityBaseline) String() string {
	if b == nil {
		return "<nil>"
	}
	return fmt.Sprintf("target=%s ping=%v ip_route=%v traceroute=%v", b.TargetIP, b.PingOK, b.IPRouteOK, b.TracerouteOK)
}

var baselineLineRE = regexp.MustCompile(`CSI_BASELINE\s+ping=(\d)\s+ip_route=(\d)\s+traceroute=(\d)`)

// ParseConnectivityBaselineFromLog extracts probe results from connectivity job stdout/stderr.
func ParseConnectivityBaselineFromLog(targetIP, log string) (*ConnectivityBaseline, error) {
	for _, line := range strings.Split(log, "\n") {
		line = strings.TrimSpace(line)
		m := baselineLineRE.FindStringSubmatch(line)
		if len(m) != 4 {
			continue
		}
		b := &ConnectivityBaseline{
			TargetIP:       targetIP,
			PingOK:         m[1] == "1",
			IPRouteOK:      m[2] == "1",
			TracerouteOK:   m[3] == "1",
			RawSummaryLine: line,
		}
		return b, nil
	}
	return nil, fmt.Errorf("no CSI_BASELINE line in probe output (target %s)", targetIP)
}
