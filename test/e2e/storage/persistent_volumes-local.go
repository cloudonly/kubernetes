/*
Copyright 2017 The Kubernetes Authors.

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

package storage

import (
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"k8s.io/api/core/v1"
	rbacv1beta1 "k8s.io/api/rbac/v1beta1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/uuid"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
)

type localTestConfig struct {
	ns     string
	nodes  *v1.NodeList
	node0  *v1.Node
	client clientset.Interface
	scName string
}

type localTestVolume struct {
	// Node that the volume is on
	node *v1.Node
	// Path to the volume on the host node
	hostDir string
	// Path to the volume in the local util container
	containerDir string
	// PVC for this volume
	pvc *v1.PersistentVolumeClaim
	// PV for this volume
	pv *v1.PersistentVolume
}

const (
	// TODO: This may not be available/writable on all images.
	hostBase      = "/tmp"
	containerBase = "/myvol"
	// 'hostBase + discoveryDir' is the path for volume discovery.
	discoveryDir = "disks"
	// Path to the first volume in the test containers
	// created via createLocalPod or makeLocalPod
	// leveraging pv_util.MakePod
	volumeDir = "/mnt/volume1"
	// testFile created in setupLocalVolume
	testFile = "test-file"
	// testFileContent written into testFile
	testFileContent = "test-file-content"
	testSCPrefix    = "local-volume-test-storageclass"

	// Following are constants used for provisioner e2e tests.
	//
	// testServiceAccount is the service account for bootstrapper
	testServiceAccount = "local-storage-bootstrapper"
	// testRoleBinding is the cluster-admin rolebinding for bootstrapper
	testRoleBinding = "local-storage:bootstrapper"
	// volumeConfigName is the configmap passed to bootstrapper and provisioner
	volumeConfigName = "local-volume-config"
	// bootstrapper and provisioner images used for e2e tests
	bootstrapperImageName = "quay.io/external_storage/local-volume-provisioner-bootstrap:v1.0.0"
	provisionerImageName  = "quay.io/external_storage/local-volume-provisioner:v1.0.0"
	// provisioner daemonSetName name, must match the one defined in bootstrapper
	daemonSetName = "local-volume-provisioner"
	// provisioner node/pv cluster role binding, must match the one defined in bootstrapper
	nodeBindingName = "local-storage:provisioner-node-binding"
	pvBindingName   = "local-storage:provisioner-pv-binding"
	// A sample request size
	testRequestSize = "10Mi"
)

var _ = SIGDescribe("PersistentVolumes-local [Feature:LocalPersistentVolumes] [Serial]", func() {
	f := framework.NewDefaultFramework("persistent-local-volumes-test")

	var (
		config *localTestConfig
		node0  *v1.Node
		scName string
	)

	BeforeEach(func() {
		// Get all the schedulable nodes
		nodes := framework.GetReadySchedulableNodesOrDie(f.ClientSet)
		Expect(len(nodes.Items)).NotTo(BeZero(), "No available nodes for scheduling")
		scName = fmt.Sprintf("%v-%v", testSCPrefix, f.Namespace.Name)
		// Choose the first node
		node0 = &nodes.Items[0]

		config = &localTestConfig{
			ns:     f.Namespace.Name,
			client: f.ClientSet,
			nodes:  nodes,
			node0:  node0,
			scName: scName,
		}
	})

	Context("when one pod requests one prebound PVC", func() {

		var testVol *localTestVolume

		BeforeEach(func() {
			testVol = setupLocalVolumePVCPV(config, node0)
		})

		AfterEach(func() {
			cleanupLocalVolume(config, testVol)
		})

		It("should be able to mount and read from the volume using one-command containers", func() {
			By("Creating a pod to read from the PV")
			//testFileContent was written during setupLocalVolume
			_, readCmd := createWriteAndReadCmds(volumeDir, testFile, "" /*writeTestFileContent*/)
			podSpec := makeLocalPod(config, testVol, readCmd)
			f.TestContainerOutput("pod reads PV", podSpec, 0, []string{testFileContent})
		})

		It("should be able to mount and write to the volume using one-command containers", func() {
			By("Creating a pod to write to the PV")
			writeCmd, readCmd := createWriteAndReadCmds(volumeDir, testFile, testVol.hostDir /*writeTestFileContent*/)
			writeThenReadCmd := fmt.Sprintf("%s;%s", writeCmd, readCmd)
			podSpec := makeLocalPod(config, testVol, writeThenReadCmd)
			f.TestContainerOutput("pod writes to PV", podSpec, 0, []string{testVol.hostDir})
		})

		It("should be able to mount volume and read from pod1", func() {
			By("Creating pod1")
			pod1, pod1Err := createLocalPod(config, testVol)
			Expect(pod1Err).NotTo(HaveOccurred())

			pod1NodeName, pod1NodeNameErr := podNodeName(config, pod1)
			Expect(pod1NodeNameErr).NotTo(HaveOccurred())
			framework.Logf("pod1 %q created on Node %q", pod1.Name, pod1NodeName)
			Expect(pod1NodeName).To(Equal(node0.Name))

			By("Reading in pod1")
			// testFileContent was written during setupLocalVolume
			_, readCmd := createWriteAndReadCmds(volumeDir, testFile, "" /*writeTestFileContent*/)
			readOut := podRWCmdExec(pod1, readCmd)
			Expect(readOut).To(ContainSubstring(testFileContent)) /*aka writeTestFileContents*/

			By("Deleting pod1")
			framework.DeletePodOrFail(config.client, config.ns, pod1.Name)
		})

		It("should be able to mount volume and write from pod1", func() {
			By("Creating pod1")
			pod1, pod1Err := createLocalPod(config, testVol)
			Expect(pod1Err).NotTo(HaveOccurred())

			pod1NodeName, pod1NodeNameErr := podNodeName(config, pod1)
			Expect(pod1NodeNameErr).NotTo(HaveOccurred())
			framework.Logf("pod1 %q created on Node %q", pod1.Name, pod1NodeName)
			Expect(pod1NodeName).To(Equal(node0.Name))

			By("Writing in pod1")
			writeCmd, _ := createWriteAndReadCmds(volumeDir, testFile, testVol.hostDir /*writeTestFileContent*/)
			podRWCmdExec(pod1, writeCmd)

			By("Deleting pod1")
			framework.DeletePodOrFail(config.client, config.ns, pod1.Name)
		})
	})

	Context("when two pods request one prebound PVC one after other", func() {

		var testVol *localTestVolume

		BeforeEach(func() {
			testVol = setupLocalVolumePVCPV(config, node0)
		})

		AfterEach(func() {
			cleanupLocalVolume(config, testVol)
		})

		It("should be able to mount volume, write from pod1, and read from pod2 using one-command containers", func() {
			By("Creating pod1 to write to the PV")
			writeCmd, readCmd := createWriteAndReadCmds(volumeDir, testFile, testVol.hostDir /*writeTestFileContent*/)
			writeThenReadCmd := fmt.Sprintf("%s;%s", writeCmd, readCmd)
			podSpec1 := makeLocalPod(config, testVol, writeThenReadCmd)
			f.TestContainerOutput("pod writes to PV", podSpec1, 0, []string{testVol.hostDir})

			By("Creating pod2 to read from the PV")
			podSpec2 := makeLocalPod(config, testVol, readCmd)
			f.TestContainerOutput("pod reads PV", podSpec2, 0, []string{testVol.hostDir})
		})

		It("should be able to mount volume in two pods one after other, write from pod1, and read from pod2", func() {
			By("Creating pod1")
			pod1, pod1Err := createLocalPod(config, testVol)
			Expect(pod1Err).NotTo(HaveOccurred())

			framework.ExpectNoError(framework.WaitForPodRunningInNamespace(config.client, pod1))
			pod1NodeName, pod1NodeNameErr := podNodeName(config, pod1)
			Expect(pod1NodeNameErr).NotTo(HaveOccurred())
			framework.Logf("Pod1 %q created on Node %q", pod1.Name, pod1NodeName)
			Expect(pod1NodeName).To(Equal(node0.Name))

			writeCmd, readCmd := createWriteAndReadCmds(volumeDir, testFile, testVol.hostDir /*writeTestFileContent*/)

			By("Writing in pod1")
			podRWCmdExec(pod1, writeCmd)

			By("Deleting pod1")
			framework.DeletePodOrFail(config.client, config.ns, pod1.Name)

			By("Creating pod2")
			pod2, pod2Err := createLocalPod(config, testVol)
			Expect(pod2Err).NotTo(HaveOccurred())

			framework.ExpectNoError(framework.WaitForPodRunningInNamespace(config.client, pod2))
			pod2NodeName, pod2NodeNameErr := podNodeName(config, pod2)
			Expect(pod2NodeNameErr).NotTo(HaveOccurred())
			framework.Logf("Pod2 %q created on Node %q", pod2.Name, pod2NodeName)
			Expect(pod2NodeName).To(Equal(node0.Name))

			By("Reading in pod2")
			readOut := podRWCmdExec(pod2, readCmd)
			Expect(readOut).To(ContainSubstring(testVol.hostDir)) /*aka writeTestFileContents*/

			By("Deleting pod2")
			framework.DeletePodOrFail(config.client, config.ns, pod2.Name)
		})
	})

	Context("when two pods request one prebound PVC at the same time", func() {

		var testVol *localTestVolume

		BeforeEach(func() {
			testVol = setupLocalVolumePVCPV(config, node0)
		})

		AfterEach(func() {
			cleanupLocalVolume(config, testVol)
		})

		It("should be able to mount volume in two pods at the same time, write from pod1, and read from pod2", func() {
			By("Creating pod1 to write to the PV")
			pod1, pod1Err := createLocalPod(config, testVol)
			Expect(pod1Err).NotTo(HaveOccurred())

			framework.ExpectNoError(framework.WaitForPodRunningInNamespace(config.client, pod1))
			pod1NodeName, pod1NodeNameErr := podNodeName(config, pod1)
			Expect(pod1NodeNameErr).NotTo(HaveOccurred())
			framework.Logf("Pod1 %q created on Node %q", pod1.Name, pod1NodeName)
			Expect(pod1NodeName).To(Equal(node0.Name))

			By("Creating pod2 to read from the PV")
			pod2, pod2Err := createLocalPod(config, testVol)
			Expect(pod2Err).NotTo(HaveOccurred())

			framework.ExpectNoError(framework.WaitForPodRunningInNamespace(config.client, pod2))
			pod2NodeName, pod2NodeNameErr := podNodeName(config, pod2)
			Expect(pod2NodeNameErr).NotTo(HaveOccurred())
			framework.Logf("Pod2 %q created on Node %q", pod2.Name, pod2NodeName)
			Expect(pod2NodeName).To(Equal(node0.Name))

			writeCmd, readCmd := createWriteAndReadCmds(volumeDir, testFile, testVol.hostDir /*writeTestFileContent*/)

			By("Writing in pod1")
			podRWCmdExec(pod1, writeCmd)
			By("Reading in pod2")
			readOut := podRWCmdExec(pod2, readCmd)

			Expect(readOut).To(ContainSubstring(testVol.hostDir)) /*aka writeTestFileContents*/

			By("Deleting pod1")
			framework.DeletePodOrFail(config.client, config.ns, pod1.Name)
			By("Deleting pod2")
			framework.DeletePodOrFail(config.client, config.ns, pod2.Name)
		})
	})

	Context("when using local volume provisioner", func() {
		var (
			volumePath string
		)

		BeforeEach(func() {
			setupLocalVolumeProvisioner(config)
			volumePath = path.Join(
				hostBase, discoveryDir, fmt.Sprintf("vol-%v", string(uuid.NewUUID())))
		})

		AfterEach(func() {
			cleanupLocalVolumeProvisioner(config, volumePath)
		})

		It("should create and recreate local persistent volume", func() {
			By("Creating bootstrapper pod to start provisioner daemonset")
			createBootstrapperPod(config)
			kind := schema.GroupKind{Group: "extensions", Kind: "DaemonSet"}
			framework.WaitForControlledPodsRunning(config.client, config.ns, daemonSetName, kind)

			By("Creating a directory under discovery path")
			framework.Logf("creating local volume under path %q", volumePath)
			mkdirCmd := fmt.Sprintf("mkdir %v -m 777", volumePath)
			_, err := framework.NodeExec(node0.Name, mkdirCmd)
			Expect(err).NotTo(HaveOccurred())
			oldPV, err := waitForLocalPersistentVolume(config.client, volumePath)
			Expect(err).NotTo(HaveOccurred())

			// Create a persistent volume claim for local volume: the above volume will be bound.
			By("Creating a persistent volume claim")
			claim, err := config.client.Core().PersistentVolumeClaims(config.ns).Create(newLocalClaim(config))
			Expect(err).NotTo(HaveOccurred())
			err = framework.WaitForPersistentVolumeClaimPhase(
				v1.ClaimBound, config.client, claim.Namespace, claim.Name, framework.Poll, 1*time.Minute)
			Expect(claim.Spec.VolumeName).To(Equal(oldPV.Name))
			Expect(err).NotTo(HaveOccurred())

			// Delete the persistent volume claim: file will be cleaned up and volume be re-created.
			By("Deleting the persistent volume claim to clean up persistent volume and re-create one")
			writeCmd, readCmd := createWriteAndReadCmds(volumePath, testFile, testFileContent)
			_, err = framework.NodeExec(node0.Name, writeCmd)
			Expect(err).NotTo(HaveOccurred())
			err = config.client.Core().PersistentVolumeClaims(claim.Namespace).Delete(claim.Name, &metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred())
			newPV, err := waitForLocalPersistentVolume(config.client, volumePath)
			Expect(err).NotTo(HaveOccurred())
			Expect(newPV.UID).NotTo(Equal(oldPV.UID))
			result, err := framework.NodeExec(node0.Name, readCmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Code).NotTo(BeZero(), "file should be deleted across local pv recreation, but exists")
		})
	})
})

// podNode wraps RunKubectl to get node where pod is running
func podNodeName(config *localTestConfig, pod *v1.Pod) (string, error) {
	runtimePod, runtimePodErr := config.client.Core().Pods(pod.Namespace).Get(pod.Name, metav1.GetOptions{})
	return runtimePod.Spec.NodeName, runtimePodErr
}

// setupLocalVolume setups a directory to user for local PV
func setupLocalVolume(config *localTestConfig) *localTestVolume {
	testDirName := "local-volume-test-" + string(uuid.NewUUID())
	testDir := filepath.Join(containerBase, testDirName)
	hostDir := filepath.Join(hostBase, testDirName)
	// populate volume with testFile containing testFileContent
	writeCmd, _ := createWriteAndReadCmds(testDir, testFile, testFileContent)
	By(fmt.Sprintf("Creating local volume on node %q at path %q", config.node0.Name, hostDir))

	_, err := framework.NodeExec(config.node0.Name, writeCmd)
	Expect(err).NotTo(HaveOccurred())
	return &localTestVolume{
		node:         config.node0,
		hostDir:      hostDir,
		containerDir: testDir,
	}
}

// Deletes the PVC/PV, and launches a pod with hostpath volume to remove the test directory
func cleanupLocalVolume(config *localTestConfig, volume *localTestVolume) {
	if volume == nil {
		return
	}

	By("Cleaning up PVC and PV")
	errs := framework.PVPVCCleanup(config.client, config.ns, volume.pv, volume.pvc)
	if len(errs) > 0 {
		framework.Failf("Failed to delete PV and/or PVC: %v", utilerrors.NewAggregate(errs))
	}

	By("Removing the test directory")
	removeCmd := fmt.Sprintf("rm -r %s", volume.containerDir)
	_, err := framework.NodeExec(config.node0.Name, removeCmd)
	Expect(err).NotTo(HaveOccurred())
}

func makeLocalPVCConfig(config *localTestConfig) framework.PersistentVolumeClaimConfig {
	return framework.PersistentVolumeClaimConfig{
		AccessModes:      []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce},
		StorageClassName: &config.scName,
	}
}

func makeLocalPVConfig(config *localTestConfig, volume *localTestVolume) framework.PersistentVolumeConfig {
	// TODO: hostname may not be the best option
	nodeKey := "kubernetes.io/hostname"
	if volume.node.Labels == nil {
		framework.Failf("Node does not have labels")
	}
	nodeValue, found := volume.node.Labels[nodeKey]
	if !found {
		framework.Failf("Node does not have required label %q", nodeKey)
	}

	return framework.PersistentVolumeConfig{
		PVSource: v1.PersistentVolumeSource{
			Local: &v1.LocalVolumeSource{
				Path: volume.hostDir,
			},
		},
		NamePrefix:       "local-pv",
		StorageClassName: config.scName,
		NodeAffinity: &v1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &v1.NodeSelector{
				NodeSelectorTerms: []v1.NodeSelectorTerm{
					{
						MatchExpressions: []v1.NodeSelectorRequirement{
							{
								Key:      nodeKey,
								Operator: v1.NodeSelectorOpIn,
								Values:   []string{nodeValue},
							},
						},
					},
				},
			},
		},
	}
}

// Creates a PVC and PV with prebinding
func createLocalPVCPV(config *localTestConfig, volume *localTestVolume) {
	pvcConfig := makeLocalPVCConfig(config)
	pvConfig := makeLocalPVConfig(config, volume)

	var err error
	volume.pv, volume.pvc, err = framework.CreatePVPVC(config.client, pvConfig, pvcConfig, config.ns, true)
	framework.ExpectNoError(err)
	framework.ExpectNoError(framework.WaitOnPVandPVC(config.client, config.ns, volume.pv, volume.pvc))
}

func makeLocalPod(config *localTestConfig, volume *localTestVolume, cmd string) *v1.Pod {
	return framework.MakePod(config.ns, []*v1.PersistentVolumeClaim{volume.pvc}, false, cmd)
}

func createLocalPod(config *localTestConfig, volume *localTestVolume) (*v1.Pod, error) {
	return framework.CreatePod(config.client, config.ns, []*v1.PersistentVolumeClaim{volume.pvc}, false, "")
}

// Create corresponding write and read commands
// to be executed inside containers with local PV attached
func createWriteAndReadCmds(testFileDir string, testFile string, writeTestFileContent string) (writeCmd string, readCmd string) {
	testFilePath := filepath.Join(testFileDir, testFile)
	writeCmd = fmt.Sprintf("mkdir -p %s; echo %s > %s", testFileDir, writeTestFileContent, testFilePath)
	readCmd = fmt.Sprintf("cat %s", testFilePath)
	return writeCmd, readCmd
}

// Execute a read or write command in a pod.
// Fail on error
func podRWCmdExec(pod *v1.Pod, cmd string) string {
	out, err := podExec(pod, cmd)
	Expect(err).NotTo(HaveOccurred())
	return out
}

// Initialize test volume on node
// and create local PVC and PV
func setupLocalVolumePVCPV(config *localTestConfig, node *v1.Node) *localTestVolume {
	By("Initializing test volume")
	testVol := setupLocalVolume(config)

	By("Creating local PVC and PV")
	createLocalPVCPV(config, testVol)

	return testVol
}

func setupLocalVolumeProvisioner(config *localTestConfig) {
	By("Bootstrapping local volume provisioner")
	createServiceAccount(config)
	createClusterRoleBinding(config)
	createVolumeConfigMap(config)

	By("Initializing local volume discovery base path")
	mkdirCmd := fmt.Sprintf("mkdir %v -m 777", path.Join(hostBase, discoveryDir))
	_, err := framework.NodeExec(config.node0.Name, mkdirCmd)
	Expect(err).NotTo(HaveOccurred())
}

func cleanupLocalVolumeProvisioner(config *localTestConfig, volumePath string) {
	By("Cleaning up cluster role binding")
	deleteClusterRoleBinding(config)

	By("Removing the test directory")
	removeCmd := fmt.Sprintf("rm -r %s", path.Join(hostBase, discoveryDir))
	_, err := framework.NodeExec(config.node0.Name, removeCmd)
	Expect(err).NotTo(HaveOccurred())

	By("Cleaning up persistent volume")
	pv, err := findLocalPersistentVolume(config.client, volumePath)
	Expect(err).NotTo(HaveOccurred())
	err = config.client.Core().PersistentVolumes().Delete(pv.Name, &metav1.DeleteOptions{})
	Expect(err).NotTo(HaveOccurred())
}

func createServiceAccount(config *localTestConfig) {
	serviceAccount := v1.ServiceAccount{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta: metav1.ObjectMeta{Name: testServiceAccount, Namespace: config.ns},
	}
	_, err := config.client.CoreV1().ServiceAccounts(config.ns).Create(&serviceAccount)
	Expect(err).NotTo(HaveOccurred())
}

func createClusterRoleBinding(config *localTestConfig) {
	subjects := []rbacv1beta1.Subject{
		{
			Kind:      rbacv1beta1.ServiceAccountKind,
			Name:      testServiceAccount,
			Namespace: config.ns,
		},
	}

	binding := rbacv1beta1.ClusterRoleBinding{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "rbac.authorization.k8s.io/v1beta1",
			Kind:       "ClusterRoleBinding",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: testRoleBinding,
		},
		RoleRef: rbacv1beta1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     "cluster-admin",
		},
		Subjects: subjects,
	}

	_, err := config.client.RbacV1beta1().ClusterRoleBindings().Create(&binding)
	Expect(err).NotTo(HaveOccurred())
}

func deleteClusterRoleBinding(config *localTestConfig) {
	err := config.client.RbacV1beta1().ClusterRoleBindings().Delete(testRoleBinding, metav1.NewDeleteOptions(0))
	Expect(err).NotTo(HaveOccurred())
	// These role bindings are created in provisioner; we just ensure it's
	// deleted and do not panic on error.
	config.client.RbacV1beta1().ClusterRoleBindings().Delete(nodeBindingName, metav1.NewDeleteOptions(0))
	config.client.RbacV1beta1().ClusterRoleBindings().Delete(pvBindingName, metav1.NewDeleteOptions(0))
}

func createVolumeConfigMap(config *localTestConfig) {
	mountConfig := struct {
		HostDir string `json:"hostDir"`
	}{
		HostDir: path.Join(hostBase, discoveryDir),
	}
	data, err := json.Marshal(&mountConfig)
	Expect(err).NotTo(HaveOccurred())

	configMap := v1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "ConfigMap",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      volumeConfigName,
			Namespace: config.ns,
		},
		Data: map[string]string{
			config.scName: string(data),
		},
	}
	_, err = config.client.CoreV1().ConfigMaps(config.ns).Create(&configMap)
	Expect(err).NotTo(HaveOccurred())
}

func createBootstrapperPod(config *localTestConfig) {
	pod := &v1.Pod{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "local-volume-tester-",
		},
		Spec: v1.PodSpec{
			RestartPolicy:      v1.RestartPolicyNever,
			ServiceAccountName: testServiceAccount,
			Containers: []v1.Container{
				{
					Name:  "volume-tester",
					Image: bootstrapperImageName,
					Env: []v1.EnvVar{
						{
							Name: "MY_NAMESPACE",
							ValueFrom: &v1.EnvVarSource{
								FieldRef: &v1.ObjectFieldSelector{
									FieldPath: "metadata.namespace",
								},
							},
						},
					},
					Args: []string{
						fmt.Sprintf("--image=%v", provisionerImageName),
						fmt.Sprintf("--volume-config=%v", volumeConfigName),
					},
				},
			},
		},
	}
	pod, err := config.client.CoreV1().Pods(config.ns).Create(pod)
	Expect(err).NotTo(HaveOccurred())
	err = framework.WaitForPodSuccessInNamespace(config.client, pod.Name, pod.Namespace)
	Expect(err).NotTo(HaveOccurred())
}

// newLocalClaim creates a new persistent volume claim.
func newLocalClaim(config *localTestConfig) *v1.PersistentVolumeClaim {
	claim := v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "local-pvc-",
			Namespace:    config.ns,
		},
		Spec: v1.PersistentVolumeClaimSpec{
			StorageClassName: &config.scName,
			AccessModes: []v1.PersistentVolumeAccessMode{
				v1.ReadWriteOnce,
			},
			Resources: v1.ResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceName(v1.ResourceStorage): resource.MustParse(testRequestSize),
				},
			},
		},
	}

	return &claim
}

// waitForLocalPersistentVolume waits a local persistent volume with 'volumePath' to be available.
func waitForLocalPersistentVolume(c clientset.Interface, volumePath string) (*v1.PersistentVolume, error) {
	var pv *v1.PersistentVolume
	for start := time.Now(); time.Since(start) < 10*time.Minute && pv == nil; time.Sleep(5 * time.Second) {
		pvs, err := c.Core().PersistentVolumes().List(metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		if len(pvs.Items) == 0 {
			continue
		}
		for _, p := range pvs.Items {
			if p.Spec.PersistentVolumeSource.Local == nil || p.Spec.PersistentVolumeSource.Local.Path != volumePath {
				continue
			}
			if p.Status.Phase != v1.VolumeAvailable {
				continue
			}
			pv = &p
			break
		}
	}
	if pv == nil {
		return nil, fmt.Errorf("Timeout while waiting for local persistent volume with path %v to be available", volumePath)
	}
	return pv, nil
}

// findLocalPersistentVolume finds persistent volume with 'spec.local.path' equals 'volumePath'.
func findLocalPersistentVolume(c clientset.Interface, volumePath string) (*v1.PersistentVolume, error) {
	pvs, err := c.Core().PersistentVolumes().List(metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for _, p := range pvs.Items {
		if p.Spec.PersistentVolumeSource.Local != nil && p.Spec.PersistentVolumeSource.Local.Path == volumePath {
			return &p, nil
		}
	}
	return nil, fmt.Errorf("Unable to find local persistent volume with path %v", volumePath)
}
