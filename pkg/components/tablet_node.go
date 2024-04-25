package components

import (
	"context"
	"fmt"

	"go.ytsaurus.tech/library/go/ptr"
	"go.ytsaurus.tech/yt/go/ypath"
	"go.ytsaurus.tech/yt/go/yt"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	ytv1 "github.com/ytsaurus/yt-k8s-operator/api/v1"
	"github.com/ytsaurus/yt-k8s-operator/pkg/apiproxy"
	"github.com/ytsaurus/yt-k8s-operator/pkg/consts"
	"github.com/ytsaurus/yt-k8s-operator/pkg/labeller"
	"github.com/ytsaurus/yt-k8s-operator/pkg/resources"
	"github.com/ytsaurus/yt-k8s-operator/pkg/ytconfig"
)

const SysBundle string = "sys"
const DefaultBundle string = "default"

type ytsaurusClientForTabletNodes interface {
	GetYtClient() yt.Client

	GetTabletCells(context.Context) ([]ytv1.TabletCellBundleInfo, error)
	RemoveTabletCells(context.Context) error
	RecoverTableCells(context.Context, []ytv1.TabletCellBundleInfo) error
	AreTabletCellsRemoved(context.Context) (bool, error)

	// TODO (l0kix2): remove later.
	Status(ctx context.Context) (ComponentStatus, error)
	GetName() string
}

type TabletNode struct {
	localServerComponent
	cfgen *ytconfig.NodeGenerator

	ytsaurusClient ytsaurusClientForTabletNodes

	initBundlesCondition string
	spec                 ytv1.TabletNodesSpec
	doInitialization     bool
}

func NewTabletNode(
	cfgen *ytconfig.NodeGenerator,
	ytsaurus *apiproxy.Ytsaurus,
	ytsaurusClient ytsaurusClientForTabletNodes,
	spec ytv1.TabletNodesSpec,
	doInitiailization bool,
) *TabletNode {
	resource := ytsaurus.GetResource()
	l := labeller.Labeller{
		ObjectMeta:     &resource.ObjectMeta,
		APIProxy:       ytsaurus.APIProxy(),
		ComponentLabel: cfgen.FormatComponentStringWithDefault(consts.YTComponentLabelTabletNode, spec.Name),
		ComponentName:  cfgen.FormatComponentStringWithDefault(string(consts.TabletNodeType), spec.Name),
	}

	if spec.InstanceSpec.MonitoringPort == nil {
		spec.InstanceSpec.MonitoringPort = ptr.Int32(consts.TabletNodeMonitoringPort)
	}

	srv := newServer(
		&l,
		ytsaurus,
		&spec.InstanceSpec,
		"/usr/bin/ytserver-node",
		"ytserver-tablet-node.yson",
		cfgen.GetTabletNodesStatefulSetName(spec.Name),
		cfgen.GetTabletNodesServiceName(spec.Name),
		func() ([]byte, error) {
			return cfgen.GetTabletNodeConfig(spec)
		},
	)

	return &TabletNode{
		localServerComponent: newLocalServerComponent(&l, ytsaurus, srv),
		cfgen:                cfgen,
		initBundlesCondition: "bundlesTabletNodeInitCompleted",
		ytsaurusClient:       ytsaurusClient,
		spec:                 spec,
		doInitialization:     doInitiailization,
	}
}

func (tn *TabletNode) IsUpdatable() bool {
	return true
}

func (tn *TabletNode) GetType() consts.ComponentType { return consts.TabletNodeType }

func (tn *TabletNode) doSync(ctx context.Context, dry bool) (ComponentStatus, error) {
	var err error

	if ytv1.IsReadyToUpdateClusterState(tn.ytsaurus.GetClusterState()) && tn.server.needUpdate() {
		return SimpleStatus(SyncStatusNeedLocalUpdate), err
	}

	if tn.ytsaurus.GetClusterState() == ytv1.ClusterStateUpdating {
		if status, err := handleUpdatingClusterState(ctx, tn.ytsaurus, tn, &tn.localComponent, tn.server, dry); status != nil {
			return *status, err
		}
	}

	if tn.NeedSync() {
		if !dry {
			err = tn.server.Sync(ctx)
		}

		return WaitingStatus(SyncStatusPending, "components"), err
	}

	if !tn.server.arePodsReady(ctx) {
		return WaitingStatus(SyncStatusBlocked, "pods"), err
	}

	if !tn.doInitialization || tn.ytsaurus.IsStatusConditionTrue(tn.initBundlesCondition) {
		return SimpleStatus(SyncStatusReady), err
	}

	ytClientStatus, err := tn.ytsaurusClient.Status(ctx)
	if err != nil {
		return ytClientStatus, err
	}
	if ytClientStatus.SyncStatus != SyncStatusReady {
		return WaitingStatus(SyncStatusBlocked, tn.ytsaurusClient.GetName()), err
	}

	if !dry && tn.doInitialization {
		tabletBundleStatus, err := tn.initBundles(ctx)
		if err != nil {
			return tabletBundleStatus, err
		}
	}

	return WaitingStatus(SyncStatusPending, fmt.Sprintf("setting %s condition", tn.initBundlesCondition)), err
}

func (tn *TabletNode) initializeBundles(ctx context.Context) error {
	ytClient := tn.ytsaurusClient.GetYtClient()

	if exists, err := ytClient.NodeExists(ctx, ypath.Path(fmt.Sprintf("//sys/tablet_cell_bundles/%s", SysBundle)), nil); err == nil {
		if !exists {
			options := map[string]string{
				"changelog_account": "sys",
				"snapshot_account":  "sys",
			}

			bootstrap := tn.getBundleBootstrap(SysBundle)
			if bootstrap != nil {
				if bootstrap.ChangelogPrimaryMedium != nil {
					options["changelog_primary_medium"] = *bootstrap.ChangelogPrimaryMedium
				}
				if bootstrap.SnapshotPrimaryMedium != nil {
					options["snapshot_primary_medium"] = *bootstrap.SnapshotPrimaryMedium
				}
			}

			_, err = ytClient.CreateObject(ctx, yt.NodeTabletCellBundle, &yt.CreateObjectOptions{
				Attributes: map[string]interface{}{
					"name":    SysBundle,
					"options": options,
				},
			})

			if err != nil {
				return fmt.Errorf("creating tablet_cell_bundle failed: %w", err)
			}
		}
	} else {
		return err
	}

	defaultBundleBootstrap := tn.getBundleBootstrap(DefaultBundle)
	if defaultBundleBootstrap != nil {
		path := ypath.Path(fmt.Sprintf("//sys/tablet_cell_bundles/%s", DefaultBundle))
		if defaultBundleBootstrap.ChangelogPrimaryMedium != nil {
			err := ytClient.SetNode(ctx, path.Attr("options/changelog_primary_medium"), *defaultBundleBootstrap.ChangelogPrimaryMedium, nil)
			if err != nil {
				return fmt.Errorf("setting changelog_primary_medium for `default` bundle failed: %w", err)
			}
		}

		if defaultBundleBootstrap.SnapshotPrimaryMedium != nil {
			err := ytClient.SetNode(ctx, path.Attr("options/snapshot_primary_medium"), *defaultBundleBootstrap.SnapshotPrimaryMedium, nil)
			if err != nil {
				return fmt.Errorf("setting snapshot_primary_medium for `default` bundle failed: %w", err)
			}
		}
	}

	for _, bundle := range []string{DefaultBundle, SysBundle} {
		tabletCellCount := 1
		bootstrap := tn.getBundleBootstrap(bundle)
		if bootstrap != nil {
			tabletCellCount = bootstrap.TabletCellCount
		}
		err := CreateTabletCells(ctx, ytClient, bundle, tabletCellCount)
		if err != nil {
			return err
		}
	}

	return nil

}

func (tn *TabletNode) getBundleBootstrap(bundle string) *ytv1.BundleBootstrapSpec {
	resource := tn.ytsaurus.GetResource()
	if resource.Spec.Bootstrap == nil || resource.Spec.Bootstrap.TabletCellBundles == nil {
		return nil
	}

	if bundle == SysBundle {
		return resource.Spec.Bootstrap.TabletCellBundles.Sys
	}

	if bundle == DefaultBundle {
		return resource.Spec.Bootstrap.TabletCellBundles.Default
	}

	return nil
}

func (tn *TabletNode) getBundleOptions(bundle string) map[string]any {
	options := map[string]any{}

	if bundle == SysBundle {
		options["changelog_account"] = "sys"
		options["snapshot_account"] = "sys"
	}

	bootstrap := tn.getBundleBootstrap(bundle)
	if bootstrap != nil {
		if bootstrap.ChangelogPrimaryMedium != nil {
			options["changelog_primary_medium"] = *bootstrap.ChangelogPrimaryMedium
		}
		if bootstrap.SnapshotPrimaryMedium != nil {
			options["snapshot_primary_medium"] = *bootstrap.SnapshotPrimaryMedium
		}
	}

	if tn.cfgen.GetMaxReplicationFactor() < 3 {
		options["changelog_replication_factor"] = 1
		options["changelog_read_quorum"] = 1
		options["changelog_write_quorum"] = 1
		options["snapshot_replication_factor"] = 1
	}

	return options
}

func (tn *TabletNode) initBundles(ctx context.Context) (ComponentStatus, error) {
	ytClient := tn.ytsaurusClient.GetYtClient()
	logger := log.FromContext(ctx)

	sysBundleExists, err := ytClient.NodeExists(ctx, ypath.Path("//sys/tablet_cell_bundles").Child(SysBundle), nil)
	if err != nil {
		return WaitingStatus(SyncStatusPending, "tablet_cell_bundle creation"), err
	}
	if !sysBundleExists {
		options := tn.getBundleOptions(SysBundle)
		_, err = ytClient.CreateObject(ctx, yt.NodeTabletCellBundle, &yt.CreateObjectOptions{
			Attributes: map[string]interface{}{
				"name":    SysBundle,
				"options": options,
			},
		})

		if err != nil {
			logger.Error(err, "Creating tablet_cell_bundle failed")
			return WaitingStatus(SyncStatusPending, "tablet_cell_bundle creation"), err
		}
	}

	{
		options := tn.getBundleOptions(DefaultBundle)
		if len(options) != 0 {
			path := ypath.Path("//sys/tablet_cell_bundles").Child(DefaultBundle)
			bundleOptions := map[string]any{}
			err = ytClient.GetNode(ctx, path.Attr("options"), &bundleOptions, nil)
			if err != nil {
				logger.Error(err, "Getting options for `default` bundle failed")
				return WaitingStatus(SyncStatusPending, "getting default bundle options"), err
			}
			for option, value := range options {
				bundleOptions[option] = value
			}
			err = ytClient.SetNode(ctx, path.Attr("options"), bundleOptions, nil)
			if err != nil {
				logger.Error(err, "Setting options for `default` bundle failed", "options", options)
				return WaitingStatus(SyncStatusPending, "setting default bundle options"), err
			}
		}
	}

	for _, bundle := range []string{DefaultBundle, SysBundle} {
		tabletCellCount := 1
		bootstrap := tn.getBundleBootstrap(bundle)
		if bootstrap != nil {
			tabletCellCount = bootstrap.TabletCellCount
		}
		err = CreateTabletCells(ctx, ytClient, bundle, tabletCellCount)
		if err != nil {
			return WaitingStatus(SyncStatusPending, "tablet cells creation"), err
		}
	}

	tn.ytsaurus.SetStatusCondition(metav1.Condition{
		Type:    tn.initBundlesCondition,
		Status:  metav1.ConditionTrue,
		Reason:  "InitBundlesCompleted",
		Message: "Init bundles successfully completed",
	})

	return SimpleStatus(SyncStatusReady), nil
}

func (tn *TabletNode) Status(ctx context.Context) (ComponentStatus, error) {
	return flowToStatus(ctx, tn, tn.getFlow(), tn.condManager)
}

func (tn *TabletNode) Sync(ctx context.Context) error {
	return flowToSync(ctx, tn.getFlow(), tn.condManager)
}
func (tn *TabletNode) Fetch(ctx context.Context) error {
	return resources.Fetch(ctx, tn.server)
}
