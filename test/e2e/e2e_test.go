/*
Copyright 2024 david amick.

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

package e2e

import (
	"fmt"
	"os/exec"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/snarlysodboxer/undying-proxy/test/utils"
)

const (
	undyingProxyNamespace     = "undying-proxy"
	connectionTesterNamespace = "connection-tester"
	projectImage              = "my-repo/undying-proxy:latest"
	connectionTesterImage     = "my-repo/connection-tester:latest"
)

var _ = Describe("controller", Ordered, func() {
	BeforeAll(func() {
		By("ensuring the right cluster to test against")
		err := utils.EnsureContext()
		ExpectWithOffset(1, err).NotTo(HaveOccurred())

		By("creating manager namespace")
		cmd := exec.Command("kubectl", "create", "ns", undyingProxyNamespace)
		_, _ = utils.Run(cmd)

		By("building the manager(Operator) image")
		cmd = exec.Command("make", "docker-build", fmt.Sprintf("IMG=%s", projectImage))
		_, err = utils.Run(cmd)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())

		By("loading the manager(Operator) image on Kind")
		err = utils.LoadImageToKindClusterWithName(projectImage)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())

		By("building the connection-tester image")
		cmd = exec.Command("docker", "build", "-t", connectionTesterImage, "test/e2e/connection-tester")
		_, err = utils.Run(cmd)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())

		By("loading the connection-tester image on Kind")
		err = utils.LoadImageToKindClusterWithName(connectionTesterImage)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())

		By("installing CRDs")
		cmd = exec.Command("make", "install")
		_, err = utils.Run(cmd)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
	})

	AfterAll(func() {
		By("removing UnDyingProxy objects")
		cmd := exec.Command(
			"kubectl",
			"-n", undyingProxyNamespace,
			"delete", "undyingproxies", "--all",
		)
		_, _ = utils.Run(cmd)
		By("removing manager namespace")
		cmd = exec.Command("kubectl", "delete", "ns", undyingProxyNamespace)
		_, _ = utils.Run(cmd)
	})

	Context("Operator", func() {
		AfterEach(func() {
			By("removing connection-tester namespace")
			cmd := exec.Command(
				"kubectl",
				"delete", "ns", connectionTesterNamespace,
				"--ignore-not-found",
			)
			_, _ = utils.Run(cmd)

			By("removing connection-tester UnDyingProxy object")
			cmd = exec.Command(
				"kubectl", "delete",
				"-f", "test/e2e/connection-tester/unDyingProxy.connection-tester.yaml",
				"--ignore-not-found",
			)
			_, _ = utils.Run(cmd)

			By("removing connection-tester client pod")
			cmd = exec.Command(
				"kubectl",
				"-n", "default",
				"delete", "pod", "connection-tester",
				"--ignore-not-found",
			)
			_, _ = utils.Run(cmd)
		})

		It("should run successfully", func() {
			By("deploying the controller-manager")
			cmd := exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", projectImage))
			_, err := utils.Run(cmd)
			ExpectWithOffset(1, err).NotTo(HaveOccurred())

			By("validating that the controller-manager pod is running as expected")
			EventuallyWithOffset(1, func() error {
				return verifyPodReady("undying-proxy", "undying-proxy")
			}, time.Minute, time.Second).Should(Succeed())
		})

		It("should create, delete, and recreate, and still work", func() {
			By("deploying the controller-manager")
			cmd := exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", projectImage))
			_, err := utils.Run(cmd)
			ExpectWithOffset(1, err).NotTo(HaveOccurred())

			By("validating that the controller-manager pod is running as expected")
			EventuallyWithOffset(1, func() error {
				return verifyPodReady("undying-proxy", "undying-proxy")
			}, time.Minute, time.Second).Should(Succeed())

			By("deploying connection-tester server")
			cmd = exec.Command(
				"bash", "-c",
				"kustomize build test/e2e/connection-tester/server-configs | kubectl apply -f -",
			)
			_, err = utils.Run(cmd)
			ExpectWithOffset(1, err).NotTo(HaveOccurred())

			By("validating that the connection-tester pod is running as expected")
			EventuallyWithOffset(1, func() error {
				return verifyPodReady("connection-tester", "connection-tester")
			}, time.Minute, time.Second).Should(Succeed())

			By("creating UnDyingProxy to test through")
			cmd = exec.Command(
				"kubectl", "apply",
				"-f", "test/e2e/connection-tester/unDyingProxy.connection-tester.yaml",
			)
			_, err = utils.Run(cmd)
			ExpectWithOffset(1, err).NotTo(HaveOccurred())

			By("waiting for UnDyingProxy to be ready")
			EventuallyWithOffset(1, func() error {
				return unDyingProxyIsReady(undyingProxyNamespace, "connection-tester")
			}, 5*time.Second, time.Second).Should(Succeed())

			testUDPCmd := func() *exec.Cmd {
				return exec.Command(
					"kubectl", "run",
					"-n", "default",
					"-i", "--rm", "connection-tester",
					"--image="+connectionTesterImage,
					"--restart=Never",
					"--image-pull-policy=Never",
					"--command", "--", "/connection-tester", "udpClient",
					"--serverAddress", "undying-proxy-udp.undying-proxy.svc.cluster.local:3002",
					"--message", "E2E testing!",
				)
			}

			testTCPCmd := func() *exec.Cmd {
				return exec.Command(
					"kubectl", "run",
					"-n", "default",
					"-i", "--rm", "connection-tester",
					"--image="+connectionTesterImage,
					"--restart=Never",
					"--image-pull-policy=Never",
					"--command", "--", "/connection-tester", "tcpClient",
					"--serverAddress", "undying-proxy-tcp.undying-proxy.svc.cluster.local:5002",
					"--message", "E2E testing!",
				)
			}

			By("sending UDP traffic through UnDyingProxy the first time")
			_, err = utils.Run(testUDPCmd())
			ExpectWithOffset(1, err).NotTo(HaveOccurred())

			By("sending TCP traffic through UnDyingProxy the first time")
			_, err = utils.Run(testTCPCmd())
			ExpectWithOffset(1, err).NotTo(HaveOccurred())

			By("removing connection-tester UnDyingProxy")
			cmd = exec.Command(
				"kubectl", "delete",
				"-f", "test/e2e/connection-tester/unDyingProxy.connection-tester.yaml",
			)
			_, err = utils.Run(cmd)
			ExpectWithOffset(1, err).NotTo(HaveOccurred())

			By("creating UnDyingProxy the second time")
			cmd = exec.Command(
				"kubectl", "apply",
				"-f", "test/e2e/connection-tester/unDyingProxy.connection-tester.yaml",
			)
			_, err = utils.Run(cmd)
			ExpectWithOffset(1, err).NotTo(HaveOccurred())

			By("waiting for UnDyingProxy to be ready")
			EventuallyWithOffset(1, func() error {
				return unDyingProxyIsReady(undyingProxyNamespace, "connection-tester")
			}, 5*time.Second, time.Second).Should(Succeed())

			// wait for the Kubernetes Endpoints to get setup
			time.Sleep(1 * time.Second)

			By("sending UDP traffic through UnDyingProxy the second time")
			_, err = utils.Run(testUDPCmd())
			ExpectWithOffset(1, err).NotTo(HaveOccurred())

			By("sending TCP traffic through UnDyingProxy the second time")
			_, err = utils.Run(testTCPCmd())
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
		})
	})
})

// verifyPodReady checks if the pod is running, ready, and not being deleted.
func verifyPodReady(namespace, app string) error {
	// Get pod name
	cmd := exec.Command(
		"kubectl",
		"-n", namespace,
		"get", "pods",
		"-l", "app="+app,
		"-o", "go-template={{ range .items }}"+
			"{{ if not .metadata.deletionTimestamp }}"+
			"{{ .metadata.name }}"+
			"{{ \"\\n\" }}{{ end }}{{ end }}",
	)
	podOutput, err := utils.Run(cmd)
	ExpectWithOffset(2, err).NotTo(HaveOccurred())
	podNames := utils.GetNonEmptyLines(string(podOutput))
	if len(podNames) != 1 {
		return fmt.Errorf("expect 1 pod running, but got %d", len(podNames))
	}
	podName := podNames[0]
	ExpectWithOffset(2, podName).Should(ContainSubstring(app))

	// Validate pod status using condition
	cmd = exec.Command(
		"kubectl",
		"-n", namespace,
		"get", "pods", podName,
		"-o", "jsonpath={.status.conditions[?(@.type==\"Ready\")].status}",
	)
	status, err := utils.Run(cmd)
	ExpectWithOffset(2, err).NotTo(HaveOccurred())
	if string(status) != "True" {
		return fmt.Errorf("pod in %s status", status)
	}

	return nil
}

func unDyingProxyIsReady(namespace, name string) error {
	cmd := exec.Command(
		"kubectl",
		"-n", namespace,
		"get", "undyingproxies", name,
		"-o", "jsonpath={.status.ready}",
	)
	status, err := utils.Run(cmd)
	ExpectWithOffset(2, err).NotTo(HaveOccurred())
	if string(status) != "true" {
		return fmt.Errorf("UnDyingProxy in '%s' status", status)
	}

	return nil
}
