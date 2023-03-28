//go:build upgrade

/*
 * Copyright 2023 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package query

import (
	"log"
	"testing"
	"time"

	"github.com/dgraph-io/dgraph/dgraphtest"
	"github.com/dgraph-io/dgraph/x"
)

func TestMain(m *testing.M) {
	mutate := func(c dgraphtest.Cluster) {
		dg, err := c.Client()
		x.Panic(err)

		client = dg
		dc = c
		populateCluster()
	}

	query := func(c dgraphtest.Cluster) {
		dg, err := c.Client()
		x.Panic(err)

		client = dg
		dc = c
		if m.Run() != 0 {
			panic("tests failed")
		}
	}

	runTest := func(before, after string) {
		conf := dgraphtest.NewClusterConfig().WithNumAlphas(1).WithNumZeros(1).
			WithReplicas(1).WithACL(time.Hour).WithVersion(before)
		c, err := dgraphtest.NewLocalCluster(conf)
		x.Panic(err)
		defer c.Cleanup()
		c.Start()

		mutate(c)
		x.Panic(c.Upgrade(after, dgraphtest.BackupRestore))
		query(c)
	}

	comboVersions := []struct {
		before, after string
	}{
		// {"v20.11.3", "v23.0.0-beta1"},
		// {"v21.03.0", "v23.0.0-beta1"},
		{"v21.03.0-92-g0c9f60156", "v23.0.0-beta1"},
		// {"v21.03.0-96-g65fff46c4-slash", "v23.0.0-beta1"},
		// {"v21.03.0-98-g19f71a78a-slash", "v23.0.0-beta1"},
		// {"v21.03.0-99-g4a03c144a-slash", "v23.0.0-beta1"},
		// {"v21.03.1", "v23.0.0-beta1"},
		// {"v21.03.2", "v23.0.0-beta1"},
		// {"v21.12.0", "v23.0.0-beta1"},
		// {"v22.0.0", "v23.0.0-beta1"},
		{"v22.0.1", "v23.0.0-beta1"},
		// {"v22.0.2", "v23.0.0-beta1"},
	}
	for _, cv := range comboVersions {
		log.Printf("running: backup in [%v], restore in [%v]", cv.before, cv.after)
		runTest(cv.before, cv.after)
	}
}

// userClient, err := testutil.DgraphClient("0.0.0.0:33196")
// x.Panic(err)
// x.Panic(userClient.LoginIntoNamespace(context.Background(), "groot", "password", 0))
// client = userClient
// m.Run()
// setup schema

// do mutations in namespace 0 -- 10

// delete namespace 5 & 10

// queries using two different users, one with access and one without
