package e2e

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/abhishek-rnjn/evals.si/internal/config"
	"github.com/abhishek-rnjn/evals.si/internal/objstore"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

// TestFlywheelOnExternalStorage runs the flywheel (OTLP traces, shadow
// replay, promotion, a run on the promoted dataset) with the Kubernetes
// storage: PostgreSQL, traces in ClickHouse, and datasets_dir in an S3
// bucket. It needs EVALSI_E2E_POSTGRES_DSN, EVALSI_E2E_CLICKHOUSE_URL and
// EVALSI_E2E_S3_ENDPOINT (with _ACCESS_KEY and _SECRET_KEY).
func TestFlywheelOnExternalStorage(t *testing.T) {
	dsn, chURL, s3 := os.Getenv("EVALSI_E2E_POSTGRES_DSN"), os.Getenv("EVALSI_E2E_CLICKHOUSE_URL"), os.Getenv("EVALSI_E2E_S3_ENDPOINT")
	if dsn == "" || chURL == "" || s3 == "" {
		t.Skip("set EVALSI_E2E_POSTGRES_DSN, EVALSI_E2E_CLICKHOUSE_URL and EVALSI_E2E_S3_ENDPOINT")
	}
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())

	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "e2e_" + suffix
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.ExecContext(ctx, "DROP SCHEMA "+schema+" CASCADE") })
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}

	t.Setenv("E2E_S3_AK", os.Getenv("EVALSI_E2E_S3_ACCESS_KEY"))
	t.Setenv("E2E_S3_SK", os.Getenv("EVALSI_E2E_S3_SECRET_KEY"))
	mc, err := minio.New(s3, &minio.Options{Creds: credentials.NewStaticV4(os.Getenv("E2E_S3_AK"), os.Getenv("E2E_S3_SK"), "")})
	if err != nil {
		t.Fatal(err)
	}
	bucket := "evalsi-e2e-" + suffix
	if err := mc.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for o := range mc.ListObjects(ctx, bucket, minio.ListObjectsOptions{Recursive: true}) {
			_ = mc.RemoveObject(ctx, bucket, o.Key, minio.RemoveObjectOptions{})
		}
		_ = mc.RemoveBucket(ctx, bucket)
	})

	chDB := "evalsi_e2e_" + suffix
	e := start(t, func(c *config.Config) {
		c.Storage = config.Storage{
			Postgres:   &config.Postgres{DSN: dsn + sep + "search_path=" + schema},
			ClickHouse: &store.ClickHouseConfig{URL: chURL, Database: chDB},
			S3:         &objstore.Config{Endpoint: s3, Insecure: true, PathStyle: true, AccessKeyEnv: "E2E_S3_AK", SecretKeyEnv: "E2E_S3_SK"},
		}
		c.DatasetsDir = "s3://" + bucket + "/datasets"
	})
	flywheel(t, e)

	// Everything landed in the external stores, none of it in data_dir.
	var runs int
	if err := admin.QueryRowContext(ctx, "SELECT count(*) FROM "+schema+".runs").Scan(&runs); err != nil || runs < 3 {
		t.Errorf("runs in postgres: %d %v", runs, err)
	}
	if _, err := mc.StatObject(ctx, bucket, "datasets/promoted/default/rome-misses.jsonl", minio.StatObjectOptions{}); err != nil {
		t.Errorf("the promoted dataset is not in the bucket: %v", err)
	}
	if _, err := os.Stat(e.dataDir + "/evalsi.db"); err == nil {
		t.Error("a SQLite database was created in data_dir")
	}
}
