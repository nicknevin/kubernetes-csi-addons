# Ginkgo v2 Test Flow and Execution Guide

This document explains how the Ginkgo v2 test framework is used in the CSI-Addons replication E2E test suite, covering test structure, execution flow, lifecycle management, and cleanup patterns.

## 1. Overview

**Ginkgo v2** is a mature BDD (Behavior-Driven Development) testing framework for Go that provides:
- Expressive test organization with nested contexts
- Flexible lifecycle hooks (BeforeSuite, AfterSuite, BeforeEach, AfterEach)
- Detailed spec reporting with state tracking
- Built-in parallelization support
- Rich test naming and filtering (via `Describe`, `It`, focus expressions)

## 2. Test Suite Structure

### 2.1 Suite Entry Point and Registration

Every Ginkgo test file has a **TestXxx function** that registers the suite with Go's test runner:

```go
package replication

import (
	"testing"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// TestReplicationE2E is the entry point called by `go test`
func TestReplicationE2E(t *testing.T) {
	RegisterFailHandler(Fail)      // Route Ginkgo failures to Go test failures
	RunSpecs(t, "Replication E2E Suite")  // Execute all registered specs
}
```

**Purpose**: 
- `RegisterFailHandler(Fail)` tells Ginkgo to call `Fail()` when an assertion fails
- `RunSpecs(t, description)` scans the package for registered `Describe` blocks and executes them

**When called**: During `go test ./test/e2e/replication` or `make test-replication-e2e`

### 2.2 Init Functions (var = declarations)

Ginkgo specs are registered at **package init time** using `var _ = <block>()` syntax. This pattern uses blank identifiers to execute code during package initialization without creating unused variables:

```go
var _ = BeforeSuite(func() { /* ... */ })
var _ = ReportAfterEach(func(report types.SpecReport) { /* ... */ })
var _ = Describe("EnableVolumeReplication", func() { /* ... */ })
```

**Timing**:
1. Go calls `init()` functions → package variables are initialized
2. Init-time `var _` statements execute → Ginkgo blocks register
3. `TestReplicationE2E(t)` is called → RunSpecs executes all registered blocks

## 3. Suite Lifecycle

### 3.1 BeforeSuite Hook

Executes **once per test run**, before any specs run. Used for cluster-wide setup:

```go
var _ = BeforeSuite(func() {
	Logf("[SETUP]", "checking USE_EXISTING_CLUSTER")
	useExistingCluster = os.Getenv("USE_EXISTING_CLUSTER") == "true"
	if !useExistingCluster {
		Skip("Replication E2E suite requires USE_EXISTING_CLUSTER=true")
	}

	// Load kubeconfig and create Kubernetes clients
	Logf("[SETUP]", "creating Kubernetes client")
	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())

	// One-time environment detection
	Logf("[SETUP]", "detecting StorageClass and provisioner")
	testEnv := GetTestEnv()
	
	// Detect NetworkFence support once
	isSupported := HasNetworkFenceSupport(context.Background(), k8sClient, testEnv.Provisioner)
	networkFenceSupportCached = &isSupported
})
```

**Key behaviors**:
- **Failure**: If BeforeSuite fails, entire suite is skipped
- **Skip**: If Skip() is called, entire suite is skipped (with reason logged)
- **Isolation**: Each call to `Expect()` can fail and stop BeforeSuite
- **Use case**: Connect to cluster, verify prerequisites, create shared test data

### 3.2 ReportAfterEach Hook

Executes **after each spec completes**, with access to spec completion details:

```go
var _ = ReportAfterEach(func(report types.SpecReport) {
	stateStr := report.State.String()
	dur := report.RunTime.Round(time.Millisecond)
	Logf("[SPEC]", "%s -> %s (%s)", report.FullText(), stateStr, dur)
})
```

**SpecReport fields**:
- `FullText()`: Complete spec path (e.g., "EnableVolumeReplication L1-E-001 Enable snapshot mode")
- `State`: Enum (Passed, Failed, Skipped, Pending, Interrupted)
- `RunTime`: Time from spec start to completion
- `Failure`: Detailed failure info if State == Failed

**Use case**: Log test progress, collect metrics per-spec, report results in real-time

### 3.3 ReportAfterSuite Hook

Executes **once after all specs complete**, with full test run information:

```go
var _ = ReportAfterSuite("Replication E2E detailed summary", func(report types.Report) {
	// Report contains:
	// - report.SpecReports: array of all SpecReport objects
	// - report.SuiteDescription, report.SuitePath
	// - report.PreRunStats.TotalSpecs, report.PreRunStats.SpecsThatWillRun
	// - report.StartTime, report.EndTime, report.RunTime
	// - report.SuiteSucceeded: bool

	// Collect specs by state (filtering out lifecycle hooks)
	var passedSpecs, failedSpecs, skippedSpecs []*types.SpecReport
	for i, spec := range report.SpecReports {
		fullText := strings.TrimSpace(spec.FullText())
		
		// Skip lifecycle hooks and internal capability checks
		if fullText == "" { continue }
		if strings.Contains(fullText, "[BeforeSuite]") { continue }
		if strings.Contains(fullText, "[AfterSuite]") { continue }
		
		if spec.State == types.SpecStatePassed {
			passedSpecs = append(passedSpecs, &report.SpecReports[i])
		} else if spec.State == types.SpecStateFailed {
			failedSpecs = append(failedSpecs, &report.SpecReports[i])
		} else if spec.State == types.SpecStateSkipped {
			skippedSpecs = append(skippedSpecs, &report.SpecReports[i])
		}
	}

	// Print summary grouped by status
	fmt.Fprintf(GinkgoWriter, "\n=== PASSED (%d) ===\n", len(passedSpecs))
	for _, spec := range passedSpecs {
		fmt.Fprintf(GinkgoWriter, "  [✅] %s (%s)\n", spec.FullText(), spec.RunTime)
	}
	
	fmt.Fprintf(GinkgoWriter, "\n=== FAILED (%d) ===\n", len(failedSpecs))
	for _, spec := range failedSpecs {
		fmt.Fprintf(GinkgoWriter, "  [❌] %s: %s\n", spec.FullText(), spec.Failure.Message)
	}
	
	fmt.Fprintf(GinkgoWriter, "\n=== SKIPPED (%d) ===\n", len(skippedSpecs))
	for _, spec := range skippedSpecs {
		fmt.Fprintf(GinkgoWriter, "  [⏭️] %s (%s)\n", spec.FullText(), spec.SkipReason)
	}
})
```

**Use case**: Print comprehensive summary, generate reports, aggregate metrics

## 4. Test Organization and Discovery

### 4.1 Describe Blocks

Hierarchically organize tests and provide context. **Describe blocks are detected at init time** and form a tree:

```go
var _ = Describe("EnableVolumeReplication", func() {
	var ctx context.Context
	var env TestEnv

	// Code here runs during registration (init time), not during test execution!
	// Declarations like `var ctx` create variables shared across all It() blocks

	Describe("L1-E-001: Enable snapshot mode replication", func() {
		// Nested Describe: creates hierarchy
		// FullText = "EnableVolumeReplication L1-E-001: Enable snapshot mode replication <It description>"

		It("should create primary volume with replication", func() {
			// This code runs during test execution
		})

		It("should wait for Replicating=True", func() {
			// This code runs during test execution
		})
	})

	Describe("L1-E-002: Enable journal mode replication", func() {
		It("should enable journal mode", func() {
			// This code runs during test execution
		})
	})
})
```

**Important distinction**:
- **Init time** (during `go test` startup): Describe blocks execute to register specs
- **Execution time** (during test run): It blocks execute, By blocks log progress

**FullText generation**:
```
EnableVolumeReplication → L1-E-001: Enable snapshot mode → should create...
↑                       ↑                                 ↑
Outer Describe          Inner Describe                    It block
```

## 4. Visual Flow Diagrams

### 4.1 Overall Suite Execution Timeline

```
go test ./test/e2e/replication/...
        │
        ▼
┌─────────────────────────────────────┐
│  Test Function Execution Begins     │  ← TestReplicationE2E(t *testing.T)
│  RegisterFailHandler(Fail)          │  ← Gomega assertion handler
│  RunSpecs(t, "Replication E2E...")  │  ← Ginkgo discovers and runs specs
└─────────────────────────────────────┘
        │
        ▼
┌─────────────────────────────────────┐
│   INIT PHASE (Registration)         │
│   ── var _ = BeforeSuite(...)   [1] │  ← Function REGISTERED at init time
│   ── var _ = ReportAfterEach(...)[2]│  ← Function REGISTERED at init time
│   ── var _ = ReportAfterSuite....[3]│  ← Function REGISTERED at init time
│   ── var _ = Describe(...) {...}[4] │  ← Describe blocks REGISTERED at init time
│       ── It("scenario", ...) {...}  │  ← Individual specs REGISTERED at init time
└─────────────────────────────────────┘
        │
        ▼
┌─────────────────────────────────────┐
│   EXECUTION PHASE (Running)         │
│                                     │
│   ┌──────────────────────────────┐  │
│   │ [1] BeforeSuite RUNS ONCE    │  │  ← Cluster setup, k8s client init
│   │     (all fixtures shared)    │  │
│   └──────────────────────────────┘  │
│         │                           │
│         ▼ (for each Describe block) │
│   ╔══════════════════════════════╗  │
│   ║  Describe: EnableVolumeRep...║  │  ← "L1-E-001 through L1-E-009"
│   ╚══════════════════════════════╝  │
│         │                           │
│         ├─ It("Snapshot mode")      │
│         │  │                        │
│         │  ├─ By("Create PVC")      │  ← Logging checkpoint
│         │  ├─ By("Create VR")       │  ← Logging checkpoint
│         │  ├─ By("Verify state")    │  ← Logging checkpoint
│         │  └─ Expect(...OK)         │  ← Assertion
│         │                           │
│         │  [2] ReportAfterEach      │  ← Logs spec completion (AFTER each It)
│         │  ✓ Passed (~10s)          │
│         │                           │
│         ├─ It("Journal mode")       │
│         │  ...                      │
│         │  [2] ReportAfterEach      │
│         │  ✓ Passed (~10s)          │
│         │                           │
│         └─ It("Peer unreachable")   │
│            ...                      │
│            [2] ReportAfterEach      │
│            ✓ Passed (~8s)           │
│                                     │
│   (Repeat for each Describe block)  │
│                                     │
│   ┌──────────────────────────────┐  │
│   │ [3] ReportAfterSuite RUNS    │  │  ← Final summary (no teardown)
│   │     (aggregated report)      │  │
│   └──────────────────────────────┘  │
└─────────────────────────────────────┘
        │
        ▼
Exit test with results
```

### 4.2 Detailed Spec Execution (Single Test Case)

```
It("Enable snapshot mode replication", func() {
    // Test body executes here
})
│
├──────────────────────────────────────┐
│  SPEC SETUP                          │
│  ┌────────────────────────────────┐  │
│  │ Setup resources:               │  │
│  │ - Create PVC (kubectl apply)   │  │
│  │ - Create VolumeReplication CR  │  │
│  │ - Wait for reconciliation      │  │
│  └────────────────────────────────┘  │
│           │                          │
│           ▼                          │
│  ┌────────────────────────────────┐  │
│  │ SPEC EXECUTION                 │  │
│  │ ─────────────────────────────  │  │
│  │ By("Create PVC")               │  │ ← Progress logging
│  │   k8sClient.Create(ctx, pvc)   │  │
│  │   Expect(err).NotTo(HaveOcc()) │  │
│  │                                │  │
│  │ By("Enable replication")       │  │ ← Progress logging
│  │   k8sClient.Create(ctx, vr)    │  │
│  │   Expect(err).NotTo(HaveOcc()) │  │
│  │                                │  │
│  │ By("Wait for Replicating")     │  │ ← Progress logging
│  │   Eventually(GetVR, 2m).       │  │
│  │   Should(haveState("Replicat."))
│  │                                │  │
│  │ By("Verify conditions")        │  │ ← Progress logging
│  │   Expect(vr.Status.Conditions)│  │
│  │   Expect(len > 0)              │  │
│  └────────────────────────────────┘  │
│           │                          │
│           ▼                          │
│  ┌────────────────────────────────┐  │
│  │ DEFERRED CLEANUP (DeferCleanup)│  │
│  │ ─────────────────────────────  │  │
│  │ Stack (LIFO order):            │  │
│  │   [3] Delete Pod (if created)  │  │
│  │   [2] Delete VR CR             │  │
│  │   [1] Delete PVC               │  │
│  └────────────────────────────────┘  │
└──────────────────────────────────────┘
        │
        ▼
┌─────────────────────────────────────┐
│  SPEC RESULT                        │
│  ✓ PASSED (10.234s)                 │
│  → ReportAfterEach hook runs        │
│  → [SPEC] It(...) -> Passed (10.2s) │
└─────────────────────────────────────┘
```

### 4.3 DeferCleanup Execution (LIFO Order)

```
During test execution, cleanup functions are registered with DeferCleanup():

It("scenario", func() {
    pvc := CreatePVC(...)
    DeferCleanup(func() {
        DeletePVC(pvc)  ← Registered [3]
    })
    │
    vr := CreateVR(...)
    DeferCleanup(func() {
        DeleteVR(vr)    ← Registered [2]
    })
    │
    pod := CreatePod(...)
    DeferCleanup(func() {
        DeletePod(pod)  ← Registered [1]
    })
    │
    ▼ (test runs to completion or failure)
})
│
▼ TEST ENDS (pass or fail)

CLEANUP EXECUTES IN LIFO ORDER:
┌──────────────────────────────┐
│  [1] Delete Pod              │
│      └─ Even if error, next  │
│        runs (continues)      │
└──────────────────────────────┘
        │
        ▼
┌──────────────────────────────┐
│  [2] Delete VR CR            │
│      └─ Runs regardless of   │
│        Pod deletion result   │
└──────────────────────────────┘
        │
        ▼
┌──────────────────────────────┐
│  [3] Delete PVC              │
│      └─ Runs last (always)   │
└──────────────────────────────┘
        │
        ▼
CLEANUP COMPLETE
(Test marked PASSED or FAILED
 based on assertions, not cleanup)
```

### 4.4 Describe Block Hierarchy

```
Root Level
    │
    Describe("Replication E2E", func() {
        │
        Describe("EnableVolumeReplication API", func() {
            │
            It("L1-E-001: Enable snapshot mode", func() {...})
            It("L1-E-002: Enable journal mode", func() {...})
            It("L1-E-003: Enable with NetworkFence", func() {...})
            It("L1-E-004: Invalid interval", func() {...})
            It("L1-E-005: Idempotent enable", func() {...})
            │
            Describe("Error Scenarios", func() {
                │
                It("L1-E-006: Invalid secret", func() {...})
                It("L1-E-007: Invalid mirror mode", func() {...})
                It("L1-E-009: Invalid time format", func() {...})
            })
        })
        │
        Describe("DisableVolumeReplication API", func() {
            │
            It("L1-DIS-001: Disable primary", func() {...})
            It("L1-DIS-002: Disable secondary", func() {...})
            ...
        })
        │
        Describe("PromoteVolume API", func() {...})
        │
        Describe("DemoteVolume API", func() {...})
        │
        Describe("ResyncVolume API", func() {...})
        │
        Describe("GetVolumeReplicationInfo API", func() {...})
    })
```

### 4.5 Hook Execution Order (Edge Cases)

**Case 1: Test PASSES**
```
┌─────────────────────────────────────┐
│ [1] Spec runs (all By blocks)       │
│ [2] DeferCleanup handlers (LIFO)    │
│ [3] ReportAfterEach hook            │
│ [4] Result: ✓ PASSED                │
└─────────────────────────────────────┘
```

**Case 2: Test FAILS (Expect assertion)**
```
┌─────────────────────────────────────┐
│ [1] Spec runs (until Expect fails)  │
│ [2] DeferCleanup handlers (LIFO)    │
│     ↳ Always runs, even on failure  │
│ [3] ReportAfterEach hook            │
│ [4] Result: ✗ FAILED                │
│     Error logged and continues      │
└─────────────────────────────────────┘
```

**Case 3: Test PANICS**
```
┌─────────────────────────────────────┐
│ [1] Spec runs (until panic)         │
│ [2] Panic caught by Ginkgo          │
│ [3] DeferCleanup handlers (LIFO)    │
│     ↳ Always runs, even on panic    │
│ [4] ReportAfterEach hook            │
│ [5] Result: ✗ PANICKED              │
│     Stack trace logged and continues│
└─────────────────────────────────────┘
```

### 4.2 It Blocks (Individual Test Cases)

Each `It()` block is one **spec** (test case):

```go
It("L1-E-001: should enable snapshot mode replication", func() {
	// This entire function is one spec
	// Spec name: "L1-E-001: should enable snapshot mode replication"
	// Spec FullText (with parent): "EnableVolumeReplication L1-E-001 should enable snapshot mode..."
})
```

**One It() = One Spec = One Pass/Fail Result**

Multiple It blocks in same Describe create multiple independent specs:

```go
Describe("EnableVolumeReplication", func() {
	It("L1-E-001", func() { /* spec 1 */ })
	It("L1-E-002", func() { /* spec 2 */ })
	It("L1-E-003", func() { /* spec 3 */ })
})
// Result: 3 specs, each runs independently
```

### 4.3 By Blocks (Progress Logging)

`By()` statements are **for readability and progress tracking only** - they don't create separate test cases:

```go
It("L1-E-001: Enable snapshot mode", func() {
	By("L1-E-001: Create namespace")
	ns := CreateNamespace(ctx, k8sClient, "test-ns")

	By("L1-E-001: Creating PVC")
	pvc := CreatePVC(ctx, k8sClient, "test-ns", "pvc-name", storageClass, "1Gi", nil)

	By("L1-E-001: Creating VolumeReplicationClass")
	vrc := CreateVolumeReplicationClass(ctx, k8sClient, "vrc-name", provisioner, secretName, secretNs, MirroringModeSnapshot)

	By("L1-E-001: Creating VolumeReplication")
	vr := CreateVolumeReplication(ctx, k8sClient, "test-ns", "vr-name", vrc.Name, pvc.Name, Primary)

	By("L1-E-001: Waiting for Replicating=True")
	WaitForVolumeReplicationReplicatingOrCompleted(ctx, k8sClient, vr, func(v *replicationv1alpha1.VolumeReplication) {
		fmt.Fprintf(GinkgoWriter, "  [VR] %s\n", FormatVRStatus(v))
	})

	By("L1-E-001: Assertion - VR state is Active")
	Expect(vr.Status.State).To(Equal(replicationv1alpha1.Active))
})
// Result: 1 spec with 7 By blocks logged as progress
```

**Output**:
```
2026-03-10 10:15:22.123 [SPEC] By: L1-E-001: Create namespace
2026-03-10 10:15:23.456 [SPEC] By: L1-E-001: Creating PVC
2026-03-10 10:15:24.789 [SPEC] By: L1-E-001: Creating VolumeReplicationClass
...
```

### 4.4 Expect Assertions

Assertions within It blocks perform validation. **First failed Expect stops the spec**:

```go
It("should validate volume replication status", func() {
	vr := GetVolumeReplication(ctx, k8sClient, "ns", "vr-name")

	By("Checking Replicating condition")
	Expect(vr.Status.Conditions).To(ContainElement(
		MatchFields(IgnoreExtras, Fields{
			"Type":   Equal("Replicating"),
			"Status": Equal(corev1.ConditionTrue),
		}),
	))
	// If above Expect fails, spec fails here. Following code doesn't run.

	By("Checking not degraded")
	Expect(vr.Status.Conditions).To(ContainElement(
		MatchFields(IgnoreExtras, Fields{
			"Type":   Equal("Degraded"),
			"Status": Equal(corev1.ConditionFalse),
		}),
	))
	// Only runs if first Expect passed
})
```

**Matchers used in tests**:
- `Equal(value)` - exact equality
- `ContainElement(matcher)` - array/slice contains matching element
- `MatchFields(ignoreExtras, fields)` - struct field matching
- `HaveOccurred()` - error is not nil
- `NotTo(HaveOccurred())` - error is nil
- `Eventually(func, timeout, interval)` - polling for condition

## 3. Cleanup and Deferred Execution

### 5.1 DeferCleanup Pattern

`DeferCleanup()` registers cleanup functions that run **when the current It block exits** (success or failure):

```go
It("should create and cleanup volume replication", func() {
	ctx := context.Background()

	// Create resources
	ns := CreateNamespace(ctx, k8sClient, "test-ns")
	pvc := CreatePVC(ctx, k8sClient, "test-ns", "pvc", storageClass, "1Gi", nil)
	vrc := CreateVolumeReplicationClass(ctx, k8sClient, "vrc", provisioner, secret, secretNs, MirroringModeSnapshot)
	vr := CreateVolumeReplication(ctx, k8sClient, "test-ns", "vr", vrc.Name, pvc.Name, Primary)

	// Register cleanup (executes AFTER spec ends)
	DeferCleanup(func() {
		cleanupCtx := context.Background()
		Logf("[CLEANUP]", "deleting VolumeReplication")
		DeleteVolumeReplicationWithCleanup(cleanupCtx, k8sClient, vr)
		
		Logf("[CLEANUP]", "deleting VolumeReplicationClass")
		DeleteVolumeReplicationClassWithCleanup(cleanupCtx, k8sClient, vrc)
		
		Logf("[CLEANUP]", "deleting PVC")
		DeletePVCWithCleanup(cleanupCtx, k8sClient, pvc)
		
		Logf("[CLEANUP]", "deleting namespace")
		DeleteNamespace(cleanupCtx, k8sClient, ns)
	})

	// Test assertions
	Eventually(func() bool {
		vr := &replicationv1alpha1.VolumeReplication{}
		_ = k8sClient.Get(ctx, client.ObjectKey{Namespace: "test-ns", Name: "vr"}, vr)
		return vr.Status.Replicating
	}, 5*time.Minute, 5*time.Second).Should(BeTrue())
})
// Execution order:
// 1. Create namespace
// 2. Create PVC
// 3. Create VolumeReplicationClass
// 4. Create VolumeReplication
// 5. Register cleanup (deferred)
// 6. Run Eventually assertions
// 7. [Spec ends] → Run DeferCleanup (delete VR, VRC, PVC, namespace)
```

**Key behaviors**:
- **Multiple DeferCleanup calls**: Execute in LIFO order (last registered = first executed)
- **On failure**: Cleanup still runs even if spec fails
- **On panic**: Cleanup still runs even if spec panics
- **Nested contexts**: Each context.Background() is independent

**Cleanup function structure**:
```go
DeferCleanup(func() {
	// Create fresh context for cleanup
	cleanupCtx := context.Background()
	
	// Use helper with logging
	DeleteVolumeReplicationWithCleanup(cleanupCtx, k8sClient, vr)
	
	// Continues even if deletion takes time or has errors
})
```

### 5.2 Cleanup Helper Pattern

The test suite provides cleanup helpers that handle timeout and logging:

```go
func DeleteVolumeReplicationWithCleanup(ctx context.Context, k8sClient client.Client, vr *replicationv1alpha1.VolumeReplication) {
	Logf("[CLEANUP]", "deleting VolumeReplication %s/%s", vr.Namespace, vr.Name)
	
	// Remove finalizers if present
	if controllerutil.ContainsFinalizer(vr, "replication.storage.openshift.io/finalizer") {
		controllerutil.RemoveFinalizer(vr, "replication.storage.openshift.io/finalizer")
		_ = k8sClient.Update(ctx, vr)
	}
	
	// Delete with timeout
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	
	err := k8sClient.Delete(ctx, vr)
	if err != nil && !errors.IsNotFound(err) {
		Logf("[CLEANUP]", "error deleting VR: %v (continuing)", err)
	}
	
	// Wait for deletion with eventual consistent check
	Eventually(func() error {
		return k8sClient.Get(ctx, client.ObjectKey{Namespace: vr.Namespace, Name: vr.Name}, vr)
	}, 10*time.Second, 500*time.Millisecond).Should(MatchError(errors.IsNotFound))
	
	Logf("[CLEANUP]", "VolumeReplication deleted successfully")
}
```

## 6. How Specs Are Detected and Executed

### 6.1 Registration Phase (Init Time)

```
go test ./test/e2e/replication
        ↓
Go runtime loads package
        ↓
init() functions run
        ↓
var _ = BeforeSuite(...) → registers BeforeSuite hook
var _ = ReportAfterEach(...) → registers ReportAfterEach hook
var _ = Describe(...) → registers top-level Describe
        ├─ Describe("...") → registers nested Describe
        │   ├─ It("...") → registers spec 1
        │   ├─ It("...") → registers spec 2
        ├─ Describe("...") → registers nested Describe
        │   ├─ It("...") → registers spec 3
var _ = ReportAfterSuite(...) → registers ReportAfterSuite hook
        ↓
go func TestReplicationE2E(t *testing.T) called
        ↓
RunSpecs() is called → execution phase begins
```

### 6.2 Execution Phase (During Test Run)

```
RunSpecs() begins
        ↓
BeforeSuite() runs once
        ↓
For each registered spec:
        ├─ Run BeforeEach (if defined)
        ├─ Run It() function
        ├─ Run ReportAfterEach
        ├─ Run AfterEach (if defined)
        └─ Run DeferCleanup handlers (in LIFO order)
        ↓
All specs complete
        ↓
AfterSuite() runs once (if defined)
        ↓
ReportAfterSuite() runs once
        ↓
RunSpecs() returns
        ↓
Test completes (Pass/Fail)
```

### 6.3 Spec Filtering and Selection

**Ginkgo allows filtering specs via:**

1. **Command-line flag**: `--focus="<pattern>"`
2. **Environment variable**: `GINKGO_FOCUS="<pattern>"`
3. **FocusedDescribe/FDescribe**: Prefix with `F`

```go
// Execute only specs matching "EnableVolumeReplication"
make test-replication-e2e GINKGO_FOCUS="EnableVolumeReplication"

// Execute only specs matching "L1-E-001"
GINKGO_FOCUS="L1-E-001" ./hack/run-replication-e2e.sh

// In code, focus specific describe
var _ = FDescribe("EnableVolumeReplication", func() {
	// Only this Describe's specs run; others are skipped
})
```

**Pattern matching**:
- Regex support: `GINKGO_FOCUS="L1-E-00[123]"` matches L1-E-001, L1-E-002, L1-E-003
- Case sensitive: `GINKGO_FOCUS="enable"` does NOT match "EnableVolumeReplication"
- Partial match: `GINKGO_FOCUS="E-001"` matches "L1-E-001" anywhere in spec name

## 7. Multiple Test Cases Within a Single Spec

A single `It()` block can contain multiple test cases via loops:

```go
It("should validate multiple replication scenarios", func() {
	testCases := []struct {
		name           string
		mirroringMode  string
		schedulingMode string
		expectError    bool
	}{
		{
			name:           "snapshot mode with 1m interval",
			mirroringMode:  "snapshot",
			schedulingMode: "1m",
			expectError:    false,
		},
		{
			name:           "journal mode with 5m interval",
			mirroringMode:  "journal",
			schedulingMode: "5m",
			expectError:    false,
		},
		{
			name:           "invalid mirroring mode",
			mirroringMode:  "invalid",
			schedulingMode: "1m",
			expectError:    true,
		},
	}

	for _, tc := range testCases {
		By(fmt.Sprintf("Testing: %s", tc.name))
		
		// Create VolumeReplication with test case parameters
		vrc := CreateVolumeReplicationClass(ctx, k8sClient, "vrc", provisioner, secret, secretNs, tc.mirroringMode)
		
		// Register cleanup before assertions (so it runs even if assertion fails)
		DeferCleanup(func() {
			DeleteVolumeReplicationClassWithCleanup(context.Background(), k8sClient, vrc)
		})
		
		// Validate
		if tc.expectError {
			Expect(vrc.Spec.MirroringMode).NotTo(Equal("invalid"))
		} else {
			Expect(vrc.Spec.MirroringMode).To(Equal(tc.mirroringMode))
		}
	}
})
// Result: 1 spec with 3 test case iterations
```

**Output**:
```
2026-03-10 10:15:22.123 [SPEC] By: Testing: snapshot mode with 1m interval
2026-03-10 10:15:23.456 [SPEC] By: Testing: journal mode with 5m interval
2026-03-10 10:15:24.789 [SPEC] By: Testing: invalid mirroring mode
2026-03-10 10:15:25.000 [SPEC] EnableVolumeReplication should validate multiple scenarios -> Passed (3s)
```

**Alternatively, create separate specs per test case** (recommended for independent failure reporting):

```go
Describe("EnableVolumeReplication", func() {
	for _, tc := range testCases {
		tc := tc  // Capture loop variable
		It(fmt.Sprintf("should handle %s", tc.name), func() {
			vrc := CreateVolumeReplicationClass(ctx, k8sClient, "vrc", provisioner, secret, secretNs, tc.mirroringMode)
			DeferCleanup(func() {
				DeleteVolumeReplicationClassWithCleanup(context.Background(), k8sClient, vrc)
			})
			
			if tc.expectError {
				Expect(vrc.Spec.MirroringMode).NotTo(Equal("invalid"))
			} else {
				Expect(vrc.Spec.MirroringMode).To(Equal(tc.mirroringMode))
			}
		})
	}
})
// Result: 3 separate specs, each passes/fails independently
```

## 8. Logging Pattern

The test suite provides a centralized `Logf()` wrapper for consistent logging:

```go
// Logf centralizes timestamp generation and formatting
func Logf(prefix, format string, args ...interface{}) {
	timestamp := time.Now().Format("2006-01-02 15:04:05.000")
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(GinkgoWriter, "%s %s %s\n", timestamp, prefix, msg)
}

// Usage in tests:
Logf("[SETUP]", "creating Kubernetes client")
Logf("[PVC]", "ns=%s name=%s phase=%s", pvc.Namespace, pvc.Name, pvc.Status.Phase)
Logf("[VR]", "replicating=%v degraded=%v", vr.Status.Replicating, vr.Status.Degraded)
Logf("[CLEANUP]", "deleting PVC %s/%s", pvc.Namespace, pvc.Name)
```

**Output format**:
```
2026-03-10 10:15:22.123 [SETUP] creating Kubernetes client
2026-03-10 10:15:23.456 [PVC] ns=test-ns name=pvc-1 phase=Bound
2026-03-10 10:15:24.789 [VR] replicating=true degraded=false
2026-03-10 10:15:25.000 [CLEANUP] deleting PVC test-ns/pvc-1
```

## 9. Summary Table: Ginkgo Execution Flow

| Phase | When | What | Registrations | Timing |
|-------|------|------|----------------|--------|
| **Init** | `go test` startup | Package initialization | var _ = BeforeSuite(), Describe(), It(), DeferCleanup() registered | Synchronous, once |
| **BeforeSuite** | Before any specs | Cluster setup, client creation | Runs once for entire suite | If fails, skip all specs |
| **BeforeEach** | Before each spec | Per-test setup | Runs for each It block | If fails, skip that spec |
| **It (Body)** | During test execution | Test assertions | Test logic | Runs once per spec |
| **By** | During It execution | Progress logging | Not execution, just logging | Synchronous output |
| **DeferCleanup** | When spec exits | Cleanup registration (at init time) + cleanup execution (after spec) | Registered during It execution | LIFO order, always runs |
| **ReportAfterEach** | After each spec | Log progress, metrics | Runs for each spec completion | Has access to SpecReport |
| **AfterEach** | After each spec | Per-test cleanup | Optional hook | Runs after spec + report |
| **ReportAfterSuite** | After all specs | Print summary, aggregate metrics | Runs once after all specs | Has access to full Report |
| **AfterSuite** | After all specs | Final cleanup | Optional hook | If fails, marks suite as failed |
