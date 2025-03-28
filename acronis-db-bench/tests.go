package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	mssql "github.com/denisenkom/go-mssqldb"
	es8 "github.com/elastic/go-elasticsearch/v8"
	"github.com/gocraft/dbr/v2"
	"github.com/lib/pq"

	"github.com/acronis/perfkit/benchmark"
	"github.com/acronis/perfkit/db"

	tenants "github.com/acronis/perfkit/acronis-db-bench/tenants-cache"
)

const (
	TestSelect      string = "select"      // TestSelect is a test category for SELECT queries
	TestUpdate      string = "update"      // TestUpdate is a test category for UPDATE queries
	TestInsert      string = "insert"      // TestInsert is a test category for INSERT queries
	TestDelete      string = "delete"      // TestDelete is a test category for DELETE queries
	TestTransaction string = "transaction" // TestTransaction is a test category for transactions
	TestOther       string = "other"       // TestOther is a test category for other queries
)

// MinChunk is a minimum number of rows to process in a single chunk
const MinChunk = 5000

// TestGroup is a group of tests
type TestGroup struct {
	name  string
	tests map[string]*TestDesc
}

// NewTestGroup creates a new test group
func NewTestGroup(name string) *TestGroup {
	return &TestGroup{name: name, tests: make(map[string]*TestDesc)}
}

var allTests *TestGroup

func (g *TestGroup) add(t *TestDesc) {
	g.tests[t.name] = t
	_, exists := allTests.tests[t.name]
	if exists {
		FatalError("Internal error: test %s already defined")
	}
	allTests.tests[t.name] = t
}

// TestCategories is a list of all test categories
var TestCategories = []string{TestSelect, TestUpdate, TestInsert, TestDelete, TestTransaction}

type testWorkerFunc func(b *benchmark.Benchmark, c *DBConnector, testDesc *TestDesc, batch int) (loops int)
type orderByFunc func(b *benchmark.Benchmark) string //nolint:unused
type launcherFunc func(b *benchmark.Benchmark, testDesc *TestDesc)

// TestDesc describes a test
type TestDesc struct {
	name        string
	metric      string
	description string
	category    string
	isReadonly  bool // indicates the test doesn't run DDL and doesn't modidy data
	isDBRTest   bool
	databases   []db.DialectName

	table TestTable // SQL table name

	launcherFunc launcherFunc
}

// dbIsSupported returns true if the database is supported by the test
func (t *TestDesc) dbIsSupported(db db.DialectName) bool {
	for _, b := range t.databases {
		if b == db {
			return true
		}
	}

	return false
}

// getDBs returns a string with supported databases
func (t *TestDesc) getDBs() string {
	ret := "["

	for _, database := range db.GetDatabases() {
		if t.dbIsSupported(database.Driver) {
			ret += database.Symbol
		} else {
			ret += "-"
		}
	}
	ret += "]"

	return ret
}

var (
	// ALL is a list of all supported databases
	ALL = []db.DialectName{db.POSTGRES, db.MYSQL, db.MSSQL, db.SQLITE, db.CLICKHOUSE, db.CASSANDRA, db.ELASTICSEARCH, db.OPENSEARCH}
	// RELATIONAL is a list of all supported relational databases
	RELATIONAL = []db.DialectName{db.POSTGRES, db.MYSQL, db.MSSQL, db.SQLITE}
	// PMWSA is a list of all supported databases except ClickHouse
	PMWSA = []db.DialectName{db.POSTGRES, db.MYSQL, db.MSSQL, db.SQLITE, db.CASSANDRA}
	// VECTOR is a list of all supported vector databases
	VECTOR = []db.DialectName{db.ELASTICSEARCH, db.OPENSEARCH}
)

// TestBaseAll tests all tests in the 'base' group
var TestBaseAll = TestDesc{
	name:        "all",
	description: "execute all tests in the 'base' group",
	databases:   ALL,
	//	launcherFunc: ...  # causes 'initialization cycle' go-lang compiler error
}

// TestPing tests just ping DB
var TestPing = TestDesc{
	name:        "ping",
	metric:      "ping/sec",
	description: "just ping DB",
	category:    TestOther,
	isReadonly:  true,
	isDBRTest:   false,
	databases:   ALL,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		worker := func(b *benchmark.Benchmark, c *DBConnector, testDesc *TestDesc, batch int) (loops int) { //nolint:revive
			if err := c.database.Ping(context.Background()); err != nil {
				return 0
			}

			return 1
		}
		testGeneric(b, testDesc, worker, 0)
	},
}

// TestRawQuery tests do custom DB query execution
var TestRawQuery = TestDesc{
	name:        "custom",
	metric:      "queries/sec",
	description: "custom DB query execution",
	category:    TestOther,
	isReadonly:  false,
	isDBRTest:   false,
	databases:   ALL,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		query := b.TestOpts.(*TestOpts).BenchOpts.Query

		var worker testWorkerFunc
		var explain = b.TestOpts.(*TestOpts).DBOpts.Explain

		// Fix for connection leaks: always close the rows object returned from a query
		// to ensure the connection is properly released back to the pool
		if strings.Contains(query, "{") {
			worker = func(b *benchmark.Benchmark, c *DBConnector, testDesc *TestDesc, batch int) (loops int) { //nolint:revive
				q := query
				if strings.Contains(q, "{CTI}") {
					rz := b.Randomizer
					ctiUUID, err := b.Vault.(*DBTestData).TenantsCache.GetRandomCTIUUID(rz, 0)
					if err != nil {
						b.Exit(err)
					}
					q = strings.Replace(q, "{CTI}", "'"+string(ctiUUID)+"'", -1)
				}
				if strings.Contains(query, "{TENANT}") {
					rz := b.Randomizer
					tenantUUID, err := b.Vault.(*DBTestData).TenantsCache.GetRandomTenantUUID(rz, 0, "")
					if err != nil {
						b.Exit(err)
					}
					q = strings.Replace(q, "{TENANT}", "'"+string(tenantUUID)+"'", -1)
				}
				fmt.Printf("query %s\n", q)

				var session = c.database.Session(c.database.Context(context.Background(), explain))
				rows, err := session.Query(q)
				if err != nil {
					b.Exit(err)
				}
				defer rows.Close()

				return 1
			}
		} else {
			worker = func(b *benchmark.Benchmark, c *DBConnector, testDesc *TestDesc, batch int) (loops int) {
				var session = c.database.Session(c.database.Context(context.Background(), explain))

				rows, err := session.Query(query)
				if err != nil {
					b.Exit(err)
				}
				defer rows.Close()

				return 1
			}
		}
		testGeneric(b, testDesc, worker, 0)
	},
}

// TestSelectOne tests do 'SELECT 1'
var TestSelectOne = TestDesc{
	name:        "select-1",
	metric:      "select/sec",
	description: "just do 'SELECT 1'",
	category:    TestSelect,
	isReadonly:  true,
	isDBRTest:   false,
	databases:   ALL,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		worker := func(b *benchmark.Benchmark, c *DBConnector, testDesc *TestDesc, batch int) (loops int) { //nolint:revive
			var ret int
			switch rawSession := c.database.RawSession().(type) {
			case *dbr.Session:
				if err := rawSession.Select("1").LoadOne(&ret); err != nil {
					b.Exit("DBRSelect load error: %v", err)
				}
			case *sql.DB:
				if err := rawSession.QueryRow("SELECT 1").Scan(&ret); err != nil {
					if c.database.DialectName() == db.CASSANDRA {
						// Cassandra driver returns error on SELECT 1
						return 1
					}
					b.Exit("can't do 'SELECT 1': %v", err)
				}
			case *es8.Client:
				var res, err = rawSession.Search(
					rawSession.Search.WithContext(context.Background()),
					rawSession.Search.WithBody(strings.NewReader(`{"size": 1}`)),
				)
				if err != nil {
					b.Exit("can't do 'SELECT 1': %v", err)
				}

				// nolint: errcheck // Need to have logger here for deferred errors
				defer res.Body.Close()

				if res.IsError() {
					if res.StatusCode != 404 {
						b.Exit("failed to perform search: %s", res.String())
					}
				}

				if res.StatusCode != 200 {
					b.Exit("failed to perform search: %s", res.String())
				}
			default:
				b.Exit("unknown driver: '%v', supported drivers are: postgres|sqlite|mysql|mssql", c.database.DialectName())
			}

			return 1
		}
		testGeneric(b, testDesc, worker, 0)
	},
}

// TestSelectNextVal tests increment a DB sequence in a loop (or use SELECT FOR UPDATE, UPDATE)
var TestSelectNextVal = TestDesc{
	name:        "select-nextval",
	metric:      "ops/sec",
	description: "increment a DB sequence in a loop (or use SELECT FOR UPDATE, UPDATE)",
	category:    TestOther,
	isReadonly:  true,
	isDBRTest:   false,
	databases:   RELATIONAL,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		c := dbConnector(b)
		c.database.CreateSequence(SequenceName)

		worker := func(b *benchmark.Benchmark, c *DBConnector, testDesc *TestDesc, batch int) (loops int) { //nolint:revive
			var explain = b.TestOpts.(*TestOpts).DBOpts.Explain
			var session = c.database.Session(c.database.Context(context.Background(), explain))
			if _, err := session.GetNextVal(SequenceName); err != nil {
				b.Exit(err)
			}

			return 1
		}

		testGeneric(b, testDesc, worker, 0)
	},
}

// TestSelectMediumLast tests select last row from the 'medium' table with few columns and 1 index
var TestSelectMediumLast = TestDesc{
	name:        "select-medium-last",
	metric:      "rows/sec",
	description: "select last row from the 'medium' table with few columns and 1 index",
	category:    TestSelect,
	isReadonly:  true,
	isDBRTest:   false,
	databases:   ALL,
	table:       TestTableMedium,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		var idToRead int64
		var orderBy = func(worker *benchmark.BenchmarkWorker) []string { return []string{"desc(id)"} } //nolint:revive
		testSelect(b, testDesc, nil, []string{"id"}, []interface{}{&idToRead}, nil, orderBy, 1)
	},
}

// TestSelectMediumLastDBR tests select last row from the 'medium' table with few columns and 1 index using golang DBR query builder
var TestSelectMediumLastDBR = TestDesc{
	name:        "dbr-select-medium-last",
	metric:      "rows/sec",
	description: "select last row from the 'medium' table with few columns and 1 index",
	category:    TestSelect,
	isReadonly:  true,
	isDBRTest:   true,
	databases:   RELATIONAL,
	table:       TestTableMedium,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		var idToRead int64
		var orderBy = func(worker *benchmark.BenchmarkWorker) []string { return []string{"desc(id)"} } //nolint:revive
		testSelect(b, testDesc, nil, []string{"id"}, []interface{}{&idToRead}, nil, orderBy, 1)
	},
}

// TestSelectMediumRand selects random row from the 'medium' table with few columns and 1 index
var TestSelectMediumRand = TestDesc{
	name:        "select-medium-rand",
	metric:      "rows/sec",
	description: "select random row from the 'medium' table with few columns and 1 index",
	category:    TestSelect,
	isReadonly:  true,
	isDBRTest:   false,
	databases:   ALL,
	table:       TestTableMedium,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		var idToRead int64

		var where = func(worker *benchmark.BenchmarkWorker) map[string][]string {
			id := worker.Randomizer.Uintn64(testDesc.table.RowsCount - 1)

			return map[string][]string{"id": {fmt.Sprintf("ge(%d)", id)}}
		}

		var orderBy = func(worker *benchmark.BenchmarkWorker) []string { //nolint:revive
			return []string{"asc(id)"}
		}

		testSelect(b, testDesc, nil, []string{"id"}, []interface{}{&idToRead}, where, orderBy, 1)
	},
}

// TestSelectMediumRandDBR selects random row from the 'medium' table using golang DBR query builder
var TestSelectMediumRandDBR = TestDesc{
	name:        "dbr-select-medium-rand",
	metric:      "rows/sec",
	description: "select random row from the 'medium' table using golang DBR query builder",
	category:    TestSelect,
	isReadonly:  true,
	isDBRTest:   true,
	databases:   RELATIONAL,
	table:       TestTableMedium,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		var idToRead int64

		var where = func(worker *benchmark.BenchmarkWorker) map[string][]string {
			var id = worker.Randomizer.Uintn64(testDesc.table.RowsCount - 1)

			return map[string][]string{"id": {fmt.Sprintf("gt(%d)", id)}}
		}

		var orderBy = func(worker *benchmark.BenchmarkWorker) []string { //nolint:revive
			return []string{"asc(id)"}
		}
		testSelect(b, testDesc, nil, []string{"id"}, []interface{}{&idToRead}, where, orderBy, 1)
	},
}

// TestSelectHeavyLast selects last row from the 'heavy' table
var TestSelectHeavyLast = TestDesc{
	name:        "select-heavy-last",
	metric:      "rows/sec",
	description: "select last row from the 'heavy' table",
	category:    TestSelect,
	isReadonly:  true,
	isDBRTest:   false,
	databases:   RELATIONAL,
	table:       TestTableHeavy,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		var idToRead int64
		var orderBy = func(worker *benchmark.BenchmarkWorker) []string { return []string{"desc(id)"} } //nolint:revive
		testSelect(b, testDesc, nil, []string{"id"}, []interface{}{&idToRead}, nil, orderBy, 1)
	},
}

// TestSelectHeavyLastDBR selects last row from the 'heavy' table using golang DBR driver
var TestSelectHeavyLastDBR = TestDesc{
	name:        "dbr-select-heavy-last",
	metric:      "rows/sec",
	description: "select last row from the 'heavy' table using golang DBR driver",
	category:    TestSelect,
	isReadonly:  true,
	isDBRTest:   true,
	databases:   RELATIONAL,
	table:       TestTableHeavy,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		var idToRead int64
		var orderBy = func(worker *benchmark.BenchmarkWorker) []string { return []string{"desc(id)"} } //nolint:revive
		testSelect(b, testDesc, nil, []string{"id"}, []interface{}{&idToRead}, nil, orderBy, 1)
	},
}

// TestSelectHeavyRand selects random row from the 'heavy' table
var TestSelectHeavyRand = TestDesc{
	name:        "select-heavy-rand",
	metric:      "rows/sec",
	description: "select random row from the 'heavy' table",
	category:    TestSelect,
	isReadonly:  true,
	isDBRTest:   false,
	databases:   RELATIONAL,
	table:       TestTableHeavy,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		var idToRead int64

		var where = func(worker *benchmark.BenchmarkWorker) map[string][]string {
			id := worker.Randomizer.Uintn64(testDesc.table.RowsCount - 1)

			return map[string][]string{"id": {fmt.Sprintf("gt(%d)", id)}}
		}

		var orderBy = func(worker *benchmark.BenchmarkWorker) []string { //nolint:revive
			return []string{"asc(id)"}
		}
		testSelect(b, testDesc, nil, []string{"id"}, []interface{}{&idToRead}, where, orderBy, 1)
	},
}

// TestSelectHeavyRandDBR selects random row from the 'heavy' table using golang DBR query builder
var TestSelectHeavyRandDBR = TestDesc{
	name:        "dbr-select-heavy-rand",
	metric:      "rows/sec",
	description: "select random row from the 'heavy' table using golang DBR query builder",
	category:    TestSelect,
	isReadonly:  true,
	isDBRTest:   true,
	databases:   RELATIONAL,
	table:       TestTableHeavy,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		var idToRead int64

		var where = func(worker *benchmark.BenchmarkWorker) map[string][]string {
			id := worker.Randomizer.Uintn64(testDesc.table.RowsCount - 1)

			return map[string][]string{"id": {fmt.Sprintf("gt(%d)", id)}}
		}

		var orderBy = func(worker *benchmark.BenchmarkWorker) []string { //nolint:revive
			return []string{"asc(id)"}
		}
		testSelect(b, testDesc, nil, []string{"id"}, []interface{}{&idToRead}, where, orderBy, 1)
	},
}

// TestSelectHeavyRandTenantLike selects random row from the 'heavy' table WHERE tenant_id = {} AND resource_name LIKE {}
var TestSelectHeavyRandTenantLike = TestDesc{
	name:        "select-heavy-rand-in-tenant-like",
	metric:      "rows/sec",
	description: "select random row from the 'heavy' table WHERE tenant_id = {} AND resource_name LIKE {}",
	category:    TestSelect,
	isReadonly:  true,
	isDBRTest:   false,
	databases:   RELATIONAL,
	table:       TestTableHeavy,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		var colConfs = testDesc.table.GetColumnsConf([]string{"tenant_id"}, false)

		var idToRead int64

		var where = func(worker *benchmark.BenchmarkWorker) map[string][]string {
			w, err := worker.Randomizer.GenFakeDataAsMap(colConfs, false)
			if err != nil {
				worker.Exit(err)
			}

			var tenant = fmt.Sprintf("%s", (*w)["tenant_id"])
			if tenant == "" {
				worker.Logger.Warn("tenant_id is empty")
			}

			return map[string][]string{
				"tenant_id":     {fmt.Sprintf("%s", (*w)["tenant_id"])},
				"resource_name": {"like(a)"},
			}
		}

		var orderBy = func(worker *benchmark.BenchmarkWorker) []string { //nolint:revive
			return []string{"desc(id)"}
		}
		testSelect(b, testDesc, nil, []string{"id"}, []interface{}{&idToRead}, where, orderBy, 1)
	},
}

// TestSelectHeavyMinMaxTenant selects min(completion_time_ns) and max(completion_time_ns) value from the 'heavy' table WHERE tenant_id = {}
var TestSelectHeavyMinMaxTenant = TestDesc{
	name:        "select-heavy-minmax-in-tenant",
	metric:      "rows/sec",
	description: "select min(completion_time_ns) and max(completion_time_ns) value from the 'heavy' table WHERE tenant_id = {}",
	category:    TestSelect,
	isReadonly:  true,
	isDBRTest:   false,
	databases:   RELATIONAL,
	table:       TestTableHeavy,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		var colConfs = testDesc.table.GetColumnsConf([]string{"tenant_id"}, false)

		var minCompletionTime int64
		var maxCompletionTime int64

		var where = func(worker *benchmark.BenchmarkWorker) map[string][]string {
			w, err := worker.Randomizer.GenFakeDataAsMap(colConfs, false)
			if err != nil {
				worker.Exit(err)
			}

			return map[string][]string{"tenant_id": {fmt.Sprintf("%s", (*w)["tenant_id"])}}
		}
		testSelect(b, testDesc, nil, []string{"min(completion_time)", "max(completion_time)"}, []interface{}{&minCompletionTime, &maxCompletionTime}, where, nil, 1)
	},
}

// TestSelectHeavyMinMaxTenantAndState selects min(completion_time_ns) and max(completion_time_ns) value from the 'heavy' table WHERE tenant_id = {} AND state = {}
var TestSelectHeavyMinMaxTenantAndState = TestDesc{
	name:        "select-heavy-minmax-in-tenant-and-state",
	metric:      "rows/sec",
	description: "select min(completion_time) and max(completion_time) value from the 'heavy' table WHERE tenant_id = {} AND state = {}",
	category:    TestSelect,
	isReadonly:  true,
	isDBRTest:   false,
	databases:   RELATIONAL,
	table:       TestTableHeavy,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {

		var colConfs = testDesc.table.GetColumnsConf([]string{"tenant_id", "state"}, false)

		var minCompletionTime int64
		var maxCompletionTime int64

		var where = func(worker *benchmark.BenchmarkWorker) map[string][]string {
			w, err := worker.Randomizer.GenFakeDataAsMap(colConfs, false)
			if err != nil {
				worker.Exit(err)
			}

			return map[string][]string{
				"tenant_id": {fmt.Sprintf("%s", (*w)["tenant_id"])},
				"state":     {fmt.Sprintf("%d", (*w)["state"])},
			}
		}

		testSelect(b, testDesc, nil, []string{"min(completion_time)", "max(completion_time)"}, []interface{}{&minCompletionTime, &maxCompletionTime}, where, nil, 1)
	},
}

// TestSelectHeavyRandPageByUUID selects random N rows from the 'heavy' table WHERE uuid IN (...)
var TestSelectHeavyRandPageByUUID = TestDesc{
	name:        "select-heavy-rand-page-by-uuid",
	metric:      "rows/sec",
	description: "select page from the 'heavy' table WHERE uuid IN (...)",
	category:    TestSelect,
	isReadonly:  true,
	isDBRTest:   false,
	databases:   ALL,
	table:       TestTableHeavy,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {

		var batchSize = b.Vault.(*DBTestData).EffectiveBatch

		var colConfs []benchmark.DBFakeColumnConf
		for i := 0; i < batchSize; i++ {
			colConfs = append(colConfs, benchmark.DBFakeColumnConf{ColumnName: "uuid", ColumnType: "uuid"})
		}

		var idToRead int64

		var where = func(worker *benchmark.BenchmarkWorker) map[string][]string {
			_, values, err := worker.Randomizer.GenFakeData(&colConfs, false)
			if err != nil {
				worker.Exit(err)
			}

			var valuesToSearch []string
			for _, v := range values {
				valuesToSearch = append(valuesToSearch, fmt.Sprintf("%s", v))
			}

			return map[string][]string{"uuid": valuesToSearch}
		}

		testSelect(b, testDesc, nil, []string{"id"}, []interface{}{&idToRead}, where, nil, 1)
	},
}

// TestSelectHeavyRandCustomerRecent selects random page from the 'heavy' table WHERE tenant_id = {} AND ordered by enqueue_time DESC
var TestSelectHeavyRandCustomerRecent = TestDesc{
	name:        "select-heavy-rand-in-customer-recent",
	metric:      "rows/sec",
	description: "select first page from the 'heavy' table WHERE tenant_id = {} ORDER BY enqueue_time DESC",
	category:    TestSelect,
	isReadonly:  true,
	isDBRTest:   false,
	databases:   ALL,
	table:       TestTableHeavy,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {

		// colConfs := testDesc.table.GetColumnsConf([]string{"tenant_id"}, false)

		var colConfs = &[]benchmark.DBFakeColumnConf{{ColumnName: "customer_id", ColumnType: "customer_uuid"}}

		var idToRead int64

		var where = func(worker *benchmark.BenchmarkWorker) map[string][]string {
			w, err := worker.Randomizer.GenFakeDataAsMap(colConfs, false)
			if err != nil {
				worker.Exit(err)
			}

			return map[string][]string{
				"tenant_vis_list": {fmt.Sprintf("%s", (*w)["customer_id"])},
			}
		}

		var orderBy = func(worker *benchmark.BenchmarkWorker) []string { //nolint:revive
			return []string{"desc(enqueue_time)"}
		}

		testSelect(b, testDesc, nil, []string{"id"}, []interface{}{&idToRead}, where, orderBy, 1)
	},
}

// TestSelectHeavyRandCustomerRecentLike selects random page from the 'heavy' table WHERE tenant_id = {} AND policy_name LIKE '%k%' AND ordered by enqueue_time DESC
var TestSelectHeavyRandCustomerRecentLike = TestDesc{
	name:        "select-heavy-rand-in-customer-recent-like",
	metric:      "rows/sec",
	description: "select first page from the 'heavy' table WHERE tenant_id = {} AND policy_name LIKE '%k%' ORDER BY enqueue_time DESC",
	category:    TestSelect,
	isReadonly:  true,
	isDBRTest:   false,
	databases:   ALL,
	table:       TestTableHeavy,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {

		// colConfs := testDesc.table.GetColumnsConf([]string{"tenant_id"}, false)

		var colConfs = &[]benchmark.DBFakeColumnConf{{ColumnName: "customer_id", ColumnType: "customer_uuid"}}

		var idToRead int64

		var where = func(worker *benchmark.BenchmarkWorker) map[string][]string {
			w, err := worker.Randomizer.GenFakeDataAsMap(colConfs, false)
			if err != nil {
				worker.Exit(err)
			}

			return map[string][]string{
				"tenant_vis_list": {fmt.Sprintf("%s", (*w)["customer_id"])},
				"policy_name":     {"like(k)"},
			}
		}

		var orderBy = func(worker *benchmark.BenchmarkWorker) []string { //nolint:revive
			return []string{"desc(enqueue_time)"}
		}

		testSelect(b, testDesc, nil, []string{"id"}, []interface{}{&idToRead}, where, orderBy, 1)
	},
}

// TestSelectHeavyRandCustomerUpdateTimePage selects random page from the 'heavy' table WHERE tenant_id = {} AND ordered by enqueue_time DESC
var TestSelectHeavyRandCustomerUpdateTimePage = TestDesc{
	name:        "select-heavy-rand-customer-update-time-page",
	metric:      "rows/sec",
	description: "select first page from the 'heavy' table WHERE customer_id = {} AND update_time_ns in 1h interval ORDER BY update_time DESC",
	category:    TestSelect,
	isReadonly:  true,
	isDBRTest:   false,
	databases:   ALL,
	table:       TestTableHeavy,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {

		var colConfs = []benchmark.DBFakeColumnConf{{ColumnName: "customer_id", ColumnType: "customer_uuid"}}
		colConfs = append(colConfs, benchmark.DBFakeColumnConf{ColumnName: "update_time", ColumnType: "time", Cardinality: 30})

		var idToRead int64

		var where = func(worker *benchmark.BenchmarkWorker) map[string][]string {
			w, err := worker.Randomizer.GenFakeDataAsMap(&colConfs, false)
			if err != nil {
				worker.Exit(err)
			}

			var pageStart = (*w)["update_time"].(time.Time)
			var pageEnd = pageStart.Add(time.Hour)

			return map[string][]string{
				"tenant_vis_list": {fmt.Sprintf("%s", (*w)["customer_id"])},
				"update_time": {
					fmt.Sprintf("ge(%s)", pageStart.Format(time.RFC3339)),
					fmt.Sprintf("lt(%s)", pageEnd.Format(time.RFC3339)),
				},
			}
		}

		var orderBy = func(worker *benchmark.BenchmarkWorker) []string { //nolint:revive
			return []string{"asc(update_time)"}
		}

		testSelect(b, testDesc, nil, []string{"id"}, []interface{}{&idToRead}, where, orderBy, 1)
	},
}

// TestSelectHeavyRandCustomerCount selects random count of rows from the 'heavy' table WHERE tenant_id = {}
var TestSelectHeavyRandCustomerCount = TestDesc{
	name:        "select-heavy-rand-in-customer-count",
	metric:      "rows/sec",
	description: "select COUNT(0) from the 'heavy' table WHERE tenant_id = {}",
	category:    TestSelect,
	isReadonly:  true,
	isDBRTest:   false,
	databases:   ALL,
	table:       TestTableHeavy,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		var colConfs = &[]benchmark.DBFakeColumnConf{{ColumnName: "customer_id", ColumnType: "customer_uuid"}}

		var countToRead int64

		var where = func(worker *benchmark.BenchmarkWorker) map[string][]string {
			w, err := worker.Randomizer.GenFakeDataAsMap(colConfs, false)
			if err != nil {
				worker.Exit(err)
			}

			return map[string][]string{
				"tenant_vis_list": {fmt.Sprintf("%s", (*w)["customer_id"])},
			}
		}

		testSelect(b, testDesc, nil, []string{"COUNT(0)"}, []interface{}{&countToRead}, where, nil, 1)
	},
}

// TestSelectHeavyRandPartnerRecent selects random page from the 'heavy' table WHERE tenant_id = {} AND ordered by enqueue_time DESC
var TestSelectHeavyRandPartnerRecent = TestDesc{
	name:        "select-heavy-rand-in-partner-recent",
	metric:      "rows/sec",
	description: "select first page from the 'heavy' table WHERE partner_id = {} ORDER BY enqueue_time DESC",
	category:    TestSelect,
	isReadonly:  true,
	isDBRTest:   false,
	databases:   ALL,
	table:       TestTableHeavy,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		var colConfs = &[]benchmark.DBFakeColumnConf{{ColumnName: "partner_id", ColumnType: "partner_uuid"}}

		var idToRead int64

		var where = func(worker *benchmark.BenchmarkWorker) map[string][]string {
			w, err := worker.Randomizer.GenFakeDataAsMap(colConfs, false)
			if err != nil {
				worker.Exit(err)
			}

			return map[string][]string{
				"tenant_vis_list": {fmt.Sprintf("%s", (*w)["partner_id"])},
			}
		}

		var orderBy = func(worker *benchmark.BenchmarkWorker) []string { //nolint:revive
			return []string{"desc(enqueue_time)"}
		}

		testSelect(b, testDesc, nil, []string{"id"}, []interface{}{&idToRead}, where, orderBy, 1)
	},
}

// TestSelectHeavyRandPartnerStartUpdateTimePage selects random page from the 'heavy' table WHERE tenant_id = {} AND ordered by enqueue_time DESC
var TestSelectHeavyRandPartnerStartUpdateTimePage = TestDesc{
	name:        "select-heavy-rand-partner-start-update-time-page",
	metric:      "rows/sec",
	description: "select first page from the 'heavy' table WHERE partner_id = {} ORDER BY enqueue_time DESC",
	category:    TestSelect,
	isReadonly:  true,
	isDBRTest:   false,
	databases:   ALL,
	table:       TestTableHeavy,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		var colConfs = []benchmark.DBFakeColumnConf{{ColumnName: "partner_id", ColumnType: "partner_uuid"}}
		colConfs = append(colConfs, benchmark.DBFakeColumnConf{ColumnName: "update_time", ColumnType: "time", Cardinality: 30})

		var idToRead int64

		var where = func(worker *benchmark.BenchmarkWorker) map[string][]string {
			w, err := worker.Randomizer.GenFakeDataAsMap(&colConfs, false)
			if err != nil {
				worker.Exit(err)
			}

			var pageStart = (*w)["update_time"].(time.Time)
			var pageEnd = pageStart.Add(2 * 24 * time.Hour)

			var pageStartStr = pageStart.Add(-time.Hour)

			return map[string][]string{
				"tenant_vis_list": {fmt.Sprintf("%s", (*w)["partner_id"])},
				"update_time": {
					fmt.Sprintf("ge(%s)", pageStart.Format(time.RFC3339)),
					fmt.Sprintf("lt(%s)", pageEnd.Format(time.RFC3339)),
				},
				"start_time": {
					fmt.Sprintf("ge(%s)", pageStartStr.Format(time.RFC3339)),
				},
			}
		}

		var orderBy = func(worker *benchmark.BenchmarkWorker) []string { //nolint:revive
			return []string{"asc(update_time)"}
		}

		testSelect(b, testDesc, nil, []string{"id"}, []interface{}{&idToRead}, where, orderBy, 1)
	},
}

// TestSelectHeavyForUpdateSkipLocked selects a row from the 'heavy' table and then updates it
var TestSelectHeavyForUpdateSkipLocked = TestDesc{
	name:        "select-heavy-for-update-skip-locked",
	metric:      "updates/sec",
	description: "do SELECT FOR UPDATE SKIP LOCKED and then UPDATE",
	category:    TestOther,
	isReadonly:  false,
	isDBRTest:   false,
	databases:   RELATIONAL,
	table:       TestTableHeavy,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		var query string
		max := b.CommonOpts.Workers*2 + 1

		var dialectName, err = db.GetDialectName(b.TestOpts.(*TestOpts).DBOpts.ConnString)
		if err != nil {
			b.Exit(err)
		}

		switch dialectName {
		case db.POSTGRES, db.MYSQL:
			query = fmt.Sprintf("SELECT id, progress FROM acronis_db_bench_heavy WHERE id < %d LIMIT 1 FOR UPDATE SKIP LOCKED", max)
		case db.MSSQL:
			query = fmt.Sprintf("SELECT TOP(1) id, progress FROM acronis_db_bench_heavy WITH (UPDLOCK, READPAST, ROWLOCK) WHERE id < %d", max)
		default:
			b.Exit("unsupported driver: '%v', supported drivers are: %s|%s|%s", dialectName, db.POSTGRES, db.MYSQL, db.MSSQL)
		}

		worker := func(b *benchmark.Benchmark, c *DBConnector, testDesc *TestDesc, batch int) (loops int) { //nolint:revive
			var explain = b.TestOpts.(*TestOpts).DBOpts.Explain
			var session = c.database.Session(c.database.Context(context.Background(), explain))
			if txErr := session.Transact(func(tx db.DatabaseAccessor) error {
				var id int64
				var progress int

				if err := session.QueryRow(query).Scan(&id, &progress); err != nil {
					return err
				}

				if _, err := session.Exec(fmt.Sprintf("UPDATE acronis_db_bench_heavy SET progress = %d WHERE id = %d", progress+1, id)); err != nil {
					return err
				}

				return nil
			}); txErr != nil {
				b.Exit(txErr.Error())
			}

			return 1
		}
		testGeneric(b, testDesc, worker, 10000)
	},
}

// TestInsertLight inserts a row into the 'light' table
var TestInsertLight = TestDesc{
	name:        "insert-light",
	metric:      "rows/sec",
	description: "insert a row into the 'light' table",
	category:    TestInsert,
	isReadonly:  false,
	isDBRTest:   false,
	databases:   ALL,
	table:       TestTableLight,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		testInsertGeneric(b, testDesc)
	},
}

// insertByPreparedDataWorker inserts a row into the 'light' table using prepared statement for the batch
func insertByPreparedDataWorker(b *benchmark.Benchmark, c *DBConnector, testDesc *TestDesc, batch int) (loops int) {
	colConfs := testDesc.table.GetColumnsForInsert(db.WithAutoInc(c.database.DialectName()))
	sess := c.database.Session(c.database.Context(context.Background(), false))

	if txErr := sess.Transact(func(tx db.DatabaseAccessor) error {
		columns, _, err := b.Randomizer.GenFakeData(colConfs, false)
		if err != nil {
			b.Exit(err)
		}

		parametersPlaceholder := db.GenDBParameterPlaceholders(0, len(*colConfs))
		sql := fmt.Sprintf("INSERT INTO %s (%s) VALUES(%s)", testDesc.table.TableName, strings.Join(columns, ","), parametersPlaceholder)
		sql = formatSQL(sql, c.database.DialectName())

		stmt, err := tx.Prepare(sql)

		if err != nil {
			c.Exit(err.Error())
		}
		for i := 0; i < batch; i++ {
			_, values, err := b.Randomizer.GenFakeData(colConfs, false)
			if err != nil {
				b.Exit(err)
			}

			_, err = stmt.Exec(values...)

			if err != nil {
				stmt.Close()
				c.Exit(err.Error())
			}
		}

		return nil
	}); txErr != nil {
		c.Exit(txErr.Error())
	}

	return batch
}

// TestInsertLightPrepared inserts a row into the 'light' table using prepared statement for the batch
var TestInsertLightPrepared = TestDesc{
	name:        "insert-light-prepared",
	metric:      "rows/sec",
	description: "insert a row into the 'light' table using prepared statement for the batch",
	category:    TestInsert,
	isReadonly:  false,
	isDBRTest:   false,
	databases:   RELATIONAL,
	table:       TestTableLight,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		testGeneric(b, testDesc, insertByPreparedDataWorker, 0)
	},
}

// insertMultiValueDataWorker inserts a row into the 'light' table using INSERT INTO t (x, y, z) VALUES (..., ..., ...)
func insertMultiValueDataWorker(b *benchmark.Benchmark, c *DBConnector, testDesc *TestDesc, batch int) (loops int) {
	colConfs := testDesc.table.GetColumnsForInsert(db.WithAutoInc(c.database.DialectName()))

	var columns []string
	var values [][]interface{}
	for i := 0; i < batch; i++ {
		var genColumns, vals, err = b.Randomizer.GenFakeData(colConfs, db.WithAutoInc(c.database.DialectName()))
		if err != nil {
			b.Exit(err)
		}
		values = append(values, vals)
		if i == 0 {
			columns = genColumns
		}
	}

	var session = c.database.Session(c.database.Context(context.Background(), false))
	if txErr := session.Transact(func(tx db.DatabaseAccessor) error {
		return tx.BulkInsert(testDesc.table.TableName, values, columns)
	}); txErr != nil {
		b.Exit(txErr.Error())
	}

	return batch
}

// TestInsertLightMultiValue inserts a row into the 'light' table using INSERT INTO t (x, y, z) VALUES (..., ..., ...)
var TestInsertLightMultiValue = TestDesc{
	name:        "insert-light-multivalue",
	metric:      "rows/sec",
	description: "insert a row into the 'light' table using INSERT INTO t (x, y, z) VALUES (..., ..., ...) ",
	category:    TestInsert,
	isReadonly:  false,
	isDBRTest:   false,
	databases:   ALL,
	table:       TestTableLight,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		testGeneric(b, testDesc, insertMultiValueDataWorker, 0)
	},
}

// copyDataWorker copies a row into the 'light' table
func copyDataWorker(b *benchmark.Benchmark, c *DBConnector, testDesc *TestDesc, batch int) (loops int) {
	var sql string
	colConfs := testDesc.table.GetColumnsForInsert(db.WithAutoInc(c.database.DialectName()))
	sess := c.database.Session(c.database.Context(context.Background(), false))

	if txErr := sess.Transact(func(tx db.DatabaseAccessor) error {
		columns, _, err := b.Randomizer.GenFakeData(colConfs, false)
		if err != nil {
			b.Exit(err)
		}

		switch c.database.DialectName() {
		case db.POSTGRES:
			sql = pq.CopyIn(testDesc.table.TableName, columns...)
		case db.MSSQL:
			sql = mssql.CopyIn(testDesc.table.TableName, mssql.BulkOptions{KeepNulls: true, RowsPerBatch: batch}, columns...)
		default:
			b.Exit("unsupported driver: '%v', supported drivers are: %s|%s", c.database.DialectName(), db.POSTGRES, db.MSSQL)
		}

		stmt, err := tx.Prepare(sql)

		if err != nil {
			c.Exit(err.Error())
		}
		for i := 0; i < batch; i++ {
			_, values, err := b.Randomizer.GenFakeData(colConfs, false)
			if err != nil {
				b.Exit(err)
			}

			_, err = stmt.Exec(values...)

			if err != nil {
				stmt.Close()
				c.Exit(err.Error())
			}
		}

		_, err = stmt.Exec()
		if err != nil {
			stmt.Close()
			c.Exit(err.Error())
		}

		return nil
	}); txErr != nil {
		c.Exit(txErr.Error())
	}

	return batch
}

// TestCopyLight copies a row into the 'light' table
var TestCopyLight = TestDesc{
	name:        "copy-light",
	metric:      "rows/sec",
	description: "copy a row into the 'light' table",
	category:    TestInsert,
	isReadonly:  false,
	isDBRTest:   false,
	databases:   []db.DialectName{db.POSTGRES, db.MSSQL},
	table:       TestTableLight,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		testGeneric(b, testDesc, copyDataWorker, 0)
	},
}

// TestInsertLightDBR inserts a row into the 'light' table using goland DBR query builder
var TestInsertLightDBR = TestDesc{
	name:        "dbr-insert-light",
	metric:      "rows/sec",
	description: "insert a row into the 'light' table using goland DBR query builder",
	category:    TestInsert,
	isReadonly:  false,
	isDBRTest:   true,
	databases:   RELATIONAL,
	table:       TestTableLight,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		testInsertGeneric(b, testDesc)
	},
}

// TestInsertMedium inserts a row into the 'medium' table
var TestInsertMedium = TestDesc{
	name:        "insert-medium",
	metric:      "rows/sec",
	description: "insert a row into the 'medium' table",
	category:    TestInsert,
	isReadonly:  false,
	isDBRTest:   false,
	databases:   ALL,
	table:       TestTableMedium,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		testInsertGeneric(b, testDesc)
	},
}

// TestInsertMediumPrepared inserts a row into the 'medium' table using prepared statement for the batch
var TestInsertMediumPrepared = TestDesc{
	name:        "insert-medium-prepared",
	metric:      "rows/sec",
	description: "insert a row into the 'medium' table using prepared statement for the batch",
	category:    TestInsert,
	isReadonly:  false,
	isDBRTest:   false,
	databases:   RELATIONAL,
	table:       TestTableMedium,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		testGeneric(b, testDesc, insertByPreparedDataWorker, 0)
	},
}

// TestInsertMediumMultiValue inserts a row into the 'medium' table using INSERT INTO t (x, y, z) VALUES (..., ..., ...)
var TestInsertMediumMultiValue = TestDesc{
	name:        "insert-medium-multivalue",
	metric:      "rows/sec",
	description: "insert a row into the 'medium' table using INSERT INTO t (x, y, z) VALUES (..., ..., ...) ",
	category:    TestInsert,
	isReadonly:  false,
	isDBRTest:   false,
	databases:   PMWSA,
	table:       TestTableMedium,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		testGeneric(b, testDesc, insertMultiValueDataWorker, 0)
	},
}

// TestCopyMedium copies a row into the 'medium' table
var TestCopyMedium = TestDesc{
	name:        "copy-medium",
	metric:      "rows/sec",
	description: "copy a row into the 'medium' table",
	category:    TestInsert,
	isReadonly:  false,
	isDBRTest:   false,
	databases:   []db.DialectName{db.POSTGRES, db.MSSQL},
	table:       TestTableMedium,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		testGeneric(b, testDesc, copyDataWorker, 0)
	},
}

// TestInsertMediumDBR inserts a row into the 'medium' table using goland DBR query builder
var TestInsertMediumDBR = TestDesc{
	name:        "dbr-insert-medium",
	metric:      "rows/sec",
	description: "insert a row into the 'medium' table using goland DBR query builder",
	category:    TestInsert,
	isReadonly:  false,
	isDBRTest:   true,
	databases:   RELATIONAL,
	table:       TestTableMedium,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		testInsertGeneric(b, testDesc)
	},
}

// TestInsertBlob inserts a row with large random blob into the 'blob' table
var TestInsertBlob = TestDesc{
	name:        "insert-blob",
	metric:      "rows/sec",
	description: "insert a row with large random blob into the 'blob' table",
	category:    TestInsert,
	isReadonly:  false,
	isDBRTest:   false,
	databases:   ALL,
	table:       TestTableBlob,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		testDesc.table.InitColumnsConf()
		for i := range testDesc.table.ColumnsConf {
			if testDesc.table.ColumnsConf[i].ColumnType == "blob" {
				testDesc.table.ColumnsConf[i].MaxSize = b.TestOpts.(*TestOpts).TestcaseOpts.MaxBlobSize
				testDesc.table.ColumnsConf[i].MinSize = b.TestOpts.(*TestOpts).TestcaseOpts.MinBlobSize
			}
		}
		testInsertGeneric(b, testDesc)
	},
}

// TestCopyBlob copies a row with large random blob into the 'blob' table
var TestCopyBlob = TestDesc{
	name:        "copy-blob",
	metric:      "rows/sec",
	description: "copy a row with large random blob into the 'blob' table",
	category:    TestInsert,
	isReadonly:  false,
	isDBRTest:   false,
	databases:   []db.DialectName{db.POSTGRES, db.MSSQL},
	table:       TestTableBlob,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		testDesc.table.InitColumnsConf()
		for i := range testDesc.table.ColumnsConf {
			if testDesc.table.ColumnsConf[i].ColumnType == "blob" {
				testDesc.table.ColumnsConf[i].MaxSize = b.TestOpts.(*TestOpts).TestcaseOpts.MaxBlobSize
				testDesc.table.ColumnsConf[i].MinSize = b.TestOpts.(*TestOpts).TestcaseOpts.MinBlobSize
			}
		}
		testGeneric(b, testDesc, copyDataWorker, 0)
	},
}

// createLargeObjectWorker inserts a row with large random object into the 'largeobject' table
func createLargeObjectWorker(b *benchmark.Benchmark, c *DBConnector, testDesc *TestDesc, batch int) (loops int) {
	colConfs := testDesc.table.GetColumnsForInsert(db.WithAutoInc(c.database.DialectName()))
	parametersPlaceholder := db.GenDBParameterPlaceholders(0, len(*colConfs))

	var session = c.database.Session(c.database.Context(context.Background(), false))

	if txErr := session.Transact(func(tx db.DatabaseAccessor) error {
		var sql string
		for i := 0; i < batch; i++ {
			columns, values, err := b.Randomizer.GenFakeData(colConfs, false)
			if err != nil {
				b.Exit(err)
			}

			blob, err := b.Randomizer.GenFakeValue("blob", "", 0, b.TestOpts.(*TestOpts).TestcaseOpts.MaxBlobSize, b.TestOpts.(*TestOpts).TestcaseOpts.MinBlobSize, nil)
			if err != nil {
				b.Exit(err)
			}

			var oid int
			if err := tx.QueryRow("SELECT lo_create(0)").Scan(&oid); err != nil {
				return err
			}

			var fd int
			if err := tx.QueryRow(fmt.Sprintf("SELECT lo_open(%d, 131072)", oid)).Scan(&fd); err != nil { // 131072 == 0x20000 - write mode
				return err
			}

			if _, err := tx.Exec("SELECT lowrite($1, $2)", fd, blob); err != nil {
				return err
			}

			if _, err := tx.Exec("SELECT lo_close($1)", fd); err != nil {
				return err
			}

			for col := range columns {
				if columns[col] == "oid" {
					values[col] = oid
				}
			}

			if i == 0 {
				insertSQL := "INSERT INTO %s (%s) VALUES(%s)"
				sqlTemplate := fmt.Sprintf(insertSQL, testDesc.table.TableName, strings.Join(columns, ","), parametersPlaceholder)
				sql = formatSQL(sqlTemplate, c.database.DialectName())
			}

			if _, err := tx.Exec(sql, values...); err != nil {
				return err
			}
		}

		return nil
	}); txErr != nil {
		c.Exit(txErr.Error())
	}

	return batch
}

// TestInsertLargeObj inserts a row with large random object into the 'largeobject' table
var TestInsertLargeObj = TestDesc{
	name:        "insert-largeobj",
	metric:      "rows/sec",
	description: "insert a row with large random object into the 'largeobject' table",
	category:    TestInsert,
	isReadonly:  false,
	isDBRTest:   false,
	databases:   []db.DialectName{db.POSTGRES},
	table:       TestTableLargeObj,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		testGeneric(b, testDesc, createLargeObjectWorker, 0)
	},
}

// TestInsertHeavy inserts a row into the 'heavy' table
var TestInsertHeavy = TestDesc{
	name:        "insert-heavy",
	metric:      "rows/sec",
	description: "insert a row into the 'heavy' table",
	category:    TestInsert,
	isReadonly:  false,
	isDBRTest:   false,
	databases:   ALL,
	table:       TestTableHeavy,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		testInsertGeneric(b, testDesc)
	},
}

// TestInsertHeavyPrepared inserts a row into the 'heavy' table using prepared statement for the batch
var TestInsertHeavyPrepared = TestDesc{
	name:        "insert-heavy-prepared",
	metric:      "rows/sec",
	description: "insert a row into the 'heavy' table using prepared statement for the batch",
	category:    TestInsert,
	isReadonly:  false,
	isDBRTest:   false,
	databases:   RELATIONAL,
	table:       TestTableHeavy,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		testGeneric(b, testDesc, insertByPreparedDataWorker, 0)
	},
}

// TestInsertHeavyMultivalue inserts a row into the 'heavy' table using INSERT INTO t (x, y, z) VALUES (..., ..., ...) "
var TestInsertHeavyMultivalue = TestDesc{
	name:        "insert-heavy-multivalue",
	metric:      "rows/sec",
	description: "insert a row into the 'heavy' table using INSERT INTO t (x, y, z) VALUES (..., ..., ...) ",
	category:    TestInsert,
	isReadonly:  false,
	isDBRTest:   false,
	databases:   ALL,
	table:       TestTableHeavy,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		testGeneric(b, testDesc, insertMultiValueDataWorker, 0)
	},
}

// TestCopyHeavy copies a row into the 'heavy' table
var TestCopyHeavy = TestDesc{
	name:        "copy-heavy",
	metric:      "rows/sec",
	description: "copy a row into the 'heavy' table",
	category:    TestInsert,
	isReadonly:  false,
	isDBRTest:   false,
	databases:   []db.DialectName{db.POSTGRES, db.MSSQL},
	table:       TestTableHeavy,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		testGeneric(b, testDesc, copyDataWorker, 0)
	},
}

// TestInsertHeavyDBR inserts a row into the 'heavy' table using golang DB query builder
var TestInsertHeavyDBR = TestDesc{
	name:        "dbr-insert-heavy",
	metric:      "rows/sec",
	description: "insert a row into the 'heavy' table using golang DB query builder",
	category:    TestInsert,
	isReadonly:  false,
	isDBRTest:   true,
	databases:   []db.DialectName{db.POSTGRES, db.ELASTICSEARCH, db.OPENSEARCH},
	table:       TestTableHeavy,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		testInsertGeneric(b, testDesc)
	},
}

// TestInsertVector768MultiValue inserts rows into the 'heavy' table using golang DB query builder
var TestInsertVector768MultiValue = TestDesc{
	name:        "insert-vector-768-multivalue",
	metric:      "rows/sec",
	description: "insert a 768-dim vectors with ids into the 'vector' table by batches",
	category:    TestInsert,
	isReadonly:  false,
	databases:   []db.DialectName{db.POSTGRES, db.ELASTICSEARCH, db.OPENSEARCH},
	table:       TestTableVector768,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		testGeneric(b, testDesc, insertMultiValueDataWorker, 0)
	},
}

// TestSelectVector768NearestL2 selects k nearest vectors by L2 from the 'vector' table to the given vector
var TestSelectVector768NearestL2 = TestDesc{
	name:        "select-vector-768-nearest-l2",
	metric:      "rows/sec",
	description: "selects k nearest vectors by L2 norm from the 'vector' table to the given 768-dim vector",
	category:    TestSelect,
	isReadonly:  false,
	databases:   []db.DialectName{db.POSTGRES, db.ELASTICSEARCH, db.OPENSEARCH},
	table:       TestTableVector768,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		var colConfs = []benchmark.DBFakeColumnConf{
			{ColumnName: "id", ColumnType: "dataset.id"},
			{ColumnName: "embedding", ColumnType: "dataset.emb.list.item"},
		}

		var idToRead int64
		var vectorToRead = make([]float64, 768)

		var orderBy = func(worker *benchmark.BenchmarkWorker) []string { //nolint:revive
			b := worker.Benchmark
			_, vals, err := b.Randomizer.GenFakeData(&colConfs, false)
			if err != nil {
				b.Exit(err)
			}
			var vec = "[" + strings.Trim(strings.Replace(fmt.Sprint(vals[1]), " ", ", ", -1), "[]") + "]"
			return []string{fmt.Sprintf("nearest(embedding;L2;%s)", vec)}
		}

		testSelect(b, testDesc, nil, []string{"id", "embedding"}, []interface{}{&idToRead, &vectorToRead}, nil, orderBy, 1)
	},
}

// TestInsertEmailSecurityMultiValue inserts email security data into the 'email_security' table
var TestInsertEmailSecurityMultiValue = TestDesc{
	name:        "insert-email-security-multivalue",
	metric:      "rows/sec",
	description: "insert an email security data into the 'email_security' table by batches",
	category:    TestInsert,
	isReadonly:  false,
	databases:   []db.DialectName{db.POSTGRES, db.ELASTICSEARCH, db.OPENSEARCH},
	table:       TestTableEmailSecurity,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		testGeneric(b, testDesc, insertMultiValueDataWorker, 0)
	},
}

// TestSelectEmailByEmbeddingNearestL2 selects k nearest vectors by L2 from the 'email_security' table to the given vector
var TestSelectEmailByEmbeddingNearestL2 = TestDesc{
	name:        "select-email-security-768-nearest-l2",
	metric:      "rows/sec",
	description: "selects k nearest emails by vector L2 norm from the 'email_security' table to the given vectorized 768-dim email",
	category:    TestSelect,
	isReadonly:  false,
	databases:   []db.DialectName{db.POSTGRES, db.ELASTICSEARCH, db.OPENSEARCH},
	table:       TestTableEmailSecurity,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		var colConfs = []benchmark.DBFakeColumnConf{
			{ColumnName: "id", ColumnType: "dataset.id"},
			// {ColumnName: "body", ColumnType: "dataset.Body"},
			{ColumnName: "embedding", ColumnType: "dataset.Embedding.list.element"},
		}

		var bodyToRead string
		var vectorToRead = make([]float64, 768)

		var orderBy = func(worker *benchmark.BenchmarkWorker) []string { //nolint:revive
			b := worker.Benchmark
			_, vals, err := b.Randomizer.GenFakeData(&colConfs, false)
			if err != nil {
				b.Exit(err)
			}
			var vec = "[" + strings.Trim(strings.Replace(fmt.Sprint(vals[1]), " ", ", ", -1), "[]") + "]"
			return []string{fmt.Sprintf("nearest(embedding;L2;%s)", vec)}
		}

		testSelect(b, testDesc, nil, []string{"body", "embedding"}, []interface{}{&bodyToRead, &vectorToRead}, nil, orderBy, 1)
	},
}

// TestInsertJSON inserts a row into a table with JSON(b) column
var TestInsertJSON = TestDesc{
	name:        "insert-json",
	metric:      "rows/sec",
	description: "insert a row into a table with JSON(b) column",
	category:    TestInsert,
	isReadonly:  false,
	isDBRTest:   false,
	databases:   []db.DialectName{db.MYSQL, db.POSTGRES},
	table:       TestTableJSON,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		testInsertGeneric(b, testDesc)
	},
}

// TestInsertJSONDBR inserts a row into a table with JSON(b) column using golang DBR driver
var TestInsertJSONDBR = TestDesc{
	name:        "dbr-insert-json",
	metric:      "rows/sec",
	description: "insert a row into a table with JSON(b) column using golang DBR driver",
	category:    TestInsert,
	isReadonly:  false,
	isDBRTest:   true,
	databases:   RELATIONAL,
	table:       TestTableJSON,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		testInsertGeneric(b, testDesc)
	},
}

// TestSelectJSONByIndexedValue selects a row from the 'json' table by some json condition
var TestSelectJSONByIndexedValue = TestDesc{
	name:        "select-json-by-indexed-value",
	metric:      "rows/sec",
	description: "select a row from the 'json' table by some json condition",
	category:    TestSelect,
	isReadonly:  true,
	isDBRTest:   false,
	databases:   []db.DialectName{db.MYSQL, db.POSTGRES},
	table:       TestTableJSON,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		where := func(worker *benchmark.BenchmarkWorker) string {
			id := worker.Randomizer.Uintn64(testDesc.table.RowsCount - 1)

			var dialectName, err = db.GetDialectName(b.TestOpts.(*TestOpts).DBOpts.ConnString)
			if err != nil {
				b.Exit(err)
			}

			switch dialectName {
			case db.MYSQL:
				return "_data_f0f0 = '10' AND id > " + strconv.FormatUint(id, 10)
			case db.POSTGRES:
				return "json_data @> '{\"field0\": {\"field0\": 10}}' AND id > " + strconv.FormatUint(id, 10)
			default:
				b.Exit("The %s test is not supported on driver: %s", testDesc.name, dialectName)
			}

			return ""
		}
		orderby := func(worker *benchmark.BenchmarkWorker) string { //nolint:revive
			return "id ASC"
		}
		testSelectRawSQLQuery(b, testDesc, nil, "id", where, orderby, 1)
	},
}

// TestSearchJSONByIndexedValue searches a row from the 'json' table using some json condition using LIKE {}
var TestSearchJSONByIndexedValue = TestDesc{
	name:        "search-json-by-indexed-value",
	metric:      "rows/sec",
	description: "search a row from the 'json' table using some json condition using LIKE {}",
	category:    TestSelect,
	isReadonly:  true,
	isDBRTest:   false,
	databases:   []db.DialectName{db.MYSQL, db.POSTGRES},
	table:       TestTableJSON,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		where := func(worker *benchmark.BenchmarkWorker) string {
			id := worker.Randomizer.Uintn64(testDesc.table.RowsCount - 1)

			var dialectName, err = db.GetDialectName(b.TestOpts.(*TestOpts).DBOpts.ConnString)
			if err != nil {
				b.Exit(err)
			}

			switch dialectName {
			case db.MYSQL:
				return "_data_f0f0f0 LIKE '%eedl%' AND id > " + strconv.FormatUint(id, 10)
			case db.POSTGRES:
				return "json_data->'field0'->'field0'->>'field0' LIKE '%eedl%' AND id > " + strconv.FormatUint(id, 10) // searching for the 'needle' word
			default:
				b.Exit("The %s test is not supported on driver: %s", testDesc.name, dialectName)
			}

			return ""
		}
		orderby := func(worker *benchmark.BenchmarkWorker) string { //nolint:revive
			return "id ASC"
		}
		testSelectRawSQLQuery(b, testDesc, nil, "id", where, orderby, 1)
	},
}

// TestSelectJSONByNonIndexedValue selects a row from the 'json' table by some json condition
var TestSelectJSONByNonIndexedValue = TestDesc{
	name:        "select-json-by-nonindexed-value",
	metric:      "rows/sec",
	description: "select a row from the 'json' table by some json condition",
	category:    TestSelect,
	isReadonly:  true,
	isDBRTest:   false,
	databases:   []db.DialectName{db.MYSQL, db.POSTGRES},
	table:       TestTableJSON,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		where := func(worker *benchmark.BenchmarkWorker) string {
			id := worker.Randomizer.Uintn64(testDesc.table.RowsCount - 1)

			var dialectName, err = db.GetDialectName(b.TestOpts.(*TestOpts).DBOpts.ConnString)
			if err != nil {
				b.Exit(err)
			}

			switch dialectName {
			case db.MYSQL:
				return "JSON_EXTRACT(json_data, '$.field0.field1') = '10' AND id > " + strconv.FormatUint(id, 10)
			case db.POSTGRES:
				return "json_data @> '{\"field0\": {\"field1\": 10}}' AND id > " + strconv.FormatUint(id, 10)
			default:
				b.Exit("The %s test is not supported on driver: %s", testDesc.name, dialectName)
			}

			return ""
		}
		orderby := func(b *benchmark.BenchmarkWorker) string { //nolint:revive
			return "id ASC"
		}
		testSelectRawSQLQuery(b, testDesc, nil, "id", where, orderby, 1)
	},
}

// TestSearchJSONByNonIndexedValue searches a row from the 'json' table using some json condition using LIKE {}
var TestSearchJSONByNonIndexedValue = TestDesc{
	name:        "search-json-by-nonindexed-value",
	metric:      "rows/sec",
	description: "search a row from the 'json' table using some json condition using LIKE {}",
	category:    TestSelect,
	isReadonly:  true,
	isDBRTest:   false,
	databases:   []db.DialectName{db.MYSQL, db.POSTGRES},
	table:       TestTableJSON,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		where := func(worker *benchmark.BenchmarkWorker) string {
			id := worker.Randomizer.Uintn64(testDesc.table.RowsCount - 1)

			var dialectName, err = db.GetDialectName(b.TestOpts.(*TestOpts).DBOpts.ConnString)
			if err != nil {
				b.Exit(err)
			}

			switch dialectName {
			case db.MYSQL:
				return "JSON_EXTRACT(json_data, '$.field0.field1') LIKE '%eedl%' AND id > " + strconv.FormatUint(id, 10)
			case db.POSTGRES:
				return "json_data->'field0'->'field0'->>'field0' LIKE '%eedl%' AND id > " + strconv.FormatUint(id, 10) // searching for the 'needle' word
			default:
				b.Exit("The %s test is not supported on driver: %s", testDesc.name, dialectName)
			}

			return ""
		}
		orderby := func(worker *benchmark.BenchmarkWorker) string { //nolint:revive
			return "id ASC"
		}
		testSelectRawSQLQuery(b, testDesc, nil, "id", where, orderby, 1)
	},
}

// TestUpdateMedium updates random row in the 'medium' table
var TestUpdateMedium = TestDesc{
	name:        "update-medium",
	metric:      "rows/sec",
	description: "update random row in the 'medium' table",
	category:    TestUpdate,
	isReadonly:  false,
	isDBRTest:   false,
	databases:   RELATIONAL,
	table:       TestTableMedium,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		testUpdateGeneric(b, testDesc, 1, nil)
	},
}

// TestUpdateMediumDBR updates random row in the 'medium' table using golang DBR query builder
var TestUpdateMediumDBR = TestDesc{
	name:        "dbr-update-medium",
	metric:      "rows/sec",
	description: "update random row in the 'medium' table using golang DB driver",
	category:    TestUpdate,
	isReadonly:  false,
	isDBRTest:   true,
	databases:   RELATIONAL,
	table:       TestTableMedium,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		testUpdateGeneric(b, testDesc, 1, nil)
	},
}

// TestUpdateHeavy updates random row in the 'heavy' table
var TestUpdateHeavy = TestDesc{
	name:        "update-heavy",
	metric:      "rows/sec",
	description: "update random row in the 'heavy' table",
	category:    TestUpdate,
	isReadonly:  false,
	isDBRTest:   false,
	databases:   RELATIONAL,
	table:       TestTableHeavy,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		testUpdateGeneric(b, testDesc, 1, nil)
	},
}

// TestUpdateHeavyDBR updates random row in the 'heavy' table using golang DBR query builder
var TestUpdateHeavyDBR = TestDesc{
	name:        "dbr-update-heavy",
	metric:      "rows/sec",
	description: "update random row in the 'heavy' table using golang DB driver",
	category:    TestUpdate,
	isReadonly:  false,
	isDBRTest:   true,
	databases:   RELATIONAL,
	table:       TestTableHeavy,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		testUpdateGeneric(b, testDesc, 1, nil)
	},
}

// TestUpdateHeavyBulk updates N rows (see --batch=, default 50000) in the 'heavy' table by single transaction
var TestUpdateHeavyBulk = TestDesc{
	name:        "bulkupdate-heavy",
	metric:      "rows/sec",
	description: "update N rows (see --batch=, default 50000) in the 'heavy' table by single transaction",
	category:    TestUpdate,
	isReadonly:  false,
	isDBRTest:   false,
	databases:   RELATIONAL,
	table:       TestTableHeavy,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		origBatch := b.Vault.(*DBTestData).EffectiveBatch
		testBatch := origBatch
		if b.TestOpts.(*TestOpts).BenchOpts.Batch == 0 {
			testBatch = 50000
		}
		b.Vault.(*DBTestData).EffectiveBatch = 1

		testUpdateGeneric(b, testDesc, uint64(testBatch), nil)

		b.Vault.(*DBTestData).EffectiveBatch = origBatch
	},
}

// TestUpdateHeavyBulkDBR updates N rows (see --batch=, default 50000) in the 'heavy' table by single transaction using DBR query builder
var TestUpdateHeavyBulkDBR = TestDesc{
	name:        "dbr-bulkupdate-heavy",
	metric:      "rows/sec",
	description: "update N rows (see --update-rows-count= ) in the 'heavy' table by single transaction using DBR query builder",
	category:    TestUpdate,
	isReadonly:  false,
	isDBRTest:   true,
	databases:   RELATIONAL,
	table:       TestTableHeavy,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		origBatch := b.Vault.(*DBTestData).EffectiveBatch
		b.Vault.(*DBTestData).EffectiveBatch = 1
		testBatch := origBatch
		if b.TestOpts.(*TestOpts).BenchOpts.Batch == 0 {
			testBatch = 50000
		}

		testUpdateGeneric(b, testDesc, uint64(testBatch), nil)

		b.Vault.(*DBTestData).EffectiveBatch = origBatch
	},
}

// TestUpdateHeavySameVal updates random row in the 'heavy' table putting the value which already exists
var TestUpdateHeavySameVal = TestDesc{
	name:        "update-heavy-sameval",
	metric:      "rows/sec",
	description: "update random row in the 'heavy' table putting the value which already exists",
	category:    TestUpdate,
	isReadonly:  false,
	isDBRTest:   false,
	databases:   RELATIONAL,
	table:       TestTableHeavy,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		colConf := testDesc.table.GetColumnsConf([]string{"const_val"}, false)
		testUpdateGeneric(b, testDesc, 1, colConf)
	},
}

// TestUpdateHeavyPartialSameVal updates random row in the 'heavy' table putting two values, where one of them is already exists in this row
var TestUpdateHeavyPartialSameVal = TestDesc{
	name:        "update-heavy-partial-sameval",
	metric:      "rows/sec",
	description: "update random row in the 'heavy' table putting two values, where one of them is already exists in this row",
	category:    TestUpdate,
	isReadonly:  false,
	isDBRTest:   false,
	databases:   RELATIONAL,
	table:       TestTableHeavy,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		colConf := testDesc.table.GetColumnsConf([]string{"const_val", "progress"}, false)
		testUpdateGeneric(b, testDesc, 1, colConf)
	},
}

/*
 * Tenant-specific tests
 */

// TestInsertTenant inserts into the 'tenants' table
var TestInsertTenant = TestDesc{
	name:        "insert-tenant",
	metric:      "tenants/sec",
	description: "insert a tenant into the 'tenants' table",
	category:    TestInsert,
	databases:   ALL,
	table:       TestTableTenants,
	isReadonly:  false,
	isDBRTest:   false,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		testGeneric(b, testDesc, CreateTenantWorker, 0)
	},
}

// TestInsertCTI inserts into the 'cti' table
var TestInsertCTI = TestDesc{
	name:        "insert-cti",
	metric:      "ctiEntity/sec",
	description: "insert a CTI entity into the 'cti' table",
	category:    TestInsert,
	databases:   ALL,
	table:       TestTableCTIEntities,
	isReadonly:  false,
	isDBRTest:   false,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		testGeneric(b, testDesc, CreateCTIEntityWorker, 0)
	},
}

/*
 * Timeseries tests
 */

// TestInsertTimeSeriesSQL inserts into the 'timeseries' SQL table
var TestInsertTimeSeriesSQL = TestDesc{
	name:        "insert-ts-sql",
	metric:      "values/sec",
	description: "batch insert into the 'timeseries' SQL table",
	category:    TestInsert,
	isReadonly:  false,
	isDBRTest:   false,
	databases:   PMWSA,
	table:       TestTableTimeSeriesSQL,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {

		origBatch := b.Vault.(*DBTestData).EffectiveBatch
		if b.TestOpts.(*TestOpts).BenchOpts.Batch == 0 {
			b.Vault.(*DBTestData).EffectiveBatch = 256
		}

		testInsertGeneric(b, testDesc)

		b.Vault.(*DBTestData).EffectiveBatch = origBatch
	},
}

// TestSelectTimeSeriesSQL selects last inserted row from the 'timeseries' SQL table
var TestSelectTimeSeriesSQL = TestDesc{
	name:        "select-ts-sql",
	metric:      "values/sec",
	description: "batch select from the 'timeseries' SQL table",
	category:    TestSelect,
	isReadonly:  false,
	isDBRTest:   false,
	databases:   PMWSA,
	table:       TestTableTimeSeriesSQL,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {

		origBatch := b.Vault.(*DBTestData).EffectiveBatch
		if b.TestOpts.(*TestOpts).BenchOpts.Batch == 0 {
			b.Vault.(*DBTestData).EffectiveBatch = 256
		}

		colConfs := testDesc.table.GetColumnsConf([]string{"tenant_id", "device_id", "metric_id"}, false)

		where := func(worker *benchmark.BenchmarkWorker) string {
			w, err := worker.Randomizer.GenFakeDataAsMap(colConfs, false)
			if err != nil {
				worker.Exit(err)
			}

			return fmt.Sprintf("tenant_id = '%s' AND device_id = '%s' AND metric_id = '%s'", (*w)["tenant_id"], (*w)["device_id"], (*w)["metric_id"])
		}
		orderby := func(worker *benchmark.BenchmarkWorker) string { //nolint:revive
			return "id DESC"
		}

		testSelectRawSQLQuery(b, testDesc, nil, "id", where, orderby, 1)

		b.Vault.(*DBTestData).EffectiveBatch = origBatch
	},
}

/*
 * Advanced monitoring simulation tests
 */

// TestInsertAdvmTasks inserts into the 'adv monitoring tasks' table
var TestInsertAdvmTasks = TestDesc{
	name:        "insert-advmtasks",
	metric:      "rows/sec",
	description: "insert into the 'adv monitoring tasks' table",
	category:    TestInsert,
	isReadonly:  false,
	isDBRTest:   false,
	databases:   []db.DialectName{db.POSTGRES, db.MSSQL},
	table:       TestTableAdvmTasks,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		testInsertGeneric(b, testDesc)
	},
}

// TestSelectAdvmTasksLast selects last inserted row from the 'adv monitoring tasks' table
var TestSelectAdvmTasksLast = TestDesc{
	name:        "select-advmtasks-last",
	metric:      "values/sec",
	description: "get number of rows grouped by week+result_code",
	category:    TestSelect,
	isReadonly:  false,
	isDBRTest:   false,
	databases:   []db.DialectName{db.POSTGRES, db.MSSQL},
	table:       TestTableAdvmTasks,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		var idToRead int64

		var where = func(worker *benchmark.BenchmarkWorker) map[string][]string { //nolint:revive
			return map[string][]string{
				"origin": {"1", "2", "3"},
			}
		}

		var orderBy = func(worker *benchmark.BenchmarkWorker) []string { //nolint:revive
			return []string{"asc(id)"}
		}
		testSelect(b, testDesc, nil, []string{"id"}, []interface{}{&idToRead}, where, orderBy, 1)
	},
}

// TestSelectAdvmTasksCodePerWeek selects number of rows grouped by week+result_code
var TestSelectAdvmTasksCodePerWeek = TestDesc{
	name:        "select-advmtasks-codeperweek",
	metric:      "values/sec",
	description: "get number of rows grouped by week+result_code",
	category:    TestSelect,
	isReadonly:  false,
	isDBRTest:   false,
	databases:   []db.DialectName{db.POSTGRES, db.MSSQL},
	table:       TestTableAdvmTasks,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		// need to implement it
		b.Exit("%s: is not implemented!\n", testDesc.name)
	},
}

// TestInsertAdvmResources inserts into the 'adv monitoring resources' table
var TestInsertAdvmResources = TestDesc{
	name:        "insert-advmresources",
	metric:      "rows/sec",
	description: "insert into the 'adv monitoring resources' table",
	category:    TestInsert,
	isReadonly:  false,
	isDBRTest:   false,
	databases:   []db.DialectName{db.POSTGRES, db.MSSQL},
	table:       TestTableAdvmResources,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		testInsertGeneric(b, testDesc)
	},
}

// TestInsertAdvmResourcesStatuses inserts into the 'adv monitoring resources statuses' table
var TestInsertAdvmResourcesStatuses = TestDesc{
	name:        "insert-advmresourcesstatuses",
	metric:      "rows/sec",
	description: "insert into the 'adv monitoring resources statuses' table",
	category:    TestInsert,
	isReadonly:  false,
	isDBRTest:   false,
	databases:   []db.DialectName{db.POSTGRES, db.MSSQL},
	table:       TestTableAdvmResourcesStatuses,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		testInsertGeneric(b, testDesc)
	},
}

// TestInsertAdvmAgentResources inserts into the 'adv monitoring agent resources' table
var TestInsertAdvmAgentResources = TestDesc{
	name:        "insert-advmagentresources",
	metric:      "rows/sec",
	description: "insert into the 'adv monitoring agent resources' table",
	category:    TestInsert,
	isReadonly:  false,
	isDBRTest:   false,
	databases:   []db.DialectName{db.POSTGRES, db.MSSQL},
	table:       TestTableAdvmAgentsResources,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		testInsertGeneric(b, testDesc)
	},
}

// TestInsertAdvmAgents inserts into the 'adv monitoring agents' table
var TestInsertAdvmAgents = TestDesc{
	name:        "insert-advmagents",
	metric:      "rows/sec",
	description: "insert into the 'adv monitoring agents' table",
	category:    TestInsert,
	isReadonly:  false,
	isDBRTest:   false,
	databases:   []db.DialectName{db.POSTGRES, db.MSSQL},
	table:       TestTableAdvmAgents,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		testInsertGeneric(b, testDesc)
	},
}

// TestInsertAdvmBackupResources inserts into the 'adv monitoring backup resources' table
var TestInsertAdvmBackupResources = TestDesc{
	name:        "insert-advmbackupresources",
	metric:      "rows/sec",
	description: "insert into the 'adv monitoring backup resources' table",
	category:    TestInsert,
	isReadonly:  false,
	isDBRTest:   false,
	databases:   []db.DialectName{db.POSTGRES, db.MSSQL},
	table:       TestTableAdvmBackupResources,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		testInsertGeneric(b, testDesc)
	},
}

// TestInsertAdvmBackups inserts into the 'adv monitoring backups' table
var TestInsertAdvmBackups = TestDesc{
	name:        "insert-advmbackups",
	metric:      "rows/sec",
	description: "insert into the 'adv monitoring backups' table",
	category:    TestInsert,
	isReadonly:  false,
	isDBRTest:   false,
	databases:   []db.DialectName{db.POSTGRES, db.MSSQL},
	table:       TestTableAdvmBackups,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		testInsertGeneric(b, testDesc)
	},
}

// TestInsertAdvmArchives inserts into the 'adv monitoring archives' table
var TestInsertAdvmArchives = TestDesc{
	name:        "insert-advmarchives",
	metric:      "rows/sec",
	description: "insert into the 'adv monitoring archives' table",
	category:    TestInsert,
	isReadonly:  false,
	isDBRTest:   false,
	databases:   []db.DialectName{db.POSTGRES, db.MSSQL},
	table:       TestTableAdvmArchives,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		testInsertGeneric(b, testDesc)
	},
}

// TestInsertAdvmVaults inserts into the 'adv monitoring vaults' table
var TestInsertAdvmVaults = TestDesc{
	name:        "insert-advmvaults",
	metric:      "rows/sec",
	description: "insert into the 'adv monitoring vaults' table",
	category:    TestInsert,
	isReadonly:  false,
	isDBRTest:   false,
	databases:   []db.DialectName{db.POSTGRES, db.MSSQL},
	table:       TestTableAdvmVaults,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		testInsertGeneric(b, testDesc)
	},
}

// TestInsertAdvmDevices inserts into the 'adv monitoring devices' table
var TestInsertAdvmDevices = TestDesc{
	name:        "insert-advmdevices",
	metric:      "rows/sec",
	description: "insert into the 'adv monitoring devices' table",
	category:    TestInsert,
	isReadonly:  false,
	isDBRTest:   false,
	databases:   []db.DialectName{db.POSTGRES, db.MSSQL},
	table:       TestTableAdvmDevices,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		testInsertGeneric(b, testDesc)
	},
}

/*
 * Other
 */

// CreateTenantWorker creates a tenant and optionally inserts an event into the event bus
func CreateTenantWorker(b *benchmark.Benchmark, c *DBConnector, testDesc *TestDesc, batch int) (loops int) { //nolint:revive
	var session = c.database.Session(c.database.Context(context.Background(), false))
	if txErr := session.Transact(func(tx db.DatabaseAccessor) error {
		for i := 0; i < batch; i++ {
			var tenantUUID, err = b.Vault.(*DBTestData).TenantsCache.CreateTenant(b.Randomizer, tx)
			if err != nil {
				return err
			}

			if b.TestOpts.(*TestOpts).BenchOpts.Events {
				if err = b.Vault.(*DBTestData).EventBus.InsertEvent(b.Randomizer, tx, string(tenantUUID)); err != nil {
					return err
				}
			}
		}

		return nil
	}); txErr != nil {
		c.Exit(txErr.Error())
	}

	return batch
}

func CreateCTIEntityWorker(b *benchmark.Benchmark, c *DBConnector, testDesc *TestDesc, batch int) (loops int) { //nolint:revive
	var session = c.database.Session(c.database.Context(context.Background(), false))
	if txErr := session.Transact(func(tx db.DatabaseAccessor) error {
		for i := 0; i < batch; i++ {
			if err := b.Vault.(*DBTestData).TenantsCache.CreateCTIEntity(b.Randomizer, tx); err != nil {
				return err
			}
		}

		return nil
	}); txErr != nil {
		c.Exit(txErr.Error())
	}

	return batch
}

func tenantAwareCTIAwareWorker(b *benchmark.Benchmark, c *DBConnector, testDesc *TestDesc, orderBy string, batch int) (loops int) { //nolint:revive
	c.Logger.Trace("tenant-aware and CTI-aware SELECT test iteration")

	tableName := testDesc.table.TableName
	query := buildTenantAwareQuery(tableName)
	ctiUUID, err := b.Vault.(*DBTestData).TenantsCache.GetRandomCTIUUID(b.Randomizer, 0)
	if err != nil {
		b.Exit(err)
	}
	ctiAwareQuery := query + fmt.Sprintf(
		" JOIN `%[1]s` AS `cti_ent` "+
			"ON `cti_ent`.`uuid` = `%[2]s`.`cti_entity_uuid` AND `%[2]s`.`cti_entity_uuid` IN ('%[4]s') "+
			"LEFT JOIN `%[3]s` as `cti_prov` "+
			"ON `cti_prov`.`tenant_id` = `tenants_child`.`id` AND `cti_prov`.`cti_entity_uuid` = `%[2]s`.`cti_entity_uuid` "+
			"WHERE `cti_prov`.`state` = 1 OR `cti_ent`.`global_state` = 1",
		tenants.TableNameCtiEntities, tableName, tenants.TableNameCtiProvisioning, string(ctiUUID))

	return tenantAwareGenericWorker(b, c, ctiAwareQuery, orderBy)
}

func tenantAwareWorker(b *benchmark.Benchmark, c *DBConnector, testDesc *TestDesc, orderBy string, batch int) (loops int) { //nolint:revive
	query := buildTenantAwareQuery(testDesc.table.TableName)

	return tenantAwareGenericWorker(b, c, query, orderBy)
}

func buildTenantAwareQuery(tableName string) string {
	return fmt.Sprintf("SELECT `%[1]s`.`id` id, `%[1]s`.`tenant_id` FROM `%[1]s` "+
		"JOIN `%[2]s` AS `tenants_child` ON ((`tenants_child`.`uuid` = `%[1]s`.`tenant_id`) AND (`tenants_child`.`is_deleted` != {true})) "+
		"JOIN `%[3]s` AS `tenants_closure` ON ((`tenants_closure`.`child_id` = `tenants_child`.`id`) AND (`tenants_closure`.`barrier` <= 0)) "+
		"JOIN `%[2]s` AS `tenants_parent` ON ((`tenants_parent`.`id` = `tenants_closure`.`parent_id`) "+
		"AND (`tenants_parent`.`uuid` IN ('{tenant_uuid}')) AND (`tenants_parent`.`is_deleted` != {true}))",
		tableName, tenants.TableNameTenants, tenants.TableNameTenantClosure)
}

func tenantAwareGenericWorker(b *benchmark.Benchmark, c *DBConnector, query string, orderBy string) (loops int) {
	c.Logger.Trace("tenant-aware SELECT test iteration")

	uuid, err := b.Vault.(*DBTestData).TenantsCache.GetRandomTenantUUID(b.Randomizer, 0, "")
	if err != nil {
		b.Exit(err)
	}

	var valTrue string

	if c.database.DialectName() == db.POSTGRES {
		valTrue = "true"
	} else {
		valTrue = "1"
	}
	query = strings.ReplaceAll(query, "{true}", valTrue)
	query = strings.ReplaceAll(query, "{tenant_uuid}", string(uuid))
	if orderBy != "" {
		query += " " + orderBy
	}
	query += " LIMIT 1"

	var id, tenantID string

	if c.database.DialectName() == db.POSTGRES {
		query = strings.ReplaceAll(query, "`", "\"")
	}

	c.Logger.Trace("executing query: %s", query)

	var session = c.database.Session(c.database.Context(context.Background(), false))
	if err = session.QueryRow(query).Scan(&id, &tenantID); err != nil {
		if !errors.Is(sql.ErrNoRows, err) {
			c.Exit(err.Error())
		}
	}

	return 1
}

// TestSelectMediumLastTenant is the same as TestSelectMediumLast but with tenant-awareness
var TestSelectMediumLastTenant = TestDesc{
	name:        "select-medium-last-in-tenant",
	metric:      "rows/sec",
	description: "select the last row from the 'medium' table WHERE tenant_id = {random tenant uuid}",
	category:    TestSelect,
	isReadonly:  true,
	isDBRTest:   false,
	databases:   ALL,
	table:       TestTableMedium,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		worker := func(b *benchmark.Benchmark, c *DBConnector, testDesc *TestDesc, batch int) (loops int) { //nolint:revive
			return tenantAwareWorker(b, c, testDesc, "ORDER BY enqueue_time DESC", 1)
		}
		testGeneric(b, testDesc, worker, 1)
	},
}

// TestSelectBlobLastTenant is the same as TestSelectBlobLast but with tenant-awareness
var TestSelectBlobLastTenant = TestDesc{
	name:        "select-blob-last-in-tenant",
	metric:      "rows/sec",
	description: "select the last row from the 'blob' table WHERE tenant_id = {random tenant uuid}",
	category:    TestSelect,
	isReadonly:  true,
	isDBRTest:   false,
	databases:   ALL,
	table:       TestTableBlob,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		worker := func(b *benchmark.Benchmark, c *DBConnector, testDesc *TestDesc, batch int) (loops int) { //nolint:revive
			return tenantAwareWorker(b, c, testDesc, "ORDER BY timestamp DESC", 1)
		}
		testGeneric(b, testDesc, worker, 1)
	},
}

// TestSelectHeavyLastTenant is the same as TestSelectHeavyLast but with tenant-awareness
var TestSelectHeavyLastTenant = TestDesc{
	name:        "select-heavy-last-in-tenant",
	metric:      "rows/sec",
	description: "select the last row from the 'heavy' table WHERE tenant_id = {random tenant uuid}",
	category:    TestSelect,
	isReadonly:  true,
	isDBRTest:   false,
	databases:   RELATIONAL,
	table:       TestTableHeavy,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		worker := func(b *benchmark.Benchmark, c *DBConnector, testDesc *TestDesc, batch int) (loops int) { //nolint:revive
			return tenantAwareWorker(b, c, testDesc, "ORDER BY enqueue_time DESC", 1)
		}
		testGeneric(b, testDesc, worker, 1)
	},
}

// TestSelectHeavyLastTenantCTI is the same as TestSelectHeavyLastTenant but with CTI-awareness
var TestSelectHeavyLastTenantCTI = TestDesc{
	name:        "select-heavy-last-in-tenant-and-cti",
	metric:      "rows/sec",
	description: "select the last row from the 'heavy' table WHERE tenant_id = {} AND cti = {}",
	category:    TestSelect,
	isReadonly:  true,
	isDBRTest:   false,
	databases:   RELATIONAL,
	table:       TestTableHeavy,
	launcherFunc: func(b *benchmark.Benchmark, testDesc *TestDesc) {
		worker := func(b *benchmark.Benchmark, c *DBConnector, testDesc *TestDesc, batch int) (loops int) { //nolint:revive
			return tenantAwareCTIAwareWorker(b, c, testDesc, "ORDER BY enqueue_time DESC", 1)
		}
		testGeneric(b, testDesc, worker, 1)
	},
}

// GetTests returns all tests in the package for execution
func GetTests() ([]*TestGroup, map[string]*TestDesc) {
	allTests = NewTestGroup("all tests")
	var g []*TestGroup

	tg := NewTestGroup("Base tests group")
	g = append(g, tg)

	TestBaseAll.launcherFunc = func(b *benchmark.Benchmark, testDesc *TestDesc) { //nolint:revive
		testOpts, ok := b.TestOpts.(*TestOpts)
		if !ok {
			b.Exit("internal error: can't cast TestOpts struct")
		}
		executeAllTests(b, testOpts)
	}

	tg.add(&TestInsertTenant)
	tg.add(&TestInsertCTI)
	tg.add(&TestInsertLight)
	tg.add(&TestInsertLightPrepared)
	tg.add(&TestInsertLightMultiValue)
	tg.add(&TestCopyLight)
	tg.add(&TestInsertMedium)
	tg.add(&TestInsertMediumPrepared)
	tg.add(&TestInsertMediumMultiValue)
	tg.add(&TestCopyMedium)
	tg.add(&TestInsertHeavy)
	tg.add(&TestInsertHeavyPrepared)
	tg.add(&TestInsertHeavyMultivalue)
	tg.add(&TestCopyHeavy)
	tg.add(&TestUpdateMedium)
	tg.add(&TestUpdateHeavy)
	tg.add(&TestSelectOne)
	tg.add(&TestSelectMediumLast)
	tg.add(&TestSelectMediumRand)
	tg.add(&TestSelectHeavyLast)
	tg.add(&TestSelectHeavyRand)
	tg.add(&TestSelectHeavyMinMaxTenant)
	tg.add(&TestSelectHeavyMinMaxTenantAndState)
	tg.add(&TestSelectHeavyRandPageByUUID)

	tg.add(&TestSelectHeavyRandCustomerRecent)
	tg.add(&TestSelectHeavyRandCustomerRecentLike)
	tg.add(&TestSelectHeavyRandCustomerUpdateTimePage)
	tg.add(&TestSelectHeavyRandCustomerCount)

	tg.add(&TestSelectHeavyRandPartnerRecent)
	tg.add(&TestSelectHeavyRandPartnerStartUpdateTimePage)

	tg.add(&TestBaseAll)

	tg = NewTestGroup("Vector tests group")
	g = append(g, tg)

	tg.add(&TestInsertVector768MultiValue)
	tg.add(&TestSelectVector768NearestL2)
	tg.add(&TestInsertEmailSecurityMultiValue)
	tg.add(&TestSelectEmailByEmbeddingNearestL2)

	tg = NewTestGroup("Advanced tests group")
	g = append(g, tg)

	tg.add(&TestSelectNextVal)
	tg.add(&TestPing)
	tg.add(&TestSelectHeavyForUpdateSkipLocked)
	tg.add(&TestInsertJSON)
	tg.add(&TestSelectJSONByIndexedValue)
	tg.add(&TestSearchJSONByIndexedValue)
	tg.add(&TestSelectJSONByNonIndexedValue)
	tg.add(&TestSearchJSONByNonIndexedValue)
	tg.add(&TestUpdateHeavySameVal)
	tg.add(&TestUpdateHeavyPartialSameVal)
	tg.add(&TestUpdateHeavyBulk)
	tg.add(&TestUpdateHeavyBulkDBR)

	tg = NewTestGroup("Tenant-aware tests")
	g = append(g, tg)

	tg.add(&TestSelectMediumLastTenant)
	tg.add(&TestSelectHeavyLastTenant)
	tg.add(&TestSelectHeavyLastTenantCTI)
	tg.add(&TestSelectHeavyRandTenantLike)

	tg = NewTestGroup("Blob tests")
	g = append(g, tg)

	tg.add(&TestInsertBlob)
	tg.add(&TestCopyBlob)
	tg.add(&TestInsertLargeObj)
	tg.add(&TestSelectBlobLastTenant)

	tg = NewTestGroup("Timeseries tests")
	g = append(g, tg)

	tg.add(&TestInsertTimeSeriesSQL)
	tg.add(&TestSelectTimeSeriesSQL)

	tg = NewTestGroup("Golang DBR query builder tests")
	g = append(g, tg)

	tg.add(&TestInsertLightDBR)
	tg.add(&TestInsertMediumDBR)
	tg.add(&TestInsertHeavyDBR)
	tg.add(&TestInsertJSONDBR)
	tg.add(&TestUpdateMediumDBR)
	tg.add(&TestUpdateHeavyDBR)
	tg.add(&TestSelectMediumLastDBR)
	tg.add(&TestSelectMediumRandDBR)
	tg.add(&TestSelectHeavyLastDBR)
	tg.add(&TestSelectHeavyRandDBR)

	tg = NewTestGroup("Advanced monitoring tests")
	g = append(g, tg)

	tg.add(&TestInsertAdvmTasks)
	tg.add(&TestSelectAdvmTasksLast)
	tg.add(&TestSelectAdvmTasksCodePerWeek)
	tg.add(&TestInsertAdvmResources)
	tg.add(&TestInsertAdvmResourcesStatuses)
	tg.add(&TestInsertAdvmAgentResources)
	tg.add(&TestInsertAdvmAgents)
	tg.add(&TestInsertAdvmBackupResources)
	tg.add(&TestInsertAdvmBackups)
	tg.add(&TestInsertAdvmArchives)
	tg.add(&TestInsertAdvmVaults)
	tg.add(&TestInsertAdvmDevices)

	ret := make(map[string]*TestDesc)

	for _, t := range allTests.tests {
		ret[t.name] = t
	}

	return g, ret
}

func executeAllTests(b *benchmark.Benchmark, testOpts *TestOpts) {
	if testOpts.BenchOpts.Chunk > testOpts.BenchOpts.Limit {
		b.Exit("--chunk option must not be less then --limit")
	}

	if testOpts.BenchOpts.Chunk < MinChunk {
		b.Exit("--chunk option must not be less then %d", MinChunk)
	}

	cleanupTables(b)
	createTables(b)

	workers := b.CommonOpts.Workers
	if workers <= 1 {
		workers = 16
	}

	for i := 0; i < testOpts.BenchOpts.Limit; i += testOpts.BenchOpts.Chunk {
		executeAllTestsOnce(b, testOpts, workers)
	}

	testData := b.Vault.(*DBTestData)

	fmt.Printf("--------------------------------------------------------------------\n")

	scores := []string{TestSelect, TestInsert, TestUpdate}
	for _, s := range scores {
		fmt.Printf("%s geomean: %.0f\n", s, b.Geomean(testData.scores[s]))
	}

	cleanupTables(b)
}

func executeOneTest(b *benchmark.Benchmark, testDesc *TestDesc) {
	// Get current dialect
	var dialectName = getDBDriver(b)

	// Skip if current dialect is not supported by this test
	dialectSupported := false
	for _, supportedDialect := range testDesc.databases {
		if dialectName == supportedDialect {
			dialectSupported = true
			break
		}
	}

	if !dialectSupported {
		// b.Log(benchmark.LogInfo, "Skipping test '%s' - not supported for dialect '%s'", testDesc.name, dialectName)
		return
	}

	testDesc.launcherFunc(b, testDesc)

	// b.Log(benchmark.LogInfo, "Test '%s' completed", testDesc.name)
	select {
	case <-b.ShutdownCh:
		b.Logger.Debug("Gracefully stopping test execution...")
		b.Exit()
	default:
		if b.NeedToExit {
			b.Exit()
		}
	}
}

func executeAllTestsOnce(b *benchmark.Benchmark, testOpts *TestOpts, workers int) {
	b.CommonOpts.Duration = 10
	b.CommonOpts.Workers = 1
	b.CommonOpts.Loops = 0
	executeOneTest(b, &TestSelectOne)

	/* Insert */

	b.CommonOpts.Duration = 0
	b.CommonOpts.Workers = 1
	b.CommonOpts.Loops = 10000
	executeOneTest(b, &TestInsertTenant)

	b.CommonOpts.Duration = 0
	b.CommonOpts.Workers = 1
	b.CommonOpts.Loops = 1000
	executeOneTest(b, &TestInsertCTI)

	//	b.CommonOpts.Duration = 0
	//	b.CommonOpts.Workers = workers
	//	b.CommonOpts.Loops = 30000
	//	executeOneTest(b, &TestInsertTenant)

	b.CommonOpts.Duration = 0
	b.CommonOpts.Workers = 1
	b.CommonOpts.Loops = testOpts.BenchOpts.Chunk / 100 * 5
	executeOneTest(b, &TestInsertLight)
	executeOneTest(b, &TestInsertMedium)
	executeOneTest(b, &TestInsertHeavy)
	executeOneTest(b, &TestInsertJSON)
	executeOneTest(b, &TestInsertTimeSeriesSQL)

	b.CommonOpts.Duration = 0
	b.CommonOpts.Workers = workers
	b.CommonOpts.Loops = testOpts.BenchOpts.Chunk / 100 * 95
	executeOneTest(b, &TestInsertLight)
	executeOneTest(b, &TestInsertMedium)
	executeOneTest(b, &TestInsertJSON)
	executeOneTest(b, &TestInsertTimeSeriesSQL)

	/* Update */

	b.CommonOpts.Duration = 0
	b.CommonOpts.Workers = 1
	b.CommonOpts.Loops = testOpts.BenchOpts.Chunk / 100 * 2
	executeOneTest(b, &TestUpdateMedium)
	executeOneTest(b, &TestUpdateHeavy)
	executeOneTest(b, &TestUpdateHeavyPartialSameVal)
	executeOneTest(b, &TestUpdateHeavySameVal)

	b.CommonOpts.Duration = 0
	b.CommonOpts.Workers = workers
	b.CommonOpts.Loops = testOpts.BenchOpts.Chunk / 100 * 28
	executeOneTest(b, &TestUpdateMedium)
	executeOneTest(b, &TestUpdateHeavy)
	executeOneTest(b, &TestUpdateHeavyPartialSameVal)
	executeOneTest(b, &TestUpdateHeavySameVal)

	/* Select */

	b.CommonOpts.Duration = 10
	b.CommonOpts.Workers = 1
	b.CommonOpts.Loops = 0
	executeOneTest(b, &TestSelectMediumRand)
	executeOneTest(b, &TestSelectHeavyRand)

	b.CommonOpts.Duration = 10
	b.CommonOpts.Workers = workers
	b.CommonOpts.Loops = 0
	executeOneTest(b, &TestSelectMediumRand)
	executeOneTest(b, &TestSelectHeavyRand)

	b.CommonOpts.Duration = 10
	b.CommonOpts.Workers = 1
	b.CommonOpts.Loops = 0
	executeOneTest(b, &TestSelectMediumLast)
	executeOneTest(b, &TestSelectHeavyLast)

	b.CommonOpts.Duration = 10
	b.CommonOpts.Workers = workers
	b.CommonOpts.Loops = 0
	executeOneTest(b, &TestSelectMediumLast)
	executeOneTest(b, &TestSelectHeavyLast)

	/* Other select's */

	b.CommonOpts.Duration = 10
	b.CommonOpts.Workers = 1
	b.CommonOpts.Loops = 0
	executeOneTest(b, &TestSelectHeavyLastTenant)
	executeOneTest(b, &TestSelectHeavyRandTenantLike)
	executeOneTest(b, &TestSelectHeavyLastTenantCTI)
	executeOneTest(b, &TestSelectJSONByIndexedValue)
	executeOneTest(b, &TestSelectJSONByNonIndexedValue)
	executeOneTest(b, &TestSelectTimeSeriesSQL)
	executeOneTest(b, &TestSelectHeavyMinMaxTenant)
	executeOneTest(b, &TestSelectHeavyMinMaxTenantAndState)

	b.CommonOpts.Duration = 10
	b.CommonOpts.Workers = workers
	b.CommonOpts.Loops = 0
	executeOneTest(b, &TestSelectHeavyLastTenant)
	executeOneTest(b, &TestSelectHeavyRandTenantLike)
	executeOneTest(b, &TestSelectHeavyLastTenantCTI)
	executeOneTest(b, &TestSelectJSONByIndexedValue)
	executeOneTest(b, &TestSelectJSONByNonIndexedValue)
	executeOneTest(b, &TestSelectTimeSeriesSQL)
	executeOneTest(b, &TestSelectHeavyMinMaxTenant)
	executeOneTest(b, &TestSelectHeavyMinMaxTenantAndState)
}
