/*
Copyright 2024 The Kubernetes-CSI-Addons Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.

ConnectivityBaseline and CompareProbeResultsToBaseline are used only by the iptables fault injector
(E2E Jobs). NetworkFence uses storage-layer CRs (e.g. VolumeReplication status), not these probes.
*/

package helpers

import (
	"fmt"
	"regexp"
	"strings"
)

// CompareProbeResultsToBaseline returns whether current probes match expected fence state.
// When expectedFenced is true, probes that were true in baseline must be false after (except
// ip_route may remain true when only OUTPUT REJECT is used — see comments).
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
	if before.TracerouteOK && after.TracerouteOK {
		return false
	}
	if before.IPRouteOK && after.IPRouteOK {
		// `ip route get` can still print a route when traffic is dropped by OUTPUT REJECT.
		if before.PingOK || before.TracerouteOK {
			Logf("[INFO]", "ip route get still shows a route after OUTPUT REJECT; ICMP/traceroute already indicate loss")
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
