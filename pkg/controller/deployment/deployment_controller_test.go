/*
Copyright 2018 Pusher Ltd.

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

package deployment

import (
	"context"
	"fmt"
	"sync"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/pusher/wave/pkg/core"
	"github.com/pusher/wave/test/utils"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var _ = Describe("Deployment controller Suite", func() {
	var c client.Client
	var m utils.Matcher

	var deployment *appsv1.Deployment
	var requests <-chan reconcile.Request
	var mgrStopped *sync.WaitGroup
	var stopMgr chan struct{}

	const timeout = time.Second * 5
	const consistentlyTimeout = time.Second

	var ownerRef metav1.OwnerReference
	var cm1 *corev1.ConfigMap
	var cm2 *corev1.ConfigMap
	var s1 *corev1.Secret
	var s2 *corev1.Secret

	var waitForDeploymentReconciled = func(obj core.Object) {
		request := reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      obj.GetName(),
				Namespace: obj.GetNamespace(),
			},
		}
		// wait for reconcile for creating the Deployment
		Eventually(requests, timeout).Should(Receive(Equal(request)))
	}

	BeforeEach(func() {
		mgr, err := manager.New(cfg, manager.Options{})
		Expect(err).NotTo(HaveOccurred())
		c = mgr.GetClient()
		m = utils.Matcher{Client: c}

		var recFn reconcile.Reconciler
		recFn, requests = SetupTestReconcile(newReconciler(mgr))
		Expect(add(mgr, recFn)).NotTo(HaveOccurred())

		stopMgr, mgrStopped = StartTestManager(mgr)

		// Create some configmaps and secrets
		cm1 = utils.ExampleConfigMap1.DeepCopy()
		cm2 = utils.ExampleConfigMap2.DeepCopy()
		s1 = utils.ExampleSecret1.DeepCopy()
		s2 = utils.ExampleSecret2.DeepCopy()

		m.Create(cm1).Should(Succeed())
		m.Create(cm2).Should(Succeed())
		m.Create(s1).Should(Succeed())
		m.Create(s2).Should(Succeed())
		m.Get(cm1, timeout).Should(Succeed())
		m.Get(cm2, timeout).Should(Succeed())
		m.Get(s1, timeout).Should(Succeed())
		m.Get(s2, timeout).Should(Succeed())

		deployment = utils.ExampleDeployment.DeepCopy()

		// Create a deployment and wait for it to be reconciled
		m.Create(deployment).Should(Succeed())
		waitForDeploymentReconciled(deployment)

		ownerRef = utils.GetOwnerRef(deployment)
	})

	AfterEach(func() {
		// Make sure to delete any finalizers (if the deployment exists)
		Eventually(func() error {
			key := types.NamespacedName{Namespace: deployment.GetNamespace(), Name: deployment.GetName()}
			err := c.Get(context.TODO(), key, deployment)
			if err != nil && errors.IsNotFound(err) {
				return nil
			}
			if err != nil {
				return err
			}
			deployment.SetFinalizers([]string{})
			return c.Update(context.TODO(), deployment)
		}, timeout).Should(Succeed())

		Eventually(func() error {
			key := types.NamespacedName{Namespace: deployment.GetNamespace(), Name: deployment.GetName()}
			err := c.Get(context.TODO(), key, deployment)
			if err != nil && errors.IsNotFound(err) {
				return nil
			}
			if err != nil {
				return err
			}
			if len(deployment.GetFinalizers()) > 0 {
				return fmt.Errorf("Finalizers not upated")
			}
			return nil
		}, timeout).Should(Succeed())

		close(stopMgr)
		mgrStopped.Wait()

		utils.DeleteAll(cfg, timeout,
			&appsv1.DeploymentList{},
			&corev1.ConfigMapList{},
			&corev1.SecretList{},
			&corev1.EventList{},
		)
	})

	Context("When a Deployment is reconciled", func() {
		Context("And it has the required annotation", func() {
			BeforeEach(func() {
				annotations := deployment.GetAnnotations()
				if annotations == nil {
					annotations = make(map[string]string)
				}
				annotations[core.RequiredAnnotation] = "true"
				deployment.SetAnnotations(annotations)

				m.Update(deployment).Should(Succeed())
				waitForDeploymentReconciled(deployment)

				// Get the updated Deployment
				m.Get(deployment, timeout).Should(Succeed())
			})

			It("Adds OwnerReferences to all children", func() {
				for _, obj := range []core.Object{cm1, cm2, s1, s2} {
					m.Eventually(obj, timeout).Should(utils.WithOwnerReferences(ContainElement(ownerRef)))
				}
			})

			It("Adds a finalizer to the Deployment", func() {
				m.Eventually(deployment, timeout).Should(utils.WithFinalizers(ContainElement(core.FinalizerString)))
			})

			It("Adds a config hash to the Pod Template", func() {
				m.Eventually(deployment, timeout).Should(utils.WithPodTemplateAnnotations(HaveKey(core.ConfigHashAnnotation)))
			})

			It("Sends an event when updating the hash", func() {
				m.Eventually(deployment, timeout).Should(utils.WithPodTemplateAnnotations(HaveKey(core.ConfigHashAnnotation)))

				events := &corev1.EventList{}
				eventMessage := func(event *corev1.Event) string {
					return event.Message
				}

				hashMessage := "Configuration hash updated to 198df8455a4fd702fc0c7fdfa4bdb213363b96240bfd48b7b098d936499315a1"
				m.Eventually(events, timeout).Should(utils.WithItems(ContainElement(WithTransform(eventMessage, Equal(hashMessage)))))
			})

			Context("And a child is removed", func() {
				var originalHash string
				BeforeEach(func() {
					m.Eventually(deployment, timeout).Should(utils.WithPodTemplateAnnotations(HaveKey(core.ConfigHashAnnotation)))
					originalHash = deployment.Spec.Template.GetAnnotations()[core.ConfigHashAnnotation]

					// Remove "container2" which references Secret example2 and ConfigMap
					// example2
					containers := deployment.Spec.Template.Spec.Containers
					Expect(containers[0].Name).To(Equal("container1"))
					deployment.Spec.Template.Spec.Containers = []corev1.Container{containers[0]}
					m.Update(deployment).Should(Succeed())
					waitForDeploymentReconciled(deployment)

					// Get the updated Deployment
					m.Get(deployment, timeout).Should(Succeed())
				})

				It("Removes the OwnerReference from the orphaned ConfigMap", func() {
					m.Eventually(cm2, timeout).ShouldNot(utils.WithOwnerReferences(ContainElement(ownerRef)))
				})

				It("Removes the OwnerReference from the orphaned Secret", func() {
					m.Eventually(s2, timeout).ShouldNot(utils.WithOwnerReferences(ContainElement(ownerRef)))
				})

				It("Updates the config hash in the Pod Template", func() {
					m.Eventually(deployment, timeout).ShouldNot(utils.WithAnnotations(HaveKeyWithValue(core.ConfigHashAnnotation, originalHash)))
				})
			})

			Context("And a child is updated", func() {
				var originalHash string

				BeforeEach(func() {
					m.Eventually(deployment, timeout).Should(utils.WithPodTemplateAnnotations(HaveKey(core.ConfigHashAnnotation)))
					originalHash = deployment.Spec.Template.GetAnnotations()[core.ConfigHashAnnotation]
				})

				Context("A ConfigMap volume is updated", func() {
					BeforeEach(func() {
						m.Get(cm1, timeout).Should(Succeed())
						cm1.Data["key1"] = "modified"
						m.Update(cm1).Should(Succeed())

						waitForDeploymentReconciled(deployment)

						// Get the updated Deployment
						m.Get(deployment, timeout).Should(Succeed())
					})

					It("Updates the config hash in the Pod Template", func() {
						m.Eventually(deployment, timeout).ShouldNot(utils.WithAnnotations(HaveKeyWithValue(core.ConfigHashAnnotation, originalHash)))
					})
				})

				Context("A ConfigMap EnvSource is updated", func() {
					BeforeEach(func() {
						m.Get(cm2, timeout).Should(Succeed())
						cm2.Data["key1"] = "modified"
						m.Update(cm2).Should(Succeed())

						waitForDeploymentReconciled(deployment)

						// Get the updated Deployment
						m.Get(deployment, timeout).Should(Succeed())
					})

					It("Updates the config hash in the Pod Template", func() {
						m.Eventually(deployment, timeout).ShouldNot(utils.WithAnnotations(HaveKeyWithValue(core.ConfigHashAnnotation, originalHash)))
					})
				})

				Context("A Secret volume is updated", func() {
					BeforeEach(func() {
						m.Get(s1, timeout).Should(Succeed())
						if s1.StringData == nil {
							s1.StringData = make(map[string]string)
						}
						s1.StringData["key1"] = "modified"
						m.Update(s1).Should(Succeed())

						waitForDeploymentReconciled(deployment)

						// Get the updated Deployment
						m.Get(deployment, timeout).Should(Succeed())
					})

					It("Updates the config hash in the Pod Template", func() {
						m.Eventually(deployment, timeout).ShouldNot(utils.WithAnnotations(HaveKeyWithValue(core.ConfigHashAnnotation, originalHash)))
					})
				})

				Context("A Secret EnvSource is updated", func() {
					BeforeEach(func() {
						m.Get(s2, timeout).Should(Succeed())
						if s2.StringData == nil {
							s2.StringData = make(map[string]string)
						}
						s2.StringData["key1"] = "modified"
						m.Update(s2).Should(Succeed())

						waitForDeploymentReconciled(deployment)

						// Get the updated Deployment
						m.Get(deployment, timeout).Should(Succeed())
					})

					It("Updates the config hash in the Pod Template", func() {
						m.Eventually(deployment, timeout).ShouldNot(utils.WithAnnotations(HaveKeyWithValue(core.ConfigHashAnnotation, originalHash)))
					})
				})
			})

			Context("And the annotation is removed", func() {
				BeforeEach(func() {
					m.Get(deployment, timeout).Should(Succeed())
					deployment.SetAnnotations(make(map[string]string))
					m.Update(deployment).Should(Succeed())
					waitForDeploymentReconciled(deployment)

					m.Eventually(deployment, timeout).ShouldNot(utils.WithAnnotations(HaveKey(core.RequiredAnnotation)))
				})

				It("Removes the OwnerReference from the all children", func() {
					for _, obj := range []core.Object{cm1, cm2, s1, s2} {
						m.Eventually(obj, timeout).ShouldNot(utils.WithOwnerReferences(ContainElement(ownerRef)))
					}
				})

				It("Removes the Deployment's finalizer", func() {
					m.Eventually(deployment, timeout).ShouldNot(utils.WithFinalizers(ContainElement(core.FinalizerString)))
				})
			})

			Context("And is deleted", func() {
				BeforeEach(func() {
					// Make sure the cache has synced before we run the test
					m.Eventually(deployment, timeout).Should(utils.WithPodTemplateAnnotations(HaveKey(core.ConfigHashAnnotation)))
					m.Delete(deployment).Should(Succeed())
					m.Eventually(deployment, timeout).ShouldNot(utils.WithDeletionTimestamp(BeNil()))
					waitForDeploymentReconciled(deployment)

					// Get the updated Deployment
					m.Get(deployment, timeout).Should(Succeed())
				})
				It("Removes the OwnerReference from the all children", func() {
					for _, obj := range []core.Object{cm1, cm2, s1, s2} {
						m.Eventually(obj, timeout).ShouldNot(utils.WithOwnerReferences(ContainElement(ownerRef)))
					}
				})

				It("Removes the Deployment's finalizer", func() {
					// Removing the finalizer causes the deployment to be deleted
					m.Get(deployment, timeout).ShouldNot(Succeed())
				})
			})
		})

		Context("And it does not have the required annotation", func() {
			BeforeEach(func() {
				// Get the updated Deployment
				m.Get(deployment, timeout).Should(Succeed())
			})

			It("Doesn't add any OwnerReferences to any children", func() {
				for _, obj := range []core.Object{cm1, cm2, s1, s2} {
					m.Consistently(obj, consistentlyTimeout).ShouldNot(utils.WithOwnerReferences(ContainElement(ownerRef)))
				}
			})

			It("Doesn't add a finalizer to the Deployment", func() {
				m.Consistently(deployment, consistentlyTimeout).ShouldNot(utils.WithFinalizers(ContainElement(core.FinalizerString)))
			})

			It("Doesn't add a config hash to the Pod Template", func() {
				m.Consistently(deployment, consistentlyTimeout).ShouldNot(utils.WithAnnotations(ContainElement(core.ConfigHashAnnotation)))
			})
		})
	})

})
