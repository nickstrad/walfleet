// Experiment 7.4 serves a checkpointed 30-step recipe snapshot over HTTP (by
// default) or SeaweedFS S3 (-s3) and queries it through modernc.org/sqlite/vfs
// backed by internal/rangefs. It counts stat and byte-range requests for a
// point lookup, index range scan, and full scan, shows cache eviction under
// PRAGMA cache_size=4, verifies remote results equal the local file, and
// demonstrates why immutable=1 and a fully checkpointed file are both required
// for WAL-format snapshots.
//
// It prints only its verdict. The measurements go to CSV tables that
// report.sql presents: go run ./cmd/course run 7.4. To read from S3 instead,
// run docker compose up -d seaweedfs and then the package itself with -s3:
//
//	go run ./ch07-vfs-and-filesystem-io/experiments/7.4-read-only-snapshot-vfs -s3
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/nickstrad/walfleet/internal/rangefs"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/results"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"modernc.org/sqlite/vfs"
)

const (
	pageSize     = 4096
	headerBytes  = 100
	snapshotName = "snapshot.db"
	rollbackName = "snapshot-rollback.db"
	liveName     = "live.db"
	s3Bucket     = "course"
	s3Prefix     = "ch07/7.4"
)

type query struct {
	name string
	sql  string
}

var queries = []query{
	{"point lookup", "SELECT id, account_id, kind, amount, note FROM events WHERE id = 372"},
	{"index range scan", "SELECT id, amount FROM events WHERE account_id = 7 ORDER BY id"},
	{"full scan", "SELECT count(*), sum(amount), sum(length(note)) FROM events"},
}

// Every measurement goes to a CSV table in <work dir>/results/. report.sql,
// next to this file, presents the tables; "go run ./cmd/course run 7.4" runs both.
var (
	set    *results.Set
	tables struct {
		requests, opens *results.Table
	}
	requestSeq int
)

func fatal(what string, err error) {
	fmt.Printf("FATAL %s: %v\n", what, err)
	os.Exit(1)
}

// logRequests writes the remote requests of one connection (attempt) of a
// part; phase says what the connection was doing when it made them.
func logRequests(part string, attempt int, phase string, requests []rangefs.Request) {
	for _, req := range requests {
		requestSeq++
		tables.requests.Row(requestSeq, part, attempt, phase, string(req.Kind), req.Name, req.Offset, req.Length, req.Found, req.Failed, req.String())
	}
}

type publisher interface {
	publish(ctx context.Context, name string, body []byte) error
	remove(ctx context.Context, name string) error
	source() rangefs.Source
	describe() (route, server, stat, read string)
}

type httpPublisher struct {
	dir    string
	server *httptest.Server
}

func newHTTPPublisher(dir string) (*httpPublisher, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &httpPublisher{dir: dir, server: httptest.NewServer(http.FileServer(http.Dir(dir)))}, nil
}

func (p *httpPublisher) publish(ctx context.Context, name string, body []byte) error {
	return os.WriteFile(filepath.Join(p.dir, name), body, 0o644)
}

func (p *httpPublisher) remove(ctx context.Context, name string) error {
	err := os.Remove(filepath.Join(p.dir, name))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (p *httpPublisher) source() rangefs.Source {
	return rangefs.HTTPSource{BaseURL: p.server.URL, Client: p.server.Client()}
}

func (p *httpPublisher) describe() (route, server, stat, read string) {
	return "http", "an in-process HTTP server (net/http FileServer, which answers Range requests)", "HEAD", "GET with a Range header"
}

type s3Publisher struct {
	client *s3.Client
}

func (p *s3Publisher) publish(ctx context.Context, name string, body []byte) error {
	return rangefs.PutObject(ctx, p.client, s3Bucket, s3Prefix+"/"+name, body)
}

func (p *s3Publisher) remove(ctx context.Context, name string) error {
	return rangefs.DeleteObject(ctx, p.client, s3Bucket, s3Prefix+"/"+name)
}

func (p *s3Publisher) source() rangefs.Source {
	return rangefs.S3Source{Client: p.client, Bucket: s3Bucket, Prefix: s3Prefix}
}

func (p *s3Publisher) describe() (route, server, stat, read string) {
	return "s3", fmt.Sprintf("SeaweedFS from compose.yaml, bucket %q, keys %s/<name>", s3Bucket, s3Prefix), "HeadObject", "GetObject with a Range header"
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "child-writer" {
		childWriter(os.Args[2])
		return
	}
	useS3 := flag.Bool("s3", false, "read the snapshot from the SeaweedFS S3 service instead of the in-process HTTP server")
	flag.Parse()
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("7.4")
	fmt.Printf("INFO: work dir %s\n", sqlitex.DisplayPath(dir))
	set = results.MustOpen(dir)
	tables.requests = set.Table("requests", "seq", "part", "attempt", "phase", "kind", "name", "offset", "length", "found", "failed", "request")
	tables.opens = set.Table("opens", "part", "attempt", "file", "params", "result", "error_code")

	var pub publisher
	if *useS3 {
		cfg := rangefs.S3ConfigFromEnv()
		client := rangefs.NewS3Client(cfg)
		timeout, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := rangefs.EnsureBucket(timeout, client, s3Bucket)
		cancel()
		if err != nil {
			fatal("S3 bucket", fmt.Errorf("%w (start the store with: docker compose up -d seaweedfs)", err))
		}
		fmt.Printf("INFO: S3 endpoint %s\n", cfg.Endpoint)
		set.Table("info_s3", "endpoint").Row(cfg.Endpoint)
		pub = &s3Publisher{client: client}
	} else {
		p, err := newHTTPPublisher(filepath.Join(dir, "published"))
		if err != nil {
			fatal("http", err)
		}
		defer p.server.Close()
		pub = p
	}
	route, server, stat, read := pub.describe()
	set.Table("route", "route", "server", "stat_request", "read_request").Row(route, server, stat, read)
	for _, name := range []string{snapshotName, rollbackName, liveName, liveName + "-wal", liveName + "-journal", snapshotName + "-wal", snapshotName + "-journal"} {
		if err := pub.remove(ctx, name); err != nil {
			fatal("remove "+name, err)
		}
	}
	remote := rangefs.New(ctx, pub.source())
	vfsName, handle, err := vfs.New(remote)
	if err != nil {
		fatal("vfs.New", err)
	}
	defer handle.Close()

	r := recipe.Default()
	local := filepath.Join(dir, snapshotName)
	snapshot := buildSnapshot(ctx, r, local)
	if err := pub.publish(ctx, snapshotName, snapshot); err != nil {
		fatal("publish", err)
	}

	queryTable := set.Table("queries", "query", "name", "sql", "plan", "cache_pages", "local_rows", "local_digest", "local_first_row", "remote_rows", "remote_digest", "remote_first_row")
	for i, q := range queries {
		runQuery(ctx, queryTable, i+1, q, local, vfsName, remote)
	}
	smallCache(ctx, queries[len(queries)-1], vfsName, remote)
	withoutImmutable(ctx, vfsName, remote, snapshotName, local)
	rollbackFormat(ctx, pub, snapshot, vfsName, remote, local)
	staleRead(ctx, r, dir, pub, vfsName, remote)
	set.Finish("READ-ONLY SNAPSHOT VFS")
}

func buildSnapshot(ctx context.Context, r recipe.Recipe, path string) []byte {
	if err := r.Build(ctx, path, r.Steps); err != nil {
		fatal("build snapshot", err)
	}
	match, err := r.MatchStep(ctx, path)
	if err != nil {
		fatal("match", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		fatal("read snapshot", err)
	}
	_, walErr := os.Stat(path + "-wal")
	set.Table("snapshot", "seed", "steps", "journal_mode", "bytes", "pages", "sha256_prefix", "file_state", "header_byte_18", "header_byte_19", "wal_present").
		Row(r.Seed, r.Steps, r.JournalMode, len(body), len(body)/pageSize, shortHash(body), match.String(), body[18], body[19], walErr == nil)
	set.Check("A", body[18] == 2 && body[19] == 2 && os.IsNotExist(walErr) && match.Step == r.Steps && match.Exact, "the snapshot is a WAL-format database with every commit checkpointed into the main file and no -wal left")
	return body
}

func shortHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

func remoteDSN(vfsName, name, params string) string {
	dsn := "file:" + name + "?vfs=" + vfsName
	if params != "" {
		dsn += "&" + params
	}
	return dsn
}

func openSingle(dsn string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func render(ctx context.Context, db *sql.DB, query string) (int, string, string, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return 0, "", "", err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return 0, "", "", err
	}
	var all strings.Builder
	first := ""
	n := 0
	for rows.Next() {
		values := make([]any, len(cols))
		pointers := make([]any, len(cols))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return 0, "", "", err
		}
		parts := make([]string, len(values))
		for i, v := range values {
			switch x := v.(type) {
			case []byte:
				parts[i] = fmt.Sprintf("%q", x)
			default:
				parts[i] = fmt.Sprintf("%v", x)
			}
		}
		line := strings.Join(parts, "|")
		if n == 0 {
			first = line
		}
		all.WriteString(line + "\n")
		n++
	}
	if err := rows.Err(); err != nil {
		return 0, "", "", err
	}
	return n, shortHash([]byte(all.String())), first, nil
}

func localResult(ctx context.Context, path, query string) (int, string, string, []string) {
	db, err := openSingle("file:" + path + "?mode=ro")
	if err != nil {
		fatal("local open", err)
	}
	defer db.Close()
	n, digest, first, err := render(ctx, db, query)
	if err != nil {
		fatal("local query", err)
	}
	rows, err := db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query)
	if err != nil {
		fatal("plan", err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			fatal("plan", err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		fatal("plan", err)
	}
	return n, digest, first, plan
}

// oddReads returns the range requests that are neither one whole page nor the
// 100-byte header.
func oddReads(requests []rangefs.Request) []string {
	var odd []string
	for _, req := range requests {
		if req.Kind != rangefs.KindRange {
			continue
		}
		header := req.Offset == 0 && req.Length == headerBytes
		page := req.Offset%pageSize == 0 && req.Length == pageSize
		if !header && !page {
			odd = append(odd, req.String())
		}
	}
	return odd
}

func runQuery(ctx context.Context, queryTable *results.Table, n int, q query, local, vfsName string, remote *rangefs.FS) {
	localRows, localDigest, localFirst, plan := localResult(ctx, local, q.sql)

	remote.TakeRequests()
	db, err := openSingle(remoteDSN(vfsName, snapshotName, "immutable=1"))
	if err != nil {
		fatal("remote open", err)
	}
	var schemaRows int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema").Scan(&schemaRows); err != nil {
		fatal("remote schema", err)
	}
	cachePages := mustCachePages(ctx, db)
	openRequests := remote.TakeRequests()
	rows, digest, first, err := render(ctx, db, q.sql)
	if err != nil {
		fatal("remote query", err)
	}
	queryRequests := remote.TakeRequests()
	if _, _, _, err := render(ctx, db, q.sql); err != nil {
		fatal("remote repeat", err)
	}
	repeatRequests := remote.TakeRequests()
	if err := db.Close(); err != nil {
		fatal("remote close", err)
	}
	queryTable.Row(n, q.name, q.sql, strings.Join(plan, "; "), cachePages, localRows, localDigest, localFirst, rows, digest, first)
	logRequests("B", n, "open", openRequests)
	logRequests("B", n, "query", queryRequests)
	logRequests("B", n, "repeat", repeatRequests)
	openStats, queryStats, repeatStats := rangefs.Summarize(openRequests), rangefs.Summarize(queryRequests), rangefs.Summarize(repeatRequests)
	odd, oddOpen := oddReads(queryRequests), oddReads(openRequests)
	set.Check("B", rows > 0 && rows == localRows && digest == localDigest, fmt.Sprintf("%s: the remote result equals the local file's (%d row(s), digest %s)", q.name, rows, digest))
	set.Check("B", len(odd) == 0 && len(oddOpen) == 0 && queryStats.StatRequests == 0 && queryStats.RangeRequests > 0, fmt.Sprintf("%s: every read is one range request for one whole %d-byte page (or the %d-byte header): one request per page miss, no read-ahead", q.name, pageSize, headerBytes))
	pagesHeld := openStats.RangeRequests + queryStats.RangeRequests
	set.Check("B", repeatStats.Requests() == 0 && pagesHeld <= cachePages, fmt.Sprintf("%s: repeating the query on the same connection makes no request (immutable=1: no change-counter check, the page cache stays valid; the %d reads so far fit in the %d-page cache)", q.name, pagesHeld, cachePages))
}

// smallCachePages shrinks the page cache below the 18-page events scan to show
// that immutable=1 skips change-counter invalidation but does not prevent cache
// eviction (pager.c:5033-5056,5436; default cache is -2000 KiB, sqliteLimit.h:161).
const smallCachePages = 4

// mustCachePages returns the connection's PRAGMA cache_size expressed in pages.
func mustCachePages(ctx context.Context, db *sql.DB) int {
	var cacheSize int
	if err := db.QueryRowContext(ctx, "PRAGMA cache_size").Scan(&cacheSize); err != nil {
		fatal("cache_size", err)
	}
	if cacheSize < 0 {
		return -cacheSize * 1024 / pageSize
	}
	return cacheSize
}

// smallCache repeats the full events scan with a 4-page cache on one immutable=1
// connection, showing that evicted pages still incur remote range requests.
func smallCache(ctx context.Context, q query, vfsName string, remote *rangefs.FS) {
	db, err := openSingle(remoteDSN(vfsName, snapshotName, "immutable=1"))
	if err != nil {
		fatal("small-cache open", err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, fmt.Sprintf("PRAGMA cache_size=%d", smallCachePages)); err != nil {
		fatal("small-cache pragma", err)
	}
	cachePages := mustCachePages(ctx, db)
	_, firstDigest, _, err := render(ctx, db, q.sql)
	if err != nil {
		fatal("small-cache query", err)
	}
	remote.TakeRequests()
	_, repeatDigest, _, err := render(ctx, db, q.sql)
	if err != nil {
		fatal("small-cache repeat", err)
	}
	repeat := remote.TakeRequests()
	repeatStats := rangefs.Summarize(repeat)
	set.Table("small_cache", "query", "cache_pages", "same_result").Row(q.name, cachePages, firstDigest == repeatDigest)
	logRequests("C", 1, "repeat", repeat)
	set.Check("C", firstDigest == repeatDigest && repeatStats.RangeRequests > cachePages, fmt.Sprintf("%s: when the query reads more pages than the cache holds, the repeat reads evicted pages again (%d range requests); only pages still in the cache are free", q.name, repeatStats.RangeRequests))
}

func errorLine(err error) string {
	if err == nil {
		return "no error"
	}
	if code, ok := sqlitex.ErrorCode(err); ok {
		return fmt.Sprintf("SQLite error code %d: %s", code, err.Error())
	}
	return err.Error()
}

func tryOpen(ctx context.Context, vfsName string, remote *rangefs.FS, name, params string) (string, error, []rangefs.Request) {
	remote.TakeRequests()
	db, err := openSingle(remoteDSN(vfsName, name, params))
	if err != nil {
		return "", err, nil
	}
	defer db.Close()
	var count int
	var version int
	err = db.QueryRowContext(ctx, "SELECT count(*) FROM events").Scan(&count)
	if err == nil {
		err = db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version)
	}
	requests := remote.TakeRequests()
	if err != nil {
		return "", err, requests
	}
	return fmt.Sprintf("user_version=%d events=%d", version, count), nil, requests
}

func localState(ctx context.Context, path string) string {
	db, err := openSingle("file:" + path)
	if err != nil {
		fatal("local", err)
	}
	defer db.Close()
	var count, version int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM events").Scan(&count); err != nil {
		fatal("local count", err)
	}
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		fatal("local version", err)
	}
	return fmt.Sprintf("user_version=%d events=%d", version, count)
}

// openRow writes one remote open of part and its requests.
func openRow(part string, attempt int, name, params, state string, err error, requests []rangefs.Request) {
	var code any
	if c, ok := sqlitex.ErrorCode(err); ok {
		code = c
	}
	if params == "" {
		params = "(no parameters)"
	}
	tables.opens.Row(part, attempt, name, params, fallback(state, err), code)
	logRequests(part, attempt, "open", requests)
}

func withoutImmutable(ctx context.Context, vfsName string, remote *rangefs.FS, name, local string) {
	for i, params := range []string{"", "mode=ro", "immutable=1"} {
		label := params
		if label == "" {
			label = "(no parameters)"
		}
		state, err, requests := tryOpen(ctx, vfsName, remote, name, params)
		openRow("D", i+1, name, params, state, err, requests)
		code, _ := sqlitex.ErrorCode(err)
		switch params {
		case "immutable=1":
			set.Check("D", err == nil && state == localState(ctx, local), "with immutable=1 the WAL-format snapshot opens and reads the same state as the local file")
		default:
			set.Check("D", code == 14, fmt.Sprintf("without immutable=1 (%s) the WAL-format snapshot fails to open with SQLITE_CANTOPEN (14)", label))
		}
	}
}

// rollbackFormat flips database header bytes 18 and 19 (file format write and
// read versions) from 2 (WAL) to 1 (rollback) on a copy of the checkpointed
// snapshot to prove that the header's WAL flag—not the page content—is what
// makes SQLite require shared memory (io_methods version >= 2) when immutable=1
// is omitted.
func rollbackFormat(ctx context.Context, pub publisher, snapshot []byte, vfsName string, remote *rangefs.FS, local string) {
	patched := append([]byte(nil), snapshot...)
	patched[18], patched[19] = 1, 1
	if err := pub.publish(ctx, rollbackName, patched); err != nil {
		fatal("publish", err)
	}
	state, err, requests := tryOpen(ctx, vfsName, remote, rollbackName, "")
	openRow("E", 1, rollbackName, "", state, err, requests)
	set.Check("E", err == nil && state == localState(ctx, local), "a checkpointed snapshot with header bytes 18-19 rewritten to 1 opens without immutable=1 and reads the same state (the WAL flag in the header, not the content, is what demands shared memory)")
	journalChecks := 0
	for _, req := range requests {
		if req.Kind == rangefs.KindStat && strings.HasSuffix(req.Name, "-journal") {
			journalChecks++
		}
	}
	set.Check("E", journalChecks > 0, fmt.Sprintf("without immutable=1 SQLite checks for a hot journal (%d stat request(s) for %s-journal), which immutable=1 skips", journalChecks, rollbackName))
}

func fallback(state string, err error) string {
	if err != nil {
		return errorLine(err)
	}
	return state
}

// childWriter commits the final recipe step in a separate process and holds its
// connection open until stdin closes. Keeping the writer in a child process
// ensures the parent's plain os.ReadFile calls do not close a descriptor to the
// same file inside the writer process and drop its POSIX locks.
func childWriter(path string) {
	ctx := context.Background()
	r := recipe.Default()
	db, err := r.Open(ctx, path)
	if err != nil {
		fatal("child open", err)
	}
	if err := r.ApplyStep(ctx, db, r.Steps); err != nil {
		fatal("child step", err)
	}
	fmt.Println("READY")
	io.Copy(io.Discard, os.Stdin)
	if err := db.Close(); err != nil {
		fatal("child close", err)
	}
}

func staleRead(ctx context.Context, r recipe.Recipe, dir string, pub publisher, vfsName string, remote *rangefs.FS) {
	live := filepath.Join(dir, liveName)
	if err := r.Build(ctx, live, r.Steps-1); err != nil {
		fatal("build live", err)
	}
	command, err := os.Executable()
	if err != nil {
		fatal("executable", err)
	}
	child := exec.CommandContext(ctx, command, "child-writer", live)
	stdin, err := child.StdinPipe()
	if err != nil {
		fatal("pipe", err)
	}
	stdout, err := child.StdoutPipe()
	if err != nil {
		fatal("pipe", err)
	}
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		fatal("child", err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "READY" {
		fatal("child", fmt.Errorf("child said %q: %v", line, err))
	}
	truth := localState(ctx, live)
	dbBytes, err := os.ReadFile(live)
	if err != nil {
		fatal("read live", err)
	}
	walBytes, err := os.ReadFile(live + "-wal")
	if err != nil {
		fatal("read live wal", err)
	}
	liveTable := set.Table("live", "built_to_step", "committed_step", "local_state", "db_bytes", "wal_bytes", "wal_frames", "wal_deleted_after_close")
	liveRow := []any{r.Steps - 1, r.Steps, truth, len(dbBytes), len(walBytes), (len(walBytes) - 32) / (24 + pageSize)}
	if err := pub.publish(ctx, liveName, dbBytes); err != nil {
		fatal("publish", err)
	}
	if err := pub.publish(ctx, liveName+"-wal", walBytes); err != nil {
		fatal("publish", err)
	}
	stale, err, requests := tryOpen(ctx, vfsName, remote, liveName, "immutable=1")
	openRow("F", 1, liveName, "immutable=1", stale, err, requests)
	walRequests := 0
	for _, req := range requests {
		if strings.HasSuffix(req.Name, "-wal") {
			walRequests++
		}
	}
	_, plainErr, plainRequests := tryOpen(ctx, vfsName, remote, liveName, "")
	openRow("F", 2, liveName, "", "", plainErr, plainRequests)
	wantStale := fmt.Sprintf("user_version=%d", r.Steps-1)
	set.Check("F", err == nil && strings.HasPrefix(stale, wantStale+" ") && strings.HasPrefix(truth, fmt.Sprintf("user_version=%d ", r.Steps)) && stale != truth,
		fmt.Sprintf("immutable=1 ignores the -wal: the remote read returns step %d's state while the writer has committed step %d (a stale read, no error)", r.Steps-1, r.Steps))
	set.Check("F", walRequests == 0, "with immutable=1 SQLite never asks for the -wal file (0 requests name it)")
	code, _ := sqlitex.ErrorCode(plainErr)
	set.Check("F", code == 14, "without immutable=1 the live WAL database cannot be opened through the read-only VFS either (SQLITE_CANTOPEN)")

	stdin.Close()
	if err := child.Wait(); err != nil {
		fatal("child exit", err)
	}
	_, walErr := os.Stat(live + "-wal")
	dbBytes, err = os.ReadFile(live)
	if err != nil {
		fatal("read live", err)
	}
	if err := pub.publish(ctx, liveName, dbBytes); err != nil {
		fatal("publish", err)
	}
	if err := pub.remove(ctx, liveName+"-wal"); err != nil {
		fatal("remove", err)
	}
	fresh, err, freshRequests := tryOpen(ctx, vfsName, remote, liveName, "immutable=1")
	openRow("F", 3, liveName, "immutable=1", fresh, err, freshRequests)
	liveTable.Row(append(liveRow, os.IsNotExist(walErr))...)
	set.Check("F", err == nil && fresh == truth && os.IsNotExist(walErr), fmt.Sprintf("once the commit is checkpointed into the main file, the republished snapshot shows step %d", r.Steps))
}
