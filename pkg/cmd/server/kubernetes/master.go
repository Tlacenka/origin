package kubernetes

import (
	"fmt"
	"io/ioutil"
	"net"
	"os"

	"github.com/emicklei/go-restful"
	"github.com/golang/glog"

	kapi "k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/client"
	"k8s.io/kubernetes/pkg/client/record"
	"k8s.io/kubernetes/pkg/cloudprovider/nodecontroller"
	"k8s.io/kubernetes/pkg/controller/replication"
	"k8s.io/kubernetes/pkg/master"
	"k8s.io/kubernetes/pkg/namespace"
	"k8s.io/kubernetes/pkg/resourcequota"
	"k8s.io/kubernetes/pkg/service"
	"k8s.io/kubernetes/pkg/util"
	"k8s.io/kubernetes/pkg/volume"
	"k8s.io/kubernetes/pkg/volume/host_path"
	"k8s.io/kubernetes/pkg/volume/nfs"
	"k8s.io/kubernetes/pkg/volumeclaimbinder"
	"k8s.io/kubernetes/plugin/pkg/scheduler"
	_ "k8s.io/kubernetes/plugin/pkg/scheduler/algorithmprovider"
	schedulerapi "k8s.io/kubernetes/plugin/pkg/scheduler/api"
	latestschedulerapi "k8s.io/kubernetes/plugin/pkg/scheduler/api/latest"
	"k8s.io/kubernetes/plugin/pkg/scheduler/factory"
)

const (
	KubeAPIPrefix        = "/api"
	KubeAPIPrefixV1Beta3 = "/api/v1beta3"
	KubeAPIPrefixV1      = "/api/v1"
)

// InstallAPI starts a Kubernetes master and registers the supported REST APIs
// into the provided mux, then returns an array of strings indicating what
// endpoints were started (these are format strings that will expect to be sent
// a single string value).
func (c *MasterConfig) InstallAPI(container *restful.Container) []string {
	c.Master.RestfulContainer = container
	_ = master.New(c.Master)

	messages := []string{}
	if c.Master.EnableV1Beta3 {
		messages = append(messages, fmt.Sprintf("Started Kubernetes API at %%s%s (deprecated)", KubeAPIPrefixV1Beta3))
	}
	if !c.Master.DisableV1 {
		messages = append(messages, fmt.Sprintf("Started Kubernetes API at %%s%s", KubeAPIPrefixV1))
	}

	return messages
}

// RunNamespaceController starts the Kubernetes Namespace Manager
func (c *MasterConfig) RunNamespaceController() {
	namespaceController := namespace.NewNamespaceManager(c.KubeClient, c.ControllerManager.NamespaceSyncPeriod)
	namespaceController.Run()
	glog.Infof("Started Kubernetes Namespace Manager")
}

// RunPersistentVolumeClaimBinder starts the Kubernetes Persistent Volume Claim Binder
func (c *MasterConfig) RunPersistentVolumeClaimBinder() {
	binder := volumeclaimbinder.NewPersistentVolumeClaimBinder(c.KubeClient, c.ControllerManager.PVClaimBinderSyncPeriod)
	binder.Run()
	glog.Infof("Started Kubernetes Persistent Volume Claim Binder")
}

func (c *MasterConfig) RunPersistentVolumeClaimRecycler(recyclerImageName string) {

	hostPathRecycler := &volume.RecyclableVolumeConfig{
		ImageName: recyclerImageName,
		Command:   []string{"/usr/share/openshift/scripts/volumes/recycler.sh"},
		Args:      []string{"/scrub"},
		Timeout:   int64(60),
	}

	nfsRecycler := &volume.RecyclableVolumeConfig{
		ImageName: recyclerImageName,
		Command:   []string{"/usr/share/openshift/scripts/volumes/recycler.sh"},
		Args:      []string{"/scrub"},
		Timeout:   int64(300),
	}

	allPlugins := []volume.VolumePlugin{}
	allPlugins = append(allPlugins, host_path.ProbeVolumePlugins(hostPathRecycler)...)
	allPlugins = append(allPlugins, nfs.ProbeVolumePlugins(nfsRecycler)...)

	recycler, err := volumeclaimbinder.NewPersistentVolumeRecycler(c.KubeClient, c.ControllerManager.PVClaimBinderSyncPeriod, allPlugins)
	if err != nil {
		glog.Fatalf("Could not start PersistentVolumeRecycler: %+v", err)
	}
	recycler.Run()
	glog.Infof("Started Kubernetes PersistentVolumeRecycler")
}

// RunReplicationController starts the Kubernetes replication controller sync loop
func (c *MasterConfig) RunReplicationController(client *client.Client) {
	controllerManager := replication.NewReplicationManager(client, replication.BurstReplicas)
	go controllerManager.Run(c.ControllerManager.ConcurrentRCSyncs, util.NeverStop)
	glog.Infof("Started Kubernetes Replication Manager")
}

// RunEndpointController starts the Kubernetes replication controller sync loop
func (c *MasterConfig) RunEndpointController() {
	endpoints := service.NewEndpointController(c.KubeClient)
	go endpoints.Run(c.ControllerManager.ConcurrentEndpointSyncs, util.NeverStop)

	glog.Infof("Started Kubernetes Endpoint Controller")
}

// RunScheduler starts the Kubernetes scheduler
func (c *MasterConfig) RunScheduler() {
	config, err := c.createSchedulerConfig()
	if err != nil {
		glog.Fatalf("Unable to start scheduler: %v", err)
	}
	eventcast := record.NewBroadcaster()
	config.Recorder = eventcast.NewRecorder(kapi.EventSource{Component: "scheduler"})
	eventcast.StartRecordingToSink(c.KubeClient.Events(""))

	s := scheduler.New(config)
	s.Run()
	glog.Infof("Started Kubernetes Scheduler")
}

// RunResourceQuotaManager starts the resource quota manager
func (c *MasterConfig) RunResourceQuotaManager() {
	resourceQuotaManager := resourcequota.NewResourceQuotaManager(c.KubeClient)
	resourceQuotaManager.Run(c.ControllerManager.ResourceQuotaSyncPeriod)
}

// RunNodeController starts the node controller
func (c *MasterConfig) RunNodeController() {
	s := c.ControllerManager
	controller := nodecontroller.NewNodeController(
		c.CloudProvider,
		c.KubeClient,
		s.RegisterRetryCount,
		s.PodEvictionTimeout,

		nodecontroller.NewPodEvictor(util.NewTokenBucketRateLimiter(s.DeletingPodsQps, s.DeletingPodsBurst)),

		s.NodeMonitorGracePeriod,
		s.NodeStartupGracePeriod,
		s.NodeMonitorPeriod,

		(*net.IPNet)(&s.ClusterCIDR),
		s.AllocateNodeCIDRs,
	)
	controller.Run(s.NodeSyncPeriod)

	glog.Infof("Started Kubernetes Node Controller")
}

func (c *MasterConfig) createSchedulerConfig() (*scheduler.Config, error) {
	var policy schedulerapi.Policy
	var configData []byte

	configFactory := factory.NewConfigFactory(c.KubeClient)
	if _, err := os.Stat(c.Options.SchedulerConfigFile); err == nil {
		configData, err = ioutil.ReadFile(c.Options.SchedulerConfigFile)
		if err != nil {
			return nil, fmt.Errorf("unable to read scheduler config: %v", err)
		}
		err = latestschedulerapi.Codec.DecodeInto(configData, &policy)
		if err != nil {
			return nil, fmt.Errorf("invalid scheduler configuration: %v", err)
		}

		return configFactory.CreateFromConfig(policy)
	}

	// if the config file isn't provided, use the default provider
	return configFactory.CreateFromProvider(factory.DefaultProvider)
}
