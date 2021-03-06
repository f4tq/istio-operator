/*
Copyright 2019 Banzai Cloud.

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

package remoteistio

import (
	"context"
	"reflect"

	"github.com/go-logr/logr"
	"github.com/gofrs/uuid"
	"github.com/goph/emperror"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	logf "sigs.k8s.io/controller-runtime/pkg/runtime/log"
	"sigs.k8s.io/controller-runtime/pkg/source"

	istiov1beta1 "github.com/banzaicloud/istio-operator/pkg/apis/istio/v1beta1"
	istioCtrl "github.com/banzaicloud/istio-operator/pkg/controller/istio"
	"github.com/banzaicloud/istio-operator/pkg/k8sutil"
	"github.com/banzaicloud/istio-operator/pkg/remoteclusters"
	"github.com/banzaicloud/istio-operator/pkg/util"
)

const finalizerID = "remote-istio-operator.finializer.banzaicloud.io"

var log = logf.Log.WithName("remote-istio-controller")

// Add creates a new RemoteConfig Controller and adds it to the Manager with default RBAC. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager, cm *remoteclusters.Manager) error {
	return add(mgr, newReconciler(mgr, cm))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager, cm *remoteclusters.Manager) reconcile.Reconciler {
	return &ReconcileRemoteConfig{
		Client:            mgr.GetClient(),
		scheme:            mgr.GetScheme(),
		remoteClustersMgr: cm,
	}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New("remoteconfig-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to RemoteConfig
	err = c.Watch(&source.Kind{Type: &istiov1beta1.RemoteIstio{TypeMeta: metav1.TypeMeta{Kind: "RemoteIstio", APIVersion: "istio.banzaicloud.io/v1beta1"}}}, &handler.EnqueueRequestForObject{}, getWatchPredicateForRemoteConfig())
	if err != nil {
		return err
	}

	// Watch for Istio changes to trigger reconciliation for RemoteIstios
	err = c.Watch(&source.Kind{Type: &istiov1beta1.Istio{TypeMeta: metav1.TypeMeta{Kind: "Istio", APIVersion: "istio.banzaicloud.io/v1beta1"}}}, &handler.EnqueueRequestsFromMapFunc{
		ToRequests: handler.ToRequestsFunc(func(object handler.MapObject) []reconcile.Request {
			return triggerRemoteIstios(mgr, object, log)
		}),
	}, istioCtrl.WatchPredicateForConfig())
	if err != nil {
		return err
	}

	return nil
}

var _ reconcile.Reconciler = &ReconcileRemoteConfig{}

// ReconcileRemoteConfig reconciles a RemoteConfig object
type ReconcileRemoteConfig struct {
	client.Client
	scheme *runtime.Scheme

	remoteClustersMgr *remoteclusters.Manager
}

// Reconcile reads that state of the cluster for a RemoteConfig object and makes changes based on the state read
// and what is in the RemoteConfig.Spec
// Automatically generate RBAC rules to allow the Controller to read and write Deployments
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=istio.banzaicloud.io,resources=remoteistios,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=istio.banzaicloud.io,resources=remoteistios/status,verbs=get;update;patch
func (r *ReconcileRemoteConfig) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	logger := log.WithValues("trigger", request.Namespace+"/"+request.Name, "correlationID", uuid.Must(uuid.NewV4()).String())
	remoteConfig := &istiov1beta1.RemoteIstio{}
	err := r.Get(context.TODO(), request.NamespacedName, remoteConfig)
	if err != nil {
		if !k8serrors.IsNotFound(err) {
			// Error reading the object - requeue the request.
			logger.Info("remoteconfig error - requeue")
			return reconcile.Result{}, err
		}

		// Handle delete with a finalizer
		return reconcile.Result{}, nil
	}

	if remoteConfig.ObjectMeta.DeletionTimestamp.IsZero() {
		if !util.ContainsString(remoteConfig.ObjectMeta.Finalizers, finalizerID) {
			remoteConfig.ObjectMeta.Finalizers = append(remoteConfig.ObjectMeta.Finalizers, finalizerID)
			if err := r.Update(context.Background(), remoteConfig); err != nil {
				return reconcile.Result{}, emperror.Wrap(err, "could not add finalizer to remoteconfig")
			}
			// we return here and do the reconciling when this update arrives
			return reconcile.Result{}, nil
		}
	} else {
		if util.ContainsString(remoteConfig.ObjectMeta.Finalizers, finalizerID) {
			if remoteConfig.Status.Status == istiov1beta1.Reconciling && remoteConfig.Status.ErrorMessage == "" {
				logger.Info("cannot remove remote istio config while reconciling")
				return reconcile.Result{}, nil
			}
			logger.Info("removing remote istio")
			cluster, err := r.remoteClustersMgr.Get(remoteConfig.Name)
			if err == nil {
				err = cluster.RemoveConfig()
				if err != nil {
					return reconcile.Result{}, emperror.Wrap(err, "could not remove remote config to remote istio")
				}
			}
			remoteConfig.ObjectMeta.Finalizers = util.RemoveString(remoteConfig.ObjectMeta.Finalizers, finalizerID)
			if err := r.Update(context.Background(), remoteConfig); err != nil {
				return reconcile.Result{}, emperror.Wrap(err, "could not remove finalizer from remoteconfig")
			}
		}

		logger.Info("remote istio removed")

		return reconcile.Result{}, nil
	}

	istio, err := r.getIstio()
	if err != nil {
		return reconcile.Result{}, err
	}

	if !istio.Spec.Version.IsSupported() {
		err = errors.New("intended Istio version is unsupported by this version of the operator")
		logger.Error(err, "", "version", istio.Spec.Version)
		return reconcile.Result{
			Requeue: false,
		}, nil
	}

	refs, err := k8sutil.SetOwnerReferenceToObject(remoteConfig, istio)
	if err != nil {
		return reconcile.Result{}, err
	}
	remoteConfig.SetOwnerReferences(refs)
	err = r.Update(context.TODO(), remoteConfig)
	if err != nil {
		return reconcile.Result{}, err
	}

	// Set default values where not set
	istiov1beta1.SetRemoteIstioDefaults(remoteConfig)
	result, err := r.reconcile(remoteConfig, istio, logger)
	if err != nil {
		updateErr := r.updateRemoteConfigStatus(remoteConfig, istiov1beta1.ReconcileFailed, err.Error(), logger)
		if updateErr != nil {
			return result, errors.WithStack(err)
		}

		return result, emperror.Wrap(err, "could not reconcile remote istio")
	}

	return result, nil
}

func (r *ReconcileRemoteConfig) reconcile(remoteConfig *istiov1beta1.RemoteIstio, istio *istiov1beta1.Istio, logger logr.Logger) (reconcile.Result, error) {
	var err error

	logger = logger.WithValues("cluster", remoteConfig.Name)

	if remoteConfig.Status.Status == "" {
		err = r.updateRemoteConfigStatus(remoteConfig, istiov1beta1.Created, "", logger)
		if err != nil {
			return reconcile.Result{}, errors.WithStack(err)
		}
	}

	if remoteConfig.Status.Status == istiov1beta1.Reconciling {
		return reconcile.Result{}, errors.New("cannot trigger reconcile while already reconciling")
	}

	err = r.updateRemoteConfigStatus(remoteConfig, istiov1beta1.Reconciling, "", logger)
	if err != nil {
		return reconcile.Result{}, errors.WithStack(err)
	}

	logger.Info("begin reconciling remote istio")

	remoteConfig, err = r.populateEnabledServicePodIPs(remoteConfig)
	if err != nil {
		return reconcile.Result{}, emperror.Wrap(err, "could not populate pod ips to remoteconfig")
	}

	remoteConfig, err = r.populateSignCerts(remoteConfig)
	if err != nil {
		return reconcile.Result{}, emperror.Wrap(err, "could not populate sign certs to remoteconfig")
	}

	cluster, _ := r.remoteClustersMgr.Get(remoteConfig.Name)
	if cluster == nil {
		cluster, err = r.getRemoteCluster(remoteConfig, logger)
	}
	if err != nil {
		return reconcile.Result{}, emperror.Wrap(err, "could not get remote cluster")
	}

	err = cluster.Reconcile(remoteConfig, istio)
	if err != nil {
		return reconcile.Result{}, emperror.Wrap(err, "could not reconcile remote istio")
	}

	err = r.updateRemoteConfigStatus(remoteConfig, istiov1beta1.Available, "", logger)
	if err != nil {
		return reconcile.Result{}, errors.WithStack(err)
	}

	logger.Info("remote istio reconciled")

	return reconcile.Result{}, nil
}

func (r *ReconcileRemoteConfig) updateRemoteConfigStatus(remoteConfig *istiov1beta1.RemoteIstio, status istiov1beta1.ConfigState, errorMessage string, logger logr.Logger) error {
	typeMeta := remoteConfig.TypeMeta
	remoteConfig.Status.Status = status
	remoteConfig.Status.ErrorMessage = errorMessage
	err := r.Status().Update(context.Background(), remoteConfig)
	if k8serrors.IsNotFound(err) {
		err = r.Update(context.Background(), remoteConfig)
	}
	if err != nil {
		if !k8serrors.IsConflict(err) {
			return emperror.Wrapf(err, "could not update remote Istio state to '%s'", status)
		}
		err := r.Get(context.TODO(), types.NamespacedName{
			Namespace: remoteConfig.Namespace,
			Name:      remoteConfig.Name,
		}, remoteConfig)
		if err != nil {
			return emperror.Wrap(err, "could not get remoteconfig for updating status")
		}
		remoteConfig.Status.Status = status
		remoteConfig.Status.ErrorMessage = errorMessage
		err = r.Status().Update(context.Background(), remoteConfig)
		if k8serrors.IsNotFound(err) {
			err = r.Update(context.Background(), remoteConfig)
		}
		if err != nil {
			return emperror.Wrapf(err, "could not update remoteconfig status to '%s'", status)
		}
	}
	// update loses the typeMeta of the config so we're resetting it from the
	remoteConfig.TypeMeta = typeMeta
	logger.Info("remoteconfig status updated", "status", status)

	return nil
}

func (r *ReconcileRemoteConfig) populateSignCerts(remoteConfig *istiov1beta1.RemoteIstio) (*istiov1beta1.RemoteIstio, error) {
	var secret corev1.Secret
	err := r.Get(context.TODO(), client.ObjectKey{
		Namespace: remoteConfig.Namespace,
		Name:      "istio-ca-secret",
	}, &secret)
	if err != nil {
		return nil, err
	}

	remoteConfig.Spec = remoteConfig.Spec.SetSignCert(istiov1beta1.SignCert{
		CA:    secret.Data["ca-cert.pem"],
		Root:  secret.Data["ca-cert.pem"],
		Chain: []byte(""),
		Key:   secret.Data["ca-key.pem"],
	})

	return remoteConfig, nil
}

func (r *ReconcileRemoteConfig) populateEnabledServicePodIPs(remoteConfig *istiov1beta1.RemoteIstio) (*istiov1beta1.RemoteIstio, error) {
	var pods corev1.PodList

	for i, svc := range remoteConfig.Spec.EnabledServices {
		o := &client.ListOptions{}
		err := o.SetLabelSelector(svc.LabelSelector)
		if err != nil {
			return remoteConfig, err
		}
		err = r.List(context.TODO(), o, &pods)
		if err != nil {
			return remoteConfig, err
		}
		for _, pod := range pods.Items {
			ready := 0
			for _, status := range pod.Status.ContainerStatuses {
				if status.Ready {
					ready++
				}
			}
			if len(pod.Spec.Containers) == ready {
				svc.IPs = append(svc.IPs, pod.Status.PodIP)
				remoteConfig.Spec.EnabledServices[i] = svc
			}
		}
	}

	return remoteConfig, nil
}

func (r *ReconcileRemoteConfig) getRemoteCluster(remoteConfig *istiov1beta1.RemoteIstio, logger logr.Logger) (*remoteclusters.Cluster, error) {
	k8sconfig, err := r.getK8SConfigForCluster(remoteConfig.ObjectMeta.Namespace, remoteConfig.Name)
	if err != nil {
		return nil, err
	}

	logger.Info("k8s config found")

	cluster, err := remoteclusters.NewCluster(remoteConfig.Name, k8sconfig, logger)
	if err != nil {
		return nil, err
	}
	err = r.remoteClustersMgr.Add(cluster)
	if err != nil {
		return nil, err
	}

	return cluster, nil
}

func (r *ReconcileRemoteConfig) getK8SConfigForCluster(namespace string, name string) ([]byte, error) {
	var secret corev1.Secret
	err := r.Get(context.TODO(), client.ObjectKey{
		Namespace: namespace,
		Name:      name,
	}, &secret)
	if err != nil {
		return nil, err
	}

	for _, config := range secret.Data {
		return config, nil
	}

	return nil, errors.New("could not found k8s config")
}

func (r *ReconcileRemoteConfig) getIstio() (*istiov1beta1.Istio, error) {
	var istios istiov1beta1.IstioList
	err := r.List(context.TODO(), &client.ListOptions{}, &istios)
	if err != nil {
		return nil, err
	}

	if len(istios.Items) != 1 {
		return nil, errors.New("istio resource not found")
	}

	config := istios.Items[0]
	gvk := config.GroupVersionKind()
	gvk.Version = istiov1beta1.SchemeGroupVersion.Version
	gvk.Group = istiov1beta1.SchemeGroupVersion.Group
	gvk.Kind = "Istio"
	config.SetGroupVersionKind(gvk)

	istiov1beta1.SetDefaults(&config)

	return &config, nil
}

func getWatchPredicateForRemoteConfig() predicate.Funcs {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return true
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return true
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			old := e.ObjectOld.(*istiov1beta1.RemoteIstio)
			new := e.ObjectNew.(*istiov1beta1.RemoteIstio)
			if !reflect.DeepEqual(old.Spec, new.Spec) ||
				old.GetDeletionTimestamp() != new.GetDeletionTimestamp() ||
				old.GetGeneration() != new.GetGeneration() {
				return true
			}
			return false
		},
	}
}

func triggerRemoteIstios(mgr manager.Manager, object handler.MapObject, logger logr.Logger) []reconcile.Request {
	requests := make([]reconcile.Request, 0)
	remoteIstios := istioCtrl.GetRemoteIstiosByOwnerReference(mgr, object.Object, logger)

	for _, remoteIstio := range remoteIstios {
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Namespace: remoteIstio.Namespace,
				Name:      remoteIstio.Name,
			},
		})
	}

	var remoteIstiosWithoutOR istiov1beta1.RemoteIstioList
	err := mgr.GetClient().List(context.Background(), &client.ListOptions{}, &remoteIstiosWithoutOR)
	if err != nil {
		logger.Error(err, "could not list remote istio resources")
	}
	for _, remoteIstio := range remoteIstiosWithoutOR.Items {
		if len(remoteIstio.GetOwnerReferences()) == 0 {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Namespace: remoteIstio.Namespace,
					Name:      remoteIstio.Name,
				},
			})
		}
	}

	return requests
}

func RemoveFinalizersFromIstios(c client.Client) error {
	var remoteistios istiov1beta1.RemoteIstioList

	err := c.List(context.TODO(), &client.ListOptions{}, &remoteistios)
	if err != nil {
		return emperror.Wrap(err, "could not list Istio resources")
	}
	for _, remoteistio := range remoteistios.Items {
		remoteistio.ObjectMeta.Finalizers = util.RemoveString(remoteistio.ObjectMeta.Finalizers, finalizerID)
		if err := c.Update(context.Background(), &remoteistio); err != nil {
			return emperror.WrapWith(err, "could not remove finalizer from RemoteIstio resource", "name", remoteistio.GetName())
		}
	}

	return nil
}
