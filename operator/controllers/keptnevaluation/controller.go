/*
Copyright 2022.

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

package keptnevaluation

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/go-logr/logr"
	klcv1alpha1 "github.com/keptn-sandbox/lifecycle-controller/operator/api/v1alpha1"
	"github.com/keptn-sandbox/lifecycle-controller/operator/api/v1alpha1/common"
	"github.com/keptn-sandbox/lifecycle-controller/operator/api/v1alpha1/semconv"
)

// KeptnEvaluationReconciler reconciles a KeptnEvaluation object
type KeptnEvaluationReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	Log      logr.Logger
	Meters   common.KeptnMeters
	Tracer   trace.Tracer
}

//+kubebuilder:rbac:groups=lifecycle.keptn.sh,resources=keptnevaluations,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=lifecycle.keptn.sh,resources=keptnevaluations/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=lifecycle.keptn.sh,resources=keptnevaluations/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the KeptnEvaluation object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.12.2/pkg/reconcile
func (r *KeptnEvaluationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	r.Log.Info("Reconciling KeptnEvaluation")
	evaluation := &klcv1alpha1.KeptnEvaluation{}

	if err := r.Client.Get(ctx, req.NamespacedName, evaluation); err != nil {
		if errors.IsNotFound(err) {
			// taking down all associated K8s resources is handled by K8s
			r.Log.Info("KeptnEvaluation resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		r.Log.Error(err, "Failed to get the KeptnEvaluation")
		return ctrl.Result{Requeue: true, RequeueAfter: 30 * time.Second}, nil
	}

	traceContextCarrier := propagation.MapCarrier(evaluation.Annotations)
	ctx = otel.GetTextMapPropagator().Extract(ctx, traceContextCarrier)

	ctx, span := r.Tracer.Start(ctx, "reconcile_evaluation", trace.WithSpanKind(trace.SpanKindConsumer))
	defer span.End()

	semconv.AddAttributeFromEvaluation(span, *evaluation)

	if !evaluation.IsStartTimeSet() {
		// metrics: increment active evaluation counter
		r.Meters.AnalysisActive.Add(ctx, 1, evaluation.GetActiveMetricsAttributes()...)
		evaluation.SetStartTime()
	}

	if !evaluation.Status.OverallStatus.IsCompleted() && evaluation.Status.RetryCount <= evaluation.Spec.Retries {
		evaluationDefinition, evaluationProvider, err := r.fetchDefinitionAndProvider(ctx, req.NamespacedName) //TODO we need to fetch using the right name not the Evaluation name
		if err != nil {
			return ctrl.Result{Requeue: true, RequeueAfter: 30 * time.Second}, nil
		}
		if evaluationDefinition == nil && evaluationProvider == nil {
			return ctrl.Result{}, nil
		}

		if len(evaluation.Status.EvaluationStatus) != len(evaluationDefinition.Spec.Objectives) {
			evaluation.InitializeEvaluationStatuses(*evaluationDefinition)
		}

		statusSummary := common.StatusSummary{}

		for i, query := range evaluationDefinition.Spec.Objectives {
			if evaluation.Status.EvaluationStatus[i].Status.IsSucceeded() {
				statusSummary = common.UpdateStatusSummary(common.StateSucceeded, statusSummary)
				continue
			}
			statusItem := r.queryEvaluation(query, *evaluationProvider)
			statusSummary = common.UpdateStatusSummary(statusItem.Status, statusSummary)
			evaluation.Status.EvaluationStatus[i] = *statusItem
		}

		evaluation.Status.RetryCount++
		evaluation.Status.OverallStatus = common.GetOverallState(statusSummary)
	}

	err := r.Client.Status().Update(ctx, evaluation)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return ctrl.Result{Requeue: true}, err
	}

	if !evaluation.Status.OverallStatus.IsCompleted() {
		return ctrl.Result{Requeue: true, RequeueAfter: evaluation.Spec.RetryInterval * time.Second}, nil
	}

	r.Log.Info("Finished Reconciling KeptnEvaluation")

	// Evaluation is completed at this place

	if !evaluation.IsEndTimeSet() {
		// metrics: decrement active evaluation counter
		r.Meters.AnalysisActive.Add(ctx, -1, evaluation.GetActiveMetricsAttributes()...)
		evaluation.SetEndTime()
	}

	err = r.Client.Status().Update(ctx, evaluation)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return ctrl.Result{Requeue: true}, err
	}

	attrs := evaluation.GetMetricsAttributes()

	r.Log.Info("Increasing evaluation count")

	// metrics: increment evaluation counter
	r.Meters.AnalysisCount.Add(ctx, 1, attrs...)

	// metrics: add evaluation duration
	duration := evaluation.Status.EndTime.Time.Sub(evaluation.Status.StartTime.Time)
	r.Meters.AnalysisDuration.Record(ctx, duration.Seconds(), attrs...)

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *KeptnEvaluationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&klcv1alpha1.KeptnEvaluation{}).
		Complete(r)
}

func (r KeptnEvaluationReconciler) fetchDefinitionAndProvider(ctx context.Context, namespace types.NamespacedName) (*klcv1alpha1.KeptnEvaluationDefinition, *klcv1alpha1.KeptnEvaluationProvider, error) {
	evaluationDefinition := &klcv1alpha1.KeptnEvaluationDefinition{}
	if err := r.Client.Get(ctx, namespace, evaluationDefinition); err != nil {
		if errors.IsNotFound(err) {
			// taking down all associated K8s resources is handled by K8s
			r.Log.Info("KeptnEvaluationDefinition resource not found. Ignoring since object must be deleted")
			return nil, nil, nil
		}
		r.Log.Error(err, "Failed to get the KeptnEvaluationDefinition")
		return nil, nil, err
	}

	evaluationProvider := &klcv1alpha1.KeptnEvaluationProvider{}
	if err := r.Client.Get(ctx, namespace, evaluationProvider); err != nil {
		if errors.IsNotFound(err) {
			// taking down all associated K8s resources is handled by K8s
			r.Log.Info("KeptnEvaluationProvider resource not found. Ignoring since object must be deleted")
			return nil, nil, nil
		}
		r.Log.Error(err, "Failed to get the KeptnEvaluationProvider")
		return nil, nil, err
	}

	return evaluationDefinition, evaluationProvider, nil
}

func (r KeptnEvaluationReconciler) queryEvaluation(objective klcv1alpha1.Objective, provider klcv1alpha1.KeptnEvaluationProvider) *klcv1alpha1.EvaluationStatusItem {
	query := &klcv1alpha1.EvaluationStatusItem{
		Name:   objective.Name,
		Value:  "",
		Status: "",
	}

	//TODO query provider like prometheus service does
	//TODO decide and update status in query

	return query
}
