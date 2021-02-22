/*
Copyright 2019 The Kruise Authors.
Copyright 2016 The Kubernetes Authors.

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

package statefulset

import (
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"strconv"
	"testing"
	"time"

	apps "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
	"k8s.io/kubernetes/pkg/controller/history"
	utilpointer "k8s.io/utils/pointer"

	appsv1beta1 "github.com/openkruise/kruise/apis/apps/v1beta1"
)

// overlappingStatefulSets sorts a list of StatefulSets by creation timestamp, using their names as a tie breaker.
// Generally used to tie break between StatefulSets that have overlapping selectors.
type overlappingStatefulSets []*appsv1beta1.StatefulSet

func (o overlappingStatefulSets) Len() int { return len(o) }

func (o overlappingStatefulSets) Swap(i, j int) { o[i], o[j] = o[j], o[i] }

func (o overlappingStatefulSets) Less(i, j int) bool {
	if o[i].CreationTimestamp.Equal(&o[j].CreationTimestamp) {
		return o[i].Name < o[j].Name
	}
	return o[i].CreationTimestamp.Before(&o[j].CreationTimestamp)
}

func TestGetParentNameAndOrdinal(t *testing.T) {
	set := newStatefulSet(3)
	pod := newStatefulSetPod(set, 1)
	if parent, ordinal := getParentNameAndOrdinal(pod); parent != set.Name {
		t.Errorf("Extracted the wrong parent name expected %s found %s", set.Name, parent)
	} else if ordinal != 1 {
		t.Errorf("Extracted the wrong ordinal expected %d found %d", 1, ordinal)
	}
	pod.Name = "1-bar"
	if parent, ordinal := getParentNameAndOrdinal(pod); parent != "" {
		t.Error("Expected empty string for non-member Pod parent")
	} else if ordinal != -1 {
		t.Error("Expected -1 for non member Pod ordinal")
	}
}

func TestIsMemberOf(t *testing.T) {
	set := newStatefulSet(3)
	set2 := newStatefulSet(3)
	set2.Name = "foo2"
	pod := newStatefulSetPod(set, 1)
	if !isMemberOf(set, pod) {
		t.Error("isMemberOf retruned false negative")
	}
	if isMemberOf(set2, pod) {
		t.Error("isMemberOf returned false positive")
	}
}

func TestIdentityMatches(t *testing.T) {
	set := newStatefulSet(3)
	pod := newStatefulSetPod(set, 1)
	if !identityMatches(set, pod) {
		t.Error("Newly created Pod has a bad identity")
	}
	pod.Name = "foo"
	if identityMatches(set, pod) {
		t.Error("identity matches for a Pod with the wrong name")
	}
	pod = newStatefulSetPod(set, 1)
	pod.Namespace = ""
	if identityMatches(set, pod) {
		t.Error("identity matches for a Pod with the wrong namespace")
	}
	pod = newStatefulSetPod(set, 1)
	delete(pod.Labels, apps.StatefulSetPodNameLabel)
	if identityMatches(set, pod) {
		t.Error("identity matches for a Pod with the wrong statefulSetPodNameLabel")
	}
}

func TestStorageMatches(t *testing.T) {
	set := newStatefulSet(3)
	pod := newStatefulSetPod(set, 1)
	if !storageMatches(set, pod) {
		t.Error("Newly created Pod has a invalid stroage")
	}
	pod.Spec.Volumes = nil
	if storageMatches(set, pod) {
		t.Error("Pod with invalid Volumes has valid storage")
	}
	pod = newStatefulSetPod(set, 1)
	for i := range pod.Spec.Volumes {
		pod.Spec.Volumes[i].PersistentVolumeClaim = nil
	}
	if storageMatches(set, pod) {
		t.Error("Pod with invalid Volumes claim valid storage")
	}
	pod = newStatefulSetPod(set, 1)
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].PersistentVolumeClaim != nil {
			pod.Spec.Volumes[i].PersistentVolumeClaim.ClaimName = "foo"
		}
	}
	if storageMatches(set, pod) {
		t.Error("Pod with invalid Volumes claim valid storage")
	}
	pod = newStatefulSetPod(set, 1)
	pod.Name = "bar"
	if storageMatches(set, pod) {
		t.Error("Pod with invalid ordinal has valid storage")
	}
}

func TestUpdateIdentity(t *testing.T) {
	set := newStatefulSet(3)
	pod := newStatefulSetPod(set, 1)
	if !identityMatches(set, pod) {
		t.Error("Newly created Pod has a bad identity")
	}
	pod.Namespace = ""
	if identityMatches(set, pod) {
		t.Error("identity matches for a Pod with the wrong namespace")
	}
	updateIdentity(set, pod)
	if !identityMatches(set, pod) {
		t.Error("updateIdentity failed to update the Pods namespace")
	}
	delete(pod.Labels, apps.StatefulSetPodNameLabel)
	updateIdentity(set, pod)
	if !identityMatches(set, pod) {
		t.Error("updateIdentity failed to restore the statefulSetPodName label")
	}
}

func TestUpdateStorage(t *testing.T) {
	set := newStatefulSet(3)
	pod := newStatefulSetPod(set, 1)
	if !storageMatches(set, pod) {
		t.Error("Newly created Pod has a invalid stroage")
	}
	pod.Spec.Volumes = nil
	if storageMatches(set, pod) {
		t.Error("Pod with invalid Volumes has valid storage")
	}
	updateStorage(set, pod)
	if !storageMatches(set, pod) {
		t.Error("updateStorage failed to recreate volumes")
	}
	pod = newStatefulSetPod(set, 1)
	for i := range pod.Spec.Volumes {
		pod.Spec.Volumes[i].PersistentVolumeClaim = nil
	}
	if storageMatches(set, pod) {
		t.Error("Pod with invalid Volumes claim valid storage")
	}
	updateStorage(set, pod)
	if !storageMatches(set, pod) {
		t.Error("updateStorage failed to recreate volume claims")
	}
	pod = newStatefulSetPod(set, 1)
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].PersistentVolumeClaim != nil {
			pod.Spec.Volumes[i].PersistentVolumeClaim.ClaimName = "foo"
		}
	}
	if storageMatches(set, pod) {
		t.Error("Pod with invalid Volumes claim valid storage")
	}
	updateStorage(set, pod)
	if !storageMatches(set, pod) {
		t.Error("updateStorage failed to recreate volume claim names")
	}
}

func TestIsRunningAndReady(t *testing.T) {
	set := newStatefulSet(3)
	pod := newStatefulSetPod(set, 1)
	if isRunningAndReady(pod) {
		t.Error("isRunningAndReady does not respect Pod phase")
	}
	pod.Status.Phase = v1.PodRunning
	if isRunningAndReady(pod) {
		t.Error("isRunningAndReady does not respect Pod condition")
	}
	condition := v1.PodCondition{Type: v1.PodReady, Status: v1.ConditionTrue}
	podutil.UpdatePodCondition(&pod.Status, &condition)
	if !isRunningAndReady(pod) {
		t.Error("Pod should be running and ready")
	}
}

func TestGetMinReadySeconds(t *testing.T) {
	set := newStatefulSet(3)
	if getMinReadySeconds(set) != 0 {
		t.Error("getMinReadySeconds should be zero")
	}
	set.Spec.UpdateStrategy.RollingUpdate = &appsv1beta1.RollingUpdateStatefulSetStrategy{}
	if getMinReadySeconds(set) != 0 {
		t.Error("getMinReadySeconds should be zero")
	}
	set.Spec.UpdateStrategy.RollingUpdate.MinReadySeconds = utilpointer.Int32Ptr(3)
	if getMinReadySeconds(set) != 3 {
		t.Error("getMinReadySeconds should be 3")
	}
	set.Spec.UpdateStrategy.RollingUpdate.MinReadySeconds = utilpointer.Int32Ptr(30)
	if getMinReadySeconds(set) != 30 {
		t.Error("getMinReadySeconds should be 3")
	}
}

func TestIsRunningAndAvailable(t *testing.T) {
	set := newStatefulSet(3)
	pod := newStatefulSetPod(set, 1)
	if avail, wait := isRunningAndAvailable(pod, 0); avail || wait != 0 {
		t.Errorf("isRunningAndAvailable does not respect Pod phase, avail = %t, wait = %d", avail, wait)
	}
	pod.Status.Phase = v1.PodPending
	if avail, wait := isRunningAndAvailable(pod, 0); avail || wait != 0 {
		t.Errorf("isRunningAndAvailable does not respect Pod phase, avail = %t, wait = %d", avail, wait)
	}
	pod.Status.Phase = v1.PodRunning
	if avail, wait := isRunningAndAvailable(pod, 0); avail || wait != 0 {
		t.Errorf("isRunningAndAvailable does not respect Pod condition, avail = %t, wait = %d", avail, wait)
	}
	pod.Status.Conditions = []corev1.PodCondition{
		{
			Type:   corev1.PodReady,
			Status: corev1.ConditionTrue,
		},
	}
	if avail, wait := isRunningAndAvailable(pod, 0); !avail || wait != 0 {
		t.Errorf("isRunningAndAvailable does not respect 0 minReadySecond, avail = %t, wait = %d", avail, wait)
	}
	if avail, wait := isRunningAndAvailable(pod, 10); avail || wait != 10*time.Second {
		t.Errorf("isRunningAndAvailable does not respect non 0 minReadySecond, avail = %t, wait = %d", avail, wait)
	}
	pod.Status.Conditions[0].LastTransitionTime = metav1.NewTime(time.Now().Add(-5 * time.Second))
	if avail, wait := isRunningAndAvailable(pod, 10); avail || wait < 4*time.Second {
		t.Errorf("isRunningAndAvailable does not respect Pod condition last transaction, avail = %t, wait = %d",
			avail, wait)
	}
	if avail, wait := isRunningAndAvailable(pod, 3); !avail || wait != 0 {
		t.Errorf("isRunningAndAvailable does not respect Pod condition  last transaction, avail = %t, wait = %d",
			avail, wait)
	}
}

func TestAscendingOrdinal(t *testing.T) {
	set := newStatefulSet(10)
	pods := make([]*v1.Pod, 10)
	perm := rand.Perm(10)
	for i, v := range perm {
		pods[i] = newStatefulSetPod(set, v)
	}
	sort.Sort(ascendingOrdinal(pods))
	if !sort.IsSorted(ascendingOrdinal(pods)) {
		t.Error("ascendingOrdinal fails to sort Pods")
	}
}

func TestOverlappingStatefulSets(t *testing.T) {
	sets := make([]*appsv1beta1.StatefulSet, 10)
	perm := rand.Perm(10)
	for i, v := range perm {
		sets[i] = newStatefulSet(10)
		sets[i].CreationTimestamp = metav1.NewTime(sets[i].CreationTimestamp.Add(time.Duration(v) * time.Second))
	}
	sort.Sort(overlappingStatefulSets(sets))
	if !sort.IsSorted(overlappingStatefulSets(sets)) {
		t.Error("ascendingOrdinal fails to sort Pods")
	}
	for i, v := range perm {
		sets[i] = newStatefulSet(10)
		sets[i].Name = strconv.FormatInt(int64(v), 10)
	}
	sort.Sort(overlappingStatefulSets(sets))
	if !sort.IsSorted(overlappingStatefulSets(sets)) {
		t.Error("ascendingOrdinal fails to sort Pods")
	}
}

func TestNewPodControllerRef(t *testing.T) {
	set := newStatefulSet(1)
	pod := newStatefulSetPod(set, 0)
	controllerRef := metav1.GetControllerOf(pod)
	if controllerRef == nil {
		t.Fatalf("No ControllerRef found on new pod")
	}
	if got, want := controllerRef.APIVersion, appsv1beta1.SchemeGroupVersion.String(); got != want {
		t.Errorf("controllerRef.APIVersion = %q, want %q", got, want)
	}
	if got, want := controllerRef.Kind, "StatefulSet"; got != want {
		t.Errorf("controllerRef.Kind = %q, want %q", got, want)
	}
	if got, want := controllerRef.Name, set.Name; got != want {
		t.Errorf("controllerRef.Name = %q, want %q", got, want)
	}
	if got, want := controllerRef.UID, set.UID; got != want {
		t.Errorf("controllerRef.UID = %q, want %q", got, want)
	}
	if got, want := *controllerRef.Controller, true; got != want {
		t.Errorf("controllerRef.Controller = %v, want %v", got, want)
	}
}

func TestCreateApplyRevision(t *testing.T) {
	set := newStatefulSet(1)
	set.Status.CollisionCount = new(int32)
	revision, err := newRevision(set, 1, set.Status.CollisionCount)
	if err != nil {
		t.Fatal(err)
	}
	set.Spec.Template.Spec.Containers[0].Name = "foo"
	if set.Annotations == nil {
		set.Annotations = make(map[string]string)
	}
	key := "foo"
	expectedValue := "bar"
	set.Annotations[key] = expectedValue
	restoredSet, err := ApplyRevision(set, revision)
	if err != nil {
		t.Fatal(err)
	}
	restoredRevision, err := newRevision(restoredSet, 2, restoredSet.Status.CollisionCount)
	if err != nil {
		t.Fatal(err)
	}
	if !history.EqualRevision(revision, restoredRevision) {
		t.Errorf("wanted %v got %v", string(revision.Data.Raw), string(restoredRevision.Data.Raw))
	}
	value, ok := restoredRevision.Annotations[key]
	if !ok {
		t.Errorf("missing annotation %s", key)
	}
	if value != expectedValue {
		t.Errorf("for annotation %s wanted %s got %s", key, expectedValue, value)
	}
}

func TestRollingUpdateApplyRevision(t *testing.T) {
	set := newStatefulSet(1)
	set.Status.CollisionCount = new(int32)
	currentSet := set.DeepCopy()
	currentRevision, err := newRevision(set, 1, set.Status.CollisionCount)
	if err != nil {
		t.Fatal(err)
	}

	set.Spec.Template.Spec.Containers[0].Env = []v1.EnvVar{{Name: "foo", Value: "bar"}}
	updateSet := set.DeepCopy()
	updateRevision, err := newRevision(set, 2, set.Status.CollisionCount)
	if err != nil {
		t.Fatal(err)
	}

	restoredCurrentSet, err := ApplyRevision(set, currentRevision)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(currentSet.Spec.Template, restoredCurrentSet.Spec.Template) {
		t.Errorf("want %v got %v", currentSet.Spec.Template, restoredCurrentSet.Spec.Template)
	}

	restoredUpdateSet, err := ApplyRevision(set, updateRevision)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(updateSet.Spec.Template, restoredUpdateSet.Spec.Template) {
		t.Errorf("want %v got %v", updateSet.Spec.Template, restoredUpdateSet.Spec.Template)
	}
}

func newPVC(name string) v1.PersistentVolumeClaim {
	return v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: v1.PersistentVolumeClaimSpec{
			Resources: v1.ResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceStorage: *resource.NewQuantity(1, resource.BinarySI),
				},
			},
		},
	}
}

func newStatefulSetWithVolumes(replicas int, name string, petMounts []v1.VolumeMount, podMounts []v1.VolumeMount) *appsv1beta1.StatefulSet {
	mounts := append(petMounts, podMounts...)
	claims := []v1.PersistentVolumeClaim{}
	for _, m := range petMounts {
		claims = append(claims, newPVC(m.Name))
	}

	vols := []v1.Volume{}
	for _, m := range podMounts {
		vols = append(vols, v1.Volume{
			Name: m.Name,
			VolumeSource: v1.VolumeSource{
				HostPath: &v1.HostPathVolumeSource{
					Path: fmt.Sprintf("/tmp/%v", m.Name),
				},
			},
		})
	}

	template := v1.PodTemplateSpec{
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:         "nginx",
					Image:        "nginx",
					VolumeMounts: mounts,
				},
			},
			Volumes: vols,
		},
	}

	template.Labels = map[string]string{"foo": "bar"}

	return &appsv1beta1.StatefulSet{
		TypeMeta: metav1.TypeMeta{
			Kind:       "StatefulSet",
			APIVersion: "apps/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: v1.NamespaceDefault,
			UID:       types.UID("test"),
		},
		Spec: appsv1beta1.StatefulSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"foo": "bar"},
			},
			Replicas:             func() *int32 { i := int32(replicas); return &i }(),
			Template:             template,
			VolumeClaimTemplates: claims,
			ServiceName:          "governingsvc",
			UpdateStrategy:       appsv1beta1.StatefulSetUpdateStrategy{Type: apps.RollingUpdateStatefulSetStrategyType},
			RevisionHistoryLimit: func() *int32 {
				limit := int32(2)
				return &limit
			}(),
		},
	}
}

func newStatefulSet(replicas int) *appsv1beta1.StatefulSet {
	petMounts := []v1.VolumeMount{
		{Name: "datadir", MountPath: "/tmp/zookeeper"},
	}
	podMounts := []v1.VolumeMount{
		{Name: "home", MountPath: "/home"},
	}
	return newStatefulSetWithVolumes(replicas, "foo", petMounts, podMounts)
}
