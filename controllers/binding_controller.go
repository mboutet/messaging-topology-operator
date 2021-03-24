/*
RabbitMQ Messaging Topology Kubernetes Operator
Copyright 2021 VMware, Inc.

This product is licensed to you under the Mozilla Public License 2.0 license (the "License").  You may not use this product except in compliance with the Mozilla 2.0 License.

This product may include a number of subcomponents with separate copyright notices and license terms. Your use of these subcomponents is subject to the terms and conditions of the subcomponent's license, as noted in the LICENSE file.
*/

package controllers

import (
	"context"
	"encoding/json"
	"errors"
	rabbithole "github.com/michaelklishin/rabbit-hole/v2"
	"github.com/rabbitmq/messaging-topology-operator/internal"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	clientretry "k8s.io/client-go/util/retry"
	"reflect"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	topologyv1alpha1 "github.com/rabbitmq/messaging-topology-operator/api/v1alpha1"
)

const bindingFinalizer = "deletion.finalizers.bindings.rabbitmq.com"

// BindingReconciler reconciles a Binding object
type BindingReconciler struct {
	client.Client
	Log      logr.Logger
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=rabbitmq.com,resources=bindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rabbitmq.com,resources=bindings/status,verbs=get;update;patch

func (r *BindingReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrl.LoggerFrom(ctx)

	binding := &topologyv1alpha1.Binding{}
	if err := r.Get(ctx, req.NamespacedName, binding); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	rabbitClient, err := rabbitholeClient(ctx, r.Client, binding.Spec.RabbitmqClusterReference)

	if errors.Is(err, NoSuchRabbitmqClusterError) && binding.ObjectMeta.DeletionTimestamp.IsZero() {
		logger.Info("Could not generate rabbitClient for non existent cluster: " + err.Error())
		return reconcile.Result{RequeueAfter: 10 * time.Second}, err
	} else if err != nil && !errors.Is(err, NoSuchRabbitmqClusterError) {
		logger.Error(err, failedGenerateRabbitClient)
		return reconcile.Result{}, err
	}

	if !binding.ObjectMeta.DeletionTimestamp.IsZero() {
		logger.Info("Deleting")
		return ctrl.Result{}, r.deleteBinding(ctx, rabbitClient, binding)
	}

	if err := r.addFinalizerIfNeeded(ctx, binding); err != nil {
		return ctrl.Result{}, err
	}
	spec, err := json.Marshal(binding.Spec)
	if err != nil {
		logger.Error(err, failedMarshalSpec)
	}

	logger.Info("Start reconciling",
		"spec", string(spec))

	if err := r.declareBinding(ctx, rabbitClient, binding); err != nil {
		// Set Condition 'Ready' to false with message
		binding.Status.Conditions = []topologyv1alpha1.Condition{topologyv1alpha1.NotReady(err.Error())}
		if writerErr := clientretry.RetryOnConflict(clientretry.DefaultRetry, func() error {
			return r.Status().Update(ctx, binding)
		}); writerErr != nil {
			logger.Error(writerErr, failedStatusUpdate)
		}
		return ctrl.Result{}, err
	}

	binding.Status.Conditions = []topologyv1alpha1.Condition{topologyv1alpha1.Ready()}
	binding.Status.ObservedGeneration = binding.GetGeneration()
	if writerErr := clientretry.RetryOnConflict(clientretry.DefaultRetry, func() error {
		return r.Status().Update(ctx, binding)
	}); writerErr != nil {
		logger.Error(writerErr, failedStatusUpdate)
	}
	logger.Info("Finished reconciling")

	return ctrl.Result{}, nil
}

func (r *BindingReconciler) declareBinding(ctx context.Context, client *rabbithole.Client, binding *topologyv1alpha1.Binding) error {
	logger := ctrl.LoggerFrom(ctx)

	info, err := internal.GenerateBindingInfo(binding)
	if err != nil {
		msg := "failed to generate binding info"
		r.Recorder.Event(binding, corev1.EventTypeWarning, "FailedDeclare", msg)
		logger.Error(err, msg)
		return err
	}

	if err := validateResponse(client.DeclareBinding(binding.Spec.Vhost, *info)); err != nil {
		msg := "failed to declare binding"
		r.Recorder.Event(binding, corev1.EventTypeWarning, "FailedDeclare", msg)
		logger.Error(err, msg)
		return err
	}

	logger.Info("Successfully declared binding")
	r.Recorder.Event(binding, corev1.EventTypeNormal, "SuccessfulDeclare", "Successfully declared binding")
	return nil
}

// deletes binding from rabbitmq server; bindings have no name; server needs BindingInfo to delete them
// when server responds with '404' Not Found, it logs and does not requeue on error
// if no binding argument is set, generating properties key by using internal.GeneratePropertiesKey
// if binding arguments are set, list all bindings between source/destination to find the binding; if it failed to find corresponding binding, it assumes that the binding is already deleted and returns no error
func (r *BindingReconciler) deleteBinding(ctx context.Context, client *rabbithole.Client, binding *topologyv1alpha1.Binding) error {
	logger := ctrl.LoggerFrom(ctx)

	var info *rabbithole.BindingInfo
	var err error
	if binding.Spec.Arguments != nil {
		info, err = r.findBindingInfo(logger, binding, client)
		if err != nil {
			return err
		}
		if info == nil {
			logger.Info("cannot find the corresponding binding info in rabbitmq server; binding already deleted")
			return r.removeFinalizer(ctx, binding)
		}
	} else {
		info, err = internal.GenerateBindingInfo(binding)
		if err != nil {
			msg := "failed to generate binding info"
			r.Recorder.Event(binding, corev1.EventTypeWarning, "FailedDelete", msg)
			logger.Error(err, msg)
			return err
		}
		info.PropertiesKey = internal.GeneratePropertiesKey(binding)
	}

	err = validateResponseForDeletion(client.DeleteBinding(binding.Spec.Vhost, *info))
	if errors.Is(err, NotFound) {
		logger.Info("cannot find binding in rabbitmq server; already deleted")
	} else if err != nil {
		msg := "failed to delete binding"
		r.Recorder.Event(binding, corev1.EventTypeWarning, "FailedDelete", msg)
		logger.Error(err, msg)
		return err
	}

	logger.Info("Successfully deleted binding")
	return r.removeFinalizer(ctx, binding)
}

func (r *BindingReconciler) findBindingInfo(logger logr.Logger, binding *topologyv1alpha1.Binding, client *rabbithole.Client) (*rabbithole.BindingInfo, error) {
	logger.Info("binding arguments set; listing bindings from server to complete deletion")
	arguments := make(map[string]interface{})
	if binding.Spec.Arguments != nil {
		if err := json.Unmarshal(binding.Spec.Arguments.Raw, &arguments); err != nil {
			msg := "failed to unmarshall binding arguments"
			r.Recorder.Event(binding, corev1.EventTypeWarning, "FailedDelete", msg)
			logger.Error(err, msg)
			return nil, err
		}
	}
	var bindingInfos []rabbithole.BindingInfo
	var err error
	if binding.Spec.DestinationType == "queue" {
		bindingInfos, err = client.ListQueueBindingsBetween(binding.Spec.Vhost, binding.Spec.Source, binding.Spec.Destination)
	} else {
		bindingInfos, err = client.ListExchangeBindingsBetween(binding.Spec.Vhost, binding.Spec.Source, binding.Spec.Destination)
	}
	if err != nil {
		msg := "failed to list binding infos"
		r.Recorder.Event(binding, corev1.EventTypeWarning, "FailedDelete", msg)
		logger.Error(err, msg)
		return nil, err
	}
	var info *rabbithole.BindingInfo
	for _, b := range bindingInfos {
		if binding.Spec.RoutingKey == b.RoutingKey && reflect.DeepEqual(b.Arguments, arguments) {
			info = &b
		}
	}
	return info, nil
}

func (r *BindingReconciler) removeFinalizer(ctx context.Context, binding *topologyv1alpha1.Binding) error {
	controllerutil.RemoveFinalizer(binding, bindingFinalizer)
	if err := r.Client.Update(ctx, binding); err != nil {
		return err
	}
	return nil
}

func (r *BindingReconciler) addFinalizerIfNeeded(ctx context.Context, binding *topologyv1alpha1.Binding) error {
	if binding.ObjectMeta.DeletionTimestamp.IsZero() && !controllerutil.ContainsFinalizer(binding, bindingFinalizer) {
		controllerutil.AddFinalizer(binding, bindingFinalizer)
		if err := r.Client.Update(ctx, binding); err != nil {
			return err
		}
	}
	return nil
}

func (r *BindingReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&topologyv1alpha1.Binding{}).
		Complete(r)
}