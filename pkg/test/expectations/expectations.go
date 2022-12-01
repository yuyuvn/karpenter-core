/*
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

//nolint:revive
package expectations

import (
	"context"
	"fmt"
	"math/rand"
	"reflect"
	"sync"
	"time"

	"github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega" //nolint:revive,stylecheck
	prometheus "github.com/prometheus/client_model/go"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/api/policy/v1beta1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"knative.dev/pkg/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/apis/provisioning/v1alpha5"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning/scheduling"
	corecontroller "github.com/aws/karpenter-core/pkg/operator/controller"
	"github.com/aws/karpenter-core/pkg/test"
)

const (
	ReconcilerPropagationTime = 10 * time.Second
	RequestInterval           = 1 * time.Second
)

func ExpectExists[T client.Object](ctx context.Context, c client.Client, obj T) T {
	return ExpectExistsWithOffset(1, ctx, c, obj)
}

func ExpectExistsWithOffset[T client.Object](offset int, ctx context.Context, c client.Client, obj T) T {
	resp := reflect.New(reflect.TypeOf(*new(T)).Elem()).Interface().(T)
	ExpectWithOffset(offset+1, c.Get(ctx, client.ObjectKeyFromObject(obj), resp)).To(Succeed())
	return resp
}

func ExpectPodExists(ctx context.Context, c client.Client, name string, namespace string) *v1.Pod {
	return ExpectPodExistsWithOffset(1, ctx, c, name, namespace)
}

func ExpectPodExistsWithOffset(offset int, ctx context.Context, c client.Client, name string, namespace string) *v1.Pod {
	return ExpectExistsWithOffset(offset+1, ctx, c, &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}})
}

func ExpectNodeExists(ctx context.Context, c client.Client, name string) *v1.Node {
	return ExpectNodeExistsWithOffset(1, ctx, c, name)
}

func ExpectNodeExistsWithOffset(offset int, ctx context.Context, c client.Client, name string) *v1.Node {
	return ExpectExistsWithOffset(offset+1, ctx, c, &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}})
}

func ExpectNotFound(ctx context.Context, c client.Client, objects ...client.Object) {
	ExpectNotFoundWithOffset(1, ctx, c, objects...)
}

func ExpectNotFoundWithOffset(offset int, ctx context.Context, c client.Client, objects ...client.Object) {
	for _, object := range objects {
		EventuallyWithOffset(offset+1, func() bool {
			return errors.IsNotFound(c.Get(ctx, types.NamespacedName{Name: object.GetName(), Namespace: object.GetNamespace()}, object))
		}, ReconcilerPropagationTime, RequestInterval).Should(BeTrue(), func() string {
			return fmt.Sprintf("expected %s to be deleted, but it still exists", client.ObjectKeyFromObject(object))
		})
	}
}

func ExpectScheduled(ctx context.Context, c client.Client, pod *v1.Pod) *v1.Node {
	p := ExpectPodExistsWithOffset(1, ctx, c, pod.Name, pod.Namespace)
	Expect(p.Spec.NodeName).ToNot(BeEmpty(), fmt.Sprintf("expected %s/%s to be scheduled", pod.Namespace, pod.Name))
	return ExpectNodeExistsWithOffset(1, ctx, c, p.Spec.NodeName)
}

func ExpectNotScheduled(ctx context.Context, c client.Client, pod *v1.Pod) *v1.Pod {
	p := ExpectPodExistsWithOffset(1, ctx, c, pod.Name, pod.Namespace)
	EventuallyWithOffset(1, p.Spec.NodeName).Should(BeEmpty(), fmt.Sprintf("expected %s/%s to not be scheduled", pod.Namespace, pod.Name))
	return p
}

func ExpectApplied(ctx context.Context, c client.Client, objects ...client.Object) {
	ExpectAppliedWithOffset(1, ctx, c, objects...)
}

func ExpectAppliedWithOffset(offset int, ctx context.Context, c client.Client, objects ...client.Object) {
	for _, object := range objects {
		current := object.DeepCopyObject().(client.Object)
		statuscopy := object.DeepCopyObject().(client.Object) // Snapshot the status, since create/update may override
		deletecopy := object.DeepCopyObject().(client.Object) // Snapshot the status, since create/update may override
		// Create or Update
		if err := c.Get(ctx, client.ObjectKeyFromObject(current), current); err != nil {
			if errors.IsNotFound(err) {
				ExpectWithOffset(offset+1, c.Create(ctx, object)).To(Succeed())
			} else {
				ExpectWithOffset(offset+1, err).ToNot(HaveOccurred())
			}
		} else {
			object.SetResourceVersion(current.GetResourceVersion())
			ExpectWithOffset(offset+1, c.Update(ctx, object)).To(Succeed())
		}
		// Update status
		statuscopy.SetResourceVersion(object.GetResourceVersion())
		ExpectWithOffset(offset+1, c.Status().Update(ctx, statuscopy)).To(Or(Succeed(), MatchError("the server could not find the requested resource"))) // Some objects do not have a status
		// Delete if timestamp set
		if deletecopy.GetDeletionTimestamp() != nil {
			ExpectWithOffset(offset+1, c.Delete(ctx, deletecopy)).To(Succeed())
		}
	}
}

func ExpectDeleted(ctx context.Context, c client.Client, objects ...client.Object) {
	for _, object := range objects {
		if err := c.Delete(ctx, object, &client.DeleteOptions{GracePeriodSeconds: ptr.Int64(0)}); !errors.IsNotFound(err) {
			ExpectWithOffset(1, err).To(BeNil())
		}
		ExpectNotFoundWithOffset(1, ctx, c, object)
	}
}

func ExpectCleanedUp(ctx context.Context, c client.Client) {
	wg := sync.WaitGroup{}
	namespaces := &v1.NamespaceList{}
	ExpectWithOffset(1, c.List(ctx, namespaces)).To(Succeed())
	nodes := &v1.NodeList{}
	ExpectWithOffset(1, c.List(ctx, nodes)).To(Succeed())
	for i := range nodes.Items {
		nodes.Items[i].SetFinalizers([]string{})
		ExpectWithOffset(1, c.Update(ctx, &nodes.Items[i])).To(Succeed())
	}
	for _, object := range []client.Object{
		&v1.Pod{},
		&v1.Node{},
		&appsv1.DaemonSet{},
		&v1beta1.PodDisruptionBudget{},
		&v1.PersistentVolumeClaim{},
		&v1.PersistentVolume{},
		&storagev1.StorageClass{},
		&v1alpha5.Provisioner{},
	} {
		for _, namespace := range namespaces.Items {
			wg.Add(1)
			go func(object client.Object, namespace string) {
				defer wg.Done()
				defer ginkgo.GinkgoRecover()
				ExpectWithOffset(1, c.DeleteAllOf(ctx, object, client.InNamespace(namespace),
					&client.DeleteAllOfOptions{DeleteOptions: client.DeleteOptions{GracePeriodSeconds: ptr.Int64(0)}})).ToNot(HaveOccurred())
			}(object, namespace.Name)
		}
	}
	wg.Wait()
}

func ExpectFinalizersRemoved(ctx context.Context, c client.Client, objects ...client.Object) {
	for _, object := range objects {
		ExpectWithOffset(1, c.Get(ctx, client.ObjectKeyFromObject(object), object)).To(Succeed())
		mergeFrom := client.MergeFrom(object.DeepCopyObject().(client.Object))
		object.SetFinalizers([]string{})
		ExpectWithOffset(1, c.Patch(ctx, object, mergeFrom)).To(Succeed())
	}
}

func ExpectProvisioned(ctx context.Context, c client.Client, recorder *test.EventRecorder, controller corecontroller.Controller,
	provisioner *provisioning.Provisioner, pods ...*v1.Pod) (result []*v1.Pod) {

	ExpectProvisionedNoBindingWithOffset(1, ctx, c, controller, provisioner, pods...)

	recorder.ForEachBinding(func(pod *v1.Pod, node *v1.Node) {
		ExpectManualBindingWithOffset(1, ctx, c, pod, node)
	})
	// reset bindings, so we don't try to bind these same pods again if a new provisioning is performed in the same test
	recorder.ResetBindings()

	// Update objects after reconciling
	for _, pod := range pods {
		result = append(result, ExpectPodExistsWithOffset(1, ctx, c, pod.GetName(), pod.GetNamespace()))
	}
	return
}

func ExpectProvisionedNoBinding(ctx context.Context, c client.Client, controller corecontroller.Controller, provisioner *provisioning.Provisioner, pods ...*v1.Pod) (result []*v1.Pod) {
	return ExpectProvisionedNoBindingWithOffset(1, ctx, c, controller, provisioner, pods...)
}

func ExpectProvisionedNoBindingWithOffset(offset int, ctx context.Context, c client.Client, controller corecontroller.Controller, provisioner *provisioning.Provisioner, pods ...*v1.Pod) (result []*v1.Pod) {
	// Persist objects
	for _, pod := range pods {
		ExpectAppliedWithOffset(offset+1, ctx, c, pod)
	}

	// shuffle the pods to try to detect any issues where we rely on pod order within a batch, we shuffle a copy of
	// the slice so we can return the provisioned pods in the same order that the test supplied them for consistency
	unorderedPods := append([]*v1.Pod{}, pods...)
	r := rand.New(rand.NewSource(ginkgo.GinkgoRandomSeed())) //nolint
	r.Shuffle(len(unorderedPods), func(i, j int) { unorderedPods[i], unorderedPods[j] = unorderedPods[j], unorderedPods[i] })
	for _, pod := range unorderedPods {
		_, _ = controller.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(pod)})
	}

	// TODO: Check the error on the provisioner reconcile
	_, _ = provisioner.Reconcile(ctx, reconcile.Request{})

	// Update objects after reconciling
	for _, pod := range pods {
		result = append(result, ExpectPodExistsWithOffset(offset+1, ctx, c, pod.GetName(), pod.GetNamespace()))
	}
	return
}

func ExpectReconcileSucceeded(ctx context.Context, reconciler reconcile.Reconciler, key client.ObjectKey) reconcile.Result {
	result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
	ExpectWithOffset(1, err).ToNot(HaveOccurred())
	return result
}

func ExpectReconcileFailed(ctx context.Context, reconciler reconcile.Reconciler, key client.ObjectKey) {
	result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
	ExpectWithOffset(1, err).ToNot(Succeed(), fmt.Sprintf("got result, %v", result))
}

func ExpectMetric(prefix string) *prometheus.MetricFamily {
	metrics, err := metrics.Registry.Gather()
	ExpectWithOffset(1, err).To(BeNil())
	var selected *prometheus.MetricFamily
	for _, mf := range metrics {
		if mf.GetName() == prefix {
			selected = mf
		}
	}
	ExpectWithOffset(1, selected).ToNot(BeNil(), fmt.Sprintf("expected to find a '%s' metric", prefix))
	return selected
}

func ExpectManualBinding(ctx context.Context, c client.Client, pod *v1.Pod, node *v1.Node) {
	ExpectManualBindingWithOffset(1, ctx, c, pod, node)
}

func ExpectManualBindingWithOffset(offset int, ctx context.Context, c client.Client, pod *v1.Pod, node *v1.Node) {
	ExpectWithOffset(offset+1, c.Create(ctx, &v1.Binding{
		TypeMeta: pod.TypeMeta,
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.ObjectMeta.Name,
			Namespace: pod.ObjectMeta.Namespace,
			UID:       pod.ObjectMeta.UID,
		},
		Target: v1.ObjectReference{
			Name: node.Name,
		},
	})).To(Succeed())
}

func ExpectSkew(ctx context.Context, c client.Client, namespace string, constraint *v1.TopologySpreadConstraint) Assertion {
	nodes := &v1.NodeList{}
	ExpectWithOffset(1, c.List(ctx, nodes)).To(Succeed())
	pods := &v1.PodList{}
	ExpectWithOffset(1, c.List(ctx, pods, scheduling.TopologyListOptions(namespace, constraint.LabelSelector))).To(Succeed())
	skew := map[string]int{}
	for i, pod := range pods.Items {
		if scheduling.IgnoredForTopology(&pods.Items[i]) {
			continue
		}
		for _, node := range nodes.Items {
			if pod.Spec.NodeName == node.Name {
				switch constraint.TopologyKey {
				case v1.LabelHostname:
					skew[node.Name]++ // Check node name since hostname labels aren't applied
				default:
					if key, ok := node.Labels[constraint.TopologyKey]; ok {
						skew[key]++
					}
				}
			}
		}
	}
	return ExpectWithOffset(1, skew)
}

// ExpectPanic is a function that should be deferred at the beginning of a test like "defer ExpectPanic()"
// It asserts that the test should panic
func ExpectPanic() {
	ExpectWithOffset(1, recover()).ToNot(BeNil())
}

type gomegaKeyType struct{}

var gomegaKey = gomegaKeyType{}

func GomegaWithContext(ctx context.Context, g Gomega) context.Context {
	return context.WithValue(ctx, gomegaKey, g)
}

func GomegaFromContext(ctx context.Context) Gomega {
	data := ctx.Value(gomegaKey)
	if data == nil {
		panic("Gomega not defined in context")
	}
	return data.(Gomega)
}
