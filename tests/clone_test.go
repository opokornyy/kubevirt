package tests

import (
	"context"
	"fmt"
	"strings"
	"time"

	virtsnapshot "kubevirt.io/api/snapshot"
	"kubevirt.io/api/snapshot/v1alpha1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/format"

	k8sv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/pointer"

	clonev1alpha1 "kubevirt.io/api/clone/v1alpha1"
	virtv1 "kubevirt.io/api/core/v1"
	instancetypev1alpha2 "kubevirt.io/api/instancetype/v1alpha2"
	"kubevirt.io/client-go/kubecli"

	virtconfig "kubevirt.io/kubevirt/pkg/virt-config"
	"kubevirt.io/kubevirt/tests/console"
	cd "kubevirt.io/kubevirt/tests/containerdisk"
	. "kubevirt.io/kubevirt/tests/framework/matcher"
	"kubevirt.io/kubevirt/tests/libinstancetype"
	"kubevirt.io/kubevirt/tests/libstorage"
	"kubevirt.io/kubevirt/tests/libvmi"
	"kubevirt.io/kubevirt/tests/util"
)

const (
	vmAPIGroup = "kubevirt.io"
)

type loginFunction func(*virtv1.VirtualMachineInstance) error

var _ = Describe("[Serial][sig-compute]VirtualMachineClone Tests", Serial, func() {
	var err error
	var virtClient kubecli.KubevirtClient

	BeforeEach(func() {
		virtClient, err = kubecli.GetKubevirtClient()
		util.PanicOnError(err)

		EnableFeatureGate(virtconfig.SnapshotGate)

		format.MaxLength = 0
	})

	createVM := func(options ...libvmi.Option) (vm *virtv1.VirtualMachine) {
		vmi := libvmi.NewCirros(options...)
		vmi.Namespace = util.NamespaceTestDefault
		vm = NewRandomVirtualMachine(vmi, false)
		vm.Annotations = vmi.Annotations
		vm.Labels = vmi.Labels

		By(fmt.Sprintf("Creating VM %s", vm.Name))
		vm, err := virtClient.VirtualMachine(vm.Namespace).Create(vm)
		Expect(err).ShouldNot(HaveOccurred())

		return
	}

	createSnapshot := func(vm *virtv1.VirtualMachine) *v1alpha1.VirtualMachineSnapshot {
		var err error

		snapshot := &v1alpha1.VirtualMachineSnapshot{
			ObjectMeta: v1.ObjectMeta{
				Name:      "snapshot-" + vm.Name,
				Namespace: vm.Namespace,
			},
			Spec: v1alpha1.VirtualMachineSnapshotSpec{
				Source: k8sv1.TypedLocalObjectReference{
					APIGroup: pointer.String(vmAPIGroup),
					Kind:     "VirtualMachine",
					Name:     vm.Name,
				},
			},
		}

		snapshot, err = virtClient.VirtualMachineSnapshot(snapshot.Namespace).Create(context.Background(), snapshot, v1.CreateOptions{})
		ExpectWithOffset(1, err).ToNot(HaveOccurred())

		return snapshot
	}

	waitSnapshotReady := func(snapshot *v1alpha1.VirtualMachineSnapshot) *v1alpha1.VirtualMachineSnapshot {
		var err error

		EventuallyWithOffset(1, func() bool {
			snapshot, err = virtClient.VirtualMachineSnapshot(snapshot.Namespace).Get(context.Background(), snapshot.Name, v1.GetOptions{})
			ExpectWithOffset(1, err).ToNot(HaveOccurred())
			return snapshot.Status != nil && snapshot.Status.ReadyToUse != nil && *snapshot.Status.ReadyToUse
		}, 180*time.Second, time.Second).Should(BeTrue(), "snapshot should be ready")

		return snapshot
	}

	waitSnapshotContentsExist := func(snapshot *v1alpha1.VirtualMachineSnapshot) *v1alpha1.VirtualMachineSnapshot {
		var contentsName string
		EventuallyWithOffset(1, func() error {
			snapshot, err = virtClient.VirtualMachineSnapshot(snapshot.Namespace).Get(context.Background(), snapshot.Name, v1.GetOptions{})
			ExpectWithOffset(2, err).ToNot(HaveOccurred())
			if snapshot.Status == nil {
				return fmt.Errorf("snapshot's status is nil")
			}

			if snapshot.Status.VirtualMachineSnapshotContentName != nil {
				contentsName = *snapshot.Status.VirtualMachineSnapshotContentName
			} else {
				return fmt.Errorf("vm snapshot contents name is nil")
			}

			return nil
		}, 30*time.Second, 1*time.Second).ShouldNot(HaveOccurred())

		EventuallyWithOffset(1, func() error {
			_, err := virtClient.VirtualMachineSnapshotContent(snapshot.Namespace).Get(context.Background(), contentsName, v1.GetOptions{})
			return err
		}).ShouldNot(HaveOccurred())

		return snapshot
	}

	generateCloneFromVM := func(sourceVM *virtv1.VirtualMachine, targetVMName string) *clonev1alpha1.VirtualMachineClone {
		vmClone := kubecli.NewMinimalCloneWithNS("testclone", util.NamespaceTestDefault)

		cloneSourceRef := &k8sv1.TypedLocalObjectReference{
			APIGroup: pointer.String(vmAPIGroup),
			Kind:     "VirtualMachine",
			Name:     sourceVM.Name,
		}

		cloneTargetRef := cloneSourceRef.DeepCopy()
		cloneTargetRef.Name = targetVMName

		vmClone.Spec.Source = cloneSourceRef
		vmClone.Spec.Target = cloneTargetRef

		return vmClone
	}

	generateCloneFromSnapshot := func(snapshot *v1alpha1.VirtualMachineSnapshot, targetVMName string) *clonev1alpha1.VirtualMachineClone {
		vmClone := kubecli.NewMinimalCloneWithNS("testclone", util.NamespaceTestDefault)

		cloneSourceRef := &k8sv1.TypedLocalObjectReference{
			APIGroup: pointer.String(virtsnapshot.GroupName),
			Kind:     "VirtualMachineSnapshot",
			Name:     snapshot.Name,
		}

		cloneTargetRef := &k8sv1.TypedLocalObjectReference{
			APIGroup: pointer.String(vmAPIGroup),
			Kind:     "VirtualMachine",
			Name:     targetVMName,
		}

		vmClone.Spec.Source = cloneSourceRef
		vmClone.Spec.Target = cloneTargetRef

		return vmClone
	}

	createCloneAndWaitForFinish := func(vmClone *clonev1alpha1.VirtualMachineClone) {
		By(fmt.Sprintf("Creating clone object %s", vmClone.Name))
		vmClone, err = virtClient.VirtualMachineClone(vmClone.Namespace).Create(context.Background(), vmClone, v1.CreateOptions{})
		Expect(err).ShouldNot(HaveOccurred())

		By(fmt.Sprintf("Waiting for the clone %s to finish", vmClone.Name))
		Eventually(func() clonev1alpha1.VirtualMachineClonePhase {
			vmClone, err = virtClient.VirtualMachineClone(vmClone.Namespace).Get(context.Background(), vmClone.Name, v1.GetOptions{})
			Expect(err).ShouldNot(HaveOccurred())

			return vmClone.Status.Phase
		}, 3*time.Minute, 3*time.Second).Should(Equal(clonev1alpha1.Succeeded), "clone should finish successfully")
	}

	expectVMRunnable := func(vm *virtv1.VirtualMachine, login loginFunction) *virtv1.VirtualMachine {
		By(fmt.Sprintf("Starting VM %s", vm.Name))
		vm = StartVirtualMachine(vm)
		targetVMI, err := virtClient.VirtualMachineInstance(vm.Namespace).Get(vm.Name, &v1.GetOptions{})
		Expect(err).ShouldNot(HaveOccurred())

		err = login(targetVMI)
		Expect(err).ShouldNot(HaveOccurred())

		vm = StopVirtualMachine(vm)

		return vm
	}

	filterOutIrrelevantKeys := func(in map[string]string) map[string]string {
		out := make(map[string]string)

		for key, val := range in {
			if !strings.Contains(key, "kubevirt.io") && !strings.Contains(key, "kubemacpool.io") {
				out[key] = val
			}
		}

		return out
	}

	Context("VM clone", func() {

		const (
			targetVMName                     = "vm-clone-target"
			cloneShouldEqualSourceMsgPattern = "cloned VM's %s should be equal to source"

			key1   = "key1"
			key2   = "key2"
			value1 = "value1"
			value2 = "value2"
		)

		var (
			sourceVM, targetVM *virtv1.VirtualMachine
			vmClone            *clonev1alpha1.VirtualMachineClone
		)

		expectEqualStrMap := func(actual, expected map[string]string, expectationMsg string, keysToExclude ...string) {
			expected = filterOutIrrelevantKeys(expected)
			actual = filterOutIrrelevantKeys(actual)

			for _, key := range keysToExclude {
				delete(expected, key)
			}

			Expect(actual).To(Equal(expected), expectationMsg)
		}

		expectEqualLabels := func(targetVM, sourceVM *virtv1.VirtualMachine, keysToExclude ...string) {
			expectEqualStrMap(targetVM.Labels, sourceVM.Labels, fmt.Sprintf(cloneShouldEqualSourceMsgPattern, "labels"), keysToExclude...)
		}

		expectEqualAnnotations := func(targetVM, sourceVM *virtv1.VirtualMachine, keysToExclude ...string) {
			expectEqualStrMap(targetVM.Annotations, sourceVM.Annotations, fmt.Sprintf(cloneShouldEqualSourceMsgPattern, "annotations"), keysToExclude...)
		}

		expectSpecsToEqualExceptForMacAddress := func(vm1, vm2 *virtv1.VirtualMachine) {
			vm1Spec := vm1.Spec.DeepCopy()
			vm2Spec := vm2.Spec.DeepCopy()

			for _, spec := range []*virtv1.VirtualMachineSpec{vm1Spec, vm2Spec} {
				for i := range spec.Template.Spec.Domain.Devices.Interfaces {
					spec.Template.Spec.Domain.Devices.Interfaces[i].MacAddress = ""
				}
			}

			Expect(vm1Spec).To(Equal(vm2Spec), fmt.Sprintf(cloneShouldEqualSourceMsgPattern, "spec not including mac adresses"))
		}

		createVM := func(options ...libvmi.Option) (vm *virtv1.VirtualMachine) {
			defaultOptions := []libvmi.Option{
				libvmi.WithLabel(key1, value1),
				libvmi.WithLabel(key2, value2),
				libvmi.WithAnnotation(key1, value1),
				libvmi.WithAnnotation(key2, value2),
			}

			options = append(options, defaultOptions...)
			return createVM(options...)
		}

		generateCloneFromVM := func() *clonev1alpha1.VirtualMachineClone {
			return generateCloneFromVM(sourceVM, targetVMName)
		}

		Context("simple VM and cloning operations", func() {

			expectVMRunnable := func(vm *virtv1.VirtualMachine) *virtv1.VirtualMachine {
				return expectVMRunnable(vm, console.LoginToCirros)
			}

			It("simple default clone", func() {
				sourceVM = createVM()
				vmClone = generateCloneFromVM()

				createCloneAndWaitForFinish(vmClone)

				By(fmt.Sprintf("Getting the target VM %s", targetVMName))
				targetVM, err = virtClient.VirtualMachine(sourceVM.Namespace).Get(targetVMName, &v1.GetOptions{})
				Expect(err).ShouldNot(HaveOccurred())

				By("Making sure target is runnable")
				targetVM = expectVMRunnable(targetVM)

				Expect(targetVM.Spec).To(Equal(sourceVM.Spec), fmt.Sprintf(cloneShouldEqualSourceMsgPattern, "spec"))
				expectEqualLabels(targetVM, sourceVM)
				expectEqualAnnotations(targetVM, sourceVM)

				By("Making sure snapshot and restore objects are cleaned up")
				Expect(vmClone.Status.SnapshotName).To(BeNil())
				Expect(vmClone.Status.RestoreName).To(BeNil())
			})

			It("simple clone with snapshot source", func() {
				By("Creating a VM")
				sourceVM = createVM()
				Eventually(func() virtv1.VirtualMachinePrintableStatus {
					sourceVM, err = virtClient.VirtualMachine(sourceVM.Namespace).Get(sourceVM.Name, &v1.GetOptions{})
					Expect(err).ToNot(HaveOccurred())

					return sourceVM.Status.PrintableStatus
				}, 30*time.Second, 1*time.Second).Should(Equal(virtv1.VirtualMachineStatusStopped))

				By("Creating a snapshot from VM")
				snapshot := createSnapshot(sourceVM)
				snapshot = waitSnapshotContentsExist(snapshot)
				// "waitSnapshotReady" is not used here intentionally since it's okay for a snapshot source
				// to not be ready when creating a clone. Therefore, it's not deterministic if snapshot would actually
				// be ready for this test or not.
				// TODO: use snapshot's createDenyVolumeSnapshotCreateWebhook() once it's refactored to work outside
				// of snapshot tests scope.

				By("Deleting VM")
				err = virtClient.VirtualMachine(sourceVM.Namespace).Delete(sourceVM.Name, &v1.DeleteOptions{})
				Expect(err).ToNot(HaveOccurred())

				By("Creating a clone with a snapshot source")
				vmClone = generateCloneFromSnapshot(snapshot, targetVMName)
				createCloneAndWaitForFinish(vmClone)

				By(fmt.Sprintf("Getting the target VM %s", targetVMName))
				targetVM, err = virtClient.VirtualMachine(sourceVM.Namespace).Get(targetVMName, &v1.GetOptions{})
				Expect(err).ShouldNot(HaveOccurred())

				By("Making sure target is runnable")
				targetVM = expectVMRunnable(targetVM)

				By("Making sure snapshot source is not being deleted")
				_, err = virtClient.VirtualMachineSnapshot(snapshot.Namespace).Get(context.Background(), snapshot.Name, v1.GetOptions{})
				Expect(err).ShouldNot(HaveOccurred())
			})

			It("clone with only some of labels/annotations", func() {
				sourceVM = createVM()
				vmClone = generateCloneFromVM()

				vmClone.Spec.LabelFilters = []string{
					"*",
					"!" + key2,
				}
				vmClone.Spec.AnnotationFilters = []string{
					key1,
				}
				createCloneAndWaitForFinish(vmClone)

				By(fmt.Sprintf("Getting the target VM %s", targetVMName))
				targetVM, err = virtClient.VirtualMachine(sourceVM.Namespace).Get(targetVMName, &v1.GetOptions{})
				Expect(err).ShouldNot(HaveOccurred())

				By("Making sure target is runnable")
				targetVM = expectVMRunnable(targetVM)

				Expect(targetVM.Spec).To(Equal(sourceVM.Spec), fmt.Sprintf(cloneShouldEqualSourceMsgPattern, "spec"))
				expectEqualLabels(targetVM, sourceVM, key2)
				expectEqualAnnotations(targetVM, sourceVM, key2)
			})

			It("clone with changed MAC address", func() {
				const newMacAddress = "BE-AD-00-00-BE-04"
				sourceVM = createVM(
					libvmi.WithInterface(libvmi.InterfaceDeviceWithMasqueradeBinding()),
					libvmi.WithNetwork(virtv1.DefaultPodNetwork()),
				)

				srcInterfaces := sourceVM.Spec.Template.Spec.Domain.Devices.Interfaces
				Expect(srcInterfaces).ToNot(BeEmpty())
				srcInterface := srcInterfaces[0]

				vmClone = generateCloneFromVM()
				vmClone.Spec.NewMacAddresses = map[string]string{
					srcInterface.Name: newMacAddress,
				}

				createCloneAndWaitForFinish(vmClone)

				By(fmt.Sprintf("Getting the target VM %s", targetVMName))
				targetVM, err = virtClient.VirtualMachine(sourceVM.Namespace).Get(targetVMName, &v1.GetOptions{})
				Expect(err).ShouldNot(HaveOccurred())

				By("Making sure target is runnable")
				targetVM = expectVMRunnable(targetVM)

				By("Finding target interface with same name as original")
				var targetInterface *virtv1.Interface
				targetInterfaces := targetVM.Spec.Template.Spec.Domain.Devices.Interfaces
				for _, iface := range targetInterfaces {
					if iface.Name == srcInterface.Name {
						targetInterface = iface.DeepCopy()
						break
					}
				}
				Expect(targetInterface).ToNot(BeNil(), fmt.Sprintf("clone target does not have interface with name %s", srcInterface.Name))

				By("Making sure new mac address is applied to target VM")
				Expect(targetInterface.MacAddress).ToNot(Equal(srcInterface.MacAddress))

				expectSpecsToEqualExceptForMacAddress(targetVM, sourceVM)
				expectEqualLabels(targetVM, sourceVM)
				expectEqualAnnotations(targetVM, sourceVM)
			})

			Context("regarding domain Firmware", func() {
				It("clone with changed SMBios serial", func() {
					const sourceSerial = "source-serial"
					const targetSerial = "target-serial"

					sourceVM = createVM(
						func(vmi *virtv1.VirtualMachineInstance) {
							vmi.Spec.Domain.Firmware = &virtv1.Firmware{Serial: sourceSerial}
						},
					)

					vmClone = generateCloneFromVM()
					vmClone.Spec.NewSMBiosSerial = pointer.String(targetSerial)

					createCloneAndWaitForFinish(vmClone)

					By(fmt.Sprintf("Getting the target VM %s", targetVMName))
					targetVM, err = virtClient.VirtualMachine(sourceVM.Namespace).Get(targetVMName, &v1.GetOptions{})
					Expect(err).ShouldNot(HaveOccurred())

					By("Making sure target is runnable")
					targetVM = expectVMRunnable(targetVM)

					By("Making sure new smBios serial is applied to target VM")
					Expect(targetVM.Spec.Template.Spec.Domain.Firmware).ToNot(BeNil())
					Expect(sourceVM.Spec.Template.Spec.Domain.Firmware).ToNot(BeNil())
					Expect(targetVM.Spec.Template.Spec.Domain.Firmware.Serial).ToNot(Equal(sourceVM.Spec.Template.Spec.Domain.Firmware.Serial))

					expectEqualLabels(targetVM, sourceVM)
					expectEqualAnnotations(targetVM, sourceVM)
				})

				It("[QUARANTINE] should strip firmware UUID", func() {
					const fakeFirmwareUUID = "fake-uuid"

					sourceVM = createVM(
						func(vmi *virtv1.VirtualMachineInstance) {
							vmi.Spec.Domain.Firmware = &virtv1.Firmware{UUID: fakeFirmwareUUID}
						},
					)
					vmClone = generateCloneFromVM()

					createCloneAndWaitForFinish(vmClone)

					By(fmt.Sprintf("Getting the target VM %s", targetVMName))
					targetVM, err = virtClient.VirtualMachine(sourceVM.Namespace).Get(targetVMName, &v1.GetOptions{})
					Expect(err).ShouldNot(HaveOccurred())

					By("Making sure target is runnable")
					targetVM = expectVMRunnable(targetVM)

					By("Making sure new smBios serial is applied to target VM")
					Expect(targetVM.Spec.Template.Spec.Domain.Firmware).ToNot(BeNil())
					Expect(sourceVM.Spec.Template.Spec.Domain.Firmware).ToNot(BeNil())
					Expect(targetVM.Spec.Template.Spec.Domain.Firmware.UUID).ToNot(Equal(sourceVM.Spec.Template.Spec.Domain.Firmware.UUID))
				})
			})

		})

		Context("with more complicated VM", func() {

			expectVMRunnable := func(vm *virtv1.VirtualMachine) *virtv1.VirtualMachine {
				return expectVMRunnable(vm, console.LoginToAlpine)
			}

			createVMWithStorageClass := func(storageClass string, running bool) *virtv1.VirtualMachine {
				vm := NewRandomVMWithDataVolumeWithRegistryImport(
					cd.DataVolumeImportUrlForContainerDisk(cd.ContainerDiskAlpine),
					util.NamespaceTestDefault,
					storageClass,
					k8sv1.ReadWriteOnce,
				)
				vm.Spec.Running = pointer.Bool(running)

				vm, err := virtClient.VirtualMachine(vm.Namespace).Create(vm)
				Expect(err).ToNot(HaveOccurred())

				for _, dvt := range vm.Spec.DataVolumeTemplates {
					libstorage.EventuallyDVWith(vm.Namespace, dvt.Name, 180, HaveSucceeded())
				}

				return vm
			}

			It("with a simple clone", func() {
				snapshotStorageClass, err := libstorage.GetSnapshotStorageClass(virtClient)
				Expect(err).ToNot(HaveOccurred())

				if snapshotStorageClass == "" {
					Skip("Skipping test, no VolumeSnapshot support")
				}

				sourceVM = createVMWithStorageClass(snapshotStorageClass, false)
				vmClone = generateCloneFromVM()

				createCloneAndWaitForFinish(vmClone)

				By(fmt.Sprintf("Getting the target VM %s", targetVMName))
				targetVM, err = virtClient.VirtualMachine(sourceVM.Namespace).Get(targetVMName, &v1.GetOptions{})
				Expect(err).ShouldNot(HaveOccurred())

				By("Making sure target is runnable")
				targetVM = expectVMRunnable(targetVM)

				expectEqualLabels(targetVM, sourceVM)
				expectEqualAnnotations(targetVM, sourceVM)
			})

			Context("should reject source with non snapshotable volume", func() {
				BeforeEach(func() {
					sc := libstorage.GetNoVolumeSnapshotStorageClass("local")
					if sc == "" {
						Skip("Skipping test, no storage class without snapshot support")
					}

					// create running in case storage is WFFC (local storage)
					By("Creating source VM")
					sourceVM = createVMWithStorageClass(sc, true)
					sourceVM, err = virtClient.VirtualMachine(sourceVM.Namespace).Get(sourceVM.Name, &v1.GetOptions{})
					Expect(err).ToNot(HaveOccurred())
					sourceVM = StopVirtualMachine(sourceVM)
				})

				It("with VM source", func() {
					vmClone = generateCloneFromVM()
					vmClone, err = virtClient.VirtualMachineClone(vmClone.Namespace).Create(context.Background(), vmClone, v1.CreateOptions{})
					Expect(err).To(HaveOccurred())
					Expect(err.Error()).Should(ContainSubstring("does not support snapshots"))
				})

				It("with snapshot source", func() {
					By("Snapshotting VM")
					snapshot := createSnapshot(sourceVM)
					snapshot = waitSnapshotReady(snapshot)

					By("Deleting VM")
					err = virtClient.VirtualMachine(sourceVM.Namespace).Delete(sourceVM.Name, &v1.DeleteOptions{})
					Expect(err).ToNot(HaveOccurred())

					By("Creating a clone and expecting error")
					vmClone = generateCloneFromSnapshot(snapshot, targetVMName)
					vmClone, err = virtClient.VirtualMachineClone(vmClone.Namespace).Create(context.Background(), vmClone, v1.CreateOptions{})
					Expect(err).Should(HaveOccurred())
					Expect(err.Error()).To(ContainSubstring("not backed up in snapshot"))
				})
			})

			Context("with instancetype and preferences", func() {
				var (
					instancetype *instancetypev1alpha2.VirtualMachineInstancetype
					preference   *instancetypev1alpha2.VirtualMachinePreference
				)

				BeforeEach(func() {
					snapshotStorageClass, err := libstorage.GetSnapshotStorageClass(virtClient)
					Expect(err).ToNot(HaveOccurred())

					if snapshotStorageClass == "" {
						Skip("Skiping test, no VolumeSnapshot support")
					}

					instancetype = &instancetypev1alpha2.VirtualMachineInstancetype{
						ObjectMeta: metav1.ObjectMeta{
							GenerateName: "vm-instancetype-",
							Namespace:    util.NamespaceTestDefault,
						},
						Spec: instancetypev1alpha2.VirtualMachineInstancetypeSpec{
							CPU: &instancetypev1alpha2.CPUInstancetype{
								Guest: 1,
							},
							Memory: &instancetypev1alpha2.MemoryInstancetype{
								Guest: resource.MustParse("128Mi"),
							},
						},
					}
					instancetype, err := virtClient.VirtualMachineInstancetype(util.NamespaceTestDefault).Create(context.Background(), instancetype, metav1.CreateOptions{})
					Expect(err).ToNot(HaveOccurred())

					preference = &instancetypev1alpha2.VirtualMachinePreference{
						ObjectMeta: metav1.ObjectMeta{
							GenerateName: "vm-preference-",
							Namespace:    util.NamespaceTestDefault,
						},
						Spec: instancetypev1alpha2.VirtualMachinePreferenceSpec{
							CPU: &instancetypev1alpha2.CPUPreferences{
								PreferredCPUTopology: instancetypev1alpha2.PreferSockets,
							},
						},
					}
					preference, err := virtClient.VirtualMachinePreference(util.NamespaceTestDefault).Create(context.Background(), preference, metav1.CreateOptions{})
					Expect(err).ToNot(HaveOccurred())

					sourceVM = NewRandomVMWithDataVolumeWithRegistryImport(
						cd.DataVolumeImportUrlForContainerDisk(cd.ContainerDiskAlpine),
						util.NamespaceTestDefault,
						snapshotStorageClass,
						k8sv1.ReadWriteOnce,
					)

					sourceVM.Spec.Template.Spec.Domain.Resources = virtv1.ResourceRequirements{}
					sourceVM.Spec.Instancetype = &virtv1.InstancetypeMatcher{
						Name: instancetype.Name,
						Kind: "VirtualMachineInstanceType",
					}
					sourceVM.Spec.Preference = &virtv1.PreferenceMatcher{
						Name: preference.Name,
						Kind: "VirtualMachinePreference",
					}

					sourceVM, err = virtClient.VirtualMachine(sourceVM.Namespace).Create(sourceVM)
					Expect(err).ToNot(HaveOccurred())

					for _, dvt := range sourceVM.Spec.DataVolumeTemplates {
						libstorage.EventuallyDVWith(sourceVM.Namespace, dvt.Name, 180, HaveSucceeded())
					}
				})

				It("should create new ControllerRevisions for cloned VM", Label("instancetype", "clone"), func() {
					By("Waiting until the source VM has instancetype and preference RevisionNames")
					libinstancetype.WaitForVMInstanceTypeRevisionNames(sourceVM.Name, virtClient)

					vmClone = generateCloneFromVM()
					createCloneAndWaitForFinish(vmClone)

					By("Waiting until the targetVM has instancetype and preference RevisionNames")
					libinstancetype.WaitForVMInstanceTypeRevisionNames(targetVMName, virtClient)

					By("Asserting that the targetVM has new instancetype and preference controllerRevisions")
					sourceVM, err := virtClient.VirtualMachine(util.NamespaceTestDefault).Get(sourceVM.Name, &metav1.GetOptions{})
					Expect(err).ToNot(HaveOccurred())
					targetVM, err := virtClient.VirtualMachine(util.NamespaceTestDefault).Get(targetVMName, &metav1.GetOptions{})
					Expect(err).ToNot(HaveOccurred())

					Expect(targetVM.Spec.Instancetype.RevisionName).ToNot(Equal(sourceVM.Spec.Instancetype.RevisionName), "source and target instancetype revision names should not be equal")
					Expect(targetVM.Spec.Preference.RevisionName).ToNot(Equal(sourceVM.Spec.Preference.RevisionName), "source and target preference revision names should not be equal")

					By("Asserting that the source and target ControllerRevisions contain the same Object")
					Expect(libinstancetype.EnsureControllerRevisionObjectsEqual(sourceVM.Spec.Instancetype.RevisionName, targetVM.Spec.Instancetype.RevisionName, virtClient)).To(BeTrue(), "source and target instance type controller revisions are expected to be equal")
					Expect(libinstancetype.EnsureControllerRevisionObjectsEqual(sourceVM.Spec.Preference.RevisionName, targetVM.Spec.Preference.RevisionName, virtClient)).To(BeTrue(), "source and target preference controller revisions are expected to be equal")
				})
			})
		})
	})
})
