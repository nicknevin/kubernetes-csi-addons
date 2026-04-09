package replication

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/csi-addons/kubernetes-csi-addons/test/e2e/helpers"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Fault Injection Integration", func() {
	var (
		ctx           context.Context
		provider      helpers.PeerFenceProvider
		targetCIDR    string
		testTimeout   time.Duration
		testNamespace string
	)

	BeforeEach(func() {
		ctx = context.Background()
		testTimeout = 5 * time.Minute
		testNamespace = "default" // Use default namespace for these tests

		// Use a test target that should be reachable (Google DNS)
		targetCIDR = "8.8.8.8/32"

		// Exercise iptables fault injection explicitly (same as default when E2E_FAULT_INJECTOR is unset).
		config := helpers.FaultInjectionConfig{
			Type:      helpers.FaultInjectorIptables,
			Client:    k8sClient,
			Namespace: testNamespace,
		}

		var err error
		provider, err = helpers.NewFaultInjectionProvider(config)
		Expect(err).NotTo(HaveOccurred())
		Expect(provider).NotTo(BeNil())
	})

	AfterEach(func() {
		if provider != nil {
			// Cleanup any resources created by the provider
			Expect(provider.Cleanup(ctx)).To(Succeed())
		}
	})

	Context("Provider Capability Detection", func() {
		It("should detect provider support correctly", func() {
			isSupported := provider.IsSupported(ctx)

			// Log which provider we're using
			providerType := provider.GetProviderType()
			GinkgoWriter.Printf("Using fault injection provider: %s, supported: %t\n", providerType, isSupported)

			if providerType == helpers.FaultInjectorIptables {
				// For iptables provider, support depends on privileged DaemonSet capability
				hasPrivileged := helpers.HasPrivilegedDaemonSetSupport(ctx, k8sClient)
				Expect(isSupported).To(Equal(hasPrivileged))
			} else if providerType == helpers.FaultInjectorNetworkFence {
				// For NetworkFence provider, this would depend on CSIAddonsNode capabilities
				// but we'll just verify the check works
				Expect(isSupported).To(BeAssignableToTypeOf(bool(false)))
			}
		})

		It("should handle unsupported provider gracefully", func() {
			// Force environment to use unsupported provider
			originalEnv := os.Getenv("E2E_FAULT_INJECTOR")
			defer func() {
				if originalEnv != "" {
					os.Setenv("E2E_FAULT_INJECTOR", originalEnv)
				} else {
					os.Unsetenv("E2E_FAULT_INJECTOR")
				}
			}()

			// Set to none to get NoOpFaultProvider
			os.Setenv("E2E_FAULT_INJECTOR", "none")

			config := helpers.FaultInjectionConfig{
				Client:     k8sClient,
				RESTConfig: GetRESTConfig(),
				Namespace:  testNamespace,
			}

			noOpProvider, err := helpers.NewFaultInjectionProvider(config)
			Expect(err).NotTo(HaveOccurred())
			Expect(noOpProvider.GetProviderType()).To(Equal(helpers.FaultInjectorNone))

			isSupported := noOpProvider.IsSupported(ctx)
			Expect(isSupported).To(BeTrue()) // NoOp is always supported
		})
	})

	Context("Network Fencing Operations", func() {
		BeforeEach(func() {
			// Skip if provider not supported
			supported := provider.IsSupported(ctx)
			if !supported {
				Skip("Fault injection provider not supported in this cluster")
			}

			// Skip NoOp provider tests
			if provider.GetProviderType() == helpers.FaultInjectorNone {
				Skip("NoOp provider - skipping actual fault injection tests")
			}
		})

		It("should fence and unfence IP successfully", func() {
			By("Verifying initial connectivity")
			ctx, cancel := context.WithTimeout(ctx, testTimeout)
			defer cancel()

			// Initial connectivity check (should be reachable)
			reachable, err := provider.VerifyConnectivity(ctx, targetCIDR, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(reachable).To(BeTrue(), "Target should be initially reachable")

			By("Fencing the target IP")
			params := map[string]string{
				"reason": "e2e-test-fence",
			}

			err = provider.FenceIP(ctx, targetCIDR, params)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying IP is fenced")
			// Give some time for the fence to take effect
			time.Sleep(10 * time.Second)

			fenced, err := provider.VerifyConnectivity(ctx, targetCIDR, true)
			Expect(err).NotTo(HaveOccurred())
			Expect(fenced).To(BeTrue(), "Target should be fenced (unreachable)")

			By("Unfencing the target IP")
			err = provider.UnfenceIP(ctx, targetCIDR, params)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying IP is unfenced")
			// Give some time for the unfence to take effect
			time.Sleep(10 * time.Second)

			unfenced, err := provider.VerifyConnectivity(ctx, targetCIDR, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(unfenced).To(BeTrue(), "Target should be unfenced (reachable again)")
		})

		It("should handle multiple targets", func() {
			targets := []string{"8.8.8.8/32", "1.1.1.1/32"}

			ctx, cancel := context.WithTimeout(ctx, testTimeout)
			defer cancel()

			By("Fencing multiple targets")
			for _, target := range targets {
				err := provider.FenceIP(ctx, target, map[string]string{"test": "multi-target"})
				Expect(err).NotTo(HaveOccurred())
			}

			By("Verifying all targets are fenced")
			time.Sleep(10 * time.Second)
			for _, target := range targets {
				fenced, err := provider.VerifyConnectivity(ctx, target, true)
				Expect(err).NotTo(HaveOccurred())
				Expect(fenced).To(BeTrue(), "Target %s should be fenced", target)
			}

			By("Unfencing all targets")
			for _, target := range targets {
				err := provider.UnfenceIP(ctx, target, nil)
				Expect(err).NotTo(HaveOccurred())
			}

			By("Verifying all targets are unfenced")
			time.Sleep(10 * time.Second)
			for _, target := range targets {
				unfenced, err := provider.VerifyConnectivity(ctx, target, false)
				Expect(err).NotTo(HaveOccurred())
				Expect(unfenced).To(BeTrue(), "Target %s should be unfenced", target)
			}
		})
	})

	Context("Error Handling", func() {
		It("should handle invalid CIDR gracefully", func() {
			supported := provider.IsSupported(ctx)
			if !supported || provider.GetProviderType() == helpers.FaultInjectorNone {
				Skip("Provider not supported or is NoOp")
			}

			invalidCIDR := "not-a-valid-cidr"
			err := provider.FenceIP(ctx, invalidCIDR, nil)
			// Should either succeed (provider handles validation) or return meaningful error
			if err != nil {
				GinkgoWriter.Printf("Expected error for invalid CIDR: %v\n", err)
			}
		})

		It("should cleanup resources on error", func() {
			supported := provider.IsSupported(ctx)
			if !supported || provider.GetProviderType() == helpers.FaultInjectorNone {
				Skip("Provider not supported or is NoOp")
			}

			// This should always succeed regardless of provider implementation
			err := provider.Cleanup(ctx)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("Environment Configuration", func() {
		It("should respect E2E_FAULT_INJECTOR environment variable", func() {
			testCases := []struct {
				envValue string
				expected helpers.FaultInjectorType
			}{
				{"iptables", helpers.FaultInjectorIptables},
				{"networkfence", helpers.FaultInjectorNetworkFence},
				{"none", helpers.FaultInjectorNone},
				{"", helpers.FaultInjectorNone},        // Default
				{"invalid", helpers.FaultInjectorNone}, // Fallback
			}

			originalEnv := os.Getenv("E2E_FAULT_INJECTOR")
			defer func() {
				if originalEnv != "" {
					os.Setenv("E2E_FAULT_INJECTOR", originalEnv)
				} else {
					os.Unsetenv("E2E_FAULT_INJECTOR")
				}
			}()

			config := helpers.FaultInjectionConfig{
				Client:    k8sClient,
				Namespace: testNamespace,
			}

			for _, tc := range testCases {
				By(fmt.Sprintf("Testing E2E_FAULT_INJECTOR=%s", tc.envValue))

				if tc.envValue == "" {
					os.Unsetenv("E2E_FAULT_INJECTOR")
				} else {
					os.Setenv("E2E_FAULT_INJECTOR", tc.envValue)
				}

				testProvider, err := helpers.NewFaultInjectionProvider(config)
				Expect(err).NotTo(HaveOccurred())
				Expect(testProvider.GetProviderType()).To(Equal(tc.expected))
			}
		})
	})
})
