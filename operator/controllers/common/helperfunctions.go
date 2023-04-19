package common

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	klcv1alpha3 "github.com/keptn/lifecycle-toolkit/operator/apis/lifecycle/v1alpha3"
	apicommon "github.com/keptn/lifecycle-toolkit/operator/apis/lifecycle/v1alpha3/common"
	controllererrors "github.com/keptn/lifecycle-toolkit/operator/controllers/errors"
	"github.com/keptn/lifecycle-toolkit/operator/controllers/lifecycle/interfaces"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// GetItemStatus retrieves the state of the task/evaluation, if it does not exists, it creates a default one
func GetItemStatus(name string, instanceStatus []klcv1alpha3.ItemStatus) klcv1alpha3.ItemStatus {
	for _, status := range instanceStatus {
		if status.DefinitionName == name {
			return status
		}
	}
	return klcv1alpha3.ItemStatus{
		DefinitionName: name,
		Status:         apicommon.StatePending,
		Name:           "",
	}
}

func GetAppVersionName(namespace string, appName string, version string) types.NamespacedName {
	return types.NamespacedName{Namespace: namespace, Name: appName + "-" + version}
}

// GetOldStatus retrieves the state of the task/evaluation
func GetOldStatus(name string, statuses []klcv1alpha3.ItemStatus) apicommon.KeptnState {
	var oldstatus apicommon.KeptnState
	for _, ts := range statuses {
		if ts.DefinitionName == name {
			oldstatus = ts.Status
		}
	}

	return oldstatus
}

// RecordEvent creates k8s Event and adds it to Eventqueue
func RecordEvent(recorder record.EventRecorder, phase apicommon.KeptnPhaseType, eventType string, reconcileObject client.Object, shortReason string, longReason string, version string) {
	msg := setEventMessage(phase, reconcileObject, longReason, version)
	annotations := setAnnotations(reconcileObject, phase)
	recorder.AnnotatedEventf(reconcileObject, annotations, eventType, fmt.Sprintf("%s%s", phase.ShortName, shortReason), msg)
}

func setEventMessage(phase apicommon.KeptnPhaseType, reconcileObject client.Object, longReason string, version string) string {
	if version == "" {
		return fmt.Sprintf("%s: %s / Namespace: %s, Name: %s", phase.LongName, longReason, reconcileObject.GetNamespace(), reconcileObject.GetName())
	}
	return fmt.Sprintf("%s: %s / Namespace: %s, Name: %s, Version: %s", phase.LongName, longReason, reconcileObject.GetNamespace(), reconcileObject.GetName(), version)
}

func setAnnotations(reconcileObject client.Object, phase apicommon.KeptnPhaseType) map[string]string {
	if reconcileObject == nil || reconcileObject.GetName() == "" || reconcileObject.GetNamespace() == "" {
		return nil
	}
	annotations := map[string]string{
		"namespace": reconcileObject.GetNamespace(),
		"name":      reconcileObject.GetName(),
		"phase":     phase.ShortName,
	}

	piWrapper, err := interfaces.NewEventObjectWrapperFromClientObject(reconcileObject)
	if err == nil {
		copyMap(annotations, piWrapper.GetEventAnnotations())
	}

	annotationsObject := reconcileObject.GetAnnotations()
	annotations["traceparent"] = annotationsObject["traceparent"]

	return annotations
}

func copyMap[M1 ~map[K]V, M2 ~map[K]V, K comparable, V any](dst M1, src M2) {
	for k, v := range src {
		dst[k] = v
	}
}

func RemoveGates(ctx context.Context, c client.Client, log logr.Logger, workloadInstance *klcv1alpha3.KeptnWorkloadInstance) error {
	switch workloadInstance.Spec.ResourceReference.Kind {
	case "Pod":
		return removePodGates(ctx, c, log, workloadInstance.Spec.ResourceReference.Name, workloadInstance.Namespace)
	case "ReplicaSet", "StatefulSet", "DaemonSet":
		podList, err := getPodsOfOwner(ctx, c, log, workloadInstance.Spec.ResourceReference.UID, workloadInstance.Spec.ResourceReference.Kind, workloadInstance.Namespace)
		if err != nil {
			log.Error(err, "cannot get pods")
			return err
		}
		for _, pod := range podList {
			err := removePodGates(ctx, c, log, pod, workloadInstance.Namespace)
			if err != nil {
				log.Error(err, "cannot remove gates from pod")
				return err
			}
		}
	default:
		return controllererrors.ErrUnsupportedWorkloadInstanceResourceReference
	}

	return nil
}

func removePodGates(ctx context.Context, c client.Client, log logr.Logger, podName string, podNamespace string) error {
	pod := &v1.Pod{}
	err := c.Get(ctx, types.NamespacedName{Namespace: podNamespace, Name: podName}, pod)
	if err != nil {
		log.Error(err, "cannot remove gates from pod - inner")
		return err
	}
	if len(pod.Annotations) == 0 {
		pod.Annotations = make(map[string]string)
	}
	pod.Annotations[apicommon.SchedullingGateRemoved] = "true"
	pod.Spec.SchedulingGates = nil
	return c.Update(ctx, pod)
}

func getPodsOfOwner(ctx context.Context, c client.Client, log logr.Logger, ownerUID types.UID, ownerKind string, namespace string) ([]string, error) {
	pods := &v1.PodList{}
	err := c.List(ctx, pods, client.InNamespace(namespace))
	if err != nil {
		log.Error(err, "cannot list pods - inner")
		return nil, err
	}

	var resultPods []string

	for _, pod := range pods.Items {
		for _, owner := range pod.OwnerReferences {
			if owner.Kind == ownerKind && owner.UID == ownerUID {
				resultPods = append(resultPods, pod.Name)
				break
			}
		}
	}

	return resultPods, nil
}
