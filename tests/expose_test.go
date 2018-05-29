package tests_test

import (
	"flag"
	"fmt"
	"strconv"
	"time"

	"github.com/google/goexpect"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	k8sv1 "k8s.io/api/core/v1"
	k8smetav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "kubevirt.io/kubevirt/pkg/api/v1"
	"kubevirt.io/kubevirt/pkg/kubecli"
	"kubevirt.io/kubevirt/pkg/virtctl/expose"
	"kubevirt.io/kubevirt/pkg/virtctl/offlinevm"
	"kubevirt.io/kubevirt/tests"
)

func newLabeledVM(label string, virtClient kubecli.KubevirtClient) (vm *v1.VirtualMachine) {
	vm = tests.NewRandomVMWithEphemeralDiskAndUserdata(tests.RegistryDiskFor(tests.RegistryDiskCirros), "#!/bin/bash\necho 'hello'\n")
	vm.Labels = map[string]string{"expose": label}
	vm, err := virtClient.VM(tests.NamespaceTestDefault).Create(vm)
	Expect(err).ToNot(HaveOccurred())
	tests.WaitForSuccessfulVMStartIgnoreWarnings(vm)
	vm, err = virtClient.VM(tests.NamespaceTestDefault).Get(vm.ObjectMeta.Name, k8smetav1.GetOptions{})
	Expect(err).ToNot(HaveOccurred())
	return
}

func generateHelloWorldServer(vm *v1.VirtualMachine, virtClient kubecli.KubevirtClient, testPort int, protocol string) {
	_, err := tests.LoggedInCirrosExpecter(vm)
	Expect(err).ToNot(HaveOccurred())

	expecter, _, err := tests.NewConsoleExpecter(virtClient, vm, 10*time.Second)
	defer expecter.Close()
	Expect(err).ToNot(HaveOccurred())

	serverCommand := fmt.Sprintf("screen -d -m nc -klp %d -e echo -e \"Hello World!\"\n", testPort)
	if protocol == "udp" {
		serverCommand = fmt.Sprintf("screen -d -m nc -uklp %d -e echo -e \"Hello UDP World!\"\n", testPort)
	}
	_, err = expecter.ExpectBatch([]expect.Batcher{
		&expect.BSnd{S: "\n"},
		&expect.BExp{R: "\\$ "},
		&expect.BSnd{S: serverCommand},
		&expect.BExp{R: "\\$ "},
		&expect.BSnd{S: "echo $?\n"},
		&expect.BExp{R: "0"},
	}, 60*time.Second)
	Expect(err).ToNot(HaveOccurred())
}

var _ = Describe("Expose", func() {

	flag.Parse()

	virtClient, err := kubecli.GetKubevirtClient()
	tests.PanicOnError(err)
	const testPort = 1500

	Context("Expose service on a VM", func() {
		var tcpVM *v1.VirtualMachine
		tests.BeforeAll(func() {
			tcpVM = newLabeledVM("vm", virtClient)
			generateHelloWorldServer(tcpVM, virtClient, testPort, "tcp")
		})

		Context("Expose ClusterIP service", func() {
			const servicePort = "27017"
			const serviceName = "cluster-ip-vm"
			It("Should expose a Cluster IP service on a VM and connect to it", func() {
				By("Exposing the service via virtctl command")
				virtctl := tests.NewRepeatableVirtctlCommand(expose.COMMAND_EXPOSE, "virtualmachine", "--namespace",
					tcpVM.Namespace, tcpVM.Name, "--port", servicePort, "--name", serviceName, "--target-port", strconv.Itoa(testPort))
				err := virtctl()
				Expect(err).ToNot(HaveOccurred())

				By("Getting back the cluster IP given for the service")
				svc, err := virtClient.CoreV1().Services(tcpVM.Namespace).Get(serviceName, k8smetav1.GetOptions{})
				Expect(err).ToNot(HaveOccurred())
				serviceIP := svc.Spec.ClusterIP

				By("Starting a pod which tries to reach the VM via ClusterIP")
				job := tests.NewHelloWorldJob(serviceIP, servicePort)
				job, err = virtClient.CoreV1().Pods(tcpVM.Namespace).Create(job)
				Expect(err).ToNot(HaveOccurred())

				By("Waiting for the pod to report a successful connection attempt")
				getStatus := func() k8sv1.PodPhase {
					pod, err := virtClient.CoreV1().Pods(job.Namespace).Get(job.Name, k8smetav1.GetOptions{})
					Expect(err).ToNot(HaveOccurred())
					return pod.Status.Phase
				}
				Eventually(getStatus, 60, 1).Should(Equal(k8sv1.PodSucceeded))
			})
		})

		Context("Expose NodePort service", func() {
			const servicePort = "27017"
			const serviceName = "node-port-vm"
			const nodePort = "30017"

			It("Should expose a NodePort service on a VM and connect to it", func() {
				By("Exposing the service via virtctl command")
				virtctl := tests.NewRepeatableVirtctlCommand(expose.COMMAND_EXPOSE, "virtualmachine", "--namespace",
					tcpVM.Namespace, tcpVM.Name, "--port", servicePort, "--name", serviceName, "--target-port", strconv.Itoa(testPort),
					"--type", "NodePort", "--node-port", nodePort)
				err := virtctl()
				Expect(err).ToNot(HaveOccurred())

				By("Getting back the the service")
				_, err = virtClient.CoreV1().Services(tcpVM.Namespace).Get(serviceName, k8smetav1.GetOptions{})
				Expect(err).ToNot(HaveOccurred())

				By("Getting the node IP from all nodes")
				nodes, err := virtClient.CoreV1().Nodes().List(k8smetav1.ListOptions{})
				Expect(err).ToNot(HaveOccurred())
				Expect(nodes.Items).ToNot(BeEmpty())
				for _, node := range nodes.Items {
					Expect(node.Status.Addresses).ToNot(BeEmpty())
					nodeIP := node.Status.Addresses[0].Address

					By("Starting a pod which tries to reach the VM via NodePort")
					job := tests.NewHelloWorldJob(nodeIP, nodePort)
					job, err = virtClient.CoreV1().Pods(tcpVM.Namespace).Create(job)
					Expect(err).ToNot(HaveOccurred())

					By("Waiting for the pod to report a successful connection attempt")
					getStatus := func() k8sv1.PodPhase {
						pod, err := virtClient.CoreV1().Pods(job.Namespace).Get(job.Name, k8smetav1.GetOptions{})
						Expect(err).ToNot(HaveOccurred())
						return pod.Status.Phase
					}
					Eventually(getStatus, 60, 1).Should(Equal(k8sv1.PodSucceeded))
				}
			})
		})
	})

	Context("Expose UDP service on a VM", func() {
		var udpVM *v1.VirtualMachine
		tests.BeforeAll(func() {
			udpVM = newLabeledVM("udp-vm", virtClient)
			generateHelloWorldServer(udpVM, virtClient, testPort, "udp")
		})

		Context("Expose NodePort UDP service", func() {
			const servicePort = "27018"
			const serviceName = "node-port-udp-vm"
			const nodePort = "30018"

			It("Should expose a NodePort service on a VM and connect to it", func() {
				By("Exposing the service via virtctl command")
				virtctl := tests.NewRepeatableVirtctlCommand(expose.COMMAND_EXPOSE, "virtualmachine", "--namespace",
					udpVM.Namespace, udpVM.Name, "--port", servicePort, "--name", serviceName, "--target-port", strconv.Itoa(testPort),
					"--type", "NodePort", "--node-port", nodePort, "--protocol", "UDP")
				err := virtctl()
				Expect(err).ToNot(HaveOccurred())

				By("Getting back the the service")
				_, err = virtClient.CoreV1().Services(udpVM.Namespace).Get(serviceName, k8smetav1.GetOptions{})
				Expect(err).ToNot(HaveOccurred())

				By("Getting the node IP from all nodes")
				nodes, err := virtClient.CoreV1().Nodes().List(k8smetav1.ListOptions{})
				Expect(err).ToNot(HaveOccurred())
				Expect(nodes.Items).ToNot(BeEmpty())
				for _, node := range nodes.Items {
					Expect(node.Status.Addresses).ToNot(BeEmpty())
					nodeIP := node.Status.Addresses[0].Address

					By("Starting a pod which tries to reach the VM via NodePort")
					job := tests.NewHelloWorldJobUDP(nodeIP, nodePort)
					job, err = virtClient.CoreV1().Pods(udpVM.Namespace).Create(job)
					Expect(err).ToNot(HaveOccurred())

					By("Waiting for the pod to report a successful connection attempt")
					getStatus := func() k8sv1.PodPhase {
						pod, err := virtClient.CoreV1().Pods(job.Namespace).Get(job.Name, k8smetav1.GetOptions{})
						Expect(err).ToNot(HaveOccurred())
						return pod.Status.Phase
					}
					Eventually(getStatus, 60, 1).Should(Equal(k8sv1.PodSucceeded))
				}
			})
		})
	})

	Context("Expose service on a VM replica set", func() {
		var vmrs *v1.VirtualMachineReplicaSet
		tests.BeforeAll(func() {
			By("Creating a VMRS object with 2 replicas")
			const numberOfVMs = 2
			template := tests.NewRandomVMWithEphemeralDiskAndUserdata(tests.RegistryDiskFor(tests.RegistryDiskCirros), "#!/bin/bash\necho 'hello'\n")
			vmrs = tests.NewRandomReplicaSetFromVM(template, int32(numberOfVMs))
			vmrs.Labels = map[string]string{"expose": "vmrs"}

			By("Start the replica set")
			vmrs, err = virtClient.ReplicaSet(tests.NamespaceTestDefault).Create(vmrs)
			Expect(err).ToNot(HaveOccurred())

			By("Checking the number of ready replicas")
			Eventually(func() int {
				rs, err := virtClient.ReplicaSet(tests.NamespaceTestDefault).Get(vmrs.ObjectMeta.Name, k8smetav1.GetOptions{})
				Expect(err).ToNot(HaveOccurred())
				return int(rs.Status.ReadyReplicas)
			}, 60*time.Second, 1*time.Second).Should(Equal(numberOfVMs))

			By("add an 'hello world' server on each VM in the replica set")
			// TODO: add label to list options
			// check size of list
			// remove check for owner
			vms, err := virtClient.VM(vmrs.ObjectMeta.Namespace).List(k8smetav1.ListOptions{})
			Expect(err).ToNot(HaveOccurred())
			for _, vm := range vms.Items {
				if vm.OwnerReferences != nil {
					generateHelloWorldServer(&vm, virtClient, testPort, "tcp")
				}
			}
		})

		Context("Expose ClusterIP service", func() {
			const servicePort = "27017"
			const serviceName = "cluster-ip-vmrs"

			It("Should create a ClusterIP service on VMRS and connect to it", func() {
				By("Expose a service on the VMRS using virtctl")
				virtctl := tests.NewRepeatableVirtctlCommand(expose.COMMAND_EXPOSE, "vmrs", "--namespace",
					vmrs.Namespace, vmrs.Name, "--port", servicePort, "--name", serviceName, "--target-port", strconv.Itoa(testPort))
				err = virtctl()
				Expect(err).ToNot(HaveOccurred())

				By("Getting back the cluster IP given for the service")
				svc, err := virtClient.CoreV1().Services(vmrs.Namespace).Get(serviceName, k8smetav1.GetOptions{})
				Expect(err).ToNot(HaveOccurred())
				serviceIP := svc.Spec.ClusterIP

				By("Starting a pod which tries to reach the VM via ClusterIP")
				job := tests.NewHelloWorldJob(serviceIP, servicePort)
				job, err = virtClient.CoreV1().Pods(vmrs.Namespace).Create(job)
				Expect(err).ToNot(HaveOccurred())

				By("Waiting for the pod to report a successful connection attempt")
				getStatus := func() k8sv1.PodPhase {
					pod, err := virtClient.CoreV1().Pods(job.Namespace).Get(job.Name, k8smetav1.GetOptions{})
					Expect(err).ToNot(HaveOccurred())
					return pod.Status.Phase
				}
				Eventually(getStatus, 60, 1).Should(Equal(k8sv1.PodSucceeded))
			})
		})
	})

	Context("Expose service on an Offline VM", func() {
		const servicePort = "27017"
		const serviceName = "cluster-ip-ovm"
		var ovm *v1.OfflineVirtualMachine

		tests.BeforeAll(func() {
			By("Creating an OVM object")
			template := tests.NewRandomVMWithEphemeralDiskAndUserdata(tests.RegistryDiskFor(tests.RegistryDiskCirros), "#!/bin/bash\necho 'hello'\n")
			template.Labels = map[string]string{"expose": "vmrs"}
			ovm = NewRandomOfflineVirtualMachine(template, false)

			By("Creating the OVM")
			_, err := virtClient.OfflineVirtualMachine(tests.NamespaceTestDefault).Create(ovm)
			Expect(err).ToNot(HaveOccurred())

			By("Exposing a service on the OVM using virtctl")
			virtctl := tests.NewRepeatableVirtctlCommand(expose.COMMAND_EXPOSE, "offlinevirtualmachine", "--namespace",
				ovm.Namespace, ovm.Name, "--port", servicePort, "--name", serviceName, "--target-port", strconv.Itoa(testPort))
			err = virtctl()
			Expect(err).ToNot(HaveOccurred())

			By("Calling the start command")
			virtctl = tests.NewRepeatableVirtctlCommand(offlinevm.COMMAND_START, "--namespace", ovm.Namespace, ovm.Name)
			err = virtctl()
			Expect(err).ToNot(HaveOccurred())

			By("Getting the status of the OVM")
			Eventually(func() bool {
				ovm, err = virtClient.OfflineVirtualMachine(ovm.Namespace).Get(ovm.Name, k8smetav1.GetOptions{})
				Expect(err).ToNot(HaveOccurred())
				return ovm.Status.Ready
			}, 120*time.Second, 1*time.Second).Should(BeTrue())

			By("Getting the running VM")
			var vm *v1.VirtualMachine
			Eventually(func() bool {
				vm, err = virtClient.VM(ovm.Namespace).Get(ovm.Name, k8smetav1.GetOptions{})
				Expect(err).ToNot(HaveOccurred())
				return vm.Status.Phase == v1.Running
			}, 120*time.Second, 1*time.Second).Should(BeTrue())

			generateHelloWorldServer(vm, virtClient, testPort, "tcp")
		})

		Context("Expose ClusterIP service", func() {
			It("Connect to ClusterIP services that was set when VM was offline", func() {
				By("Getting back the cluster IP given for the service")
				svc, err := virtClient.CoreV1().Services(ovm.Namespace).Get(serviceName, k8smetav1.GetOptions{})
				Expect(err).ToNot(HaveOccurred())
				serviceIP := svc.Spec.ClusterIP

				By("Starting a pod which tries to reach the VM via ClusterIP")
				job := tests.NewHelloWorldJob(serviceIP, servicePort)
				job, err = virtClient.CoreV1().Pods(ovm.Namespace).Create(job)
				Expect(err).ToNot(HaveOccurred())

				By("Waiting for the pod to report a successful connection attempt")
				getStatus := func() k8sv1.PodPhase {
					pod, err := virtClient.CoreV1().Pods(job.Namespace).Get(job.Name, k8smetav1.GetOptions{})
					Expect(err).ToNot(HaveOccurred())
					return pod.Status.Phase
				}

				Eventually(getStatus, 60, 1).Should(Equal(k8sv1.PodSucceeded))
			})
		})
	})
})
