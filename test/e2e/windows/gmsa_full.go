/*
Copyright 2019 The Kubernetes Authors.

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

// This test ensures that the whole GMSA process works as intended.
// However, it does require a pretty heavy weight set up to run correctly;
// in particular, it does make a number of assumptions about the cluster it
// runs against:
//  * there exists a Windows worker node with the agentpool=windowsgmsa label on it
//  * that node is joined to a working Active Directory domain.
//  * a GMSA account has been created in that AD domain, and then installed on that
//    same worker.
//  * a valid k8s manifest file containing a single CRD definition has been generated using
//    https://github.com/kubernetes-sigs/windows-gmsa/blob/master/scripts/GenerateCredentialSpecResource.ps1
//    with the credential specs of that GMSA account, or type GMSACredentialSpec and named gmsa-e2e;
//    and that manifest file has been written to C:\gmsa\gmsa-cred-spec-gmsa-e2e.yml
//    on that same worker node.
//  * the API has both MutatingAdmissionWebhook and ValidatingAdmissionWebhook
//    admission controllers enabled.
//  * the cluster comprises at least one Linux node that accepts workloads - it
//    can be the master, but any other Linux node is fine too. This is needed for
//    the webhook's pod.
// All these assumptions are fulfilled by an AKS extension when setting up the AKS
// cluster we run daily e2e tests against, but they do make running this test
// outside of that very specific context pretty hard.

package windows

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	imageutils "k8s.io/kubernetes/test/utils/image"

	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
)

const (
	// gmsaFullNodeLabel is the label we expect to find on at least one node
	// that is then expected to fulfill all the expectations explained above.
	gmsaFullNodeLabel = "agentpool=windowsgmsa"

	// gmsaCrdManifestPath is where we expect to find the manifest file for
	// the GMSA cred spec on that node - see explanations above.
	gmsaCrdManifestPath = `C:\gmsa\gmsa-cred-spec-gmsa-e2e.yml`

	// gmsaCustomResourceName is the expected name of the GMSA custom resource
	// defined at gmsaCrdManifestPath
	gmsaCustomResourceName = "gmsa-e2e"

	// gmsaWebhookDeployScriptURL is the URL of the deploy script for the GMSA webook
	// TODO(wk8): we should pin versions.
	gmsaWebhookDeployScriptURL = "https://raw.githubusercontent.com/kubernetes-sigs/windows-gmsa/master/admission-webhook/deploy/deploy-gmsa-webhook.sh"

	// output from the nltest /query command should have this in it
	expectedQueryOutput = "The command completed successfully"

	// The name of the expected domain
	gmsaDomain = "pks-ad.local"

	// The name of the volume mounted on gmsa pod
	sharedVolume = "\\\\" + gmsaDomain + "\\SYSVOL"
)

var _ = SIGDescribe("[Feature:Windows] GMSA Full [Serial] [Slow]", func() {
	var (
		c                     clientset.Interface
		node                  v1.Node
		serviceAccountName    string
		webhookCleanup        func()
		customResourceCleanup func()
		rbacRoleCleanup       func(clientset.Interface)
	)

	f := framework.NewDefaultFramework("gmsa-full-test-windows")
	ginkgo.BeforeEach(func() {
		e2eskipper.SkipUnlessNodeOSDistroIs("windows")
		c = f.ClientSet

		ginkgo.By("finding the worker node that fulfills this test's assumptions")
		nodes := findPreconfiguredGmsaNodes(f.ClientSet)
		if len(nodes) != 1 {
			e2eskipper.Skipf("Expected to find exactly one node with the %q label, found %d", gmsaFullNodeLabel, len(nodes))
		}
		node = nodes[0]

		ginkgo.By("retrieving the contents of the GMSACredentialSpec custom resource manifest from the node")
		crdManifestContents := retrieveCRDManifestFileContents(f, node)

		ginkgo.By("downloading the GMSA webhook deploy script")
		deployScriptPath, err := downloadFile(gmsaWebhookDeployScriptURL)
		defer func() { os.Remove(deployScriptPath) }()
		if err != nil {
			framework.Failf(err.Error())
		}

		ginkgo.By("deploying the GMSA webhook")
		webhookCleanup, err = deployGmsaWebhook(f, deployScriptPath)
		if err != nil {
			framework.Failf(err.Error())
		}

		ginkgo.By("creating the GMSA custom resource")
		customResourceCleanup, err = createGmsaCustomResource(f.Namespace.Name, crdManifestContents)
		if err != nil {
			framework.Failf(err.Error())
		}

		var rbacRoleName string
		ginkgo.By("creating an RBAC role to grant use access to that GMSA resource")
		rbacRoleName, rbacRoleCleanup, err = createRBACRoleForGmsa(f)
		if err != nil {
			framework.Failf(err.Error())
		}

		ginkgo.By("creating a service account")
		serviceAccountName = createServiceAccount(f)

		ginkgo.By("binding the RBAC role to the service account")
		bindRBACRoleToServiceAccount(f, serviceAccountName, rbacRoleName)
	})

	ginkgo.AfterEach(func() {
		webhookCleanup()
		customResourceCleanup()
		rbacRoleCleanup(c)
	})

	ginkgo.Describe("GMSA support", func() {
		ginkgo.It("works end to end", func() {
			defer ginkgo.GinkgoRecover()
			ginkgo.By("creating a pod using the GMSA cred spec")
			podName := createPodWithGmsa(f, serviceAccountName, "", "")

			// nltest /QUERY will only return successfully if there is a GMSA
			// identity configured, _and_ it succeeds in contacting the AD controller
			// and authenticating with it.
			ginkgo.By("checking that nltest /QUERY returns successfully")
			gomega.Eventually(func() bool {
				output, err := runKubectlExecInNamespace(f.Namespace.Name, podName, "nltest", "/QUERY")
				if err != nil {
					framework.Logf("unable to run command in container via exec: %s", err)
					return false
				}

				if !isValidOutput(output) {
					// try repairing the secure channel by running reset command
					// https://kubernetes.io/docs/tasks/configure-pod-container/configure-gmsa/#troubleshooting
					output, err = runKubectlExecInNamespace(f.Namespace.Name, podName, "nltest", fmt.Sprintf("/sc_reset:%s", gmsaDomain))
					if err != nil {
						framework.Logf("unable to run command in container via exec: %s", err)
						return false
					}
					framework.Logf("failed to connect to domain; tried resetting the domain, output:\n%s", string(output))
					return false
				}
				return true
			}, 1*time.Minute, 1*time.Second).Should(gomega.BeTrue())
		})

		ginkgo.It("access shared network resources", func() {
			defer ginkgo.GinkgoRecover()

			ginkgo.By("creating a pod using the GMSA cred spec and NetworkService username")
			podName := createPodWithGmsa(f, serviceAccountName, "NT AUTHORITY\\NetworkService", "")

			ginkgo.By("checking that dir " + sharedVolume + " returns successfully")
			gomega.Eventually(func() bool {
				_, err := runKubectlExecInNamespace(f.Namespace.Name, podName, "cmd", "/S", "/C", "dir", sharedVolume)
				if err != nil {
					framework.Logf("unable to run command in container via exec: %s", err)
					return false
				}
				return true
			}, 1*time.Minute, 1*time.Second).Should(gomega.BeTrue())
		})

		ginkgo.It("access local storage", func() {
			defer ginkgo.GinkgoRecover()

			ginkgo.By("creating a pod with hostPath using the GMSA cred spec and mount volumes")
			volumePath := "/test-pd"
			createVolumePathOnHost(node, volumePath)
			podName := createPodWithGmsa(f, serviceAccountName, "", volumePath)

			ginkgo.By("checking that file can be written into the volumes successfully")
			gomega.Eventually(func() bool {
				output, err := // exec( [String]::IsNullOrWhiteSpace((Get-content .\\write_test.txt)) )
				if err != nil {
					framework.Logf("unable to get file from local storage via exec: %s", err)
					return false
				}
				return !output
			}, 1*time.Minute, 1*time.Second).Should(gomega.BeTrue())

			ginkgo.By("checking that file can be read from the volumes successfully")
			gomega.Eventually(func() bool {
				_, err := // exec( echo \"The local volume is mounted!\" > .\\read_test.txt )
				if err != nil {
					framework.Logf("unable to create file in local storage via exec: %s", err)
					return false
				}

				output, err = runKubectlExecInNamespace(f.Namespace.Name, podName, "-s", volumePath + "\\read_test.txt")
				if err != nil {
					framework.Logf("unable to run command in container via exec: %s", err)
					return false
				}
				return true
			}, 1*time.Minute, 1*time.Second).Should(gomega.BeTrue())
		})
	})
})

func isValidOutput(output string) bool {
	return strings.Contains(output, expectedQueryOutput) &&
		!strings.Contains(output, "ERROR_NO_LOGON_SERVERS") &&
		!strings.Contains(output, "RPC_S_SERVER_UNAVAILABLE") &&
		!strings.Contains(output, "ERROR_NO_TRUST_LSA_SECRET")
}

// findPreconfiguredGmsaNode finds node with the gmsaFullNodeLabel label on it.
func findPreconfiguredGmsaNodes(c clientset.Interface) []v1.Node {
	nodeOpts := metav1.ListOptions{
		LabelSelector: gmsaFullNodeLabel,
	}
	nodes, err := c.CoreV1().Nodes().List(context.TODO(), nodeOpts)
	if err != nil {
		framework.Failf("Unable to list nodes: %v", err)
	}
	return nodes.Items
}

// retrieveCRDManifestFileContents retrieves the contents of the file
// at gmsaCrdManifestPath on node; it does so by scheduling a single pod
// on nodes with the gmsaFullNodeLabel label with that file's directory
// mounted on it, and then exec-ing into that pod to retrieve the file's
// contents.
func retrieveCRDManifestFileContents(f *framework.Framework, node v1.Node) string {
	podName := "retrieve-gmsa-crd-contents"
	// we can't use filepath.Dir here since the test itself runs on a Linux machine
	splitPath := strings.Split(gmsaCrdManifestPath, `\`)
	dirPath := strings.Join(splitPath[:len(splitPath)-1], `\`)
	volumeName := "retrieve-gmsa-crd-contents-volume"

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: f.Namespace.Name,
		},
		Spec: v1.PodSpec{
			NodeSelector: node.Labels,
			Containers: []v1.Container{
				{
					Name:  podName,
					Image: imageutils.GetPauseImageName(),
					VolumeMounts: []v1.VolumeMount{
						{
							Name:      volumeName,
							MountPath: dirPath,
						},
					},
				},
			},
			Volumes: []v1.Volume{
				{
					Name: volumeName,
					VolumeSource: v1.VolumeSource{
						HostPath: &v1.HostPathVolumeSource{
							Path: dirPath,
						},
					},
				},
			},
		},
	}
	f.PodClient().CreateSync(pod)

	output, err := runKubectlExecInNamespace(f.Namespace.Name, podName, "cmd", "/S", "/C", fmt.Sprintf("type %s", gmsaCrdManifestPath))
	if err != nil {
		framework.Failf("failed to retrieve the contents of %q on node %q: %v", gmsaCrdManifestPath, node.Name, err)
	}

	// Windows to linux new lines
	return strings.ReplaceAll(output, "\r\n", "\n")
}

// deployGmsaWebhook deploys the GMSA webhook, and returns a cleanup function
// to be called when done with testing, that removes the temp files it's created
// on disks as well as the API resources it's created.
func deployGmsaWebhook(f *framework.Framework, deployScriptPath string) (func(), error) {
	cleanUpFunc := func() {}

	tempDir, err := os.MkdirTemp("", "")
	if err != nil {
		return cleanUpFunc, fmt.Errorf("unable to create temp dir: %w", err)
	}

	manifestsFile := path.Join(tempDir, "manifests.yml")
	podName := f.Namespace.Name
	name := "gmsa-webhook"
	namespace := podName + "-webhook"
	certsDir := path.Join(tempDir, "certs")

	// regardless of whether the deployment succeeded, let's do a best effort at cleanup
	cleanUpFunc = func() {
		framework.RunKubectl(podName, "delete", "--filename", manifestsFile)
		framework.RunKubectl(podName, "delete", "CustomResourceDefinition", "gmsacredentialspecs.windows.k8s.io")
		framework.RunKubectl(podName, "delete", "CertificateSigningRequest", fmt.Sprintf("%s.%s", name, namespace))
		os.RemoveAll(tempDir)
	}

	cmd := exec.Command("bash", deployScriptPath,
		"--file", manifestsFile,
		"--name", name,
		"--namespace", namespace,
		"--certs-dir", certsDir,
		"--tolerate-master")

	output, err := cmd.CombinedOutput()
	if err == nil {
		framework.Logf("GMSA webhook successfully deployed, output:\n%s", string(output))
	} else {
		err = fmt.Errorf("unable to deploy GMSA webhook, output:\n%s: %w", string(output), err)
	}

	return cleanUpFunc, err
}

// createGmsaCustomResource creates the GMSA API object from the contents
// of the manifest file retrieved from the worker node.
// It returns a function to clean up both the temp file it creates and
// the API object it creates when done with testing.
func createGmsaCustomResource(ns string, crdManifestContents string) (func(), error) {
	cleanUpFunc := func() {}

	tempFile, err := os.CreateTemp("", "")
	if err != nil {
		return cleanUpFunc, fmt.Errorf("unable to create temp file: %w", err)
	}
	defer tempFile.Close()

	tempFilename := tempFile.Name()
	cleanUpFunc = func() {
		framework.RunKubectl(ns, "delete", "--filename", tempFilename)
		os.Remove(tempFilename)
	}

	_, err = tempFile.WriteString(crdManifestContents)
	if err != nil {
		err = fmt.Errorf("unable to write GMSA contents to %q: %w", tempFile.Name(), err)
		return cleanUpFunc, err
	}

	output, err := framework.RunKubectl(ns, "apply", "--filename", tempFile.Name())
	if err != nil {
		err = fmt.Errorf("unable to create custom resource, output:\n%s: %w", output, err)
	}

	return cleanUpFunc, err
}

// createRBACRoleForGmsa creates an RBAC cluster role to grant use
// access to our test credential spec.
// It returns the role's name, as well as a function to delete it when done.
func createRBACRoleForGmsa(f *framework.Framework) (string, func(clientset.Interface), error) {
	roleName := f.Namespace.Name + "-rbac-role"

	role := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name: roleName,
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups:     []string{"windows.k8s.io"},
				Resources:     []string{"gmsacredentialspecs"},
				Verbs:         []string{"use"},
				ResourceNames: []string{gmsaCustomResourceName},
			},
		},
	}

	cleanUpFunc := func(c clientset.Interface) {
		c.RbacV1().ClusterRoles().Delete(context.TODO(), roleName, metav1.DeleteOptions{})
	}

	_, err := f.ClientSet.RbacV1().ClusterRoles().Create(context.TODO(), role, metav1.CreateOptions{})
	if err != nil {
		err = fmt.Errorf("unable to create RBAC cluster role %q: %w", roleName, err)
	}

	return roleName, cleanUpFunc, err
}

// createServiceAccount creates a service account, and returns its name.
func createServiceAccount(f *framework.Framework) string {
	accountName := f.Namespace.Name + "-sa"
	account := &v1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      accountName,
			Namespace: f.Namespace.Name,
		},
	}
	if _, err := f.ClientSet.CoreV1().ServiceAccounts(f.Namespace.Name).Create(context.TODO(), account, metav1.CreateOptions{}); err != nil {
		framework.Failf("unable to create service account %q: %v", accountName, err)
	}
	return accountName
}

// bindRBACRoleToServiceAccount binds the given RBAC cluster role to the given service account.
func bindRBACRoleToServiceAccount(f *framework.Framework, serviceAccountName, rbacRoleName string) {
	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      f.Namespace.Name + "-rbac-binding",
			Namespace: f.Namespace.Name,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      serviceAccountName,
				Namespace: f.Namespace.Name,
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     rbacRoleName,
		},
	}
	f.ClientSet.RbacV1().RoleBindings(f.Namespace.Name).Create(context.TODO(), binding, metav1.CreateOptions{})
}

// createPodWithGmsa creates a pod using the test GMSA cred spec, and returns its name.
func createPodWithGmsa(f *framework.Framework, serviceAccountName, username string, volumePath string) string {
	podName := "pod-with-gmsa"
	credSpecName := gmsaCustomResourceName

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: f.Namespace.Name,
		},
		Spec: v1.PodSpec{
			ServiceAccountName: serviceAccountName,
			Containers: []v1.Container{
				{
					Name:  podName,
					Image: imageutils.GetE2EImage(imageutils.BusyBox),
					Command: []string{
						"powershell.exe",
						"-Command",
						"sleep -Seconds 600",
					},
				},
			},
			SecurityContext: &v1.PodSecurityContext{
				WindowsOptions: &v1.WindowsSecurityContextOptions{
					GMSACredentialSpecName: &credSpecName,
				},
			},
		},
	}

	// set WindowsOptions.RunAsUserName if parameter is passed.
	if username != "" {
		pod.Spec.SecurityContext.WindowsOptions.RunAsUserName = &username
	}

	if volumePath != "" {

		pod.Spec.Containers[0].Command = []string{
			"powershell.exe",
			"-Command",
			"echo \"The local volume is mounted!\" > " + volumePath + "\\write_test.txt; sleep -Seconds 600",
		},

		pod.Spec.Containers[0].VolumeMounts := []v1.VolumeMount{
			{
				Name: "test-volume",
				MountPath: volumePath,
			},
		}

		pod.Spec.Volumes := []v1.Volume {
			{
				Name: "test-volume",
				VolumeSource: v1.VolumeSource {
					HostPath: &v1.HostPathVolumeSource {
						Path: sharedVolume,
					},
				},
			},
		}
	}

	f.PodClient().CreateSync(pod)

	return podName
}

func createVolumePathOnHost(node v1.Node, volumePath string) error {
	// exec("powershell.exe -Command mkdir" + volumePath)
}

func runKubectlExecInNamespace(namespace string, args ...string) (string, error) {
	namespaceOption := fmt.Sprintf("--namespace=%s", namespace)
	return framework.RunKubectl(namespace, append([]string{"exec", namespaceOption}, args...)...)
}

c:\\gmsa