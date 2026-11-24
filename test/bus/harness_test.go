//go:build integration

// Package bus_test runs Billing's event side end to end (LLD §7, §14.2):
// Postgres 17 with the production roles and migrations, and floci for SNS,
// SQS and the Glue Schema Registry. Each test creates its own topic,
// queues and registry, so tests never see each other's traffic.
package bus_test

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awscreds "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/glue"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

const (
	flociImage   = "floci/floci:2.1.0"
	flociRegion  = "ap-south-1"
	flociAccount = "000000000000"
)

var (
	superPool, appPool, systemPool *pgcommon.Pool
	flociEndpoint                  string
	awsCfg                         aws.Config
)

func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	ctx := context.Background()
	pg, err := tcpostgres.Run(ctx, "postgres:17-alpine",
		tcpostgres.WithDatabase("billing"), tcpostgres.WithUsername("postgres"), tcpostgres.WithPassword("testpassword"),
		tcpostgres.WithInitScripts("../../scripts/init-db.sql"),
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).WithStartupTimeout(90*time.Second)),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "postgres container: %v\n", err)
		return 1
	}
	defer func() { _ = pg.Terminate(ctx) }()

	floci, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{Started: true,
		ContainerRequest: testcontainers.ContainerRequest{
			Image: flociImage, ExposedPorts: []string{"4566/tcp"},
			Env:        map[string]string{"FLOCI_DEFAULT_REGION": flociRegion, "FLOCI_DEFAULT_ACCOUNT_ID": flociAccount},
			WaitingFor: wait.ForLog("Ready.").WithStartupTimeout(90 * time.Second),
		}})
	if err != nil {
		fmt.Fprintf(os.Stderr, "floci container: %v\n", err)
		return 1
	}
	defer func() { _ = floci.Terminate(ctx) }()
	host, _ := floci.Host(ctx)
	port, err := floci.MappedPort(ctx, "4566/tcp")
	if err != nil {
		fmt.Fprintf(os.Stderr, "floci port: %v\n", err)
		return 1
	}
	flociEndpoint = fmt.Sprintf("http://%s:%s", host, port.Port())
	// The SDK needs credentials even though floci accepts any.
	_ = os.Setenv("AWS_ACCESS_KEY_ID", "test")
	_ = os.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	awsCfg, err = awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(flociRegion),
		awsconfig.WithCredentialsProvider(awscreds.NewStaticCredentialsProvider("test", "test", "")))
	if err != nil {
		fmt.Fprintf(os.Stderr, "aws config: %v\n", err)
		return 1
	}

	superDSN, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintf(os.Stderr, "dsn: %v\n", err)
		return 1
	}
	if err := pgadapter.MigrateSchema(ctx, withUser(superDSN, "billing_migrator"), nil); err != nil {
		fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
		return 1
	}
	mk := func(dsn, name string, guc bool) *pgcommon.Pool {
		cfg := pgcommon.Config{DSN: dsn, PoolName: name, MaxConns: 6, PGBouncerMode: name != "super"}
		if guc {
			cfg.GUCProvider = pgcommon.GUCSetFromContext
		}
		p, err := pgcommon.NewPool(ctx, cfg)
		if err != nil {
			panic(fmt.Sprintf("pool %s: %v", name, err))
		}
		return p
	}
	superPool = mk(superDSN, "super", false)
	appPool = mk(withUser(superDSN, "billing_app"), "app", true)
	systemPool = mk(withUser(superDSN, "billing_system"), "system", false)
	defer func() {
		for _, p := range []*pgcommon.Pool{superPool, appPool, systemPool} {
			p.Close()
		}
	}()
	return m.Run()
}

func withUser(dsn, user string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		panic(err)
	}
	u.User = url.UserPassword(user, "devpassword")
	return u.String()
}

func endpointOpt[T any](set func(*T, *string)) func(*T) {
	ep := flociEndpoint
	return func(o *T) { set(o, &ep) }
}

func snsClient() *sns.Client {
	return sns.NewFromConfig(awsCfg, endpointOpt(func(o *sns.Options, ep *string) { o.BaseEndpoint = ep }))
}

func sqsClient() *sqs.Client {
	return sqs.NewFromConfig(awsCfg, endpointOpt(func(o *sqs.Options, ep *string) { o.BaseEndpoint = ep }))
}

func glueClient() *glue.Client {
	return glue.NewFromConfig(awsCfg, endpointOpt(func(o *glue.Options, ep *string) { o.BaseEndpoint = ep }))
}

func unique(base string) string {
	return base + "-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
}

func arn(service, name string) string {
	return fmt.Sprintf("arn:aws:%s:%s:%s:%s", service, flociRegion, flociAccount, name)
}

// queueWithDLQ creates name and name-dlq with a RedrivePolicy, returning
// both URLs.
func queueWithDLQ(t *testing.T, name string, maxReceive int) (string, string) {
	t.Helper()
	ctx := context.Background()
	c := sqsClient()
	dlq, err := c.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String(name + "-dlq")})
	if err != nil {
		t.Fatal(err)
	}
	policy := fmt.Sprintf(`{"deadLetterTargetArn":"%s","maxReceiveCount":"%d"}`, arn("sqs", name+"-dlq"), maxReceive)
	q, err := c.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String(name),
		Attributes: map[string]string{"RedrivePolicy": policy, "VisibilityTimeout": "2"}})
	if err != nil {
		t.Fatal(err)
	}
	return *q.QueueUrl, *dlq.QueueUrl
}

// receive polls url until want messages arrive or timeout.
func receive(t *testing.T, url string, want int, timeout time.Duration) []sqstypes.Message {
	t.Helper()
	ctx := context.Background()
	var out []sqstypes.Message
	deadline := time.Now().Add(timeout)
	for len(out) < want && time.Now().Before(deadline) {
		r, err := sqsClient().ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: &url, MaxNumberOfMessages: 10, WaitTimeSeconds: 1,
			MessageAttributeNames: []string{"All"}, MessageSystemAttributeNames: []sqstypes.MessageSystemAttributeName{"All"}})
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range r.Messages {
			out = append(out, m)
			_, _ = sqsClient().DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: &url, ReceiptHandle: m.ReceiptHandle})
		}
	}
	if len(out) < want {
		t.Fatalf("received %d of %d messages from %s", len(out), want, url)
	}
	return out
}

func newID() string { return uuid.NewString() }

func exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	err := pgcommon.RunInTx(context.Background(), superPool, pgcommon.TxOptions{}, func(ctx context.Context, tx pgcommon.Tx) error {
		_, err := tx.Exec(ctx, sql, args...)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// drain returns how many messages arrive on url within two seconds.
func drain(t *testing.T, url string) int {
	t.Helper()
	n := 0
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r, err := sqsClient().ReceiveMessage(context.Background(), &sqs.ReceiveMessageInput{QueueUrl: &url, MaxNumberOfMessages: 10, WaitTimeSeconds: 1})
		if err != nil {
			t.Fatal(err)
		}
		n += len(r.Messages)
	}
	return n
}
