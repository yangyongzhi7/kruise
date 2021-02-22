package scale

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	appspub "github.com/openkruise/kruise/apis/apps/pub"
	appsv1alpha1 "github.com/openkruise/kruise/apis/apps/v1alpha1"
	clonesetcore "github.com/openkruise/kruise/pkg/controller/cloneset/core"
	clonesetutils "github.com/openkruise/kruise/pkg/controller/cloneset/utils"
	"github.com/openkruise/kruise/pkg/util"
	"github.com/openkruise/kruise/pkg/util/expectations"
	"github.com/openkruise/kruise/pkg/util/lifecycle"
	apps "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// LengthOfInstanceID is the length of instance-id
	LengthOfInstanceID = 5

	// When batching pod creates, initialBatchSize is the size of the initial batch.
	initialBatchSize = 1
)

// Interface for managing replicas including create and delete pod/pvc.
type Interface interface {
	Manage(
		currentCS, updateCS *appsv1alpha1.CloneSet,
		currentRevision, updateRevision string,
		pods []*v1.Pod, pvcs []*v1.PersistentVolumeClaim,
	) (bool, error)
}

// New returns a scale control.
func New(c client.Client, recorder record.EventRecorder) Interface {
	return &realControl{Client: c, recorder: recorder}
}

type realControl struct {
	client.Client
	recorder record.EventRecorder
}

func (r *realControl) Manage(
	currentCS, updateCS *appsv1alpha1.CloneSet,
	currentRevision, updateRevision string,
	pods []*v1.Pod, pvcs []*v1.PersistentVolumeClaim,
) (bool, error) {
	if updateCS.Spec.Replicas == nil {
		return false, fmt.Errorf("spec.Replicas is nil")
	}

	controllerKey := clonesetutils.GetControllerKey(updateCS)
	coreControl := clonesetcore.New(updateCS)
	if !coreControl.IsReadyToScale() {
		klog.Warningf("CloneSet %s skip scaling for not ready to scale", controllerKey)
		return false, nil
	}

	updatedPods, notUpdatedPods := clonesetutils.SplitPodsByRevision(pods, updateRevision)
	diff, currentRevDiff := calculateDiffs(updateCS, updateRevision == currentRevision, len(pods), len(notUpdatedPods))

	// 1. scale out
	if diff < 0 {
		// total number of this creation
		expectedCreations := diff * -1
		// lack number of current version
		expectedCurrentCreations := 0
		if currentRevDiff < 0 {
			expectedCurrentCreations = currentRevDiff * -1
		}

		klog.V(3).Infof("CloneSet %s begin to scale out %d pods including %d (current rev)",
			controllerKey, expectedCreations, expectedCurrentCreations)

		// available instance-id come from free pvc
		availableIDs := getOrGenAvailableIDs(expectedCreations, pods, pvcs)
		// existing pvc names
		existingPVCNames := sets.NewString()
		for _, pvc := range pvcs {
			existingPVCNames.Insert(pvc.Name)
		}

		return r.createPods(expectedCreations, expectedCurrentCreations,
			currentCS, updateCS, currentRevision, updateRevision, availableIDs.List(), existingPVCNames)
	}

	// 2. specified scale in
	podsSpecifiedToDelete, podsInPreDelete := getPlannedDeletedPods(updateCS, pods)
	podsToDelete := util.MergePods(podsSpecifiedToDelete, podsInPreDelete)
	if len(podsToDelete) > 0 {
		klog.V(3).Infof("CloneSet %s find pods %v specified to delete and pods %v in preDelete",
			controllerKey, util.GetPodNames(podsSpecifiedToDelete).List(), util.GetPodNames(podsInPreDelete).List())

		if modified, err := r.managePreparingDelete(updateCS, pods, podsInPreDelete, len(podsToDelete)); err != nil || modified {
			return modified, err
		}

		if modified, err := r.deletePods(updateCS, podsToDelete, pvcs); err != nil || modified {
			return modified, err
		}
	}

	// 3. scale in
	if diff > 0 {
		if len(podsToDelete) > 0 {
			klog.V(3).Infof("CloneSet %s skip to scale in %d for existing pods to delete", controllerKey, diff)
			return false, nil
		}

		klog.V(3).Infof("CloneSet %s begin to scale in %d pods including %d (current rev)",
			controllerKey, diff, currentRevDiff)

		podsToDelete := choosePodsToDelete(diff, currentRevDiff, notUpdatedPods, updatedPods)

		return r.deletePods(updateCS, podsToDelete, pvcs)
	}

	return false, nil
}

func (r *realControl) managePreparingDelete(cs *appsv1alpha1.CloneSet, pods, podsInPreDelete []*v1.Pod, numToDelete int) (bool, error) {
	diff := int(*cs.Spec.Replicas) - len(pods) + numToDelete
	var modified bool
	for _, pod := range podsInPreDelete {
		if diff <= 0 {
			return modified, nil
		}
		if isPodSpecifiedDelete(cs, pod) {
			continue
		}

		klog.V(3).Infof("CloneSet %s patch pod %s lifecycle from PreparingDelete to Normal",
			clonesetutils.GetControllerKey(cs), pod.Name)
		if patched, err := lifecycle.PatchPodLifecycle(r, pod, appspub.LifecycleStateNormal); err != nil {
			return modified, err
		} else if patched {
			modified = true
			clonesetutils.ResourceVersionExpectations.Expect(pod)
		}
		diff--
	}
	return modified, nil
}

func (r *realControl) createPods(
	expectedCreations, expectedCurrentCreations int,
	currentCS, updateCS *appsv1alpha1.CloneSet,
	currentRevision, updateRevision string,
	availableIDs []string, existingPVCNames sets.String,
) (bool, error) {
	// new all pods need to create
	coreControl := clonesetcore.New(updateCS)
	newPods, err := coreControl.NewVersionedPods(currentCS, updateCS, currentRevision, updateRevision,
		expectedCreations, expectedCurrentCreations, availableIDs)
	if err != nil {
		return false, err
	}

	podsCreationChan := make(chan *v1.Pod, len(newPods))
	for _, p := range newPods {
		clonesetutils.ScaleExpectations.ExpectScale(clonesetutils.GetControllerKey(updateCS), expectations.Create, p.Name)
		podsCreationChan <- p
	}

	var created int64
	successPodNames := sync.Map{}
	_, err = clonesetutils.DoItSlowly(len(newPods), initialBatchSize, func() error {
		pod := <-podsCreationChan

		cs := updateCS
		if pod.Labels[apps.ControllerRevisionHashLabelKey] == currentRevision {
			cs = currentCS
		}
		lifecycle.SetPodLifecycle(appspub.LifecycleStateNormal)(pod)

		var createErr error
		if createErr = r.createOnePod(cs, pod, existingPVCNames); createErr != nil {
			return createErr
		}

		atomic.AddInt64(&created, 1)

		successPodNames.Store(pod.Name, struct{}{})
		return nil
	})

	// rollback to ignore failure pods because the informer won't observe these pods
	for _, pod := range newPods {
		if _, ok := successPodNames.Load(pod.Name); !ok {
			clonesetutils.ScaleExpectations.ObserveScale(clonesetutils.GetControllerKey(updateCS), expectations.Create, pod.Name)
		}
	}

	if created == 0 {
		return false, err
	}
	return true, err
}

func (r *realControl) createOnePod(cs *appsv1alpha1.CloneSet, pod *v1.Pod, existingPVCNames sets.String) error {
	claims := clonesetutils.GetPersistentVolumeClaims(cs, pod)
	for _, c := range claims {
		if existingPVCNames.Has(c.Name) {
			continue
		}
		clonesetutils.ScaleExpectations.ExpectScale(clonesetutils.GetControllerKey(cs), expectations.Create, c.Name)
		if err := r.Create(context.TODO(), &c); err != nil {
			clonesetutils.ScaleExpectations.ObserveScale(clonesetutils.GetControllerKey(cs), expectations.Create, c.Name)
			r.recorder.Eventf(cs, v1.EventTypeWarning, "FailedCreate", "failed to create pvc: %v, pvc: %v", err, util.DumpJSON(c))
			return err
		}
	}

	if err := r.Create(context.TODO(), pod); err != nil {
		r.recorder.Eventf(cs, v1.EventTypeWarning, "FailedCreate", "failed to create pod: %v, pod: %v", err, util.DumpJSON(pod))
		return err
	}

	r.recorder.Eventf(cs, v1.EventTypeNormal, "SuccessfulCreate", "succeed to create pod %s", pod.Name)
	return nil
}

func (r *realControl) deletePods(cs *appsv1alpha1.CloneSet, podsToDelete []*v1.Pod, pvcs []*v1.PersistentVolumeClaim) (bool, error) {
	var modified bool
	for _, pod := range podsToDelete {
		if cs.Spec.Lifecycle != nil && lifecycle.IsPodHooked(cs.Spec.Lifecycle.PreDelete, pod) {
			if patched, err := lifecycle.PatchPodLifecycle(r, pod, appspub.LifecycleStatePreparingDelete); err != nil {
				return false, err
			} else if patched {
				klog.V(3).Infof("CloneSet %s scaling patch pod %s lifecycle to PreparingDelete",
					clonesetutils.GetControllerKey(cs), pod.Name)
				modified = true
				clonesetutils.ResourceVersionExpectations.Expect(pod)
			}
			continue
		}

		clonesetutils.ScaleExpectations.ExpectScale(clonesetutils.GetControllerKey(cs), expectations.Delete, pod.Name)
		if err := r.Delete(context.TODO(), pod); err != nil {
			clonesetutils.ScaleExpectations.ObserveScale(clonesetutils.GetControllerKey(cs), expectations.Delete, pod.Name)
			r.recorder.Eventf(cs, v1.EventTypeWarning, "FailedDelete", "failed to delete pod %s: %v", pod.Name, err)
			return modified, err
		}
		modified = true
		r.recorder.Event(cs, v1.EventTypeNormal, "SuccessfulDelete", fmt.Sprintf("succeed to delete pod %s", pod.Name))

		// delete pvcs which have the same instance-id
		for _, pvc := range pvcs {
			if pvc.Labels[appsv1alpha1.CloneSetInstanceID] != pod.Labels[appsv1alpha1.CloneSetInstanceID] {
				continue
			}

			clonesetutils.ScaleExpectations.ExpectScale(clonesetutils.GetControllerKey(cs), expectations.Delete, pvc.Name)
			if err := r.Delete(context.TODO(), pvc); err != nil {
				clonesetutils.ScaleExpectations.ObserveScale(clonesetutils.GetControllerKey(cs), expectations.Delete, pvc.Name)
				r.recorder.Eventf(cs, v1.EventTypeWarning, "FailedDelete", "failed to delete pvc %s: %v", pvc.Name, err)
				return modified, err
			}
		}
	}

	return modified, nil
}
