// Copyright 2018 The Cockroach Authors.
//
// Licensed as a CockroachDB Enterprise file under the Cockroach Community
// License (the "License"); you may not use this file except in compliance with
// the License. You may obtain a copy of the License at
//
//     https://github.com/cockroachdb/cockroach/blob/master/licenses/CCL.txt

package changefeedccl

import (
	"bufio"
	"bytes"
	"context"
	gosql "database/sql"
	gojson "encoding/json"
	"fmt"
	"io/ioutil"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cockroachdb/apd"
	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/internal/client"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/security"
	"github.com/cockroachdb/cockroach/pkg/server"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/distsqlrun"
	"github.com/cockroachdb/cockroach/pkg/sql/parser"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlbase"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/sqlutils"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/mon"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/cockroach/pkg/workload"
	"github.com/jackc/pgx"
	"github.com/pkg/errors"
)

type benchSink struct {
	syncutil.Mutex
	cond      *sync.Cond
	emits     int
	emitBytes int64
}

func makeBenchSink() *benchSink {
	s := &benchSink{}
	s.cond = sync.NewCond(&s.Mutex)
	return s
}

func (s *benchSink) EmitRow(
	ctx context.Context, _ *sqlbase.TableDescriptor, k, v []byte, _ hlc.Timestamp,
) error {
	return s.emit(int64(len(k) + len(v)))
}
func (s *benchSink) EmitResolvedTimestamp(_ context.Context, e Encoder, ts hlc.Timestamp) error {
	var noTopic string
	p, err := e.EncodeResolvedTimestamp(noTopic, ts)
	if err != nil {
		return err
	}
	return s.emit(int64(len(p)))
}
func (s *benchSink) Flush(_ context.Context) error { return nil }
func (s *benchSink) Close() error                  { return nil }
func (s *benchSink) emit(bytes int64) error {
	s.Lock()
	defer s.Unlock()
	s.emits++
	s.emitBytes += bytes
	s.cond.Broadcast()
	return nil
}

// WaitForEmit blocks until at least one thing is emitted by the sink. It
// returns the number of emitted messages and bytes since the last WaitForEmit.
func (s *benchSink) WaitForEmit() (int, int64) {
	s.Lock()
	defer s.Unlock()
	for s.emits == 0 {
		s.cond.Wait()
	}
	emits, emitBytes := s.emits, s.emitBytes
	s.emits, s.emitBytes = 0, 0
	return emits, emitBytes
}

// createBenchmarkChangefeed starts a stripped down changefeed. It watches
// `database.table` and outputs to `sinkURI`. The given `feedClock` is only used
// for the internal ExportRequest polling, so a benchmark can write data with
// different timestamps beforehand and simulate the changefeed going through
// them in steps.
//
// The returned sink can be used to count emits and the closure handed back
// cancels the changefeed (blocking until it's shut down) and returns an error
// if the changefeed had failed before the closure was called.
//
// This intentionally skips the distsql and sink parts to keep the benchmark
// focused on the core changefeed work, but it does include the poller.
func createBenchmarkChangefeed(
	ctx context.Context,
	s serverutils.TestServerInterface,
	feedClock *hlc.Clock,
	database, table string,
) (*benchSink, func() error) {
	tableDesc := sqlbase.GetTableDescriptor(s.DB(), database, table)
	spans := []roachpb.Span{tableDesc.PrimaryIndexSpan()}
	details := jobspb.ChangefeedDetails{
		Targets: jobspb.ChangefeedTargets{tableDesc.ID: jobspb.ChangefeedTarget{
			StatementTimeName: tableDesc.Name,
		}},
		Opts: map[string]string{
			optEnvelope: string(optEnvelopeRow),
		},
	}
	initialHighWater := hlc.Timestamp{}
	encoder := makeJSONEncoder(details.Opts)
	sink := makeBenchSink()

	settings := s.ClusterSettings()
	metrics := MakeMetrics(server.DefaultHistogramWindowInterval).(*Metrics)
	buf := makeBuffer()
	leaseMgr := s.LeaseManager().(*sql.LeaseManager)
	mm := mon.MakeUnlimitedMonitor(
		context.Background(), "test", mon.MemoryResource,
		nil /* curCount */, nil /* maxHist */, math.MaxInt64, settings,
	)
	poller := makePoller(
		settings, s.DB(), feedClock, s.Gossip(), spans, details, initialHighWater, buf,
		leaseMgr, metrics, &mm,
	)

	th := makeTableHistory(func(context.Context, *sqlbase.TableDescriptor) error { return nil }, initialHighWater)
	thUpdater := &tableHistoryUpdater{
		settings: settings,
		db:       s.DB(),
		targets:  details.Targets,
		m:        th,
	}
	rowsFn := kvsToRows(s.LeaseManager().(*sql.LeaseManager), details, buf.Get)
	tickFn := emitEntries(
		s.ClusterSettings(), details, spans, encoder, sink, rowsFn, TestingKnobs{}, metrics)

	ctx, cancel := context.WithCancel(ctx)
	go func() { _ = poller.Run(ctx) }()
	go func() { _ = thUpdater.PollTableDescs(ctx) }()

	errCh := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := func() error {
			sf := makeSpanFrontier(spans...)
			for {
				// This is basically the ChangeAggregator processor.
				resolvedSpans, err := tickFn(ctx)
				if err != nil {
					return err
				}
				// This is basically the ChangeFrontier processor, the resolved
				// spans are normally sent using distsql, so we're missing a bit
				// of overhead here.
				for _, rs := range resolvedSpans {
					if sf.Forward(rs.Span, rs.Timestamp) {
						frontier := sf.Frontier()
						if err := emitResolvedTimestamp(ctx, encoder, sink, frontier); err != nil {
							return err
						}
					}
				}
			}
		}()
		errCh <- err
	}()
	cancelFn := func() error {
		select {
		case err := <-errCh:
			return err
		default:
		}
		cancel()
		wg.Wait()
		return nil
	}
	return sink, cancelFn
}

// loadWorkloadBatches inserts a workload.Table's row batches, each in one
// transaction. It returns the timestamps of these transactions and the byte
// size for use with b.SetBytes.
func loadWorkloadBatches(sqlDB *gosql.DB, table workload.Table) ([]time.Time, int64, error) {
	if _, err := sqlDB.Exec(`CREATE TABLE "` + table.Name + `" ` + table.Schema); err != nil {
		return nil, 0, err
	}

	var now time.Time
	var timestamps []time.Time
	var benchBytes int64

	var insertStmtBuf bytes.Buffer
	var params []interface{}
	for batchIdx := 0; batchIdx < table.InitialRows.NumBatches; batchIdx++ {
		if _, err := sqlDB.Exec(`BEGIN`); err != nil {
			return nil, 0, err
		}

		params = params[:0]
		insertStmtBuf.Reset()
		insertStmtBuf.WriteString(`INSERT INTO "` + table.Name + `" VALUES `)
		for _, row := range table.InitialRows.Batch(batchIdx) {
			if len(params) != 0 {
				insertStmtBuf.WriteString(`,`)
			}
			insertStmtBuf.WriteString(`(`)
			for colIdx, datum := range row {
				if colIdx != 0 {
					insertStmtBuf.WriteString(`,`)
				}
				benchBytes += workload.ApproxDatumSize(datum)
				params = append(params, datum)
				fmt.Fprintf(&insertStmtBuf, `$%d`, len(params))
			}
			insertStmtBuf.WriteString(`)`)
		}
		if _, err := sqlDB.Exec(insertStmtBuf.String(), params...); err != nil {
			return nil, 0, err
		}

		if err := sqlDB.QueryRow(`SELECT transaction_timestamp(); COMMIT;`).Scan(&now); err != nil {
			return nil, 0, err
		}
		timestamps = append(timestamps, now)
	}

	if table.InitialRows.NumTotal != 0 {
		var totalRows int
		if err := sqlDB.QueryRow(
			`SELECT count(*) FROM "` + table.Name + `"`,
		).Scan(&totalRows); err != nil {
			return nil, 0, err
		}
		if table.InitialRows.NumTotal != totalRows {
			return nil, 0, errors.Errorf(`sanity check failed: expected %d rows got %d`,
				table.InitialRows.NumTotal, totalRows)
		}
	}

	return timestamps, benchBytes, nil
}

type testfeedFactory interface {
	Feed(t testing.TB, create string, args ...interface{}) testfeed
	Server() serverutils.TestServerInterface
}

type testfeed interface {
	Partitions() []string
	Next(t testing.TB) (topic, partition string, key, value, payload []byte, ok bool)
	Err() error
	Close(t testing.TB)
}

type sinklessFeedFactory struct {
	s serverutils.TestServerInterface
}

func makeSinkless(s serverutils.TestServerInterface) *sinklessFeedFactory {
	return &sinklessFeedFactory{s: s}
}

func (f *sinklessFeedFactory) Feed(t testing.TB, create string, args ...interface{}) testfeed {
	t.Helper()
	url, cleanup := sqlutils.PGUrl(t, f.s.ServingAddr(), t.Name(), url.User(security.RootUser))
	q := url.Query()
	q.Add(`results_buffer_size`, `1`)
	url.RawQuery = q.Encode()
	s := &sinklessFeed{cleanup: cleanup, seen: make(map[string]struct{})}
	url.Path = `d`
	// Use pgx directly instead of database/sql so we can close the conn
	// (instead of returning it to the pool).
	pgxConfig, err := pgx.ParseConnectionString(url.String())
	if err != nil {
		t.Fatal(err)
	}
	s.conn, err = pgx.Connect(pgxConfig)
	if err != nil {
		t.Fatal(err)
	}

	// The syntax for a sinkless changefeed is `EXPERIMENTAL CHANGEFEED FOR ...`
	// but it's convenient to accept the `CREATE CHANGEFEED` syntax from the
	// test, so we can keep the current abstraction of running each test over
	// both types. This bit turns what we received into the real sinkless
	// syntax.
	create = strings.Replace(create, `CREATE CHANGEFEED`, `EXPERIMENTAL CHANGEFEED`, 1)

	s.rows, err = s.conn.Query(create, args...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (f *sinklessFeedFactory) Server() serverutils.TestServerInterface {
	return f.s
}

// sinklessFeed is an implementation of the `testfeed` interface for a
// "sinkless" (results returned over pgwire) feed.
type sinklessFeed struct {
	conn    *pgx.Conn
	cleanup func()
	rows    *pgx.Rows
	seen    map[string]struct{}
}

func (c *sinklessFeed) Partitions() []string { return []string{`sinkless`} }

func (c *sinklessFeed) Next(
	t testing.TB,
) (topic, partition string, key, value, resolved []byte, ok bool) {
	t.Helper()
	partition = `sinkless`
	var noKey, noValue, noResolved []byte
	for {
		if !c.rows.Next() {
			return ``, ``, nil, nil, nil, false
		}
		var maybeTopic gosql.NullString
		if err := c.rows.Scan(&maybeTopic, &key, &value); err != nil {
			t.Fatal(err)
		}
		if len(maybeTopic.String) > 0 {
			// TODO(dan): This skips duplicates, since they're allowed by the
			// semantics of our changefeeds. Now that we're switching to
			// RangeFeed, this can actually happen (usually because of splits)
			// and cause flakes. However, we really should be de-deuping key+ts,
			// this is too coarse. Fixme.
			seenKey := maybeTopic.String + partition + string(key) + string(value)
			if _, ok := c.seen[seenKey]; ok {
				continue
			}
			c.seen[seenKey] = struct{}{}
			return maybeTopic.String, partition, key, value, noResolved, true
		}
		resolvedPayload := value
		return ``, partition, noKey, noValue, resolvedPayload, true
	}
}

func (c *sinklessFeed) Err() error {
	if c.rows != nil {
		return c.rows.Err()
	}
	return nil
}

func (c *sinklessFeed) Close(t testing.TB) {
	t.Helper()
	if err := c.conn.Close(); err != nil {
		t.Error(err)
	}
	c.cleanup()
}

type jobFeed struct {
	db      *gosql.DB
	flushCh chan struct{}

	jobID  int64
	jobErr error
}

func (f *jobFeed) fetchJobError() error {
	// To avoid busy waiting, we wait for the AfterFlushHook (which is called
	// after results are flushed to a sink) in between polls. It is required
	// that this is hooked up to `flushCh`, which is usually handled by the
	// `enterpriseTest` helper.
	//
	// The trickiest bit is handling errors in the changefeed. The tests want to
	// eventually notice them, but want to return all generated results before
	// giving up and returning the error. This is accomplished by checking the
	// job error immediately before every poll. If it's set, the error is
	// stashed and one more poll's result set is paged through, before finally
	// returning the error. If we're careful to run the last poll after getting
	// the error, then it's guaranteed to contain everything flushed by the
	// changefeed before it shut down.
	if f.jobErr != nil {
		return f.jobErr
	}

	// We're not guaranteed to get a flush notification if the feed exits,
	// so bound how long we wait.
	select {
	case <-f.flushCh:
	case <-time.After(30 * time.Millisecond):
	}

	// If the error was set, save it, but do one more poll as described
	// above.
	var errorStr gosql.NullString
	if err := f.db.QueryRow(
		`SELECT error FROM [SHOW JOBS] WHERE job_id=$1`, f.jobID,
	).Scan(&errorStr); err != nil {
		return err
	}
	if len(errorStr.String) > 0 {
		f.jobErr = errors.New(errorStr.String)
	}
	return nil
}

type tableFeedFactory struct {
	s       serverutils.TestServerInterface
	db      *gosql.DB
	flushCh chan struct{}
}

func makeTable(
	s serverutils.TestServerInterface, db *gosql.DB, flushCh chan struct{},
) *tableFeedFactory {
	return &tableFeedFactory{s: s, db: db, flushCh: flushCh}
}

func (f *tableFeedFactory) Feed(t testing.TB, create string, args ...interface{}) testfeed {
	t.Helper()

	sink, cleanup := sqlutils.PGUrl(t, f.s.ServingAddr(), t.Name(), url.User(security.RootUser))
	sink.Path = fmt.Sprintf(`table_%d`, timeutil.Now().UnixNano())

	db, err := gosql.Open("postgres", sink.String())
	if err != nil {
		t.Fatal(err)
	}

	sink.Scheme = sinkSchemeExperimentalSQL
	c := &tableFeed{
		jobFeed: jobFeed{
			db:      db,
			flushCh: f.flushCh,
		},
		urlCleanup: cleanup,
		sinkURI:    sink.String(),
		seen:       make(map[string]struct{}),
	}
	if _, err := c.db.Exec(`CREATE DATABASE ` + sink.Path); err != nil {
		t.Fatal(err)
	}

	parsed, err := parser.ParseOne(create)
	if err != nil {
		t.Fatal(err)
	}
	createStmt := parsed.AST.(*tree.CreateChangefeed)
	if createStmt.SinkURI != nil {
		t.Fatalf(`unexpected sink provided: "INTO %s"`, tree.AsString(createStmt.SinkURI))
	}
	createStmt.SinkURI = tree.NewStrVal(c.sinkURI)

	if err := f.db.QueryRow(createStmt.String(), args...).Scan(&c.jobID); err != nil {
		t.Fatal(err)
	}
	return c
}

func (f *tableFeedFactory) Server() serverutils.TestServerInterface {
	return f.s
}

type tableFeed struct {
	jobFeed
	sinkURI    string
	urlCleanup func()

	rows *gosql.Rows
	seen map[string]struct{}
}

func (c *tableFeed) Partitions() []string {
	// The sqlSink hardcodes these.
	return []string{`0`, `1`, `2`}
}

func (c *tableFeed) Next(
	t testing.TB,
) (topic, partition string, key, value, payload []byte, ok bool) {
	// sinkSink writes all changes to a table with primary key of topic,
	// partition, message_id. To simulate the semantics of kafka, message_ids
	// are only comparable within a given (topic, partition). Internally the
	// message ids are generated as a 64 bit int with a timestamp in bits 1-49
	// and a hash of the partition in 50-64. This tableFeed.Next function works
	// by repeatedly fetching and deleting all rows in the table. Then it pages
	// through the results until they are empty and repeats.
	for {
		if c.rows != nil && c.rows.Next() {
			var msgID int64
			if err := c.rows.Scan(&topic, &partition, &msgID, &key, &value, &payload); err != nil {
				t.Fatal(err)
			}

			// Scan turns NULL bytes columns into a 0-length, non-nil byte
			// array, which is pretty unexpected. Nil them out before returning.
			// Either key+value or payload will be set, but not both.
			if len(key) > 0 || len(value) > 0 {
				// TODO(dan): This skips duplicates, since they're allowed by
				// the semantics of our changefeeds. Now that we're switching to
				// RangeFeed, this can actually happen (usually because of
				// splits) and cause flakes. However, we really should be
				// de-deuping key+ts, this is too coarse. Fixme.
				seenKey := topic + partition + string(key) + string(value)
				if _, ok := c.seen[seenKey]; ok {
					continue
				}
				c.seen[seenKey] = struct{}{}

				payload = nil
			} else {
				key, value = nil, nil
			}
			return topic, partition, key, value, payload, true
		}
		if c.rows != nil {
			if err := c.rows.Close(); err != nil {
				t.Fatal(err)
			}
			c.rows = nil
		}

		if err := c.fetchJobError(); err != nil {
			return ``, ``, nil, nil, nil, false
		}

		// TODO(dan): It's a bummer that this mutates the sqlsink table. I
		// originally tried paging through message_id by repeatedly generating a
		// new high-water with GenerateUniqueInt, but this was racy with rows
		// being flushed out by the sink. An alternative is to steal the nanos
		// part from `high_water_timestamp` in `crdb_internal.jobs` and run it
		// through `builtins.GenerateUniqueID`, but that would mean we're only
		// ever running tests on rows that have gotten a resolved timestamp,
		// which seems limiting.
		var err error
		c.rows, err = c.db.Query(
			`SELECT * FROM [DELETE FROM sqlsink RETURNING *] ORDER BY topic, partition, message_id`)
		if err != nil {
			t.Fatal(err)
		}
	}
}

func (c *tableFeed) Err() error {
	return c.jobErr
}

func (c *tableFeed) Close(t testing.TB) {
	if c.rows != nil {
		if err := c.rows.Close(); err != nil {
			t.Errorf(`could not close rows: %v`, err)
		}
	}
	if _, err := c.db.Exec(`CANCEL JOB $1`, c.jobID); err != nil {
		log.Infof(context.Background(), `could not cancel feed %d: %v`, c.jobID, err)
	}
	if err := c.db.Close(); err != nil {
		t.Error(err)
	}
	c.urlCleanup()
}

var cloudFeedFileRE = regexp.MustCompile(`^\d{33}-(.+?)-(\d+)-`)

type cloudFeedFactory struct {
	s       serverutils.TestServerInterface
	db      *gosql.DB
	dir     string
	flushCh chan struct{}

	feedIdx int
}

func makeCloud(
	s serverutils.TestServerInterface, db *gosql.DB, dir string, flushCh chan struct{},
) *cloudFeedFactory {
	return &cloudFeedFactory{s: s, db: db, dir: dir, flushCh: flushCh}
}

func (f *cloudFeedFactory) Feed(t testing.TB, create string, args ...interface{}) testfeed {
	t.Helper()

	parsed, err := parser.ParseOne(create)
	if err != nil {
		t.Fatal(err)
	}
	createStmt := parsed.AST.(*tree.CreateChangefeed)
	if createStmt.SinkURI != nil {
		t.Fatalf(`unexpected sink provided: "INTO %s"`, tree.AsString(createStmt.SinkURI))
	}
	feedDir := strconv.Itoa(f.feedIdx)
	f.feedIdx++
	sinkURI := `experimental-nodelocal:///` + feedDir
	// TODO(dan): This is a pretty unsatisfying way to test that the sink passes
	// through params it doesn't understand to ExportStorage.
	sinkURI += `?should_be=ignored`
	createStmt.SinkURI = tree.NewStrVal(sinkURI)

	// Nodelocal puts its dir under `ExternalIODir`, which is passed into
	// cloudFeedFactory.
	feedDir = filepath.Join(f.dir, feedDir)
	if err := os.Mkdir(feedDir, 0755); err != nil {
		t.Fatal(err)
	}

	c := &cloudFeed{
		jobFeed: jobFeed{
			db:      f.db,
			flushCh: f.flushCh,
		},
		dir:  feedDir,
		seen: make(map[string]struct{}),
	}
	if err := f.db.QueryRow(createStmt.String(), args...).Scan(&c.jobID); err != nil {
		t.Fatal(err)
	}
	return c
}

func (f *cloudFeedFactory) Server() serverutils.TestServerInterface {
	return f.s
}

type cloudFeedEntry struct {
	topic          string
	value, payload []byte
}

type cloudFeed struct {
	jobFeed
	dir string

	resolved string
	rows     []cloudFeedEntry

	seen map[string]struct{}
}

const cloudFeedPartition = ``

func (c *cloudFeed) Partitions() []string {
	// TODO(dan): Try to plumb these through somehow?
	return []string{cloudFeedPartition}
}

func (c *cloudFeed) Next(
	t testing.TB,
) (topic, partition string, key, value, payload []byte, ok bool) {
	for {
		if len(c.rows) > 0 {
			e := c.rows[0]
			c.rows = c.rows[1:]
			topic, key, value, payload = e.topic, nil, e.value, e.payload

			if len(value) > 0 {
				seen := topic + string(value)
				if _, ok := c.seen[seen]; ok {
					continue
				}
				c.seen[seen] = struct{}{}
				payload = nil
				return topic, cloudFeedPartition, key, value, payload, true
			}
			key, value = nil, nil
			return topic, cloudFeedPartition, key, value, payload, true
		}

		if err := c.fetchJobError(); err != nil {
			return ``, ``, nil, nil, nil, false
		}
		if err := filepath.Walk(c.dir, c.walkDir); err != nil {
			t.Fatal(err)
		}
	}
}

func (c *cloudFeed) walkDir(path string, info os.FileInfo, _ error) error {
	if info.IsDir() {
		// Nothing to do for directories.
		return nil
	}

	var rows []cloudFeedEntry
	if strings.Compare(c.resolved, path) >= 0 {
		// Already output this in a previous walkDir.
		return nil
	}
	if strings.HasSuffix(path, `RESOLVED`) {
		c.rows = append(c.rows, rows...)
		resolvedPayload, err := ioutil.ReadFile(path)
		if err != nil {
			return err
		}
		resolvedEntry := cloudFeedEntry{payload: resolvedPayload}
		c.rows = append(c.rows, resolvedEntry)
		c.resolved = path
		return nil
	}

	var topic string
	subs := cloudFeedFileRE.FindStringSubmatch(filepath.Base(path))
	if subs == nil {
		return errors.Errorf(`unexpected file: %s`, path)
	}
	topic = subs[1]

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	// NB: This is the logic for JSON. Avro will involve parsing an
	// "Object Container File".
	s := bufio.NewScanner(f)
	for s.Scan() {
		c.rows = append(c.rows, cloudFeedEntry{
			topic: topic,
			value: s.Bytes(),
		})
	}
	return nil
}

func (c *cloudFeed) Err() error {
	return c.jobErr
}

func (c *cloudFeed) Close(t testing.TB) {
	if _, err := c.db.Exec(`CANCEL JOB $1`, c.jobID); err != nil {
		log.Infof(context.Background(), `could not cancel feed %d: %v`, c.jobID, err)
	}
	if err := c.db.Close(); err != nil {
		t.Error(err)
	}
}

func waitForSchemaChange(
	t testing.TB, sqlDB *sqlutils.SQLRunner, stmt string, arguments ...interface{},
) {
	sqlDB.Exec(t, stmt, arguments...)
	row := sqlDB.QueryRow(t, "SELECT job_id FROM [SHOW JOBS] ORDER BY created DESC LIMIT 1")
	var jobID string
	row.Scan(&jobID)

	testutils.SucceedsSoon(t, func() error {
		row := sqlDB.QueryRow(t, "SELECT status FROM [SHOW JOBS] WHERE job_id = $1", jobID)
		var status string
		row.Scan(&status)
		if status != "succeeded" {
			return fmt.Errorf("Job %s had status %s, wanted 'succeeded'", jobID, status)
		}
		return nil
	})
}

func assertPayloads(t testing.TB, f testfeed, expected []string) {
	t.Helper()

	var actual []string
	for len(actual) < len(expected) {
		topic, _, key, value, _, ok := f.Next(t)
		if log.V(1) {
			log.Infof(context.TODO(), `%v %s: %s->%s`, ok, topic, key, value)
		}
		if !ok {
			t.Fatalf(`expected another row: %s`, f.Err())
		} else if key != nil || value != nil {
			actual = append(actual, fmt.Sprintf(`%s: %s->%s`, topic, key, value))
		}
	}

	// The tests that use this aren't concerned with order, just that these are
	// the next len(expected) messages.
	sort.Strings(expected)
	sort.Strings(actual)
	if !reflect.DeepEqual(expected, actual) {
		t.Fatalf("expected\n  %s\ngot\n  %s",
			strings.Join(expected, "\n  "), strings.Join(actual, "\n  "))
	}
}

func avroToJSON(t testing.TB, reg *testSchemaRegistry, avroBytes []byte) []byte {
	if len(avroBytes) == 0 {
		return nil
	}
	native, err := reg.encodedAvroToNative(avroBytes)
	if err != nil {
		t.Fatal(err)
	}
	// The avro textual format is a more natural fit, but it's non-deterministic
	// because of go's randomized map ordering. Instead, we use gojson.Marshal,
	// which sorts its object keys and so is deterministic.
	json, err := gojson.Marshal(native)
	if err != nil {
		t.Fatal(err)
	}
	return json
}

func assertPayloadsAvro(t testing.TB, reg *testSchemaRegistry, f testfeed, expected []string) {
	t.Helper()

	var actual []string
	for len(actual) < len(expected) {
		topic, _, keyBytes, valueBytes, _, ok := f.Next(t)
		if !ok {
			break
		} else if keyBytes != nil || valueBytes != nil {
			key, value := avroToJSON(t, reg, keyBytes), avroToJSON(t, reg, valueBytes)
			actual = append(actual, fmt.Sprintf(`%s: %s->%s`, topic, key, value))
		}
	}

	// The tests that use this aren't concerned with order, just that these are
	// the next len(expected) messages.
	sort.Strings(expected)
	sort.Strings(actual)
	if !reflect.DeepEqual(expected, actual) {
		t.Fatalf("expected\n  %s\ngot\n  %s",
			strings.Join(expected, "\n  "), strings.Join(actual, "\n  "))
	}
}

func skipResolvedTimestamps(t *testing.T, f testfeed) {
	for {
		table, _, key, value, _, ok := f.Next(t)
		if !ok {
			break
		}
		if key != nil || value != nil {
			t.Errorf(`unexpected row %s: %s->%s`, table, key, value)
		}
	}
}

func parseTimeToHLC(t testing.TB, s string) hlc.Timestamp {
	t.Helper()
	d, _, err := apd.NewFromString(s)
	if err != nil {
		t.Fatal(err)
	}
	ts, err := tree.DecimalToHLC(d)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

func expectResolvedTimestamp(t testing.TB, f testfeed) hlc.Timestamp {
	t.Helper()
	topic, _, key, value, resolved, _ := f.Next(t)
	if key != nil || value != nil {
		t.Fatalf(`unexpected row %s: %s -> %s`, topic, key, value)
	}
	if resolved == nil {
		t.Fatal(`expected a resolved timestamp notification`)
	}

	var valueRaw struct {
		Resolved string `json:"resolved"`
	}
	if err := gojson.Unmarshal(resolved, &valueRaw); err != nil {
		t.Fatal(err)
	}

	return parseTimeToHLC(t, valueRaw.Resolved)
}

func expectResolvedTimestampAvro(t testing.TB, reg *testSchemaRegistry, f testfeed) hlc.Timestamp {
	t.Helper()
	topic, _, keyBytes, valueBytes, resolvedBytes, _ := f.Next(t)
	if keyBytes != nil || valueBytes != nil {
		key, value := avroToJSON(t, reg, keyBytes), avroToJSON(t, reg, valueBytes)
		t.Fatalf(`unexpected row %s: %s -> %s`, topic, key, value)
	}
	if resolvedBytes == nil {
		t.Fatal(`expected a resolved timestamp notification`)
	}
	resolvedNative, err := reg.encodedAvroToNative(resolvedBytes)
	if err != nil {
		t.Fatal(err)
	}
	resolved := resolvedNative.(map[string]interface{})[`resolved`]
	return parseTimeToHLC(t, resolved.(map[string]interface{})[`string`].(string))
}

func sinklessTest(testFn func(*testing.T, *gosql.DB, testfeedFactory)) func(*testing.T) {
	return func(t *testing.T) {
		ctx := context.Background()
		knobs := base.TestingKnobs{DistSQL: &distsqlrun.TestingKnobs{Changefeed: &TestingKnobs{}}}
		s, db, _ := serverutils.StartServer(t, base.TestServerArgs{
			Knobs:       knobs,
			UseDatabase: `d`,
		})
		defer s.Stopper().Stop(ctx)
		sqlDB := sqlutils.MakeSQLRunner(db)
		sqlDB.Exec(t, `SET CLUSTER SETTING kv.rangefeed.enabled = true`)
		// TODO(dan): We currently have to set this to an extremely conservative
		// value because otherwise schema changes become flaky (they don't commit
		// their txn in time, get pushed by closed timestamps, and retry forever).
		// This is more likely when the tests run slower (race builds or inside
		// docker). The conservative value makes our tests take a lot longer,
		// though. Figure out some way to speed this up.
		sqlDB.Exec(t, `SET CLUSTER SETTING kv.closed_timestamp.target_duration = '1s'`)
		// TODO(dan): This is still needed to speed up table_history, that should be
		// moved to RangeFeed as well.
		sqlDB.Exec(t, `SET CLUSTER SETTING changefeed.experimental_poll_interval = '10ms'`)
		sqlDB.Exec(t, `CREATE DATABASE d`)

		f := makeSinkless(s)
		testFn(t, db, f)
	}
}

func enterpriseTest(testFn func(*testing.T, *gosql.DB, testfeedFactory)) func(*testing.T) {
	return func(t *testing.T) {
		ctx := context.Background()

		flushCh := make(chan struct{}, 1)
		defer close(flushCh)
		knobs := base.TestingKnobs{DistSQL: &distsqlrun.TestingKnobs{Changefeed: &TestingKnobs{
			AfterSinkFlush: func() error {
				select {
				case flushCh <- struct{}{}:
				default:
				}
				return nil
			},
		}}}

		s, db, _ := serverutils.StartServer(t, base.TestServerArgs{
			UseDatabase: "d",
			Knobs:       knobs,
		})
		defer s.Stopper().Stop(ctx)
		sqlDB := sqlutils.MakeSQLRunner(db)
		// TODO(dan): Switch this to RangeFeed, too. It seems wasteful right now
		// because the RangeFeed version of the tests take longer due to
		// closed_timestamp.target_duration's interaction with schema changes.
		sqlDB.Exec(t, `SET CLUSTER SETTING changefeed.push.enabled = false`)
		sqlDB.Exec(t, `SET CLUSTER SETTING changefeed.experimental_poll_interval = '10ms'`)
		sqlDB.Exec(t, `CREATE DATABASE d`)
		f := makeTable(s, db, flushCh)

		testFn(t, db, f)
	}
}

func pollerTest(
	metaTestFn func(func(*testing.T, *gosql.DB, testfeedFactory)) func(*testing.T),
	testFn func(*testing.T, *gosql.DB, testfeedFactory),
) func(*testing.T) {
	return func(t *testing.T) {
		metaTestFn(func(t *testing.T, db *gosql.DB, f testfeedFactory) {
			sqlDB := sqlutils.MakeSQLRunner(db)
			sqlDB.Exec(t, `SET CLUSTER SETTING changefeed.push.enabled = false`)
			sqlDB.Exec(t, `SET CLUSTER SETTING changefeed.experimental_poll_interval = '10ms'`)
			testFn(t, db, f)
		})(t)
	}
}

func forceTableGC(
	t testing.TB,
	tsi serverutils.TestServerInterface,
	sqlDB *sqlutils.SQLRunner,
	database, table string,
) {
	t.Helper()
	tblID := sqlutils.QueryTableID(t, sqlDB.DB, database, table)

	tblKey := roachpb.Key(keys.MakeTablePrefix(tblID))
	gcr := roachpb.GCRequest{
		RequestHeader: roachpb.RequestHeader{
			Key:    tblKey,
			EndKey: tblKey.PrefixEnd(),
		},
		Threshold: tsi.Clock().Now(),
	}
	if _, err := client.SendWrapped(context.Background(), tsi.DistSender(), &gcr); err != nil {
		t.Fatal(err)
	}
}
