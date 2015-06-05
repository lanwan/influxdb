package continuous_querier

import (
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/influxdb/influxdb/cluster"
	"github.com/influxdb/influxdb/influxql"
	"github.com/influxdb/influxdb/meta"
)

var (
	expectedErr   = errors.New("expected error")
	unexpectedErr = errors.New("unexpected error")
)

// Test closing never opened, open, open already open, close, and close already closed.
func TestOpenAndClose(t *testing.T) {
	s := NewTestService(t)

	if err := s.Close(); err != nil {
		t.Error(err)
	} else if err = s.Open(); err != nil {
		t.Error(err)
	} else if err = s.Open(); err != nil {
		t.Error(err)
	} else if err = s.Close(); err != nil {
		t.Error(err)
	} else if err = s.Close(); err != nil {
		t.Error(err)
	}
}

// Test ExecuteContinuousQuery happy path.
func TestExecuteContinuousQuery_HappyPath(t *testing.T) {
	s := NewTestService(t)
	dbis, _ := s.MetaStore.Databases()
	dbi := dbis[0]
	cqi := dbi.ContinuousQueries[0]

	pointCnt := 100
	qe := s.QueryExecutor.(*QueryExecutor)
	qe.Results = []*influxql.Result{genResult(1, pointCnt)}

	pw := s.PointsWriter.(*PointsWriter)
	pw.WritePointsFn = func(p *cluster.WritePointsRequest) error {
		if len(p.Points) != pointCnt {
			return fmt.Errorf("exp = %d, got = %d", pointCnt, len(p.Points))
		}
		return nil
	}

	err := s.ExecuteContinuousQuery(&dbi, &cqi)
	if err != nil {
		t.Error(err)
	}
}

// Test the service happy path.
func TestService_HappyPath(t *testing.T) {
	s := NewTestService(t)

	pointCnt := 100
	qe := s.QueryExecutor.(*QueryExecutor)
	qe.Results = []*influxql.Result{genResult(1, pointCnt)}

	done := make(chan struct{}, 5)
	defer close(done)
	pw := s.PointsWriter.(*PointsWriter)
	gotCnt := -1
	pw.WritePointsFn = func(p *cluster.WritePointsRequest) error {
		gotCnt = len(p.Points)
		done <- struct{}{}
		return nil
	}

	s.Open()
	if err := wait(done, time.Second); err != nil {
		t.Error(err)
	} else if gotCnt != pointCnt {
		t.Errorf("exp = %d, got = %d", pointCnt, gotCnt)
	}
	s.Close()
}

// Test service when not the cluster leader (CQs shouldn't run).
func TestService_NotLeader(t *testing.T) {
	s := NewTestService(t)
	// Set RunInterval high so we can test triggering with the RunCh below.
	s.RunInterval = 10 * time.Second
	s.MetaStore.(*MetaStore).Leader = false

	done := make(chan struct{})
	qe := s.QueryExecutor.(*QueryExecutor)
	// Set a callback for ExecuteQuery. Shouldn't get called because we're not the leader.
	qe.ExecuteQueryFn = func(query *influxql.Query, database string, chunkSize int) (<-chan *influxql.Result, error) {
		done <- struct{}{}
		return nil, unexpectedErr
	}

	s.Open()
	// Trigger service to run CQs.
	s.RunCh <- struct{}{}
	// Expect timeout error because ExecuteQuery callback wasn't called.
	if err := wait(done, 100*time.Millisecond); err == nil {
		t.Error(err)
	}
	s.Close()
}

// Test service behavior when meta store fails to get databases.
func TestService_MetaStoreFailsToGetDatabases(t *testing.T) {
	s := NewTestService(t)
	// Set RunInterval high so we can test triggering with the RunCh below.
	s.RunInterval = 10 * time.Second
	s.MetaStore.(*MetaStore).Err = expectedErr

	done := make(chan struct{})
	qe := s.QueryExecutor.(*QueryExecutor)
	// Set ExecuteQuery callback, which shouldn't get called because of meta store failure.
	qe.ExecuteQueryFn = func(query *influxql.Query, database string, chunkSize int) (<-chan *influxql.Result, error) {
		done <- struct{}{}
		return nil, unexpectedErr
	}

	s.Open()
	// Trigger service to run CQs.
	s.RunCh <- struct{}{}
	// Expect timeout error because ExecuteQuery callback wasn't called.
	if err := wait(done, 100*time.Millisecond); err == nil {
		t.Error(err)
	}
	s.Close()
}

// Test ExecuteContinuousQuery with invalid queries.
func TestExecuteContinuousQuery_InvalidQueries(t *testing.T) {
	s := NewTestService(t)
	dbis, _ := s.MetaStore.Databases()
	dbi := dbis[0]
	cqi := dbi.ContinuousQueries[0]

	cqi.Query = `this is not a query`
	err := s.ExecuteContinuousQuery(&dbi, &cqi)
	if err == nil {
		t.Error("expected error but got nil")
	}

	// Valid query but invalid continuous query.
	cqi.Query = `SELECT * FROM cpu`
	err = s.ExecuteContinuousQuery(&dbi, &cqi)
	if err == nil {
		t.Error("expected error but got nil")
	}

	// Group by requires aggregate.
	cqi.Query = `SELECT value INTO other_value FROM cpu WHERE time > now() - 1h GROUP BY time(1s)`
	err = s.ExecuteContinuousQuery(&dbi, &cqi)
	if err == nil {
		t.Error("expected error but got nil")
	}
}

// Test ExecuteContinuousQuery when QueryExecutor returns an error.
func TestExecuteContinuousQuery_QueryExecutor_Error(t *testing.T) {
	s := NewTestService(t)
	qe := s.QueryExecutor.(*QueryExecutor)
	qe.Err = expectedErr

	dbis, _ := s.MetaStore.Databases()
	dbi := dbis[0]
	cqi := dbi.ContinuousQueries[0]

	err := s.ExecuteContinuousQuery(&dbi, &cqi)
	if err != expectedErr {
		t.Errorf("exp = %s, got = %v", expectedErr, err)
	}
}

// NewTestService returns a new *Service with default mock object members.
func NewTestService(t *testing.T) *Service {
	s := NewService(NewConfig())
	s.MetaStore = NewMetaStore(t)
	s.QueryExecutor = NewQueryExecutor(t)
	s.PointsWriter = NewPointsWriter(t)
	s.RunInterval = time.Millisecond

	// Set Logger to write to dev/null so stdout isn't polluted.
	//null, _ := os.Open(os.DevNull)
	s.Logger = log.New(os.Stdout, "", 0)

	return s
}

// MetaStore is a mock meta store.
type MetaStore struct {
	Leader        bool
	DatabaseInfos []meta.DatabaseInfo
	Err           error
	t             *testing.T
}

// NewMetaStore returns a *MetaStore.
func NewMetaStore(t *testing.T) *MetaStore {
	return &MetaStore{
		Leader: true,
		DatabaseInfos: []meta.DatabaseInfo{
			{
				Name: "db",
				DefaultRetentionPolicy: "rp",
				ContinuousQueries: []meta.ContinuousQueryInfo{
					{
						Name:  "cq",
						Query: `SELECT count(cpu) INTO cpu_count FROM cpu WHERE time > now() - 1h GROUP BY time(1s)`,
					},
				},
			},
		},
		t: t,
	}
}

// IsLeader returns true if the node is the cluster leader.
func (ms *MetaStore) IsLeader() bool { return ms.Leader }

// Databases returns a list of database info about each database in the cluster.
func (ms *MetaStore) Databases() ([]meta.DatabaseInfo, error) { return ms.DatabaseInfos, ms.Err }

// QueryExecutor is a mock query executor.
type QueryExecutor struct {
	ExecuteQueryFn      func(query *influxql.Query, database string, chunkSize int) (<-chan *influxql.Result, error)
	Results             []*influxql.Result
	ResultInterval      time.Duration
	Err                 error
	ErrAfterResult      int
	StopRespondingAfter int
	Wg                  *sync.WaitGroup
	t                   *testing.T
}

// NewQueryExecutor returns a *QueryExecutor.
func NewQueryExecutor(t *testing.T) *QueryExecutor {
	return &QueryExecutor{
		ErrAfterResult:      -1,
		StopRespondingAfter: -1,
		t:                   t,
	}
}

// ExecuteQuery returns a channel that the caller can read query results from.
func (qe *QueryExecutor) ExecuteQuery(query *influxql.Query, database string, chunkSize int) (<-chan *influxql.Result, error) {

	// If the test set a callback, call it.
	if qe.ExecuteQueryFn != nil {
		if _, err := qe.ExecuteQueryFn(query, database, chunkSize); err != nil {
			return nil, err
		}
	}

	// Are we supposed to error immediately?
	if qe.ErrAfterResult == -1 && qe.Err != nil {
		return nil, qe.Err
	}

	ch := make(chan *influxql.Result)
	qe.Wg = &sync.WaitGroup{}
	qe.Wg.Add(1)

	// Start a go routine to send results and / or error.
	go func() {
		defer func() { qe.t.Log("ExecuteQuery(): go routine exited"); qe.Wg.Done() }()
		n := 0
		for i, r := range qe.Results {
			if i == qe.ErrAfterResult-1 {
				qe.t.Logf("ExecuteQuery(): ErrAfterResult %d", qe.ErrAfterResult-1)
				ch <- &influxql.Result{Err: qe.Err}
				close(ch)
				return
			} else if i == qe.StopRespondingAfter {
				qe.t.Log("ExecuteQuery(): StopRespondingAfter")
				return
			}
			ch <- r
			n++
			time.Sleep(qe.ResultInterval)
		}
		qe.t.Logf("ExecuteQuery(): all (%d) results sent", n)
		close(ch)
	}()

	return ch, nil
}

// PointsWriter is a mock points writer.
type PointsWriter struct {
	WritePointsFn   func(p *cluster.WritePointsRequest) error
	Err             error
	PointsPerSecond int
	t               *testing.T
}

// NewPointsWriter returns a new *PointsWriter.
func NewPointsWriter(t *testing.T) *PointsWriter {
	return &PointsWriter{
		PointsPerSecond: 25000,
		t:               t,
	}
}

// WritePoints mocks writing points.
func (pw *PointsWriter) WritePoints(p *cluster.WritePointsRequest) error {
	// If the test set a callback, call it.
	if pw.WritePointsFn != nil {
		if err := pw.WritePointsFn(p); err != nil {
			return err
		}
	}

	if pw.Err != nil {
		return pw.Err
	}
	ns := time.Duration((1 / pw.PointsPerSecond) * 1000000000)
	time.Sleep(ns)
	return nil
}

// genResult generates a dummy query result.
func genResult(rowCnt, valCnt int) *influxql.Result {
	rows := make(influxql.Rows, 0, rowCnt)
	now := time.Now()
	for n := 0; n < rowCnt; n++ {
		vals := make([][]interface{}, 0, valCnt)
		for m := 0; m < valCnt; m++ {
			vals = append(vals, []interface{}{now, float64(m)})
			now.Add(time.Second)
		}
		row := &influxql.Row{
			Name:    "cpu",
			Tags:    map[string]string{"host": "server01"},
			Columns: []string{"time", "value"},
			Values:  vals,
		}
		rows = append(rows, row)
	}
	return &influxql.Result{
		Series: rows,
	}
}

func wait(c chan struct{}, d time.Duration) (err error) {
	select {
	case <-c:
	case <-time.After(d):
		err = errors.New("timed out")
	}
	return
}
