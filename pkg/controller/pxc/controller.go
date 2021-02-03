package pxc

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	"github.com/robfig/cron/v3"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1beta1 "k8s.io/api/policy/v1beta1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/percona/percona-xtradb-cluster-operator/clientcmd"
	api "github.com/percona/percona-xtradb-cluster-operator/pkg/apis/pxc/v1"
	"github.com/percona/percona-xtradb-cluster-operator/pkg/k8s"
	"github.com/percona/percona-xtradb-cluster-operator/pkg/pxc"
	"github.com/percona/percona-xtradb-cluster-operator/pkg/pxc/app/config"
	"github.com/percona/percona-xtradb-cluster-operator/pkg/pxc/app/statefulset"
	"github.com/percona/percona-xtradb-cluster-operator/version"
)

// Add creates a new PerconaXtraDBCluster Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	r, err := newReconciler(mgr)
	if err != nil {
		return err
	}

	return add(mgr, r)
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) (reconcile.Reconciler, error) {
	sv, err := version.Server()
	if err != nil {
		return nil, errors.Wrap(err, "get version")
	}
	cli, err := clientcmd.NewClient()
	if err != nil {
		return nil, errors.Wrap(err, "create clientcmd")
	}
	return &ReconcilePerconaXtraDBCluster{
		client:        mgr.GetClient(),
		scheme:        mgr.GetScheme(),
		crons:         NewCronRegistry(),
		serverVersion: sv,
		clientcmd:     cli,
		lockers:       newLockStore(),
	}, nil
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New("perconaxtradbcluster-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource PerconaXtraDBCluster
	err = c.Watch(&source.Kind{Type: &api.PerconaXtraDBCluster{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	return nil
}

var _ reconcile.Reconciler = &ReconcilePerconaXtraDBCluster{}

// ReconcilePerconaXtraDBCluster reconciles a PerconaXtraDBCluster object
type ReconcilePerconaXtraDBCluster struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client         client.Client
	scheme         *runtime.Scheme
	crons          CronRegistry
	clientcmd      *clientcmd.Client
	syncUsersState int32
	serverVersion  *version.ServerVersion
	lockers        lockStore
}

func (r *ReconcilePerconaXtraDBCluster) logger(name, namespace string) logr.Logger {
	return log.NewDelegatingLogger(log.NullLogger{}).WithName("controller_perconaxtradbcluster").
		WithValues("cluster name", name, "namespace", namespace)
}

type lockStore struct {
	store *sync.Map
}

func newLockStore() lockStore {
	return lockStore{
		store: new(sync.Map),
	}
}

func (l lockStore) LoadOrCreate(key string) lock {
	val, _ := l.store.LoadOrStore(key, lock{
		statusMutex: new(sync.Mutex),
		updateSync:  new(int32),
	})

	return val.(lock)
}

type lock struct {
	statusMutex *sync.Mutex
	updateSync  *int32
}

const (
	updateDone = 0
	updateWait = 1
)

type CronRegistry struct {
	crons *cron.Cron
	jobs  map[string]Shedule
}

type Shedule struct {
	ID          int
	CronShedule string
}

const (
	stateFree   = 0
	stateLocked = 1
)

func NewCronRegistry() CronRegistry {
	c := CronRegistry{
		crons: cron.New(),
		jobs:  make(map[string]Shedule),
	}

	c.crons.Start()

	return c
}

// Reconcile reads that state of the cluster for a PerconaXtraDBCluster object and makes changes based on the state read
// and what is in the PerconaXtraDBCluster.Spec
// Note:
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcilePerconaXtraDBCluster) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	rr := reconcile.Result{
		RequeueAfter: time.Second * 5,
	}

	// As operator can handle a few clusters
	// lock should be created per cluster to not lock cron jobs of other clusters
	l := r.lockers.LoadOrCreate(request.NamespacedName.String())

	// Fetch the PerconaXtraDBCluster instance
	// PerconaXtraDBCluster object is also accessed and changed by a version service's cron job (that run concurrently)
	l.statusMutex.Lock()
	defer l.statusMutex.Unlock()
	// we have to be sure the reconcile loop will be run at least once
	// in-between any version service jobs (hence any two vs jobs shouldn't be run sequentially).
	// the version service job sets the state to  `updateWait` and the next job can be run only
	// after the state was dropped to`updateDone` again
	defer atomic.StoreInt32(l.updateSync, updateDone)

	o := &api.PerconaXtraDBCluster{}
	err := r.client.Get(context.TODO(), request.NamespacedName, o)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			return rr, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	reqLogger := r.logger(o.Name, o.Namespace)

	// wait untill token issued to run PXC in data encrypted mode.
	if o.ShouldWaitForTokenIssue() {
		reqLogger.Info("wait for token issuing")
		return rr, nil
	}

	changed, err := o.CheckNSetDefaults(r.serverVersion)
	if err != nil {
		return reconcile.Result{}, errors.Wrap(err, "wrong PXC options")
	}

	defer func() {
		uerr := r.updateStatus(o, err)
		if uerr != nil {
			reqLogger.Error(uerr, "Update status")
		}
	}()

	if o.CompareVersionWith("1.7.0") >= 0 && *o.Spec.PXC.AutoRecovery {
		err = r.recoverFullClusterCrashIfNeeded(o)
		if err != nil {
			reqLogger.Error(err, "Failed to check if cluster needs to recover")
		}
	}

	err = r.reconcileUsersSecret(o)
	if err != nil {
		return reconcile.Result{}, errors.Wrap(err, "reconcile users secret")
	}
	var pxcAnnotations, proxysqlAnnotations map[string]string
	if o.CompareVersionWith("1.5.0") >= 0 {
		pxcAnnotations, proxysqlAnnotations, err = r.reconcileUsers(o)
		if err != nil {
			return rr, errors.Wrap(err, "reconcileUsers")
		}
	}

	r.resyncPXCUsersWithProxySQL(o)

	// update CR if there was changes that may be read by another cr (e.g. pxc-backup)
	if changed {
		err = r.client.Update(context.TODO(), o)
		if err != nil {
			return reconcile.Result{}, errors.Wrap(err, "update PXC CR")
		}
	}

	if o.Status.PXC.Version == "" || strings.HasSuffix(o.Status.PXC.Version, "intermediate") {
		err := r.ensurePXCVersion(o, VersionServiceClient{OpVersion: o.Version().String()})
		if err != nil {
			reqLogger.Info("failed to ensure version, running with default", "error", err)
		}
	}

	if o.ObjectMeta.DeletionTimestamp != nil {
		finalizers := []string{}
		for _, fnlz := range o.GetFinalizers() {
			var sfs api.StatefulApp
			switch fnlz {
			case "delete-proxysql-pvc":
				sfs = statefulset.NewProxy(o)
				// deletePVC is always true on this stage
				// because we never reach this point without finalizers
				err = r.deleteStatefulSet(o.Namespace, sfs, true)
			case "delete-pxc-pvc":
				sfs = statefulset.NewNode(o)
				err = r.deleteStatefulSet(o.Namespace, sfs, true)
			// nil error gonna be returned only when there is no more pods to delete (only 0 left)
			// until than finalizer won't be deleted
			case "delete-pxc-pods-in-order":
				err = r.deletePXCPods(o)
			}
			if err != nil {
				finalizers = append(finalizers, fnlz)
			}
		}

		o.SetFinalizers(finalizers)
		err = r.client.Update(context.TODO(), o)

		// object is being deleted, no need in further actions
		return rr, err
	}

	err = r.deploy(o)
	if err != nil {
		return reconcile.Result{}, err
	}

	operatorPod, err := k8s.OperatorPod(r.client)
	if err != nil {
		return reconcile.Result{}, errors.Wrap(err, "get operator deployment")
	}

	inits := []corev1.Container{}
	if o.CompareVersionWith("1.5.0") >= 0 {
		imageName := operatorPod.Spec.Containers[0].Image
		if o.CompareVersionWith(version.Version) != 0 {
			imageName = strings.Split(imageName, ":")[0] + ":" + o.Spec.CRVersion
		}
		var initResources *api.PodResources
		if o.CompareVersionWith("1.6.0") >= 0 {
			initResources = o.Spec.PXC.Resources
		}
		if len(o.Spec.InitImage) > 0 {
			imageName = o.Spec.InitImage
		}
		initC, err := statefulset.EntrypointInitContainer(imageName, initResources, o.Spec.PXC.ContainerSecurityContext)
		if err != nil {
			return reconcile.Result{}, err
		}
		inits = append(inits, initC)
	}

	pxcSet := statefulset.NewNode(o)
	pxc.MergeTemplateAnnotations(pxcSet.StatefulSet(), pxcAnnotations)
	err = r.updatePod(pxcSet, o.Spec.PXC.PodSpec, o, inits)
	if err != nil {
		return reconcile.Result{}, errors.Wrap(err, "pxc upgrade error")
	}

	for _, pxcService := range []*corev1.Service{pxc.NewServicePXC(o), pxc.NewServicePXCUnready(o)} {
		currentService := &corev1.Service{}
		err = r.client.Get(context.TODO(), types.NamespacedName{Name: pxcService.Name, Namespace: pxcService.Namespace}, currentService)
		if err != nil {
			return reconcile.Result{}, errors.Wrap(err, "failed to get current PXC service")
		}

		if reflect.DeepEqual(currentService.Spec.Ports, pxcService.Spec.Ports) {
			continue
		}

		currentService.Spec.Ports = pxcService.Spec.Ports

		err = r.client.Update(context.TODO(), currentService)
		if err != nil {
			return reconcile.Result{}, errors.Wrap(err, "PXC service upgrade error")
		}
	}

	if o.Spec.HAProxy != nil && o.Spec.HAProxy.Enabled {

		err := r.updatePod(statefulset.NewHAProxy(o), o.Spec.HAProxy, o, nil)
		if err != nil {
			return reconcile.Result{}, errors.Wrap(err, "HAProxy upgrade error")
		}

		oldHAProxyService := &corev1.Service{}
		err = r.client.Get(context.TODO(), pxc.HaproxyServiceNamespacedName(o), oldHAProxyService)
		if err != nil {
			return reconcile.Result{}, errors.Wrap(err, "failed to get HAProxy service")
		}

		newHAProxyService := oldHAProxyService.DeepCopy()

		ports := []corev1.ServicePort{
			{
				Port:       3306,
				Protocol:   corev1.ProtocolTCP,
				TargetPort: intstr.FromInt(3306),
				Name:       "mysql",
			},
			{
				Port:       3309,
				Protocol:   corev1.ProtocolTCP,
				TargetPort: intstr.FromInt(3309),
				Name:       "proxy-protocol",
			},
		}

		newHAProxyService.Spec.Ports = ports
		newHAProxyService.Spec.Type = corev1.ServiceTypeClusterIP
		if len(o.Spec.HAProxy.ServiceType) > 0 {
			newHAProxyService.Spec.Type = o.Spec.HAProxy.ServiceType
		}

		if newHAProxyService.Spec.Type == corev1.ServiceTypeLoadBalancer || newHAProxyService.Spec.Type == corev1.ServiceTypeNodePort {
			if len(o.Spec.HAProxy.ExternalTrafficPolicy) > 0 {
				newHAProxyService.Spec.ExternalTrafficPolicy = o.Spec.HAProxy.ExternalTrafficPolicy
			} else if newHAProxyService.Spec.ExternalTrafficPolicy != o.Spec.HAProxy.ExternalTrafficPolicy {
				newHAProxyService.Spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyTypeCluster
			}
		}

		if o.CompareVersionWith("1.6.0") >= 0 {
			newHAProxyService.Spec.Ports = append(ports,
				corev1.ServicePort{
					Port:       33062,
					Protocol:   corev1.ProtocolTCP,
					TargetPort: intstr.FromInt(33062),
					Name:       "mysql-admin",
				},
			)
		}

		if isServiceNeedsToUpdate(r.client, oldHAProxyService, newHAProxyService) {
			err = r.client.Update(context.Background(), newHAProxyService)
			if err != nil {
				return reconcile.Result{}, errors.Wrap(err, "HAProxy service upgrade")
			}
		}

		currentHAProxyServiceReplicas := &corev1.Service{}
		err = r.client.Get(context.TODO(), pxc.HAProxyReplicasNamespacedName(o), currentHAProxyServiceReplicas)
		if err != nil {
			return reconcile.Result{}, errors.Wrap(err, "failed to get HAProxyReplicas service")
		}

		newHAProxyServiceReplicas := currentHAProxyServiceReplicas.DeepCopy()

		replicaPorts := []corev1.ServicePort{
			{
				Port:       3306,
				Protocol:   corev1.ProtocolTCP,
				TargetPort: intstr.FromInt(3307),
				Name:       "mysql-replicas",
			},
		}

		newHAProxyServiceReplicas.Spec.Ports = replicaPorts
		newHAProxyServiceReplicas.Spec.Type = corev1.ServiceTypeClusterIP
		if len(o.Spec.HAProxy.ReplicasServiceType) > 0 {
			newHAProxyServiceReplicas.Spec.Type = o.Spec.HAProxy.ReplicasServiceType
		}

		if newHAProxyServiceReplicas.Spec.Type == corev1.ServiceTypeLoadBalancer || newHAProxyServiceReplicas.Spec.Type == corev1.ServiceTypeNodePort {
			if len(o.Spec.HAProxy.ReplicasExternalTrafficPolicy) > 0 {
				newHAProxyServiceReplicas.Spec.ExternalTrafficPolicy = o.Spec.HAProxy.ReplicasExternalTrafficPolicy
			} else {
				newHAProxyServiceReplicas.Spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyTypeCluster
			}
		}

		if isServiceNeedsToUpdate(r.client, currentHAProxyServiceReplicas, newHAProxyServiceReplicas) {
			err = r.client.Update(context.TODO(), newHAProxyServiceReplicas)
			if err != nil {
				return reconcile.Result{}, errors.Wrap(err, "HAProxyReplicas service upgrade error")
			}
		}
	} else {
		err = r.deleteStatefulSet(o.Namespace, statefulset.NewHAProxy(o), false)
		if err != nil {
			return reconcile.Result{}, errors.Wrap(err, "delete HAProxy stateful set")
		}
		err = r.deleteServices([]types.NamespacedName{pxc.HaproxyServiceNamespacedName(o), pxc.HAProxyReplicasNamespacedName(o)})
		if err != nil {
			return reconcile.Result{}, errors.Wrap(err, "delete HAProxy replica service")
		}
	}

	proxysqlSet := statefulset.NewProxy(o)
	pxc.MergeTemplateAnnotations(proxysqlSet.StatefulSet(), proxysqlAnnotations)
	proxysqlService := pxc.NewServiceProxySQL(o)

	if o.Spec.ProxySQL != nil && o.Spec.ProxySQL.Enabled {
		err = r.updatePod(proxysqlSet, o.Spec.ProxySQL, o, nil)
		if err != nil {
			return reconcile.Result{}, errors.Wrap(err, "ProxySQL upgrade error")
		}

		currentService := &corev1.Service{}
		err := r.client.Get(context.TODO(), types.NamespacedName{Name: proxysqlService.Name, Namespace: proxysqlService.Namespace}, currentService)
		if err != nil {
			return reconcile.Result{}, errors.Wrap(err, "failed to get current proxysql service sate")
		}

		ports := []corev1.ServicePort{
			{
				Port: 3306,
				Name: "mysql",
			},
		}

		if len(o.Spec.ProxySQL.ServiceType) > 0 {
			//Upgrading service only if something is changed
			if currentService.Spec.Type != o.Spec.ProxySQL.ServiceType {
				currentService.Spec.Ports = ports
				currentService.Spec.Type = o.Spec.ProxySQL.ServiceType
			}
			//Checking default ServiceType
		} else if currentService.Spec.Type != corev1.ServiceTypeClusterIP {
			currentService.Spec.Ports = ports
			currentService.Spec.Type = corev1.ServiceTypeClusterIP
		}

		if currentService.Spec.Type == corev1.ServiceTypeLoadBalancer || currentService.Spec.Type == corev1.ServiceTypeNodePort {
			if len(o.Spec.ProxySQL.ExternalTrafficPolicy) > 0 {
				currentService.Spec.ExternalTrafficPolicy = o.Spec.ProxySQL.ExternalTrafficPolicy
			} else if currentService.Spec.ExternalTrafficPolicy != o.Spec.ProxySQL.ExternalTrafficPolicy {
				currentService.Spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyTypeCluster
			}
		}

		if o.CompareVersionWith("1.6.0") >= 0 {
			currentService.Spec.Ports = append(
				ports,
				corev1.ServicePort{
					Port: 33062,
					Name: "mysql-admin",
				},
			)
		}

		err = r.client.Update(context.TODO(), currentService)
		if err != nil {
			return reconcile.Result{}, errors.Wrap(err, "ProxySQL service upgrade error")
		}
	} else {
		// check if there is need to delete pvc
		deletePVC := false
		for _, fnlz := range o.GetFinalizers() {
			if fnlz == "delete-proxysql-pvc" {
				deletePVC = true
				break
			}
		}

		err = r.deleteStatefulSet(o.Namespace, proxysqlSet, deletePVC)
		if err != nil {
			return reconcile.Result{}, err
		}
		err = r.deleteServices([]types.NamespacedName{pxc.ProxySQLServiceNamespacedName(o), pxc.ProxySQLUnreadyServiceNamespacedName(o)})
		if err != nil {
			return reconcile.Result{}, err
		}
	}

	err = r.reconcileBackups(o)
	if err != nil {
		return reconcile.Result{}, err
	}

	if err := r.fetchVersionFromPXC(o, pxcSet); err != nil {
		return rr, errors.Wrap(err, "update CR version")
	}

	err = r.sheduleEnsurePXCVersion(o, VersionServiceClient{OpVersion: o.Version().String()})
	if err != nil {
		return reconcile.Result{}, errors.Wrap(err, "failed to ensure version")
	}

	return rr, nil
}

func (r *ReconcilePerconaXtraDBCluster) deploy(cr *api.PerconaXtraDBCluster) error {
	stsApp := statefulset.NewNode(cr)
	err := r.reconcileConfigMap(cr)
	if err != nil {
		return err
	}

	operatorPod, err := k8s.OperatorPod(r.client)
	if err != nil {
		return errors.Wrap(err, "get operator deployment")
	}

	inits := []corev1.Container{}
	if cr.CompareVersionWith("1.5.0") >= 0 {
		imageName := operatorPod.Spec.Containers[0].Image
		if cr.CompareVersionWith(version.Version) != 0 {
			imageName = strings.Split(imageName, ":")[0] + ":" + cr.Spec.CRVersion
		}
		var initResources *api.PodResources
		if cr.CompareVersionWith("1.6.0") >= 0 {
			initResources = cr.Spec.PXC.Resources
		}
		if len(cr.Spec.InitImage) > 0 {
			imageName = cr.Spec.InitImage
		}
		initC, err := statefulset.EntrypointInitContainer(imageName, initResources, cr.Spec.PXC.ContainerSecurityContext)
		if err != nil {
			return err
		}
		inits = append(inits, initC)
	}

	nodeSet, err := pxc.StatefulSet(stsApp, cr.Spec.PXC.PodSpec, cr, inits)
	if err != nil {
		return errors.Wrap(err, "get pxc statefulset")
	}

	// TODO: code duplication with updatePod function
	if nodeSet.Spec.Template.Annotations == nil {
		nodeSet.Spec.Template.Annotations = make(map[string]string)
	}
	if cr.CompareVersionWith("1.1.0") >= 0 {
		nodeSet.Spec.Template.Annotations["percona.com/configuration-hash"] = r.getConfigHash(cr, stsApp)
	}

	err = r.reconsileSSL(cr)
	if err != nil {
		return errors.Wrapf(err, "failed to reconcile SSL.Please create your TLS secret %s and %s manually or setup cert-manager correctly",
			cr.Spec.PXC.SSLSecretName, cr.Spec.PXC.SSLInternalSecretName)
	}

	sslHash, err := r.getSecretHash(cr, cr.Spec.PXC.SSLSecretName, cr.Spec.AllowUnsafeConfig)
	if err != nil {
		return errors.Wrap(err, "get secret hash")
	}
	if sslHash != "" && cr.CompareVersionWith("1.1.0") >= 0 {
		nodeSet.Spec.Template.Annotations["percona.com/ssl-hash"] = sslHash
	}

	sslInternalHash, err := r.getSecretHash(cr, cr.Spec.PXC.SSLInternalSecretName, cr.Spec.AllowUnsafeConfig)
	if err != nil && !k8serrors.IsNotFound(err) {
		return errors.Wrap(err, "get internal secret hash")
	}
	if sslInternalHash != "" && cr.CompareVersionWith("1.1.0") >= 0 {
		nodeSet.Spec.Template.Annotations["percona.com/ssl-internal-hash"] = sslInternalHash
	}

	vaultConfigHash, err := r.getSecretHash(cr, cr.Spec.VaultSecretName, true)
	if err != nil {
		return errors.Wrap(err, "get vault config hash")
	}
	if vaultConfigHash != "" && cr.CompareVersionWith("1.6.0") >= 0 {
		nodeSet.Spec.Template.Annotations["percona.com/vault-config-hash"] = vaultConfigHash
	}

	err = setControllerReference(cr, nodeSet, r.scheme)
	if err != nil {
		return err
	}

	err = r.client.Create(context.TODO(), nodeSet)
	if err != nil && !k8serrors.IsAlreadyExists(err) {
		return errors.Wrap(err, "create newStatefulSetNode")
	}

	err = r.createService(cr, pxc.NewServicePXCUnready(cr))
	if err != nil {
		return errors.Wrap(err, "create PXC ServiceUnready")
	}
	err = r.createService(cr, pxc.NewServicePXC(cr))
	if err != nil {
		return errors.Wrap(err, "create PXC Service")
	}

	// PodDisruptionBudget object for nodes
	err = r.client.Get(context.TODO(), types.NamespacedName{Name: nodeSet.Name, Namespace: nodeSet.Namespace}, nodeSet)
	if err == nil {
		err := r.reconcilePDB(cr.Spec.PXC.PodDisruptionBudget, stsApp, cr.Namespace, nodeSet)
		if err != nil {
			return errors.Wrapf(err, "PodDisruptionBudget for %s", nodeSet.Name)
		}
	} else if !k8serrors.IsNotFound(err) {
		return errors.Wrap(err, "get PXC stateful set")
	}

	// HAProxy StatefulSet
	if cr.Spec.HAProxy != nil && cr.Spec.HAProxy.Enabled {
		sfsHAProxy := statefulset.NewHAProxy(cr)
		haProxySet, err := pxc.StatefulSet(sfsHAProxy, cr.Spec.HAProxy, cr, nil)
		if err != nil {
			return errors.Wrap(err, "create HAProxy StatefulSet")
		}
		err = setControllerReference(cr, haProxySet, r.scheme)
		if err != nil {
			return err
		}

		// TODO: code duplication with updatePod function
		if haProxySet.Spec.Template.Annotations == nil {
			haProxySet.Spec.Template.Annotations = make(map[string]string)
		}
		if nodeSet.Spec.Template.Annotations == nil {
			nodeSet.Spec.Template.Annotations = make(map[string]string)
		}
		haProxySet.Spec.Template.Annotations["percona.com/configuration-hash"] = r.getConfigHash(cr, sfsHAProxy)
		if cr.CompareVersionWith("1.5.0") == 0 {
			if sslHash != "" {
				haProxySet.Spec.Template.Annotations["percona.com/ssl-hash"] = sslHash
			}
			if sslInternalHash != "" {
				haProxySet.Spec.Template.Annotations["percona.com/ssl-internal-hash"] = sslInternalHash
			}
		}
		err = r.client.Create(context.TODO(), haProxySet)
		if err != nil && !k8serrors.IsAlreadyExists(err) {
			return errors.Wrap(err, "create newStatefulSetHAProxy")
		}

		//HAProxy Service
		err = r.createService(cr, pxc.NewServiceHAProxy(cr))
		if err != nil {
			return errors.Wrap(err, "create HAProxy Service")
		}

		//HAProxyReplicas Service
		err = r.createService(cr, pxc.NewServiceHAProxyReplicas(cr))
		if err != nil {
			return errors.Wrap(err, "create HAProxyReplicas Service")
		}

		// PodDisruptionBudget object for HAProxy
		err = r.client.Get(context.TODO(), types.NamespacedName{Name: haProxySet.Name, Namespace: haProxySet.Namespace}, haProxySet)
		if err == nil {
			err := r.reconcilePDB(cr.Spec.HAProxy.PodDisruptionBudget, sfsHAProxy, cr.Namespace, haProxySet)
			if err != nil {
				return errors.Wrapf(err, "PodDisruptionBudget for %s", haProxySet.Name)
			}
		} else if !k8serrors.IsNotFound(err) {
			return errors.Wrap(err, "get HAProxy stateful set")
		}
	}

	if cr.Spec.ProxySQL != nil && cr.Spec.ProxySQL.Enabled {
		sfsProxy := statefulset.NewProxy(cr)
		proxySet, err := pxc.StatefulSet(sfsProxy, cr.Spec.ProxySQL, cr, nil)
		if err != nil {
			return errors.Wrap(err, "create ProxySQL Service")
		}
		err = setControllerReference(cr, proxySet, r.scheme)
		if err != nil {
			return err
		}

		// TODO: code duplication with updatePod function
		if proxySet.Spec.Template.Annotations == nil {
			proxySet.Spec.Template.Annotations = make(map[string]string)
		}
		if nodeSet.Spec.Template.Annotations == nil {
			nodeSet.Spec.Template.Annotations = make(map[string]string)
		}
		if cr.CompareVersionWith("1.1.0") >= 0 {
			proxySet.Spec.Template.Annotations["percona.com/configuration-hash"] = r.getConfigHash(cr, sfsProxy)
			if sslHash != "" {
				proxySet.Spec.Template.Annotations["percona.com/ssl-hash"] = sslHash
			}
			if sslInternalHash != "" {
				proxySet.Spec.Template.Annotations["percona.com/ssl-internal-hash"] = sslInternalHash
			}
		}

		err = r.client.Create(context.TODO(), proxySet)
		if err != nil && !k8serrors.IsAlreadyExists(err) {
			return errors.Wrap(err, "create newStatefulSetProxySQL")
		}

		// ProxySQL Service
		err = r.createService(cr, pxc.NewServiceProxySQL(cr))
		if err != nil {
			return errors.Wrap(err, "create ProxySQL Service")
		}

		// ProxySQL Unready Service
		err = r.createService(cr, pxc.NewServiceProxySQLUnready(cr))
		if err != nil {
			return errors.Wrap(err, "create ProxySQL ServiceUnready")
		}

		// PodDisruptionBudget object for ProxySQL
		err = r.client.Get(context.TODO(), types.NamespacedName{Name: proxySet.Name, Namespace: proxySet.Namespace}, proxySet)
		if err == nil {
			err := r.reconcilePDB(cr.Spec.ProxySQL.PodDisruptionBudget, sfsProxy, cr.Namespace, proxySet)
			if err != nil {
				return errors.Wrapf(err, "PodDisruptionBudget for %s", proxySet.Name)
			}
		} else if !k8serrors.IsNotFound(err) {
			return errors.Wrap(err, "get ProxySQL stateful set")
		}
	}

	return nil
}

func (r *ReconcilePerconaXtraDBCluster) createService(cr *api.PerconaXtraDBCluster, svc *corev1.Service) error {
	err := setControllerReference(cr, svc, r.scheme)
	if err != nil {
		return errors.Wrap(err, "setControllerReference")
	}

	err = r.client.Get(context.TODO(), types.NamespacedName{Name: svc.Name, Namespace: svc.Namespace}, &corev1.Service{})
	if err != nil && k8serrors.IsNotFound(err) {
		err := r.client.Create(context.TODO(), svc)
		return errors.WithMessage(err, "create")
	}

	return errors.WithMessage(err, "check if exists")
}

func (r *ReconcilePerconaXtraDBCluster) reconcileConfigMap(cr *api.PerconaXtraDBCluster) error {
	stsApp := statefulset.NewNode(cr)
	ls := stsApp.Labels()
	limitMemory := ""
	requestMemory := ""

	if cr.Spec.PXC.Resources != nil {
		if cr.Spec.PXC.Resources.Limits != nil {
			if cr.Spec.PXC.Resources.Limits.Memory != "" {
				limitMemory = cr.Spec.PXC.Resources.Limits.Memory
			}
		}
		if cr.Spec.PXC.Resources.Requests != nil {
			if cr.Spec.PXC.Resources.Requests.Memory != "" {
				requestMemory = cr.Spec.PXC.Resources.Requests.Memory
			}
		}
	}
	if cr.CompareVersionWith("1.3.0") >= 0 {
		if len(limitMemory) > 0 || len(requestMemory) > 0 {
			configMap, err := config.NewAutoTuneConfigMap(cr, "auto-"+ls["app.kubernetes.io/instance"]+"-"+ls["app.kubernetes.io/component"])
			if err != nil {
				return errors.Wrap(err, "new auto-config map")
			}
			err = setControllerReference(cr, configMap, r.scheme)
			if err != nil {
				return errors.Wrap(err, "set auto-config controller ref")
			}

			err = createOrUpdateConfigmap(r.client, configMap)
			if err != nil {
				return errors.Wrap(err, "auto-config config map")
			}
		}
	}

	if cr.Spec.PXC.Configuration != "" {
		configMap := config.NewConfigMap(cr, ls["app.kubernetes.io/instance"]+"-"+ls["app.kubernetes.io/component"], "init.cnf", cr.Spec.PXC.Configuration)
		err := setControllerReference(cr, configMap, r.scheme)
		if err != nil {
			return errors.Wrap(err, "set controller ref")
		}

		err = createOrUpdateConfigmap(r.client, configMap)
		if err != nil {
			return errors.Wrap(err, "pxc config map")
		}
	}

	if cr.Spec.ProxySQL != nil && cr.Spec.ProxySQL.Enabled && cr.Spec.ProxySQL.Configuration != "" {
		configMap := config.NewConfigMap(cr, ls["app.kubernetes.io/instance"]+"-proxysql", "proxysql.cnf", cr.Spec.ProxySQL.Configuration)
		err := setControllerReference(cr, configMap, r.scheme)
		if err != nil {
			return errors.Wrap(err, "set controller ref ProxySQL")
		}

		err = createOrUpdateConfigmap(r.client, configMap)
		if err != nil {
			return errors.Wrap(err, "proxysql config map")
		}
	}

	if cr.Spec.HAProxy != nil && cr.Spec.HAProxy.Enabled && cr.Spec.HAProxy.Configuration != "" {
		configMap := config.NewConfigMap(cr, ls["app.kubernetes.io/instance"]+"-haproxy", "haproxy-global.cfg", cr.Spec.HAProxy.Configuration)
		err := setControllerReference(cr, configMap, r.scheme)
		if err != nil {
			return errors.Wrap(err, "set controller ref HAProxy")
		}

		err = createOrUpdateConfigmap(r.client, configMap)
		if err != nil {
			return errors.Wrap(err, "haproxy config map")
		}
	}

	if cr.Spec.LogCollector != nil && cr.Spec.LogCollector.Configuration != "" && cr.CompareVersionWith("1.7.0") >= 0 {
		configMap := config.NewConfigMap(cr, ls["app.kubernetes.io/instance"]+"-logcollector", "fluentbit_custom.conf", cr.Spec.LogCollector.Configuration)
		err := setControllerReference(cr, configMap, r.scheme)
		if err != nil {
			return errors.Wrap(err, "set controller ref LogCollector")
		}
		err = r.client.Create(context.TODO(), configMap)
		if err != nil && k8serrors.IsAlreadyExists(err) {
			err = r.client.Update(context.TODO(), configMap)
			if err != nil {
				return errors.Wrap(err, "update ConfigMap LogCollector")
			}
		} else if err != nil {
			return errors.Wrap(err, "create ConfigMap LogCollector")
		}
	}

	return nil
}

func (r *ReconcilePerconaXtraDBCluster) reconcilePDB(spec *api.PodDisruptionBudgetSpec, sfs api.StatefulApp, namespace string, owner runtime.Object) error {
	if spec == nil {
		return nil
	}

	pdb := pxc.PodDisruptionBudget(spec, sfs, namespace)
	err := setControllerReference(owner, pdb, r.scheme)
	if err != nil {
		return errors.Wrap(err, "set owner reference")
	}

	cpdb := &policyv1beta1.PodDisruptionBudget{}
	err = r.client.Get(context.TODO(), types.NamespacedName{Name: pdb.Name, Namespace: namespace}, cpdb)
	if err != nil && k8serrors.IsNotFound(err) {
		return r.client.Create(context.TODO(), pdb)
	} else if err != nil {
		return errors.Wrap(err, "reconcile PDB")
	}

	cpdb.Spec = pdb.Spec
	return r.client.Update(context.TODO(), cpdb)
}

func (r *ReconcilePerconaXtraDBCluster) deletePXCPods(cr *api.PerconaXtraDBCluster) error {
	sfs := statefulset.NewNode(cr)
	err := r.deleteStatefulSetPods(cr.Namespace, sfs)
	if err != nil {
		return errors.Wrap(err, "delete statefulset pods")
	}
	if cr.Spec.Backup != nil && cr.Spec.Backup.PITR.Enabled {
		return errors.Wrap(r.deletePITR(cr), "delete pitr pod")
	}

	return nil
}

func (r *ReconcilePerconaXtraDBCluster) deleteStatefulSetPods(namespace string, sfs api.StatefulApp) error {
	list := corev1.PodList{}

	err := r.client.List(context.TODO(),
		&list,
		&client.ListOptions{
			Namespace:     namespace,
			LabelSelector: labels.SelectorFromSet(sfs.Labels()),
		},
	)
	if err != nil {
		return errors.Wrap(err, "get pod list")
	}

	// the last pod left - we can leave it for the stateful set
	if len(list.Items) <= 1 {
		time.Sleep(time.Second * 3)
		return nil
	}

	// after setting the pods for delete we need to downscale statefulset to 1 under,
	// otherwise it will be trying to deploy the nodes again to reach the desired replicas count
	cSet := sfs.StatefulSet()
	err = r.client.Get(context.TODO(), types.NamespacedName{Name: cSet.Name, Namespace: cSet.Namespace}, cSet)
	if err != nil {
		return errors.Wrap(err, "get StatefulSet")
	}

	dscaleTo := int32(1)
	cSet.Spec.Replicas = &dscaleTo
	err = r.client.Update(context.TODO(), cSet)
	if err != nil {
		return errors.Wrap(err, "downscale StatefulSet")
	}

	return errors.New("waiting for pods to be deleted")
}

func (r *ReconcilePerconaXtraDBCluster) deleteStatefulSet(namespace string, sfs api.StatefulApp, deletePVC bool) error {
	err := r.client.Get(context.TODO(), types.NamespacedName{
		Name:      sfs.StatefulSet().Name,
		Namespace: namespace,
	}, &appsv1.StatefulSet{})
	if err != nil && !k8serrors.IsNotFound(err) {
		return errors.Wrapf(err, "get statefulset: %s", sfs.StatefulSet().Name)
	}

	if k8serrors.IsNotFound(err) {
		return nil
	}

	err = r.client.Delete(context.TODO(), sfs.StatefulSet())
	if err != nil && !k8serrors.IsNotFound(err) {
		return errors.Wrapf(err, "delete statefulset: %s", sfs.StatefulSet().Name)
	}
	if deletePVC {
		err = r.deletePVC(namespace, sfs.Labels())
		if err != nil {
			return errors.Wrapf(err, "delete pvc: %s", sfs.StatefulSet().Name)
		}
	}

	return nil
}

func (r *ReconcilePerconaXtraDBCluster) deleteServices(svcs []types.NamespacedName) error {
	for _, svcName := range svcs {
		svc := &corev1.Service{}
		err := r.client.Get(context.TODO(), svcName, svc)
		if err != nil && !k8serrors.IsNotFound(err) {
			return errors.Wrapf(err, "get service: %s", svcName.Name)
		}

		if k8serrors.IsNotFound(err) {
			continue
		}

		err = r.client.Delete(context.TODO(), svc)
		if err != nil {
			return errors.Wrapf(err, "delete service: %s", svcName.Name)
		}
	}
	return nil
}

func (r *ReconcilePerconaXtraDBCluster) deletePVC(namespace string, lbls map[string]string) error {
	list := corev1.PersistentVolumeClaimList{}
	err := r.client.List(context.TODO(),
		&list,
		&client.ListOptions{
			Namespace:     namespace,
			LabelSelector: labels.SelectorFromSet(lbls),
		},
	)
	if err != nil {
		return errors.Wrap(err, "get PVC list")
	}

	for _, pvc := range list.Items {
		err := r.client.Delete(context.TODO(), &pvc)
		if err != nil {
			return errors.Wrapf(err, "delete PVC %s", pvc.Name)
		}
	}

	return nil
}

func setControllerReference(ro runtime.Object, obj metav1.Object, scheme *runtime.Scheme) error {
	ownerRef, err := OwnerRef(ro, scheme)
	if err != nil {
		return err
	}
	obj.SetOwnerReferences(append(obj.GetOwnerReferences(), ownerRef))
	return nil
}

// OwnerRef returns OwnerReference to object
func OwnerRef(ro runtime.Object, scheme *runtime.Scheme) (metav1.OwnerReference, error) {
	gvk, err := apiutil.GVKForObject(ro, scheme)
	if err != nil {
		return metav1.OwnerReference{}, err
	}

	trueVar := true

	ca, err := meta.Accessor(ro)
	if err != nil {
		return metav1.OwnerReference{}, err
	}

	return metav1.OwnerReference{
		APIVersion: gvk.GroupVersion().String(),
		Kind:       gvk.Kind,
		Name:       ca.GetName(),
		UID:        ca.GetUID(),
		Controller: &trueVar,
	}, nil
}

// resyncPXCUsersWithProxySQL calls the method of synchronizing users and makes sure that only one Goroutine works at a time
func (r *ReconcilePerconaXtraDBCluster) resyncPXCUsersWithProxySQL(cr *api.PerconaXtraDBCluster) {
	if cr.Spec.ProxySQL == nil || !cr.Spec.ProxySQL.Enabled {
		return
	}
	if cr.Status.Status != api.AppStateReady || !atomic.CompareAndSwapInt32(&r.syncUsersState, stateFree, stateLocked) {
		return
	}
	go func() {
		err := r.syncPXCUsersWithProxySQL(cr)
		if err != nil && !k8serrors.IsNotFound(err) {
			r.logger(cr.Name, cr.Namespace).Error(err, "sync users")
		}
		atomic.StoreInt32(&r.syncUsersState, stateFree)
	}()
}

func createOrUpdateConfigmap(cl client.Client, configMap *corev1.ConfigMap) error {
	currMap := &corev1.ConfigMap{}
	err := cl.Get(context.TODO(), types.NamespacedName{
		Namespace: configMap.Namespace,
		Name:      configMap.Name,
	}, currMap)
	if err != nil && !k8serrors.IsNotFound(err) {
		return errors.Wrap(err, "get current configmap")
	}

	if k8serrors.IsNotFound(err) {
		return cl.Create(context.TODO(), configMap)
	}

	if !reflect.DeepEqual(currMap.Data, configMap.Data) {
		return cl.Update(context.TODO(), configMap)
	}

	return nil
}

func isServiceNeedsToUpdate(cl client.Client, old, new *corev1.Service) bool {
	if !isServicesSpecEqual(old.Spec, new.Spec) || !reflect.DeepEqual(old.Annotations, new.Annotations) ||
		!reflect.DeepEqual(old.Labels, new.Labels) || !reflect.DeepEqual(new.Spec.Selector, old.Spec.Selector) {

		return true
	}
	return false
}

func isServicesSpecEqual(old, new corev1.ServiceSpec) bool {
	if old.Type != new.Type || !isServicesPortsEqual(old.Ports, new.Ports) ||
		old.ExternalTrafficPolicy != new.ExternalTrafficPolicy ||
		!reflect.DeepEqual(old.LoadBalancerSourceRanges, new.LoadBalancerSourceRanges) {

		return false
	}
	return true
}

func isServicesPortsEqual(old, new []corev1.ServicePort) bool {
	if len(old) != len(new) {
		return false
	}

	// sort slice to be able to compare elements by index
	sort.Slice(old, func(i, j int) bool {
		return old[i].Name < old[j].Name
	})

	sort.Slice(new, func(i, j int) bool {
		return new[i].Name < new[j].Name
	})

	for i := 0; i < len(old); i++ {
		oldSpec, newSpec := old[i], new[i]
		if oldSpec.Name != newSpec.Name || oldSpec.Port != newSpec.Port || oldSpec.Protocol != newSpec.Protocol ||
			oldSpec.TargetPort.IntVal != newSpec.TargetPort.IntVal {

			return false
		}
	}
	return true
}
