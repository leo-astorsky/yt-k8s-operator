package controllers

import (
	"context"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	apiProxy "github.com/ytsaurus/yt-k8s-operator/pkg/apiproxy"
	"github.com/ytsaurus/yt-k8s-operator/pkg/components"
	"github.com/ytsaurus/yt-k8s-operator/pkg/labeller"
	"github.com/ytsaurus/yt-k8s-operator/pkg/ytconfig"
)

type componentsStructured struct {
	discovery   components.Component2
	master      components.Component2
	httpProxies []components.Component2
	ytClient    components.YtsaurusClient2
	dataNodes   []components.Component2
	// (optional) ui (depends on master)
	// (optional) rpcproxies (depends on master)
	// (optional) tcpproxies (depends on master)
	// (optional) execnodes (depends on master)
	// (optional) tabletnodes (depends on master, yt client)
	// (optional) scheduler (depends on master, exec nodes, tablet nodes)
	// (optional) controller agents (depends on master)
	// (optional) querytrackers (depends on yt client and tablet nodes)
	// (optional) queueagents (depend on y cli, master, tablet nodes)
	// (optional) yqlagents (depend on master)
	// (optional) strawberry (depend on master, scheduler, data nodes)
}

type ComponentManager struct {
	ytsaurus              *apiProxy.Ytsaurus
	allComponents         []components.Component
	allStructured         componentsStructured
	queryTrackerComponent components.Component
	schedulerComponent    components.Component
	status                ComponentManagerStatus
}

type ComponentManagerStatus struct {
	needSync           bool
	needInit           bool
	needFullUpdate     bool
	needLocalUpdate    []components.Component
	allReadyOrUpdating bool
}

func NewComponentManager(ytsaurus *apiProxy.Ytsaurus) (*ComponentManager, error) {
	resource := ytsaurus.GetResource()

	cfgen := ytconfig.NewGenerator(resource, getClusterDomain(ytsaurus.APIProxy().Client()))

	d := components.NewDiscovery(cfgen, ytsaurus)
	m := components.NewMaster(cfgen, ytsaurus)
	var hps []components.Component
	var hps2 []components.Component2
	for _, hpSpec := range ytsaurus.GetResource().Spec.HTTPProxies {
		hp := components.NewHTTPProxy(cfgen, ytsaurus, m, hpSpec)
		hps = append(hps, hp)
		hps2 = append(hps2, hp)
	}
	yc := components.NewYtsaurusClient(cfgen, ytsaurus, hps[0])

	var dnds []components.Component
	var dnds2 []components.Component2
	if resource.Spec.DataNodes != nil && len(resource.Spec.DataNodes) > 0 {
		for _, dndSpec := range ytsaurus.GetResource().Spec.DataNodes {
			dns := components.NewDataNode(cfgen, ytsaurus, m, dndSpec)
			dnds = append(dnds, dns)
			dnds2 = append(dnds2, dns)
		}
	}

	var s components.Component

	allComponents := []components.Component{
		d, m, yc,
	}
	allComponents = append(allComponents, dnds...)
	allComponents = append(allComponents, hps...)

	if resource.Spec.UI != nil {
		ui := components.NewUI(cfgen, ytsaurus, m)
		allComponents = append(allComponents, ui)
	}

	if resource.Spec.RPCProxies != nil && len(resource.Spec.RPCProxies) > 0 {
		var rps []components.Component
		for _, rpSpec := range ytsaurus.GetResource().Spec.RPCProxies {
			rps = append(rps, components.NewRPCProxy(cfgen, ytsaurus, m, rpSpec))
		}
		allComponents = append(allComponents, rps...)
	}

	if resource.Spec.TCPProxies != nil && len(resource.Spec.TCPProxies) > 0 {
		var tps []components.Component
		for _, tpSpec := range ytsaurus.GetResource().Spec.TCPProxies {
			tps = append(tps, components.NewTCPProxy(cfgen, ytsaurus, m, tpSpec))
		}
		allComponents = append(allComponents, tps...)
	}

	var ends []components.Component
	if resource.Spec.ExecNodes != nil && len(resource.Spec.ExecNodes) > 0 {
		for _, endSpec := range ytsaurus.GetResource().Spec.ExecNodes {
			ends = append(ends, components.NewExecNode(cfgen, ytsaurus, m, endSpec))
		}
	}
	allComponents = append(allComponents, ends...)

	var tnds []components.Component
	if resource.Spec.TabletNodes != nil && len(resource.Spec.TabletNodes) > 0 {
		for idx, tndSpec := range ytsaurus.GetResource().Spec.TabletNodes {
			tnds = append(tnds, components.NewTabletNode(cfgen, ytsaurus, yc, tndSpec, idx == 0))
		}
	}
	allComponents = append(allComponents, tnds...)

	if resource.Spec.Schedulers != nil {
		s = components.NewScheduler(cfgen, ytsaurus, m, ends, tnds)
		allComponents = append(allComponents, s)
	}

	if resource.Spec.ControllerAgents != nil {
		ca := components.NewControllerAgent(cfgen, ytsaurus, m)
		allComponents = append(allComponents, ca)
	}

	var q components.Component
	if resource.Spec.QueryTrackers != nil && resource.Spec.Schedulers != nil && resource.Spec.TabletNodes != nil && len(resource.Spec.TabletNodes) > 0 {
		q = components.NewQueryTracker(cfgen, ytsaurus, yc, tnds)
		allComponents = append(allComponents, q)
	}

	if resource.Spec.QueueAgents != nil && resource.Spec.TabletNodes != nil && len(resource.Spec.TabletNodes) > 0 {
		qa := components.NewQueueAgent(cfgen, ytsaurus, yc, m, tnds)
		allComponents = append(allComponents, qa)
	}

	if resource.Spec.YQLAgents != nil {
		yqla := components.NewYQLAgent(cfgen, ytsaurus, m)
		allComponents = append(allComponents, yqla)
	}

	if (resource.Spec.DeprecatedChytController != nil || resource.Spec.StrawberryController != nil) && resource.Spec.Schedulers != nil {
		strawberry := components.NewStrawberryController(cfgen, ytsaurus, m, s, dnds)
		allComponents = append(allComponents, strawberry)
	}
	return &ComponentManager{
		ytsaurus:      ytsaurus,
		allComponents: allComponents,
		allStructured: componentsStructured{
			discovery:   d,
			master:      m,
			httpProxies: hps2,
			ytClient:    yc,
			dataNodes:   dnds2,
		},
		queryTrackerComponent: q,
		schedulerComponent:    s,
		//status:                status,
	}, nil
}

func (cm *ComponentManager) FetchAll(ctx context.Context) error {
	logger := log.FromContext(ctx)
	// Fetch component status.
	var readyComponents []string
	var notReadyComponents []string

	status := ComponentManagerStatus{
		needInit:           false,
		needSync:           false,
		needFullUpdate:     false,
		needLocalUpdate:    nil,
		allReadyOrUpdating: true,
	}
	for _, c := range cm.allComponents {
		err := c.Fetch(ctx)
		if err != nil {
			logger.Error(err, "failed to fetch status for controller", "component", c.GetName())
			return nil
		}

		componentStatus := c.Status(ctx)
		c.SetReadyCondition(componentStatus)
		syncStatus := componentStatus.SyncStatus

		if syncStatus == components.SyncStatusNeedFullUpdate {
			status.needFullUpdate = true
		}

		if syncStatus == components.SyncStatusNeedLocalUpdate {
			if status.needLocalUpdate == nil {
				status.needLocalUpdate = make([]components.Component, 0)
			}
			status.needLocalUpdate = append(status.needLocalUpdate, c)
		}

		if !components.IsRunningStatus(syncStatus) {
			status.needInit = true
		}

		if syncStatus != components.SyncStatusReady && syncStatus != components.SyncStatusUpdating {
			status.allReadyOrUpdating = false
		}

		if syncStatus != components.SyncStatusReady {
			logger.Info("component is not ready", "component", c.GetName(), "syncStatus", syncStatus)
			notReadyComponents = append(notReadyComponents, c.GetName())
			status.needSync = true
		} else {
			readyComponents = append(readyComponents, c.GetName())
		}
	}

	resource := cm.ytsaurus.GetResource()
	logger.Info("Ytsaurus sync status",
		"notReadyComponents", notReadyComponents,
		"readyComponents", readyComponents,
		"updateState", resource.Status.UpdateStatus.State,
		"clusterState", resource.Status.State)

	cm.status = status
	return nil
}

func (cm *ComponentManager) Sync(ctx context.Context) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	hasPending := false
	for _, c := range cm.allComponents {
		status := c.Status(ctx)

		if status.SyncStatus == components.SyncStatusPending ||
			status.SyncStatus == components.SyncStatusUpdating {
			hasPending = true
			logger.Info("component sync", "component", c.GetName())
			if err := c.Sync(ctx); err != nil {
				logger.Error(err, "component sync failed", "component", c.GetName())
				return ctrl.Result{Requeue: true}, err
			}
		}
	}

	if err := cm.ytsaurus.APIProxy().UpdateStatus(ctx); err != nil {
		logger.Error(err, "update Ytsaurus status failed")
		return ctrl.Result{Requeue: true}, err
	}

	if !hasPending {
		// All components are blocked.
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	return ctrl.Result{RequeueAfter: time.Second}, nil
}

func (cm *ComponentManager) needSync() bool {
	return cm.status.needSync
}

func (cm *ComponentManager) needInit() bool {
	return cm.status.needInit
}

func (cm *ComponentManager) needFullUpdate() bool {
	return cm.status.needFullUpdate
}

func (cm *ComponentManager) needLocalUpdate() []components.Component {
	return cm.status.needLocalUpdate
}

func (cm *ComponentManager) allReadyOrUpdating() bool {
	return cm.status.allReadyOrUpdating
}

func (cm *ComponentManager) needQueryTrackerUpdate() bool {
	return cm.queryTrackerComponent != nil && components.IsUpdatingComponent(cm.ytsaurus, cm.queryTrackerComponent)
}

func (cm *ComponentManager) needSchedulerUpdate() bool {
	return cm.schedulerComponent != nil && components.IsUpdatingComponent(cm.ytsaurus, cm.schedulerComponent)
}

func (cm *ComponentManager) arePodsRemoved() bool {
	for _, cmp := range cm.allComponents {
		if components.IsUpdatingComponent(cm.ytsaurus, cmp) && !cm.areComponentPodsRemoved(cmp) {
			return false
		}
	}

	return true
}

func (cm *ComponentManager) areComponentPodsRemoved(component components.Component) bool {
	return cm.ytsaurus.IsUpdateStatusConditionTrue(labeller.GetPodsRemovedCondition(component.GetName()))
}
