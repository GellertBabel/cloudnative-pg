/*
Copyright © contributors to CloudNativePG, established as
CloudNativePG a Series of LF Projects, LLC.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

SPDX-License-Identifier: Apache-2.0
*/

package controller

import (
	"context"
	"time"

	"github.com/cloudnative-pg/machinery/pkg/log"
	"github.com/cloudnative-pg/machinery/pkg/types"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/postgres"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/postgres/replication"
)

// synchronousReplicationProbe is the part of the PostgreSQL instance used to
// record the synchronous replication barrier
type synchronousReplicationProbe interface {
	RequestSynchronousReplicationCheckpoint(ctx context.Context) (types.LSN, error)
	GetLatestCheckpointRedoLSN(ctx context.Context) (types.LSN, error)
	IsCheckpointerIdle(ctx context.Context) (bool, error)
	GetCurrentWALFlushLSN(ctx context.Context) (types.LSN, error)
}

// synchronousReplicationCheckpointWait is how long a reconciliation waits for
// a requested checkpoint before checking it again in a later one
var synchronousReplicationCheckpointWait = 10 * time.Second

// synchronousReplicationCheckpointRenewal is how long to wait for a requested
// checkpoint before requesting it again, as a checkpoint that fails drops the
// request
const synchronousReplicationCheckpointRenewal = 5 * time.Minute

// resetFailoverQuorumObject resets the content of the sync quorum object
// to prevent unsafe failovers when we are changing the configuration
func (r *InstanceReconciler) resetFailoverQuorumObject(ctx context.Context, cluster *apiv1.Cluster) error {
	if !r.shouldManageFailoverQuorumObject(cluster) {
		return nil
	}

	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var livingQuorumStatus apiv1.FailoverQuorum

		err := r.client.Get(ctx, client.ObjectKeyFromObject(cluster), &livingQuorumStatus)
		if err != nil {
			return err
		}

		livingQuorumStatus.Status = apiv1.FailoverQuorumStatus{}
		return r.client.Status().Update(ctx, &livingQuorumStatus)
	})
}

// updateFailoverQuorumObject updates the sync quorum object reading the
// current synchronous replica metadata from the PG instance. It returns true
// when the synchronous replication barrier could not be recorded and the
// update should be retried.
func (r *InstanceReconciler) updateFailoverQuorumObject(
	ctx context.Context,
	cluster *apiv1.Cluster,
) (requeue bool, err error) {
	if !r.shouldManageFailoverQuorumObject(cluster) {
		r.failoverQuorumBarrier.forget()
		return false, nil
	}

	metadata, err := r.Instance().GetSynchronousReplicationMetadata(ctx)
	if err != nil {
		return false, err
	}

	newStatus, requeue := r.desiredFailoverQuorumStatus(ctx, metadata, r.Instance(), time.Now())

	return requeue, retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var livingQuorumStatus apiv1.FailoverQuorum

		err := r.client.Get(ctx, client.ObjectKeyFromObject(cluster), &livingQuorumStatus)
		if err != nil {
			return err
		}

		if equality.Semantic.DeepEqual(livingQuorumStatus.Status, newStatus) {
			return nil
		}

		updatedQuorumStatus := livingQuorumStatus.DeepCopy()
		updatedQuorumStatus.Status = newStatus
		return r.client.Status().Update(ctx, updatedQuorumStatus)
	})
}

// desiredFailoverQuorumStatus computes the failover quorum status, recording
// the synchronous replication barrier when needed. Until the barrier is
// recorded, it returns an empty status, which denies failovers, and true to
// request a retry.
func (r *InstanceReconciler) desiredFailoverQuorumStatus(
	ctx context.Context,
	metadata *postgres.SynchronousStandbyNamesConfig,
	probe synchronousReplicationProbe,
	now time.Time,
) (apiv1.FailoverQuorumStatus, bool) {
	contextLogger := log.FromContext(ctx)

	status := buildFailoverQuorumStatus(metadata, r.instance.GetPodName())
	if len(status.StandbyNames) == 0 {
		r.failoverQuorumBarrier.forget()
		return status, false
	}

	barrier, err := r.failoverQuorumBarrier.get(ctx, status, probe, now)
	if err != nil {
		contextLogger.Warning(
			"Cannot record the synchronous replication barrier, failover is denied until it is recorded",
			"err", err)
		return apiv1.FailoverQuorumStatus{}, true
	}
	if barrier == "" {
		contextLogger.Debug("Waiting for synchronous replication to be enforced, failover is denied until then")
		return apiv1.FailoverQuorumStatus{}, true
	}

	status.BarrierLSN = string(barrier)
	return status, false
}

// buildFailoverQuorumStatus computes the failover quorum status, without the
// barrier, from the synchronous replication metadata of the primary
func buildFailoverQuorumStatus(
	metadata *postgres.SynchronousStandbyNamesConfig,
	primary string,
) apiv1.FailoverQuorumStatus {
	status := apiv1.FailoverQuorumStatus{}
	if metadata == nil {
		return status
	}

	status.Method = metadata.Method
	status.Primary = primary
	status.StandbyNumber = metadata.NumSync

	// We ensure the primary is not included in the standby names
	status.StandbyNames = make([]string, 0, len(metadata.StandbyNames))
	for _, name := range metadata.StandbyNames {
		if name == primary {
			continue
		}
		status.StandbyNames = append(status.StandbyNames, name)
	}

	return status
}

// failoverQuorumBarrier tracks whether commits wait for the synchronous
// standbys and the barrier LSN recorded for the synchronous replication
// configuration
type failoverQuorumBarrier struct {
	// enforced is set once commits are known to wait for the synchronous
	// standbys. PostgreSQL keeps enforcing it while synchronous_standby_names
	// is not empty, even when its value changes.
	enforced bool

	// requestLSN and requestedAt describe the checkpoint requested to confirm
	// that commits wait for the synchronous standbys, and passedRedoLSN is
	// the redo position of the first completed checkpoint after requestLSN
	requestLSN    types.LSN
	requestedAt   time.Time
	passedRedoLSN types.LSN

	// lsn is the barrier recorded for the status configuration
	status apiv1.FailoverQuorumStatus
	lsn    types.LSN

	// filesStatus is the configuration of the latest configuration files
	filesStatus apiv1.FailoverQuorumStatus
}

// get returns the barrier for the passed configuration, or an empty LSN while
// it is not known yet whether commits wait for the synchronous standbys
func (b *failoverQuorumBarrier) get(
	ctx context.Context,
	status apiv1.FailoverQuorumStatus,
	probe synchronousReplicationProbe,
	now time.Time,
) (types.LSN, error) {
	if b.lsn != "" && equality.Semantic.DeepEqual(b.status, status) {
		return b.lsn, nil
	}
	b.dropBarrier()

	if !b.enforced {
		enforced, err := b.confirmEnforced(ctx, probe, now)
		if err != nil || !enforced {
			return "", err
		}
	}

	lsn, err := probe.GetCurrentWALFlushLSN(ctx)
	if err != nil {
		return "", err
	}

	b.status = status
	b.lsn = lsn
	return lsn, nil
}

// confirmEnforced checks whether commits wait for the synchronous standbys,
// requesting the needed checkpoint when there is no pending request
func (b *failoverQuorumBarrier) confirmEnforced(
	ctx context.Context,
	probe synchronousReplicationProbe,
	now time.Time,
) (bool, error) {
	contextLogger := log.FromContext(ctx)

	if b.requestLSN == "" || now.Sub(b.requestedAt) >= synchronousReplicationCheckpointRenewal {
		requestCtx, cancel := context.WithTimeout(ctx, synchronousReplicationCheckpointWait)
		requestLSN, err := probe.RequestSynchronousReplicationCheckpoint(requestCtx)
		timedOut := requestCtx.Err() != nil
		cancel()

		// The request persists when we stop waiting for the checkpoint
		if err != nil && (!timedOut || requestLSN == "") {
			return false, err
		}

		// A renewed request keeps the first insert position, read after
		// the configuration was loaded, so that a running checkpoint that
		// started after it still counts
		if b.requestLSN == "" {
			b.requestLSN = requestLSN
		}
		b.requestedAt = now
		contextLogger.Info(
			"Requested a checkpoint to confirm that commits wait for the synchronous standbys",
			"requestLSN", b.requestLSN)
	}

	done, err := b.isCheckpointDone(ctx, probe)
	if err != nil || !done {
		return false, err
	}

	contextLogger.Info("Confirmed that commits wait for the synchronous standbys", "requestLSN", b.requestLSN)
	b.enforced = true
	b.requestLSN = ""
	b.requestedAt = time.Time{}
	b.passedRedoLSN = ""
	return true, nil
}

// isCheckpointDone checks whether the checkpointer applied the configuration
// loaded before requestLSN. That is the case once a checkpoint that started
// after requestLSN completed and the checkpointer was idle afterwards, or
// started another checkpoint, as it applies pending reloads between
// checkpoints.
func (b *failoverQuorumBarrier) isCheckpointDone(
	ctx context.Context,
	probe synchronousReplicationProbe,
) (bool, error) {
	redoLSN, err := probe.GetLatestCheckpointRedoLSN(ctx)
	if err != nil {
		return false, err
	}

	switch {
	case b.passedRedoLSN == "":
		if redoLSN.Less(b.requestLSN) {
			return false, nil
		}
		b.passedRedoLSN = redoLSN
	case b.passedRedoLSN.Less(redoLSN):
		return true, nil
	}

	// Read after the checkpoint was seen as completed
	return probe.IsCheckpointerIdle(ctx)
}

// dropBarrier discards the recorded barrier, keeping what is known about
// commits waiting for the synchronous standbys
func (b *failoverQuorumBarrier) dropBarrier() {
	b.status = apiv1.FailoverQuorumStatus{}
	b.lsn = ""
}

// forget discards everything, to be used when commits may stop waiting for
// the synchronous standbys
func (b *failoverQuorumBarrier) forget() {
	*b = failoverQuorumBarrier{}
}

// invalidateFailoverQuorumBarrier discards the barrier when the configuration
// files are about to change, as PostgreSQL may load them before the next
// update of the FailoverQuorum object. It compares with the previous files
// rather than with the recorded barrier, so that a configuration that failed
// to load doesn't move the barrier at every reconciliation.
func (r *InstanceReconciler) invalidateFailoverQuorumBarrier(ctx context.Context, cluster *apiv1.Cluster) {
	if !cluster.IsFailoverQuorumActive() {
		r.failoverQuorumBarrier.forget()
		return
	}

	config := replication.GetSynchronousStandbyNames(ctx, cluster)
	status := buildFailoverQuorumStatus(&config, r.instance.GetPodName())
	switch {
	case len(status.StandbyNames) == 0:
		r.failoverQuorumBarrier.forget()
	case !equality.Semantic.DeepEqual(r.failoverQuorumBarrier.filesStatus, status):
		r.failoverQuorumBarrier.dropBarrier()
		r.failoverQuorumBarrier.filesStatus = status
	}
}

func (r *InstanceReconciler) shouldManageFailoverQuorumObject(cluster *apiv1.Cluster) bool {
	if cluster.Status.TargetPrimary != r.instance.GetPodName() {
		return false
	}
	if cluster.Status.CurrentPrimary != cluster.Status.TargetPrimary {
		return false
	}
	if cluster.Spec.PostgresConfiguration.Synchronous == nil {
		return false
	}

	return cluster.IsFailoverQuorumActive()
}
