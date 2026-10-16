package repository

// Integration tests against a real MongoDB (v1.0.6). Run with
//   MONGO_TEST_URI=mongodb://127.0.0.1:27017 go test ./internal/repository/
// Each test uses its own database and drops it.

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"relay-agent/internal/util"
)

func testRepo(t *testing.T) *MongoRepository {
	t.Helper()
	uri := os.Getenv("MONGO_TEST_URI")
	if uri == "" {
		t.Skip("MONGO_TEST_URI not set")
	}
	repo, err := NewMongoRepository(uri, fmt.Sprintf("relay_agent_test_%d", time.Now().UnixNano()), zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_ = repo.database.Drop(ctx)
		_ = repo.Close(ctx)
	})
	return repo
}

func received(mgw, rcpt, queueID string, at time.Time) *Email {
	e := GetEmail()
	e.MailgatewayQueueID, e.Recipient, e.Sender, e.Status, e.QueueID = mgw, rcpt, "s@sender.example", "received", queueID
	e.ReceivedAt, e.CreatedAt = at, at
	return e
}

func final(queueID, rcpt, status string) *Email {
	e := GetEmail()
	e.QueueID, e.Recipient, e.Sender, e.Status, e.DSN = queueID, rcpt, "s@sender.example", status, "2.0.0"
	e.CreatedAt = util.NowTurkey()
	return e
}

func count(t *testing.T, r *MongoRepository, f bson.M) int64 {
	t.Helper()
	n, err := r.emails.CountDocuments(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// An existing v1.0.5 collection: unique sparse idx_queue_id and a record stuck with queue_id "".
func TestQueueIndexMigrationFromV105(t *testing.T) {
	r := testRepo(t)
	ctx := context.Background()
	if _, err := r.emails.Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "queue_id", Value: 1}},
		Options: options.Index().SetUnique(true).SetSparse(true).SetName(legacyQueueIndex)}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.emails.InsertOne(ctx, bson.M{"mailgateway_queue_id": "MGW-STUCK", "recipient": "a@x.example", "status": "received", "queue_id": ""}); err != nil {
		t.Fatal(err)
	}
	if err := r.EnsureIndexes(ctx); err != nil {
		t.Fatal(err)
	}
	cur, _ := r.emails.Indexes().List(ctx)
	var specs []bson.M
	_ = cur.All(ctx, &specs)
	names := map[string]bson.M{}
	for _, sp := range specs {
		names[sp["name"].(string)] = sp
	}
	if _, ok := names[legacyQueueIndex]; ok {
		t.Fatalf("legacy index still present: %v", names)
	}
	sp, ok := names[queueIndexName]
	if !ok || sp["unique"] != true || sp["partialFilterExpression"] == nil {
		t.Fatalf("new index missing or not unique/partial: %v", sp)
	}
	if n := count(t, r, bson.M{"queue_id": ""}); n != 0 {
		t.Fatalf("empty queue_id left: %d", n)
	}
	// idempotent
	if err := r.EnsureIndexes(ctx); err != nil {
		t.Fatal(err)
	}
}

// v1.0.5 lost the second of two mails that reached the relay before the first was delivered.
func TestReceivedRecordsDoNotCollide(t *testing.T) {
	r := testRepo(t)
	ctx := context.Background()
	if err := r.EnsureIndexes(ctx); err != nil {
		t.Fatal(err)
	}
	now := util.NowTurkey()
	for i, mgw := range []string{"MGW-A", "MGW-B", "MGW-C"} {
		if err := r.BulkUpsertByMailgatewayQueueID(ctx, []*Email{received(mgw, fmt.Sprintf("r%d@icloud.com", i), "", now)}); err != nil {
			t.Fatal(err)
		}
	}
	if n := count(t, r, bson.M{}); n != 3 {
		t.Fatalf("records: %d, want 3", n)
	}
	if n := count(t, r, bson.M{"queue_id": bson.M{"$exists": true}}); n != 0 {
		t.Fatalf("unknown queue ID stored: %d", n)
	}
}

// One message, two recipients: one record each, one delivery result each (queue ID + recipient).
func TestMultiRecipientRecordsAndResults(t *testing.T) {
	r := testRepo(t)
	ctx := context.Background()
	if err := r.EnsureIndexes(ctx); err != nil {
		t.Fatal(err)
	}
	now := util.NowTurkey()
	if err := r.BulkUpsertByMailgatewayQueueID(ctx, []*Email{
		received("MGW-M", "a@icloud.com", "4A1B2C3D4E", now), received("MGW-M", "b@icloud.com", "4A1B2C3D4E", now)}); err != nil {
		t.Fatal(err)
	}
	if n := count(t, r, bson.M{"mailgateway_queue_id": "MGW-M"}); n != 2 {
		t.Fatalf("records: %d, want 2", n)
	}
	for rcpt, st := range map[string]string{"a@icloud.com": "sent", "b@icloud.com": "bounced"} {
		mgw, err := r.UpdateByQueueID(ctx, final("4A1B2C3D4E", rcpt, st))
		if err != nil || mgw != "MGW-M" {
			t.Fatalf("UpdateByQueueID(%s) = %q, %v", rcpt, mgw, err)
		}
	}
	if count(t, r, bson.M{"recipient": "a@icloud.com", "status": "sent"}) != 1 || count(t, r, bson.M{"recipient": "b@icloud.com", "status": "bounced"}) != 1 {
		t.Fatal("per-recipient results not kept")
	}
	// the same (queue ID, recipient) twice is still refused (unique index), and does not abort the batch
	if err := r.BulkUpsertByMailgatewayQueueID(ctx, []*Email{received("MGW-OTHER", "a@icloud.com", "4A1B2C3D4E", now),
		received("MGW-N", "c@icloud.com", "", now)}); err != nil {
		t.Fatal(err)
	}
	if count(t, r, bson.M{"mailgateway_queue_id": "MGW-OTHER"}) != 0 || count(t, r, bson.M{"mailgateway_queue_id": "MGW-N"}) != 1 {
		t.Fatal("duplicate accepted or the rest of the batch lost")
	}
}

// Without a queue ID (reply not read): matched by sender + recipient — also after 10 minutes, oldest first.
func TestRecipientMatchFallback(t *testing.T) {
	r := testRepo(t)
	ctx := context.Background()
	if err := r.EnsureIndexes(ctx); err != nil {
		t.Fatal(err)
	}
	now := util.NowTurkey()
	if err := r.BulkUpsertByMailgatewayQueueID(ctx, []*Email{
		received("MGW-OLD", "x@shop.example", "", now.Add(-18*time.Minute)),
		received("MGW-NEW", "x@shop.example", "", now.Add(-2*time.Minute))}); err != nil {
		t.Fatal(err)
	}
	f := final("7F7F7F7F7F", "x@shop.example", "sent")
	mgw, err := r.UpdateByQueueID(ctx, f)
	if err != nil || mgw != "" {
		t.Fatalf("no record carries the queue ID yet: %q, %v", mgw, err)
	}
	mgw, err = r.UpdateByRecipientMatch(ctx, f)
	if err != nil || mgw != "MGW-OLD" {
		t.Fatalf("oldest record after 18 minutes: %q, %v", mgw, err)
	}
	// a later result of the same message updates it by queue ID
	mgw, err = r.UpdateByQueueID(ctx, final("7F7F7F7F7F", "x@shop.example", "bounced"))
	if err != nil || mgw != "MGW-OLD" {
		t.Fatalf("follow-up by queue ID: %q, %v", mgw, err)
	}
	// older than the window: not matched
	if err := r.BulkUpsertByMailgatewayQueueID(ctx, []*Email{received("MGW-ANCIENT", "y@shop.example", "", now.Add(-RecipientMatchWindow-time.Hour))}); err != nil {
		t.Fatal(err)
	}
	if mgw, _ := r.UpdateByRecipientMatch(ctx, final("1A1A1A1A1A", "y@shop.example", "sent")); mgw != "" {
		t.Fatalf("matched outside the window: %q", mgw)
	}
}
