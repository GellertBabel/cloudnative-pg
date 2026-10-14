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
	"errors"
	"time"

	"github.com/cloudnative-pg/machinery/pkg/types"

	apiv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	postgresManagement "github.com/cloudnative-pg/cloudnative-pg/pkg/management/postgres"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/postgres"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// fakeProbe simulates the PostgreSQL instance used to record the barrier
type fakeProbe struct {
	insertLSN  types.LSN
	flushLSN   types.LSN
	requestErr error
	// requestTimesOut makes the checkpoint request outlast the wait
	requestTimesOut bool
	// redoLSN is the redo position of the latest completed checkpoint
	redoLSN types.LSN
	idle    bool

	requests       int
	lastRequestCtx context.Context
}

func (p *fakeProbe) RequestSynchronousReplicationCheckpoint(ctx context.Context) (types.LSN, error) {
	p.requests++
	p.lastRequestCtx = ctx
	if p.requestErr != nil {
		return "", p.requestErr
	}
	if p.requestTimesOut {
		<-ctx.Done()
		return p.insertLSN, ctx.Err()
	}
	return p.insertLSN, nil
}

func (p *fakeProbe) GetLatestCheckpointRedoLSN(context.Context) (types.LSN, error) {
	return p.redoLSN, nil
}

func (p *fakeProbe) IsCheckpointerIdle(context.Context) (bool, error) {
	return p.idle, nil
}

func (p *fakeProbe) GetCurrentWALFlushLSN(context.Context) (types.LSN, error) {
	return p.flushLSN, nil
}

var _ = Describe("buildFailoverQuorumStatus", func() {
	It("is empty when no synchronous replication is configured", func() {
		Expect(buildFailoverQuorumStatus(nil, "pg-1")).To(Equal(apiv1.FailoverQuorumStatus{}))
	})

	It("excludes the primary from the standby names", func() {
		status := buildFailoverQuorumStatus(&postgres.SynchronousStandbyNamesConfig{
			Method:       "ANY",
			NumSync:      1,
			StandbyNames: []string{"pg-1", "pg-2", "pg-3"},
		}, "pg-1")
		Expect(status).To(Equal(apiv1.FailoverQuorumStatus{
			Method:        "ANY",
			Primary:       "pg-1",
			StandbyNumber: 1,
			StandbyNames:  []string{"pg-2", "pg-3"},
		}))
	})
})

var _ = Describe("failoverQuorumBarrier", func() {
	var (
		barrier failoverQuorumBarrier
		probe   *fakeProbe
		now     time.Time
	)

	config := apiv1.FailoverQuorumStatus{
		Method:        "ANY",
		Primary:       "pg-1",
		StandbyNumber: 1,
		StandbyNames:  []string{"pg-2"},
	}
	otherConfig := apiv1.FailoverQuorumStatus{
		Method:        "ANY",
		Primary:       "pg-1",
		StandbyNumber: 1,
		StandbyNames:  []string{"pg-2", "pg-3"},
	}

	BeforeEach(func() {
		barrier = failoverQuorumBarrier{}
		probe = &fakeProbe{insertLSN: "0/3000000", flushLSN: "0/3000100", redoLSN: "0/3000000", idle: true}
		now = time.Now()
	})

	It("records the flush position once the requested checkpoint is done", func(ctx SpecContext) {
		Expect(barrier.get(ctx, config, probe, now)).To(Equal(types.LSN("0/3000100")))
		Expect(probe.requests).To(Equal(1))
		Expect(barrier.enforced).To(BeTrue())
	})

	It("bounds the wait for the requested checkpoint", func(ctx SpecContext) {
		_, _ = barrier.get(ctx, config, probe, now)
		deadline, ok := probe.lastRequestCtx.Deadline()
		Expect(ok).To(BeTrue())
		Expect(time.Until(deadline)).To(BeNumerically("<=", synchronousReplicationCheckpointWait))
	})

	It("reuses the barrier while the configuration is unchanged", func(ctx SpecContext) {
		_, _ = barrier.get(ctx, config, probe, now)
		probe.flushLSN = "0/4000000"
		Expect(barrier.get(ctx, config, probe, now)).To(Equal(types.LSN("0/3000100")))
		Expect(probe.requests).To(Equal(1))
	})

	It("moves the barrier without a checkpoint when the configuration changes", func(ctx SpecContext) {
		_, _ = barrier.get(ctx, config, probe, now)
		probe.flushLSN = "0/4000000"
		Expect(barrier.get(ctx, otherConfig, probe, now)).To(Equal(types.LSN("0/4000000")))
		Expect(probe.requests).To(Equal(1))
	})

	It("does not count a checkpoint that started before the request", func(ctx SpecContext) {
		probe.redoLSN = "0/2FFFFFF"
		Expect(barrier.get(ctx, config, probe, now)).To(BeEmpty())
		Expect(barrier.enforced).To(BeFalse())
	})

	When("the checkpointer stays busy", func() {
		BeforeEach(func() {
			probe.idle = false
		})

		It("waits after the first checkpoint that started after the request", func(ctx SpecContext) {
			Expect(barrier.get(ctx, config, probe, now)).To(BeEmpty())
			Expect(barrier.get(ctx, config, probe, now)).To(BeEmpty())
		})

		It("confirms once a later checkpoint completed", func(ctx SpecContext) {
			_, _ = barrier.get(ctx, config, probe, now)
			probe.redoLSN = "0/4000000"
			Expect(barrier.get(ctx, config, probe, now)).To(Equal(types.LSN("0/3000100")))
			Expect(probe.requests).To(Equal(1))
		})
	})

	When("the requested checkpoint outlasts the wait", func() {
		BeforeEach(func() {
			probe.requestTimesOut = true
			probe.redoLSN = "0/2000000"

			wait := synchronousReplicationCheckpointWait
			synchronousReplicationCheckpointWait = 10 * time.Millisecond
			DeferCleanup(func() { synchronousReplicationCheckpointWait = wait })
		})

		It("reports no barrier yet, without an error", func(ctx SpecContext) {
			Expect(barrier.get(ctx, config, probe, now)).To(BeEmpty())
			Expect(barrier.enforced).To(BeFalse())
		})

		It("checks the pending request again instead of requesting a new one", func(ctx SpecContext) {
			_, _ = barrier.get(ctx, config, probe, now)
			Expect(barrier.get(ctx, config, probe, now.Add(time.Minute))).To(BeEmpty())
			Expect(probe.requests).To(Equal(1))

			probe.redoLSN = "0/3000000"
			Expect(barrier.get(ctx, config, probe, now.Add(2*time.Minute))).To(Equal(types.LSN("0/3000100")))
			Expect(probe.requests).To(Equal(1))
		})

		It("renews the request but keeps the first insert position", func(ctx SpecContext) {
			_, _ = barrier.get(ctx, config, probe, now)
			probe.insertLSN = "0/5000000"
			probe.redoLSN = "0/4000000"
			Expect(barrier.get(ctx, config, probe, now.Add(synchronousReplicationCheckpointRenewal))).
				To(Equal(types.LSN("0/3000100")))
			Expect(probe.requests).To(Equal(2))
		})
	})

	It("does not keep a request that failed", func(ctx SpecContext) {
		probe.requestErr = errors.New("permission denied")
		_, err := barrier.get(ctx, config, probe, now)
		Expect(err).To(HaveOccurred())
		Expect(barrier.requestLSN).To(BeEmpty())

		probe.requestErr = nil
		Expect(barrier.get(ctx, config, probe, now)).To(Equal(types.LSN("0/3000100")))
		Expect(probe.requests).To(Equal(2))
	})

	It("requests a new checkpoint after forget", func(ctx SpecContext) {
		_, _ = barrier.get(ctx, config, probe, now)
		barrier.forget()
		_, _ = barrier.get(ctx, config, probe, now)
		Expect(probe.requests).To(Equal(2))
	})
})

var _ = Describe("invalidateFailoverQuorumBarrier", func() {
	newCluster := func(healthy ...string) *apiv1.Cluster {
		return &apiv1.Cluster{
			Spec: apiv1.ClusterSpec{
				Instances: 3,
				PostgresConfiguration: apiv1.PostgresConfiguration{
					Synchronous: &apiv1.SynchronousReplicaConfiguration{
						Method:         apiv1.SynchronousReplicaConfigurationMethodAny,
						Number:         1,
						DataDurability: apiv1.DataDurabilityLevelPreferred,
						FailoverQuorum: true,
					},
				},
			},
			Status: apiv1.ClusterStatus{
				CurrentPrimary:  "pg-1",
				TargetPrimary:   "pg-1",
				InstancesStatus: map[apiv1.PodStatus][]string{apiv1.PodHealthy: healthy},
			},
		}
	}

	var r *InstanceReconciler

	recorded := apiv1.FailoverQuorumStatus{
		Method:        "ANY",
		Primary:       "pg-1",
		StandbyNumber: 1,
		StandbyNames:  []string{"pg-2"},
	}

	BeforeEach(func() {
		r = &InstanceReconciler{instance: postgresManagement.NewInstance().WithPodName("pg-1")}
		r.failoverQuorumBarrier = failoverQuorumBarrier{
			enforced:    true,
			status:      recorded,
			lsn:         "0/3000000",
			filesStatus: recorded,
		}
	})

	It("keeps the barrier while the configuration files don't change", func(ctx SpecContext) {
		r.invalidateFailoverQuorumBarrier(ctx, newCluster("pg-1", "pg-2"))
		Expect(r.failoverQuorumBarrier.lsn).To(Equal(types.LSN("0/3000000")))
	})

	It("drops only the barrier when the files get other synchronous standbys", func(ctx SpecContext) {
		r.invalidateFailoverQuorumBarrier(ctx, newCluster("pg-1", "pg-2", "pg-3"))
		Expect(r.failoverQuorumBarrier.lsn).To(BeEmpty())
		Expect(r.failoverQuorumBarrier.enforced).To(BeTrue())
	})

	It("drops the barrier once when the files differ from the loaded configuration", func(ctx SpecContext) {
		// A configuration that PostgreSQL failed to load must not move the
		// barrier at every reconciliation
		cluster := newCluster("pg-1", "pg-2", "pg-3")
		r.invalidateFailoverQuorumBarrier(ctx, cluster)
		r.failoverQuorumBarrier.status = recorded
		r.failoverQuorumBarrier.lsn = "0/4000000"

		r.invalidateFailoverQuorumBarrier(ctx, cluster)
		Expect(r.failoverQuorumBarrier.lsn).To(Equal(types.LSN("0/4000000")))
	})

	It("forgets everything when the files have no synchronous standby", func(ctx SpecContext) {
		r.invalidateFailoverQuorumBarrier(ctx, newCluster("pg-1"))
		Expect(r.failoverQuorumBarrier).To(Equal(failoverQuorumBarrier{}))
	})

	It("forgets everything when the failover quorum is not active", func(ctx SpecContext) {
		cluster := newCluster("pg-1", "pg-2")
		cluster.Spec.PostgresConfiguration.Synchronous = nil
		r.invalidateFailoverQuorumBarrier(ctx, cluster)
		Expect(r.failoverQuorumBarrier).To(Equal(failoverQuorumBarrier{}))
	})
})

var _ = Describe("desiredFailoverQuorumStatus", func() {
	metadata := &postgres.SynchronousStandbyNamesConfig{
		Method:       "ANY",
		NumSync:      1,
		StandbyNames: []string{"pg-2"},
	}

	var (
		r     *InstanceReconciler
		probe *fakeProbe
	)

	BeforeEach(func() {
		r = &InstanceReconciler{instance: postgresManagement.NewInstance().WithPodName("pg-1")}
		probe = &fakeProbe{insertLSN: "0/3000000", flushLSN: "0/3000100", redoLSN: "0/3000000", idle: true}
	})

	It("publishes the standbys with the barrier", func(ctx SpecContext) {
		status, requeue := r.desiredFailoverQuorumStatus(ctx, metadata, probe, time.Now())
		Expect(requeue).To(BeFalse())
		Expect(status).To(Equal(apiv1.FailoverQuorumStatus{
			Method:        "ANY",
			Primary:       "pg-1",
			StandbyNumber: 1,
			StandbyNames:  []string{"pg-2"},
			BarrierLSN:    "0/3000100",
		}))
	})

	It("denies failovers and requests a retry while the checkpoint is pending", func(ctx SpecContext) {
		probe.redoLSN = "0/2000000"
		status, requeue := r.desiredFailoverQuorumStatus(ctx, metadata, probe, time.Now())
		Expect(requeue).To(BeTrue())
		Expect(status).To(Equal(apiv1.FailoverQuorumStatus{}))
	})

	It("denies failovers and requests a retry when the barrier cannot be recorded", func(ctx SpecContext) {
		probe.requestErr = errors.New("permission denied")
		status, requeue := r.desiredFailoverQuorumStatus(ctx, metadata, probe, time.Now())
		Expect(requeue).To(BeTrue())
		Expect(status).To(Equal(apiv1.FailoverQuorumStatus{}))
	})

	It("needs no barrier and forgets everything without synchronous standbys", func(ctx SpecContext) {
		r.failoverQuorumBarrier = failoverQuorumBarrier{enforced: true, lsn: "0/3000000"}
		status, requeue := r.desiredFailoverQuorumStatus(ctx, nil, probe, time.Now())
		Expect(requeue).To(BeFalse())
		Expect(status).To(Equal(apiv1.FailoverQuorumStatus{}))
		Expect(r.failoverQuorumBarrier).To(Equal(failoverQuorumBarrier{}))
		Expect(probe.requests).To(BeZero())
	})
})
