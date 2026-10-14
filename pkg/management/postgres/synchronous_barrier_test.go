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

package postgres

import (
	"database/sql"
	"errors"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/cloudnative-pg/machinery/pkg/types"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("synchronous replication checkpoint", func() {
	const (
		insertLSNQuery = "SELECT pg_catalog.pg_current_wal_insert_lsn()"
		redoQuery      = "SELECT redo_lsn FROM pg_catalog.pg_control_checkpoint()"
		idleQuery      = "SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_stat_activity " +
			"WHERE backend_type = 'checkpointer' AND wait_event = 'CheckpointerMain')"
	)

	var (
		db   *sql.DB
		mock sqlmock.Sqlmock
	)

	BeforeEach(func() {
		var err error
		db, mock, err = sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
		Expect(err).ToNot(HaveOccurred())
	})

	AfterEach(func() {
		Expect(mock.ExpectationsWereMet()).To(Succeed())
	})

	It("reads the insert position before requesting the checkpoint", func(ctx SpecContext) {
		mock.ExpectQuery(insertLSNQuery).WillReturnRows(sqlmock.NewRows([]string{"lsn"}).AddRow("0/3000000"))
		mock.ExpectExec("CHECKPOINT").WillReturnResult(sqlmock.NewResult(0, 0))

		Expect(requestSynchronousReplicationCheckpoint(ctx, db)).To(Equal(types.LSN("0/3000000")))
	})

	It("returns the insert position when the checkpoint request fails", func(ctx SpecContext) {
		mock.ExpectQuery(insertLSNQuery).WillReturnRows(sqlmock.NewRows([]string{"lsn"}).AddRow("0/3000000"))
		mock.ExpectExec("CHECKPOINT").WillReturnError(errors.New("canceling statement"))

		lsn, err := requestSynchronousReplicationCheckpoint(ctx, db)
		Expect(err).To(HaveOccurred())
		Expect(lsn).To(Equal(types.LSN("0/3000000")))
	})

	It("reads the redo position of the latest checkpoint", func(ctx SpecContext) {
		mock.ExpectQuery(redoQuery).WillReturnRows(sqlmock.NewRows([]string{"redo_lsn"}).AddRow("0/3000000"))

		Expect(getLatestCheckpointRedoLSN(ctx, db)).To(Equal(types.LSN("0/3000000")))
	})

	DescribeTable("reads whether the checkpointer is idle",
		func(ctx SpecContext, idle bool) {
			mock.ExpectQuery(idleQuery).WillReturnRows(sqlmock.NewRows([]string{"idle"}).AddRow(idle))

			Expect(isCheckpointerIdle(ctx, db)).To(Equal(idle))
		},
		Entry("idle", true),
		Entry("busy", false),
	)

	It("reads the flush position", func(ctx SpecContext) {
		mock.ExpectQuery("SELECT pg_catalog.pg_current_wal_flush_lsn()").
			WillReturnRows(sqlmock.NewRows([]string{"lsn"}).AddRow("0/3000100"))

		Expect(getCurrentWALFlushLSN(ctx, db)).To(Equal(types.LSN("0/3000100")))
	})
})
