package v122to123

import (
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	"github.com/longhorn/longhorn-manager/engineapi"
	longhorn "github.com/longhorn/longhorn-manager/k8s/pkg/apis/longhorn/v1beta2"
	lhclientset "github.com/longhorn/longhorn-manager/k8s/pkg/client/clientset/versioned"
	"github.com/longhorn/longhorn-manager/types"
	upgradeutil "github.com/longhorn/longhorn-manager/upgrade/util"
)

const (
	upgradeLogPrefix = "upgrade from v1.2.2 to v1.2.3: "
)

func UpgradeResources(namespace string, lhClient *lhclientset.Clientset, resourceMaps map[string]interface{}) (err error) {
	if err := upgradeBackups(namespace, lhClient, resourceMaps); err != nil {
		return err
	}
	if err := upgradeEngines(namespace, lhClient, resourceMaps); err != nil {
		return err
	}
	return nil
}

func upgradeBackups(namespace string, lhClient *lhclientset.Clientset, resourceMaps map[string]interface{}) (err error) {
	defer func() {
		err = errors.Wrapf(err, upgradeLogPrefix+"upgrade backups failed")
	}()

	// Copy backupStatus from engine CRs to backup CRs
	backupMap, err := upgradeutil.ListAndUpdateBackupsInProvidedCache(namespace, lhClient, resourceMaps)
	if err != nil {
		return err
	}

	engineMap, err := upgradeutil.ListAndUpdateEnginesInProvidedCache(namespace, lhClient, resourceMaps)
	if err != nil {
		return err
	}
	volumeNameToEngines := make(map[string][]*longhorn.Engine)
	for _, e := range engineMap {
		volumeNameToEngines[e.Labels[types.LonghornLabelVolume]] = append(volumeNameToEngines[e.Labels[types.LonghornLabelVolume]], e)
	}

	volumeMap, err := upgradeutil.ListAndUpdateVolumesInProvidedCache(namespace, lhClient, resourceMaps)
	if err != nil {
		return err
	}

	progressMonitor := upgradeutil.NewProgressMonitor("upgradeBackups", 0, len(backupMap))
	// Loop all the backup CRs
	for _, backup := range backupMap {
		progressMonitor.Inc()
		// Get volume name from label
		volumeName, exist := backup.Labels[types.LonghornLabelBackupVolume]
		if !exist {
			continue
		}

		engines := volumeNameToEngines[volumeName]

		// No engine CR found
		var engine *longhorn.Engine
		switch len(engines) {
		case 0:
			continue
		case 1:
			engine = engines[0]
		default:
			v, ok := volumeMap[volumeName]
			if !ok {
				continue
			}

			for _, e := range engines {
				if e.Spec.NodeID == v.Status.CurrentNodeID &&
					e.Spec.DesireState == longhorn.InstanceStateRunning &&
					e.Status.CurrentState == longhorn.InstanceStateRunning {
					engine = e
					break
				}
			}
		}

		// No corresponding backupStatus inside engine CR
		backupStatus, exist := engine.Status.BackupStatus[backup.Name]
		if !exist {
			continue
		}

		backup.Status.Progress = backupStatus.Progress
		backup.Status.URL = backupStatus.BackupURL
		backup.Status.Error = backupStatus.Error
		backup.Status.SnapshotName = backupStatus.SnapshotName
		backup.Status.State = engineapi.ConvertEngineBackupState(backupStatus.State)
		backup.Status.ReplicaAddress = backupStatus.ReplicaAddress
	}
	return nil
}

func upgradeEngines(namespace string, lhClient *lhclientset.Clientset, resourceMaps map[string]interface{}) (err error) {
	defer func() {
		err = errors.Wrapf(err, upgradeLogPrefix+"upgrade engines failed")
	}()

	// Do the field update separately to avoid messing up.

	if err := checkAndRemoveEngineBackupStatus(namespace, lhClient, resourceMaps); err != nil {
		return err
	}

	if err := checkAndUpdateEngineActiveState(namespace, lhClient, resourceMaps); err != nil {
		return err
	}

	return nil
}

func checkAndRemoveEngineBackupStatus(namespace string, lhClient *lhclientset.Clientset, resourceMaps map[string]interface{}) error {
	engineMap, err := upgradeutil.ListAndUpdateEnginesInProvidedCache(namespace, lhClient, resourceMaps)
	if err != nil {
		return err
	}

	progressMonitor := upgradeutil.NewProgressMonitor("checkAndRemoveEngineBackupStatus", 0, len(engineMap))
	for _, engine := range engineMap {
		progressMonitor.Inc()
		engine.Status.BackupStatus = nil
	}

	return nil
}

func checkAndUpdateEngineActiveState(namespace string, lhClient *lhclientset.Clientset, resourceMaps map[string]interface{}) error {
	engineMap, err := upgradeutil.ListAndUpdateEnginesInProvidedCache(namespace, lhClient, resourceMaps)
	if err != nil {
		return err
	}

	volumeEngineMap := map[string][]*longhorn.Engine{}
	for _, e := range engineMap {
		if e.Spec.VolumeName == "" {
			// Cannot do anything in the upgrade path if there is really an orphan engine CR.
			continue
		}
		volumeEngineMap[e.Spec.VolumeName] = append(volumeEngineMap[e.Spec.VolumeName], e)
	}

	progressMonitor := upgradeutil.NewProgressMonitor("checkAndUpdateEngineActiveState", 0, len(volumeEngineMap))
	for volumeName, engineList := range volumeEngineMap {
		progressMonitor.Inc()
		skip := false
		for _, e := range engineList {
			if e.Spec.Active {
				skip = true
				break
			}
		}
		if skip {
			continue
		}

		var currentEngine *longhorn.Engine
		if len(engineList) == 1 {
			currentEngine = engineList[0]
		} else {
			v, err := upgradeutil.GetVolumeFromProvidedCache(namespace, lhClient, resourceMaps, volumeName)
			if err != nil {
				return err
			}
			for i := range engineList {
				if (v.Spec.NodeID != "" && v.Spec.NodeID == engineList[i].Spec.NodeID) ||
					(v.Status.CurrentNodeID != "" && v.Status.CurrentNodeID == engineList[i].Spec.NodeID) ||
					(v.Status.PendingNodeID != "" && v.Status.PendingNodeID == engineList[i].Spec.NodeID) {
					currentEngine = engineList[i]
					break
				}
			}
		}
		if currentEngine == nil {
			logrus.Errorf("Failed to get the current engine for volume %v during upgrade, will ignore it and continue", volumeName)
			continue
		}
		currentEngine.Spec.Active = true
	}

	return nil
}
