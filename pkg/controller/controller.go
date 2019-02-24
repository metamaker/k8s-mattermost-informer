package controller

import (
	"fmt"
	"strconv"
	"time"

	"github.com/mattermost/mattermost-server/model"

	"github.com/lnsp/mattermost-informer/pkg/client"
	"github.com/lnsp/mattermost-informer/pkg/utils"
	"k8s.io/klog"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

type Controller struct {
	indexer    cache.Indexer
	queue      workqueue.RateLimitingInterface
	informer   cache.Controller
	mattermost *utils.MattermostClient
	clientset  kubernetes.Interface

	timeouts map[string]time.Time
}

// NewController instantiates a new controller.
func NewController(clientset kubernetes.Interface, mattermost *utils.MattermostClient, queue workqueue.RateLimitingInterface, indexer cache.Indexer, informer cache.Controller) *Controller {
	return &Controller{
		clientset:  clientset,
		mattermost: mattermost,
		informer:   informer,
		indexer:    indexer,
		queue:      queue,
		timeouts:   make(map[string]time.Time),
	}
}

func (c *Controller) processNextItem() bool {
	// Wait until there is a new item in the working queue
	key, quit := c.queue.Get()
	if quit {
		return false
	}
	// Tell the queue that we are done with processing this key. This unblocks the key for other workers
	// This allows safe parallel processing because two pods with the same key are never processed in
	// parallel.
	defer c.queue.Done(key)

	// Invoke the method containing the business logic
	err := c.syncToStdout(key.(string))
	// Handle the error if something went wrong during the execution of the business logic
	c.handleErr(err, key)
	return true
}

const (
	annotationEnableMattermost       = "espe.tech/mattermost"
	annotationEnableMattermostInform = "inform"
)

func (c *Controller) hasValidAnnotation(pod *v1.Pod) bool {
	return pod.GetObjectMeta().GetAnnotations()[annotationEnableMattermost] == annotationEnableMattermostInform
}

const (
	annotationMattermostBackoff        = "espe.tech/mattermost-backoff"
	annotationMattermostBackoffDefault = time.Minute * 10
)

func (c *Controller) refreshBackoff(pod *v1.Pod, container *v1.ContainerStatus) bool {
	backoff := annotationMattermostBackoffDefault
	if backoffVal := pod.GetObjectMeta().GetAnnotations()[annotationMattermostBackoff]; backoffVal != "" {
		if seconds, err := strconv.Atoi(backoffVal); err != nil {
			backoff = time.Duration(seconds) * time.Second
		}
	}
	if time.Since(c.timeouts[pod.GetName()]) < backoff {
		return false
	}
	c.timeouts[pod.GetName()] = time.Now()
	return true
}

func (c *Controller) clearTimeout(pod *v1.Pod) {
	delete(c.timeouts, pod.GetName())
}

func (c *Controller) sendCrashNotification(pod *v1.Pod, container *v1.ContainerStatus) {
	logs, _ := c.clientset.
		CoreV1().Pods(pod.Namespace).
		GetLogs(pod.Name, &v1.PodLogOptions{Container: container.Name}).Do().Raw()
	message := fmt.Sprintf("Container %s of pod %s keeps crashing, maybe its time to intervene.", container.Name, pod.Name)
	attachment := &model.SlackAttachment{
		Color: "#AD2200",
		Text:  message,
		Title: "Crash loop detected!",
		Fields: []*model.SlackAttachmentField{
			{
				Title: "Logs",
				Value: "```\n" + string(logs) + "```",
			},
		},
	}
	// Check for termination message
	if container.LastTerminationState.Terminated != nil {
		attachment.Fields = append(attachment.Fields, &model.SlackAttachmentField{
			Title: "Reason",
			Value: container.LastTerminationState.Terminated.Reason,
		})
	}
	c.mattermost.SendAttachements(attachment)
}

func (c *Controller) handlePodUpdate(pod *v1.Pod) {
	for _, container := range pod.Status.ContainerStatuses {
		if !container.Ready && container.State.Waiting != nil && c.hasValidAnnotation(pod) {
			switch container.State.Waiting.Reason {
			case "CrashLoopBackOff":
				if !c.refreshBackoff(pod, &container) {
					continue
				}
				c.sendCrashNotification(pod, &container)
			}
		}
	}
}

// syncToStdout is the business logic of the controller. In this controller it simply prints
// information about the pod to stdout. In case an error happened, it has to simply return the error.
// The retry logic should not be part of the business logic.
func (c *Controller) syncToStdout(key string) error {
	obj, exists, err := c.indexer.GetByKey(key)
	if err != nil {
		klog.Errorf("Fetching object with key %s from store failed with %v", key, err)
		return err
	}
	if !exists {
		// Below we will warm up our cache with a Pod, so that we will see a delete for one pod
		klog.Infof("Pod %s does not exist anymore\n", key)
		// Clean up intervals
		c.clearTimeout(obj.(*v1.Pod))
	} else {
		klog.Infof("Received create/update/delete for Pod %s\n", key)
		// Note that you also have to check the uid if you have a local controlled resource, which
		// is dependent on the actual instance, to detect that a Pod was recreated with the same name
		c.handlePodUpdate(obj.(*v1.Pod))
	}
	return nil
}

// handleErr checks if an error happened and makes sure we will retry later.
func (c *Controller) handleErr(err error, key interface{}) {
	if err == nil {
		// Forget about the #AddRateLimited history of the key on every successful synchronization.
		// This ensures that future processing of updates for this key is not delayed because of
		// an outdated error history.
		c.queue.Forget(key)
		return
	}

	// This controller retries 5 times if something goes wrong. After that, it stops trying.
	if c.queue.NumRequeues(key) < 5 {
		klog.Infof("Error syncing pod %v: %v", key, err)

		// Re-enqueue the key rate limited. Based on the rate limiter on the
		// queue and the re-enqueue history, the key will be processed later again.
		c.queue.AddRateLimited(key)
		return
	}

	c.queue.Forget(key)
	// Report to an external entity that, even after several retries, we could not successfully process this key
	runtime.HandleError(err)
	klog.Infof("Dropping pod %q out of the queue: %v", key, err)
}

func (c *Controller) Run(threadiness int, stopCh chan struct{}) {
	defer runtime.HandleCrash()

	// Let the workers stop when we are done
	defer c.queue.ShutDown()
	klog.Info("Starting Pod controller")

	go c.informer.Run(stopCh)

	// Wait for all involved caches to be synced, before processing items from the queue is started
	if !cache.WaitForCacheSync(stopCh, c.informer.HasSynced) {
		runtime.HandleError(fmt.Errorf("Timed out waiting for caches to sync"))
		return
	}

	for i := 0; i < threadiness; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	<-stopCh
	klog.Info("Stopping Pod controller")
}

func (c *Controller) runWorker() {
	for c.processNextItem() {
	}
}

func Run() {
	mattermost, err := utils.NewMattermostClient()
	if err != nil {
		klog.Fatal(err)
	}

	clientset, err := client.InCluster()
	if err != nil {
		klog.Fatal(err)
	}

	namespace, err := utils.Namespace()
	if err != nil {
		klog.Fatal(err)
	}
	klog.Infof("Watching namespace %s", namespace)

	// create the pod watcher
	podListWatcher := cache.NewListWatchFromClient(clientset.CoreV1().RESTClient(), "pods", namespace, fields.Everything())

	// create the workqueue
	queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())

	// Bind the workqueue to a cache with the help of an informer. This way we make sure that
	// whenever the cache is updated, the pod key is added to the workqueue.
	// Note that when we finally process the item from the workqueue, we might see a newer version
	// of the Pod than the version which was responsible for triggering the update.
	indexer, informer := cache.NewIndexerInformer(podListWatcher, &v1.Pod{}, 0, cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(obj)
			if err == nil {
				queue.Add(key)
			}
		},
		UpdateFunc: func(old interface{}, new interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(new)
			if err == nil {
				queue.Add(key)
			}
		},
		DeleteFunc: func(obj interface{}) {
			// IndexerInformer uses a delta queue, therefore for deletes we have to use this
			// key function.
			key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			if err == nil {
				queue.Add(key)
			}
		},
	}, cache.Indexers{})

	controller := NewController(clientset, mattermost, queue, indexer, informer)

	stop := make(chan struct{})
	defer close(stop)
	go controller.Run(1, stop)

	// Wait forever
	select {}
}
