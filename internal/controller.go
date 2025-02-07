package internal

import (
	"context"
	"fmt"
	"strings"
	"time"

	v1 "github.com/K-Phoen/dark/internal/pkg/apis/controller/v1"
	clientset "github.com/K-Phoen/dark/internal/pkg/generated/clientset/versioned"
	samplescheme "github.com/K-Phoen/dark/internal/pkg/generated/clientset/versioned/scheme"
	informers "github.com/K-Phoen/dark/internal/pkg/generated/informers/externalversions/controller/v1"
	listers "github.com/K-Phoen/dark/internal/pkg/generated/listers/controller/v1"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
)

const controllerAgentName = "dark-controller"

const (
	// SuccessSynced is used as part of the Event 'reason' when a GrafanaDashboard is synced
	SuccessSynced = "Synced"

	// WarningNotSynced is used as part of the Event 'reason' when a GrafanaDashboard could not be synced
	WarningNotSynced = "Not synced"

	// MessageResourceSynced is the message used for an Event fired when a GrafanaDashboard
	// is synced successfully
	MessageResourceSynced = "GrafanaDashboard synced successfully"
)

type dashboardCreator interface {
	FromRawSpec(folderName string, uid string, rawJSON []byte) error
	Delete(uid string) error
}

// Controller is the controller implementation for GrafanaDashboard resources
type Controller struct {
	logger *zap.Logger

	// kubeclientset is a standard kubernetes clientset
	kubeclientset kubernetes.Interface
	// darkClientSet is a clientset for our own API group
	darkClientSet clientset.Interface

	dashboardsLister listers.GrafanaDashboardLister
	dashboardsSynced cache.InformerSynced

	// workqueue is a rate limited work queue. This is used to queue work to be
	// processed instead of performing it as soon as a change happens. This
	// means we can ensure we only process a fixed amount of resources at a
	// time, and makes it easy to ensure we are never processing the same item
	// simultaneously in two different workers.
	workqueue workqueue.RateLimitingInterface
	// recorder is an event recorder for recording Event resources to the
	// Kubernetes API.
	recorder record.EventRecorder

	dashboardCreator dashboardCreator
}

// NewController returns a new sample controller
func NewController(logger *zap.Logger, kubeclientset kubernetes.Interface, darkClientSet clientset.Interface, dashboardInformer informers.GrafanaDashboardInformer, dashboardCreator dashboardCreator) *Controller {
	// Create event broadcaster
	// Add dark-controller types to the default Kubernetes Scheme so Events can be
	// logged for dark-controller types.
	utilruntime.Must(samplescheme.AddToScheme(scheme.Scheme))
	logger.Info("creating event broadcaster")
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(func(format string, args ...interface{}) {
		logger.Info(fmt.Sprintf(format, args...))
	})
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeclientset.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: controllerAgentName})

	controller := &Controller{
		logger:           logger,
		kubeclientset:    kubeclientset,
		darkClientSet:    darkClientSet,
		dashboardsLister: dashboardInformer.Lister(),
		dashboardsSynced: dashboardInformer.Informer().HasSynced,
		workqueue:        workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "GrafanaDashboards"),
		recorder:         recorder,
		dashboardCreator: dashboardCreator,
	}

	logger.Info("setting up event handlers")
	// Set up an event handler for when GrafanaDashboard resources change
	dashboardInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueueDashboard,
		UpdateFunc: func(old, new interface{}) {
			newDashboard := new.(*v1.GrafanaDashboard)
			oldDashboard := old.(*v1.GrafanaDashboard)

			if newDashboard.ResourceVersion == oldDashboard.ResourceVersion {
				// Periodic resync will send update events for all known Deployments.
				// Two different versions of the same Deployment will always have different RVs.
				return
			}

			controller.enqueueDashboard(new)
		},
		DeleteFunc: controller.enqueueDeletion,
	})

	return controller
}

// Run will set up the event handlers for types we are interested in, as well
// as syncing informer caches and starting workers. It will block until stopCh
// is closed, at which point it will shutdown the work-queue and wait for
// workers to finish processing their current work items.
func (c *Controller) Run(threadiness int, stopCh <-chan struct{}) error {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()

	// Start the informer factories to begin populating the informer caches
	c.logger.Info("starting main controller loop")

	// Wait for the caches to be synced before starting workers
	c.logger.Info("waiting for informer caches to sync")
	if ok := cache.WaitForCacheSync(stopCh, c.dashboardsSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	c.logger.Info("starting controller workers")
	// Launch two workers to process GrafanaDashboard resources
	for i := 0; i < threadiness; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	c.logger.Info("started workers")
	<-stopCh
	c.logger.Info("shutting down workers")

	return nil
}

// runWorker is a long-running function that will continually call the
// processNextWorkItem function in order to read and process a message on the
// workqueue.
func (c *Controller) runWorker() {
	for c.processNextWorkItem() {
	}
}

// processNextWorkItem will read a single work item off the workqueue and
// attempt to process it, by calling the syncHandler.
func (c *Controller) processNextWorkItem() bool {
	obj, shutdown := c.workqueue.Get()

	if shutdown {
		return false
	}

	// We wrap this block in a func so we can defer c.workqueue.Done.
	err := func(obj interface{}) error {
		// We call Done here so the workqueue knows we have finished
		// processing this item. We also must remember to call Forget if we
		// do not want this work item being re-queued. For example, we do
		// not call Forget if a transient error occurs, instead the item is
		// put back on the workqueue and attempted again after a back-off
		// period.
		defer c.workqueue.Done(obj)

		ctx := context.Background()

		var key string
		var ok bool
		// We expect strings to come off the workqueue. These are of the
		// form namespace/name. We do this as the delayed nature of the
		// workqueue means the items in the informer cache may actually be
		// more up to date that when the item was initially put onto the
		// workqueue.
		if key, ok = obj.(string); !ok {
			// As the item in the workqueue is actually invalid, we call
			// Forget here else we'd go into a loop of attempting to
			// process a work item that is invalid.
			c.workqueue.Forget(obj)
			utilruntime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}

		if strings.HasPrefix(key, "deletion") {
			parts := strings.Split(key, ":")

			// Run the deletionHandler, passing it the uid of the dashboard to delete.
			c.deletionHandler(parts[1])

			c.logger.Info("successfully deleted dashboard", zap.String("key", key))
		} else {
			// Run the syncHandler, passing it the namespace/name string of the
			// GrafanaDashboard resource to be synced.
			if err := c.syncHandler(ctx, key); err != nil {
				// Put the item back on the work-queue to handle any transient errors.
				c.workqueue.AddRateLimited(key)
				return fmt.Errorf("error syncing '%s': %w, requeuing", key, err)
			}

			c.logger.Info("Successfully synced dashboard", zap.String("key", key))
		}

		// Finally, if no error occurs we Forget this item so it does not
		// get queued again until another change happens.
		c.workqueue.Forget(obj)
		return nil
	}(obj)

	if err != nil {
		utilruntime.HandleError(err)
		return true
	}

	return true
}

// syncHandler compares the actual state with the desired, and attempts to
// converge the two. It then updates the Status block of the GrafanaDashboard resource
// with the current status of the resource.
func (c *Controller) syncHandler(ctx context.Context, key string) error {
	// Convert the namespace/name string into a distinct namespace and name
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	// Get the GrafanaDashboard resource with this namespace/name
	dashboard, err := c.dashboardsLister.GrafanaDashboards(namespace).Get(name)
	if err != nil {
		// The GrafanaDashboard resource may no longer exist, in which case we stop
		// processing.
		if errors.IsNotFound(err) {
			utilruntime.HandleError(fmt.Errorf("dashboard '%s' in work queue no longer exists", key))
			c.recorder.Event(dashboard, corev1.EventTypeWarning, WarningNotSynced, fmt.Sprintf("dashboard '%s' in work queue no longer exists", key))
			return nil
		}

		return err
	}

	if err := c.dashboardCreator.FromRawSpec(dashboard.Folder, dashboard.ObjectMeta.Name, dashboard.Spec.Raw); err != nil {
		utilruntime.HandleError(fmt.Errorf("could not create '%s' dashboard from spec: %w", dashboard.ObjectMeta.Name, err))
		c.recorder.Event(dashboard, corev1.EventTypeWarning, WarningNotSynced, fmt.Sprintf("could not create dashboard from spec: %s", err))
		c.updateDashboardStatus(ctx, dashboard, err)
		c.logger.Warn("error while updating dashboard", zap.Error(err), zap.String("namespace", dashboard.Namespace), zap.String("name", dashboard.Name))

		return nil
	}

	c.recorder.Event(dashboard, corev1.EventTypeNormal, SuccessSynced, MessageResourceSynced)
	c.updateDashboardStatus(ctx, dashboard, nil)

	return nil
}

func (c *Controller) updateDashboardStatus(ctx context.Context, dashboard *v1.GrafanaDashboard, err error) {
	// NEVER modify objects from the store. It's a read-only, local cache.
	// You can use DeepCopy() to make a deep copy of original object and modify this copy
	// Or create a copy manually for better performance
	dashboardCopy := dashboard.DeepCopy()

	if err == nil {
		dashboardCopy.Status.Status = "OK"
		dashboardCopy.Status.Message = "Synchronized"
	} else {
		dashboardCopy.Status.Status = "Error"
		dashboardCopy.Status.Message = err.Error()
	}

	// If the CustomResourceSubresources feature gate is not enabled,
	// we must use Update instead of UpdateStatus to update the Status block of the Foo resource.
	// UpdateStatus will not allow changes to the Spec of the resource,
	// which is ideal for ensuring nothing other than resource status has been updated.
	_, err = c.darkClientSet.ControllerV1().GrafanaDashboards(dashboardCopy.Namespace).UpdateStatus(ctx, dashboardCopy, metav1.UpdateOptions{})
	if err != nil {
		c.logger.Warn("error while updating dashboard status", zap.Error(err), zap.String("namespace", dashboard.Namespace), zap.String("name", dashboard.Name))
	}
}

func (c *Controller) deletionHandler(uid string) {
	if err := c.dashboardCreator.Delete(uid); err != nil {
		utilruntime.HandleError(fmt.Errorf("dashboard '%s' in work queue could not be deleted", uid))
	}
}

// enqueueDashboard takes a GrafanaDashboard resource and converts it into a namespace/name
// string which is then put onto the work queue. This method should *not* be
// passed resources of any type other than GrafanaDashboard.
func (c *Controller) enqueueDashboard(obj interface{}) {
	var key string
	var err error
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		utilruntime.HandleError(err)
		return
	}
	c.workqueue.Add(key)
}

// enqueueDeletion takes a GrafanaDashboard resource and converts it into a deletion/uid
// string which is then put onto the work queue. This method should *not* be
// passed resources of any type other than GrafanaDashboard.
func (c *Controller) enqueueDeletion(obj interface{}) {
	dashboard := obj.(*v1.GrafanaDashboard)

	c.workqueue.Add("deletion:" + dashboard.ObjectMeta.Name)
}
